package handlers

import (
	"beanpon_messenger/database"
	"beanpon_messenger/models"
	"beanpon_messenger/utils"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// errGroupFull — atomik limit yoxlamasında "qrup dolu" sentinel xətası.
var errGroupFull = errors.New("group full")

type GroupHandler struct {
	wsHub interface {
		IsUserOnline(userID uint) bool
		SendToUser(userID uint, messageType string, data interface{})
		SendToMultipleUsers(userIDs []uint, messageType string, data interface{})
	}
	encryptionService interface {
		EncryptMessage(plainText string) (string, error)
		DecryptMessage(encryptedText string) (string, error)
	}
}

func NewGroupHandler(wsHub interface {
	IsUserOnline(userID uint) bool
	SendToUser(userID uint, messageType string, data interface{})
	SendToMultipleUsers(userIDs []uint, messageType string, data interface{})
}, encryptionService interface {
	EncryptMessage(plainText string) (string, error)
	DecryptMessage(encryptedText string) (string, error)
}) *GroupHandler {
	return &GroupHandler{wsHub: wsHub, encryptionService: encryptionService}
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

// ── Qrup icazələri (admin ayarları) ─────────────────────────────────────────

// defaultGroupPermissions — bütün əməliyyatlar AÇIQ.
func defaultGroupPermissions() map[string]bool {
	return map[string]bool{
		"allow_text":         true,
		"allow_media":        true,
		"allow_gif":          true,
		"allow_voice":        true,
		"allow_circle_video": true,
	}
}

// parseGroupPermissions — jsonb sütununu map-ə açır; NULL/bozuq → default
// (hamısı açıq). Çatışmayan açar = true (yeni icazə əlavə olunsa köhnə
// qruplar pozulmasın).
func parseGroupPermissions(raw *string) map[string]bool {
	perms := defaultGroupPermissions()
	if raw == nil || *raw == "" {
		return perms
	}
	var parsed map[string]bool
	if err := json.Unmarshal([]byte(*raw), &parsed); err != nil {
		return perms
	}
	for k, v := range parsed {
		perms[k] = v
	}
	return perms
}

// GET /api/v1/groups/:conversation_id/permissions
// Cari qrup icazələri (hər üzv oxuya bilər — UI input-u buna görə qurulur).
func (h *GroupHandler) GetGroupPermissions(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	convID, _ := strconv.ParseUint(c.Param("conversation_id"), 10, 32)
	conversationID := uint(convID)

	var me models.ConversationParticipant
	if err := database.DB.Where(
		"conversation_id = ? AND user_id = ? AND left_at IS NULL AND deleted_at IS NULL",
		conversationID, userID,
	).First(&me).Error; err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "Bu grubun üyesi değilsiniz"})
		return
	}

	var conv models.Conversation
	database.DB.Where("id = ?", conversationID).First(&conv)

	c.JSON(http.StatusOK, gin.H{
		"permissions": parseGroupPermissions(conv.GroupPermissions),
		"my_role":     me.Role,
	})
}

// PUT /api/v1/groups/:conversation_id/permissions
// Yalnız admin/owner. Body: {"allow_text":bool,...} — yalnız göndərilən
// açarlar dəyişir (partial update). Bütün üzvlərə group_permissions_changed
// WS event-i yayılır (input dərhal disable/enable olsun).
func (h *GroupHandler) UpdateGroupPermissions(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	convID, _ := strconv.ParseUint(c.Param("conversation_id"), 10, 32)
	conversationID := uint(convID)

	var me models.ConversationParticipant
	if err := database.DB.Where(
		"conversation_id = ? AND user_id = ? AND left_at IS NULL AND deleted_at IS NULL",
		conversationID, userID,
	).First(&me).Error; err != nil || (me.Role != "owner" && me.Role != "admin") {
		c.JSON(http.StatusForbidden, gin.H{"error": "Yalnızca yöneticiler değiştirebilir"})
		return
	}

	var body map[string]bool
	if err := c.ShouldBindJSON(&body); err != nil || len(body) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz body"})
		return
	}

	var conv models.Conversation
	database.DB.Where("id = ?", conversationID).First(&conv)
	perms := parseGroupPermissions(conv.GroupPermissions)

	allowedKeys := defaultGroupPermissions()
	for k, v := range body {
		if _, ok := allowedKeys[k]; ok {
			perms[k] = v
		}
	}

	jsonBytes, err := json.Marshal(perms)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "JSON xətası"})
		return
	}
	if err := database.DB.Table("conversations").
		Where("id = ?", conversationID).
		Update("group_permissions", string(jsonBytes)).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Kaydedilemedi"})
		return
	}

	// Bütün üzvlərə yay — input dərhal yenilənsin.
	memberIDs := getGroupParticipantIDs(conversationID)
	h.wsHub.SendToMultipleUsers(memberIDs, "group_permissions_changed", gin.H{
		"conversation_id": conversationID,
		"permissions":     perms,
		"changed_by":      userID,
	})

	c.JSON(http.StatusOK, gin.H{"permissions": perms})
}

// sendGroupInvitePush — "X sizi Y qrupuna dəvət etdi" FCM push-u Laravel
// üzərindən (`/notification/group-invite`). Laravel həm FCM göndərir, həm
// notifications cədvəlinə yazır (bildirimlər səhifəsində görünsün).
// Qeyri-bloklayıcı (goroutine-də çağırılır), xəta yalnız loglanır.
func (h *GroupHandler) sendGroupInvitePush(inviterID, conversationID uint, groupName string, receiverIDs []uint) {
	backendURL := os.Getenv("BACKEND_URL")
	cloudToken := os.Getenv("CLOUD_TOKEN")
	if backendURL == "" || cloudToken == "" || len(receiverIDs) == 0 {
		return
	}

	payload := map[string]interface{}{
		"receiver_ids":    receiverIDs,
		"inviter_id":      inviterID,
		"conversation_id": conversationID,
		"group_name":      groupName,
	}
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return
	}

	req, err := http.NewRequest("POST", backendURL+"/notification/group-invite",
		bytes.NewBuffer(jsonData))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", cloudToken)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("❌ Qrup dəvət push xətası: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		log.Printf("❌ Qrup dəvət push uğursuz, status: %d", resp.StatusCode)
	}
}

