package handlers

import (
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"beanpon_messenger/database"
	"beanpon_messenger/models"
	"beanpon_messenger/utils"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
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
		Avatar:         req.Avatar,
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
	if _, err := insertRoomMember(room.ID, userID, "owner"); err != nil {
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
	// A deleted / unknown room is a clear 404 instead of a foreign-key 500.
	var room models.ChatRoom
	if err := database.DB.Select("id").First(&room, roomID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Otaq tapılmadı", "code": "ROOM_NOT_FOUND"})
		return
	}
	log.Printf("[Room] JoinRoom start room=%d user=%d", roomID, userID)
	if err := h.ensureMember(roomID, userID); err != nil {
		log.Printf("[Room] JoinRoom ensureMember err room=%d user=%d: %v", roomID, userID, err)
		// DEBUG: gerçek hatayı response'da da döndür (geçici teşhis için).
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Qoşulma alınmadı", "debug": err.Error()})
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
// join). Təkrar çağırış təsirsizdir. The row is read back after the insert, so a
// silently skipped insert surfaces as an error instead of a fake "joined".
func (h *RoomHandler) ensureMember(roomID, userID uint) error {
	if roomMembership(roomID, userID) != nil {
		return nil
	}
	inserted, err := insertRoomMember(roomID, userID, "member")
	if err != nil {
		log.Printf("[Room] ensureMember insert FAILED room=%d user=%d: %v", roomID, userID, err)
		return err
	}
	if roomMembership(roomID, userID) == nil {
		log.Printf("[Room] ensureMember: no membership row after insert room=%d user=%d", roomID, userID)
		return errMembershipNotSaved
	}
	if inserted {
		database.DB.Model(&models.ChatRoom{}).Where("id = ?", roomID).
			UpdateColumn("join_count", gorm.Expr("join_count + 1"))
		h.bumpScore(roomID)
	}
	return nil
}

var errMembershipNotSaved = errors.New("room membership row was not saved")

// insertRoomMember writes a room_members row with the core columns only — the
// per-user columns (is_muted, is_archived, is_pinned, …) take their DB defaults,
// so the insert does not depend on every model column existing. The target-less
// ON CONFLICT DO NOTHING absorbs a concurrent duplicate and, unlike
// ON CONFLICT (room_id, user_id), does not require that unique index to exist.
// inserted=false → the row was already there.
func insertRoomMember(roomID, userID uint, role string) (inserted bool, err error) {
	now := time.Now()
	res := database.DB.Exec(`
		INSERT INTO room_members (room_id, user_id, role, joined_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING`, roomID, userID, role, now, now, now)
	return res.RowsAffected > 0, res.Error
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

// UpdateRoom — PUT /api/v1/rooms/:room_id. Admin otaq redaktəsi (ad/təsvir/
// avatar/tarixçə). Yalnız göndərilən (non-nil) sahələr dəyişir.
func (h *RoomHandler) UpdateRoom(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	roomID := parseRoomID(c)
	if !h.requireAdmin(c, roomID, userID) {
		return
	}
	var req models.UpdateRoomRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Yanlış məlumat"})
		return
	}

	updates := map[string]interface{}{}
	if req.Name != nil && *req.Name != "" {
		updates["name"] = *req.Name
	}
	if req.Description != nil {
		updates["description"] = *req.Description
	}
	if req.Avatar != nil {
		updates["avatar"] = *req.Avatar
	}
	if req.HistoryVisible != nil {
		updates["history_visible"] = *req.HistoryVisible
	}
	if len(updates) > 0 {
		database.DB.Model(&models.ChatRoom{}).Where("id = ?", roomID).Updates(updates)
	}

	var room models.ChatRoom
	if err := database.DB.First(&room, roomID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Otaq tapılmadı"})
		return
	}
	role := roomMembership(roomID, userID)
	var rolePtr *string
	if role != nil {
		rolePtr = &role.Role
	}
	// Üzvlərə WS bildir (client başlıq/avatar yeniləsin).
	h.wsHub.SendToMultipleUsers(h.roomMemberIDs(roomID), "room_updated", gin.H{
		"room_id":     roomID,
		"name":        room.Name,
		"description": room.Description,
		"avatar":      room.Avatar,
	})
	c.JSON(http.StatusOK, gin.H{"data": h.toRoomResponse(room, rolePtr, role != nil)})
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

// GET /api/v1/rooms/:room_id/members — otaq üzvləri (detay səhifəsi üçün).
// Group GetMembers ikizi: `{members: [...]}`. İki yönlü block cascade — cari
// istifadəçi ilə bloklu üzvlər siyahıda görünmür. Üzvlük tələb olunmur (oxuyan
// da detay aça bilər), amma guest yox.
func (h *RoomHandler) GetRoomMembers(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	if isGuest(userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Qonaq hesab", "code": "GUEST_FORBIDDEN"})
		return
	}
	roomID := parseRoomID(c)

	type row struct {
		UserID       uint       `gorm:"column:user_id"`
		Name         string     `gorm:"column:name"`
		Username     string     `gorm:"column:username"`
		IsVerified   bool       `gorm:"column:is_verified"`
		ProfileImage *string    `gorm:"column:profile_image"`
		Role         string     `gorm:"column:role"`
		JoinedAt     *time.Time `gorm:"column:joined_at"`
	}
	var rows []row
	database.DB.Raw(`
		SELECT rm.user_id, u.name, u.username, u.is_verified,
		       p.profile_image, rm.role, rm.joined_at
		FROM room_members rm
		JOIN users u ON u.id = rm.user_id
		LEFT JOIN profiles p ON p.user_id = rm.user_id
		WHERE rm.room_id = ?
		  AND NOT EXISTS (
		      SELECT 1 FROM user_blocks ub
		      WHERE (ub.blocker_id = ? AND ub.blocked_id = rm.user_id)
		         OR (ub.blocker_id = rm.user_id AND ub.blocked_id = ?)
		  )
		ORDER BY
			CASE rm.role WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END,
			rm.joined_at ASC NULLS LAST
	`, roomID, userID, userID).Scan(&rows)

	out := make([]models.RoomMemberResponse, 0, len(rows))
	for _, r := range rows {
		out = append(out, models.RoomMemberResponse{
			UserID:       r.UserID,
			Name:         r.Name,
			Username:     r.Username,
			IsVerified:   r.IsVerified,
			ProfileImage: utils.PrependBaseURL(r.ProfileImage),
			Role:         r.Role,
			JoinedAt:     r.JoinedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"members": out})
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
