package handlers

import (
	"beanpon_messenger/database"
	"beanpon_messenger/models"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type ConversationHandler struct {
	wsHub interface {
		IsUserOnline(userID uint) bool
		SendToUser(userID uint, messageType string, data interface{})
		BroadcastScreenshotProtectionChange(user1ID, user2ID uint, isDisabled bool, changedByUserID uint) // ✅ YENİ
	}
	encryptionService interface {
		EncryptMessage(plainText string) (string, error)
		DecryptMessage(encryptedText string) (string, error)
	}
}

func NewConversationHandler(wsHub interface {
	IsUserOnline(userID uint) bool
	SendToUser(userID uint, messageType string, data interface{})
	BroadcastScreenshotProtectionChange(user1ID, user2ID uint, isDisabled bool, changedByUserID uint)
}, encryptionService interface {
	EncryptMessage(plainText string) (string, error)
	DecryptMessage(encryptedText string) (string, error)
}) *ConversationHandler {
	return &ConversationHandler{
		wsHub:             wsHub,
		encryptionService: encryptionService,
	}
}

// GetOrCreateConversation iki kullanıcı arasında conversation getir veya oluştur
func (h *ConversationHandler) GetOrCreateConversation(user1ID, user2ID uint) (*models.Conversation, error) {
	// Küçük ID'yi user1, büyük ID'yi user2 yap (tutarlılık için)
	if user1ID > user2ID {
		user1ID, user2ID = user2ID, user1ID
	}

	var conversation models.Conversation

	// Önce mevcut conversation'ı ara
	err := database.DB.Where("user1_id = ? AND user2_id = ?", user1ID, user2ID).First(&conversation).Error

	if errors.Is(err, gorm.ErrRecordNotFound) {
		// Yeni conversation oluştur
		conversation = models.Conversation{
			User1ID:                 user1ID,
			User2ID:                 user2ID,
			Status:                  "pending",
			Type:                    "request_based",
			User1MessageCount:       0,
			User2MessageCount:       0,
			MaxPendingMessages:      3,
			User1FollowsUser2:       false,
			User2FollowsUser1:       false,
			MutualFollow:            false,
			HasPreviousConversation: false,
			User1Muted:              false,
			User2Muted:              false,
			User1Restricted:         false,
			User2Restricted:         false,
			TotalMessagesCount:      0,
		}

		// Follow ilişkilerini kontrol et
		h.updateFollowRelations(&conversation)

		// 🆕 YENİ: Screenshot protection kontrolü
		// User1'in ayarlarını kontrol et
		var user1Settings models.UserSettings
		if err := database.DB.Where("user_id = ?", user1ID).First(&user1Settings).Error; err == nil {
			if user1Settings.ConversationScreenshotDisabled {
				conversation.User1ScreenshotDisabled = true
				now := time.Now()
				conversation.User1ScreenshotDisabledAt = &now
			}
		}

		// User2'nin ayarlarını kontrol et
		var user2Settings models.UserSettings
		if err := database.DB.Where("user_id = ?", user2ID).First(&user2Settings).Error; err == nil {
			if user2Settings.ConversationScreenshotDisabled {
				conversation.User2ScreenshotDisabled = true
				now := time.Now()
				conversation.User2ScreenshotDisabledAt = &now
			}
		}

		if err := database.DB.Create(&conversation).Error; err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}

	return &conversation, nil
}

// updateFollowRelations follow ilişkilerini güncelle
func (h *ConversationHandler) updateFollowRelations(conversation *models.Conversation) {
	// follows tablosunu kontrol et (eğer varsa)
	var count1, count2 int64

	// User1 -> User2 follow kontrolü
	database.DB.Table("follows").Where("follower_id = ? AND following_id = ?",
		conversation.User1ID, conversation.User2ID).Count(&count1)

	// User2 -> User1 follow kontrolü
	database.DB.Table("follows").Where("follower_id = ? AND following_id = ?",
		conversation.User2ID, conversation.User1ID).Count(&count2)

	conversation.User1FollowsUser2 = count1 > 0
	conversation.User2FollowsUser1 = count2 > 0
	conversation.MutualFollow = conversation.User1FollowsUser2 && conversation.User2FollowsUser1

	// Type'ı güncelle
	if conversation.MutualFollow {
		conversation.Type = "follow_based"
	} else {
		conversation.Type = "request_based"
	}
}

// CanSendMessage kullanıcının mesaj gönderip gönderemeyeceğini kontrol et
func (h *ConversationHandler) CanSendMessage(senderID, receiverID uint) (bool, string, error) {
	// Önce block kontrolü
	if models.IsBlocked(database.DB, senderID, receiverID) {
		return false, "Bu istifadəçiyə mesaj göndərə bilməzsiniz (blokladınız)", nil
	}

	// Conversation'ı bul
	var conversation models.Conversation
	err := database.DB.Where(
		"(user1_id = ? AND user2_id = ?) OR (user1_id = ? AND user2_id = ?)",
		senderID, receiverID, receiverID, senderID,
	).First(&conversation).Error

	// Conversation yoksa, yeni conversation oluşturulacak - verified kontrolü yap
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// 🆕 SADECE YENİ CONVERSATION İÇİN VERIFIED KONTROLÜ
			var receiverSettings models.UserSettings
			if err := database.DB.Where("user_id = ?", receiverID).First(&receiverSettings).Error; err == nil {
				// Eğer ONLY_VERIFIED ise, gönderende verified kontrolü yap
				if receiverSettings.MessageRequests == "ONLY_VERIFIED" {
					var sender models.User
					if err := database.DB.Where("id = ?", senderID).First(&sender).Error; err != nil {
						return false, "İstifadəçi tapılmadı", err
					}

					if !sender.IsVerified {
						return false, "Bu istifadəçiyə mesaj göndərmək üçün təsdiqlənmiş hesab tələb olunur", nil
					}
				}
			}
			// Eğer user_settings kaydı yoksa veya ALL ise, izin ver
			return true, "", nil
		}
		return false, "Verilənlər bazası xətası", err
	}

	// 🎯 Conversation VARSA (daha önce mesajlaşmışlarsa), verified kontrolü YOK
	// Sadece conversation durumunu kontrol et

	switch conversation.Status {
	case "active":
		// Active ise her şey tamam
		return true, "", nil

	case "pending":
		// Pending durumda, gönderen kullanıcının mesaj limitini kontrol et
		var senderMessageCount int
		if conversation.User1ID == senderID {
			senderMessageCount = conversation.User1MessageCount
		} else {
			senderMessageCount = conversation.User2MessageCount
		}

		if senderMessageCount >= conversation.MaxPendingMessages {
			return false, "Mesaj limiti doldu. Qarşı tərəf cavab verməlidir", nil
		}

		return true, "", nil

	case "restricted":
		// Restricted durumda kimse mesaj gönderemez
		return false, "Bu söhbət məhdudlaşdırılıb", nil

	default:
		return false, "Naməlum söhbət statusu", nil
	}
}

