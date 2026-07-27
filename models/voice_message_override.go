package models

import "time"

// VoiceMessageOverride — sesli mesaj izninin kullanıcı-bazlı istisnası.
// (UserID, TargetUserID, Allowed) = "UserID, TargetUserID'nin ona sesli mesaj
// göndermesine Allowed izin veriyor". Satır yoksa global ayara (user_settings.
// allow_voice_messages) uyulur. Laravel migration ilə eyni cədvəl.
type VoiceMessageOverride struct {
	ID           uint      `json:"id" gorm:"primaryKey"`
	UserID       uint      `json:"user_id"`        // ayar sahibi (sesi ALAN)
	TargetUserID uint      `json:"target_user_id"` // izin/yasak uygulanan (sesi GÖNDEREN)
	Allowed      bool      `json:"allowed"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func (VoiceMessageOverride) TableName() string { return "voice_message_overrides" }
