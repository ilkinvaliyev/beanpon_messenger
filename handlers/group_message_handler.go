package handlers

import (
	"beanpon_messenger/database"
	"beanpon_messenger/models"
	"beanpon_messenger/utils"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type GroupMessageHandler struct {
	encryptionService interface {
		EncryptMessage(plainText string) (string, error)
		DecryptMessage(encryptedText string) (string, error)
	}
	wsHub interface {
		IsUserOnline(userID uint) bool
		SendToUser(userID uint, messageType string, data interface{})
		SendToMultipleUsers(userIDs []uint, messageType string, data interface{})
		SendGroupPushNotification(conversationID, senderID uint, groupName, message string, memberIDs []uint)
	}
}

func NewGroupMessageHandler(
	encryptionService interface {
		EncryptMessage(plainText string) (string, error)
		DecryptMessage(encryptedText string) (string, error)
	},
	wsHub interface {
		IsUserOnline(userID uint) bool
		SendToUser(userID uint, messageType string, data interface{})
		SendToMultipleUsers(userIDs []uint, messageType string, data interface{})
		SendGroupPushNotification(conversationID, senderID uint, groupName, message string, memberIDs []uint)
	},
) *GroupMessageHandler {
	return &GroupMessageHandler{
		encryptionService: encryptionService,
		wsHub:             wsHub,
	}
}

// POST /api/v1/groups/:conversation_id/messages
func (h *GroupMessageHandler) SendGroupMessage(c *gin.Context) {
	senderID := c.MustGet("user_id").(uint)
	convID, err := strconv.ParseUint(c.Param("conversation_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz conversation_id"})
		return
	}
	conversationID := uint(convID)

	var req struct {
		Text             string  `json:"text" binding:"required"`
		ReplyToMessageID *string `json:"reply_to_message_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Grup üyesi ve kısıtlı değil mi?
	var participant models.ConversationParticipant
	err = database.DB.Where(
		"conversation_id = ? AND user_id = ? AND left_at IS NULL AND deleted_at IS NULL",
		conversationID, senderID,
	).First(&participant).Error
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "Bu grubun üyesi değilsiniz"})
		return
	}
	if participant.IsRestricted {
		c.JSON(http.StatusForbidden, gin.H{"error": "Mesaj gönderme yetkiniz kısıtlandı"})
		return
	}

	encryptedText, err := h.encryptionService.EncryptMessage(req.Text)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Şifreleme hatası"})
		return
	}

	messageID := uuid.New().String()
	now := time.Now()

	message := models.Message{
		ID:               messageID,
		SenderID:         senderID,
		ConversationID:   &conversationID,
		ReplyToMessageID: req.ReplyToMessageID,
		EncryptedText:    encryptedText,
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	if err := database.DB.Create(&message).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Mesaj kaydedilemedi"})
		return
	}

	// Gönderen otomatik read
	messageRead := models.MessageRead{
		MessageID:      messageID,
		UserID:         senderID,
		ConversationID: conversationID,
		ReadAt:         now,
		CreatedAt:      now,
	}
	database.DB.Create(&messageRead)

	// Participant mesaj sayacını artır
	database.DB.Model(&participant).
		Update("message_count", gorm.Expr("message_count + 1"))

	// Conversation last_message_at güncelle
	database.DB.Table("conversations").
		Where("id = ?", conversationID).
		Update("last_message_at", now)

	// Sender bilgisi al
	var senderInfo struct {
		Name         string  `json:"name"`
		Username     string  `json:"username"`
		IsVerified   bool    `json:"is_verified"`
		ProfileImage *string `json:"profile_image"`
	}
	database.DB.Raw(`
		SELECT u.name, u.username, u.is_verified, p.profile_image
		FROM users u
		LEFT JOIN profiles p ON p.user_id = u.id
		WHERE u.id = ?
	`, senderID).Scan(&senderInfo)

	// Reply bilgisi — göndərənin username/avatar/verified ilə birlikdə
	// (Flutter reply preview-da şəkil + ad + badge göstərir).
	var replyData map[string]interface{}
	if req.ReplyToMessageID != nil {
		var replyMsg models.Message
		if err := database.DB.Where("id = ?", *req.ReplyToMessageID).First(&replyMsg).Error; err == nil {
			replyDecrypted, _ := h.encryptionService.DecryptMessage(replyMsg.EncryptedText)

			var replySender struct {
				Username     string  `gorm:"column:username"`
				IsVerified   bool    `gorm:"column:is_verified"`
				ProfileImage *string `gorm:"column:profile_image"`
			}
			database.DB.Raw(`
				SELECT u.username, u.is_verified, p.profile_image
				FROM users u
				LEFT JOIN profiles p ON p.user_id = u.id
				WHERE u.id = ?
			`, replyMsg.SenderID).Scan(&replySender)

			replyData = map[string]interface{}{
				"id":                 replyMsg.ID,
				"sender_id":          replyMsg.SenderID,
				"sender_username":    replySender.Username,
				"sender_is_verified": replySender.IsVerified,
				"sender_avatar":      utils.PrependBaseURL(replySender.ProfileImage),
				"text":               replyDecrypted,
				"created_at":         replyMsg.CreatedAt,
			}
		}
	}

	// Tüm üyelere WebSocket ile gönder
	memberIDs := getGroupParticipantIDs(conversationID)
	wsPayload := map[string]interface{}{
		"id":                  messageID,
		"conversation_id":     conversationID,
		"chat_type":           "group",
		"sender_id":           senderID,
		"sender_name":         senderInfo.Name,
		"sender_username":     senderInfo.Username,
		"sender_is_verified":  senderInfo.IsVerified,
		"sender_avatar":       utils.PrependBaseURL(senderInfo.ProfileImage),
		"text":                req.Text,
		"reply_to_message_id": req.ReplyToMessageID,
		"reply_to_message":    replyData,
		// Flutter parse tutarlılığı — GetGroupMessages item formatı ilə eyni.
		"is_edited":        false,
		"is_starred_by_me": false,
		"reactions":        []gin.H{},
		"created_at":       now.UTC().Format(time.RFC3339),
	}
	h.wsHub.SendToMultipleUsers(memberIDs, "new_group_message", wsPayload)

	// Push notification: mute olmayan üzvlərə (göndərən xaric). Mute yoxlaması
	// burada (Go) edilir; Laravel sadəcə verilən siyahıya FCM göndərir.
	var pushTargets []uint
	database.DB.Model(&models.ConversationParticipant{}).
		Where("conversation_id = ? AND user_id != ? AND left_at IS NULL AND deleted_at IS NULL", conversationID, senderID).
		Where("is_muted = false OR (muted_until IS NOT NULL AND muted_until < ?)", now).
		Pluck("user_id", &pushTargets)

	if len(pushTargets) > 0 {
		// Qrup adı (bildiriş başlığı üçün).
		var groupName string
		database.DB.Table("conversations").
			Where("id = ?", conversationID).
			Select("COALESCE(group_name, '')").
			Scan(&groupName)
		h.wsHub.SendGroupPushNotification(conversationID, senderID, groupName, req.Text, pushTargets)
	}

	c.JSON(http.StatusCreated, gin.H{
		"message": "Mesaj gönderildi",
		"data":    wsPayload,
	})
}

// GET /api/v1/groups/:conversation_id/messages
func (h *GroupMessageHandler) GetGroupMessages(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	convID, err := strconv.ParseUint(c.Param("conversation_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz conversation_id"})
		return
	}
	conversationID := uint(convID)

	var me models.ConversationParticipant
	err = database.DB.Where(
		"conversation_id = ? AND user_id = ? AND deleted_at IS NULL",
		conversationID, userID,
	).First(&me).Error
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "Bu grubun üyesi değilsiniz"})
		return
	}

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	offset := (page - 1) * limit

	// Yeni qoşulan üzv KÖHNƏ mesajları görmür: yalnız joined_at-dan SONRAKI
	// mesajlar. JoinedAt null-dursa (köhnə qeydlər) participant created_at
	// fallback. cleared_at varsa ("söhbəti təmizlə — özüm üçün") ondan sonrakı.
	joinedAtFilter := me.CreatedAt
	if me.JoinedAt != nil {
		joinedAtFilter = *me.JoinedAt
	}

	var messages []struct {
		ID               string    `gorm:"column:id"`
		SenderID         uint      `gorm:"column:sender_id"`
		SenderName       string    `gorm:"column:sender_name"`
		SenderUsername   string    `gorm:"column:sender_username"`
		SenderIsVerified bool      `gorm:"column:sender_is_verified"`
		SenderAvatar     *string   `gorm:"column:sender_avatar"`
		EncryptedText    string    `gorm:"column:encrypted_text"`
		ReplyToMessageID *string   `gorm:"column:reply_to_message_id"`
		ReplyText        *string   `gorm:"column:reply_text"`
		ReplyToSenderID  *uint     `gorm:"column:reply_to_sender_id"`
		ReplyUsername    *string   `gorm:"column:reply_username"`
		ReplyIsVerified  *bool     `gorm:"column:reply_is_verified"`
		ReplyAvatar      *string   `gorm:"column:reply_avatar"`
		ReadCount        int       `gorm:"column:read_count"`
		IsEdited         bool      `gorm:"column:is_edited"`
		IsStarredByMe    bool      `gorm:"column:is_starred_by_me"`
		CreatedAt        time.Time `gorm:"column:created_at"`
	}

	database.DB.Raw(`
		SELECT
			m.id,
			m.sender_id,
			u.name as sender_name,
			u.username as sender_username,
			u.is_verified as sender_is_verified,
			p.profile_image as sender_avatar,
			m.encrypted_text,
			m.reply_to_message_id,
			reply.encrypted_text as reply_text,
			reply.sender_id as reply_to_sender_id,
			reply_u.username as reply_username,
			reply_u.is_verified as reply_is_verified,
			reply_p.profile_image as reply_avatar,
			(SELECT COUNT(*) FROM message_reads mr WHERE mr.message_id = m.id AND mr.user_id != m.sender_id) as read_count,
			m.is_edited,
			EXISTS (
				SELECT 1 FROM group_message_stars gms
				WHERE gms.message_id = m.id AND gms.user_id = ?
			) as is_starred_by_me,
			m.created_at
		FROM messages m
		JOIN users u ON u.id = m.sender_id
		LEFT JOIN profiles p ON p.user_id = m.sender_id
		LEFT JOIN messages reply ON reply.id = m.reply_to_message_id
		LEFT JOIN users reply_u ON reply_u.id = reply.sender_id
		LEFT JOIN profiles reply_p ON reply_p.user_id = reply.sender_id
		WHERE m.conversation_id = ?
		  AND m.deleted_at IS NULL
		  AND m.created_at >= ?
		  AND m.created_at > COALESCE(?, '1970-01-01'::timestamptz)
		  AND NOT EXISTS (
		      SELECT 1 FROM user_blocks ub
		      WHERE (ub.blocker_id = ? AND ub.blocked_id = m.sender_id)
		         OR (ub.blocker_id = m.sender_id AND ub.blocked_id = ?)
		  )
		ORDER BY m.created_at DESC
		LIMIT ? OFFSET ?
	`, userID, conversationID, joinedAtFilter, me.ClearedAt, userID, userID, limit, offset).Scan(&messages)

	go h.markGroupMessagesRead(userID, conversationID)

	// Reaksiyalar — N+1 yox: səhifədəki bütün mesaj id-ləri üçün BİR sorğu,
	// sonra Go-da map ilə mesajlara paylanır.
	reactionsByMessage := map[string][]gin.H{}
	if len(messages) > 0 {
		msgIDs := make([]string, 0, len(messages))
		for _, msg := range messages {
			msgIDs = append(msgIDs, msg.ID)
		}

		var reactionRows []struct {
			MessageID string `gorm:"column:message_id"`
			UserID    uint   `gorm:"column:user_id"`
			Emoji     string `gorm:"column:emoji"`
		}
		database.DB.Raw(`
			SELECT message_id, user_id, emoji
			FROM group_message_reactions
			WHERE message_id IN (?)
		`, msgIDs).Scan(&reactionRows)

		for _, r := range reactionRows {
			reactionsByMessage[r.MessageID] = append(reactionsByMessage[r.MessageID], gin.H{
				"user_id": r.UserID,
				"emoji":   r.Emoji,
			})
		}
	}

	var result []gin.H
	for _, msg := range messages {
		text, _ := h.encryptionService.DecryptMessage(msg.EncryptedText)

		reactions := reactionsByMessage[msg.ID]
		if reactions == nil {
			reactions = []gin.H{}
		}

		item := gin.H{
			"id":                  msg.ID,
			"conversation_id":     conversationID,
			"sender_id":           msg.SenderID,
			"sender_name":         msg.SenderName,
			"sender_username":     msg.SenderUsername,
			"sender_is_verified":  msg.SenderIsVerified,
			"sender_avatar":       utils.PrependBaseURL(msg.SenderAvatar),
			"text":                text,
			"reply_to_message_id": msg.ReplyToMessageID,
			"read_count":          msg.ReadCount,
			"is_edited":           msg.IsEdited,
			"is_starred_by_me":    msg.IsStarredByMe,
			"reactions":           reactions,
			"created_at":          msg.CreatedAt,
		}

		if msg.ReplyToMessageID != nil && msg.ReplyText != nil {
			replyText, _ := h.encryptionService.DecryptMessage(*msg.ReplyText)
			replyVerified := false
			if msg.ReplyIsVerified != nil {
				replyVerified = *msg.ReplyIsVerified
			}
			replyUsername := ""
			if msg.ReplyUsername != nil {
				replyUsername = *msg.ReplyUsername
			}
			item["reply_to_message"] = gin.H{
				"id":                 *msg.ReplyToMessageID,
				"sender_id":          msg.ReplyToSenderID,
				"sender_username":    replyUsername,
				"sender_is_verified": replyVerified,
				"sender_avatar":      utils.PrependBaseURL(msg.ReplyAvatar),
				"text":               replyText,
				"created_at":         msg.CreatedAt, // reply-də də tarix (Flutter parse crash olmasın)
			}
		}

		result = append(result, item)
	}

	var total int64
	database.DB.Model(&models.Message{}).
		Where("conversation_id = ? AND deleted_at IS NULL", conversationID).
		Count(&total)

	c.JSON(http.StatusOK, gin.H{
		"data":  result,
		"page":  page,
		"limit": limit,
		"total": total,
	})
}

// GET /api/v1/groups/messages/:message_id/reads
func (h *GroupMessageHandler) GetMessageReads(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	messageID := c.Param("message_id")

	// Mesajın bu kullanıcının grubuna ait olduğunu doğrula
	var msg models.Message
	if err := database.DB.Where("id = ?", messageID).First(&msg).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Mesaj bulunamadı"})
		return
	}

	if msg.ConversationID == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Bu DM mesajı, group mesajı değil"})
		return
	}

	// Üye mi?
	var me models.ConversationParticipant
	err := database.DB.Where(
		"conversation_id = ? AND user_id = ? AND deleted_at IS NULL",
		*msg.ConversationID, userID,
	).First(&me).Error
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "Yetkiniz yok"})
		return
	}

	var reads []models.MessageReadDetail
	database.DB.Raw(`
		SELECT
			mr.user_id,
			u.name,
			u.username,
			u.is_verified,
			p.profile_image as avatar,
			mr.read_at
		FROM message_reads mr
		JOIN users u ON u.id = mr.user_id
		LEFT JOIN profiles p ON p.user_id = mr.user_id
		WHERE mr.message_id = ?
		  AND mr.user_id != ?
		ORDER BY mr.read_at ASC
	`, messageID, msg.SenderID).Scan(&reads)

	// Avatar nisbi yol gəlir — tam URL-ə çevir (Flutter birbaşa göstərsin).
	for i := range reads {
		reads[i].Avatar = utils.PrependBaseURL(reads[i].Avatar)
	}

	c.JSON(http.StatusOK, gin.H{
		"reads": reads,
		"count": len(reads),
	})
}

// markGroupMessagesRead — kullanıcının conversation'daki okunmamış mesajlarını işaretle
func (h *GroupMessageHandler) markGroupMessagesRead(userID, conversationID uint) {
	// Henüz okunmamış mesaj ID'leri bul
	var unreadIDs []string
	database.DB.Raw(`
		SELECT m.id FROM messages m
		LEFT JOIN message_reads mr ON mr.message_id = m.id AND mr.user_id = ?
		WHERE m.conversation_id = ?
		  AND m.sender_id != ?
		  AND mr.id IS NULL
		  AND m.deleted_at IS NULL
	`, userID, conversationID, userID).Pluck("id", &unreadIDs)

	if len(unreadIDs) == 0 {
		return
	}

	now := time.Now()
	var reads []models.MessageRead
	for _, msgID := range unreadIDs {
		reads = append(reads, models.MessageRead{
			MessageID:      msgID,
			UserID:         userID,
			ConversationID: conversationID,
			ReadAt:         now,
			CreatedAt:      now,
		})
	}

	// Bulk insert, çakışmada skip
	database.DB.Clauses().CreateInBatches(reads, 100)

	// participant last_read güncelle
	lastID := unreadIDs[len(unreadIDs)-1]
	database.DB.Model(&models.ConversationParticipant{}).
		Where("conversation_id = ? AND user_id = ?", conversationID, userID).
		Updates(map[string]interface{}{
			"last_read_at":         now,
			"last_read_message_id": lastID,
		})

	// Gönderenlere WebSocket ile bildir
	memberIDs := getGroupParticipantIDs(conversationID)
	h.wsHub.SendToMultipleUsers(memberIDs, "group_message_read", map[string]interface{}{
		"conversation_id": conversationID,
		"reader_id":       userID,
		"message_ids":     unreadIDs,
		"read_at":         now,
	})
}

// findGroupMessage — qrup mesajını tap + üzvlüyü yoxla. Hər message-id əsaslı
// handler-də təkrarlanan addımlar: (1) mesaj mövcud və silinməyib,
// (2) qrup mesajıdır (conversation_id dolu), (3) requester aktiv üzvdür.
// Uğursuzluqda HTTP cavabı yazılır və ok=false qaytarılır.
func (h *GroupMessageHandler) findGroupMessage(c *gin.Context, messageID string, userID uint) (msg models.Message, ok bool) {
	err := database.DB.Where("id = ? AND deleted_at IS NULL", messageID).First(&msg).Error
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Mesaj bulunamadı"})
		return msg, false
	}

	if msg.ConversationID == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Bu DM mesajı, group mesajı değil"})
		return msg, false
	}

	var me models.ConversationParticipant
	err = database.DB.Where(
		"conversation_id = ? AND user_id = ? AND left_at IS NULL AND deleted_at IS NULL",
		*msg.ConversationID, userID,
	).First(&me).Error
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "Bu grubun üyesi değilsiniz"})
		return msg, false
	}

	return msg, true
}

// DELETE /api/v1/groups/messages/:message_id
// Yalnız öz mesajını silə bilər. Soft-delete (deleted_at = now), sonra bütün
// üzvlərə group_message_deleted WS event-i.
func (h *GroupMessageHandler) DeleteGroupMessage(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	messageID := c.Param("message_id")

	msg, ok := h.findGroupMessage(c, messageID, userID)
	if !ok {
		return
	}

	// İcazə: öz mesajı VƏ YA admin/owner (admin hər kəsin mesajını hamı
	// üçün silə bilər).
	if msg.SenderID != userID {
		var me models.ConversationParticipant
		err := database.DB.Where(
			"conversation_id = ? AND user_id = ? AND left_at IS NULL AND deleted_at IS NULL",
			*msg.ConversationID, userID,
		).First(&me).Error
		if err != nil || (me.Role != "owner" && me.Role != "admin") {
			c.JSON(http.StatusForbidden, gin.H{"error": "Yalnız öz mesajını silə bilərsən"})
			return
		}
	}

	now := time.Now()
	if err := database.DB.Model(&models.Message{}).
		Where("id = ?", messageID).
		Update("deleted_at", now).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Mesaj silinemedi"})
		return
	}

	conversationID := *msg.ConversationID
	memberIDs := getGroupParticipantIDs(conversationID)
	h.wsHub.SendToMultipleUsers(memberIDs, "group_message_deleted", map[string]interface{}{
		"conversation_id": conversationID,
		"message_id":      messageID,
	})

	c.JSON(http.StatusOK, gin.H{
		"message": "Mesaj silindi",
		"data": gin.H{
			"conversation_id": conversationID,
			"message_id":      messageID,
		},
	})
}

// PUT /api/v1/groups/messages/:message_id
// Body: {"text": "..."}. Yalnız öz mesajı. Yeni text şifrələnir,
// encrypted_text + is_edited=true yenilənir (DM EditMessage kimi — vaxt
// limiti YOXDUR). Bütün üzvlərə group_message_edited WS event-i.
func (h *GroupMessageHandler) EditGroupMessage(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	messageID := c.Param("message_id")

	var body struct {
		Text string `json:"text" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	msg, ok := h.findGroupMessage(c, messageID, userID)
	if !ok {
		return
	}

	if msg.SenderID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "Yalnız öz mesajını edit edə bilərsən"})
		return
	}

	encryptedText, err := h.encryptionService.EncryptMessage(body.Text)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Şifreleme hatası"})
		return
	}

	now := time.Now()
	if err := database.DB.Model(&models.Message{}).
		Where("id = ?", messageID).
		Updates(map[string]interface{}{
			"encrypted_text": encryptedText,
			"is_edited":      true,
			"updated_at":     now,
		}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Mesaj güncellenemedi"})
		return
	}

	conversationID := *msg.ConversationID
	editPayload := map[string]interface{}{
		"conversation_id": conversationID,
		"message_id":      messageID,
		"text":            body.Text,
		"is_edited":       true,
		"edited_at":       now,
	}

	memberIDs := getGroupParticipantIDs(conversationID)
	h.wsHub.SendToMultipleUsers(memberIDs, "group_message_edited", editPayload)

	c.JSON(http.StatusOK, gin.H{
		"message": "Mesaj güncellendi",
		"data":    editPayload,
	})
}

