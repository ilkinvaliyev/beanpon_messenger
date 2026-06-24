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

// GetBlockedUserIDs — userID ilə hər hansı istiqamətdə (bloklayan VƏ ya
// bloklanan) block əlaqəsi olan bütün qarşı tərəf ID-lərini TEK sorğuda qaytarır.
//
// Niyə: söhbət siyahısı kimi yerlərdə hər element üçün ayrıca IsBlocked çağırmaq
// N+1 problemi yaradır (50 söhbət = 50 sorğu). Bunun əvəzinə bu funksiya bir
// dəfə çağırılır, nəticə map-ə qoyulur və yoxlama yaddaşda (O(1)) edilir.
//
// İstifadə:
//
//	blocked := models.GetBlockedUserIDs(db, myID)
//	if blocked[otherUserID] { /* bloklu */ }
func GetBlockedUserIDs(db *gorm.DB, userID uint) map[uint]bool {
	var ids []uint
	// İki istiqaməti UNION ilə birləşdiririk: userID-nin blokladıqları +
	// userID-ni bloklayanlar. Nəticə: qarşı tərəfin ID-ləri.
	db.Raw(`
		SELECT blocked_id AS other_id FROM user_blocks WHERE blocker_id = ?
		UNION
		SELECT blocker_id AS other_id FROM user_blocks WHERE blocked_id = ?
	`, userID, userID).Scan(&ids)

	result := make(map[uint]bool, len(ids))
	for _, id := range ids {
		result[id] = true
	}
	return result
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
