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
		ProfileImage *string `json:"profile_image"`
	}
	database.DB.Raw(`
		SELECT u.name, u.username, p.profile_image
		FROM users u
		LEFT JOIN profiles p ON p.user_id = u.id
		WHERE u.id = ?
	`, senderID).Scan(&senderInfo)

	// Reply bilgisi
	var replyData map[string]interface{}
	if req.ReplyToMessageID != nil {
		var replyMsg models.Message
		if err := database.DB.Where("id = ?", *req.ReplyToMessageID).First(&replyMsg).Error; err == nil {
			replyDecrypted, _ := h.encryptionService.DecryptMessage(replyMsg.EncryptedText)
			replyData = map[string]interface{}{
				"id":         replyMsg.ID,
				"sender_id":  replyMsg.SenderID,
				"text":       replyDecrypted,
				"created_at": replyMsg.CreatedAt,
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
		"sender_avatar":       utils.PrependBaseURL(senderInfo.ProfileImage),
		"text":                req.Text,
		"reply_to_message_id": req.ReplyToMessageID,
		"reply_to_message":    replyData,
		"created_at":          now.UTC().Format(time.RFC3339),
	}
	h.wsHub.SendToMultipleUsers(memberIDs, "new_group_message", wsPayload)

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

	// Üye mi?
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

	var messages []struct {
		ID               string    `gorm:"column:id"`
		SenderID         uint      `gorm:"column:sender_id"`
		SenderName       string    `gorm:"column:sender_name"`
		SenderUsername   string    `gorm:"column:sender_username"`
		SenderAvatar     *string   `gorm:"column:sender_avatar"`
		EncryptedText    string    `gorm:"column:encrypted_text"`
		ReplyToMessageID *string   `gorm:"column:reply_to_message_id"`
		ReplyText        *string   `gorm:"column:reply_text"`
		ReplyToSenderID  *uint     `gorm:"column:reply_to_sender_id"`
		ReadCount        int       `gorm:"column:read_count"`
		CreatedAt        time.Time `gorm:"column:created_at"`
	}

	database.DB.Raw(`
		SELECT 
			m.id,
			m.sender_id,
			u.name as sender_name,
			u.username as sender_username,
			p.profile_image as sender_avatar,
			m.encrypted_text,
			m.reply_to_message_id,
			reply.encrypted_text as reply_text,
			reply.sender_id as reply_to_sender_id,
			(SELECT COUNT(*) FROM message_reads mr WHERE mr.message_id = m.id AND mr.user_id != m.sender_id) as read_count,
			m.created_at
		FROM messages m
		JOIN users u ON u.id = m.sender_id
		LEFT JOIN profiles p ON p.user_id = m.sender_id
		LEFT JOIN messages reply ON reply.id = m.reply_to_message_id
		WHERE m.conversation_id = ?
		  AND m.deleted_at IS NULL
		ORDER BY m.created_at DESC
		LIMIT ? OFFSET ?
	`, conversationID, limit, offset).Scan(&messages)

	// Bulk read: bu kullanıcının henüz okumadığı mesajları işaretle
	go h.markGroupMessagesRead(userID, conversationID)

	// Decrypt ve response hazırla
	var result []gin.H
	for _, msg := range messages {
		text, _ := h.encryptionService.DecryptMessage(msg.EncryptedText)

		item := gin.H{
			"id":                  msg.ID,
			"conversation_id":     conversationID,
			"sender_id":           msg.SenderID,
			"sender_name":         msg.SenderName,
			"sender_username":     msg.SenderUsername,
			"sender_avatar":       msg.SenderAvatar,
			"text":                text,
			"reply_to_message_id": msg.ReplyToMessageID,
			"read_count":          msg.ReadCount,
			"created_at":          msg.CreatedAt,
		}

		if msg.ReplyToMessageID != nil && msg.ReplyText != nil {
			replyText, _ := h.encryptionService.DecryptMessage(*msg.ReplyText)
			item["reply_to_message"] = gin.H{
				"id":        *msg.ReplyToMessageID,
				"sender_id": msg.ReplyToSenderID,
				"text":      replyText,
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
			u.username,
			p.profile_image as avatar,
			mr.read_at
		FROM message_reads mr
		JOIN users u ON u.id = mr.user_id
		LEFT JOIN profiles p ON p.user_id = mr.user_id
		WHERE mr.message_id = ?
		  AND mr.user_id != ?
		ORDER BY mr.read_at ASC
	`, messageID, msg.SenderID).Scan(&reads)

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