// POST /api/v1/groups/:conversation_id/invite/accept
// PENDING dəvəti QƏBUL et → tam üzv (invite_status='active'). Bütün üzvlərə
// group_member_joined WS event-i (mövcud "qatıldı" axını ilə eyni).
func (h *GroupHandler) AcceptInvite(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	convID, err := strconv.ParseUint(c.Param("conversation_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz conversation_id"})
		return
	}
	conversationID := uint(convID)

	var p models.ConversationParticipant
	err = database.DB.Where(
		"conversation_id = ? AND user_id = ? AND left_at IS NULL AND deleted_at IS NULL AND invite_status = 'pending'",
		conversationID, userID,
	).First(&p).Error
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Bekleyen davet bulunamadı"})
		return
	}

	now := time.Now()
	if err := database.DB.Model(&p).Updates(map[string]interface{}{
		"invite_status": "active",
		"joined_at":     now,
	}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Davet kabul edilemedi"})
		return
	}

	// Üzvlərə "X qatıldı" bildir (mövcud event adı — Flutter onsuz da emal edir).
	var username string
	database.DB.Raw(`SELECT username FROM users WHERE id = ?`, userID).Scan(&username)
	memberIDs := getGroupParticipantIDs(conversationID)
	h.wsHub.SendToMultipleUsers(memberIDs, "group_member_joined", gin.H{
		"conversation_id": conversationID,
		"user_id":         userID,
		"username":        username,
	})

	var groupName string
	database.DB.Table("conversations").
		Where("id = ?", conversationID).
		Select("COALESCE(group_name, '')").Scan(&groupName)

	c.JSON(http.StatusOK, gin.H{
		"message":         "Davet kabul edildi",
		"conversation_id": conversationID,
		"group_name":      groupName,
		"my_role":         p.Role,
	})
}

// POST /api/v1/groups/:conversation_id/invite/decline
// PENDING dəvəti RƏDD et → participant sətri silinir (qrup siyahıdan çıxır).
func (h *GroupHandler) DeclineInvite(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	convID, err := strconv.ParseUint(c.Param("conversation_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz conversation_id"})
		return
	}
	conversationID := uint(convID)

	result := database.DB.Unscoped().Where(
		"conversation_id = ? AND user_id = ? AND invite_status = 'pending'",
		conversationID, userID,
	).Delete(&models.ConversationParticipant{})

	if result.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Bekleyen davet bulunamadı"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Davet reddedildi"})
}

// filterInvitableUsers — qrup dəvəti İCAZƏ filtri (Laravel
// users.group_invite_permission sütunu; default 'following'):
//   - nobody    → heç kim dəvət edə bilməz (siyahıdan çıxar)
//   - following → yalnız adayın İZLƏDİYİ adam dəvət edə bilər:
//     follows(follower_id = aday, following_id = inviter) yoxlanır
//   - everyone  → hər kəs dəvət edə bilər
//
// Qaytarır: icazəsi olan user id-ləri (sıra qorunur, inviter çıxarılır).
// Bilinməyən/NULL dəyər 'following' kimi davranır (migration default-u).
func filterInvitableUsers(inviterID uint, userIDs []uint) []uint {
	if len(userIDs) == 0 {
		return userIDs
	}

	// Adayların icazə dəyərlərini TƏK sorğu ilə oxu.
	var rows []struct {
		ID         uint   `gorm:"column:id"`
		Permission string `gorm:"column:group_invite_permission"`
	}
	database.DB.Raw(`
		SELECT id, COALESCE(group_invite_permission, 'following') as group_invite_permission
		FROM users WHERE id IN ?
	`, userIDs).Scan(&rows)
	permByID := make(map[uint]string, len(rows))
	for _, r := range rows {
		permByID[r.ID] = r.Permission
	}

	// 'following' olanlar üçün: aday inviter-i izləyirmi? — TƏK sorğu.
	// follows(follower_id = aday, following_id = inviter).
	var followerIDs []uint
	database.DB.Table("follows").
		Where("following_id = ? AND follower_id IN ?", inviterID, userIDs).
		Pluck("follower_id", &followerIDs)
	followsInviter := make(map[uint]bool, len(followerIDs))
	for _, id := range followerIDs {
		followsInviter[id] = true
	}

	allowed := make([]uint, 0, len(userIDs))
	for _, uid := range userIDs {
		if uid == inviterID {
			continue
		}
		switch permByID[uid] {
		case "everyone":
			allowed = append(allowed, uid)
		case "nobody":
			// dəvət qadağandır — atla
		default: // 'following' (və ya bilinməyən/users-də tapılmayan)
			if followsInviter[uid] {
				allowed = append(allowed, uid)
			}
		}
	}
	return allowed
}

