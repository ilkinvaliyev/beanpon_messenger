package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"beanpon_messenger/database"
	"beanpon_messenger/models"
	"beanpon_messenger/utils"

	"github.com/gin-gonic/gin"
)

// Room moderation, privacy and visibility helpers + endpoints:
//   - owner-block cascade: a user blocked with the room owner (either way)
//     never sees the owner's rooms (404 everywhere, filtered from every list);
//   - per-user hide (room_hides): a hidden room shows nowhere until unhidden;
//   - admin tools: kick, write block (room_write_blocks), screenshot switch,
//     messaging lock (admins only / 8 h / 24 h / until reopened);
//   - join/leave timeline events (room_events), visible only to the users who
//     were members at that moment;
//   - member-only message search and the share-sheet room ranking.

// roomOwnerNotBlockedSQL — the viewer and rooms.owner_id have no block in
// either direction. Bind the viewer id twice.
const roomOwnerNotBlockedSQL = `NOT EXISTS (
	SELECT 1 FROM user_blocks ub
	WHERE (ub.blocker_id = ? AND ub.blocked_id = rooms.owner_id)
	   OR (ub.blocker_id = rooms.owner_id AND ub.blocked_id = ?))`

// roomNotHiddenSQL — the viewer did not hide the room. Bind the viewer id.
const roomNotHiddenSQL = `NOT EXISTS (
	SELECT 1 FROM room_hides rh WHERE rh.room_id = rooms.id AND rh.user_id = ?)`

// ownerBlockedWith — the viewer and the room owner blocked each other (either way).
func ownerBlockedWith(ownerID, userID uint) bool {
	if ownerID == userID {
		return false
	}
	var n int64
	database.DB.Table("user_blocks").
		Where("(blocker_id = ? AND blocked_id = ?) OR (blocker_id = ? AND blocked_id = ?)",
			userID, ownerID, ownerID, userID).
		Count(&n)
	return n > 0
}

// loadVisibleRoom — loads the room for this viewer. A missing room and a room
// whose owner is blocked with the viewer both answer 404: for that viewer the
// room does not exist. Writes the error response and returns ok=false.
func loadVisibleRoom(c *gin.Context, roomID, userID uint) (*models.ChatRoom, bool) {
	var room models.ChatRoom
	if err := database.DB.First(&room, roomID).Error; err != nil || ownerBlockedWith(room.OwnerID, userID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Otaq tapılmadı", "code": "ROOM_NOT_FOUND"})
		return nil, false
	}
	return &room, true
}

func isRoomHidden(roomID, userID uint) bool {
	var n int64
	database.DB.Table("room_hides").Where("room_id = ? AND user_id = ?", roomID, userID).Count(&n)
	return n > 0
}

func isWriteBlocked(roomID, userID uint) bool {
	var n int64
	database.DB.Table("room_write_blocks").Where("room_id = ? AND user_id = ?", roomID, userID).Count(&n)
	return n > 0
}

// lockUntilIfActive — the lock end for an active timed lock, nil otherwise
// (no lock, expired lock, or "until reopened").
func lockUntilIfActive(r models.ChatRoom) *time.Time {
	if !r.IsMessagingLocked(time.Now()) {
		return nil
	}
	return r.MessagingLockedUntil
}

// roomMemberCounts — current member count per room (one grouped query).
func roomMemberCounts(roomIDs []uint) map[uint]int {
	out := map[uint]int{}
	if len(roomIDs) == 0 {
		return out
	}
	var rows []struct {
		RoomID uint
		N      int
	}
	database.DB.Model(&models.RoomMember{}).
		Select("room_id, COUNT(*) AS n").
		Where("room_id IN ?", roomIDs).Group("room_id").Scan(&rows)
	for _, r := range rows {
		out[r.RoomID] = r.N
	}
	return out
}

// canModerate — the owner may act on anyone but the owner; an admin only on
// plain members (never on another admin or the owner).
func canModerate(actor, target *models.RoomMember) bool {
	if target.IsOwner() {
		return false
	}
	if actor.IsOwner() {
		return true
	}
	return actor.IsAdmin() && !target.HasAdminAccess()
}

func parseTargetUserID(c *gin.Context) uint {
	id, _ := strconv.ParseUint(c.Param("user_id"), 10, 32)
	return uint(id)
}

