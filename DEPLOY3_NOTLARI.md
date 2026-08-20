# DEPLOY 3 — NOTLAR

Bu deploy'da **istemci tarafında hiçbir değişiklik yok.** Flutter `beanpon_app`
ve App Store'daki eski `piokio_ios` sürümleri dahil, tüm istemciler aynı
kalıyor. Sunucu içi iyileştirmeler.

---

## 1. Sohbet listesi sorgusu (`GetConversations`) — en büyük kazanç

**Dosya:** `handlers/message_handler.go`
**Migration:** `MIGRATION_deploy3_conversations.sql` (2 index)

### Neydi

Sorgu `ROW_NUMBER() OVER (PARTITION BY karşı_taraf ORDER BY created_at DESC)`
kullanıyordu. Bu şu demek: kullanıcının **bütün** mesajları okunur, hepsine
sıra numarası verilir, sıralanır, sonra sadece `rn = 1` olanlar (yani her
sohbetin son mesajı) alınır.

Yani maliyet **sohbet sayısıyla değil, toplam mesaj sayısıyla** büyüyordu.
130 bin mesajı olan bir kullanıcıda sıralama belleğe sığmıyor, PostgreSQL
34 MB'lık geçici dosyayı **diske** yazıyordu.

### Ne oldu

Üç adım:

1. **Karşı taraf listesi — "gevşek index taraması"**
   Özyinelemeli bir CTE, index üzerinde her adımda bir sonraki *farklı*
   karşı tarafa atlıyor. 200 sohbet = 200 index inişi. Mesaj sayısı artık
   hiç okunmuyor. (PostgreSQL 16'da index skip-scan yok; bu onun elle
   yazılmış karşılığı.)

2. **Her sohbet için iki nokta atışı**
   Giden yönde son mesaj, gelen yönde son mesaj — her biri `LIMIT 1` ile
   tek satır.

3. **`DISTINCT ON`** ile iki yönden yeni olan seçiliyor.
   `id DESC` eşitlik bozucu eklendi: eskiden aynı mikrosaniyeye denk gelen
   iki mesajda hangisinin önizlemede görüneceği **tanımsızdı**.

Ayrıca `conversations` join'i LATERAL'e çevrildi — planlayıcı artık her
durumda index kullanıyor, tabloyu tarayamıyor.

### Ölçüm

PostgreSQL 16.13, 430.280 mesaj, 202.001 kullanıcı, 11.909 sohbet:

| kullanıcı profili        | ÖNCE   | SONRA  | okunan blok      |
|--------------------------|--------|--------|------------------|
| 130k mesaj / 200 sohbet  | 154 ms | 18 ms  | 24.030 → 8.690   |
| 50k mesaj / 1 sohbet     | 53 ms  | 2,3 ms | 4.390 → 65       |
| 491 mesaj / 16 sohbet    | 2,2 ms | 3,0 ms | 701 → 499        |

Diske taşma (34 MB geçici dosya) **tamamen bitti**.

Küçük kullanıcıdaki 0,8 ms'lik fark yerel CPU gürültüsü — okunan blok sayısı
orada da düşüyor, yani uzak veritabanında (I/O baskın) yeni sorgu her profilde
daha ucuz.

### Doğrulama

**120 senaryoda eski ve yeni sorgu satır satır birebir aynı sonucu verdi.**
4 status filtresi × 3 archived filtresi × 10 kullanıcı, artı şu kenar durumları:

- tüm mesajları silinmiş sohbet → listede çıkmamalı ✓
- yalnız giden / yalnız gelen mesajı olan sohbet ✓
- kendine mesaj (self-chat), yarısı silinmiş ✓
- giden mesajların hepsi silinmiş ama gelen duruyor ✓
- iki yönde aynı mikrosaniyeli mesaj (eşitlik) ✓

---

## 2. Düzgün kapanış (graceful shutdown)

**Dosyalar:** `cmd/main/main.go`, `websocket/hub.go`, `websocket/presence.go`

### Neydi

`ListenAndServe` ana goroutine'i bloke ediyordu. SIGTERM gelince proses
**anında** ölüyordu:

- Bağlı olan herkesin soketi close frame'siz kopuyordu. İstemci bunu "hata"
  sayıp yeniden bağlanma merdivenine (backoff) giriyor — kullanıcı her
  deploy'da 1–5 saniye "bağlanıyor" görüyordu.
- Uçuştaki HTTP istekleri yarıda kesiliyordu.
- `user_presences` satırları `is_online = true` kalıyordu.

### Ne oldu

SIGTERM/SIGINT'te sırayla:

1. Her WebSocket'e normal **close frame** gönderilir. İstemci bunu "sunucu
   kapandı" olarak görür ve *beklemeden* yeniden bağlanır.
