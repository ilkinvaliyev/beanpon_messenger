package models

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"time"

	"gorm.io/gorm"

	"beanpon_messenger/cache"
)

// IsMessagingBanned kullanıcının spam ban'ı olup olmadığını kontrol eder.
//
// Kural (basit ve net):
//
//	spam_bans tablosunda bu user_id'ye ait bir kayıt varsa VE
//	o kaydın deleted_at sütunu NULL ise → kullanıcı spam'dir, mesaj yazamaz.
//
// Önemli tasarım kararları:
//   - actions / expires_at sütunlarına BAKILMAZ. Yalnızca "kayıt var mı +
//     deleted_at null mu" yeterlidir. Böylece bu sütunlar veritabanında
//     mevcut olmasa bile (migration uyumsuzluğu) sorgu kırılmaz.
//   - SELECT yalnızca "id" sütununu çeker — SELECT * kullanılsaydı, gorm
//     struct'taki tüm alanları (actions vb.) sorgular ve eksik bir sütun
//     "column does not exist" SQL hatası verirdi. Bu hata önceki versiyonda
//     fonksiyonun sessizce false dönmesine ve spam kullanıcının geçmesine
//     yol açıyordu.
//   - deleted_at IS NULL filtresi açıkça yazılır (raw Count sorgusu).
//
// spam_bans tablosu beanpon (site) tarafından yönetilir; messenger yalnızca okur.
func IsMessagingBanned(db *gorm.DB, userID uint) bool {
	// CACHE-FIRST: paylaşılan spam_ban payload-undan "Banned" sahəsini oxu.
	// Cache HIT-də DB-yə getmirik.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if payload, hit, _ := cache.GetSpamBan(ctx, userID); hit && payload != nil {
		return payload.Banned
	}

	// DB FALLBACK
	var count int64
	err := db.Table("spam_bans").
		Where("user_id = ?", userID).
		Where("deleted_at IS NULL").
		Count(&count).Error

	if err != nil {
		// Beklenmeyen DB hatası (bağlantı vb.) — logla. Kullanıcıyı
		// engellemiyoruz ki, geçici bir DB sorunu tüm yeni sohbetleri
		// kilitlemesin; ama hata görünür olsun diye loglanır.
		log.Printf("IsMessagingBanned: spam_bans sorgusu başarısız (user_id=%d): %v", userID, err)
		return false
	}

	return count > 0
}

