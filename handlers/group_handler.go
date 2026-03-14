package handlers

import (
	"beanpon_messenger/database"
	"beanpon_messenger/models"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

type GroupHandler struct {
	wsHub interface {
		IsUserOnline(userID uint) bool
		SendToUser(userID uint, messageType string, data interface{})
		SendToMultipleUsers(userIDs []uint, messageType string, data interface{})
	}
}

func NewGroupHandler(wsHub interface {
	IsUserOnline(userID uint) bool
	SendToUser(userID uint, messageType string, data interface{})
	SendToMultipleUsers(userIDs []uint, messageType string, data interface{})
}) *GroupHandler {
	return &GroupHandler{wsHub: wsHub}
}

// generateInviteToken 32 karakter random hex token üret
func generateInviteToken() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// getGroupParticipantIDs group'taki aktif üyelerin ID listesini döndür
func getGroupParticipantIDs(conversationID uint) []uint {
	var ids []uint
	database.DB.Model(&models.ConversationParticipant{}).
		Where("conversation_id = ? AND left_at IS NULL AND deleted_at IS NULL", conversationID).
		Pluck("user_id", &ids)
	return ids
}

// POST /api/v1/groups
func (h *GroupHandler) CreateGroup(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)

	var req models.CreateGroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	token, err := generateInviteToken()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Token üretilemedi"})
		return
	}

	now := time.Now()

	// Conversation oluştur (chat_type = 'group')
	conv := map[string]interface{}{
		"chat_type":    "group",
		"group_name":   req.Name,
		"group_desc":   req.Description,
		"created_by":   userID,
		"invite_token": token,
		"max_members":  256,
		"status":       "active",
		"created_at":   now,
		"updated_at":   now,
	}

	result := database.DB.Table("conversations").Create(&conv)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Grup oluşturulamadı"})
		return
	}

	// ID'yi al
	var conversation models.Conversation
	database.DB.Table("conversations").Where("invite_token = ?", token).First(&conversation)

	// Owner olarak ekle
	owner := models.ConversationParticipant{
		ConversationID: conversation.ID,
		UserID:         userID,
		Role:           "owner",
		JoinedAt:       &now,
	}
	database.DB.Create(&owner)

	// Başlangıç üyelerini ekle
	for _, memberID := range req.MemberIDs {
		if memberID == userID {
			continue
		}
		member := models.ConversationParticipant{
			ConversationID: conversation.ID,
			UserID:         memberID,
			Role:           "member",
			JoinedAt:       &now,
		}
		database.DB.Create(&member)

		// Bildirim gönder
		h.wsHub.SendToUser(memberID, "group_added", gin.H{
			"conversation_id": conversation.ID,
			"group_name":      req.Name,
			"added_by":        userID,
		})
	}

	c.JSON(http.StatusCreated, gin.H{
		"message": "Grup oluşturuldu",
		"data": gin.H{
			"id":           conversation.ID,
			"name":         req.Name,
			"invite_token": token,
		},
	})
}

