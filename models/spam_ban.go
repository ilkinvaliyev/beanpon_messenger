package models

import (
	"encoding/json"
	"time"

	"gorm.io/gorm"
)

// spamMessageAction — spam_bans.actions içinde mesaj yazmayı yasaklayan action adı.
// beanpon (site) SpamService ile aynı: ['message', 'gift', 'post', 'comment', 'like'].
const spamMessageAction = "message"

// SpamBan beanpon (site) tarafından yönetilen spam_bans tablosunu temsil eder.
// Bu tablo messenger tarafından yalnızca OKUNUR — kayıt oluşturulmaz/güncellenmez.
type SpamBan struct {
	ID        uint           `gorm:"primaryKey" json:"id"`
	UserID    uint           `gorm:"not null;uniqueIndex" json:"user_id"`
	BannedBy  *uint          `json:"banned_by"`
	Reason    *string        `json:"reason"`
	Actions   *string        `gorm:"column:actions;type:json" json:"actions"` // null = hamısı banlı
	BannedAt  time.Time      `json:"banned_at"`
	ExpiresAt *time.Time     `json:"expires_at"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"deleted_at"`
}

// TableName tablo adını belirtir
func (SpamBan) TableName() string {
	return "spam_bans"
}

// IsMessagingBanned kullanıcının MESAJ YAZMA açısından spam ban'lı olup
// olmadığını kontrol eder.
//
// Bir kullanıcı şu durumda mesaj yazamaz:
//   - spam_bans tablosunda kaydı vardır VE
//   - deleted_at IS NULL (soft-delete edilmemiş) VE
//   - expires_at NULL'dur (kalıcı) VEYA expires_at gelecektedir (süresi dolmamış) VE
//   - actions NULL'dur (tüm aksiyonlar banlı)
//     VEYA actions dizisi "message" içerir.
//
// actions örnekleri:
//   - NULL                       → tüm aksiyonlar banlı  → mesaj YAZAMAZ
//   - ["post","comment"]         → "message" yok         → mesaj YAZABİLİR
//   - ["message","comment"]      → "message" var         → mesaj YAZAMAZ
//
// Bu mantık beanpon (site) SpamService::isBanned($userId, 'message') ile aynıdır.
// gorm.DeletedAt sayesinde deleted_at IS NULL filtresi sorguya otomatik eklenir.
func IsMessagingBanned(db *gorm.DB, userID uint) bool {
	var ban SpamBan
	err := db.Where("user_id = ?", userID).
		Where("expires_at IS NULL OR expires_at > ?", time.Now()).
		First(&ban).Error
	if err != nil {
		// Kayıt yok (veya soft-delete edilmiş / süresi dolmuş) → banlı değil
		return false
	}

	// actions NULL → tüm aksiyonlar banlı → mesaj yazamaz
	if ban.Actions == nil || *ban.Actions == "" {
		return true
	}

	// actions dolu → JSON dizisini parse et, "message" var mı bak
	var actions []string
	if err := json.Unmarshal([]byte(*ban.Actions), &actions); err != nil {
		// Bozuk JSON — güvenli tarafta kal: ban kaydı var, yasakla.
		return true
	}

	for _, a := range actions {
		if a == spamMessageAction {
			return true
		}
	}

	// actions dolu ama "message" yok → mesaj yazabilir
	return false
}
