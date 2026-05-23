package models

import (
	"time"

	"gorm.io/gorm"
)

// ─────────────────────────────────────────────────────────────────────────────
// Moderasiya kateqoriyaları
//
// AI mesajı analiz edib bu kateqoriyalardan birini (və ya heç birini) qaytarır.
// Hər kateqoriyanın öz risk səviyyəsi və arxa-plan davranışı var.
// ─────────────────────────────────────────────────────────────────────────────
const (
	// CategoryThreat — birisi digərini hədələyir (zərər, zorakılıq və s.).
	CategoryThreat = "threat"

	// CategoryIllegalGoods — qanunsuz alqı-satqı: narkotik, silah, saxta sənəd.
	CategoryIllegalGoods = "illegal_goods"

	// CategoryAntiState — dövlət əleyhinə fəaliyyət/təşviq, terror təbliğatı.
	CategoryAntiState = "anti_state"

	// CategoryOffPlatform — söhbəti tətbiqdən kənara çıxarma cəhdi
	// (Instagram, TikTok, Telegram, WhatsApp, Facebook və s.).
	// Yeganə kateqoriya ki, GÖNDƏRƏNƏ xəbərdarlıq notification-ı gedir.
	CategoryOffPlatform = "off_platform"

	// CategoryHarassment — təkrarlanan təcavüz, təhqir, aşağılama.
	CategoryHarassment = "harassment"

	// CategoryScam — dələduzluq, fırıldaq, saxta qazanc vədi.
	CategoryScam = "scam"

	// CategoryCSAE — uşaq təhlükəsizliyi. Ən yüksək prioritet.
	CategoryCSAE = "csae"

	// CategorySelfHarm — özünə zərər / intihar riski.
	CategorySelfHarm = "self_harm"
)

// Risk səviyyələri
const (
	SeverityLow      = "low"
	SeverityMedium   = "medium"
	SeverityHigh     = "high"
	SeverityCritical = "critical"
)

// CategorySeverity — hər kateqoriyanın standart risk səviyyəsi.
// AI confidence-i yüksəkdirsə severity worker tərəfindən artırıla bilər.
var CategorySeverity = map[string]string{
	CategoryThreat:       SeverityHigh,
	CategoryIllegalGoods: SeverityHigh,
	CategoryAntiState:    SeverityCritical,
	CategoryOffPlatform:  SeverityLow,
	CategoryHarassment:   SeverityMedium,
	CategoryScam:         SeverityMedium,
	CategoryCSAE:         SeverityCritical,
	CategorySelfHarm:     SeverityHigh,
}

// ValidCategories — AI-dan gələn dəyəri yoxlamaq üçün set.
var ValidCategories = map[string]bool{
	CategoryThreat:       true,
	CategoryIllegalGoods: true,
	CategoryAntiState:    true,
	CategoryOffPlatform:  true,
	CategoryHarassment:   true,
	CategoryScam:         true,
	CategoryCSAE:         true,
	CategorySelfHarm:     true,
}

// IsValidCategory — AI-nın qaytardığı kateqoriya tanınırmı?
func IsValidCategory(c string) bool {
	return ValidCategories[c]
}

// MessageModerationLog — şübhəli mesaj qeydi.
//
// Tablo Laravel migration ilə yaradılır (2026_05_23_..._create_message_moderation_logs_table.php).
// Messenger servisi YALNIZ yazır; Filament/admin oxuyur.
// Təmiz mesajlar üçün heç bir sətir yazılmır.
type MessageModerationLog struct {
	ID         uint    `json:"id" gorm:"primaryKey"`
	MessageID  *string `json:"message_id" gorm:"type:uuid;index"`
	SenderID   uint    `json:"sender_id" gorm:"not null;index"`
	ReceiverID *uint   `json:"receiver_id" gorm:"index"`

	Category   string  `json:"category" gorm:"type:varchar(40);not null;index"`
	Confidence float64 `json:"confidence" gorm:"type:decimal(4,2);default:0"`
	Severity   string  `json:"severity" gorm:"type:varchar(16);default:'medium';index"`

	Reason  string `json:"reason" gorm:"type:text"`
	Snippet string `json:"snippet" gorm:"type:varchar(500)"`

	// ActionsTaken — JSON массив string kimi saxlanır, məs. ["notify_sender"].
	ActionsTaken   string `json:"actions_taken" gorm:"type:json"`
	SenderNotified bool   `json:"sender_notified" gorm:"default:false"`

	IsReviewed bool       `json:"is_reviewed" gorm:"default:false;index"`
	ReviewedBy *uint      `json:"reviewed_by"`
	ReviewedAt *time.Time `json:"reviewed_at"`

	AIRawResponse string `json:"ai_raw_response" gorm:"type:json"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName — Laravel migration-dakı tablo adı ilə uyğunlaşdırır.
func (MessageModerationLog) TableName() string {
	return "message_moderation_logs"
}

// BeforeCreate — yaradılma anında severity boşdursa kateqoriyaya görə doldur.
func (m *MessageModerationLog) BeforeCreate(tx *gorm.DB) error {
	if m.Severity == "" {
		if sev, ok := CategorySeverity[m.Category]; ok {
			m.Severity = sev
		} else {
			m.Severity = SeverityMedium
		}
	}
	return nil
}