// IsMessagingBannedByActions — istifadəçinin spam_bans-da aktiv qeydi varsa
// VƏ həmin qeydin `actions` sütunu mesaj göndərməni qadağan edirsə true qaytarır.
//
// Qayda (yalnız İLK conversation yaradılması üçün istifadə olunur):
//
//	spam_bans-da deleted_at IS NULL olan qeyd var VƏ:
//	  • actions sütunu NULL-dur                        → mesaj GETMƏSİN (true)
//	  • actions JSON массivində "message" var          → mesaj GETMƏSİN (true)
//	  • actions var amma "message" yoxdur (məs. ["post"]) → mesaj GEDƏ BİLƏR (false)
//	Aktiv qeyd yoxdursa → mesaj GEDƏ BİLƏR (false).
//
// IsMessagingBanned-dən fərqi: o, actions-a baxmır (sadəcə "qeyd var?").
// Bu funksiya isə actions-ın məzmununu da nəzərə alır. Mövcud
// IsMessagingBanned dəyişdirilmir — başqa yerlərdə işlənir.
//
// actions sütunu Laravel-də JSON tipindədir; burada xam mətn kimi oxunub
// parse olunur. Sütun ümumiyyətlə mövcud deyilsə (köhnə DB) — SQL xətası
// alınır, fail-open: false qaytarırıq ki, müvəqqəti problem bütün yeni
// söhbətləri kilitləməsin.
func IsMessagingBannedByActions(db *gorm.DB, userID uint) bool {
	// ──── CACHE-FIRST LOOKUP ──────────────────────────────────────
	// Laravel SharedSpamBanCache::forget() banUser/unbanUser zamanı bu key-i
	// yenidən yazır. Buradakı oxuma demək olar ki, hər zaman HIT olur —
	// yalnız ban statusu yeni dəyişib və TTL-də olmayan halda DB-yə düşür.
	// Cache disable və ya circuit open olduqda da DB fallback-i tetiklenir.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	if payload, hit, _ := cache.GetSpamBan(ctx, userID); hit && payload != nil {
		return payload.BlocksMessaging()
	}

	// ──── DB FALLBACK ─────────────────────────────────────────────
	type spamRow struct {
		Actions *string `gorm:"column:actions"`
	}

	var rows []spamRow
	err := db.Table("spam_bans").
		Select("actions").
		Where("user_id = ?", userID).
		Where("deleted_at IS NULL").
		Scan(&rows).Error

	if err != nil {
		log.Printf("IsMessagingBannedByActions: spam_bans sorgusu başarısız (user_id=%d): %v", userID, err)
		return false
	}

	// Aktiv qeyd yoxdur — mesaj gedə bilər. Cache-ə "banned:false" yaz ki,
	// növbəti oxuma da HIT olsun.
	if len(rows) == 0 {
		writeSpamBanCache(userID, cache.SpamBanPayload{Banned: false})
		return false
	}

	// İstifadəçinin birdən çox aktiv qeydi ola bilər — hər hansı biri
	// mesajı qadağan edirsə, qadağandır.
	blocked := false
	// Cache üçün təmsilçi payload — birdən çox satır varsa hər hansı birini
	// götürürük (mesajı bloklayanı üstün tut). Laravel-in yazdığı payload
	// kanonik versiyadır; bu sadəcə fallback warm-cache-dir.
	cachePayload := cache.SpamBanPayload{Banned: true, Actions: nil}
	for _, r := range rows {
		if actionsBlocksMessaging(r.Actions) {
			blocked = true
			// Bloklayan satır üçün actions sahəsini cache-ə yaz (NULL ola bilər).
			if r.Actions != nil {
				var arr []string
				if err := json.Unmarshal([]byte(*r.Actions), &arr); err == nil {
					cachePayload.Actions = &arr
				}
			}
			break
		}
	}
	if !blocked && len(rows) > 0 {
		// Heç bir satır bloklamır — amma aktiv qeyd var (məs. yalnız "post" banlı).
		// Cache-ə actions massivinin birini yaz (qaytarış BlocksMessaging false olsun).
		if rows[0].Actions != nil {
			var arr []string
			if err := json.Unmarshal([]byte(*rows[0].Actions), &arr); err == nil {
				cachePayload.Actions = &arr
			}
		}
	}
	writeSpamBanCache(userID, cachePayload)
	return blocked
}

// writeSpamBanCache — DB-dən oxunan nəticəni paylaşılan Redis-ə yazır.
// Yazma uğursuz olsa belə əsas funksionallıq pozulmur — bu sadəcə optimizasiyadır.
func writeSpamBanCache(userID uint, payload cache.SpamBanPayload) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := cache.SetSpamBan(ctx, userID, payload); err != nil {
		log.Printf("writeSpamBanCache: SetSpamBan uğursuz (user_id=%d): %v", userID, err)
	}
}

// actionsBlocksMessaging — `actions` JSON dəyəri mesaj göndərməni
// qadağan edirmi?
//
//	nil / "" / "null"                  → true  (actions yoxdur → qadağan)
//	JSON массiv, "message" elementi var → true
//	JSON массiv, "message" yoxdur       → false
func actionsBlocksMessaging(raw *string) bool {
	// actions sütunu NULL — qaydaya görə mesaj getməsin.
	if raw == nil {
		return true
	}

	trimmed := strings.TrimSpace(*raw)
	if trimmed == "" || trimmed == "null" {
		return true
	}

	// actions JSON массiv kimi parse olunur: ["post","message"] və s.
	var actions []string
	if err := json.Unmarshal([]byte(trimmed), &actions); err != nil {
		// Parse olunmadı — gözlənilməz format. Təhlükəsiz tərəfə keç:
		// qeyd var deməkdir, mesajı qadağan et.
		log.Printf("actionsBlocksMessaging: actions parse edilə bilmədi (%q): %v", trimmed, err)
		return true
	}

	for _, a := range actions {
		if strings.EqualFold(strings.TrimSpace(a), "message") {
			return true
		}
	}
	return false
}