// roomDetailResponse — RoomResponse + the viewer's own state (role, write
// block, notification mute, hidden, joined_at, can_write) for the room screen
// and its detail page.
func (h *RoomHandler) roomDetailResponse(room models.ChatRoom, userID uint) models.RoomResponse {
	mem := roomMembership(room.ID, userID)
	var rolePtr *string
	isMember := mem != nil
	if mem != nil {
		rolePtr = strPtr(mem.Role)
	} else if room.OwnerID == userID {
		// The creator is always a member (self-heal, same as ListRooms).
		rolePtr = strPtr("owner")
		isMember = true
	}
	resp := h.toRoomResponse(room, rolePtr, isMember)
	resp.MemberCount = roomMemberCounts([]uint{room.ID})[room.ID]
	resp.IsHidden = isRoomHidden(room.ID, userID)
	isAdmin := rolePtr != nil && (*rolePtr == "owner" || *rolePtr == "admin")
	if mem != nil {
		resp.IsMuted = mem.IsMuted && (mem.MutedUntil == nil || mem.MutedUntil.After(time.Now()))
		resp.JoinedAt = mem.JoinedAt
	}
	resp.WriteBlocked = !isAdmin && isWriteBlocked(room.ID, userID)
	resp.CanWrite = !room.IsFrozen && !resp.WriteBlocked && (isAdmin || !resp.MessagingLocked)
	return resp
}

// recordRoomEvent — stores a join/leave line and pushes it live to the users
// who are members right now (the only ones allowed to see it). Users blocked
// with the subject and users who hid the room are skipped. Best-effort: a
// failure never breaks the join/leave itself.
func (h *RoomHandler) recordRoomEvent(roomID, userID uint, kind string, at time.Time) {
	ev := models.RoomEvent{RoomID: roomID, UserID: userID, Kind: kind, CreatedAt: at}
	if err := database.DB.Create(&ev).Error; err != nil {
		log.Printf("[Room] event insert failed room=%d user=%d kind=%s: %v", roomID, userID, kind, err)
		return
	}
	var u models.User
	database.DB.Select("id, name, username").First(&u, userID)
	payload := models.RoomEventResponse{
		ID:        fmt.Sprintf("evt_%d", ev.ID),
		RoomID:    roomID,
		Kind:      kind,
		UserID:    userID,
		Username:  u.Username,
		Name:      u.Name,
		CreatedAt: at,
	}
	h.wsHub.SendToMultipleUsers(h.roomMemberIDsExcludingBlocked(roomID, userID), "room_member_event", payload)
}

// roomEventsForViewer — join/leave lines in [from, to) the viewer may see:
// only events at or after the viewer's own joined_at (they were a member at
// that moment), no events of users blocked with the viewer.
func roomEventsForViewer(roomID, viewerID uint, joinedAt, from, to time.Time) []models.RoomEventResponse {
	if from.Before(joinedAt) {
		from = joinedAt
	}
	var rows []struct {
		ID        uint      `gorm:"column:id"`
		Kind      string    `gorm:"column:kind"`
		UserID    uint      `gorm:"column:user_id"`
		Username  string    `gorm:"column:username"`
		Name      string    `gorm:"column:name"`
		CreatedAt time.Time `gorm:"column:created_at"`
	}
	database.DB.Raw(`
		SELECT e.id, e.kind, e.user_id, u.username, u.name, e.created_at
		FROM room_events e
		JOIN users u ON u.id = e.user_id
		WHERE e.room_id = ?
		  AND e.created_at >= ? AND e.created_at < ?
		  AND NOT EXISTS (
		      SELECT 1 FROM user_blocks ub
		      WHERE (ub.blocker_id = ? AND ub.blocked_id = e.user_id)
		         OR (ub.blocker_id = e.user_id AND ub.blocked_id = ?)
		  )
		ORDER BY e.created_at ASC, e.id ASC
		LIMIT 300
	`, roomID, from, to, viewerID, viewerID).Scan(&rows)
	out := make([]models.RoomEventResponse, 0, len(rows))
	for _, r := range rows {
		out = append(out, models.RoomEventResponse{
			ID:        fmt.Sprintf("evt_%d", r.ID),
			RoomID:    roomID,
			Kind:      r.Kind,
			UserID:    r.UserID,
			Username:  r.Username,
			Name:      r.Name,
			CreatedAt: r.CreatedAt,
		})
	}
	return out
}

