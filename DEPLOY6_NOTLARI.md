# DEPLOY 6 — PgBouncer düzeltmesi + W3 (yavaş istemcide bağlantı kopması)

**Migration YOK. İstemci değişikliği YOK.** Sadece deploy.

---

## 1. PgBouncer / `statement_timeout` — CANLI ARIZANIN KALICI ÇÖZÜMÜ

### Ne olmuştu

Deploy 1'de veritabanı bağlantı adresine (DSN) `statement_timeout=15000`
eklemiştim — takılan bir sorgu havuzdaki bağlantıyı sonsuza kadar tutmasın diye.

Veritabanı **PgBouncer** arkasında. PgBouncer, bağlantı **açılışında** tanımadığı
parametreleri reddeder:

```
FATAL: unsupported startup parameter: statement_timeout  (SQLSTATE 08P01)
```

Sonuç: uygulama veritabanına **hiç bağlanamadı** → mesajlaşma tamamen durdu.

Neden aylar sonra patladı: kod `perf/deploy1` dalındaydı, sunucu `main`'i
çekiyordu. Yani Deploy 1'in Go tarafı hiç yayınlanmamıştı. Dal düzeltilince
**8 commit birden** indi ve bu satır ilk kez üretime çıktı.

### Ne yapıldı — iki katmanlı koruma

1. `PGBOUNCER_ENABLED=true` ise parametre DSN'e **hiç yazılmaz**.
2. Bayrak ayarlanmamış olsa bile: bağlantı `unsupported startup parameter`
   ile reddedilirse parametre **atılıp bir kez daha denenir**.

Yani bu satır bir daha servisi düşüremez. Diğer bağlantı hataları (yanlış
parola, ağ, olmayan veritabanı) yedek yolu tetiklemez — testle kilitlendi.

### Zaman aşımı korumasını geri kazanmak (isteğe bağlı)

PgBouncer'ı hiç ilgilendirmeyen doğru yer veritabanı rolüdür:

```sql
ALTER ROLE beanpon_user SET statement_timeout = '15s';
```

---

## 2. W3 — Yavaş istemcide bağlantı artık kopartılmıyor

### Neydi

Her istemcinin 256 frame'lik bir gönderim kuyruğu var. Kuyruk dolduğu an
sunucu **bağlantıyı kopartıyordu**.

Kuyruk şu durumlarda dolar: karşı tarafın şebekesi bir an yavaşlar, telefon
arka plandan uyanır, ya da kalabalık bir grupta ard arda olay gelir.
Kullanıcı gözünde bu **"bağlantı sürekli kopuyor"** demektir.

Oysa kuyruğu dolduranların çoğu **geçici** frame'dir: "yazıyor…", online
durumu, okunmamış sayacı. Bunların eskisi zaten değersizdir — yenisi
geldiğinde eskisinin anlamı kalmaz.

### Ne oldu

| kuyruk dolu + frame | önce | sonra |
|---|---|---|
| `user_typing`, `group_typing`, `user_status`, `unread_count_update`, `online_users` | **bağlantı kopar** | frame atılır, **bağlantı yaşar** |
| `new_message`, `message_ack`, `message_read`, `conversation_update`, … | bağlantı kopar | bağlantı kopar (aynı) |

Kritik frame'i sessizce atmak **gerçek veri kaybı** olurdu. Kopartmak değil:
istemci yeniden bağlanıp delta-sync ile kaçırdığını toplar. Kural şu —
*kaybetmektense kopart, ama gereksiz yere kopartma.*

Bu davranış tek bir yerde toplandı (`Client.trySend`) ve 5 çağrı yerinin
hepsi oradan geçiyor: doğrudan teslim, çoklu teslim, durum yayını ve iki
küme-içi teslim yolu.

```
WS_DROP_TRANSIENT=false     # eski davranışa dön (deploy gerekmez)
```

### Etkisini nasıl göreceksin

Yeni sayaç:

```
beanpon_messenger_ws_frame_dropped_total{type="user_typing"}
```

Bu sayı **kurtarılmış bağlantı** demektir — eskiden her biri bir kopma olurdu.

Karşılaştır:

```bash
./scripts/chat-metrics.py --watch 300
```

`kopartılan bağlantı` sayacının düşmesi, `atılan geçici frame` sayacının
artması beklenir.

---

## Testler

Yeni 6 test (`websocket/proto_test.go`, `database/database_test.go`),
veritabanı gerekmiyor:

- geçici frame listesi kilitli — **kritik bir tip oraya sızarsa test patlar**
  (mesaj sessizce atılırdı)
- kuyruk doluyken geçici frame: bağlantı yaşıyor
- kuyruk doluyken kritik frame: bağlantı kopuyor (kasıtlı)
- kuyrukta yer varken ikisi de normal yazılıyor
- `WS_DROP_TRANSIENT=false` eski davranışı geri getiriyor
- PgBouncer'ın gerçek hata metni tanınıyor; başka hatalar tanınmıyor

Toplam: **36 test geçiyor.**

---

## Deploy

```bash
docker compose up -d --build
```

## Geri alma

| madde | geri alma |
|-------|-----------|
| PgBouncer düzeltmesi | geri alma **önerilmez** — arızanın kendisi buydu |
| W3 | `WS_DROP_TRANSIENT=false` (deploy gerekmez, yeniden başlat) |

---

## Bundan sonra

Birkaç gün veri biriksin, sonra:

```bash
./scripts/chat-metrics.py --watch 300
```

İki soruya bakacağız:

1. **`perm` / `persist` / `fanout` — hangisi en pahalı?** Bir sonraki sunucu
   maddesi o.
2. **`kopartılan bağlantı` sıfıra indi mi?** İnmediyse kritik frame'ler de
   kuyruk dolduruyor demektir; o zaman kuyruk boyutu ve `writePump` yazma
   hızına bakarız.
