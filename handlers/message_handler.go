package handlers

import (
	"beanpon_messenger/database"
	"beanpon_messenger/models"
	"beanpon_messenger/utils"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type MessageHandler struct {
	encryptionService interface {
		EncryptMessage(plainText string) (string, error)
		DecryptMessage(encryptedText string) (string, error)
	}
	wsHub interface {
		HandleNewMessage(senderID, receiverID uint, messageID, content, msgType string, createdAt time.Time)
		HandleMessageRead(messageID string, senderID, readerID uint)
		IsUserOnline(userID uint) bool
		SendToUser(userID uint, messageType string, data interface{})
	}
}

func NewMessageHandler(encryptionService interface {
	EncryptMessage(plainText string) (string, error)
	DecryptMessage(encryptedText string) (string, error)
}, wsHub interface {
	HandleNewMessage(senderID, receiverID uint, messageID, content, msgType string, createdAt time.Time)
	HandleMessageRead(messageID string, senderID, readerID uint)
	IsUserOnline(userID uint) bool
	SendToUser(userID uint, messageType string, data interface{})
}) *MessageHandler {
	return &MessageHandler{
		encryptionService: encryptionService,
		wsHub:             wsHub,
	}
}

// SendMessage mesaj gönder
func (h *MessageHandler) SendMessage(c *gin.Context) {
	// JWT'den user ID al
	senderID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	var req struct {
		ReceiverID       uint    `json:"receiver_id" binding:"required"`
		Text             string  `json:"text" binding:"required"`
		Type             string  `json:"type,omitempty"`
		ReplyToMessageID *string `json:"reply_to_message_id,omitempty"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Kendi kendine mesaj göndermesini engelle
	if senderID.(uint) == req.ReceiverID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Kendi kendinize mesaj gönderemezsiniz"})
		return
	}

	// Conversation kontrolü - mesaj gönderebilir mi?
	conversationHandler := NewConversationHandler(h.wsHub, h.encryptionService)
	canSend, reason, err := conversationHandler.CanSendMessage(senderID.(uint), req.ReceiverID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Conversation kontrolü başarısız"})
		return
	}

	if !canSend {
		c.JSON(http.StatusForbidden, gin.H{"error": reason})
		return
	}

	// Mesajı şifrele
	encryptedText, err := h.encryptionService.EncryptMessage(req.Text)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Mesaj şifrelenirken hata oluştu"})
		return
	}

	// Veritabanına kaydet
	message := models.Message{
		ID:            uuid.New().String(),
		SenderID:      senderID.(uint),
		ReceiverID:    req.ReceiverID,
		EncryptedText: encryptedText,
		Read:          false,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}

	if err := database.DB.Create(&message).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Mesaj kaydedilemedi"})
		return
	}

	// Conversation durumunu güncelle
	if err := conversationHandler.UpdateConversationOnMessage(senderID.(uint), req.ReceiverID); err != nil {
		// Log et ama işlemi durdurmaja
		log.Printf("Conversation güncellemesi başarısız: %v", err)
	}

	// WebSocket üzerinden real-time yayınla (hem gönderen hem alıcıya)
	h.wsHub.HandleNewMessage(
		message.SenderID,
		message.ReceiverID,
		message.ID,
		req.Text,
		req.Type,
		message.CreatedAt,
	)

	// API response
	c.JSON(http.StatusCreated, gin.H{
		"message": "Mesaj başarıyla gönderildi",
		"data": gin.H{
			"id":          message.ID,
			"sender_id":   message.SenderID,
			"receiver_id": message.ReceiverID,
			"text":        req.Text,
			"read":        message.Read,
			"created_at":  message.CreatedAt,
			"is_online":   h.wsHub.IsUserOnline(req.ReceiverID),
		},
	})
}

// GetMessages belirli kullanıcı ile mesajları getir
func (h *MessageHandler) GetMessages(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	otherUserID, err := strconv.ParseUint(c.Param("user_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz kullanıcı ID"})
		return
	}

	// Sayfa parametreleri
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	offset := (page - 1) * limit

	var messages []models.Message

	// 🆕 Silinmiş mesajları filter et - user'a görə
	query := `
		SELECT * FROM messages 
		WHERE ((sender_id = ? AND receiver_id = ?) OR (sender_id = ? AND receiver_id = ?))
		AND (
			CASE 
				WHEN sender_id = ? THEN is_deleted_by_sender = false
				ELSE is_deleted_by_receiver = false
			END
		)
		ORDER BY created_at DESC 
		LIMIT ? OFFSET ?
	`

	err = database.DB.Raw(query,
		userID, otherUserID, otherUserID, userID, // mesaj filtri
		userID, // delete filtri üçün
		limit, offset,
	).Find(&messages).Error

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Mesajlar alınamadı"})
		return
	}

	// Mesajları çöz ve response'a hazırla
	var responseMessages []gin.H
	for _, msg := range messages {
		decryptedText, err := h.encryptionService.DecryptMessage(msg.EncryptedText)
		if err != nil {
			decryptedText = "Mesaj çözülemedi"
		}

		responseMessages = append(responseMessages, gin.H{
			"id":                  msg.ID,
			"sender_id":           msg.SenderID,
			"receiver_id":         msg.ReceiverID,
			"reply_to_message_id": msg.ReplyToMessageID,
			"text":                decryptedText,
			"read":                msg.Read,
			"sender_reaction":     msg.SenderReaction,
			"receiver_reaction":   msg.ReceiverReaction,
			"created_at":          msg.CreatedAt,
			"updated_at":          msg.UpdatedAt,
		})
	}

	// Okunmamış mesajları okundu olarak işaretle (sadece gelen mesajlar)
	go h.markReceivedMessagesAsRead(userID.(uint), uint(otherUserID))

	c.JSON(http.StatusOK, gin.H{
		"data":      responseMessages, // "messages" deyil "data" olacaq
		"page":      page,
		"limit":     limit,
		"total":     len(responseMessages),
		"is_online": h.wsHub.IsUserOnline(uint(otherUserID)),
	})
}

// markReceivedMessagesAsRead alınan mesajları okundu olarak işaretle
func (h *MessageHandler) markReceivedMessagesAsRead(currentUserID, otherUserID uint) {
	var unreadMessages []models.Message

	// Karşı taraftan gelen okunmamış mesajları bul
	err := database.DB.Where(
		"sender_id = ? AND receiver_id = ? AND read = false",
		otherUserID, currentUserID,
	).Find(&unreadMessages).Error

	if err != nil {
		return
	}

	// Okundu olarak işaretle
	database.DB.Model(&models.Message{}).Where(
		"sender_id = ? AND receiver_id = ? AND read = false",
		otherUserID, currentUserID,
	).Update("read", true)

	// Her mesaj için WebSocket bildirimi gönder
	for _, msg := range unreadMessages {
		h.wsHub.HandleMessageRead(msg.ID, msg.SenderID, currentUserID)
	}
}

// MarkAsRead mesajı okundu olarak işaretle
func (h *MessageHandler) MarkAsRead(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	messageID := c.Param("message_id")

	var message models.Message
	err := database.DB.Where("id = ?", messageID).First(&message).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "Mesaj bulunamadı"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Veritabanı hatası"})
		}
		return
	}

	// Sadece alıcı mesajı okundu olarak işaretleyebilir
	if message.ReceiverID != userID.(uint) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Bu mesajı okundu olarak işaretleme yetkiniz yok"})
		return
	}

	// Zaten okunmuşsa
	if message.Read {
		c.JSON(http.StatusOK, gin.H{"message": "Mesaj zaten okunmuş"})
		return
	}

	// Okundu olarak işaretle
	message.Read = true
	message.UpdatedAt = time.Now()

	if err := database.DB.Save(&message).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Mesaj güncellenemedi"})
		return
	}

	// WebSocket üzerinden gönderene bildir
	h.wsHub.HandleMessageRead(message.ID, message.SenderID, userID.(uint))

	c.JSON(http.StatusOK, gin.H{
		"message": "Mesaj okundu olarak işaretlendi",
		"data": gin.H{
			"message_id": message.ID,
			"read":       message.Read,
			"read_at":    message.UpdatedAt,
		},
	})
}

// GetConversations sohbet listesi
func (h *MessageHandler) GetConversations(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	// En son mesajları getir - silinmiş olanları hariç tut
	var conversations []struct {
		OtherUserID     uint      `json:"other_user_id"`
		LastMessageID   string    `json:"last_message_id"`
		LastMessageText string    `json:"last_message_text"`
		LastMessageTime time.Time `json:"last_message_time"`
		IsLastFromMe    bool      `json:"is_last_from_me"`
		UnreadCount     int       `json:"unread_count"`

		OtherUserName     string  `json:"other_user_name"`
		OtherUserUsername string  `json:"other_user_username"`
		AccountTypeID     int     `json:"account_type_id"`
		ProfileImage      *string `json:"profile_image"`
	}

	query := `
		WITH latest_messages AS (
			SELECT 
			   CASE 
				  WHEN sender_id = ? THEN receiver_id 
				  ELSE sender_id 
			   END as other_user_id,
			   id,
			   encrypted_text,
			   created_at,
			   sender_id = ? as is_from_me,
			   ROW_NUMBER() OVER (
				  PARTITION BY CASE WHEN sender_id = ? THEN receiver_id ELSE sender_id END 
				  ORDER BY created_at DESC
			   ) as rn
			FROM messages 
			WHERE (sender_id = ? OR receiver_id = ?)
			AND (
			   CASE 
				  WHEN sender_id = ? THEN is_deleted_by_sender = false
				  ELSE is_deleted_by_receiver = false
			   END
			)
			),
			unread_counts AS (
				SELECT 
				   sender_id as other_user_id,
				   COUNT(*) as unread_count
				FROM messages 
				WHERE receiver_id = ? AND read = false 
				AND is_deleted_by_receiver = false
				GROUP BY sender_id
			)
			SELECT 
				lm.other_user_id,
				lm.id as last_message_id,
				lm.encrypted_text as last_message_text,
				lm.created_at as last_message_time,
				lm.is_from_me,
				COALESCE(uc.unread_count, 0) as unread_count,
				u.name as other_user_name,
				u.username as other_user_username,
				u.account_type_id,
				p.profile_image
			FROM latest_messages lm
			LEFT JOIN unread_counts uc ON lm.other_user_id = uc.other_user_id
			LEFT JOIN users u ON u.id = lm.other_user_id
			LEFT JOIN profiles p ON p.user_id = lm.other_user_id
			LEFT JOIN conversations conv ON (
			  (conv.user1_id = ? AND conv.user2_id = lm.other_user_id) OR 
			  (conv.user2_id = ? AND conv.user1_id = lm.other_user_id)
			)
			WHERE lm.rn = 1 
			AND (conv.status = 'active' OR conv.status IS NULL)
			ORDER BY lm.created_at DESC
			`

	err := database.DB.Raw(query,
		userID, userID, userID, userID, userID, // latest_messages üçün
		userID, // delete filter üçün
		userID, // unread_counts üçün
	).Scan(&conversations).Error

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Konuşmalar alınamadı"})
		return
	}

	// Mesajları çöz ve online durumları ekle
	var responseConversations []gin.H
	for _, conv := range conversations {
		decryptedText, err := h.encryptionService.DecryptMessage(conv.LastMessageText)
		if err != nil {
			decryptedText = "Mesaj çözülemedi"
		}

		responseConversations = append(responseConversations, gin.H{
			"other_user_id":       conv.OtherUserID,
			"other_user_name":     conv.OtherUserName,
			"other_user_username": conv.OtherUserUsername,
			"account_type_id":     conv.AccountTypeID,
			"profile_image":       utils.PrependBaseURL(conv.ProfileImage),
			"last_message_id":     conv.LastMessageID,
			"last_message_text":   decryptedText,
			"last_message_time":   conv.LastMessageTime,
			"is_last_from_me":     conv.IsLastFromMe,
			"unread_count":        conv.UnreadCount,
			"is_online":           h.wsHub.IsUserOnline(conv.OtherUserID),
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"conversations": responseConversations,
		"total":         len(responseConversations),
	})
}

// GetUnreadCount okunmamış mesaj sayısı
func (h *MessageHandler) GetUnreadCount(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	var count int64
	err := database.DB.Model(&models.Message{}).Where(
		"receiver_id = ? AND read = false AND is_deleted_by_receiver = false",
		userID,
	).Count(&count).Error

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Sayım yapılamadı"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"unread_count": count,
	})
}

// DeleteMessage mesajı sil (yalnız özündən və ya hər iki tərəfdən)
func (h *MessageHandler) DeleteMessage(c *gin.Context) {
	userIDVal, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	userID := userIDVal.(uint)

	messageID := c.Param("message_id")

	var body struct {
		DeleteType string `json:"delete_type" binding:"required"` // "me" və ya "both"
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "delete_type: 'me' və ya 'both' olmalıdır"})
		return
	}

	var message models.Message
	err := database.DB.Where("id = ?", messageID).First(&message).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Mesaj tapılmadı"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Veritabanı xətası"})
		}
		return
	}

	now := time.Now()

	// Silmə növünə görə işləmə
	switch body.DeleteType {
	case "me":
		if userID == message.SenderID {
			message.IsDeletedBySender = true
		} else if userID == message.ReceiverID {
			message.IsDeletedByReceiver = true
		} else {
			c.JSON(http.StatusForbidden, gin.H{"error": "Bu mesajı silmək icazən yoxdur"})
			return
		}

	case "both":
		// Yalnız göndərən hər iki tərəfdən silə bilər
		if userID != message.SenderID {
			c.JSON(http.StatusForbidden, gin.H{"error": "Yalnız göndərən hər iki tərəfdən silə bilər"})
			return
		}
		message.IsDeletedBySender = true
		message.IsDeletedByReceiver = true

	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçərsiz delete_type. 'me' və ya 'both' olmalıdır"})
		return
	}

	message.UpdatedAt = now
	if err := database.DB.Save(&message).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Silinmə uğursuz oldu"})
		return
	}

	// 🔔 WebSocket bildirimi hər iki tərəfə
	deletePayload := map[string]interface{}{
		"message_id":  message.ID,
		"deleted_by":  userID,
		"delete_type": body.DeleteType,
		"deleted_at":  now,
	}

	h.wsHub.SendToUser(message.SenderID, "message_deleted", deletePayload)
	h.wsHub.SendToUser(message.ReceiverID, "message_deleted", deletePayload)

	c.JSON(http.StatusOK, gin.H{
		"message": "Mesaj silindi",
		"data":    deletePayload,
	})
}

// DELETE /api/v1/conversations/:other_user_id/clear  body: { "delete_type": "me" | "both" }
func (h *MessageHandler) ClearConversation(c *gin.Context) {
	u, ok := c.Get("user_id")
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	currentUserID := u.(uint)

	otherStr := c.Param("other_user_id")
	otherU64, err := strconv.ParseUint(otherStr, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz user_id"})
		return
	}
	otherUserID := uint(otherU64)

	var body struct {
		DeleteType string `json:"delete_type" binding:"required"` // "me" | "both"
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "delete_type 'me' veya 'both' olmalıdır"})
		return
	}

	now := time.Now()
	tx := database.DB.Begin()
	if tx.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Transaction açılamadı"})
		return
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	var sentRes, recvRes int64

	switch body.DeleteType {
	case "me":
		// Benim GÖNDERDİKLERİM → sender tarafında gizle
		r1 := tx.Model(&models.Message{}).
			Where("sender_id = ? AND receiver_id = ? AND is_deleted_by_sender = FALSE", currentUserID, otherUserID).
			Updates(map[string]interface{}{"is_deleted_by_sender": true, "updated_at": now})
		sentRes = r1.RowsAffected
		if r1.Error != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Gönderdiğin mesajlar gizlenemedi"})
			return
		}

		// Benim ALDIKLARIM → receiver tarafında gizle
		r2 := tx.Model(&models.Message{}).
			Where("receiver_id = ? AND sender_id = ? AND is_deleted_by_receiver = FALSE", currentUserID, otherUserID).
			Updates(map[string]interface{}{"is_deleted_by_receiver": true, "updated_at": now})
		recvRes = r2.RowsAffected
		if r2.Error != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Aldığın mesajlar gizlenemedi"})
			return
		}

	case "both":
		// SADECE BENİM GÖNDERDİKLERİM → iki taraf için de gizle
		r1 := tx.Model(&models.Message{}).
			Where("sender_id = ? AND receiver_id = ? AND (is_deleted_by_sender = FALSE OR is_deleted_by_receiver = FALSE)", currentUserID, otherUserID).
			Updates(map[string]interface{}{"is_deleted_by_sender": true, "is_deleted_by_receiver": true, "updated_at": now})
		sentRes = r1.RowsAffected
		if r1.Error != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "İki taraftan gizleme (senin gönderdiklerin) başarısız"})
			return
		}

		// Karşı tarafın GÖNDERDİKLERİ → yalnızca benim tarafımda gizle (etik/izin gereği)
		r2 := tx.Model(&models.Message{}).
			Where("receiver_id = ? AND sender_id = ? AND is_deleted_by_receiver = FALSE", currentUserID, otherUserID).
			Updates(map[string]interface{}{"is_deleted_by_receiver": true, "updated_at": now})
		recvRes = r2.RowsAffected
		if r2.Error != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Karşıdan gelenleri gizleme başarısız"})
			return
		}

	default:
		tx.Rollback()
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz delete_type"})
		return
	}

	if err := tx.Commit().Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Commit başarısız"})
		return
	}

	payload := gin.H{
		"cleared_by":    currentUserID,
		"other_user_id": otherUserID,
		"delete_type":   body.DeleteType,
		"cleared_at":    now,
		"affected_sent": sentRes,
		"affected_recv": recvRes,
		"scope":         "conversation",
	}
	// UI’nin senkron olması için event
	h.wsHub.SendToUser(currentUserID, "conversation_cleared", payload)
	h.wsHub.SendToUser(otherUserID, "peer_conversation_cleared", payload)

	c.JSON(http.StatusOK, gin.H{
		"message": "Konuşma temizlendi",
		"data":    payload,
	})
}

// DELETE /api/v1/conversations/clear-all  body: { "delete_type": "me" | "both" }
func (h *MessageHandler) ClearAllMyMessages(c *gin.Context) {
	u, ok := c.Get("user_id")
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	currentUserID := u.(uint)

	var body struct {
		DeleteType string `json:"delete_type" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "delete_type 'me' veya 'both' olmalıdır"})
		return
	}

	now := time.Now()
	tx := database.DB.Begin()
	if tx.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Transaction açılamadı"})
		return
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	var sentRes, recvRes int64

	switch body.DeleteType {
	case "me":
		r1 := tx.Model(&models.Message{}).
			Where("sender_id = ? AND is_deleted_by_sender = FALSE", currentUserID).
			Updates(map[string]interface{}{"is_deleted_by_sender": true, "updated_at": now})
		sentRes = r1.RowsAffected
		if r1.Error != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Gönderdiğin mesajlar gizlenemedi"})
			return
		}

		r2 := tx.Model(&models.Message{}).
			Where("receiver_id = ? AND is_deleted_by_receiver = FALSE", currentUserID).
			Updates(map[string]interface{}{"is_deleted_by_receiver": true, "updated_at": now})
		recvRes = r2.RowsAffected
		if r2.Error != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Aldığın mesajlar gizlenemedi"})
			return
		}

	case "both":
		// Sadece benim GÖNDERDİKLERİM iki taraftan gizlenir
		r1 := tx.Model(&models.Message{}).
			Where("sender_id = ? AND (is_deleted_by_sender = FALSE OR is_deleted_by_receiver = FALSE)", currentUserID).
			Updates(map[string]interface{}{"is_deleted_by_sender": true, "is_deleted_by_receiver": true, "updated_at": now})
		sentRes = r1.RowsAffected
		if r1.Error != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "İki taraf için gizleme (gönderdiğin) başarısız"})
			return
		}

		// Aldıkların benim tarafımda gizlenir
		r2 := tx.Model(&models.Message{}).
			Where("receiver_id = ? AND is_deleted_by_receiver = FALSE", currentUserID).
			Updates(map[string]interface{}{"is_deleted_by_receiver": true, "updated_at": now})
		recvRes = r2.RowsAffected
		if r2.Error != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Aldığın mesajlar gizlenemedi"})
			return
		}

	default:
		tx.Rollback()
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz delete_type"})
		return
	}

	if err := tx.Commit().Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Commit başarısız"})
		return
	}

	payload := gin.H{
		"cleared_by":    currentUserID,
		"delete_type":   body.DeleteType,
		"cleared_at":    now,
		"affected_sent": sentRes,
		"affected_recv": recvRes,
		"scope":         "all",
	}
	h.wsHub.SendToUser(currentUserID, "all_messages_cleared", payload)

	c.JSON(http.StatusOK, gin.H{
		"message": "Tüm mesaj geçmişin temizlendi",
		"data":    payload,
	})
}
