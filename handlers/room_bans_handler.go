package handlers

import (
	"log"
	"net/http"
	"time"

	"beanpon_messenger/database"
	"beanpon_messenger/models"
	"beanpon_messenger/utils"

	"github.com/gin-gonic/gin"
)

// Room entry bans (room_bans, Laravel migration 2026_10_10_200000).
//
// Removing a member from a room bans them: from then on the room does not
// exist for that user — every room endpoint answers 404 (loadVisibleRoom), the
// room is filtered from every list (discover, hidden rooms; chats and the share
// sheet are membership-based) and joining is impossible. Only admins see the
// room's ban list, and any admin can lift a ban; the user can then find and
// join the room again (not re-added automatically).

// roomNotBannedSQL — the viewer is not banned from rooms.id. Bind the viewer id.
const roomNotBannedSQL = `NOT EXISTS (
	SELECT 1 FROM room_bans rb WHERE rb.room_id = rooms.id AND rb.user_id = ?)`

func isRoomBanned(roomID, userID uint) bool {
	var n int64
	database.DB.Table("room_bans").Where("room_id = ? AND user_id = ?", roomID, userID).Count(&n)
	return n > 0
}

// banRoomUser — idempotent (a second ban keeps the first one's time/author).
func banRoomUser(roomID, userID, bannedBy uint) error {
	return database.DB.Exec(`
		INSERT INTO room_bans (room_id, user_id, banned_by, created_at, updated_at)
		VALUES (?, ?, ?, NOW(), NOW())
		ON CONFLICT (room_id, user_id) DO NOTHING`, roomID, userID, bannedBy).Error
}

// GET /api/v1/rooms/:room_id/bans?limit=&offset= — the room's banned users,
// newest ban first (admins only). Response: {bans: [...], total, has_more}.
func (h *RoomHandler) GetRoomBans(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	roomID := parseRoomID(c)
	if _, ok := loadVisibleRoom(c, roomID, userID); !ok {
		return
	}
	if !h.requireAdmin(c, roomID, userID) {
		return
	}
	limit := 50
	if v, ok := atoiPos(c.Query("limit")); ok && v > 0 && v <= 200 {
		limit = v
	}
	offset := 0
	if v, ok := atoiPos(c.Query("offset")); ok {
		offset = v
	}

	var total int64
	database.DB.Table("room_bans").Where("room_id = ?", roomID).Count(&total)

	var rows []struct {
		UserID           uint      `gorm:"column:user_id"`
		Name             string    `gorm:"column:name"`
		Username         string    `gorm:"column:username"`
		IsVerified       bool      `gorm:"column:is_verified"`
		ProfileImage     *string   `gorm:"column:profile_image"`
		BannedAt         time.Time `gorm:"column:banned_at"`
		BannedByUsername *string   `gorm:"column:banned_by_username"`
	}
	database.DB.Raw(`
		SELECT rb.user_id, u.name, u.username, u.is_verified, p.profile_image,
		       rb.created_at AS banned_at, bu.username AS banned_by_username
		FROM room_bans rb
		JOIN users u ON u.id = rb.user_id
		LEFT JOIN profiles p ON p.user_id = rb.user_id
		LEFT JOIN users bu ON bu.id = rb.banned_by
		WHERE rb.room_id = ?
		ORDER BY rb.created_at DESC, rb.id DESC
		LIMIT ? OFFSET ?`, roomID, limit, offset).Scan(&rows)

	out := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		out = append(out, gin.H{
			"user_id":            r.UserID,
			"name":               r.Name,
			"username":           r.Username,
			"is_verified":        r.IsVerified,
			"profile_image":      utils.PrependBaseURL(r.ProfileImage),
			"banned_at":          r.BannedAt,
			"banned_by_username": r.BannedByUsername,
		})
	}
	c.JSON(http.StatusOK, gin.H{"bans": out, "total": total, "has_more": len(rows) == limit})
}

// DELETE /api/v1/rooms/:room_id/bans/:user_id — lift a ban (admins only).
// Idempotent: no ban → still ok.
func (h *RoomHandler) UnbanRoomUser(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	roomID := parseRoomID(c)
	if _, ok := loadVisibleRoom(c, roomID, userID); !ok {
		return
	}
	if !h.requireAdmin(c, roomID, userID) {
		return
	}
	targetID := parseTargetUserID(c)
	if targetID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Yanlış istifadəçi"})
		return
	}
	if err := database.DB.Exec(`DELETE FROM room_bans WHERE room_id = ? AND user_id = ?`, roomID, targetID).Error; err != nil {
		log.Printf("[Room] unban failed room=%d target=%d: %v", roomID, targetID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Alınmadı"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// roomBanTargetAllowed — the owner can never be banned (an owner always
// belongs to the room); everyone else follows canModerate when still a member.
func roomBanTargetAllowed(room models.ChatRoom, actor, target *models.RoomMember, targetID uint) bool {
	if targetID == room.OwnerID {
		return false
	}
	if target == nil {
		// Already out of the room (left meanwhile): plain admins may ban too.
		return true
	}
	return canModerate(actor, target)
}
