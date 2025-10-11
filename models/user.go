package models

import (
	"time"
)

type User struct {
	ID            int64     `json:"id" gorm:"primaryKey;autoIncrement"`
	Name          string    `json:"name" gorm:"type:varchar(255);not null"`
	Username      string    `json:"username" gorm:"type:varchar(255);uniqueIndex;not null"`
	Email         string    `json:"email" gorm:"type:varchar(255);uniqueIndex;not null"`
	ProfileImage  *string   `json:"profile_image" gorm:"type:text"`
	AccountTypeID int       `json:"account_type_id" gorm:"default:1"`
	IsVerified    bool      `json:"is_verified" gorm:"default:false"`
	CreatedAt     time.Time `json:"created_at" gorm:"autoCreateTime"`
	UpdatedAt     time.Time `json:"updated_at" gorm:"autoUpdateTime"`
}

func (User) TableName() string {
	return "users"
}
