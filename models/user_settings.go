package models

import (
	"time"
)

type UserSettings struct {
	ID              uint      `json:"id" gorm:"primaryKey;autoIncrement"`
	UserID          uint      `json:"user_id" gorm:"uniqueIndex;not null"`
	MessageRequests string    `json:"message_requests" gorm:"type:enum('ALL','ONLY_VERIFIED');default:'ALL'"`
	CreatedAt       time.Time `json:"created_at" gorm:"autoCreateTime"`
	UpdatedAt       time.Time `json:"updated_at" gorm:"autoUpdateTime"`
}

func (UserSettings) TableName() string {
	return "user_settings"
}
