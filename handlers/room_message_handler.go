package handlers

import (
	"log"
	"net/http"
	"strings"
	"time"

	"beanpon_messenger/database"
	"beanpon_messenger/models"
	"beanpon_messenger/services"
	"beanpon_messenger/utils"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SendRoomMessage — POST /api/v1/rooms/:room_id/messages
// Tək-klik join: yazmaq istəyən avtomatik üzv olur. Guest yaza bilməz, donmuş
// otağa yazılmaz. Reply + two-way block dəstəklənir.
func (h *RoomHandler) SendRoomMessage(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	if isGuest(userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Qonaq hesab yaza bilməz", "code": "GUEST_FORBIDDEN"})
		return
	}
	roomID := parseRoomID(c)

	roomPtr, ok := loadVisibleRoom(c, roomID, userID)
	if !ok {
		return
	}
	room := *roomPtr

	var req struct {
		Text             string  `json:"text" binding:"required,min=1"`
		ReplyToMessageID *string `json:"reply_to_message_id"`
		ClientMessageID  *string `json:"client_message_id"`
		// New clients (durable send queue + room composer): refuse instead of
		// auto-joining when the sender is not a member — a retry queued before
		// the user left / was removed must not silently re-join them.
		RequireMember bool `json:"require_member"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Mətn tələb olunur"})
		return
	}
	// Canonical (lowercase) UUID: the database returns ids lowercase, so the
	// reply / WS echo must use the same spelling or clients see two messages.
	messageID, clientGiven, err := resolveMessageID(req.ClientMessageID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": "INVALID_CLIENT_MESSAGE_ID"})
		return
	}
	// A retry of a message that is already stored (its first reply was lost)
	// is answered BEFORE the write gates: a freeze, lock, write block or
	// removal that came after the original send must not turn a delivered
	// message into a failure on the sender's screen — nor re-join them.
	if clientGiven {
		var existing models.Message
		if err := database.DB.Unscoped().Where("id = ?", messageID).Limit(1).Find(&existing).Error; err == nil && existing.ID != "" {
			h.replyRoomDuplicate(c, existing, userID, roomID)
			return
		}
	}

	if room.IsFrozen {
		c.JSON(http.StatusForbidden, gin.H{"error": "Otaq dondurulub", "code": "ROOM_FROZEN"})
		return
	}
	// Admin locks: messaging closed for everyone but admins, or this user is
	// blocked from writing. Enforced here so every path (composer, post share,
	// retries, old clients) is covered.
	mem := roomMembership(roomID, userID)
	if req.RequireMember && mem == nil && room.OwnerID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "Otağın üzvü deyilsiniz", "code": "ROOM_NOT_MEMBER"})
		return
	}
	senderIsAdmin := (mem != nil && mem.HasAdminAccess()) || room.OwnerID == userID
	if !senderIsAdmin && room.IsMessagingLocked(time.Now()) {
		c.JSON(http.StatusForbidden, gin.H{
			"error":                  "Otaqda yazışma bağlanıb",
			"code":                   "ROOM_LOCKED",
			"messaging_locked_until": lockUntilIfActive(room),
		})
		return
	}
	if !senderIsAdmin && isWriteBlocked(roomID, userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Bu otaqda yaza bilməzsiniz", "code": "ROOM_WRITE_BLOCKED"})
		return
	}

	// Tək-klik join: yazan avtomatik üzv. BEST-EFFORT — mesaj göndərməni
	// bloklamır. Üzvlük yazısı alınmasa belə mesaj gedir (ensureMember özü
	// uğursuzluğu loglayır). Owner/mövcud üzv üçün onsuz da no-op-dur.
	// Exception: a ban that landed while this request was in flight.
	if err := h.ensureMember(roomID, userID); err == errRoomBanned {
		c.JSON(http.StatusNotFound, gin.H{"error": "Otaq tapılmadı", "code": "ROOM_NOT_FOUND"})
		return
	}

	encryptedText, err := h.encryptionService.EncryptMessage(req.Text)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Şifrələmə xətası"})
		return
	}

	now := time.Now()

	message := models.Message{
		ID:               messageID,
		SenderID:         userID,
		RoomID:           &roomID,
		ReplyToMessageID: req.ReplyToMessageID,
		EncryptedText:    encryptedText,
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	createRes := database.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoNothing: true,
	}).Create(&message)
	if createRes.Error != nil {
		// GERÇEK DB hatasını logla (room_id kolonu yoxdursa / NOT NULL pozuntusu
		// burada görünəcək — "mesaj gedir amma qalmır" probleminin kökü).
		log.Printf("[Room] message INSERT FAILED room=%d user=%d id=%s: %v",
			roomID, userID, messageID, createRes.Error)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Mesaj saxlanılmadı"})
		return
	}
	if createRes.RowsAffected == 0 {
		// RowsAffected==0: ya həqiqi təkrar (eyni id), ya da conflict target
		// uyğun gəlmədi. Təkrar olub-olmadığını DB-dən yoxla — əks halda mesaj
		// "göndərildi" görünür amma əslində YAZILMAYIB (istifadəçinin şikayəti).
		// (Two retries racing past the early lookup above end up here.)
		var existing models.Message
		if err := database.DB.Unscoped().Where("id = ?", messageID).First(&existing).Error; err != nil {
			log.Printf("[Room] message NOT PERSISTED (0 rows, not a dup) room=%d user=%d id=%s",
				roomID, userID, messageID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Mesaj saxlanılmadı"})
			return
		}
		h.replyRoomDuplicate(c, existing, userID, roomID)
		return
	}

	// Media in the text (images/videos/voices) is now referenced — otherwise the
	// 24 h orphan-media sweep deletes it from S3 (rooms used to skip this).
	services.MarkMediaReferenced(database.DB, req.Text)

	// Sayğac + son aktivlik + skor.
	database.DB.Model(&models.ChatRoom{}).Where("id = ?", roomID).Updates(map[string]interface{}{
		"message_count":    gorm.Expr("message_count + 1"),
		"last_activity_at": now,
	})
	h.bumpScore(roomID)

	// Göndərənin məlumatı (WS yük üçün).
	var sender models.User
	database.DB.First(&sender, userID)

	// WS fan-out — otaq üzvlərinə. Block filtri alıcı tərəfdə (client) deyil,
	// burada tətbiq olunur: göndərənlə block əlaqəsi olan üzvə göndərmə.
	payload := roomMessagePayload(message, roomID, sender, req.Text)
	for _, mid := range h.roomMemberIDsExcludingBlocked(roomID, userID) {
		if mid == userID {
			continue
		}
		h.wsHub.SendToUser(mid, "new_room_message", payload)
	}

	// Push bildirişi — JOIN olmuş, muted OLMAYAN, bloklu olmayan üzvlərə. Room
	// push (route room_chat_page): delayed 10 s and skipped for members who read
	// the room past this message in the meantime (they saw it live).
	pushTargets := h.roomPushTargets(roomID, userID)
	if len(pushTargets) > 0 {
		avatar := ""
		if room.Avatar != nil {
			avatar = *room.Avatar
		}
		h.wsHub.ScheduleRoomPushNotification(
			roomID, userID, room.Name, avatar, req.Text, messageID, now,
			pushTargets, 10*time.Second,
		)
	}

	c.JSON(http.StatusOK, gin.H{"message": "göndərildi", "data": payload})
}

// replyRoomDuplicate answers a send whose id is already stored. Həqiqi təkrar
// göndərmə — sayğac/WS/push təkrarlanmır. The stored message comes back whole
// (server time + sender), so a retry whose first reply was lost confirms
// exactly like the original send.
func (h *RoomHandler) replyRoomDuplicate(c *gin.Context, existing models.Message, userID, roomID uint) {
	// The id belongs to someone else's message / another chat: never confirm
	// a send with a message the sender does not own.
	if existing.SenderID != userID || existing.RoomID == nil || *existing.RoomID != roomID {
		c.JSON(http.StatusConflict, gin.H{"error": "client_message_id başqa mesaja aiddir", "code": "CLIENT_MESSAGE_ID_CONFLICT"})
		return
	}
	if existing.DeletedAt.Valid {
		// Already deleted (moderation / delete for everyone) — only the id: the
		// client keeps its own copy until the delete reaches it.
		c.JSON(http.StatusOK, gin.H{"message": "göndərildi", "duplicate": true, "deleted": true,
			"data": gin.H{"id": existing.ID}})
		return
	}
	var dupSender models.User
	database.DB.First(&dupSender, userID)
	storedText, _ := h.encryptionService.DecryptMessage(existing.EncryptedText)
	c.JSON(http.StatusOK, gin.H{"message": "göndərildi", "duplicate": true,
		"data": roomMessagePayload(existing, roomID, dupSender, storedText)})
}

// roomMessagePayload — a just-sent room message as clients receive it (WS
// `new_room_message` + the send reply, first send and duplicate retry alike).
func roomMessagePayload(msg models.Message, roomID uint, sender models.User, text string) gin.H {
	return gin.H{
		"id":                  msg.ID,
		"room_id":             roomID,
		"sender_id":           msg.SenderID,
		"sender_name":         sender.Name,
		"sender_username":     sender.Username,
		"sender_avatar":       sender.ProfileImage,
		"sender_verified":     sender.IsVerified,
		"text":                text,
		"reply_to_message_id": msg.ReplyToMessageID,
		// Group chat paritesi — client null-ı pozuq sayır, boş massiv ver.
		"reactions": []gin.H{},
		// Same value and precision as GET /messages (the column keeps
		// microseconds): a whole-second time would put a live message inside
		// a page's time span, where clients treat missing rows as deleted.
		"created_at": msg.CreatedAt.UTC().Truncate(time.Microsecond),
	}
}

// GetRoomMessages — GET /api/v1/rooms/:room_id/messages
// Two-way block filtri + reply-erişim kartı + history_visible (yeni üzv köhnə
// mesajları görməsin seçimi). Guest oxuya bilməz.
func (h *RoomHandler) GetRoomMessages(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	if isGuest(userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Qonaq hesab", "code": "GUEST_FORBIDDEN"})
		return
	}
	roomID := parseRoomID(c)

	roomPtr, ok := loadVisibleRoom(c, roomID, userID)
	if !ok {
		return
	}
	if roomPtr.IsFrozen {
		c.JSON(http.StatusForbidden, gin.H{"error": "Otaq dondurulub", "code": "ROOM_FROZEN"})
		return
	}

	limit := 40
	if v := c.Query("limit"); v != "" {
		if n, e := atoiPos(v); e && n > 0 && n <= 100 {
			limit = n
		}
	}
	// Açıq otaq qaydası: HƏR KƏS (join olmayan belə) BÜTÜN mesajları görür.
	// Join yalnız mesaj yazmaq üçün lazımdır (SendRoomMessage).
	//
	// Paging (new clients): before_id = the oldest message the client has; the
	// page continues strictly older than it. Without it → the newest page.
	cursorSQL := ""
	var cursorArgs []interface{}
	var upperBound *time.Time
	if beforeID := c.Query("before_id"); beforeID != "" {
		var anchor struct {
			CreatedAt time.Time `gorm:"column:created_at"`
		}
		database.DB.Raw(`SELECT created_at FROM messages WHERE id = ? AND room_id = ?`, beforeID, roomID).Scan(&anchor)
		if !anchor.CreatedAt.IsZero() {
			cursorSQL = ` AND (m.created_at < ? OR (m.created_at = ? AND m.id < ?))`
			cursorArgs = []interface{}{anchor.CreatedAt, anchor.CreatedAt, beforeID}
			upperBound = &anchor.CreatedAt
		}
	}

	type row struct {
		ID             string
		SenderID       uint
		SenderName     string
		SenderUsername string
		SenderAvatar   *string
		SenderVerified bool
		EncryptedText  string
		ReplyToID      *string
		ReplyEnc       *string
		ReplySender    *string
		ReplyDeleted   *bool
		ReplyBlocked   *bool
		CreatedAt      time.Time
	}
	var rows []row

	// İki yönlü block: block olan göndərənin mesajları gizlənir.
	// Reply: reply edilən mesaj block/silinmişsə ReplyBlocked=true.
	database.DB.Raw(`
		SELECT
			m.id,
			m.sender_id,
			u.name AS sender_name,
			u.username AS sender_username,
			p.profile_image AS sender_avatar,
			u.is_verified AS sender_verified,
			m.encrypted_text,
			m.reply_to_message_id AS reply_to_id,
			reply.encrypted_text AS reply_enc,
			reply_u.username AS reply_sender,
			(reply.id IS NOT NULL AND reply.deleted_at IS NOT NULL) AS reply_deleted,
			(reply.id IS NOT NULL AND EXISTS (
				SELECT 1 FROM user_blocks ub
				WHERE (ub.blocker_id = ? AND ub.blocked_id = reply.sender_id)
				   OR (ub.blocker_id = reply.sender_id AND ub.blocked_id = ?)
			)) AS reply_blocked,
			m.created_at
		FROM messages m
		JOIN users u ON u.id = m.sender_id
		LEFT JOIN profiles p ON p.user_id = m.sender_id
		LEFT JOIN messages reply ON reply.id = m.reply_to_message_id
		LEFT JOIN users reply_u ON reply_u.id = reply.sender_id
		WHERE m.room_id = ?
		  AND m.deleted_at IS NULL
		  AND NOT EXISTS (
		      SELECT 1 FROM user_blocks ub
		      WHERE (ub.blocker_id = ? AND ub.blocked_id = m.sender_id)
		         OR (ub.blocker_id = m.sender_id AND ub.blocked_id = ?)
		  )`+cursorSQL+`
		ORDER BY m.created_at DESC, m.id DESC
		LIMIT ?
	`, append(append([]interface{}{userID, userID, roomID, userID, userID}, cursorArgs...), limit)...).Scan(&rows)
	log.Printf("[Room] GetRoomMessages room=%d userID=%d → rows=%d", roomID, userID, len(rows))

	// Reaksiyalar — N+1 yox: səhifədəki bütün mesaj id-ləri üçün BİR sorğu,
	// sonra Go-da map ilə mesajlara paylanır (group GetGroupMessages ikizi).
	reactionsByMessage := map[string][]models.RoomReaction{}
	if len(rows) > 0 {
		msgIDs := make([]string, 0, len(rows))
		for _, r := range rows {
			msgIDs = append(msgIDs, r.ID)
		}
		var reactionRows []struct {
			MessageID string `gorm:"column:message_id"`
			UserID    uint   `gorm:"column:user_id"`
			Emoji     string `gorm:"column:emoji"`
		}
		database.DB.Raw(`
			SELECT message_id, user_id, emoji
			FROM room_message_reactions
			WHERE message_id IN (?)
		`, msgIDs).Scan(&reactionRows)
		for _, rr := range reactionRows {
			reactionsByMessage[rr.MessageID] = append(reactionsByMessage[rr.MessageID],
				models.RoomReaction{UserID: rr.UserID, Emoji: rr.Emoji})
		}
	}

	out := make([]models.RoomMessageResponse, 0, len(rows))
	for _, r := range rows {
		text, _ := h.encryptionService.DecryptMessage(r.EncryptedText)
		reactions := reactionsByMessage[r.ID]
		if reactions == nil {
			reactions = []models.RoomReaction{}
		}
		item := models.RoomMessageResponse{
			ID:             r.ID,
			RoomID:         roomID,
			SenderID:       r.SenderID,
			SenderName:     r.SenderName,
			SenderUsername: r.SenderUsername,
			SenderAvatar:   utils.PrependBaseURL(r.SenderAvatar),
			SenderVerified: r.SenderVerified,
			Text:           text,
			ReplyToID:      r.ReplyToID,
			Reactions:      reactions,
			CreatedAt:      r.CreatedAt,
		}
		// Reply kartı: block/silinmiş → "erişiminiz yoxdur" (mətn boş, blocked=true).
		if r.ReplyToID != nil {
			blocked := (r.ReplyBlocked != nil && *r.ReplyBlocked) || (r.ReplyDeleted != nil && *r.ReplyDeleted)
			if blocked || r.ReplyEnc == nil {
				item.ReplyBlocked = true
			} else {
				rt, _ := h.encryptionService.DecryptMessage(*r.ReplyEnc)
				item.ReplyText = &rt
				item.ReplySender = r.ReplySender
			}
		}
		out = append(out, item)
	}

	resp := gin.H{"data": out, "has_more": len(rows) == limit}
	// include_events=1 (new clients): the join/leave lines of this page's time
	// span, only those the viewer may see (members since before the event).
	if c.Query("include_events") == "1" {
		events := []models.RoomEventResponse{}
		if mem := roomMembership(roomID, userID); mem != nil && mem.JoinedAt != nil {
			to := time.Now().Add(time.Minute)
			if upperBound != nil {
				to = *upperBound
			}
			from := *mem.JoinedAt
			if len(rows) == limit {
				// More history exists: this page starts at its oldest message.
				from = rows[len(rows)-1].CreatedAt
			}
			events = roomEventsForViewer(roomID, userID, *mem.JoinedAt, from, to)
		}
		resp["events"] = events
	}
	c.JSON(http.StatusOK, resp)
}

// SetRoomReaction — POST /api/v1/rooms/:room_id/messages/:message_id/reaction
// Per-user emoji reaksiya toggle (group SetGroupReaction ikizi). Eyni emoji →
// sil; fərqli → əvəzlə; yoxdursa → əlavə et. Bütün otaq üzvlərinə
// room_reaction_updated WS event-i (block filtrli — bloklu göndərənə getmir).
func (h *RoomHandler) SetRoomReaction(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	if isGuest(userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Qonaq hesab", "code": "GUEST_FORBIDDEN"})
		return
	}
	roomID := parseRoomID(c)
	messageID := canonicalMessageID(c.Param("message_id"))
	if _, ok := loadVisibleRoom(c, roomID, userID); !ok {
		return
	}

	var body struct {
		Emoji string `json:"emoji" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "emoji tələb olunur"})
		return
	}

	// Mesaj bu otağa aiddirmi?
	var msg models.Message
	if err := database.DB.Where("id = ? AND room_id = ?", messageID, roomID).First(&msg).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Mesaj tapılmadı"})
		return
	}

	now := time.Now()
	// Mövcud reaksiyaya bax — eyni emoji isə toggle (sil).
	var existing struct {
		Emoji *string `gorm:"column:emoji"`
	}
	database.DB.Raw(`
		SELECT emoji FROM room_message_reactions
		WHERE message_id = ? AND user_id = ?
	`, messageID, userID).Scan(&existing)

	// WS fan-out üçün üzvlər (block filtrli, group chat paritesi).
	targets := h.roomMemberIDsExcludingBlocked(roomID, userID)

	if existing.Emoji != nil && *existing.Emoji == body.Emoji {
		if err := database.DB.Exec(`
			DELETE FROM room_message_reactions
			WHERE message_id = ? AND user_id = ?
		`, messageID, userID).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Reaksiya silinmədi"})
			return
		}
		removedPayload := gin.H{
			"room_id":    roomID,
			"message_id": messageID,
			"user_id":    userID,
			"emoji":      nil,
			"action":     "removed",
		}
		h.wsHub.SendToMultipleUsers(targets, "room_reaction_updated", removedPayload)
		c.JSON(http.StatusOK, gin.H{"message": "Reaksiya silindi", "data": removedPayload})
		return
	}

	// Yoxdursa insert, fərqlidirsə yenisi ilə əvəzlə (UPSERT).
	if err := database.DB.Exec(`
		INSERT INTO room_message_reactions (message_id, user_id, room_id, emoji, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (message_id, user_id)
		DO UPDATE SET emoji = EXCLUDED.emoji, updated_at = EXCLUDED.updated_at
	`, messageID, userID, roomID, body.Emoji, now, now).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Reaksiya saxlanılmadı"})
		return
	}
	addedPayload := gin.H{
		"room_id":    roomID,
		"message_id": messageID,
		"user_id":    userID,
		"emoji":      body.Emoji,
		"action":     "added",
	}
	h.wsHub.SendToMultipleUsers(targets, "room_reaction_updated", addedPayload)
	c.JSON(http.StatusOK, gin.H{"message": "Reaksiya əlavə olundu", "data": addedPayload})
}

