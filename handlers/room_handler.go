package handlers

import (
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"beanpon_messenger/database"
	"beanpon_messenger/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// RoomHandler — açıq (public) söhbət otaqları. Qrup çatının public variantı.
// Oxumaq üçün üzvlük tələb olunmur; yazmaq üçün tək-klik join (mesaj göndərmə
// anında avtomatik üzv olunur). Owner + adminlər moderasiya edir.
type RoomHandler struct {
	wsHub interface {
		IsUserOnline(userID uint) bool
		SendToUser(userID uint, messageType string, data interface{})
		SendToMultipleUsers(userIDs []uint, messageType string, data interface{})
		// Qrup push helper-ini otaqlar üçün də istifadə edirik (eyni FCM axını).
		ScheduleGroupPushNotification(conversationID, senderID uint, groupName, message, messageID string, memberIDs []uint, delay time.Duration)
	}
	encryptionService interface {
		EncryptMessage(plainText string) (string, error)
		DecryptMessage(encryptedText string) (string, error)
	}
}

func NewRoomHandler(wsHub interface {
	IsUserOnline(userID uint) bool
	SendToUser(userID uint, messageType string, data interface{})
	SendToMultipleUsers(userIDs []uint, messageType string, data interface{})
	ScheduleGroupPushNotification(conversationID, senderID uint, groupName, message, messageID string, memberIDs []uint, delay time.Duration)
}, encryptionService interface {
	EncryptMessage(plainText string) (string, error)
	DecryptMessage(encryptedText string) (string, error)
}) *RoomHandler {
	return &RoomHandler{wsHub: wsHub, encryptionService: encryptionService}
}

// roomCreateLimit — bir istifadəçinin eyni anda sahib ola biləcəyi aktiv otaq
// sayı. Filament/config bunu `app_settings`-dən idarə edir; env fallback.
func roomCreateLimit() int {
	// Laravel app_settings → key 'room_create_limit' (varsa), yoxdursa env,
	// o da yoxdursa 5 (default).
	var row struct{ Value *string }
	if err := database.DB.Table("app_settings").
		Select("value").Where("key = ?", "room_create_limit").Scan(&row).Error; err == nil && row.Value != nil {
		if n, e := strconv.Atoi(*row.Value); e == nil && n > 0 {
			return n
		}
	}
	if v := os.Getenv("ROOM_CREATE_LIMIT"); v != "" {
		if n, e := strconv.Atoi(v); e == nil && n > 0 {
			return n
		}
	}
	return 5
}

// isGuest — istifadəçi guest-dir? (guest otaq yarada bilməz, oxuya bilməz).
func isGuest(userID uint) bool {
	var g bool
	database.DB.Table("users").Select("is_guest").Where("id = ?", userID).Scan(&g)
	return g
}

// roomMembership — istifadəçinin otaqdakı üzvlüyü (yoxdursa nil).
func roomMembership(roomID, userID uint) *models.RoomMember {
	var m models.RoomMember
	if err := database.DB.Where("room_id = ? AND user_id = ?", roomID, userID).First(&m).Error; err != nil {
		return nil
	}
	return &m
}

// recomputeScore — karma skoru: join + mesaj + təzəlik. Ağırlıqlar env-dən.
// score = join_count*w1 + message_count*w2, son aktivlikdən keçən saata görə
// üstəl azalma (decay). Hər aktivlikdə yenilənir (list sorğusu üçün hazır).
func recomputeScore(r *models.ChatRoom) float64 {
	w1 := envFloat("ROOM_SCORE_JOIN_WEIGHT", 2.0)
	w2 := envFloat("ROOM_SCORE_MSG_WEIGHT", 1.0)
	base := float64(r.JoinCount)*w1 + float64(r.MessageCount)*w2
	// Təzəlik: son aktivlikdən keçən saat artdıqca skor azalır (yarı-ömür 48 saat).
	if r.LastActivityAt != nil {
		hours := time.Since(*r.LastActivityAt).Hours()
		if hours < 0 {
			hours = 0
		}
		decay := 1.0 / (1.0 + hours/48.0)
		return base * decay
	}
	return base
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

// POST /api/v1/rooms — otaq yarat (guest olmaz, 5-otaq limiti).
func (h *RoomHandler) CreateRoom(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	if isGuest(userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Qonaq hesab otaq yarada bilməz", "code": "GUEST_FORBIDDEN"})
		return
	}

	var req models.CreateRoomRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Yanlış məlumat"})
		return
	}

	// Aktiv otaq limiti (silinməmiş, owner).
	var active int64
	database.DB.Model(&models.ChatRoom{}).Where("owner_id = ?", userID).Count(&active)
	if int(active) >= roomCreateLimit() {
		c.JSON(http.StatusForbidden, gin.H{"error": "Otaq limitinə çatdınız", "code": "ROOM_LIMIT"})
		return
	}

	historyVisible := true
	if req.HistoryVisible != nil {
		historyVisible = *req.HistoryVisible
	}

	now := time.Now()
	room := models.ChatRoom{
		OwnerID:        userID,
		Name:           req.Name,
		Description:    req.Description,
		HistoryVisible: historyVisible,
		JoinCount:      1,
		LastActivityAt: &now,
	}
	room.Score = recomputeScore(&room)

	if err := database.DB.Create(&room).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Otaq yaradılmadı"})
		return
	}

	// Owner avtomatik üzv. Xəta yutulmasın — logla (member satırı yazılmasa
	// otaq "Söhbətlər"də görünməz və owner "üzv deyil" kimi görünər).
	if err := database.DB.Create(&models.RoomMember{
		RoomID: room.ID, UserID: userID, Role: "owner", JoinedAt: &now,
	}).Error; err != nil {
		log.Printf("[Room] owner member insert FAILED room=%d user=%d: %v", room.ID, userID, err)
	}

	c.JSON(http.StatusOK, gin.H{"data": h.toRoomResponse(room, strPtr("owner"), true)})
}

