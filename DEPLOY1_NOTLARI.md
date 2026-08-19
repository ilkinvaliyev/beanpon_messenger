# DEPLOY 1 — TAMAMLANDI (sunucu tarafı)

Branch: `perf/deploy1` · Derleme: ✅ `go build ./...` · `go vet` ✅ · Test ✅ (5 geriye-uyumluluk testi)

**Eski istemciye etki: SIFIR.** Flutter ve App Store'daki eski iOS bugünkü davranışı birebir görür — sadece daha hızlı.

---

## Değişen dosyalar (12)

| Dosya | Ne değişti | ÖNCE → SONRA |
|---|---|---|
| `websocket/hub.go` | 7 değişiklik (aşağıda ayrıntılı) | — |
| `websocket/cluster.go` | `clusterFrame`'e `cs` (chat subject) alanı | Sadece `WS_STATUS_FANOUT=chat` iken dolar; `omitempty` → eski instance görmezden gelir |
| `websocket/proto_test.go` | **YENİ** — geriye uyumluluk testleri | — |
| `handlers/proto.go` | **YENİ** — `X-Chat-Proto` başlığı okuma | — |
| `handlers/message_handler.go` | `markReceivedMessagesAsRead` sayfa kapısı + `COUNT(*)` kapısı | — |
| `models/user_block.go` | `IsBlocked`: `COUNT(*)` → `EXISTS` | Tüm eşleşmeleri sayıyordu → ilk eşleşmede duruyor |
| `models/hidden_mode.go` | `HiddenBlockedPeerIDs`'e `hiddenModeEnabled` guard'ı | Tutarsızlık bitti + sohbet listesi başına −1..3 sorgu |
| `database/database.go` | Havuz env'e taşındı + `statement_timeout` | 25 sabit → 50 (env) · timeout yoktu → 15 sn |
| `cache/redis.go` | `MinIdleConns` + hata log'u kısıldı | 0 → 5 · her hatada log → 60 sn'de bir özet |
| `config/config.go` | 6 yeni env değişkeni | — |
| `services/moderation_queue.go` | Kuyruk-dolu log'u kısıldı | Mesaj başına log → 60 sn'de bir özet |
| `cmd/main/main.go` | Gin release modu + `http.Server` zaman aşımları | Debug modu + her istekte stdout → release, log kapalı |

---

## `websocket/hub.go` — 7 değişiklik

| # | ÖNCE | SONRA | Kazanç |
|---|---|---|---|
| 1 | `log.Printf("📨 PUSH-GATE…")` her mesajda; argümanları için `IsUserOnline` + `IsUserInChatWith` **fazladan** çağrılıyordu | Satır silindi; iki değer **bir kez** hesaplanıp değişkene alınıyor. `inChat` sadece online ise sorgulanıyor | Mesaj başına **4 → 2 kilit**, 2 olası Redis GET, 1 stderr yazımı |
| 2 | `log.Printf("Okunmamış mesaj sayısı gönderildi…")` her mesajda | Silindi | 1 stderr yazımı/mesaj |
| 3 | `log.Printf("✅ Push notification gönderildi…")` her push'ta | `PUSH_LOG=true` ile açılır, default kapalı. Hata log'u aynen duruyor | 1 stderr yazımı/push |
| 4 | `SetReadLimit` **yoktu** — sınırsız frame | `256 KB` | Bellek koruması |
| 5 | `statusTargetsLocked` **yazma kilidi altında** O(N) tarama → bu sırada tüm mesaj teslimatı duruyordu | `statusTargets` kendi `RLock`'unu alıyor, yazma kilidi dışında | Her bağlan/kop'ta teslimat donması bitti |
| 6 | `GetUnreadCount` (WS) `users` join'siz, REST'teki join'li → **rozet zıplıyordu** | İkisi de aynı sorgu (join'li) | Tutarlılık |
| 7 | Bağlantıda **31 frame** tarihçe seli, iOS hepsini çöpe atıyor | Sadece `ProtoVersion < 2` istemcilere | Yeni istemcide **31 frame + ağır sorgu** iptal |