// --- Admin: kick / write block / settings ---

// DELETE /api/v1/rooms/:room_id/members/:user_id — remove a member (admin).
// The removed user may join again; a write block survives (room_write_blocks).
func (h *RoomHandler) KickRoomMember(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	roomID := parseRoomID(c)
	actor := roomMembership(roomID, userID)
	if actor == nil || !actor.HasAdminAccess() {
		c.JSON(http.StatusForbidden, gin.H{"error": "İcazə yoxdur"})
		return
	}
	targetID := parseTargetUserID(c)
	if targetID == 0 || targetID == userID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Yanlış istifadəçi"})
		return
	}
	target := roomMembership(roomID, targetID)
	if target == nil {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
		return
	}
	if !canModerate(actor, target) {
		c.JSON(http.StatusForbidden, gin.H{"error": "İcazə yoxdur"})
		return
	}
	database.DB.Delete(&models.RoomMember{}, target.ID)
	h.recordRoomEvent(roomID, targetID, "leave", time.Now())
	// The removed user's open room screen falls back to the reader state.
	h.wsHub.SendToUser(targetID, "room_member_removed", gin.H{"room_id": roomID})
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// PUT /api/v1/rooms/:room_id/members/:user_id/write-block — body {blocked}.
// Admin blocks / unblocks a member from writing in the room.
func (h *RoomHandler) SetRoomWriteBlock(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	roomID := parseRoomID(c)
	actor := roomMembership(roomID, userID)
	if actor == nil || !actor.HasAdminAccess() {
		c.JSON(http.StatusForbidden, gin.H{"error": "İcazə yoxdur"})
		return
	}
	targetID := parseTargetUserID(c)
	target := roomMembership(roomID, targetID)
	if target == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Üzv tapılmadı"})
		return
	}
	// Admins always write, so a write block only makes sense for members.
	if !canModerate(actor, target) || target.HasAdminAccess() {
		c.JSON(http.StatusForbidden, gin.H{"error": "İcazə yoxdur"})
		return
	}
	var body struct {
		Blocked bool `json:"blocked"`
	}
	_ = c.ShouldBindJSON(&body)
	var err error
	if body.Blocked {
		err = database.DB.Exec(`
			INSERT INTO room_write_blocks (room_id, user_id, blocked_by, created_at, updated_at)
			VALUES (?, ?, ?, NOW(), NOW())
			ON CONFLICT (room_id, user_id) DO NOTHING`, roomID, targetID, userID).Error
	} else {
		err = database.DB.Exec(`DELETE FROM room_write_blocks WHERE room_id = ? AND user_id = ?`, roomID, targetID).Error
	}
	if err != nil {
		log.Printf("[Room] write-block failed room=%d target=%d: %v", roomID, targetID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Alınmadı"})
		return
	}
	h.wsHub.SendToUser(targetID, "room_member_updated", gin.H{
		"room_id": roomID, "user_id": targetID, "role": target.Role, "write_blocked": body.Blocked,
	})
	c.JSON(http.StatusOK, gin.H{"status": "ok", "write_blocked": body.Blocked})
}

