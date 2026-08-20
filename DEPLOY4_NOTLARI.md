# DEPLOY 4 — NOTLAR (denetim maddesi C3)

**Migration YOK.** Şema değişikliği yok, index yok. Sadece deploy.
**İstemci değişikliği YOK.** Flutter ve App Store'daki eski iOS aynen çalışır.

---

## Neydi

Tek bir DM için sunucunun ürettiği trafik:

| ne | WS frame | Redis PUBLISH |
|---|---|---|
| `new_message` (gönderen echo + alıcı) | 2 | 1 |
| `conversation_update` (gönderen + alıcı) | 2 | 2 |
| okunmamış sayacı | 1 | 1 |
| `message_delivered` | 1 | 1 |
| **toplam** | **6** | **5** |

İki ayrı israf vardı.

### 1. Tek instans çalışırken 5 Redis PUBLISH tamamen boşa gidiyordu

`WS_CLUSTER_ENABLED` Redis açıksa varsayılan olarak açık. Ama `docker-compose.yml`
tek konteyner çalıştırıyor. Yani her mesajda 5 kez:

```
frame JSON'a çevrilir → Redis'e yazılır → Redis onu BİZE geri verir
→ açılır → `frame.Origin == instanceID` → ATILIR
```

Beş turun beşi de çöpe gidiyordu. Üstelik yayın kuyruğu dolduğunda frame'ler
**sessizce atılıyordu** (`cluster.go`) — yani boşa giden iş, gerçek yükte
gerçek frame kaybına yol açabiliyordu.

### 2. `conversation_update` mesajın tamamını ikinci kez taşıyordu

Bu frame `message_data` alanında **mesajın kendisini** içeriyor — yani her
mesaj kablodan iki kez geçiyordu.

---

## Ne oldu

### 1. Başka instans yoksa yayın yapılmıyor (`DM-C1`)

"Başka instans var mı?" sorusu **pasif** öğreniliyor, ek Redis komutu yok:

- Her instans aynı kanala **10 saniyede bir** küçük bir "buradayım" frame'i
  yazıyor. Mesaj sayısından bağımsız: saniyede 0,1 PUBLISH.
- Başka bir instanstan **herhangi** bir frame gelince "peer var" damgası
  yenileniyor.
- Damga 45 saniye eskirse peer yok sayılıyor ve veri yayını duruyor
  (heartbeat devam ediyor).

**Yarış yok — iki koruma var:**

1. Yeni instans kalkar kalkmaz ilk heartbeat'ini **hemen** yazıyor, yani eski
   instans onu milisaniyeler içinde görüyor.
2. Yeni instans kendi ilk **60 saniyesi** boyunca "peer var" varsayıyor.

Yani rolling deploy sırasında (eski + yeni birlikte çalışırken) frame kaybı
olmuyor.

```
WS_CLUSTER_SOLO_SKIP=false    # optimizasyonu tamamen kapat (eski davranış)
```

### 2. `conversation_update` v2 istemciye gönderilmiyor (`DM-C2`)

`?cv=2` ile bağlanan istemciye artık gönderilmiyor. **Doğrulandı:** iOS sohbet
listesi `new_message`'ı zaten tam işliyor —
`ConversationsViewModel.processNewMessage` önizlemeyi, zamanı, okunmamış
sayacını, son mesaj id'sini ve `isLastFromMe`'yi güncelliyor; sohbet listede
yoksa tam yenileme yapıyor. `conversation_update` "authoritative" sayılıyordu
ama **zorunlu değildi**.

**Eski istemci tam korunuyor:** Flutter ve App Store'daki eski iOS `cv`
göndermiyor → `protoLegacy` → frame eskisi gibi gidiyor.

**Fail-open:** kullanıcı bu instansta bulunamazsa (offline ya da başka
instansta) frame **gönderiliyor** — bilinmeyen durumda eski davranış.

> ⚠️ Bu yalnız **yeni-mesaj** kaynaklı `conversation_update`'e ait.
> Reaksiya event'leri ayrı bir yol ve **dokunulmadı** — orada `new_message`
> yok, yani istemcinin başka bilgi kaynağı yok.

```
WS_CONV_UPDATE=all    # eski davranışa dön (deploy'suz geri alma)
```

---

## Sonuç

| | önce | sonra |
|---|---|---|
| Redis PUBLISH / mesaj | 5 | **0** (+ 10 sn'de bir heartbeat) |
| WS frame / mesaj (iki taraf da yeni iOS) | 6 | **4** |
| WS frame / mesaj (Flutter) | 6 | 6 |

Flutter kullanıcısı için frame sayısı aynı kalıyor ama **Redis yükü yine sıfıra
iniyor** — yani kazanç her iki istemcide de var.

Bu bir **kapasite** maddesi, hız maddesi değil: kullanıcı "hızlandı" demez, ama
aynı makine belirgin şekilde daha fazla eşzamanlı mesaj taşır ve yayın kuyruğu
dolup frame atma riski ortadan kalkar.

---

## Testler

`websocket/proto_test.go` içine 7 test eklendi (hepsi geçiyor, veritabanı
gerekmiyor):

- açılış penceresinde **her zaman** yayın yapılıyor (rolling deploy koruması)
- peer görülmemişse yayın duruyor
- taze peer varsa yayın açık (**mesaj kaybı koruması**)
- peer damgası eskiyince yayın tekrar duruyor
- `WS_CLUSTER_SOLO_SKIP=false` eski davranışı geri getiriyor
- `notePeerFrame` yayını açıyor
- `needsConversationUpdate`: v2 → hayır, legacy → **evet**, bilinmeyen → **evet**,
  `WS_CONV_UPDATE=all` → evet

---

## Deploy

```bash
docker compose up -d --build
```

Migration yok, env değişikliği gerekmiyor.

**Açılıştan ~1 dakika sonra** logda şunu görmelisin:

```
ws-cluster: başqa instans yoxdur — data yayımı dayandırıldı (heartbeat davam edir)
```

Bu satır çıkıyorsa optimizasyon çalışıyor demektir. İkinci bir instans
açarsan bunun yerine şunu görürsün:

```
ws-cluster: başqa instans göründü — yayım açıq
```

## Geri alma

| madde | geri alma |
|-------|-----------|
| Yayın atlaması | `WS_CLUSTER_SOLO_SKIP=false` (deploy gerekmez, yeniden başlat) |
| `conversation_update` | `WS_CONV_UPDATE=all` (deploy gerekmez, yeniden başlat) |
| ikisi de | git revert |