**Yeni: `Client.ProtoVersion`** — `?cv=2` yoksa `1` (eski). `parseProtoVersion` bozuk/eksik her girdide `1` döner; testle korunuyor.

**Yeni: `message_ack` frame'i** — sadece v2 istemciye, ~80 bayt:
```json
{"type":"message_ack","data":{"cid":"...","id":"...","receiver_id":123,"created_at":"...","duplicate":false}}
```
v1 istemciye **hiç yazılmaz** (test var). `new_message` echo'su Deploy 3'e kadar aynen devam ediyor.

**Yeni: `message_error` frame'lerine `cid` alanı** — v2 istemci hangi baloncuğun reddedildiğini bilsin. Eski istemci bu ek alanı görmezden gelir.

---

## Yeni env değişkenleri (hepsi opsiyonel, default = güvenli)

```bash
# Veritabanı
DB_MAX_OPEN_CONNS=50          # eskiden sabit 25
DB_MAX_IDLE_CONNS=25
DB_STATEMENT_TIMEOUT_MS=15000 # 0 = kapat (eski davranış)

# Redis
REDIS_POOL_SIZE=50            # eskiden 20
REDIS_MIN_IDLE_CONNS=5        # eskiden ayarlanmamış (0)

# Log
PUSH_LOG=false                # true → push başarı log'u geri gelir
GIN_ACCESS_LOG=false          # true → her HTTP isteği için log satırı

# user_status fan-out — ŞİMDİLİK ELLEMEYİN
WS_STATUS_FANOUT=all          # "chat" = O(N²) düzeltmesi. Flutter'ı test etmeden açmayın.
```

---

## ⚠️ Deploy öncesi 3 kontrol

1. **`SHOW max_connections;`** — `DB_MAX_OPEN_CONNS × replica sayısı + Laravel + pgbouncer` bundan küçük olmalı. Emin değilseniz `DB_MAX_OPEN_CONNS=25` ile deploy edin (eski davranış), sonra artırın.
2. **`Dockerfile:16`** `go mod tidy && go mod vendor` çalıştırıyor → build sırasında ağ gerekiyor. Değiştirmedim ama bilin.
3. **Caddy WebSocket okuma zaman aşımı** — hâlâ bilmiyoruz. 60 sn'nin altındaysa bağlantılar dakikada bir kopuyordur. Bu Deploy 1'den bağımsız ama en büyük bilinmeyen.

---

## Geri alma

```bash
git checkout main          # kod
# veya sadece davranışı geri al (redeploy yok):
DB_MAX_OPEN_CONNS=25 DB_STATEMENT_TIMEOUT_MS=0 REDIS_POOL_SIZE=20 REDIS_MIN_IDLE_CONNS=0
```

---

## Deploy sonrası doğrulama

| Ne | Nasıl | Beklenen |
|---|---|---|
| Eski istemci bozulmadı | Flutter uygulamasıyla mesaj gönder/al, sohbet listesi aç | Fark yok |
| Tarihçe seli hâlâ gidiyor | Flutter bağlanınca `history_message` frame'leri | 31 frame geliyor (v1) |
| Log hacmi | `docker logs --since 5m | wc -l` | Belirgin düşüş |
| Rozet tutarlılığı | WS rozeti ↔ REST `/messages/unread-count` | Aynı sayı |
| Sohbet listesi | Gizli moddaki kullanıcı varsa listede görünür | Beklenen (Karar 1) |
| Havuz | Log'da `PostgreSQL hovuzu: maxOpen=50 …` | Görünmeli |

---

## SIRADA — sizden gerekli

```sql
-- 1) Mevcut index'ler (en büyük kazanç buna bağlı)
SELECT indexname, indexdef FROM pg_indexes
WHERE tablename IN ('messages','conversations') ORDER BY tablename, indexname;

-- 2) Havuz sınırı
SHOW max_connections;

-- 3) Gizli mod etkisi (bilgi amaçlı)
SELECT COUNT(*) FROM users WHERE hidden_mode = true;
```

Ayrıca: **Caddy config'inde `msg.beanpon.com` / `gw.beanpon.com` bloğu.**

Bunlar gelince index migration'ını hazırlayıp Deploy 2'ye (iOS WebSocket gönderim yolu) geçiyorum.
