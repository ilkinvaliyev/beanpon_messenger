package cache

import (
	"context"
	"encoding/json"
	"log"
)

// SpamBanPayload — Laravel SharedSpamBanCache::forget() tərəfindən yazılan
// JSON struktur. Eyni format hər iki tərəfdə oxunur/yazılır.
type SpamBanPayload struct {
	Banned  bool      `json:"banned"`
	Actions *[]string `json:"actions"` // nil = "hamısı banlı"
}

// BlocksMessaging — bu ban kayıt mesajlaşmanı qadağan edirmi?
//
// Qayda (models.spam_ban.go-dakı actionsBlocksMessaging ilə eyni):
//
//	banned=false                  → false (mesaja təsir yox)
//	banned=true, actions=nil      → true  (hamısı banlı, mesaj da)
//	banned=true, actions["message" var] → true
//	banned=true, actions["post"]  → false (yalnız post banlı, mesaj gedir)
func (p *SpamBanPayload) BlocksMessaging() bool {
	if p == nil || !p.Banned {
		return false
	}
	if p.Actions == nil {
		// actions = NULL → "hamısı banlı" konvensiyası
		return true
	}
	for _, a := range *p.Actions {
		if a == "message" {
			return true
		}
	}
	return false
}

// GetSpamBan — paylaşılan cache-dən istifadəçinin spam_ban statusunu oxuyur.
//
// Qaytarış semantikası:
//
//	(payload, true, nil)  — cache HIT, payload düzgün parse edildi
//	(nil, false, nil)     — cache MISS (key yoxdur və ya Redis disable/circuit open)
//	(nil, false, err)     — cache GET xətası (parse yox, şəbəkə vs.)
//
// Çağıran kod (məs. IsMessagingBannedByActions) MISS halında DB-yə düşüb
// nəticəni SetSpamBan ilə geri yazmalıdır.
func GetSpamBan(ctx context.Context, userID uint) (*SpamBanPayload, bool, error) {
	c := GetClient()
	if c == nil || !c.Enabled() {
		return nil, false, nil
	}

	readCtx, cancel := context.WithTimeout(ctx, c.readTimeout)
	defer cancel()

	key := c.SharedKey(SharedSpamBan(userID))
	raw, found, err := c.Get(readCtx, key)
	if err != nil || !found {
		return nil, false, err
	}

	var payload SpamBanPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		// Parse oluna bilmədi — cache-i atıb DB-yə düş. Stale dəyəri uzun
		// müddət saxlamamaq üçün invalidate də edirik.
		log.Printf("cache: SpamBanPayload parse xətası (user_id=%d, raw=%q): %v", userID, raw, err)
		_ = c.Del(ctx, key)
		return nil, false, nil
	}

	return &payload, true, nil
}

// SetSpamBan — DB-dən oxunan statusu paylaşılan cache-ə yazır.
// TTL safety net rolundadır — əsas invalidate-i Laravel banUser/unbanUser edir.
func SetSpamBan(ctx context.Context, userID uint, payload SpamBanPayload) error {
	c := GetClient()
	if c == nil || !c.Enabled() {
		return nil
	}

	data, err := json.Marshal(payload)
	if err != nil {
		// SpamBanPayload sadə bir struct-dur, marshal xətası demək olar ki,
		// mümkün deyil — amma yenə də müdafiə qatı.
		return err
	}

	writeCtx, cancel := context.WithTimeout(ctx, c.writeTimeout)
	defer cancel()

	key := c.SharedKey(SharedSpamBan(userID))
	return c.Set(writeCtx, key, string(data), TTLSharedSpamBan)
}

// InvalidateSpamBan — messenger özü ban yenilədikdə (gələcəkdə lazım olarsa)
// cache-i təmizləmək üçün. Hələlik invalidate-i Laravel edir.
func InvalidateSpamBan(ctx context.Context, userID uint) error {
	c := GetClient()
	if c == nil || !c.Enabled() {
		return nil
	}

	delCtx, cancel := context.WithTimeout(ctx, c.writeTimeout)
	defer cancel()

	key := c.SharedKey(SharedSpamBan(userID))
	return c.Del(delCtx, key)
}