// UpdateConversationOnMessage mesaj gönderildikten sonra conversation güncelle
func (h *ConversationHandler) UpdateConversationOnMessage(senderID, receiverID uint) error {
	conversation, err := h.GetOrCreateConversation(senderID, receiverID)
	if err != nil {
		return err
	}

	now := time.Now()

	// İlk mesaj mı?
	if conversation.FirstMessageAt == nil {
		conversation.FirstMessageAt = &now
	}

	// Son mesaj zamanını güncelle
	conversation.LastMessageAt = &now

	// Mesaj sayaçlarını artır
	if senderID == conversation.User1ID {
		conversation.User1MessageCount++
	} else {
		conversation.User2MessageCount++
	}

	conversation.TotalMessagesCount++

	// Her iki taraftan da mesaj varsa active yap ve previous conversation işaretle
	if conversation.User1MessageCount > 0 && conversation.User2MessageCount > 0 {
		conversation.Status = "active"
		conversation.HasPreviousConversation = true
		conversation.StatusChangedAt = &now
	}

	// Pending durumda tek taraflı mesaj limitini kontrol et
	if conversation.Status == "pending" {
		maxCount := 0
		if conversation.User1MessageCount > maxCount {
			maxCount = conversation.User1MessageCount
		}
		if conversation.User2MessageCount > maxCount {
			maxCount = conversation.User2MessageCount
		}

		// Sadece bir taraf yazmışsa ve limit aşılmışsa restricted yap
		if (conversation.User1MessageCount == 0 || conversation.User2MessageCount == 0) &&
			maxCount > conversation.MaxPendingMessages {
			conversation.Status = "restricted"
			conversation.StatusChangedAt = &now
			conversation.RestrictionReason = StringPtr("Tek taraflı mesaj limiti aşıldı")
		}
	}

	return database.DB.Save(conversation).Error
}