// POST /api/v1/groups/join/:token
func (h *GroupHandler) JoinByToken(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	token := c.Param("token")

	var conversation models.Conversation
	err := database.DB.Table("conversations").
		Where("invite_token = ? AND chat_type = 'group' AND deleted_at IS NULL", token).
		First(&conversation).Error
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Geçersiz davet linki"})
		return
	}

	// Token süresi kontrolü
	if conversation.InviteTokenExpiresAt != nil && time.Now().After(*conversation.InviteTokenExpiresAt) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Davet linki süresi dolmuş"})
		return
	}

	// Zaten üye mi?
	var existing models.ConversationParticipant
	err = database.DB.Where("conversation_id = ? AND user_id = ? AND left_at IS NULL AND deleted_at IS NULL",
		conversation.ID, userID).First(&existing).Error
	if err == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Zaten bu grubun üyesisiniz"})
		return
	}

	// Üye sayısı kontrolü
	var memberCount int64
	database.DB.Model(&models.ConversationParticipant{}).
		Where("conversation_id = ? AND left_at IS NULL AND deleted_at IS NULL", conversation.ID).
		Count(&memberCount)
	if int(memberCount) >= conversation.MaxMembers {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Grup dolu"})
		return
	}

	now := time.Now()

	// Daha önce ayrılmış mı? → güncelle
	var old models.ConversationParticipant
	err = database.DB.Unscoped().Where("conversation_id = ? AND user_id = ?", conversation.ID, userID).
		First(&old).Error
	if err == nil {
		database.DB.Model(&old).Updates(map[string]interface{}{
			"left_at":           nil,
			"kicked_by":         nil,
			"deleted_at":        nil,
			"joined_at":         now,
			"invite_token_used": token,
		})
	} else {
		participant := models.ConversationParticipant{
			ConversationID:  conversation.ID,
			UserID:          userID,
			Role:            "member",
			JoinedAt:        &now,
			InviteTokenUsed: &token,
		}
		database.DB.Create(&participant)
	}

	// Diğer üyelere bildir
	memberIDs := getGroupParticipantIDs(conversation.ID)
	for _, mid := range memberIDs {
		if mid == userID {
			continue
		}
		h.wsHub.SendToUser(mid, "group_member_joined", gin.H{
			"conversation_id": conversation.ID,
			"user_id":         userID,
			"joined_at":       now,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"message":         "Gruba katıldınız",
		"conversation_id": conversation.ID,
		"group_name":      conversation.GroupName,
	})
}

// POST /api/v1/groups/:conversation_id/leave
func (h *GroupHandler) LeaveGroup(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	convID, err := strconv.ParseUint(c.Param("conversation_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz conversation_id"})
		return
	}
	conversationID := uint(convID)

	var participant models.ConversationParticipant
	err = database.DB.Where("conversation_id = ? AND user_id = ? AND left_at IS NULL AND deleted_at IS NULL",
		conversationID, userID).First(&participant).Error
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Bu grubun üyesi değilsiniz"})
		return
	}

	// Owner son kişiyse grubu dağıt
	if participant.Role == "owner" {
		var adminCount int64
		database.DB.Model(&models.ConversationParticipant{}).
			Where("conversation_id = ? AND role IN ('owner','admin') AND user_id != ? AND left_at IS NULL AND deleted_at IS NULL",
				conversationID, userID).Count(&adminCount)

		if adminCount == 0 {
			// Başka admin/owner yok — ownership transfer et veya grubu kapat
			// Şimdilik: ilk member'ı admin yap
			var firstMember models.ConversationParticipant
			err = database.DB.Where("conversation_id = ? AND user_id != ? AND left_at IS NULL AND deleted_at IS NULL",
				conversationID, userID).
				Order("joined_at ASC").First(&firstMember).Error
			if err == nil {
				database.DB.Model(&firstMember).Update("role", "admin")
				h.wsHub.SendToUser(firstMember.UserID, "group_role_changed", gin.H{
					"conversation_id": conversationID,
					"new_role":        "admin",
					"reason":          "owner_left",
				})
			}
		}
	}

	now := time.Now()
	database.DB.Model(&participant).Update("left_at", now)

	// Diğer üyelere bildir
	memberIDs := getGroupParticipantIDs(conversationID)
	h.wsHub.SendToMultipleUsers(memberIDs, "group_member_left", gin.H{
		"conversation_id": conversationID,
		"user_id":         userID,
		"left_at":         now,
	})

	c.JSON(http.StatusOK, gin.H{"message": "Gruptan ayrıldınız"})
}

