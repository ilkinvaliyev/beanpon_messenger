package handlers

import (
	"beanpon_messenger/database"
	"beanpon_messenger/models"
	"beanpon_messenger/services"
	"beanpon_messenger/websocket"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

type MessageHandler struct {
	encryptionService *services.EncryptionService
	wsHub             *websocket.Hub
}

// NewMessageHandler yeni message handler oluştur
func NewMessageHandler(encryptionService *services.EncryptionService, wsHub *websocket.Hub) *MessageHandler {
	return &MessageHandler{
		encryptionService: encryptionService,
		wsHub:             wsHub,
	}
}

// SendMessage mesaj gönder
func (h *MessageHandler) SendMessage(c *gin.Context) {
	// JWT'den user ID al
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	var req models.SendMessageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz mesaj formatı"})
		return
	}

	// Kendi kendine mesaj gönderme kontrolü
	if userID.(uint) == req.ReceiverID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Kendinize mesaj gönderemezsiniz"})
		return
	}

	// Alıcının var olup olmadığını kontrol et
	var receiver models.User
	if err := database.DB.First(&receiver, req.ReceiverID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Alıcı bulunamadı"})
		return
	}

	// Mesajı şifrele
	encryptedText, err := h.encryptionService.EncryptMessage(req.Text)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Mesaj şifreleme hatası"})
		return
	}

	// Mesajı veritabanına kaydet
	message := models.Message{
		SenderID:      userID.(uint),
		ReceiverID:    req.ReceiverID,
		EncryptedText: encryptedText,
	}

	if err := database.DB.Create(&message).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Mesaj kaydetme hatası"})
		return
	}

	// WebSocket ile alıcıya gönder
	h.wsHub.SendToUser(req.ReceiverID, "new_message", map[string]interface{}{
		"id":         message.ID,
		"sender_id":  message.SenderID,
		"text":       req.Text, // Şifrelenmemiş metin
		"created_at": message.CreatedAt,
	})

	// Response döndür
	c.JSON(http.StatusCreated, gin.H{
		"success": true,
		"message": "Mesaj gönderildi",
		"data": models.MessageResponse{
			ID:         message.ID,
			SenderID:   message.SenderID,
			ReceiverID: message.ReceiverID,
			Text:       req.Text,
			IsEdited:   message.IsEdited,
			Delivered:  message.Delivered,
			Read:       message.Read,
			CreatedAt:  message.CreatedAt,
			UpdatedAt:  message.UpdatedAt,
		},
	})
}

// GetMessages mesajları listele (pagination ile)
func (h *MessageHandler) GetMessages(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	// URL parametrelerini al
	otherUserIDStr := c.Param("user_id")
	otherUserID, err := strconv.ParseUint(otherUserIDStr, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz kullanıcı ID"})
		return
	}

	// Pagination parametreleri
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	offset := (page - 1) * limit

	// Mesajları getir
	var messages []models.Message
	query := database.DB.Where(
		"(sender_id = ? AND receiver_id = ?) OR (sender_id = ? AND receiver_id = ?)",
		userID.(uint), uint(otherUserID), uint(otherUserID), userID.(uint),
	).Order("created_at DESC").Limit(limit).Offset(offset)

	if err := query.Find(&messages).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Mesajlar getirilemedi"})
		return
	}

	// Mesajları çöz ve response formatına çevir
	var responses []models.MessageResponse
	for _, msg := range messages {
		// Mesajı çöz
		decryptedText, err := h.encryptionService.DecryptMessage(msg.EncryptedText)
		if err != nil {
			decryptedText = "Mesaj çözülemedi"
		}

		responses = append(responses, models.MessageResponse{
			ID:         msg.ID,
			SenderID:   msg.SenderID,
			ReceiverID: msg.ReceiverID,
			Text:       decryptedText,
			IsEdited:   msg.IsEdited,
			Delivered:  msg.Delivered,
			Read:       msg.Read,
			ReadAt:     msg.ReadAt,
			CreatedAt:  msg.CreatedAt,
			UpdatedAt:  msg.UpdatedAt,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    responses,
		"page":    page,
		"limit":   limit,
	})
}

// MarkAsRead mesajı okundu olarak işaretle
func (h *MessageHandler) MarkAsRead(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	messageID := c.Param("message_id")

	// Mesajı bul ve güncelle
	result := database.DB.Model(&models.Message{}).
		Where("id = ? AND receiver_id = ?", messageID, userID.(uint)).
		Updates(map[string]interface{}{
			"read":    true,
			"read_at": "NOW()",
		})

	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Mesaj güncellenemedi"})
		return
	}

	if result.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Mesaj bulunamadı"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Mesaj okundu olarak işaretlendi",
	})
}

// GetConversations sohbet listesi
func (h *MessageHandler) GetConversations(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	// En son mesajları getir (her kullanıcı için)
	var conversations []struct {
		OtherUserID   uint   `json:"other_user_id"`
		LastMessage   string `json:"last_message"`
		LastMessageAt string `json:"last_message_at"`
		UnreadCount   int    `json:"unread_count"`
	}

	// Bu karmaşık bir query, basitleştirilmiş hali
	query := `
		SELECT 
			CASE 
				WHEN sender_id = ? THEN receiver_id 
				ELSE sender_id 
			END as other_user_id,
			encrypted_text as last_message,
			created_at as last_message_at
		FROM messages 
		WHERE sender_id = ? OR receiver_id = ?
		ORDER BY created_at DESC
	`

	if err := database.DB.Raw(query, userID.(uint), userID.(uint), userID.(uint)).Scan(&conversations).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Sohbetler getirilemedi"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    conversations,
	})
}