// updateConversationStatus conversation durumunu güncelle
func (h *ConversationHandler) updateConversationStatus(conversationID uint, status string) error {
	now := time.Now()
	return database.DB.Model(&models.Conversation{}).
		Where("id = ?", conversationID).
		Updates(map[string]interface{}{
			"status":            status,
			"status_changed_at": &now,
		}).Error
}

// MuteConversation konuşmayı sessize al
func (h *ConversationHandler) MuteConversation(c *gin.Context) {
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

	var requestBody struct {
		MuteDuration int `json:"muteDuration"` // dakika cinsinden
	}

	if err := c.ShouldBindJSON(&requestBody); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz request body"})
		return
	}

	conversation, err := h.GetOrCreateConversation(userID.(uint), uint(otherUserID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Conversation bulunamadı"})
		return
	}

	now := time.Now()

	// Hangi kullanıcı mute ediyor?
	if userID.(uint) == conversation.User1ID {
		conversation.User1Muted = true
		conversation.User1MutedAt = &now

		// MuteDuration kontrolü
		if requestBody.MuteDuration > 0 {
			mutedUntil := now.Add(time.Duration(requestBody.MuteDuration) * time.Minute)
			conversation.User1MutedUntil = &mutedUntil
		} else {
			// Always mute (0 geldiyse null olsun)
			conversation.User1MutedUntil = nil
		}
	} else {
		conversation.User2Muted = true
		conversation.User2MutedAt = &now

		// MuteDuration kontrolü
		if requestBody.MuteDuration > 0 {
			mutedUntil := now.Add(time.Duration(requestBody.MuteDuration) * time.Minute)
			conversation.User2MutedUntil = &mutedUntil
		} else {
			// Always mute (0 geldiyse null olsun)
			conversation.User2MutedUntil = nil
		}
	}

	if err := database.DB.Save(conversation).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Mute işlemi başarısız"})
		return
	}

	response := gin.H{
		"message":  "Konuşma sessize alındı",
		"muted_at": now,
	}

	// Eğer süre belirtilmişse response'a ekle
	if requestBody.MuteDuration > 0 {
		mutedUntil := now.Add(time.Duration(requestBody.MuteDuration) * time.Minute)
		response["muted_until"] = mutedUntil
	}

	c.JSON(http.StatusOK, response)
}

// UnmuteConversation konuşma sesini aç
func (h *ConversationHandler) UnmuteConversation(c *gin.Context) {
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

	conversation, err := h.GetOrCreateConversation(userID.(uint), uint(otherUserID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Conversation bulunamadı"})
		return
	}

	// Hangi kullanıcı unmute ediyor?
	if userID.(uint) == conversation.User1ID {
		conversation.User1Muted = false
		conversation.User1MutedAt = nil
		conversation.User1MutedUntil = nil
	} else {
		conversation.User2Muted = false
		conversation.User2MutedAt = nil
		conversation.User2MutedUntil = nil
	}

	if err := database.DB.Save(conversation).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Unmute işlemi başarısız"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "Konuşma sesi açıldı",
	})
}

