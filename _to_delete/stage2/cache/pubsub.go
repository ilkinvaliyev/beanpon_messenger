package cache

import (
	"context"
	"log"
	"time"

	"github.com/go-redis/redis/v8"
)

// ── Issue 4 — instance-lar arası pub/sub ────────────────────────────────────
//
// PROBLEM
// `Hub.clients` YALNIZ bu prosesin yaddaşındakı map-dir. `SendToUser` sırf
// həmin map-ə baxır. Tək replica ilə bu işləyir; İKİNCİ replica qalxdığı an:
//   • A instansındakı göndərən → B instansındakı alıcı: canlı mesaj HEÇ VAXT
//     çatmır (yalnız DB-dədir, alıcı ancaq yenidən açanda görür);
//   • `IsUserOnline` yalan danışır → "offline" sayılıb push göndərilir,
//     halbuki istifadəçi onlayn və çatda ola bilər (və ya əksi);
//   • yazır…/oxundu/reaksiya/qrup event-lərinin hamısı eyni şəkildə itir.
// Yəni horizontal scale ETMƏK MÜMKÜN DEYİL — sistem gizli şəkildə tək
// replica-ya pinlənib.
//
// HƏLL — Redis pub/sub üzərindən yayım + paylaşılan presence.
// Bu fayl nazik nəqliyyat qatıdır; məntiq `websocket/cluster.go`-dadır.

// Publish — kanal üzərinə mesaj yayımlayır. Redis söndürülübsə no-op.
func (c *Client) Publish(ctx context.Context, channel string, payload string) error {
	if !c.guard() {
		return nil
	}
	pubCtx, cancel := context.WithTimeout(ctx, c.writeTimeout)
	defer cancel()
	err := c.rdb.Publish(pubCtx, channel, payload).Err()
	c.observe("PUBLISH", err)
	return err
}

// SubscribeLoop — `channel`-ə abunə olur və hər mesajı `handle`-ə verir.
// Bağlantı qopduqda avtomatik yenidən abunə olur (eksponensial gözləmə ilə).
// `ctx` bağlananda təmiz çıxır. Bloklayandır — `go` ilə çağırılmalıdır.
//
// DİQQƏT: burada circuit breaker İSTİFADƏ OLUNMUR. Breaker qısa sorğular
// üçündür; uzun-ömürlü abunəni onunla kəsmək yayımı səssizcə söndürərdi.
func (c *Client) SubscribeLoop(ctx context.Context, channel string, handle func(payload string)) {
	if !c.Enabled() || c.rdb == nil {
		log.Printf("cache: Redis söndürülüb — %q kanalına abunə olunmadı", channel)
		return
	}

	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		if ctx.Err() != nil {
			return
		}

		pubsub := c.rdb.Subscribe(ctx, channel)

		// İlk təsdiq — abunənin həqiqətən qurulduğunu yoxla.
		if _, err := pubsub.Receive(ctx); err != nil {
			_ = pubsub.Close()
			if ctx.Err() != nil {
				return
			}
			log.Printf("cache: %q abunəsi qurulmadı: %v — %s sonra təkrar", channel, err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < maxBackoff {
				backoff *= 2
			}
			continue
		}

		log.Printf("cache: %q kanalına abunə olundu", channel)
		backoff = time.Second // uğurlu bağlantı — gözləməni sıfırla

		ch := pubsub.Channel()
	receive:
		for {
			select {
			case <-ctx.Done():
				_ = pubsub.Close()
				return
			case msg, ok := <-ch:
				if !ok {
					break receive // kanal bağlandı → yenidən abunə
				}
				// Panic bir mesaj ucbatından bütün abunəni öldürməsin.
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("cache: %q mesaj emalında panic: %v", channel, r)
						}
					}()
					handle(msg.Payload)
				}()
			}
		}

		_ = pubsub.Close()
		if ctx.Err() != nil {
			return
		}
		log.Printf("cache: %q abunəsi qopdu — yenidən qoşulur", channel)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
		}
	}
}

// MGet — bir neçə açarı TƏK gedişdə oxuyur. Tapılmayanlar üçün nəticə map-ində
// açar OLMUR. Böyük siyahılarda (məs. 5000 üzvlü qrupun presence yoxlaması)
// açar-başına GET fəlakət olardı.
func (c *Client) MGet(ctx context.Context, fullKeys ...string) map[string]string {
	out := make(map[string]string, len(fullKeys))
	if !c.guard() || len(fullKeys) == 0 {
		return out
	}

	getCtx, cancel := context.WithTimeout(ctx, c.readTimeout)
	defer cancel()

	// Redis MGET-in arqument sayında praktik limiti var — partiyalara böl.
	const chunk = 512
	for start := 0; start < len(fullKeys); start += chunk {
		end := start + chunk
		if end > len(fullKeys) {
			end = len(fullKeys)
		}
		batch := fullKeys[start:end]

		vals, err := c.readRdb.MGet(getCtx, batch...).Result()
		c.observe("MGET", err)
		if err != nil {
			if err == redis.Nil {
				continue
			}
			return out // qismən nəticə — çağıran fail-open davranır
		}
		for i, v := range vals {
			if v == nil {
				continue
			}
			if s, ok := v.(string); ok {
				out[batch[i]] = s
			}
		}
	}
	return out
}