// GET /api/v1/rooms — otaq siyahısı, karma sıralı. Guest oxuya bilməz.
func (h *RoomHandler) ListRooms(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	if isGuest(userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Qonaq hesab otaqları görə bilməz", "code": "GUEST_FORBIDDEN"})
		return
	}

	limit := 30
	if v := c.Query("limit"); v != "" {
		if n, e := strconv.Atoi(v); e == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	offset := 0
	if v := c.Query("offset"); v != "" {
		if n, e := strconv.Atoi(v); e == nil && n >= 0 {
			offset = n
		}
	}

	var rooms []models.ChatRoom
	// Dondurulmuş otaqlar siyahıda görünmür (yalnız admin idarəçiliyi).
	database.DB.Where("is_frozen = ?", false).
		Order("score DESC, last_activity_at DESC NULLS LAST").
		Limit(limit).Offset(offset).Find(&rooms)

	// Üzvlük xəritəsi (bir sorğu).
	roomIDs := make([]uint, 0, len(rooms))
	for _, r := range rooms {
		roomIDs = append(roomIDs, r.ID)
	}
	memberRole := map[uint]string{}
	if len(roomIDs) > 0 {
		var mems []models.RoomMember
		database.DB.Where("room_id IN ? AND user_id = ?", roomIDs, userID).Find(&mems)
		for _, m := range mems {
			memberRole[m.RoomID] = m.Role
		}
	}

	out := make([]models.RoomResponse, 0, len(rooms))
	for _, r := range rooms {
		var rolePtr *string
		isMember := false
		if role, ok := memberRole[r.ID]; ok {
			rolePtr = strPtr(role)
			isMember = true
		} else if r.OwnerID == userID {
			// Yaradan HƏMİŞƏ üzvdür — member satırı hər hansı səbəbdən yoxdursa
			// belə owner kimi göstər (self-heal).
			rolePtr = strPtr("owner")
			isMember = true
		}
		out = append(out, h.toRoomResponse(r, rolePtr, isMember))
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// GET /api/v1/rooms/:room_id — otaq detayı.
func (h *RoomHandler) GetRoom(c *gin.Context) {
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
	mem := roomMembership(roomID, userID)
	var rolePtr *string
	if mem != nil {
		rolePtr = strPtr(mem.Role)
	}
	c.JSON(http.StatusOK, gin.H{"data": h.toRoomResponse(room, rolePtr, mem != nil)})
}

// POST /api/v1/rooms/:room_id/join — tək-klik join (idempotent).
func (h *RoomHandler) JoinRoom(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	if isGuest(userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Qonaq hesab", "code": "GUEST_FORBIDDEN"})
		return
	}
	roomID := parseRoomID(c)
	if err := h.ensureMember(roomID, userID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Qoşulma alınmadı"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// POST /api/v1/rooms/:room_id/leave — otaqdan çıx (owner çıxa bilməz).
func (h *RoomHandler) LeaveRoom(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	roomID := parseRoomID(c)
	mem := roomMembership(roomID, userID)
	if mem == nil {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
		return
	}
	if mem.IsOwner() {
		c.JSON(http.StatusForbidden, gin.H{"error": "Sahib otaqdan çıxa bilməz, otağı silin"})
		return
	}
	database.DB.Delete(&models.RoomMember{}, mem.ID)
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// ensureMember — üzv deyilsə member kimi əlavə et + join_count artır (tək-klik
// join). Təkrar çağırış təsirsizdir.
func (h *RoomHandler) ensureMember(roomID, userID uint) error {
	if roomMembership(roomID, userID) != nil {
		return nil
	}
	now := time.Now()
	res := database.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&models.RoomMember{
		RoomID: roomID, UserID: userID, Role: "member", JoinedAt: &now,
	})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected > 0 {
		database.DB.Model(&models.ChatRoom{}).Where("id = ?", roomID).
			UpdateColumn("join_count", gorm.Expr("join_count + 1"))
		h.bumpScore(roomID)
	}
	return nil
}

// bumpScore — otağın skorunu təzədən hesabla (sayğac dəyişəndən sonra).
func (h *RoomHandler) bumpScore(roomID uint) {
	var r models.ChatRoom
	if err := database.DB.First(&r, roomID).Error; err != nil {
		return
	}
	database.DB.Model(&models.ChatRoom{}).Where("id = ?", roomID).
		UpdateColumn("score", recomputeScore(&r))
}

// --- Admin aksiyonları ---

// POST /api/v1/rooms/:room_id/freeze — dondur / aç (owner|admin).
func (h *RoomHandler) FreezeRoom(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	roomID := parseRoomID(c)
	if !h.requireAdmin(c, roomID, userID) {
		return
	}
	var body struct {
		Frozen bool `json:"frozen"`
	}
	_ = c.ShouldBindJSON(&body)
	database.DB.Model(&models.ChatRoom{}).Where("id = ?", roomID).
		UpdateColumn("is_frozen", body.Frozen)
	// Üzvlərə WS bildir (client ekranı kilidləsin).
	h.wsHub.SendToMultipleUsers(h.roomMemberIDs(roomID), "room_frozen", gin.H{
		"room_id": roomID, "frozen": body.Frozen,
	})
	c.JSON(http.StatusOK, gin.H{"status": "ok", "frozen": body.Frozen})
}

// DELETE /api/v1/rooms/:room_id — otağı sil (yalnız owner).
func (h *RoomHandler) DeleteRoom(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	roomID := parseRoomID(c)
	mem := roomMembership(roomID, userID)
	if mem == nil || !mem.IsOwner() {
		c.JSON(http.StatusForbidden, gin.H{"error": "Yalnız sahib otağı silə bilər"})
		return
	}
	memberIDs := h.roomMemberIDs(roomID)
	// Otaq + üzvlər + mesajlar (soft delete).
	database.DB.Where("room_id = ?", roomID).Delete(&models.Message{})
	database.DB.Where("room_id = ?", roomID).Delete(&models.RoomMember{})
	database.DB.Delete(&models.ChatRoom{}, roomID)
	h.wsHub.SendToMultipleUsers(memberIDs, "room_deleted", gin.H{"room_id": roomID})
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// PUT /api/v1/rooms/:room_id/admin/:user_id — admin et / geri al (yalnız owner).
func (h *RoomHandler) SetAdmin(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	roomID := parseRoomID(c)
	mem := roomMembership(roomID, userID)
	if mem == nil || !mem.IsOwner() {
		c.JSON(http.StatusForbidden, gin.H{"error": "Yalnız sahib admin təyin edə bilər"})
		return
	}
	targetID64, _ := strconv.ParseUint(c.Param("user_id"), 10, 32)
	targetID := uint(targetID64)
	var body struct {
		Admin bool `json:"admin"`
	}
	_ = c.ShouldBindJSON(&body)
	// Hədəf üzv deyilsə əvvəlcə üzv et.
	if err := h.ensureMember(roomID, targetID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Alınmadı"})
		return
	}
	role := "member"
	if body.Admin {
		role = "admin"
	}
	// Owner rolunu dəyişmə (owner həmişə owner).
	database.DB.Model(&models.RoomMember{}).
		Where("room_id = ? AND user_id = ? AND role <> 'owner'", roomID, targetID).
		UpdateColumn("role", role)
	c.JSON(http.StatusOK, gin.H{"status": "ok", "role": role})
}

// requireAdmin — owner|admin deyilsə 403 yazıb false qaytarır.
func (h *RoomHandler) requireAdmin(c *gin.Context, roomID, userID uint) bool {
	mem := roomMembership(roomID, userID)
	if mem == nil || !mem.HasAdminAccess() {
		c.JSON(http.StatusForbidden, gin.H{"error": "İcazə yoxdur"})
		return false
	}
	return true
}

// roomMemberIDs — otağın bütün üzv id-ləri (WS fan-out üçün).
func (h *RoomHandler) roomMemberIDs(roomID uint) []uint {
	var ids []uint
	database.DB.Model(&models.RoomMember{}).Where("room_id = ?", roomID).Pluck("user_id", &ids)
	return ids
}

// --- helpers ---

func (h *RoomHandler) toRoomResponse(r models.ChatRoom, myRole *string, isMember bool) models.RoomResponse {
	return models.RoomResponse{
		ID:             r.ID,
		Name:           r.Name,
		Description:    r.Description,
		Avatar:         r.Avatar,
		OwnerID:        r.OwnerID,
		IsFrozen:       r.IsFrozen,
		HistoryVisible: r.HistoryVisible,
		JoinCount:      r.JoinCount,
		MessageCount:   r.MessageCount,
		MyRole:         myRole,
		IsMember:       isMember,
		LastActivityAt: r.LastActivityAt,
		CreatedAt:      r.CreatedAt,
	}
}

func parseRoomID(c *gin.Context) uint {
	id, _ := strconv.ParseUint(c.Param("room_id"), 10, 32)
	return uint(id)
}

func strPtr(s string) *string { return &s }

// newRoomMessageID — yeni UUID v4 (client_message_id yoxdursa).
func newRoomMessageID() string { return uuid.NewString() }