// GetConversationDetails conversation detaylarını getir
func (h *ConversationHandler) GetConversationDetails(c *gin.Context) {
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

	conversation, err := h.GetOrCreateConversation(userID.(uint), uint(otherUserID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Conversation bulunamadı"})
		return
	}

	// Conversation durumu analizi
	canSendMessage := true
	conversationType := "normal"        // normal, pending, restricted
	var stopMessageReason *string = nil // 🆕 YENİ ALAN

	switch conversation.Status {
	case "pending":
		conversationType = "pending"
		// Pending durumda mesaj limiti kontrol et
		var myMessageCount int
		if userID.(uint) == conversation.User1ID {
			myMessageCount = conversation.User1MessageCount
		} else {
			myMessageCount = conversation.User2MessageCount
		}

		if myMessageCount >= conversation.MaxPendingMessages {
			canSendMessage = false
		}
	case "restricted":
		conversationType = "restricted"
		canSendMessage = false
	case "active":
		conversationType = "normal"
	}

	// Kullanıcıya göre detayları ayarla
	var myMessageCount, otherMessageCount int
	var isMutedByMe, amIRestricted, isOtherMuted, isOtherRestricted bool

	if userID.(uint) == conversation.User1ID {
		myMessageCount = conversation.User1MessageCount
		otherMessageCount = conversation.User2MessageCount
		isMutedByMe = conversation.User1Muted
		amIRestricted = conversation.User1Restricted
		isOtherMuted = conversation.User2Muted
		isOtherRestricted = conversation.User2Restricted
	} else {
		myMessageCount = conversation.User2MessageCount
		otherMessageCount = conversation.User1MessageCount
		isMutedByMe = conversation.User2Muted
		amIRestricted = conversation.User2Restricted
		isOtherMuted = conversation.User1Muted
		isOtherRestricted = conversation.User1Restricted
	}

	// Kişisel kısıtlamalar kontrol et
	if amIRestricted {
		canSendMessage = false
	}

	// 🆕 STOP_MESSAGE_REASON KONTROLÜ
	// Sadece daha önce hiç mesaj atılmamışsa (yeni conversation)
	totalMessages := conversation.User1MessageCount + conversation.User2MessageCount

	if totalMessages == 0 {
		// Karşı tarafın ayarlarını kontrol et
		var otherUserSettings models.UserSettings
		if err := database.DB.Where("user_id = ?", otherUserID).First(&otherUserSettings).Error; err == nil {
			// Ayar varsa kontrolü yap
			if otherUserSettings.MessageRequests == "ONLY_VERIFIED" {
				// Benim verified durumumu kontrol et
				var myUser models.User
				if err := database.DB.Where("id = ?", userID).First(&myUser).Error; err == nil {
					if !myUser.IsVerified {
						// Verified değilim ve karşı taraf ONLY_VERIFIED istiyor
						reason := "ONLY_VERIFIED"
						stopMessageReason = &reason
						canSendMessage = false
					} else {
						// Verified'im, izin var
						reason := "ALL"
						stopMessageReason = &reason
					}
				}
			} else {
				// MessageRequests = "ALL" ise
				reason := "ALL"
				stopMessageReason = &reason
			}
		} else {
			// Ayar yoksa default ALL
			reason := "ALL"
			stopMessageReason = &reason
		}
	} else {
		// Daha önce mesaj varsa, artık kısıtlama yok (eski conversation)
		// stop_message_reason null kalır veya "PREVIOUS_CONVERSATION" diyebiliriz
		reason := "PREVIOUS_CONVERSATION"
		stopMessageReason = &reason
	}

	responseData := gin.H{
		"conversation": gin.H{
			"id":                   conversation.ID,
			"status":               conversation.Status,
			"type":                 conversationType,
			"can_send_message":     canSendMessage,
			"stop_message_reason":  stopMessageReason, // 🆕 YENİ ALAN
			"is_muted_by_me":       isMutedByMe,
			"am_i_restricted":      amIRestricted,
			"is_other_muted":       isOtherMuted,
			"is_other_restricted":  isOtherRestricted,
			"my_message_count":     myMessageCount,
			"other_message_count":  otherMessageCount,
			"max_pending_messages": conversation.MaxPendingMessages,
		},
	}

	c.JSON(http.StatusOK, responseData)
}

// buildConversationResponse kullanıcıya göre response oluştur
func (h *ConversationHandler) buildConversationResponse(conv *models.Conversation, currentUserID uint) models.ConversationResponse {
	var otherUserID uint
	var myMessageCount, otherMessageCount int
	var isMutedByMe, isRestrictedForMe bool
	var myScreenshotDisabled, otherScreenshotDisabled bool // ✅ YENİ

	if currentUserID == conv.User1ID {
		otherUserID = conv.User2ID
		myMessageCount = conv.User1MessageCount
		otherMessageCount = conv.User2MessageCount
		isMutedByMe = conv.User1Muted
		isRestrictedForMe = conv.User1Restricted
		myScreenshotDisabled = conv.User1ScreenshotDisabled    // ✅ YENİ
		otherScreenshotDisabled = conv.User2ScreenshotDisabled // ✅ YENİ
	} else {
		otherUserID = conv.User1ID
		myMessageCount = conv.User2MessageCount
		otherMessageCount = conv.User1MessageCount
		isMutedByMe = conv.User2Muted
		isRestrictedForMe = conv.User2Restricted
		myScreenshotDisabled = conv.User2ScreenshotDisabled    // ✅ YENİ
		otherScreenshotDisabled = conv.User1ScreenshotDisabled // ✅ YENİ
	}

	canSend, _, _ := h.CanSendMessage(currentUserID, otherUserID)

	return models.ConversationResponse{
		ID:                      conv.ID,
		OtherUserID:             otherUserID,
		Status:                  conv.Status,
		Type:                    conv.Type,
		MyMessageCount:          myMessageCount,
		OtherMessageCount:       otherMessageCount,
		IsMutedByMe:             isMutedByMe,
		IsRestrictedForMe:       isRestrictedForMe,
		CanSendMessage:          canSend,
		MaxPendingMessages:      conv.MaxPendingMessages,
		HasPreviousConversation: conv.HasPreviousConversation,
		LastMessageAt:           conv.LastMessageAt,

		// ✅ YENİ: Screenshot bilgileri
		IsScreenshotDisabled:    myScreenshotDisabled || otherScreenshotDisabled,
		MyScreenshotDisabled:    myScreenshotDisabled,
		OtherScreenshotDisabled: otherScreenshotDisabled,

		CreatedAt: conv.CreatedAt,
	}
}

// StringPtr string pointer helper
func StringPtr(s string) *string {
	return &s
}

// GetPendingRequests bekleyen istekleri getir
func (h *ConversationHandler) GetPendingRequests(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	var requests []struct {
		ConversationID    uint      `json:"conversation_id"`
		RequesterID       uint      `json:"requester_id"`
		RequesterName     string    `json:"requester_name"`
		RequesterUsername string    `json:"requester_username"`
		ProfileImage      *string   `json:"profile_image"`
		MessageCount      int       `json:"message_count"`
		LastMessageText   string    `json:"last_message_text"`
		LastMessageTime   time.Time `json:"last_message_time"`
		CreatedAt         time.Time `json:"created_at"`
	}

	query := `
        SELECT 
            c.id as conversation_id,
            CASE 
                WHEN c.user1_id = ? THEN c.user2_id 
                ELSE c.user1_id 
            END as requester_id,
            u.name as requester_name,
            u.username as requester_username,
            p.profile_image,
            CASE 
                WHEN c.user1_id = ? THEN c.user2_message_count 
                ELSE c.user1_message_count 
            END as message_count,
            '' as last_message_text,
            COALESCE(c.last_message_at, c.created_at) as last_message_time,
            c.created_at
        FROM conversations c
        JOIN users u ON u.id = CASE WHEN c.user1_id = ? THEN c.user2_id ELSE c.user1_id END
        LEFT JOIN profiles p ON p.user_id = u.id
        WHERE (c.user1_id = ? OR c.user2_id = ?)
        AND c.status = 'pending'
        AND CASE 
            WHEN c.user1_id = ? THEN c.user2_message_count > 0 
            ELSE c.user1_message_count > 0 
        END
        ORDER BY c.last_message_at DESC
    `

	err := database.DB.Raw(query, userID, userID, userID, userID, userID, userID).Scan(&requests).Error
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "İstekler alınamadı"})
		return
	}

	// Son mesajları al
	for i := range requests {
		var lastMessage struct {
			EncryptedText string `json:"encrypted_text"`
		}

		database.DB.Raw(`
            SELECT encrypted_text 
            FROM messages 
            WHERE sender_id = ? AND receiver_id = ?
            AND is_deleted_by_receiver = false
            ORDER BY created_at DESC 
            LIMIT 1
        `, requests[i].RequesterID, userID).Scan(&lastMessage)

		if lastMessage.EncryptedText != "" {
			if decrypted, err := h.encryptionService.DecryptMessage(lastMessage.EncryptedText); err == nil {
				requests[i].LastMessageText = decrypted
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"requests": requests,
		"count":    len(requests),
	})
}