// PUT /api/v1/rooms/:room_id/settings — admin room switches. Body (all optional):
//
//	screenshot_disabled bool  — screenshots off for the room chat
//	messaging_locked    bool  — only admins can write
//	lock_hours          int   — with messaging_locked=true: 8 / 24; 0 or
//	                            missing = until an admin reopens it
func (h *RoomHandler) UpdateRoomSettings(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	roomID := parseRoomID(c)
	if !h.requireAdmin(c, roomID, userID) {
		return
	}
	var req struct {
		ScreenshotDisabled *bool `json:"screenshot_disabled"`
		MessagingLocked    *bool `json:"messaging_locked"`
		LockHours          *int  `json:"lock_hours"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Yanlış məlumat"})
		return
	}
	updates := map[string]interface{}{}
	if req.ScreenshotDisabled != nil {
		updates["screenshot_disabled"] = *req.ScreenshotDisabled
	}
	if req.MessagingLocked != nil {
		updates["messaging_locked"] = *req.MessagingLocked
		updates["messaging_locked_until"] = nil
		if *req.MessagingLocked && req.LockHours != nil && *req.LockHours > 0 {
			hours := *req.LockHours
			if hours > 24*30 {
				hours = 24 * 30
			}
			updates["messaging_locked_until"] = time.Now().Add(time.Duration(hours) * time.Hour)
		}
	}
	if len(updates) > 0 {
		if err := database.DB.Model(&models.ChatRoom{}).Where("id = ?", roomID).Updates(updates).Error; err != nil {
			log.Printf("[Room] settings update failed room=%d: %v", roomID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Alınmadı"})
			return
		}
	}
	var room models.ChatRoom
	if err := database.DB.First(&room, roomID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Otaq tapılmadı"})
		return
	}
	// Every member's open room screen switches the composer / secure canvas live.
	h.wsHub.SendToMultipleUsers(h.roomMemberIDs(roomID), "room_settings_updated", gin.H{
		"room_id":                roomID,
		"screenshot_disabled":    room.ScreenshotDisabled,
		"messaging_locked":       room.IsMessagingLocked(time.Now()),
		"messaging_locked_until": lockUntilIfActive(room),
	})
	c.JSON(http.StatusOK, gin.H{"data": h.roomDetailResponse(room, userID)})
}

// --- Per-user hide ---

// POST /api/v1/rooms/:room_id/hide — the room disappears for this user from
// discover, chats, the share sheet and pushes until unhidden.
func (h *RoomHandler) HideRoom(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	roomID := parseRoomID(c)
	var n int64
	database.DB.Model(&models.ChatRoom{}).Where("id = ?", roomID).Count(&n)
	if n == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Otaq tapılmadı", "code": "ROOM_NOT_FOUND"})
		return
	}
	if err := database.DB.Exec(`
		INSERT INTO room_hides (room_id, user_id, created_at, updated_at)
		VALUES (?, ?, NOW(), NOW())
		ON CONFLICT (room_id, user_id) DO NOTHING`, roomID, userID).Error; err != nil {
		log.Printf("[Room] hide failed room=%d user=%d: %v", roomID, userID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Alınmadı"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "hidden": true})
}

// POST /api/v1/rooms/:room_id/unhide
func (h *RoomHandler) UnhideRoom(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	roomID := parseRoomID(c)
	database.DB.Exec(`DELETE FROM room_hides WHERE room_id = ? AND user_id = ?`, roomID, userID)
	c.JSON(http.StatusOK, gin.H{"status": "ok", "hidden": false})
}

// GET /api/v1/rooms/hidden — the rooms this user hid (newest hide first), for
// the "Hidden rooms" screen where they can be brought back.
func (h *RoomHandler) GetHiddenRooms(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	var rooms []models.ChatRoom
	database.DB.
		Joins("JOIN room_hides rh ON rh.room_id = rooms.id AND rh.user_id = ?", userID).
		Where(roomOwnerNotBlockedSQL, userID, userID).
		Order("rh.created_at DESC").
		Limit(200).
		Find(&rooms)

	ids := make([]uint, 0, len(rooms))
	for _, r := range rooms {
		ids = append(ids, r.ID)
	}
	counts := roomMemberCounts(ids)
	roles := map[uint]string{}
	if len(ids) > 0 {
		var mems []models.RoomMember
		database.DB.Where("room_id IN ? AND user_id = ?", ids, userID).Find(&mems)
		for _, m := range mems {
			roles[m.RoomID] = m.Role
		}
	}
	out := make([]models.RoomResponse, 0, len(rooms))
	for _, r := range rooms {
		var rolePtr *string
		isMember := false
		if role, ok := roles[r.ID]; ok {
			rolePtr = strPtr(role)
			isMember = true
		}
		resp := h.toRoomResponse(r, rolePtr, isMember)
		resp.MemberCount = counts[r.ID]
		resp.IsHidden = true
		out = append(out, resp)
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// --- Share sheet ---

// GET /api/v1/rooms/share-targets — joined rooms the user can post into right
// now (not hidden, not frozen, owner not blocked, not write-blocked, not
// locked unless admin), ranked by how many messages the user wrote there in
// the last 14 days: a room the user is active in comes first, a room where
// only others talk sinks to the end.
func (h *RoomHandler) GetRoomShareTargets(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	if isGuest(userID) {
		c.JSON(http.StatusOK, gin.H{"rooms": []gin.H{}})
		return
	}
	since := time.Now().Add(-14 * 24 * time.Hour)
	var rows []struct {
		ID                   uint       `gorm:"column:id"`
		Name                 string     `gorm:"column:name"`
		Avatar               *string    `gorm:"column:avatar"`
		Role                 string     `gorm:"column:role"`
		MessagingLocked      bool       `gorm:"column:messaging_locked"`
		MessagingLockedUntil *time.Time `gorm:"column:messaging_locked_until"`
		MyRecent             int        `gorm:"column:my_recent"`
		MemberCount          int        `gorm:"column:member_count"`
	}
	database.DB.Raw(`
		SELECT r.id, r.name, r.avatar, rm.role, r.messaging_locked, r.messaging_locked_until,
		       (SELECT COUNT(*) FROM messages m
		         WHERE m.room_id = r.id AND m.sender_id = ? AND m.deleted_at IS NULL
		           AND m.created_at > ?) AS my_recent,
		       (SELECT COUNT(*) FROM room_members x WHERE x.room_id = r.id) AS member_count
		FROM room_members rm
		JOIN rooms r ON r.id = rm.room_id
		WHERE rm.user_id = ?
		  AND r.deleted_at IS NULL
		  AND r.is_frozen = false
		  AND NOT EXISTS (SELECT 1 FROM room_hides rh WHERE rh.room_id = r.id AND rh.user_id = rm.user_id)
		  AND NOT EXISTS (
		      SELECT 1 FROM user_blocks ub
		      WHERE (ub.blocker_id = rm.user_id AND ub.blocked_id = r.owner_id)
		         OR (ub.blocker_id = r.owner_id AND ub.blocked_id = rm.user_id)
		  )
		  AND (rm.role IN ('owner', 'admin') OR NOT EXISTS (
		      SELECT 1 FROM room_write_blocks wb WHERE wb.room_id = r.id AND wb.user_id = rm.user_id))
		ORDER BY my_recent DESC, r.last_activity_at DESC NULLS LAST
		LIMIT 50
	`, userID, since, userID).Scan(&rows)

	now := time.Now()
	out := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		isAdmin := r.Role == "owner" || r.Role == "admin"
		locked := r.MessagingLocked && (r.MessagingLockedUntil == nil || r.MessagingLockedUntil.After(now))
		if locked && !isAdmin {
			continue
		}
		out = append(out, gin.H{
			"id":                 r.ID,
			"name":               r.Name,
			"avatar":             r.Avatar,
			"my_role":            r.Role,
			"member_count":       r.MemberCount,
			"my_recent_messages": r.MyRecent,
		})
	}
	c.JSON(http.StatusOK, gin.H{"rooms": out})
}

// --- Search ---

// GET /api/v1/rooms/:room_id/messages/search?q=&limit=&before_ms=
// Members only (owner/admin included). Room texts are AES-encrypted, so rows
// are scanned newest-first in batches, decrypted and matched case-insensitively
// (DM SearchMessages approach). Media/voice JSON payloads carry no text and are
// skipped; senders blocked with the viewer are excluded. Continue with
// next_before_ms while has_more is true.
func (h *RoomHandler) SearchRoomMessages(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	roomID := parseRoomID(c)
	room, ok := loadVisibleRoom(c, roomID, userID)
	if !ok {
		return
	}
	if roomMembership(roomID, userID) == nil && room.OwnerID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "Yalnız üzvlər axtara bilər", "code": "ROOM_MEMBERS_ONLY"})
		return
	}
	q := strings.TrimSpace(c.Query("q"))
	if q == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "q tələb olunur"})
		return
	}
	qLower := strings.ToLower(q)
	limit := 25
	if v, ok := atoiPos(c.Query("limit")); ok && v > 0 && v <= 50 {
		limit = v
	}
	cursor := time.Now().Add(time.Hour)
	if ms, err := strconv.ParseInt(c.Query("before_ms"), 10, 64); err == nil && ms > 0 {
		cursor = time.UnixMilli(ms)
	}

	const batchSize = 200
	const scanCap = 2000
	type row struct {
		ID             string    `gorm:"column:id"`
		SenderID       uint      `gorm:"column:sender_id"`
		SenderName     string    `gorm:"column:sender_name"`
		SenderUsername string    `gorm:"column:sender_username"`
		SenderAvatar   *string   `gorm:"column:sender_avatar"`
		SenderVerified bool      `gorm:"column:sender_verified"`
		EncryptedText  string    `gorm:"column:encrypted_text"`
		CreatedAt      time.Time `gorm:"column:created_at"`
	}
	matches := make([]models.RoomMessageResponse, 0, limit)
	scanned := 0
	exhausted := false
	for scanned < scanCap && len(matches) < limit {
		var rows []row
		if err := database.DB.Raw(`
			SELECT m.id, m.sender_id, u.name AS sender_name, u.username AS sender_username,
			       p.profile_image AS sender_avatar, u.is_verified AS sender_verified,
			       m.encrypted_text, m.created_at
			FROM messages m
			JOIN users u ON u.id = m.sender_id
			LEFT JOIN profiles p ON p.user_id = m.sender_id
			WHERE m.room_id = ? AND m.deleted_at IS NULL AND m.created_at < ?
			  AND NOT EXISTS (
			      SELECT 1 FROM user_blocks ub
			      WHERE (ub.blocker_id = ? AND ub.blocked_id = m.sender_id)
			         OR (ub.blocker_id = m.sender_id AND ub.blocked_id = ?)
			  )
			ORDER BY m.created_at DESC
			LIMIT ?
		`, roomID, cursor, userID, userID, batchSize).Scan(&rows).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Mesajlar alınmadı"})
			return
		}
		if len(rows) == 0 {
			exhausted = true
			break
		}
		for _, r := range rows {
			cursor = r.CreatedAt
			scanned++
			text, err := h.encryptionService.DecryptMessage(r.EncryptedText)
			if err != nil {
				continue
			}
			trimmed := strings.TrimSpace(text)
			if strings.HasPrefix(trimmed, "{") {
				var payload map[string]interface{}
				if json.Unmarshal([]byte(trimmed), &payload) == nil {
					if _, typed := payload["type"]; typed {
						continue
					}
				}
			}
			if !strings.Contains(strings.ToLower(text), qLower) {
				continue
			}
			matches = append(matches, models.RoomMessageResponse{
				ID:             r.ID,
				RoomID:         roomID,
				SenderID:       r.SenderID,
				SenderName:     r.SenderName,
				SenderUsername: r.SenderUsername,
				SenderAvatar:   utils.PrependBaseURL(r.SenderAvatar),
				SenderVerified: r.SenderVerified,
				Text:           text,
				Reactions:      []models.RoomReaction{},
				CreatedAt:      r.CreatedAt,
			})
			if len(matches) >= limit {
				break
			}
		}
		if len(rows) < batchSize {
			exhausted = true
			break
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"data":           matches,
		"has_more":       !exhausted,
		"next_before_ms": cursor.UnixMilli(),
	})
}

// GET /api/v1/rooms/:room_id/messages/:message_id/reactions — who reacted with
// what (reaction details sheet). Members no longer see the member list, so the
// reactors' public profile bits come from here. Reactors blocked with the
// viewer (either way) are left out.
func (h *RoomHandler) GetRoomMessageReactions(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	roomID := parseRoomID(c)
	if _, ok := loadVisibleRoom(c, roomID, userID); !ok {
		return
	}
	messageID := c.Param("message_id")
	var rows []struct {
		UserID       uint    `gorm:"column:user_id" json:"user_id"`
		Emoji        string  `gorm:"column:emoji" json:"emoji"`
		Username     string  `gorm:"column:username" json:"username"`
		Name         string  `gorm:"column:name" json:"name"`
		IsVerified   bool    `gorm:"column:is_verified" json:"is_verified"`
		ProfileImage *string `gorm:"column:profile_image" json:"profile_image"`
	}
	database.DB.Raw(`
		SELECT r.user_id, r.emoji, u.username, u.name, u.is_verified, p.profile_image
		FROM room_message_reactions r
		JOIN users u ON u.id = r.user_id
		LEFT JOIN profiles p ON p.user_id = r.user_id
		WHERE r.room_id = ? AND r.message_id = ?
		  AND NOT EXISTS (
		      SELECT 1 FROM user_blocks ub
		      WHERE (ub.blocker_id = ? AND ub.blocked_id = r.user_id)
		         OR (ub.blocker_id = r.user_id AND ub.blocked_id = ?)
		  )
		ORDER BY r.updated_at DESC
	`, roomID, messageID, userID, userID).Scan(&rows)
	for i := range rows {
		rows[i].ProfileImage = utils.PrependBaseURL(rows[i].ProfileImage)
	}
	c.JSON(http.StatusOK, gin.H{"reactions": rows})
}
