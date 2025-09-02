package models

import (
	"gorm.io/gorm"
	"time"
)

type UserBlock struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	BlockerID uint      `gorm:"not null;index" json:"blocker_id"`
	BlockedID uint      `gorm:"not null;index" json:"blocked_id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName specify table name
func (UserBlock) TableName() string {
	return "user_blocks"
}

// IsBlocked iki kullanıcı arasında block var mı kontrol et
func IsBlocked(db *gorm.DB, userID1, userID2 uint) bool {
	var count int64
	db.Model(&UserBlock{}).Where(
		"(blocker_id = ? AND blocked_id = ?) OR (blocker_id = ? AND blocked_id = ?)",
		userID1, userID2, userID2, userID1,
	).Count(&count)

	return count > 0
}

// BlockUser kullanıcıyı block et
func BlockUser(db *gorm.DB, blockerID, blockedID uint) error {
	block := &UserBlock{
		BlockerID: blockerID,
		BlockedID: blockedID,
	}

	return db.FirstOrCreate(block, UserBlock{
		BlockerID: blockerID,
		BlockedID: blockedID,
	}).Error
}

// UnblockUser kullanıcıyı unblock et
func UnblockUser(db *gorm.DB, blockerID, blockedID uint) error {
	return db.Where("blocker_id = ? AND blocked_id = ?", blockerID, blockedID).
		Delete(&UserBlock{}).Error
}