// POST /api/v1/groups/messages/:message_id/star
// Per-user toggle: group_message_stars-da qeyd varsa silinir (unstar),
// yoxdursa əlavə olunur (star). Ulduz şəxsidir — WS broadcast YOXDUR.
func (h *GroupMessageHandler) ToggleGroupStar(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	messageID := c.Param("message_id")

	msg, ok := h.findGroupMessage(c, messageID, userID)
	if !ok {
		return
	}

	conversationID := *msg.ConversationID

	var existing int64
	database.DB.Raw(`
		SELECT COUNT(*) FROM group_message_stars
		WHERE message_id = ? AND user_id = ?
	`, messageID, userID).Scan(&existing)

	if existing > 0 {
		if err := database.DB.Exec(`
			DELETE FROM group_message_stars
			WHERE message_id = ? AND user_id = ?
		`, messageID, userID).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Ulduzlama uğursuz oldu"})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"message":    "OK",
			"message_id": messageID,
			"is_starred": false,
		})
		return
	}

	if err := database.DB.Exec(`
		INSERT INTO group_message_stars (message_id, user_id, conversation_id, created_at)
		VALUES (?, ?, ?, ?)
	`, messageID, userID, conversationID, time.Now()).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Ulduzlama uğursuz oldu"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message":    "OK",
		"message_id": messageID,
		"is_starred": true,
	})
}