// POST /api/v1/groups/:conversation_id/kick/:user_id
func (h *GroupHandler) KickMember(c *gin.Context) {
	requesterID := c.MustGet("user_id").(uint)
	convID, _ := strconv.ParseUint(c.Param("conversation_id"), 10, 32)
	targetID, _ := strconv.ParseUint(c.Param("user_id"), 10, 32)
	conversationID := uint(convID)
	targetUserID := uint(targetID)

	// Requester'ın rolünü kontrol et
	var requesterParticipant models.ConversationParticipant
	err := database.DB.Where("conversation_id = ? AND user_id = ? AND left_at IS NULL AND deleted_at IS NULL",
		conversationID, requesterID).First(&requesterParticipant).Error
	if err != nil || requesterParticipant.Role == "member" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Yetkiniz yok"})
		return
	}

	// Hedef kişiyi bul
	var targetParticipant models.ConversationParticipant
	err = database.DB.Where("conversation_id = ? AND user_id = ? AND left_at IS NULL AND deleted_at IS NULL",
		conversationID, targetUserID).First(&targetParticipant).Error
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Kullanıcı bu grupta değil"})
		return
	}

	// Admin, owner'ı kick edemez
	if requesterParticipant.Role == "admin" && targetParticipant.Role == "owner" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Owner'ı kick edemezsiniz"})
		return
	}

	now := time.Now()
	database.DB.Model(&targetParticipant).Updates(map[string]interface{}{
		"left_at":   now,
		"kicked_by": requesterID,
	})

	// Kick edilen kişiye bildir
	h.wsHub.SendToUser(targetUserID, "group_kicked", gin.H{
		"conversation_id": conversationID,
		"kicked_by":       requesterID,
		"kicked_at":       now,
	})

	// Diğer üyelere bildir
	memberIDs := getGroupParticipantIDs(conversationID)
	h.wsHub.SendToMultipleUsers(memberIDs, "group_member_kicked", gin.H{
		"conversation_id": conversationID,
		"user_id":         targetUserID,
		"kicked_by":       requesterID,
	})

	c.JSON(http.StatusOK, gin.H{"message": "Kullanıcı gruptan çıkarıldı"})
}