// GetPendingRequestCount bekleyen istek sayısı
func (h *ConversationHandler) GetPendingRequestCount(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	var count int64

	query := `
        SELECT COUNT(*) 
        FROM conversations c
        WHERE (c.user1_id = ? OR c.user2_id = ?)
        AND c.status = 'pending'
        AND CASE 
            WHEN c.user1_id = ? THEN c.user2_message_count > 0 
            ELSE c.user1_message_count > 0 
        END
    `

	database.DB.Raw(query, userID, userID, userID).Scan(&count)

	c.JSON(http.StatusOK, gin.H{
		"pending_requests_count": count,
	})
}

// AcceptConversationRequest conversation isteğini kabul et
func (h *ConversationHandler) AcceptConversationRequest(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	requesterID, err := strconv.ParseUint(c.Param("requester_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz requester ID"})
		return
	}

	conversation, err := h.GetOrCreateConversation(userID.(uint), uint(requesterID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Conversation bulunamadı"})
		return
	}

	if conversation.Status != "pending" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Bu conversation zaten kabul edilmiş"})
		return
	}

	// Conversation'ı active yap
	now := time.Now()
	conversation.Status = "active"
	conversation.HasPreviousConversation = true
	conversation.StatusChangedAt = &now

	if err := database.DB.Save(conversation).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "İstek kabul edilemedi"})
		return
	}

	// WebSocket bildirimi gönder
	h.wsHub.SendToUser(uint(requesterID), "conversation_accepted", map[string]interface{}{
		"conversation_id": conversation.ID,
		"accepted_by":     userID,
		"accepted_at":     now,
	})

	c.JSON(http.StatusOK, gin.H{
		"message": "Conversation isteği kabul edildi",
		"data": gin.H{
			"conversation_id": conversation.ID,
			"status":          conversation.Status,
			"accepted_at":     now,
		},
	})
}