// GET /api/v1/groups/:conversation_id/starred
// Bu istifadəçinin BU qrupda ulduzladığı mesajlar (GetGroupMessages item
// formatında, decrypt olunmuş). Silinmiş mesajlar görünmür.
func (h *GroupMessageHandler) GetGroupStarred(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	convID, err := strconv.ParseUint(c.Param("conversation_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz conversation_id"})
		return
	}
	conversationID := uint(convID)

	var me models.ConversationParticipant
	err = database.DB.Where(
		"conversation_id = ? AND user_id = ? AND left_at IS NULL AND deleted_at IS NULL",
		conversationID, userID,
	).First(&me).Error
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "Bu grubun üyesi değilsiniz"})
		return
	}

	var rows []struct {
		ID               string    `gorm:"column:id"`
		SenderID         uint      `gorm:"column:sender_id"`
		SenderName       string    `gorm:"column:sender_name"`
		SenderUsername   string    `gorm:"column:sender_username"`
		SenderIsVerified bool      `gorm:"column:sender_is_verified"`
		SenderAvatar     *string   `gorm:"column:sender_avatar"`
		EncryptedText    string    `gorm:"column:encrypted_text"`
		IsEdited         bool      `gorm:"column:is_edited"`
		CreatedAt        time.Time `gorm:"column:created_at"`
	}

	database.DB.Raw(`
		SELECT
			m.id,
			m.sender_id,
			u.name as sender_name,
			u.username as sender_username,
			u.is_verified as sender_is_verified,
			p.profile_image as sender_avatar,
			m.encrypted_text,
			m.is_edited,
			m.created_at
		FROM group_message_stars gms
		JOIN messages m ON m.id = gms.message_id
		JOIN users u ON u.id = m.sender_id
		LEFT JOIN profiles p ON p.user_id = m.sender_id
		WHERE gms.conversation_id = ?
		  AND gms.user_id = ?
		  AND m.deleted_at IS NULL
		ORDER BY m.created_at DESC
	`, conversationID, userID).Scan(&rows)

	result := []gin.H{}
	for _, r := range rows {
		text, _ := h.encryptionService.DecryptMessage(r.EncryptedText)
		result = append(result, gin.H{
			"id":                 r.ID,
			"conversation_id":    conversationID,
			"sender_id":          r.SenderID,
			"sender_name":        r.SenderName,
			"sender_username":    r.SenderUsername,
			"sender_is_verified": r.SenderIsVerified,
			"sender_avatar":      utils.PrependBaseURL(r.SenderAvatar),
			"text":               text,
			"is_edited":          r.IsEdited,
			"is_starred_by_me":   true,
			"created_at":         r.CreatedAt,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"messages": result,
		"total":    len(result),
	})
}

