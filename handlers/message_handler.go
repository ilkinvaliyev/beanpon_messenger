package handlers

import (
	"beanpon_messenger/database"
	"beanpon_messenger/models"
	"beanpon_messenger/services"
	"beanpon_messenger/utils"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// moderationEnqueuer — şübhəli mesaj analizi üçün queue-ya iş qoymaq üçün
// minimal interfeys. services.ModerationQueue bunu ödəyir. Qeyri-bloklayıcıdır.
type moderationEnqueuer interface {
	Enqueue(job services.ModerationJob)
}

type MessageHandler struct {
	encryptionService interface {
		EncryptMessage(plainText string) (string, error)
		DecryptMessage(encryptedText string) (string, error)
	}
	wsHub interface {
		HandleNewMessage(senderID, receiverID uint, messageID, content, msgType string, createdAt time.Time, replyToMessageID *string, storyID *uint, conversationStatus string, silent bool) // conversationStatus + silent eklendi
		HandleMessageRead(messageID string, senderID, readerID uint)
		IsUserOnline(userID uint) bool
		SendToUser(userID uint, messageType string, data interface{})
	}
	// moderationQueue — opsional. nil olduqda moderasiya sakitcə atlanır.
	moderationQueue moderationEnqueuer
}

func NewMessageHandler(encryptionService interface {
	EncryptMessage(plainText string) (string, error)
	DecryptMessage(encryptedText string) (string, error)
}, wsHub interface {
	HandleNewMessage(senderID, receiverID uint, messageID, content, msgType string, createdAt time.Time, replyToMessageID *string, storyID *uint, conversationStatus string, silent bool) // conversationStatus + silent eklendi
	HandleMessageRead(messageID string, senderID, readerID uint)
	IsUserOnline(userID uint) bool
	SendToUser(userID uint, messageType string, data interface{})
}) *MessageHandler {
	return &MessageHandler{
		encryptionService: encryptionService,
		wsHub:             wsHub,
	}
}

// SetModerationQueue — moderasiya queue-sunu handler-a bağlayır.
// main.go-da wsHub və queue qurulduqdan sonra çağırılır.
func (h *MessageHandler) SetModerationQueue(q moderationEnqueuer) {
	h.moderationQueue = q
}

type wsHubForConversation interface {
	IsUserOnline(userID uint) bool
	SendToUser(userID uint, messageType string, data interface{})
	BroadcastScreenshotProtectionChange(user1ID, user2ID uint, isDisabled bool, changedByUserID uint)
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
		StoryID          *uint   `json:"story_id,omitempty"` // BU SATIRI EKLE
		ReplyToMessageID *string `json:"reply_to_message_id,omitempty"`
		// Səssiz göndərmə: true olduqda qarşı tərəfə push notification GETMİR
		// (mesaj normal çatır, WS yayılır). Opsional — köhnə client-lər
		// göndərməsə false olur (adi davranış).
		Silent bool `json:"silent,omitempty"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Kendi kendine mesaj göndermesini engelle
	//if senderID.(uint) == req.ReceiverID {
	//	c.JSON(http.StatusBadRequest, gin.H{"error": "Kendi kendinize mesaj gönderemezsiniz"})
	//	return
	//}

	// Block kontrolü ekle
	if models.IsBlocked(database.DB, senderID.(uint), req.ReceiverID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Bu kullanıcıya mesaj gönderemezsiniz"})
		return
	}

	// 🚫 SPAM SHADOW-BAN — GLOBAL (yeni VƏ mövcud conversation üçün).
	//
	// Yalnız `actions` sütununa baxılır. Qaydalar:
	//   • actions = NULL                          → mesaj BLOKLANIR
	//   • actions massivində "message" var        → mesaj BLOKLANIR
	//   • actions = ["post"], ["story"], ["post","story"] və s. (message yox)
	//                                              → mesaj GEDƏ BİLƏR
	//   • spam_bans-da aktiv qeyd yoxdursa        → mesaj GEDƏ BİLƏR
	//
	// Yəni admin "post" və ya "story" üçün ban verə bilər, bu mesajlaşmaya
	// təsir etmir. Yalnız `actions`-da açıq şəkildə "message" varsa və ya
	// `actions` heç təyin olunmayıbsa (NULL — "hamısı") mesajlaşma dayanır.
	//
	// Davranış: shadow-ban
	//   • Göndərənə 201 sahte response qaytarılır (uydurma UUID ilə)
	//   • DB-yə YAZILMIR
	//   • WebSocket ilə qarşı tərəfə YAYILMIR
	//   • Push notification GETMİR
	//   • Moderasiya queue-ya QOYULMUR
	//   • Conversation yaradılmır / yenilənmir
	// 🟢 İSTİSNA: receiver_id == 1 olan istifadəçiyə HƏMİŞƏ mesaj gedə bilər.
	// Spam/shadow-ban olsa belə bu istifadəçiyə yazmağa icazə verilir.
	if req.ReceiverID != 1 && models.IsMessagingBannedByActions(database.DB, senderID.(uint)) {
		log.Printf("🚫 SPAM SHADOW-BAN: sender_id=%d → receiver_id=%d mesajı bloklandı (DB yazılmadı, WS yayılmadı, push yox)",
			senderID.(uint), req.ReceiverID)
		c.JSON(http.StatusCreated, gin.H{
			"message": "Mesaj başarıyla gönderildi",
			"data": gin.H{
				"id":          uuid.New().String(),
				"sender_id":   senderID.(uint),
				"receiver_id": req.ReceiverID,
				"text":        req.Text,
				"read":        false,
				"created_at":  time.Now().UTC(),
				"is_online":   h.wsHub.IsUserOnline(req.ReceiverID),
			},
		})
		return
	}

	wsHubForConv := h.wsHub.(wsHubForConversation)

	// Conversation kontrolü - mesaj gönderebilir mi?
	// 🟢 İSTİSNA: receiver_id == 1 olduqda heç bir göndərmə yoxlaması (spam,
	// verified, follow, limit, restricted) tətbiq olunmur — mesaj həmişə gedir.
	conversationHandler := NewConversationHandler(wsHubForConv, h.encryptionService)
	canSend, reason, err := true, "", error(nil)
	if req.ReceiverID != 1 {
		canSend, reason, err = conversationHandler.CanSendMessage(senderID.(uint), req.ReceiverID)
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Conversation kontrolü başarısız"})
		return
	}

	if !canSend {
		// 🚫 SPAM: spam'lı kullanıcı conversation başlatamaz/mesaj gönderemez.
		// Sessizce başarısız ol — kullanıcıya 403/hata gösterme, ama mesaj da
		// kaydetme. Gönderene başarılıymış gibi 201 dön (shadow-ban davranışı).
		// CanSendMessage spam durumunda reason="" döner; diğer red sebepleri
		// (verified, follow, limit, restricted) dolu bir reason ile gelir.
		if reason == "" {
			c.JSON(http.StatusCreated, gin.H{
				"message": "Mesaj başarıyla gönderildi",
				"data": gin.H{
					"id":          uuid.New().String(),
					"sender_id":   senderID.(uint),
					"receiver_id": req.ReceiverID,
					"text":        req.Text,
					"read":        false,
					"created_at":  time.Now().UTC(),
					"is_online":   h.wsHub.IsUserOnline(req.ReceiverID),
				},
			})
			return
		}

		c.JSON(http.StatusForbidden, gin.H{"error": reason})
		return
	}

	// Mesajı şifrele
	encryptedText, err := h.encryptionService.EncryptMessage(req.Text)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Mesaj şifrelenirken hata oluştu"})
		return
	}

	if req.StoryID != nil {
		var story models.Story
		err := database.DB.Where("id = ?", *req.StoryID).First(&story).Error
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Story bulunamadı"})
			return
		}

		if story.UserID != req.ReceiverID {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Story ve alıcı uyuşmuyor"})
			return
		}

		if story.UserID == senderID.(uint) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Kendi story'nize mesaj gönderemezsiniz"})
			return
		}
	}

	// Veritabanına kaydet
	message := models.Message{
		ID:               uuid.New().String(),
		SenderID:         senderID.(uint),
		ReceiverID:       &req.ReceiverID,
		EncryptedText:    encryptedText,
		ReplyToMessageID: req.ReplyToMessageID,
		StoryID:          req.StoryID,
		Read:             false,
		CreatedAt:        time.Now().UTC(),
		UpdatedAt:        time.Now().UTC(),
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
		*message.ReceiverID,
		message.ID,
		req.Text,
		req.Type,
		message.CreatedAt,
		req.ReplyToMessageID, // YENİ parametre
		req.StoryID,
		"active",
		req.Silent, // səssiz göndərmə → push getməsin
	)

	// 🔍 MODERASIYA — mesaj şifrələnib göndərildi, indi arxa planda analizə
	// qoyuruq. Enqueue() qeyri-bloklayıcıdır: bu sətir mesaj göndərmə
	// sürətinə HEÇ təsir etmir. Yalnız text tipli mesajları analiz edirik
	// (image/video/voice mətn daşımır).
	if h.moderationQueue != nil && (req.Type == "" || req.Type == "text") {
		h.moderationQueue.Enqueue(services.ModerationJob{
			MessageID:  message.ID,
			SenderID:   message.SenderID,
			ReceiverID: req.ReceiverID,
			PlainText:  req.Text,
			CreatedAt:  message.CreatedAt,
		})
	}

	// API response
	c.JSON(http.StatusCreated, gin.H{
		"message": "Mesaj başarıyla gönderildi",
		"data": gin.H{
			"id":                  message.ID,
			"sender_id":           message.SenderID,
			"receiver_id":         message.ReceiverID,
			"reply_to_message_id": message.ReplyToMessageID,
			"text":                req.Text,
			"read":                message.Read,
			"created_at":          message.CreatedAt,
			"is_online":           h.wsHub.IsUserOnline(req.ReceiverID),
		},
	})
}

// BroadcastMessage — eyni mətni bir neçə (maks 20) istifadəçiyə TOPLU göndərir.
// Hər alıcı üçün ayrıca mesaj yaradılır (SendMessage ilə eyni addımlar: icazə
// yoxlaması, şifrələmə, conversation update, WS yayımı, push, moderasiya).
// Bir alıcı uğursuz olsa (məs. spam/icazə yox) digərləri davam edir.
func (h *MessageHandler) BroadcastMessage(c *gin.Context) {
	senderIDVal, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	senderID := senderIDVal.(uint)

	var req struct {
		ReceiverIDs []uint `json:"receiver_ids" binding:"required"`
		Text        string `json:"text" binding:"required"`
		Type        string `json:"type,omitempty"`
		Silent      bool   `json:"silent,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Dedup + özünü çıxar + maks 20 limiti.
	seen := map[uint]bool{}
	var targets []uint
	for _, id := range req.ReceiverIDs {
		if id == 0 || id == senderID || seen[id] {
			continue
		}
		seen[id] = true
		targets = append(targets, id)
		if len(targets) >= 20 {
			break
		}
	}
	if len(targets) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçerli alıcı yok"})
		return
	}

	wsHubForConv := h.wsHub.(wsHubForConversation)
	conversationHandler := NewConversationHandler(wsHubForConv, h.encryptionService)

	var sentTo []uint
	for _, receiverID := range targets {
		if models.IsBlocked(database.DB, senderID, receiverID) {
			continue
		}
		if receiverID != 1 && models.IsMessagingBannedByActions(database.DB, senderID) {
			continue // shadow-ban: səssizcə atla
		}
		if receiverID != 1 {
			canSend, _, cErr := conversationHandler.CanSendMessage(senderID, receiverID)
			if cErr != nil || !canSend {
				continue
			}
		}

		encryptedText, encErr := h.encryptionService.EncryptMessage(req.Text)
		if encErr != nil {
			continue
		}

		rid := receiverID
		message := models.Message{
			ID:            uuid.New().String(),
			SenderID:      senderID,
			ReceiverID:    &rid,
			EncryptedText: encryptedText,
			Read:          false,
			CreatedAt:     time.Now().UTC(),
			UpdatedAt:     time.Now().UTC(),
		}
		if err := database.DB.Create(&message).Error; err != nil {
			continue
		}

		if err := conversationHandler.UpdateConversationOnMessage(senderID, receiverID); err != nil {
			log.Printf("Broadcast conversation güncellemesi başarısız (rcv=%d): %v", receiverID, err)
		}

		h.wsHub.HandleNewMessage(
			message.SenderID,
			*message.ReceiverID,
			message.ID,
			req.Text,
			req.Type,
			message.CreatedAt,
			nil,
			nil,
			"active",
			req.Silent,
		)

		if h.moderationQueue != nil && (req.Type == "" || req.Type == "text") {
			h.moderationQueue.Enqueue(services.ModerationJob{
				MessageID:  message.ID,
				SenderID:   message.SenderID,
				ReceiverID: receiverID,
				PlainText:  req.Text,
				CreatedAt:  message.CreatedAt,
			})
		}

		sentTo = append(sentTo, receiverID)
	}

	c.JSON(http.StatusCreated, gin.H{
		"message": "Toplu mesaj gönderildi",
		"sent_to": sentTo,
		"count":   len(sentTo),
	})
}

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

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	offset := (page - 1) * limit

	var messages []struct {
		ID                   string     `gorm:"column:id"`
		SenderID             uint       `gorm:"column:sender_id"`
		ReceiverID           uint       `gorm:"column:receiver_id"`
		StoryID              *uint      `gorm:"column:story_id"`
		StoryMetadata        *string    `gorm:"column:story_metadata"`
		ReplyToMessageID     *string    `gorm:"column:reply_to_message_id"`
		EncryptedText        string     `gorm:"column:encrypted_text"`
		Read                 bool       `gorm:"column:read"`
		IsEdited             bool       `gorm:"column:is_edited"` // ← YENİ
		SenderReaction       *string    `gorm:"column:sender_reaction"`
		ReceiverReaction     *string    `gorm:"column:receiver_reaction"`
		StarredBySender      bool       `gorm:"column:starred_by_sender"`
		StarredByReceiver    bool       `gorm:"column:starred_by_receiver"`
		CreatedAt            time.Time  `gorm:"column:created_at"`
		UpdatedAt            time.Time  `gorm:"column:updated_at"`
		ReplyToMessageText   *string    `gorm:"column:reply_to_message_text"`
		ReplyToMessageSender *uint      `gorm:"column:reply_to_message_sender"`
		ReplyToCreatedAt     *time.Time `gorm:"column:reply_to_created_at"`
		StoryType            *string    `gorm:"column:story_type"`
		StoryMediaURL        *string    `gorm:"column:story_media_url"`
		StoryContent         *string    `gorm:"column:story_content"`
		StoryUserID          *uint      `gorm:"column:story_user_id"`
		StoryCreatedAt       *time.Time `gorm:"column:story_created_at"`
	}

	query := `
        SELECT 
            m.*,
            reply.encrypted_text as reply_to_message_text,
            reply.sender_id as reply_to_message_sender,
            reply.created_at as reply_to_created_at,
            s.type as story_type,
            s.media_url as story_media_url,
            s.content as story_content,
            s.media_metadata as story_metadata,
            s.user_id as story_user_id,
            s.created_at as story_created_at
        FROM messages m
        LEFT JOIN messages reply ON m.reply_to_message_id = reply.id
        LEFT JOIN stories s ON m.story_id = s.id
        WHERE ((m.sender_id = ? AND m.receiver_id = ?) OR (m.sender_id = ? AND m.receiver_id = ?))
        AND (
            CASE 
                WHEN m.sender_id = ? THEN m.is_deleted_by_sender = false
                ELSE m.is_deleted_by_receiver = false
            END
        )
        ORDER BY m.created_at DESC 
        LIMIT ? OFFSET ?
    `

	err = database.DB.Raw(query,
		userID, otherUserID, otherUserID, userID,
		userID,
		limit, offset,
	).Scan(&messages).Error

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Mesajlar alınamadı"})
		return
	}

	var responseMessages []gin.H
	for _, msg := range messages {
		decryptedText, err := h.encryptionService.DecryptMessage(msg.EncryptedText)
		if err != nil {
			decryptedText = "Mesaj çözülemedi"
		}

		responseMessage := gin.H{
			"id":                  msg.ID,
			"sender_id":           msg.SenderID,
			"receiver_id":         msg.ReceiverID,
			"story_id":            msg.StoryID,
			"reply_to_message_id": msg.ReplyToMessageID,
			"text":                decryptedText,
			"read":                msg.Read,
			"is_edited":           msg.IsEdited, // ← YENİ
			"sender_reaction":     msg.SenderReaction,
			"receiver_reaction":   msg.ReceiverReaction,
			"is_starred_by_me":    starredByUser(userID.(uint), msg.SenderID, msg.StarredBySender, msg.StarredByReceiver),
			"created_at":          msg.CreatedAt,
			"updated_at":          msg.UpdatedAt,
		}

		if msg.StoryID != nil {
			if msg.StoryType != nil {
				storyResponse := gin.H{
					"id":         *msg.StoryID,
					"type":       *msg.StoryType,
					"media_url":  utils.PrependS3URL(msg.StoryMediaURL),
					"content":    msg.StoryContent,
					"user_id":    *msg.StoryUserID,
					"created_at": msg.StoryCreatedAt,
					"available":  true,
				}

				if *msg.StoryType == "video" && msg.StoryMetadata != nil {
					var metadata map[string]interface{}
					if err := json.Unmarshal([]byte(*msg.StoryMetadata), &metadata); err == nil {
						if thumbnailURL, exists := metadata["thumbnail_url"].(string); exists && thumbnailURL != "" {
							storyResponse["thumbnail_url"] = utils.PrependS3URL(&thumbnailURL)
						}
					}
				}

				responseMessage["story"] = storyResponse
			} else {
				responseMessage["story"] = gin.H{
					"id":        *msg.StoryID,
					"available": false,
					"message":   "Bu story artık mevcut değil",
				}
			}
		}

		if msg.ReplyToMessageID != nil && msg.ReplyToMessageText != nil {
			replyDecryptedText, err := h.encryptionService.DecryptMessage(*msg.ReplyToMessageText)
			if err != nil {
				replyDecryptedText = "Mesaj çözülemedi"
			}

			responseMessage["reply_to_message"] = gin.H{
				"id":         *msg.ReplyToMessageID,
				"sender_id":  msg.ReplyToMessageSender,
				"text":       replyDecryptedText,
				"created_at": msg.ReplyToCreatedAt,
			}
		}

		responseMessages = append(responseMessages, responseMessage)
	}

	go h.markReceivedMessagesAsRead(userID.(uint), uint(otherUserID))

	var totalCount int64
	countQuery := `
        SELECT COUNT(*) 
        FROM messages 
        WHERE ((sender_id = ? AND receiver_id = ?) OR (sender_id = ? AND receiver_id = ?))
        AND (
            CASE 
                WHEN sender_id = ? THEN is_deleted_by_sender = false
                ELSE is_deleted_by_receiver = false
            END
        )
    `

	err = database.DB.Raw(countQuery,
		userID, otherUserID, otherUserID, userID,
		userID,
	).Count(&totalCount).Error

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Toplam sayı alınamadı"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data":      responseMessages,
		"page":      page,
		"limit":     limit,
		"total":     int(totalCount),
		"is_online": h.wsHub.IsUserOnline(uint(otherUserID)),
	})
}