2. Redis presence kayıtları temizlenir (yalnız bu instance'a ait olanlar).
3. `user_presences` tek SQL ile kapatılır — **oturum süresi muhasebesi
   doğru yapılarak** (aşağıya bak).
4. HTTP sunucusu uçuştaki istekleri bitirir.

```
GRACEFUL_SHUTDOWN_SECONDS   varsayılan 15
```

> ⚠️ Orkestratörünüzün bekleme süresi bundan **büyük** olmalı, yoksa araya
> SIGKILL girer.
> - docker-compose: `stop_grace_period: 30s`
> - Kubernetes: `terminationGracePeriodSeconds: 30`
> - systemd: `TimeoutStopSec=30`

### Testler

`websocket/proto_test.go` içine 3 test eklendi, hepsi geçiyor:

- `TestShutdown_NoClients` — bağlantı yokken DB'ye hiç dokunmamalı, asılmamalı
- `TestShutdown_ClosesEveryClient` — süre dolsa bile close frame'ler gitmeli
- `TestCloseSend_Idempotent` — `Shutdown` ile `unregister` çakışırsa panic olmamalı

---

## 3. Açılıştaki global presence sıfırlaması

**Dosya:** `cmd/main/main.go`

### Neydi

Her açılışta koşulsuz:

```sql
UPDATE user_presences SET is_online = false, last_seen_at = NOW() WHERE is_online = true
```

İki sorun:

1. **Çok instans varsa**, yeni açılan replica *diğer* replica'lardaki online
   kullanıcıları da offline yazıyordu → "online görünmüyorum".
2. `total_online_seconds` hiç işlenmiyordu. Yani **her deploy'da o anda bağlı
   olan herkesin oturum süresi kayboluyordu** — "Günün Kartı" ekran süresi
   dahil.

### Ne oldu

Madde 2'deki düzgün kapanış presence'i zaten doğru şekilde kapatıyor. Bu satır
artık sadece "proses SIGKILL yedi / OOM oldu" hâli için emniyet ağı — ve
kapatılabilir hâle geldi:

```
PRESENCE_RESET_ON_BOOT=false    # çok instanslı kurulumda bunu kullanın
```

Env verilmezse **bugünkü davranışın aynısı**.

---

## Deploy sırası

```bash
# 1) Index'ler — CONCURRENTLY, tabloyu kilitlemez, psql'den TEK TEK
psql ... -f MIGRATION_deploy3_conversations.sql

# 2) Orkestratör bekleme süresini yükselt (30s)

# 3) Yeni sunucuyu deploy et
```

Ters sırada da bir şey kırılmaz — o aralıkta sadece sorgu kazancı olmaz.

## Geri alma

| madde | geri alma |
|-------|-----------|
| Sohbet listesi sorgusu | git revert. Index'ler zararsızdır, kalabilir. |
| Düzgün kapanış | git revert. |
| Presence sıfırlaması | env'i kaldırın (varsayılan eski davranış). |

---

## Bu deploy'a ALINMAYANLAR ve nedeni

**Okunmamış sayacının Redis `INCR`/`DECR`'a taşınması.**
Deploy 1'de `idx_messages_unread_dm` ile 21 ms → 6 ms'ye indi ve 300 ms
biriktirme eklendi. Redis sayacı bunu ~0,1 ms yapardı, ama sayacın
bozulabileceği yol çok: okundu bildirimi, mesaj silme, blok, arşiv, çoklu
cihaz, cluster. Yanlış okunmamış rozeti kullanıcının doğrudan gördüğü bir
hatadır. Ayrı bir adım olarak, sayaç doğrulama (periyodik DB ile karşılaştırma)
ile birlikte yapılmalı.

**`writePump` frame birleştirme.**
İncelendi, **yapılmamalı**. WebSocket'te iki metin frame'i tek frame'e
birleştirmek, istemcinin frame başına bir JSON beklediği anlamına gelen
sözleşmeyi bozar (gorilla'nın örneğindeki newline ile ayırma yöntemi tam
olarak budur). Eski istemcileri kırar, kazancı ise sadece birkaç syscall.

**Eski istemciler bitene kadar bekleyenler** (Deploy 4 adayları):
- `sendRecentMessages`'ın kaldırılması
- v2 istemcilere `new_message` echo'sunun kesilmesi
- v2 istemcilere `conversation_update` gönderiminin kesilmesi
- `WS_STATUS_FANOUT=chat` (env hazır, açmak yeterli)

Bunlar App Store'daki eski `piokio_ios` sürümleri ve Flutter istemcisi
kullanımdan kalkmadan açılmamalı.