// POST /api/v1/groups/messages/:message_id/reaction
// Body: {"emoji": "..."}. Toggle davranışı:
//   - eyni emoji artıq varsa → silinir (action: "removed")
//   - yoxdursa/fərqlidirsə → UPSERT (action: "added")
//
// Bütün üzvlərə group_reaction_updated WS event-i (silinəndə emoji null).
func (h *GroupMessageHandler) SetGroupReaction(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	messageID := c.Param("message_id")

	var body struct {
		Emoji string `json:"emoji" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "emoji gerekli"})
		return
	}

	msg, ok := h.findGroupMessage(c, messageID, userID)
	if !ok {
		return
	}

	conversationID := *msg.ConversationID
	now := time.Now()

	// Mövcud reaksiyaya bax — eyni emoji isə toggle (sil).
	var existing struct {
		Emoji *string `gorm:"column:emoji"`
	}
	database.DB.Raw(`
		SELECT emoji FROM group_message_reactions
		WHERE message_id = ? AND user_id = ?
	`, messageID, userID).Scan(&existing)

	memberIDs := getGroupParticipantIDs(conversationID)

	if existing.Emoji != nil && *existing.Emoji == body.Emoji {
		if err := database.DB.Exec(`
			DELETE FROM group_message_reactions
			WHERE message_id = ? AND user_id = ?
		`, messageID, userID).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Reaksiyon kaldırılamadı"})
			return
		}

		removedPayload := map[string]interface{}{
			"conversation_id": conversationID,
			"message_id":      messageID,
			"user_id":         userID,
			"emoji":           nil,
			"action":          "removed",
		}
		h.wsHub.SendToMultipleUsers(memberIDs, "group_reaction_updated", removedPayload)

		c.JSON(http.StatusOK, gin.H{
			"message": "Reaksiyon kaldırıldı",
			"data":    removedPayload,
		})
		return
	}

	// Yoxdursa insert, fərqlidirsə yenisi ilə əvəzlə (UPSERT).
	if err := database.DB.Exec(`
		INSERT INTO group_message_reactions (message_id, user_id, conversation_id, emoji, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (message_id, user_id)
		DO UPDATE SET emoji = EXCLUDED.emoji, updated_at = EXCLUDED.updated_at
	`, messageID, userID, conversationID, body.Emoji, now, now).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Reaksiyon kaydedilemedi"})
		return
	}

	addedPayload := map[string]interface{}{
		"conversation_id": conversationID,
		"message_id":      messageID,
		"user_id":         userID,
		"emoji":           body.Emoji,
		"action":          "added",
	}
	h.wsHub.SendToMultipleUsers(memberIDs, "group_reaction_updated", addedPayload)

	c.JSON(http.StatusOK, gin.H{
		"message": "Reaksiyon eklendi",
		"data":    addedPayload,
	})
}