// POST /api/v1/groups
func (h *GroupHandler) CreateGroup(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)

	var req models.CreateGroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Qrup limiti: maksimum 50 nəfər (yaradan DAXİL). Yaratma zamanı ilkin
	// üzv siyahısı da yoxlanır (yaradan + member_ids <= 50).
	const groupMaxMembers = 50
	if len(req.MemberIDs)+1 > groupMaxMembers {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Grup en fazla 50 kişi olabilir",
		})
		return
	}

	// 🔒 Dəvət icazəsi filtri (group_invite_permission):
	//   nobody → çıxarılır; following → yaradan adayın izlədikləri arasında
	//   deyilsə çıxarılır. Filtrdən sonra yaradan XARİC heç kim qalmırsa
	//   qrup YARADILMIR — boş/tək nəfərlik qrup yaranmasın.
	req.MemberIDs = filterInvitableUsers(userID, req.MemberIDs)
	if len(req.MemberIDs) == 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error": "Seçilen kullanıcılar grup davetlerine izin vermiyor",
			"code":  "no_invitable_members",
		})
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
		"max_members":  groupMaxMembers,
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

	// Başlangıç üyeleri — PENDING dəvət kimi (onay axını). Üzv "Qatıl"
	// deyənə qədər mesajları görmür; siyahıda "qrupa dəvət edildiniz"
	// sətri görünür.
	pendingStatus := "pending"
	for _, memberID := range req.MemberIDs {
		if memberID == userID {
			continue
		}
		member := models.ConversationParticipant{
			ConversationID: conversation.ID,
			UserID:         memberID,
			Role:           "member",
			JoinedAt:       &now,
			InviteStatus:   &pendingStatus,
		}
		database.DB.Create(&member)

		// WS bildirimi (siyahı yenilənsin) + FCM (Laravel üzərindən).
		h.wsHub.SendToUser(memberID, "group_invited", gin.H{
			"conversation_id": conversation.ID,
			"group_name":      req.Name,
			"invited_by":      userID,
		})
	}
	if len(req.MemberIDs) > 0 {
		go h.sendGroupInvitePush(userID, conversation.ID, req.Name, req.MemberIDs)
	}

	// YARADANA da group_added göndər — ConversationsPage bu event-də qrup
	// siyahısını yeniləyir (_loadGroups). Əvvəl yalnız üzvlərə gedirdi:
	// yaradan öz siyahısında qrupu görmürdü, kimsə mesaj yazana qədər
	// (new_group_message gələnə qədər) görünmürdü.
	h.wsHub.SendToUser(userID, "group_added", gin.H{
		"conversation_id": conversation.ID,
		"group_name":      req.Name,
		"added_by":        userID,
	})

	c.JSON(http.StatusCreated, gin.H{
		"message": "Grup oluşturuldu",
		"data": gin.H{
			"id":           conversation.ID,
			"name":         req.Name,
			"invite_token": token,
		},
	})
}

