package cache

import (
	"fmt"
	"time"
)

// Bu fayl messenger Redis key suffix-lərinin mərkəzi reyestridir.
//
// İki prefix istifadə olunur:
//
//   "bp:shared:" — Laravel ilə paylaşılan key-lər (spam_ban, user).
//                  Laravel öz tərəfində eyni suffix-lərə yazır.
//
//   "bp:msg:"    — Yalnız messenger daxili key-lər (hələlik istifadə yox).
//
// Hər funksiya yalnız suffix qaytarır — prefix Client.SharedKey() və ya
// Client.LocalKey() ilə əlavə olunur.

// ─── TTL sabitləri ─────────────────────────────────────────────

const (
	// SpamBan cache — Laravel banUser/unbanUser zamanı invalidate edir.
	// TTL safety net 5 dəqiqədir — Laravel invalidate etməsə də 5 dəq sonra
	// Redis özünü təmizləyir və növbəti oxumada DB-dən yenilənir.
	TTLSharedSpamBan = 5 * time.Minute
)

// ─── Paylaşılan key suffix-ləri ─────────────────────────────────

// SharedSpamBan — istifadəçinin spam_ban statusu.
//
// Dəyər formatı (JSON):
//
//	{
//	  "banned": true,
//	  "actions": ["post","story"]  // və ya null — "hamısı banlı"
//	}
//
//	Aktiv ban yoxdursa: {"banned": false}
//
// Laravel SharedSpamBanCache::forget() bu key-i invalidate edir.
//
//	bp:shared:spam_ban:{userId}
func SharedSpamBan(userID uint) string {
	return fmt.Sprintf("spam_ban:%d", userID)
}