// GetMessages belirli kullanıcı ile mesajları getir
func (h *MessageHandler) GetMessagesOld(c *gin.Context) {
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
	if message.ReceiverID == nil || *message.ReceiverID != userID.(uint) {
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
	message.UpdatedAt = time.Now().UTC()

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

// MarkConversationAsRead — A↔B söhbətində bütün okunmamış mesajları (where
// sender_id=other AND receiver_id=current AND read=false) toplu okundu et.
//
// İstifadə yeri: native (iOS/Android) inline reply (Quick Reply) — kullanıcı
// bildirimi aşağı çekib reply göndərdiyi anda qarşı tərəfin mesajlarını
// avtomatik okundu hesab edirik. Eyni zamanda chat-page açılışında batch
// işarələmə üçün də istifadə oluna bilər. WebSocket-dən aynı işi yapan
// `mark_read` event-i mevcuddur; bu HTTP wrapper-i WS bağlantısı yoxsa
// fallback edir.
func (h *MessageHandler) MarkConversationAsRead(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	readerID := userID.(uint)

	otherStr := c.Param("other_user_id")
	otherUint64, err := strconv.ParseUint(otherStr, 10, 64)
	if err != nil || otherUint64 == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz other_user_id"})
		return
	}
	otherID := uint(otherUint64)

	if otherID == readerID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "other_user_id self ola bilməz"})
		return
	}

	now := time.Now().UTC()
	result := database.DB.Model(&models.Message{}).
		Where("sender_id = ? AND receiver_id = ? AND read = false", otherID, readerID).
		Updates(map[string]interface{}{
			"read":       true,
			"updated_at": now,
		})

	if result.Error != nil {
		log.Printf("MarkConversationAsRead DB error: %v", result.Error)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Veritabanı hatası"})
		return
	}

	updatedCount := result.RowsAffected

	// WebSocket üzərindən qarşı tərəfə (mesajları göndərənə) bildir
	// — UI tick'lərini "görüldü" olaraq dəyişdirmək üçün. Hub-da eyni
	// event format-ı (`message_read`) istifadə olunur.
	if updatedCount > 0 {
		readData := map[string]interface{}{
			"reader_id":     readerID,
			"other_user_id": otherID,
			"read_count":    updatedCount,
		}
		h.wsHub.SendToUser(otherID, "message_read", readData)
	}

	c.JSON(http.StatusOK, gin.H{
		"message":    "Conversation okundu olarak işaretlendi",
		"read_count": updatedCount,
		"read_at":    now,
	})
}