// GET /api/v1/groups/join/:token/preview
// Dəvət linki ÖNİZLƏMƏSİ — qoşulmazdan ƏVVƏL qrup adı/avatar/üzv siyahısı.
// Üzvlük TƏLƏB OLUNMUR (JWT kifayət) — link alan hər kəs kimlərin olduğunu
// görüb "Qatıl / Qəbul etmə" qərarı verir. Heç bir yazma əməliyyatı YOXDUR.
func (h *GroupHandler) PreviewByToken(c *gin.Context) {
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

	// Token süresi kontrolü (JoinByToken ilə eyni).
	if conversation.InviteTokenExpiresAt != nil && time.Now().After(*conversation.InviteTokenExpiresAt) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Davet linki süresi dolmuş"})
		return
	}

	// Artıq üzvdürmü? (Flutter bilsin — "Qatıl" əvəzinə birbaşa aça bilər.)
	var existing models.ConversationParticipant
	alreadyMember := database.DB.Where(
		"conversation_id = ? AND user_id = ? AND left_at IS NULL AND deleted_at IS NULL",
		conversation.ID, userID,
	).First(&existing).Error == nil

	// Üzv siyahısı — ad/username/avatar (preview-da göstərmək üçün).
	var members []struct {
		UserID       uint    `json:"user_id" gorm:"column:user_id"`
		Name         string  `json:"name" gorm:"column:name"`
		Username     string  `json:"username" gorm:"column:username"`
		IsVerified   bool    `json:"is_verified" gorm:"column:is_verified"`
		ProfileImage *string `json:"profile_image" gorm:"column:profile_image"`
	}
	database.DB.Raw(`
		SELECT cp.user_id, u.name, u.username, u.is_verified, p.profile_image
		FROM conversation_participants cp
		JOIN users u ON u.id = cp.user_id
		LEFT JOIN profiles p ON p.user_id = cp.user_id
		WHERE cp.conversation_id = ? AND cp.left_at IS NULL AND cp.deleted_at IS NULL
		  AND u.deactivated_at IS NULL
		ORDER BY cp.created_at ASC
	`, conversation.ID).Scan(&members)

	memberList := make([]gin.H, 0, len(members))
	for _, m := range members {
		memberList = append(memberList, gin.H{
			"user_id":       m.UserID,
			"name":          m.Name,
			"username":      m.Username,
			"is_verified":   m.IsVerified,
			"profile_image": utils.PrependBaseURL(m.ProfileImage),
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"conversation_id": conversation.ID,
		"group_name":      conversation.GroupName,
		"avatar":          utils.PrependBaseURL(conversation.GroupAvatar),
		"member_count":    len(memberList),
		"already_member":  alreadyMember,
		"members":         memberList,
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

	// Zaten üye mi? → XƏTA DEYİL, uğurlu cavab (conversation_id ilə).
	// Flutter linki açan artıq-üzv istifadəçini birbaşa qrup çatına aparır.
	// Əvvəl 400 qaytarırdı → client "qrupa qatıla bilmədi" göstərirdi.
	var existing models.ConversationParticipant
	err = database.DB.Where("conversation_id = ? AND user_id = ? AND left_at IS NULL AND deleted_at IS NULL",
		conversation.ID, userID).First(&existing).Error
	if err == nil {
		groupName := ""
		if conversation.GroupName != nil {
			groupName = *conversation.GroupName
		}
		c.JSON(http.StatusOK, gin.H{
			"message":         "Zaten bu grubun üyesisiniz",
			"already_member":  true,
			"conversation_id": conversation.ID,
			"group_name":      groupName,
			"my_role":         existing.Role,
		})
		return
	}

	now := time.Now()

	// ⚠️ ATOMİK limit yoxlaması: əvvəl COUNT→INSERT ayrı idi və paralel
	// qoşulmalarda (link viral olanda) yarış vəziyyəti limiti aşırdı
	// (max_members=256 ikən 800+ üzv). İndi TRANSACTION + conversation
	// sətrində FOR UPDATE kilidi: eyni qrupa paralel join-lər sıraya düzülür,
	// hər biri kilid altında təzə COUNT görür — limit FİZİKİ aşıla bilmir.
	joinErr := database.DB.Transaction(func(tx *gorm.DB) error {
		// Qrup sətrini kilidlə (digər join-lər burada gözləyir).
		var lockedConv models.Conversation
		if err := tx.Table("conversations").
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", conversation.ID).
			First(&lockedConv).Error; err != nil {
			return err
		}

		var memberCount int64
		if err := tx.Model(&models.ConversationParticipant{}).
			Where("conversation_id = ? AND left_at IS NULL AND deleted_at IS NULL", conversation.ID).
			Count(&memberCount).Error; err != nil {
			return err
		}
		if int(memberCount) >= lockedConv.MaxMembers {
			return errGroupFull
		}

		// Daha önce ayrılmış mı? → güncelle
		var old models.ConversationParticipant
		err := tx.Unscoped().Where("conversation_id = ? AND user_id = ?", conversation.ID, userID).
			First(&old).Error
		if err == nil {
			return tx.Model(&old).Updates(map[string]interface{}{
				"left_at":           nil,
				"kicked_by":         nil,
				"deleted_at":        nil,
				"joined_at":         now,
				"invite_token_used": token,
			}).Error
		}
		participant := models.ConversationParticipant{
			ConversationID:  conversation.ID,
			UserID:          userID,
			Role:            "member",
			JoinedAt:        &now,
			InviteTokenUsed: &token,
		}
		return tx.Create(&participant).Error
	})

	if joinErr == errGroupFull {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Grup dolu"})
		return
	}
	if joinErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Gruba katılamadınız"})
		return
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

	// Admin/owner çıxır və BAŞQA admin/owner YOXDURSA → qrup SİLİNİR
	// (istifadəçi tələbi: idarəçisiz qrup qalmasın). Üzvlərə group_deleted
	// göndərilir — Flutter siyahıdan çıxarır və çatı bağlayır.
	if participant.Role == "owner" || participant.Role == "admin" {
		var adminCount int64
		database.DB.Model(&models.ConversationParticipant{}).
			Where("conversation_id = ? AND role IN ('owner','admin') AND user_id != ? AND left_at IS NULL AND deleted_at IS NULL",
				conversationID, userID).Count(&adminCount)

		if adminCount == 0 {
			now := time.Now()
			// Çıxan adminin participant qeydini bağla.
			database.DB.Model(&participant).Update("left_at", now)

			// Qalan üzvlərə xəbər ver (siyahı silinmədən ƏVVƏL götürülür).
			memberIDs := getGroupParticipantIDs(conversationID)

			// Qrupu soft-delete et.
			database.DB.Model(&models.Conversation{}).
				Where("id = ?", conversationID).
				Update("deleted_at", now)

			h.wsHub.SendToMultipleUsers(memberIDs, "group_deleted", gin.H{
				"conversation_id": conversationID,
				"reason":          "admin_left",
			})

			c.JSON(http.StatusOK, gin.H{"message": "Grup kapatıldı", "group_deleted": true})
			return
		}
	}

	now := time.Now()
	database.DB.Model(&participant).Update("left_at", now)

	// Çıxanın adı — Flutter çatda "X qrupu tərk etdi" sistem sətri göstərsin.
	var leaverUsername string
	database.DB.Raw(`SELECT username FROM users WHERE id = ?`, userID).Scan(&leaverUsername)

	// Diğer üyelere bildir
	memberIDs := getGroupParticipantIDs(conversationID)
	h.wsHub.SendToMultipleUsers(memberIDs, "group_member_left", gin.H{
		"conversation_id": conversationID,
		"user_id":         userID,
		"username":        leaverUsername,
		"left_at":         now,
	})

	c.JSON(http.StatusOK, gin.H{"message": "Gruptan ayrıldınız"})
}

func (h *GroupHandler) MuteGroup(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	convID, err := strconv.ParseUint(c.Param("conversation_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz conversation_id"})
		return
	}
	conversationID := uint(convID)

	var requestBody struct {
		MuteDuration int `json:"muteDuration"` // dakika cinsinden
	}
	if err := c.ShouldBindJSON(&requestBody); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz request body"})
		return
	}

	now := time.Now()
	updates := map[string]interface{}{
		"is_muted": true,
	}

	if requestBody.MuteDuration > 0 {
		mutedUntil := now.Add(time.Duration(requestBody.MuteDuration) * time.Minute)
		updates["muted_until"] = mutedUntil
	} else {
		updates["muted_until"] = nil
	}

	database.DB.Model(&models.ConversationParticipant{}).
		Where("conversation_id = ? AND user_id = ?", conversationID, userID).
		Updates(updates)

	response := gin.H{
		"message":  "Grup sessize alındı",
		"muted_at": now,
	}
	if requestBody.MuteDuration > 0 {
		response["muted_until"] = now.Add(time.Duration(requestBody.MuteDuration) * time.Minute)
	}

	c.JSON(http.StatusOK, response)
}

