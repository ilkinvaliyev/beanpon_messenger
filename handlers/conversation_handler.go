package handlers

import (
	"beanpon_messenger/database"
	"beanpon_messenger/models"
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
	}
}

func NewConversationHandler(wsHub interface {
	IsUserOnline(userID uint) bool
	SendToUser(userID uint, messageType string, data interface{})
}) *ConversationHandler {
	return &ConversationHandler{
		wsHub: wsHub,
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

	if err == gorm.ErrRecordNotFound {
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
	conversation, err := h.GetOrCreateConversation(senderID, receiverID)
	if err != nil {
		return false, "Conversation kontrolü başarısız", err
	}

	var senderMessageCount int
	var senderRestricted bool

	// Gönderen user1 mi user2 mi?
	if senderID == conversation.User1ID {
		senderMessageCount = conversation.User1MessageCount
		senderRestricted = conversation.User1Restricted
	} else {
		senderMessageCount = conversation.User2MessageCount
		senderRestricted = conversation.User2Restricted
	}

	// Restriction kontrolü
	if senderRestricted {
		return false, "Mesaj gönderme yetkiniz kısıtlanmış", nil
	}

	// Status'a göre kontroller
	switch conversation.Status {
	case "active":
		return true, "", nil

	case "pending":
		// Mutual follow varsa direkt gönderilebilir
		if conversation.MutualFollow {
			// Status'u active yap
			h.updateConversationStatus(conversation.ID, "active")
			return true, "", nil
		}

		// Pending durumda maksimum mesaj kontrolü
		if senderMessageCount >= conversation.MaxPendingMessages {
			return false, "Maksimum bekleyen mesaj sayısına ulaştınız", nil
		}

		return true, "", nil

	case "restricted":
		return false, "Bu konuşma kısıtlanmış", nil

	default:
		return false, "Bilinmeyen conversation durumu", nil
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
	} else {
		conversation.User2Muted = true
		conversation.User2MutedAt = &now
	}

	if err := database.DB.Save(conversation).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Mute işlemi başarısız"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message":  "Konuşma sessize alındı",
		"muted_at": now,
	})
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
	} else {
		conversation.User2Muted = false
		conversation.User2MutedAt = nil
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

	// Response'u kullanıcıya göre ayarla
	response := h.buildConversationResponse(conversation, userID.(uint))

	c.JSON(http.StatusOK, gin.H{
		"conversation": response,
	})
}

// buildConversationResponse kullanıcıya göre response oluştur
func (h *ConversationHandler) buildConversationResponse(conv *models.Conversation, currentUserID uint) models.ConversationResponse {
	var otherUserID uint
	var myMessageCount, otherMessageCount int
	var isMutedByMe, isRestrictedForMe bool

	if currentUserID == conv.User1ID {
		otherUserID = conv.User2ID
		myMessageCount = conv.User1MessageCount
		otherMessageCount = conv.User2MessageCount
		isMutedByMe = conv.User1Muted
		isRestrictedForMe = conv.User1Restricted
	} else {
		otherUserID = conv.User1ID
		myMessageCount = conv.User2MessageCount
		otherMessageCount = conv.User1MessageCount
		isMutedByMe = conv.User2Muted
		isRestrictedForMe = conv.User2Restricted
	}

	// Mesaj gönderebilir mi?
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
		CreatedAt:               conv.CreatedAt,
	}
}

// StringPtr string pointer helper
func StringPtr(s string) *string {
	return &s
}