// GetConversations sohbet listesi
func (h *MessageHandler) GetConversations(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	statusFilter := c.DefaultQuery("status", "all")
	// archived filtri: "false" (default) → yalnız arxivlənməmiş; "true" →
	// yalnız arxivlənmiş; "all" → hamısı. Per-user (cari istifadəçiyə görə).
	archivedFilter := c.DefaultQuery("archived", "false")

	var conversations []struct {
		OtherUserID        uint      `json:"other_user_id"`
		LastMessageID      string    `json:"last_message_id"`
		LastMessageText    string    `json:"last_message_text"`
		LastMessageTime    time.Time `json:"last_message_time"`
		IsLastFromMe       bool      `json:"is_last_from_me"`
		LastMessageRead    *bool     `json:"last_message_read"`
		UnreadCount        int       `json:"unread_count"`
		ConversationStatus string    `json:"conversation_status"`

		ConversationID     *uint      `json:"conversation_id"`
		MyMessageCount     *int       `json:"my_message_count"`
		OtherMessageCount  *int       `json:"other_message_count"`
		AmIMuted           *bool      `json:"am_i_muted"`
		AmIArchived        *bool      `json:"am_i_archived"`
		AmIPinned          *bool      `json:"am_i_pinned"`
		PinnedAt           *time.Time `json:"pinned_at"`
		MyNickname         *string    `json:"my_nickname"`
		MyWallpaperID      *uint      `json:"my_wallpaper_id"`
		AmIRestricted      *bool      `json:"am_i_restricted"`
		IsOtherMuted       *bool      `json:"is_other_muted"`
		IsOtherRestricted  *bool      `json:"is_other_restricted"`
		MaxPendingMessages *int       `json:"max_pending_messages"`

		OtherUserName       string  `json:"other_user_name"`
		OtherUserUsername   string  `json:"other_user_username"`
		OtherUserIsVerified bool    `json:"other_user_is_verified"`
		AccountTypeID       int     `json:"account_type_id"`
		ProfileImage        *string `json:"profile_image"`

		AllowVoiceMessages bool `json:"allow_voice_messages"`
		ShowReadReceipts   bool `json:"show_read_receipts"`

		// ✅ YENİ: son reaksiya
		LastReactionEmoji    *string    `json:"last_reaction_emoji"`
		LastReactionAt       *time.Time `json:"last_reaction_at"`
		LastReactionByUserID *uint      `json:"last_reaction_by_user_id"`
	}

	statusWhereClause := ""
	var extraParams []interface{}

	if statusFilter == "pending" {
		statusWhereClause = `
        AND COALESCE(conv.status, 'active') = 'pending'
        AND CASE 
            WHEN conv.user1_id = ? THEN conv.user2_message_count > 0 
            ELSE conv.user1_message_count > 0 
        END
    `
		extraParams = append(extraParams, userID)
	} else if statusFilter == "active" {
		statusWhereClause = `
        AND (
            COALESCE(conv.status, 'active') = 'active'
            OR (
                COALESCE(conv.status, 'active') = 'pending'
                AND CASE 
                    WHEN conv.user1_id = ? THEN conv.user1_message_count > 0
                    ELSE conv.user2_message_count > 0
                END
            )
        )
    `
		extraParams = append(extraParams, userID)
	} else if statusFilter == "restricted" {
		statusWhereClause = "AND COALESCE(conv.status, 'active') = 'restricted'"
	}

	// Arxiv filtri (per-user). Default: arxivlənmişləri GİZLƏT.
	//   archived=false → yalnız arxivlənməmiş (əsas siyahı)
	//   archived=true  → yalnız arxivlənmiş (arxiv səhifəsi)
	//   archived=all   → filtr yox
	// QEYD: Bu WHERE statusWhereClause-dan SONRA query-yə əlavə olunur, ona görə
	// parametrləri də extraParams-a status parametrindən SONRA əlavə edirik.
	archivedWhereClause := ""
	if archivedFilter == "true" {
		archivedWhereClause = `
        AND CASE
            WHEN conv.user1_id = ? THEN conv.user1_archived
            WHEN conv.user2_id = ? THEN conv.user2_archived
            ELSE FALSE
        END = TRUE
    `
		extraParams = append(extraParams, userID, userID)
	} else if archivedFilter != "all" {
		// default "false": arxivlənmişləri çıxar
		archivedWhereClause = `
        AND COALESCE(CASE
            WHEN conv.user1_id = ? THEN conv.user1_archived
            WHEN conv.user2_id = ? THEN conv.user2_archived
            ELSE FALSE
        END, FALSE) = FALSE
    `
		extraParams = append(extraParams, userID, userID)
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
            read,
            ROW_NUMBER() OVER (
                PARTITION BY CASE WHEN sender_id = ? THEN receiver_id ELSE sender_id END 
                ORDER BY created_at DESC
            ) as rn
        FROM messages
        WHERE (sender_id = ? OR receiver_id = ?)
        AND conversation_id IS NULL
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
        AND conversation_id IS NULL
        GROUP BY sender_id
    )
    SELECT 
        lm.other_user_id,
        lm.id as last_message_id,
        lm.encrypted_text as last_message_text,
        lm.created_at as last_message_time,
        lm.is_from_me,
        CASE 
            WHEN lm.is_from_me = true THEN lm.read
            ELSE NULL
        END as last_message_read,
        COALESCE(uc.unread_count, 0) as unread_count,
        COALESCE(conv.status, 'active') as conversation_status,
        conv.id as conversation_id,
        CASE 
            WHEN conv.user1_id = ? THEN conv.user1_message_count
            WHEN conv.user2_id = ? THEN conv.user2_message_count
            ELSE NULL
        END as my_message_count,
        CASE 
            WHEN conv.user1_id = ? THEN conv.user2_message_count
            WHEN conv.user2_id = ? THEN conv.user1_message_count
            ELSE NULL
        END as other_message_count,
        CASE
            WHEN conv.user1_id = ? THEN conv.user1_muted
            WHEN conv.user2_id = ? THEN conv.user2_muted
            ELSE NULL
        END as am_i_muted,
        CASE
            WHEN conv.user1_id = ? THEN conv.user1_archived
            WHEN conv.user2_id = ? THEN conv.user2_archived
            ELSE FALSE
        END as am_i_archived,
        CASE
            WHEN conv.user1_id = ? THEN (conv.user1_pinned_at IS NOT NULL)
            WHEN conv.user2_id = ? THEN (conv.user2_pinned_at IS NOT NULL)
            ELSE FALSE
        END as am_i_pinned,
        CASE
            WHEN conv.user1_id = ? THEN conv.user1_pinned_at
            WHEN conv.user2_id = ? THEN conv.user2_pinned_at
            ELSE NULL
        END as pinned_at,
        CASE
            WHEN conv.user1_id = ? THEN conv.user1_nickname
            WHEN conv.user2_id = ? THEN conv.user2_nickname
            ELSE NULL
        END as my_nickname,
        CASE
            WHEN conv.user1_id = ? THEN conv.user1_wallpaper_id
            WHEN conv.user2_id = ? THEN conv.user2_wallpaper_id
            ELSE NULL
        END as my_wallpaper_id,
        CASE
            WHEN conv.user1_id = ? THEN conv.user1_restricted
            WHEN conv.user2_id = ? THEN conv.user2_restricted
            ELSE NULL
        END as am_i_restricted,
        CASE 
            WHEN conv.user1_id = ? THEN conv.user2_muted
            WHEN conv.user2_id = ? THEN conv.user1_muted
            ELSE NULL
        END as is_other_muted,
        CASE 
            WHEN conv.user1_id = ? THEN conv.user2_restricted
            WHEN conv.user2_id = ? THEN conv.user1_restricted
            ELSE NULL
        END as is_other_restricted,
        conv.max_pending_messages,
        u.name as other_user_name,
        u.username as other_user_username,
        u.account_type_id,
        u.is_verified as other_user_is_verified,
        p.profile_image,
        COALESCE(us.allow_voice_messages, true) as allow_voice_messages,
        COALESCE(us.show_read_receipts, true) as show_read_receipts,
        conv.last_reaction_emoji,
        conv.last_reaction_at,
        conv.last_reaction_by_user_id
    FROM latest_messages lm
    LEFT JOIN unread_counts uc ON lm.other_user_id = uc.other_user_id
    LEFT JOIN users u ON u.id = lm.other_user_id
    LEFT JOIN profiles p ON p.user_id = lm.other_user_id
    LEFT JOIN user_settings us ON us.user_id = lm.other_user_id
    LEFT JOIN conversations conv ON (
        (conv.user1_id = LEAST(?, lm.other_user_id) AND conv.user2_id = GREATEST(?, lm.other_user_id))
    )
    WHERE lm.rn = 1 ` + statusWhereClause + archivedWhereClause + `
    ORDER BY
        CASE
            WHEN conv.user1_id = ? THEN (conv.user1_pinned_at IS NOT NULL)
            WHEN conv.user2_id = ? THEN (conv.user2_pinned_at IS NOT NULL)
            ELSE FALSE
        END DESC,
        CASE
            WHEN conv.user1_id = ? THEN conv.user1_pinned_at
            WHEN conv.user2_id = ? THEN conv.user2_pinned_at
            ELSE NULL
        END DESC NULLS LAST,
        lm.created_at DESC
    `

	// Parametr sırası query-dəki ? ardıcıllığı ilə DƏQİQ uyğundur:
	//  CTE: other_user_id CASE, is_from_me, PARTITION, WHERE sender, WHERE recv,
	//       is_deleted CASE  → 6
	//  unread_counts WHERE receiver  → 1 (cəmi yuxarıda 5+1 kimi yazılıb)
	//  SELECT CASE-lər: my_count(2), other_count(2), am_i_muted(2),
	//       am_i_archived(2), am_i_pinned(2) ← YENİ, pinned_at(2) ← YENİ,
	//       am_i_restricted(2), is_other_muted(2), is_other_restricted(2) = 18
	//  SELECT CASE-lər: my_count(2), other_count(2), am_i_muted(2),
	//       am_i_archived(2), am_i_pinned(2), pinned_at(2), my_nickname(2),
	//       my_wallpaper_id(2) ← YENİ, am_i_restricted(2), is_other_muted(2),
	//       is_other_restricted(2) = 22
	//  CTE(6)+unread(1)+SELECT(22)+JOIN(2) = 31 static
	//  (sonra: extraParams = status+archived WHERE)
	//  ORDER BY: pin CASE(2) + pinned_at CASE(2) = 4
	params := []interface{}{
		userID, userID, userID, userID, userID,
		userID,
		userID,
		userID, userID, userID, userID, userID, userID, userID, userID, userID, userID, userID, userID, userID, userID, userID, userID, userID, userID, userID, userID, userID, userID,
		userID, userID,
	}
	params = append(params, extraParams...)
	// ORDER BY parametrləri (WHERE/extraParams-dan SONRA query mətnində gəlir).
	params = append(params, userID, userID, userID, userID)

	err := database.DB.Raw(query, params...).Scan(&conversations).Error
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Konuşmalar alınamadı"})
		return
	}

	var responseConversations []gin.H
	for _, conv := range conversations {
		decryptedText, err := h.encryptionService.DecryptMessage(conv.LastMessageText)
		if err != nil {
			decryptedText = "Mesaj çözülemedi"
		}

		canSendMessage := true
		conversationActive := true
		conversationType := "normal"

		if conv.ConversationStatus != "" && conv.ConversationStatus != "active" {
			conversationActive = false

			switch conv.ConversationStatus {
			case "pending":
				conversationType = "pending"
				if conv.MyMessageCount != nil && conv.MaxPendingMessages != nil {
					if *conv.MyMessageCount >= *conv.MaxPendingMessages {
						canSendMessage = false
					}
				}
			case "restricted":
				conversationType = "restricted"
				canSendMessage = false
			}
		}

		if conv.AmIRestricted != nil && *conv.AmIRestricted {
			canSendMessage = false
		}

		isMutedByMe := conv.AmIMuted != nil && *conv.AmIMuted
		isArchivedByMe := conv.AmIArchived != nil && *conv.AmIArchived
		// Pin = pinned_at dolu (NULL deyil). AmIPinned (*bool) bəzi hallarda
		// GORM Raw scan-da nil qaldığı üçün birbaşa PinnedAt-dan hesablayırıq —
		// PinnedAt həmişə düzgün scan olunur (dolu/NULL).
		isPinnedByMe := conv.PinnedAt != nil

		// Əgər tərəflərdən biri digərini bloklayıbsa, online statusu göstərilməməlidir.
		isOnline := false
		if !models.IsBlocked(database.DB, userID.(uint), conv.OtherUserID) {
			isOnline = h.wsHub.IsUserOnline(conv.OtherUserID)
		}

		responseData := gin.H{
			"other_user_id":            conv.OtherUserID,
			"other_user_name":          conv.OtherUserName,
			"other_user_username":      conv.OtherUserUsername,
			"other_user_is_verified":   conv.OtherUserIsVerified,
			"account_type_id":          conv.AccountTypeID,
			"last_reaction_emoji":      conv.LastReactionEmoji,
			"last_reaction_at":         conv.LastReactionAt,
			"last_reaction_by_user_id": conv.LastReactionByUserID,
			"profile_image":            utils.PrependBaseURL(conv.ProfileImage),
			"last_message_id":          conv.LastMessageID,
			"last_message_text":        decryptedText,
			"last_message_time":        conv.LastMessageTime,
			"is_last_from_me":          conv.IsLastFromMe,
			"last_message_read":        conv.LastMessageRead,
			"unread_count":             conv.UnreadCount,
			"is_online":                isOnline,
			"conversation_active":      conversationActive,
			"is_archived_by_me":        isArchivedByMe,
			"is_pinned_by_me":          isPinnedByMe,
			"pinned_at":                conv.PinnedAt,
			"my_nickname":              conv.MyNickname,
			"my_wallpaper_id":          conv.MyWallpaperID,
			"conversation": gin.H{
				"id":                   conv.ConversationID,
				"status":               conv.ConversationStatus,
				"type":                 conversationType,
				"can_send_message":     canSendMessage,
				"is_muted_by_me":       isMutedByMe,
				"is_archived_by_me":    isArchivedByMe,
				"is_pinned_by_me":      isPinnedByMe,
				"my_nickname":          conv.MyNickname,
				"my_wallpaper_id":      conv.MyWallpaperID,
				"am_i_restricted":      conv.AmIRestricted,
				"is_other_muted":       conv.IsOtherMuted,
				"is_other_restricted":  conv.IsOtherRestricted,
				"my_message_count":     conv.MyMessageCount,
				"other_message_count":  conv.OtherMessageCount,
				"max_pending_messages": conv.MaxPendingMessages,
				"allow_voice_messages": conv.AllowVoiceMessages,
				"show_read_receipts":   conv.ShowReadReceipts,
			},
		}

		responseConversations = append(responseConversations, responseData)
	}

	c.JSON(http.StatusOK, gin.H{
		"conversations": responseConversations,
		"total":         len(responseConversations),
		"status_filter": statusFilter,
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
	err := database.DB.Model(&models.Message{}).
		Joins("JOIN users ON users.id = messages.sender_id").
		Where("messages.receiver_id = ? AND messages.read = false AND messages.is_deleted_by_receiver = false AND users.deleted_at IS NULL", userID).
		Count(&count).Error

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

	now := time.Now().UTC()

	// Silmə növünə görə işləmə
	switch body.DeleteType {
	case "me":
		if userID == message.SenderID {
			message.IsDeletedBySender = true
		} else if message.ReceiverID != nil && userID == *message.ReceiverID {
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
	if message.ReceiverID != nil {
		h.wsHub.SendToUser(*message.ReceiverID, "message_deleted", deletePayload)
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "Mesaj silindi",
		"data":    deletePayload,
	})
}

// ToggleStar — mesajı ulduzla/ulduzdan çıxar (per-user, toggle).
// POST /api/v1/messages/:message_id/star
func (h *MessageHandler) ToggleStar(c *gin.Context) {
	userIDVal, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	userID := userIDVal.(uint)

	messageID := c.Param("message_id")

	var message models.Message
	if err := database.DB.Where("id = ?", messageID).First(&message).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Mesaj tapılmadı"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Veritabanı xətası"})
		}
		return
	}

	// Yalnız söhbətin tərəfi ulduzlaya bilər.
	isSender := userID == message.SenderID
	isReceiver := message.ReceiverID != nil && userID == *message.ReceiverID
	if !isSender && !isReceiver {
		c.JSON(http.StatusForbidden, gin.H{"error": "İcazən yoxdur"})
		return
	}

	// Toggle per-user.
	var newVal bool
	if isSender {
		message.StarredBySender = !message.StarredBySender
		newVal = message.StarredBySender
	} else {
		message.StarredByReceiver = !message.StarredByReceiver
		newVal = message.StarredByReceiver
	}
	message.UpdatedAt = time.Now().UTC()

	if err := database.DB.Save(&message).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Ulduzlama uğursuz oldu"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message":    "OK",
		"message_id": message.ID,
		"is_starred": newVal,
	})
}

// GetStarredMessages — bu söhbətdə MƏNİM ulduzladığım mesajlar.
// GET /api/v1/conversations/:other_user_id/starred
//
// KRİTİK fallback: mesaj göstərilir yalnız əgər (1) mən ulduzlamışam,
// (2) mən silməmişəm, VƏ (3) GÖNDƏRƏN onu silməyib (mesajın sahibi silsə,
// ulduzlayan da görməməlidir). Şəkil/sender info da qaytarılır.
func (h *MessageHandler) GetStarredMessages(c *gin.Context) {
	userIDVal, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	userID := userIDVal.(uint)

	otherUserID, err := strconv.ParseUint(c.Param("other_user_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz kullanıcı ID"})
		return
	}

	var rows []struct {
		ID                 string    `gorm:"column:id"`
		SenderID           uint      `gorm:"column:sender_id"`
		ReceiverID         uint      `gorm:"column:receiver_id"`
		EncryptedText      string    `gorm:"column:encrypted_text"`
		IsEdited           bool      `gorm:"column:is_edited"`
		CreatedAt          time.Time `gorm:"column:created_at"`
		SenderName         string    `gorm:"column:sender_name"`
		SenderUsername     string    `gorm:"column:sender_username"`
		SenderProfileImage *string   `gorm:"column:sender_profile_image"`
	}

	// Per-user ulduz + per-user silmə + GÖNDƏRƏN silməyib.
	query := `
		SELECT
			m.id, m.sender_id, m.receiver_id, m.encrypted_text, m.is_edited, m.created_at,
			u.name as sender_name, u.username as sender_username, p.profile_image as sender_profile_image
		FROM messages m
		LEFT JOIN users u ON u.id = m.sender_id
		LEFT JOIN profiles p ON p.user_id = m.sender_id
		WHERE ((m.sender_id = ? AND m.receiver_id = ?) OR (m.sender_id = ? AND m.receiver_id = ?))
		  AND (
		    CASE WHEN m.sender_id = ? THEN m.starred_by_sender ELSE m.starred_by_receiver END
		  ) = TRUE
		  AND (
		    CASE WHEN m.sender_id = ? THEN m.is_deleted_by_sender ELSE m.is_deleted_by_receiver END
		  ) = FALSE
		  AND m.is_deleted_by_sender = FALSE
		  AND m.deleted_at IS NULL
		ORDER BY m.created_at DESC
	`

	if err := database.DB.Raw(query,
		userID, otherUserID, otherUserID, userID,
		userID, // ulduz CASE
		userID, // mənim silmə CASE
	).Scan(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Ulduzlu mesajlar alınamadı"})
		return
	}

	var result []gin.H
	for _, r := range rows {
		decrypted, derr := h.encryptionService.DecryptMessage(r.EncryptedText)
		if derr != nil {
			decrypted = "Mesaj çözülemedi"
		}
		result = append(result, gin.H{
			"id":                   r.ID,
			"sender_id":            r.SenderID,
			"receiver_id":          r.ReceiverID,
			"text":                 decrypted,
			"is_edited":            r.IsEdited,
			"is_starred_by_me":     true,
			"created_at":           r.CreatedAt,
			"sender_name":          r.SenderName,
			"sender_username":      r.SenderUsername,
			"sender_profile_image": utils.PrependBaseURL(r.SenderProfileImage),
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"messages": result,
		"total":    len(result),
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

	now := time.Now().UTC()
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

	now := time.Now().UTC()
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

func (h *MessageHandler) EditMessage(c *gin.Context) {
	userIDVal, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	userID := userIDVal.(uint)

	messageID := c.Param("message_id")

	var body struct {
		Text string `json:"text" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var message models.Message
	if err := database.DB.Where("id = ?", messageID).First(&message).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Mesaj tapılmadı"})
		return
	}

	// Yalnız göndərən edit edə bilər
	if message.SenderID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "Yalnız öz mesajını edit edə bilərsən"})
		return
	}

	// Yalnız text tipli mesajlar edit edilə bilər
	//if message.Type != nil && *message.Type != "text" && *message.Type != "" {
	//	c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "Yalnız text mesajlar edit edilə bilər"})
	//	return
	//}

	encryptedText, err := h.encryptionService.EncryptMessage(body.Text)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Şifrələmə xətası"})
		return
	}

	now := time.Now().UTC()
	if err := database.DB.Model(&message).Updates(map[string]interface{}{
		"encrypted_text": encryptedText,
		"is_edited":      true,
		"updated_at":     now,
	}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Mesaj yenilənə bilmədi"})
		return
	}

	// WebSocket ilə hər iki tərəfə bildir
	editPayload := map[string]interface{}{
		"message_id": messageID,
		"text":       body.Text,
		"is_edited":  true,
		"edited_at":  now,
	}

	h.wsHub.SendToUser(message.SenderID, "message_edited", editPayload)
	if message.ReceiverID != nil {
		h.wsHub.SendToUser(*message.ReceiverID, "message_edited", editPayload)
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "Mesaj yeniləndi",
		"data":    editPayload,
	})
}