// DELETE /api/v1/rooms/:room_id/messages/:message_id — mesaj sil
// (owner|admin hər kəsinkini silə bilər; göndərən özününkünü silə bilər).
func (h *RoomHandler) DeleteRoomMessage(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	roomID := parseRoomID(c)
	messageID := canonicalMessageID(c.Param("message_id"))

	var msg models.Message
	if err := database.DB.Where("id = ? AND room_id = ?", messageID, roomID).First(&msg).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Mesaj tapılmadı"})
		return
	}
	mem := roomMembership(roomID, userID)
	canDelete := msg.SenderID == userID || (mem != nil && mem.HasAdminAccess())
	if !canDelete {
		c.JSON(http.StatusForbidden, gin.H{"error": "İcazə yoxdur"})
		return
	}
	// Soft delete: gone for everyone instantly, the row (and its encrypted text)
	// stays for moderation; room_message_deletions records who removed it.
	database.DB.Delete(&models.Message{}, "id = ?", messageID)
	database.DB.Exec(`
		INSERT INTO room_message_deletions (room_id, message_id, sender_id, deleted_by, by_admin, created_at)
		VALUES (?, ?, ?, ?, ?, NOW())`, roomID, messageID, msg.SenderID, userID, msg.SenderID != userID)
	// Silinən mesajın reaksiyalarını da təmizlə (sahibsiz qalmasın).
	database.DB.Exec(`DELETE FROM room_message_reactions WHERE message_id = ?`, messageID)
	h.wsHub.SendToMultipleUsers(h.roomMemberIDs(roomID), "room_message_deleted", gin.H{
		"room_id": roomID, "message_id": messageID, "deleted_by": userID,
	})
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// roomMemberIDsExcludingBlocked — WS fan-out üçün üzvlər; göndərənlə iki yönlü
// block əlaqəsi olanlar çıxarılır (mesaj onlara çatmasın).
func (h *RoomHandler) roomMemberIDsExcludingBlocked(roomID, senderID uint) []uint {
	var ids []uint
	database.DB.Raw(`
		SELECT rm.user_id FROM room_members rm
		WHERE rm.room_id = ?
		  AND NOT EXISTS (
		      SELECT 1 FROM user_blocks ub
		      WHERE (ub.blocker_id = rm.user_id AND ub.blocked_id = ?)
		         OR (ub.blocker_id = ? AND ub.blocked_id = rm.user_id)
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM room_hides rh WHERE rh.room_id = rm.room_id AND rh.user_id = rm.user_id
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM room_bans rb WHERE rb.room_id = rm.room_id AND rb.user_id = rm.user_id
		  )
	`, roomID, senderID, senderID).Scan(&ids)
	return ids
}

// canonicalMessageID — the lowercase UUID spelling the database returns, so a
// WS broadcast (reaction / delete) matches the id clients got from history,
// whatever casing the request used. Non-UUID input is returned unchanged
// (the lookup then simply finds nothing, as before).
func canonicalMessageID(raw string) string {
	if parsed, err := uuid.Parse(strings.TrimSpace(raw)); err == nil {
		return parsed.String()
	}
	return raw
}

// atoiPos — kiçik köməkçi (strconv importunu bu faylda saxlamamaq üçün).
func atoiPos(s string) (int, bool) {
	n := 0
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 0, false
		}
		n = n*10 + int(ch-'0')
	}
	return n, true
}