func (h *GroupHandler) UnmuteGroup(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	convID, _ := strconv.ParseUint(c.Param("conversation_id"), 10, 32)
	conversationID := uint(convID)

	database.DB.Model(&models.ConversationParticipant{}).
		Where("conversation_id = ? AND user_id = ?", conversationID, userID).
		Updates(map[string]interface{}{
			"is_muted":    false,
			"muted_until": nil,
		})

	c.JSON(http.StatusOK, gin.H{"message": "Grup sesi açıldı"})
}

// POST /api/v1/groups/:conversation_id/pin — qrupu sabitlə (per-user).
func (h *GroupHandler) PinGroup(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	convID, _ := strconv.ParseUint(c.Param("conversation_id"), 10, 32)
	conversationID := uint(convID)

	now := time.Now()
	database.DB.Model(&models.ConversationParticipant{}).
		Where("conversation_id = ? AND user_id = ? AND left_at IS NULL AND deleted_at IS NULL", conversationID, userID).
		Updates(map[string]interface{}{
			"is_pinned": true,
			"pinned_at": now,
		})

	c.JSON(http.StatusOK, gin.H{"message": "Grup sabitləndi", "is_pinned": true})
}

// POST /api/v1/groups/:conversation_id/unpin
func (h *GroupHandler) UnpinGroup(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	convID, _ := strconv.ParseUint(c.Param("conversation_id"), 10, 32)
	conversationID := uint(convID)

	database.DB.Model(&models.ConversationParticipant{}).
		Where("conversation_id = ? AND user_id = ? AND left_at IS NULL AND deleted_at IS NULL", conversationID, userID).
		Updates(map[string]interface{}{
			"is_pinned": false,
			"pinned_at": nil,
		})

	c.JSON(http.StatusOK, gin.H{"message": "Sabitləmə götürüldü", "is_pinned": false})
}

// POST /api/v1/groups/:conversation_id/archive — qrupu arxivlə (per-user).
func (h *GroupHandler) ArchiveGroup(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	convID, _ := strconv.ParseUint(c.Param("conversation_id"), 10, 32)
	conversationID := uint(convID)

	now := time.Now()
	database.DB.Model(&models.ConversationParticipant{}).
		Where("conversation_id = ? AND user_id = ? AND left_at IS NULL AND deleted_at IS NULL", conversationID, userID).
		Updates(map[string]interface{}{
			"is_archived": true,
			"archived_at": now,
			// Arxivlə eyni zamanda pin götürülür (DM davranışı ilə uyğun).
			"is_pinned": false,
			"pinned_at": nil,
		})

	c.JSON(http.StatusOK, gin.H{"message": "Grup arxivləndi", "is_archived": true})
}

// POST /api/v1/groups/:conversation_id/unarchive
func (h *GroupHandler) UnarchiveGroup(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	convID, _ := strconv.ParseUint(c.Param("conversation_id"), 10, 32)
	conversationID := uint(convID)

	database.DB.Model(&models.ConversationParticipant{}).
		Where("conversation_id = ? AND user_id = ? AND left_at IS NULL AND deleted_at IS NULL", conversationID, userID).
		Updates(map[string]interface{}{
			"is_archived": false,
			"archived_at": nil,
		})

	c.JSON(http.StatusOK, gin.H{"message": "Arxivdən çıxarıldı", "is_archived": false})
}