// GetShareRecipients — share modal-da göstəriləcək tövsiyə olunan istifadəçilər.
// Wave/post share zamanı ilkin auditoriya `follow-list`-dən deyil, **chat
// keçmişindən** alınır: ən son danışdığım VƏ ən çox danışdığım dostlar
// üstə çıxsın. Boş chat tarixi olan user üçün caller (Flutter tərəf)
// follow-list-ə düşür.
//
// Score: w_recency * recency_score + w_freq * freq_score
//
//	recency_score = 1 / (1 + days_since_last_message)
//	freq_score    = LN(my_count + other_count + 1) / 5.0  (cap ~ ln(150)/5 = 1.0)
//
// Default w_recency=0.6, w_freq=0.4 — son danışıq əhəmiyyətli, amma ümumi
// münasibət sıxlığı da nəzərə alınır.
//
// Response: [{ user_id, name, username, profile_image, is_verified, score }]
func (h *MessageHandler) GetShareRecipients(c *gin.Context) {
	userIDRaw, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	userID, ok := userIDRaw.(uint)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid user"})
		return
	}

	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "10"))
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if offset < 0 {
		offset = 0
	}

	type Row struct {
		UserID       uint    `json:"user_id"`
		Name         string  `json:"name"`
		Username     string  `json:"username"`
		ProfileImage *string `json:"profile_image"`
		IsVerified   bool    `json:"is_verified"`
		Score        float64 `json:"score"`
	}

	// Birbaşa messages cədvəlindən: current user-in göndərdiyi mesajlar
	// (sender_id = current). Receiver-ə görə qruplaşdırılır:
	//   • Primary sort: ən son mesajın tarixi (MAX(created_at) DESC)
	//   • Secondary sort: ümumi mesaj sayı (COUNT(*) DESC)
	// Bu halda son danışılan kişi öndə olur, eyni gündə danışdığı bir
	// neçə nəfər varsa daha çox yazışdığı öndə.
	// `profile_image` users-də deyil, `profiles` cədvəlindədir (eyni
	// pattern GetConversations-dakı kimi). LEFT JOIN istifadə edirik ki,
	// profile satırı olmayan user-lər də ekrandan çıxmasın.
	const stmt = `
SELECT
    m.receiver_id AS user_id,
    u.name,
    u.username,
    p.profile_image,
    u.is_verified,
    EXTRACT(EPOCH FROM MAX(m.created_at)) AS score
FROM messages m
INNER JOIN users u ON u.id = m.receiver_id
LEFT JOIN profiles p ON p.user_id = m.receiver_id
WHERE m.sender_id = ?
  AND m.receiver_id IS NOT NULL
  AND m.deleted_at IS NULL
  AND COALESCE(m.is_deleted_by_sender, false) = false
GROUP BY m.receiver_id, u.name, u.username, p.profile_image, u.is_verified
ORDER BY MAX(m.created_at) DESC, COUNT(*) DESC
LIMIT ? OFFSET ?`

	var rows []Row
	if err := database.GetDB().Raw(stmt, userID, limit, offset).
		Scan(&rows).Error; err != nil {
		log.Printf("GetShareRecipients query error (userID=%d): %v", userID, err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "DB error",
			"detail":  err.Error(),
			"user_id": userID,
		})
		return
	}

	// profile_image relative key kimi gəlir (məs. "profile_images/abc.jpg") —
	// frontend-ə tam URL göndər. GetConversations da məhz `PrependBaseURL`
	// (default StorageLocal → /storage/...) işlədir, S3 storage tipi deyil.
	for i := range rows {
		rows[i].ProfileImage = utils.PrependBaseURL(rows[i].ProfileImage)
	}

	c.JSON(http.StatusOK, gin.H{
		"data": rows,
	})
}

// starredByUser — istifadəçi bu mesajı ulduzlayıbmı (per-user). userID mesajın
// göndəricisidirsə StarredBySender, yoxsa (alıcıdırsa) StarredByReceiver.
func starredByUser(userID, senderID uint, starredBySender, starredByReceiver bool) bool {
	if userID == senderID {
		return starredBySender
	}
	return starredByReceiver
}
