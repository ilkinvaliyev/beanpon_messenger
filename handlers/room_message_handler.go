package handlers

import (
	"log"
	"net/http"
	"time"

	"beanpon_messenger/database"
	"beanpon_messenger/models"

	"github.com/gin-gonic/gin"
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

	var room models.ChatRoom
	if err := database.DB.First(&room, roomID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Otaq tapılmadı"})
		return
	}
	if room.IsFrozen {
		c.JSON(http.StatusForbidden, gin.H{"error": "Otaq dondurulub", "code": "ROOM_FROZEN"})
		return
	}

	var req struct {
		Text             string  `json:"text" binding:"required,min=1"`
		ReplyToMessageID *string `json:"reply_to_message_id"`
		ClientMessageID  *string `json:"client_message_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Mətn tələb olunur"})
		return
	}

	// Tək-klik join: yazan avtomatik üzv. BEST-EFFORT — mesaj göndərməni
	// bloklamır. Üzvlük yazısı alınmasa belə mesaj gedir (ensureMember özü
	// uğursuzluğu loglayır). Owner/mövcud üzv üçün onsuz da no-op-dur.
	_ = h.ensureMember(roomID, userID)

	encryptedText, err := h.encryptionService.EncryptMessage(req.Text)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Şifrələmə xətası"})
		return
	}

	messageID := req.ClientMessageID
	if messageID == nil || *messageID == "" {
		id := newRoomMessageID()
		messageID = &id
	}
	now := time.Now()

	message := models.Message{
		ID:               *messageID,
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
			roomID, userID, *messageID, createRes.Error)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Mesaj saxlanılmadı"})
		return
	}
	if createRes.RowsAffected == 0 {
		// RowsAffected==0: ya həqiqi təkrar (eyni id), ya da conflict target
		// uyğun gəlmədi. Təkrar olub-olmadığını DB-dən yoxla — əks halda mesaj
		// "göndərildi" görünür amma əslində YAZILMAYIB (istifadəçinin şikayəti).
		var exists int64
		database.DB.Model(&models.Message{}).Where("id = ?", *messageID).Count(&exists)
		if exists == 0 {
			log.Printf("[Room] message NOT PERSISTED (0 rows, not a dup) room=%d user=%d id=%s",
				roomID, userID, *messageID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Mesaj saxlanılmadı"})
			return
		}
		// Həqiqi təkrar göndərmə — sayğac/WS təkrarlanmır.
		c.JSON(http.StatusOK, gin.H{"message": "göndərildi", "duplicate": true, "data": gin.H{"id": *messageID}})
		return
	}

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
	payload := gin.H{
		"id":                  *messageID,
		"room_id":             roomID,
		"sender_id":           userID,
		"sender_name":         sender.Name,
		"sender_username":     sender.Username,
		"sender_avatar":       sender.ProfileImage,
		"sender_verified":     sender.IsVerified,
		"text":                req.Text,
		"reply_to_message_id": req.ReplyToMessageID,
		// Group chat paritesi — client null-ı pozuq sayır, boş massiv ver.
		"reactions":  []gin.H{},
		"created_at": now.UTC().Format(time.RFC3339),
	}
	for _, mid := range h.roomMemberIDsExcludingBlocked(roomID, userID) {
		if mid == userID {
			continue
		}
		h.wsHub.SendToUser(mid, "new_room_message", payload)
	}

	// Push bildirişi — JOIN olmuş, muted OLMAYAN, bloklu olmayan üzvlərə (group
	// chat paritesi). Qrupun push helper-i təkrar istifadə olunur. Gecikmə ilə
	// (10s) göndərilir ki, istifadəçi onlayn görübsə təkrar bildiriş olmasın.
	pushTargets := h.roomPushTargets(roomID, userID)
	if len(pushTargets) > 0 {
		h.wsHub.ScheduleGroupPushNotification(
			roomID, userID, room.Name, req.Text, *messageID,
			pushTargets, 10*time.Second,
		)
	}

	c.JSON(http.StatusOK, gin.H{"message": "göndərildi", "data": payload})
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

	var room models.ChatRoom
	if err := database.DB.First(&room, roomID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Otaq tapılmadı"})
		return
	}
	if room.IsFrozen {
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
	// Join yalnız mesaj yazmaq üçün lazımdır (SendRoomMessage). Ona görə tarixçə
	// filtri yoxdur — həmişə 1970-dən bəri (yəni bütün mesajlar).
	var joinedArg interface{} = time.Unix(0, 0)

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
			u.profile_image AS sender_avatar,
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
		LEFT JOIN messages reply ON reply.id = m.reply_to_message_id
		LEFT JOIN users reply_u ON reply_u.id = reply.sender_id
		WHERE m.room_id = ?
		  AND m.deleted_at IS NULL
		  AND m.created_at >= ?
		  AND NOT EXISTS (
		      SELECT 1 FROM user_blocks ub
		      WHERE (ub.blocker_id = ? AND ub.blocked_id = m.sender_id)
		         OR (ub.blocker_id = m.sender_id AND ub.blocked_id = ?)
		  )
		ORDER BY m.created_at DESC, m.id DESC
		LIMIT ?
	`, userID, userID, roomID, joinedArg, userID, userID, limit).Scan(&rows)

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
			SenderAvatar:   r.SenderAvatar,
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

	c.JSON(http.StatusOK, gin.H{"data": out})
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
	messageID := c.Param("message_id")

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
	messageID := c.Param("message_id")

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
	database.DB.Delete(&models.Message{}, "id = ?", messageID)
	// Silinən mesajın reaksiyalarını da təmizlə (sahibsiz qalmasın).
	database.DB.Exec(`DELETE FROM room_message_reactions WHERE message_id = ?`, messageID)
	h.wsHub.SendToMultipleUsers(h.roomMemberIDs(roomID), "room_message_deleted", gin.H{
		"room_id": roomID, "message_id": messageID,
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
	`, roomID, senderID, senderID).Scan(&ids)
	return ids
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