// POST /api/v1/groups/:conversation_id/clear — söhbəti təmizlə.
// Body: {"mode": "me" | "all"}
//
//	me  — yalnız özüm üçün: participant.cleared_at = now (server mesajlara
//	      toxunmur; GetGroupMessages cleared_at-dan sonrakıları qaytarır).
//	all — yalnız admin/owner: bütün mesajlar soft-delete + group_chat_cleared
//	      WS event (hamının çatı boşalır).
func (h *GroupHandler) ClearGroupChat(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	convID, _ := strconv.ParseUint(c.Param("conversation_id"), 10, 32)
	conversationID := uint(convID)

	var body struct {
		Mode string `json:"mode"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || (body.Mode != "me" && body.Mode != "all") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "mode 'me' veya 'all' olmalı"})
		return
	}

	var me models.ConversationParticipant
	err := database.DB.Where(
		"conversation_id = ? AND user_id = ? AND left_at IS NULL AND deleted_at IS NULL",
		conversationID, userID,
	).First(&me).Error
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "Bu grubun üyesi değilsiniz"})
		return
	}

	now := time.Now()

	if body.Mode == "all" {
		// Yalnız admin/owner hamı üçün təmizləyə bilər.
		if me.Role != "owner" && me.Role != "admin" {
			c.JSON(http.StatusForbidden, gin.H{"error": "Yalnızca yöneticiler herkes için temizleyebilir"})
			return
		}
		// Bütün mesajları soft-delete et.
		database.DB.Model(&models.Message{}).
			Where("conversation_id = ? AND deleted_at IS NULL", conversationID).
			Update("deleted_at", now)

		// Üzvlərə bildir — Flutter çatı boşaldır + lokal cache təmizləyir.
		memberIDs := getGroupParticipantIDs(conversationID)
		h.wsHub.SendToMultipleUsers(memberIDs, "group_chat_cleared", gin.H{
			"conversation_id": conversationID,
			"cleared_by":      userID,
			"mode":            "all",
		})

		c.JSON(http.StatusOK, gin.H{"message": "Sohbet herkes için temizlendi", "mode": "all"})
		return
	}

	// mode == "me": yalnız özüm üçün — cleared_at qoy.
	database.DB.Model(&me).Update("cleared_at", now)
	c.JSON(http.StatusOK, gin.H{"message": "Sohbet sizin için temizlendi", "mode": "me"})
}

// POST /api/v1/groups/:conversation_id/wallpaper — qrup çatı üçün fon (per-user).
// Body: {"wallpaper_id": <int|null>} — null/0 = sıfırla (qlobal/default görünür).
// Seçim conversation_participants.chat_wallpaper_id-də saxlanır, yalnız bu
// istifadəçiyə təsir edir.
func (h *GroupHandler) SetGroupWallpaper(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	convID, err := strconv.ParseUint(c.Param("conversation_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz conversation_id"})
		return
	}
	conversationID := uint(convID)

	var body struct {
		WallpaperID *uint `json:"wallpaper_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz istek"})
		return
	}

	var me models.ConversationParticipant
	err = database.DB.Where(
		"conversation_id = ? AND user_id = ? AND left_at IS NULL AND deleted_at IS NULL",
		conversationID, userID,
	).First(&me).Error
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "Bu grubun üyesi değilsiniz"})
		return
	}

	// null və ya 0 → sıfırla (NULL).
	if body.WallpaperID == nil || *body.WallpaperID == 0 {
		if err := database.DB.Model(&me).Update("chat_wallpaper_id", nil).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Duvar kağıdı kaydedilemedi"})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"message":           "Duvar kağıdı sıfırlandı",
			"chat_wallpaper_id": nil,
		})
		return
	}

	if err := database.DB.Model(&me).Update("chat_wallpaper_id", *body.WallpaperID).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Duvar kağıdı kaydedilemedi"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message":           "Duvar kağıdı güncellendi",
		"chat_wallpaper_id": *body.WallpaperID,
	})
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
		IsVerified   bool       `json:"is_verified"`
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
			u.is_verified,
			p.profile_image,
			cp.role,
			cp.joined_at
		FROM conversation_participants cp
		JOIN users u ON u.id = cp.user_id
		LEFT JOIN profiles p ON p.user_id = cp.user_id
		WHERE cp.conversation_id = ?
		  AND cp.left_at IS NULL
		  AND cp.deleted_at IS NULL
		  AND u.deactivated_at IS NULL
		  AND NOT EXISTS (
		      SELECT 1 FROM user_blocks ub
		      WHERE (ub.blocker_id = ? AND ub.blocked_id = cp.user_id)
		         OR (ub.blocker_id = cp.user_id AND ub.blocked_id = ?)
		  )
		ORDER BY
			CASE cp.role WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END,
			cp.joined_at ASC
	`, conversationID, userID, userID).Scan(&members)

	for i := range members {
		members[i].IsOnline = h.wsHub.IsUserOnline(members[i].UserID)
		// Profil şəkli nisbi yol gəlir — tam URL-ə çevir (Flutter birbaşa
		// göstərə bilsin). PrependBaseURL nil-safe-dir.
		members[i].ProfileImage = utils.PrependBaseURL(members[i].ProfileImage)
	}

	c.JSON(http.StatusOK, gin.H{
		"members": members,
		"count":   len(members),
	})
}

// GET /api/v1/groups - kullanıcının gruplarını listele
// Query: archived=false (default, arxivlənmişlər gizli) | true (yalnız arxiv) | all
func (h *GroupHandler) GetMyGroups(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	archivedFilter := c.DefaultQuery("archived", "false")

	archivedWhere := ""
	switch archivedFilter {
	case "true":
		archivedWhere = "AND COALESCE(cp.is_archived, false) = true"
	case "all":
		archivedWhere = ""
	default:
		archivedWhere = "AND COALESCE(cp.is_archived, false) = false"
	}

	var groups []struct {
		ConversationID     uint       `json:"conversation_id"`
		GroupName          *string    `json:"group_name"`
		GroupAvatar        *string    `json:"group_avatar"`
		MyRole             string     `json:"my_role"`
		InviteStatus       string     `json:"invite_status"`
		IsMuted            bool       `json:"is_muted"`
		IsPinned           bool       `json:"is_pinned"`
		PinnedAt           *time.Time `json:"pinned_at"`
		IsArchived         bool       `json:"is_archived"`
		MemberCount        int        `json:"member_count"`
		LastMessageAt      *time.Time `json:"last_message_at"`
		UnreadCount        int        `json:"unread_count"`
		LastMessageText    *string    `json:"last_message_text"`
		LastSenderUsername *string    `json:"last_sender_username"`
	}

	database.DB.Raw(`
		SELECT
			c.id as conversation_id,
			c.group_name,
			c.group_avatar,
			cp.role as my_role,
			COALESCE(cp.invite_status, 'active') as invite_status,
			cp.is_muted,
			COALESCE(cp.is_pinned, false) as is_pinned,
			cp.pinned_at,
			COALESCE(cp.is_archived, false) as is_archived,
			(SELECT COUNT(*) FROM conversation_participants cp2
			 WHERE cp2.conversation_id = c.id AND cp2.left_at IS NULL AND cp2.deleted_at IS NULL) as member_count,
			-- BOŞ QRUP FIX: heç mesaj yoxdursa last_message_at NULL gəlirdi →
			-- Flutter epoch-0 ilə siyahının ƏN DİBİNƏ atırdı (görünməz kimi).
			-- Fallback: qoşulma/yaranma vaxtı → yeni qrup yuxarılarda görünür.
			COALESCE(c.last_message_at, cp.joined_at, c.created_at) as last_message_at,
			(SELECT COUNT(*) FROM messages m
			 LEFT JOIN message_reads mr ON mr.message_id = m.id AND mr.user_id = ?
			 WHERE m.conversation_id = c.id
			   AND m.sender_id != ?
			   AND mr.id IS NULL
			   AND m.deleted_at IS NULL
			   AND NOT EXISTS (
			       SELECT 1 FROM user_blocks ub
			       WHERE (ub.blocker_id = ? AND ub.blocked_id = m.sender_id)
			          OR (ub.blocker_id = m.sender_id AND ub.blocked_id = ?)
			   )) as unread_count,
			last_msg.encrypted_text as last_message_text,
			last_msg_user.username as last_sender_username
		FROM conversations c
		JOIN conversation_participants cp ON cp.conversation_id = c.id
		LEFT JOIN LATERAL (
			SELECT m.encrypted_text, m.sender_id
			FROM messages m
			WHERE m.conversation_id = c.id AND m.deleted_at IS NULL
			ORDER BY m.created_at DESC LIMIT 1
		) last_msg ON true
		LEFT JOIN users last_msg_user ON last_msg_user.id = last_msg.sender_id
		WHERE cp.user_id = ?
		  AND cp.left_at IS NULL
		  AND cp.deleted_at IS NULL
		  AND c.chat_type = 'group'
		  AND c.deleted_at IS NULL
		  `+archivedWhere+`
		ORDER BY
			COALESCE(cp.is_pinned, false) DESC,
			cp.pinned_at DESC NULLS LAST,
			COALESCE(c.last_message_at, cp.joined_at, c.created_at) DESC NULLS LAST
	`, userID, userID, userID, userID, userID).Scan(&groups)

	for i := range groups {
		if groups[i].LastMessageText != nil {
			decrypted, err := h.encryptionService.DecryptMessage(*groups[i].LastMessageText)
			if err == nil {
				groups[i].LastMessageText = &decrypted
			} else {
				empty := ""
				groups[i].LastMessageText = &empty
			}
		}
	}

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

// GET /api/v1/groups/:conversation_id
func (h *GroupHandler) GetGroupDetail(c *gin.Context) {
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

	var conv models.Conversation
	if err := database.DB.Where("id = ? AND chat_type = 'group' AND deleted_at IS NULL", conversationID).
		First(&conv).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Grup bulunamadı"})
		return
	}

	var memberCount int64
	database.DB.Model(&models.ConversationParticipant{}).
		Where("conversation_id = ? AND left_at IS NULL AND deleted_at IS NULL", conversationID).
		Count(&memberCount)

	var memberPreviews []struct {
		Username string `gorm:"column:username"`
	}
	database.DB.Raw(`
		SELECT u.username
		FROM conversation_participants cp
		JOIN users u ON u.id = cp.user_id
		WHERE cp.conversation_id = ?
		  AND cp.left_at IS NULL
		  AND cp.deleted_at IS NULL
		  AND cp.user_id != ?
		ORDER BY cp.joined_at ASC
		LIMIT 3
	`, conversationID, userID).Scan(&memberPreviews)

	previews := make([]string, 0, len(memberPreviews))
	for _, m := range memberPreviews {
		previews = append(previews, m.Username)
	}

	var inviteToken *string
	if me.Role == "owner" || me.Role == "admin" {
		inviteToken = conv.InviteToken
	}

	// 📌 Pinned mesaj (banner üçün): id + decrypt text + göndərən username.
	// Mesaj silinmişsə (deleted_at) pin sayılmır.
	var pinnedMessage interface{} = nil
	if conv.PinnedMessageID != nil {
		var pinned models.Message
		if err := database.DB.Where("id = ? AND deleted_at IS NULL",
			*conv.PinnedMessageID).First(&pinned).Error; err == nil {
			decrypted, _ := h.encryptionService.DecryptMessage(pinned.EncryptedText)
			var senderUsername string
			database.DB.Raw(`SELECT username FROM users WHERE id = ?`, pinned.SenderID).
				Scan(&senderUsername)
			pinnedMessage = gin.H{
				"id":              pinned.ID,
				"text":            decrypted,
				"sender_id":       pinned.SenderID,
				"sender_username": senderUsername,
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"id":              conv.ID,
		"name":            conv.GroupName,
		"avatar":          conv.GroupAvatar,
		"description":     conv.GroupDesc,
		"member_count":    memberCount,
		"my_role":         me.Role,
		"is_muted":        me.IsMuted,
		"my_wallpaper_id": me.ChatWallpaperID,
		"invite_token":    inviteToken,
		"member_previews": previews,
		// Admin icazələri — Flutter input-u buna görə disable/enable edir.
		"permissions": parseGroupPermissions(conv.GroupPermissions),
		// 📌 Sabitlənmiş mesaj (null = pin yoxdur).
		"pinned_message": pinnedMessage,
		"created_at":     conv.CreatedAt,
	})
}

// POST /api/v1/groups/:conversation_id/members
func (h *GroupHandler) AddMembers(c *gin.Context) {
	requesterID := c.MustGet("user_id").(uint)
	convID, err := strconv.ParseUint(c.Param("conversation_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz conversation_id"})
		return
	}
	conversationID := uint(convID)

	var body struct {
		UserIDs []uint `json:"user_ids" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || len(body.UserIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_ids gerekli"})
		return
	}

	// Requester üye mi?
	var me models.ConversationParticipant
	err = database.DB.Where(
		"conversation_id = ? AND user_id = ? AND left_at IS NULL AND deleted_at IS NULL",
		conversationID, requesterID,
	).First(&me).Error
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "Bu grubun üyesi değilsiniz"})
		return
	}

	// Yalnız admin/owner üzv əlavə edə bilər.
	if me.Role != "owner" && me.Role != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Yalnızca yöneticiler üye ekleyebilir"})
		return
	}

	// 🔒 Dəvət icazəsi filtri (CreateGroup ilə eyni qayda):
	//   nobody → çıxarılır; following → əlavə edən adayın izlədikləri
	//   arasında deyilsə çıxarılır. Hamısı süzülürsə xəta qaytarılır.
	body.UserIDs = filterInvitableUsers(requesterID, body.UserIDs)
	if len(body.UserIDs) == 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error": "Seçilen kullanıcılar grup davetlerine izin vermiyor",
			"code":  "no_invitable_members",
		})
		return
	}

	var conv models.Conversation
	database.DB.Where("id = ?", conversationID).First(&conv)

	now := time.Now()
	added := []uint{}

	// ⚠️ ATOMİK limit: TRANSACTION + conversation FOR UPDATE kilidi.
	// Əvvəl COUNT→INSERT ayrı idi — paralel əlavə/qoşulmalarda yarış vəziyyəti
	// limiti aşırdı. İndi kilid altında təzə COUNT görülür, hər insert-dən
	// əvvəl yenidən yoxlanır (siyahının ortasında limit dolarsa qalanı atılır).
	txErr := database.DB.Transaction(func(tx *gorm.DB) error {
		var lockedConv models.Conversation
		if err := tx.Table("conversations").
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", conversationID).
			First(&lockedConv).Error; err != nil {
			return err
		}

		var currentCount int64
		if err := tx.Model(&models.ConversationParticipant{}).
			Where("conversation_id = ? AND left_at IS NULL AND deleted_at IS NULL", conversationID).
			Count(&currentCount).Error; err != nil {
			return err
		}
		if int(currentCount)+len(body.UserIDs) > lockedConv.MaxMembers {
			return errGroupFull
		}

		for _, uid := range body.UserIDs {
			if uid == requesterID {
				continue
			}

			// Zaten üye mi?
			var existing models.ConversationParticipant
			err := tx.Where(
				"conversation_id = ? AND user_id = ? AND left_at IS NULL AND deleted_at IS NULL",
				conversationID, uid,
			).First(&existing).Error
			if err == nil {
				continue // zaten üye
			}

			// Daha önce ayrılmış mı? → PENDING dəvət kimi yenilə.
			pendingStatus := "pending"
			var old models.ConversationParticipant
			err = tx.Unscoped().Where(
				"conversation_id = ? AND user_id = ?", conversationID, uid,
			).First(&old).Error
			if err == nil {
				if err := tx.Model(&old).Updates(map[string]interface{}{
					"left_at":       nil,
					"kicked_by":     nil,
					"deleted_at":    nil,
					"joined_at":     now,
					"invite_status": pendingStatus,
				}).Error; err != nil {
					return err
				}
			} else {
				participant := models.ConversationParticipant{
					ConversationID: conversationID,
					UserID:         uid,
					Role:           "member",
					JoinedAt:       &now,
					InviteStatus:   &pendingStatus,
				}
				if err := tx.Create(&participant).Error; err != nil {
					return err
				}
			}

			added = append(added, uid)
		}
		return nil
	})

	if txErr == errGroupFull {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Grup kapasitesi aşılıyor"})
		return
	}
	if txErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Üye eklenemedi"})
		return
	}

	// Bildirimlər TRANSACTION-dan SONRA (kilid tutularkən WS göndərmə).
	// PENDING dəvət — group_invited event-i (siyahıda "dəvət edildiniz").
	for _, uid := range added {
		h.wsHub.SendToUser(uid, "group_invited", gin.H{
			"conversation_id": conversationID,
			"group_name":      conv.GroupName,
			"invited_by":      requesterID,
		})
	}
	// FCM push (Laravel üzərindən): "X sizi Y qrupuna dəvət etdi".
	if len(added) > 0 {
		groupName := ""
		if conv.GroupName != nil {
			groupName = *conv.GroupName
		}
		go h.sendGroupInvitePush(requesterID, conversationID, groupName, added)
	}

	// Əlavə olunanların username-ləri — Flutter çatda "X qrupa qatıldı"
	// sistem sətri göstərsin.
	var addedUsernames []string
	if len(added) > 0 {
		database.DB.Raw(`SELECT username FROM users WHERE id IN (?)`, added).
			Pluck("username", &addedUsernames)
	}

	// Mevcut üyelere bildir
	memberIDs := getGroupParticipantIDs(conversationID)
	h.wsHub.SendToMultipleUsers(memberIDs, "group_members_added", gin.H{
		"conversation_id": conversationID,
		"added_user_ids":  added,
		"added_usernames": addedUsernames,
		"added_by":        requesterID,
	})

	c.JSON(http.StatusOK, gin.H{
		"message": "Üyeler eklendi",
		"added":   added,
	})
}

