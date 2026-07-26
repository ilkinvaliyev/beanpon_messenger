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

// ─── Lokal (messenger daxili) key suffix-ləri ───────────────────

const (
	// TTLWSPresence — Issue 4: paylaşılan WebSocket presence qeydinin ömrü.
	// Heartbeat bunun YARISINDAN tez-tez yeniləyir, ona görə sağlam instansda
	// qeyd heç vaxt bitmir. İnstans qəflətən ölsə (SIGKILL, OOM, node itkisi)
	// qeyd bu müddətdən sonra öz-özünə yox olur — "zombi onlayn" qalmır.
	TTLWSPresence = 90 * time.Second

	// WSPresenceRefresh — heartbeat dövrü (TTL-in ~1/3-ü).
	WSPresenceRefresh = 30 * time.Second
)

// WSFanout — Issue 4: instance-lar arası canlı yayım kanalı.
//
//	bp:msg:ws:fanout
func WSFanout() string {
	return "ws:fanout"
}

// WSPresence — istifadəçinin hansı instansda olduğu + açıq çat konteksti.
//
// Dəyər formatı (JSON):
//
//	{"i":"<instance-id>","dm":123,"grp":456,"at":1234567890}
//
//	dm  — hazırda açıq DM-in qarşı tərəf user id-si (0 = yoxdur)
//	grp — hazırda açıq qrupun conversation id-si (0 = yoxdur)
//
//	bp:msg:ws:presence:{userId}
func WSPresence(userID uint) string {
	return fmt.Sprintf("ws:presence:%d", userID)
}
