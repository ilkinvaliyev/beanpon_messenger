package handlers

import (
	"errors"
	"strings"
	"unicode/utf8"

	"beanpon_messenger/models"

	"gorm.io/gorm"
)

// Room names are unique across live (not deleted) rooms, compared trimmed and
// case-insensitively: "Chat", " chat" and "CHAT" are one name. Create and
// rename take a per-name advisory lock and check inside one transaction, so
// two concurrent requests can't both win; the unique index rooms_name_unique_ci
// (Laravel migration 2026_10_10_200000) is the last line of defense.

var errRoomNameTaken = errors.New("room name is taken")

// roomNameLockClass — the first key of the two-key advisory lock, so these
// locks never share a key with single-key locks used elsewhere.
const roomNameLockClass = 7311

// cleanRoomName — the stored form (surrounding whitespace removed) and whether
// it is a valid name (1–255 characters, the rooms.name column size).
func cleanRoomName(raw string) (string, bool) {
	name := strings.TrimSpace(raw)
	return name, name != "" && utf8.RuneCountInString(name) <= 255
}

// lockRoomName — serializes create/rename of this name until tx ends.
func lockRoomName(tx *gorm.DB, name string) error {
	return tx.Exec(`SELECT pg_advisory_xact_lock(?, hashtext(?))`,
		roomNameLockClass, strings.ToLower(name)).Error
}

// claimRoomName — locks the name and fails with errRoomNameTaken when a live
// room other than exceptID (0 on create) already uses it. Call inside tx.
func claimRoomName(tx *gorm.DB, name string, exceptID uint) error {
	if err := lockRoomName(tx, name); err != nil {
		return err
	}
	var n int64
	if err := tx.Model(&models.ChatRoom{}).
		Where("lower(btrim(name)) = lower(btrim(?)) AND id <> ?", name, exceptID).
		Count(&n).Error; err != nil {
		return err
	}
	if n > 0 {
		return errRoomNameTaken
	}
	return nil
}

// isRoomNameConflict — our own check or the unique index refused the name
// (SQLSTATE 23505; the name index is the only unique key these writes can hit).
func isRoomNameConflict(err error) bool {
	if errors.Is(err, errRoomNameTaken) {
		return true
	}
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}
