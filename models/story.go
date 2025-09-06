package models

import (
	"gorm.io/gorm"
	"time"
)

type Story struct {
	ID        uint           `json:"id" gorm:"primaryKey"`
	UserID    uint           `json:"user_id" gorm:"not null;index"`
	Type      string         `json:"type" gorm:"not null"` // text, image, video
	MediaURL  string         `json:"media_url"`
	Content   *string        `json:"content"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at" gorm:"index"`

	// İlişki
	User User `json:"user" gorm:"foreignKey:UserID"`
}

// Tablo adını belirt
func (Story) TableName() string {
	return "stories"
}