// PUT /api/v1/groups/:conversation_id/role/:user_id
func (h *GroupHandler) ChangeRole(c *gin.Context) {
	requesterID := c.MustGet("user_id").(uint)
	convID, _ := strconv.ParseUint(c.Param("conversation_id"), 10, 32)
	targetID, _ := strconv.ParseUint(c.Param("user_id"), 10, 32)
	conversationID := uint(convID)
	targetUserID := uint(targetID)

	var body struct {
		Role string `json:"role" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || (body.Role != "admin" && body.Role != "member") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "role: 'admin' veya 'member' olmalı"})
		return
	}

	// Sadece owner rol değiştirebilir
	var requesterP models.ConversationParticipant
	err := database.DB.Where("conversation_id = ? AND user_id = ? AND left_at IS NULL AND deleted_at IS NULL",
		conversationID, requesterID).First(&requesterP).Error
	if err != nil || requesterP.Role != "owner" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Sadece owner rol değiştirebilir"})
		return
	}

	var targetP models.ConversationParticipant
	err = database.DB.Where("conversation_id = ? AND user_id = ? AND left_at IS NULL AND deleted_at IS NULL",
		conversationID, targetUserID).First(&targetP).Error
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Kullanıcı bu grupta değil"})
		return
	}

	database.DB.Model(&targetP).Update("role", body.Role)

	h.wsHub.SendToUser(targetUserID, "group_role_changed", gin.H{
		"conversation_id": conversationID,
		"new_role":        body.Role,
		"changed_by":      requesterID,
	})

	c.JSON(http.StatusOK, gin.H{"message": "Rol güncellendi"})
}

// GET /api/v1/groups/:conversation_id/members
func (h *GroupHandler) GetMembers(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	convID, _ := strconv.ParseUint(c.Param("conversation_id"), 10, 32)
	conversationID := uint(convID)

	// Üye mi kontrol et
	var me models.ConversationParticipant
	err := database.DB.Where("conversation_id = ? AND user_id = ? AND left_at IS NULL AND deleted_at IS NULL",
		conversationID, userID).First(&me).Error
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "Bu grubun üyesi değilsiniz"})
		return
	}

	var members []struct {
		UserID       uint       `json:"user_id"`
		Name         string     `json:"name"`
		Username     string     `json:"username"`
		ProfileImage *string    `json:"profile_image"`
		Role         string     `json:"role"`
		JoinedAt     *time.Time `json:"joined_at"`
		IsOnline     bool       `json:"is_online"`
	}

	database.DB.Raw(`
		SELECT 
			cp.user_id,
			u.name,
			u.username,
			p.profile_image,
			cp.role,
			cp.joined_at
		FROM conversation_participants cp
		JOIN users u ON u.id = cp.user_id
		LEFT JOIN profiles p ON p.user_id = cp.user_id
		WHERE cp.conversation_id = ?
		  AND cp.left_at IS NULL
		  AND cp.deleted_at IS NULL
		ORDER BY 
			CASE cp.role WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END,
			cp.joined_at ASC
	`, conversationID).Scan(&members)

	// Online durumlarını ekle
	for i := range members {
		members[i].IsOnline = h.wsHub.IsUserOnline(members[i].UserID)
	}

	c.JSON(http.StatusOK, gin.H{
		"members": members,
		"count":   len(members),
	})
}

// GET /api/v1/groups - kullanıcının gruplarını listele
func (h *GroupHandler) GetMyGroups(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)

	var groups []struct {
		ConversationID uint       `json:"conversation_id"`
		GroupName      *string    `json:"group_name"`
		GroupAvatar    *string    `json:"group_avatar"`
		MyRole         string     `json:"my_role"`
		IsMuted        bool       `json:"is_muted"`
		MemberCount    int        `json:"member_count"`
		LastMessageAt  *time.Time `json:"last_message_at"`
		UnreadCount    int        `json:"unread_count"`
	}

	database.DB.Raw(`
		SELECT 
			c.id as conversation_id,
			c.group_name,
			c.group_avatar,
			cp.role as my_role,
			cp.is_muted,
			(SELECT COUNT(*) FROM conversation_participants cp2 
			 WHERE cp2.conversation_id = c.id AND cp2.left_at IS NULL AND cp2.deleted_at IS NULL) as member_count,
			c.last_message_at,
			(SELECT COUNT(*) FROM messages m
			 LEFT JOIN message_reads mr ON mr.message_id = m.id AND mr.user_id = ?
			 WHERE m.conversation_id = c.id
			   AND m.sender_id != ?
			   AND mr.id IS NULL
			   AND m.deleted_at IS NULL) as unread_count
		FROM conversations c
		JOIN conversation_participants cp ON cp.conversation_id = c.id
		WHERE cp.user_id = ?
		  AND cp.left_at IS NULL
		  AND cp.deleted_at IS NULL
		  AND c.chat_type = 'group'
		  AND c.deleted_at IS NULL
		ORDER BY c.last_message_at DESC NULLS LAST
	`, userID, userID, userID).Scan(&groups)

	c.JSON(http.StatusOK, gin.H{
		"groups": groups,
		"count":  len(groups),
	})
}

// POST /api/v1/groups/:conversation_id/invite-token/refresh
func (h *GroupHandler) RefreshInviteToken(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	convID, _ := strconv.ParseUint(c.Param("conversation_id"), 10, 32)
	conversationID := uint(convID)

	// Admin/owner kontrolü
	var me models.ConversationParticipant
	err := database.DB.Where("conversation_id = ? AND user_id = ? AND left_at IS NULL AND deleted_at IS NULL",
		conversationID, userID).First(&me).Error
	if err != nil || me.Role == "member" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Yetkiniz yok"})
		return
	}

	token, err := generateInviteToken()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Token üretilemedi"})
		return
	}

	database.DB.Table("conversations").
		Where("id = ?", conversationID).
		Update("invite_token", token)

	c.JSON(http.StatusOK, gin.H{"invite_token": token})
}
