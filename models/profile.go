package models

import "time"

type Profile struct {
	ID           int64   `json:"id" gorm:"primaryKey;autoIncrement"`
	UserID       int64   `json:"user_id" gorm:"not null;uniqueIndex"`
	ProfileImage *string `json:"profile_image" gorm:"type:text"`
	Bio          *string `json:"bio" gorm:"type:text"`
	// Add other profile fields as needed
	CreatedAt        time.Time `json:"created_at" gorm:"autoCreateTime"`
	UpdatedAt        time.Time `json:"updated_at" gorm:"autoUpdateTime"`
	ProfileImageType *string   `json:"profile_image_type" gorm:"type:varchar(20);default:'image'"`

	User User `json:"user" gorm:"foreignKey:UserID"`
}

func (Profile) TableName() string {
	return "profiles"
}