// PUT /api/v1/groups/:conversation_id
func (h *GroupHandler) UpdateGroup(c *gin.Context) {
	requesterID := c.MustGet("user_id").(uint)
	convID, err := strconv.ParseUint(c.Param("conversation_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz conversation_id"})
		return
	}
	conversationID := uint(convID)

	// Admin/owner kontrolü
	var me models.ConversationParticipant
	err = database.DB.Where(
		"conversation_id = ? AND user_id = ? AND left_at IS NULL AND deleted_at IS NULL",
		conversationID, requesterID,
	).First(&me).Error
	if err != nil || me.Role == "member" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Yetkiniz yok"})
		return
	}

	var body struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
		Avatar      *string `json:"avatar"` // S3 URL (Laravel-dən yüklənib gəlir)
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	updates := map[string]interface{}{}
	if body.Name != nil && *body.Name != "" {
		updates["group_name"] = *body.Name
	}
	if body.Description != nil {
		updates["group_desc"] = *body.Description
	}
	// Avatar: boş string göndərilsə silinir (NULL), dolu URL set edilir.
	if body.Avatar != nil {
		if *body.Avatar == "" {
			updates["group_avatar"] = nil
		} else {
			updates["group_avatar"] = *body.Avatar
		}
	}

	if len(updates) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Güncellenecek alan yok"})
		return
	}

	database.DB.Table("conversations").Where("id = ?", conversationID).Updates(updates)

	// Üyelere bildir
	memberIDs := getGroupParticipantIDs(conversationID)
	h.wsHub.SendToMultipleUsers(memberIDs, "group_updated", gin.H{
		"conversation_id": conversationID,
		"updates":         updates,
		"updated_by":      requesterID,
	})

	c.JSON(http.StatusOK, gin.H{"message": "Grup güncellendi"})
}
