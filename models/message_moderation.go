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

// CategoryMinConfidence — bir kateqoriyada TABLE-yə LOG yazılması üçün
// AI-nın minimum əminlik dərəcəsi. AI flaq etsə belə, confidence bu həddən
// aşağıdırsa nəticə tamamilə ATILIR (nə log, nə notification).
//
// Məntiq:
//   - csae / anti_state — kritik. Aşağı eşik (0.50): şübhə varsa belə tut.
//   - threat / self_harm / illegal_goods — ciddi. Orta eşik (0.65).
//   - harassment / scam — yüngül, yanlış-pozitiv ehtimalı yüksək (0.75).
//   - off_platform — aşağı eşik (0.50): həm açıq köçürmə təklifi, həm
//     dolayı maraq ("instada varsan?") table-yə düşməlidir. Notification
//     gedib-getməməsi isə AYRI eşiklə (NotificationMinConfidence) idarə
//     olunur — yalnız yüksək əminlikli açıq təkliflərə push gedir.
var CategoryMinConfidence = map[string]float64{
	CategoryCSAE:         0.50,
	CategoryAntiState:    0.50,
	CategoryThreat:       0.65,
	CategorySelfHarm:     0.65,
	CategoryIllegalGoods: 0.65,
	CategoryHarassment:   0.75,
	CategoryScam:         0.75,
	CategoryOffPlatform:  0.50,
}

// DefaultMinConfidence — kateqoriya xəritədə yoxdursa istifadə olunan log eşiyi.
const DefaultMinConfidence = 0.70

// MeetsConfidenceThreshold — verilən kateqoriya və əminlik nəticəni
// TABLE-yə LOG kimi yazmağa kifayət edirmi?
func MeetsConfidenceThreshold(category string, confidence float64) bool {
	threshold, ok := CategoryMinConfidence[category]
	if !ok {
		threshold = DefaultMinConfidence
	}
	return confidence >= threshold
}

// NotificationMinConfidence — bir kateqoriyada istifadəçiyə PUSH/WS
// xəbərdarlıq notification-ı GÖNDƏRİLMƏSİ üçün minimum əminlik.
//
// Bu, log eşiyindən AYRIDIR və ondan YÜKSƏKDİR: nəticə table-yə düşə bilər
// (audit üçün), amma istifadəçini yalnız AI həqiqətən əmin olduqda
// narahat edirik.
//
//   - off_platform — yalnız confidence >= 0.90 (AÇIQ köçürmə təklifi)
//     olduqda push gedir. "instada varsan?" kimi dolayı maraq (0.55-0.75)
//     table-yə yazılır, amma notification getmir.
var NotificationMinConfidence = map[string]float64{
	CategoryOffPlatform: 0.90,
}

// ShouldNotify — bu kateqoriya və əminlik istifadəçiyə notification
// göndərməyə kifayət edirmi? Kateqoriya xəritədə yoxdursa, log eşiyi
// (MeetsConfidenceThreshold) ilə eyni davranır.
func ShouldNotify(category string, confidence float64) bool {
	threshold, ok := NotificationMinConfidence[category]
	if !ok {
		return MeetsConfidenceThreshold(category, confidence)
	}
	return confidence >= threshold
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