// RejectConversationRequest conversation isteğini reddet
func (h *ConversationHandler) RejectConversationRequest(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	requesterID, err := strconv.ParseUint(c.Param("requester_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz requester ID"})
		return
	}

	conversation, err := h.GetOrCreateConversation(userID.(uint), uint(requesterID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Conversation bulunamadı"})
		return
	}

	if conversation.Status != "pending" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Bu conversation zaten işlenmiş"})
		return
	}

	// Conversation'ı restricted yap
	now := time.Now()
	conversation.Status = "restricted"
	conversation.StatusChangedAt = &now
	conversation.RestrictionReason = StringPtr("İstek reddedildi")

	if err := database.DB.Save(conversation).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "İstek reddedilemedi"})
		return
	}

	// WebSocket bildirimi gönder
	h.wsHub.SendToUser(uint(requesterID), "conversation_rejected", map[string]interface{}{
		"conversation_id": conversation.ID,
		"rejected_by":     userID,
		"rejected_at":     now,
	})

	c.JSON(http.StatusOK, gin.H{
		"message": "Conversation isteği reddedildi",
		"data": gin.H{
			"conversation_id": conversation.ID,
			"status":          conversation.Status,
			"rejected_at":     now,
		},
	})
}

// ToggleScreenshotProtection - Screenshot korumayı aç/kapat
func (h *ConversationHandler) ToggleScreenshotProtection(c *gin.Context) {
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

	var requestBody struct {
		Enabled bool `json:"enabled"` // true = screenshot kapalı, false = screenshot açık
	}

	if err := c.ShouldBindJSON(&requestBody); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz request body"})
		return
	}

	conversation, err := h.GetOrCreateConversation(userID.(uint), uint(otherUserID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Conversation bulunamadı"})
		return
	}

	now := time.Now()

	// Hangi kullanıcı değiştiriyor?
	if userID.(uint) == conversation.User1ID {
		conversation.User1ScreenshotDisabled = requestBody.Enabled
		if requestBody.Enabled {
			conversation.User1ScreenshotDisabledAt = &now
		} else {
			conversation.User1ScreenshotDisabledAt = nil
		}
	} else {
		conversation.User2ScreenshotDisabled = requestBody.Enabled
		if requestBody.Enabled {
			conversation.User2ScreenshotDisabledAt = &now
		} else {
			conversation.User2ScreenshotDisabledAt = nil
		}
	}

	if err := database.DB.Save(conversation).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Screenshot ayarı değiştirilemedi"})
		return
	}

	// ✅ Her iki taraftan biri de disable ettiyse true
	bothDisabled := conversation.User1ScreenshotDisabled || conversation.User2ScreenshotDisabled

	// ✅ WebSocket üzerinden HER İKİ kullanıcıya da bildir
	// wsHub interface'ini WebSocketHub'a cast et
	if wsHubTyped, ok := h.wsHub.(interface {
		BroadcastScreenshotProtectionChange(user1ID, user2ID uint, isDisabled bool, changedByUserID uint)
	}); ok {
		wsHubTyped.BroadcastScreenshotProtectionChange(
			conversation.User1ID,
			conversation.User2ID,
			bothDisabled,
			userID.(uint),
		)
	} else {
		// Fallback - eski yöntem (sadece karşı tarafa gönder)
		h.wsHub.SendToUser(uint(otherUserID), "screenshot_protection_changed", map[string]interface{}{
			"conversation_id":        conversation.ID,
			"changed_by":             userID,
			"is_screenshot_disabled": bothDisabled,
			"changed_at":             now,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"message":                "Screenshot ayarı güncellendi",
		"my_screenshot_disabled": requestBody.Enabled,
		"is_screenshot_disabled": bothDisabled, // Genel durum
	})
}

// GetScreenshotProtectionStatus - Screenshot durumunu getir
func (h *ConversationHandler) GetScreenshotProtectionStatus(c *gin.Context) {
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

	conversation, err := h.GetOrCreateConversation(userID.(uint), uint(otherUserID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Conversation bulunamadı"})
		return
	}

	var myDisabled, otherDisabled bool

	if userID.(uint) == conversation.User1ID {
		myDisabled = conversation.User1ScreenshotDisabled
		otherDisabled = conversation.User2ScreenshotDisabled
	} else {
		myDisabled = conversation.User2ScreenshotDisabled
		otherDisabled = conversation.User1ScreenshotDisabled
	}

	c.JSON(http.StatusOK, gin.H{
		"my_screenshot_disabled":    myDisabled,
		"other_screenshot_disabled": otherDisabled,
		"is_screenshot_disabled":    myDisabled || otherDisabled, // Her iki taraftan biri disable ettiyse true
	})
}
