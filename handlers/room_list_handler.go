package handlers

import (
	"net/http"
	"time"

	"beanpon_messenger/database"
	"beanpon_messenger/models"

	"github.com/gin-gonic/gin"
)

// GetMyRooms — GET /api/v1/rooms/my
// İstifadəçinin JOIN olduğu otaqlar, "Söhbətlər" siyahısı üçün (group chat
// paritesi). Hər sətir: son mesaj önizləmə + unread + mute/archive/pin.
// ?archived=all → arxivlənmişlər də daxil (iOS getMyGroups kimi).
func (h *RoomHandler) GetMyRooms(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	if isGuest(userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Qonaq hesab", "code": "GUEST_FORBIDDEN"})
		return
	}
	archived := c.Query("archived") // "" | "all" | "only"

	var members []models.RoomMember
	q := database.DB.Where("user_id = ?", userID)
	switch archived {
	case "all":
		// hamısı
	case "only":
		q = q.Where("is_archived = ?", true)
	default:
		q = q.Where("is_archived = ?", false)
	}
	q.Find(&members)

	// SELF-HEAL: yaradan HƏMİŞƏ "Söhbətlər"də görünməlidir. Member satırı hər
	// hansı səbəbdən yoxdursa (köhnə data / insert xətası) sintetik owner sətri
	// əlavə et ki, owner öz otağını siyahıda görsün. "only" (arxiv) rejimində
	// əlavə etmə — owner sətri arxivlənməyib.
	if archived != "only" {
		haveRoom := map[uint]bool{}
		for _, m := range members {
			haveRoom[m.RoomID] = true
		}
		var owned []models.ChatRoom
		database.DB.Where("owner_id = ?", userID).Find(&owned)
		now := time.Now()
		for _, r := range owned {
			if haveRoom[r.ID] {
				continue
			}
			members = append(members, models.RoomMember{
				RoomID: r.ID, UserID: userID, Role: "owner", JoinedAt: &now,
			})
		}
	}

	if len(members) == 0 {
		c.JSON(http.StatusOK, gin.H{"rooms": []models.RoomListItem{}})
		return
	}

	roomIDs := make([]uint, 0, len(members))
	memByRoom := map[uint]models.RoomMember{}
	for _, m := range members {
		roomIDs = append(roomIDs, m.RoomID)
		memByRoom[m.RoomID] = m
	}

	// Otaqlar (silinmiş/dondurulmuş olmayan — frozen siyahıda görünməsin). Hidden
	// rooms, rooms whose owner is blocked with the user and rooms the user is
	// banned from never show.
	var rooms []models.ChatRoom
	database.DB.Where("id IN ? AND is_frozen = ?", roomIDs, false).
		Where(roomOwnerNotBlockedSQL, userID, userID).
		Where(roomNotHiddenSQL, userID).
		Where(roomNotBannedSQL, userID).
		Find(&rooms)

	// My write blocks in these rooms (one query).
	writeBlocked := map[uint]bool{}
	var blockedRoomIDs []uint
	database.DB.Table("room_write_blocks").
		Where("user_id = ? AND room_id IN ?", userID, roomIDs).
		Pluck("room_id", &blockedRoomIDs)
	for _, id := range blockedRoomIDs {
		writeBlocked[id] = true
	}

	// Üzv sayıları (bir sorğu).
	type cnt struct {
		RoomID uint
		N      int
	}
	var counts []cnt
	database.DB.Model(&models.RoomMember{}).
		Select("room_id, COUNT(*) as n").
		Where("room_id IN ?", roomIDs).Group("room_id").Scan(&counts)
	memberCount := map[uint]int{}
	for _, c2 := range counts {
		memberCount[c2.RoomID] = c2.N
	}

	out := make([]models.RoomListItem, 0, len(rooms))
	for _, r := range rooms {
		mem := memByRoom[r.ID]

		// Son mesaj (block filtrli — bloklu göndərənin mesajını önizləməyə qoyma).
		var last struct {
			EncryptedText string
			SenderUser    string
			CreatedAt     time.Time
		}
		database.DB.Raw(`
			SELECT m.encrypted_text, u.username AS sender_user, m.created_at
			FROM messages m JOIN users u ON u.id = m.sender_id
			WHERE m.room_id = ? AND m.deleted_at IS NULL
			  AND NOT EXISTS (
			      SELECT 1 FROM user_blocks ub
			      WHERE (ub.blocker_id = ? AND ub.blocked_id = m.sender_id)
			         OR (ub.blocker_id = m.sender_id AND ub.blocked_id = ?)
			  )
			ORDER BY m.created_at DESC, m.id DESC LIMIT 1
		`, r.ID, userID, userID).Scan(&last)

		var lastText, lastSender *string
		var lastAt *time.Time
		if !last.CreatedAt.IsZero() {
			t, _ := h.encryptionService.DecryptMessage(last.EncryptedText)
			lastText = &t
			s := last.SenderUser
			lastSender = &s
			ca := last.CreatedAt
			lastAt = &ca
		}

		// Unread: son oxumadan sonra, başqasının göndərdiyi, bloklu olmayan.
		unread := 0
		var afterArg interface{} = time.Unix(0, 0)
		if mem.LastReadAt != nil {
			afterArg = *mem.LastReadAt
		}
		database.DB.Raw(`
			SELECT COUNT(*) FROM messages m
			WHERE m.room_id = ? AND m.deleted_at IS NULL
			  AND m.sender_id <> ?
			  AND m.created_at > ?
			  AND NOT EXISTS (
			      SELECT 1 FROM user_blocks ub
			      WHERE (ub.blocker_id = ? AND ub.blocked_id = m.sender_id)
			         OR (ub.blocker_id = m.sender_id AND ub.blocked_id = ?)
			  )
		`, r.ID, userID, afterArg, userID, userID).Scan(&unread)

		out = append(out, models.RoomListItem{
			ID:              r.ID,
			Name:            r.Name,
			Avatar:          r.Avatar,
			MyRole:          mem.Role,
			MemberCount:     memberCount[r.ID],
			IsFrozen:        r.IsFrozen,
			LastMessageText: lastText,
			LastSender:      lastSender,
			LastMessageAt:   lastAt,
			UnreadCount:     unread,
			IsMuted:         mem.IsMuted,
			IsArchived:      mem.IsArchived,
			IsPinned:        mem.IsPinned,
			PinnedAt:        mem.PinnedAt,
			// Settings + my write state.
			ScreenshotDisabled:   r.ScreenshotDisabled,
			MessagingLocked:      r.IsMessagingLocked(time.Now()),
			MessagingLockedUntil: lockUntilIfActive(r),
			WriteBlocked:         writeBlocked[r.ID] && mem.Role != "owner" && mem.Role != "admin",
		})
	}

	c.JSON(http.StatusOK, gin.H{"rooms": out})
}

// roomPushTargets — push bildirişi göndəriləcək üzvlər: JOIN olmuş, muted
// OLMAYAN (muted_until keçibsə mute sayılmır), göndərənlə bloklu olmayan,
// göndərənin özü olmayan. Group chat push məntiqi ilə eyni. Also skipped:
// members who hid the room and members blocked with the room owner (for them
// the room does not exist).
func (h *RoomHandler) roomPushTargets(roomID, senderID uint) []uint {
	var ids []uint
	database.DB.Raw(`
		SELECT rm.user_id FROM room_members rm
		JOIN rooms r ON r.id = rm.room_id
		WHERE rm.room_id = ?
		  AND rm.user_id <> ?
		  AND (rm.is_muted = false OR (rm.muted_until IS NOT NULL AND rm.muted_until < NOW()))
		  AND NOT EXISTS (
		      SELECT 1 FROM user_blocks ub
		      WHERE (ub.blocker_id = rm.user_id AND ub.blocked_id = ?)
		         OR (ub.blocker_id = ? AND ub.blocked_id = rm.user_id)
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM room_hides rh WHERE rh.room_id = rm.room_id AND rh.user_id = rm.user_id
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM room_bans rb WHERE rb.room_id = rm.room_id AND rb.user_id = rm.user_id
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM user_blocks ob
		      WHERE (ob.blocker_id = rm.user_id AND ob.blocked_id = r.owner_id)
		         OR (ob.blocker_id = r.owner_id AND ob.blocked_id = rm.user_id)
		  )
	`, roomID, senderID, senderID, senderID).Scan(&ids)
	return ids
}

// POST /api/v1/rooms/:room_id/mark-read — otağı oxundu işarələ.
func (h *RoomHandler) MarkRoomRead(c *gin.Context) {
	userID := c.MustGet("user_id").(uint)
	roomID := parseRoomID(c)
	now := time.Now()
	// Üzv deyilsə (yalnız oxuyan) sessizcə keç — oxundu üzvlük tələb edir.
	database.DB.Model(&models.RoomMember{}).
		Where("room_id = ? AND user_id = ?", roomID, userID).
		Updates(map[string]interface{}{"last_read_at": now})
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// POST /api/v1/rooms/:room_id/mute  (body {duration_minutes?}) ve /unmute
func (h *RoomHandler) MuteRoom(c *gin.Context) {
	h.setMuteRoom(c, true)
}
func (h *RoomHandler) UnmuteRoom(c *gin.Context) {
	h.setMuteRoom(c, false)
}
func (h *RoomHandler) setMuteRoom(c *gin.Context, mute bool) {
	userID := c.MustGet("user_id").(uint)
	roomID := parseRoomID(c)
	upd := map[string]interface{}{"is_muted": mute, "muted_until": nil}
	if mute {
		var body struct {
			DurationMinutes *int `json:"duration_minutes"`
		}
		_ = c.ShouldBindJSON(&body)
		if body.DurationMinutes != nil && *body.DurationMinutes > 0 {
			until := time.Now().Add(time.Duration(*body.DurationMinutes) * time.Minute)
			upd["muted_until"] = until
		}
	}
	database.DB.Model(&models.RoomMember{}).
		Where("room_id = ? AND user_id = ?", roomID, userID).Updates(upd)
	c.JSON(http.StatusOK, gin.H{"status": "ok", "muted": mute})
}

// POST /api/v1/rooms/:room_id/archive  ve /unarchive
func (h *RoomHandler) ArchiveRoom(c *gin.Context)   { h.setArchiveRoom(c, true) }
func (h *RoomHandler) UnarchiveRoom(c *gin.Context) { h.setArchiveRoom(c, false) }
func (h *RoomHandler) setArchiveRoom(c *gin.Context, arch bool) {
	userID := c.MustGet("user_id").(uint)
	roomID := parseRoomID(c)
	upd := map[string]interface{}{"is_archived": arch, "archived_at": nil}
	if arch {
		upd["archived_at"] = time.Now()
	}
	database.DB.Model(&models.RoomMember{}).
		Where("room_id = ? AND user_id = ?", roomID, userID).Updates(upd)
	c.JSON(http.StatusOK, gin.H{"status": "ok", "archived": arch})
}

// POST /api/v1/rooms/:room_id/pin  ve /unpin
func (h *RoomHandler) PinRoom(c *gin.Context)   { h.setPinRoom(c, true) }
func (h *RoomHandler) UnpinRoom(c *gin.Context) { h.setPinRoom(c, false) }
func (h *RoomHandler) setPinRoom(c *gin.Context, pin bool) {
	userID := c.MustGet("user_id").(uint)
	roomID := parseRoomID(c)
	upd := map[string]interface{}{"is_pinned": pin, "pinned_at": nil}
	if pin {
		upd["pinned_at"] = time.Now()
	}
	database.DB.Model(&models.RoomMember{}).
		Where("room_id = ? AND user_id = ?", roomID, userID).Updates(upd)
	c.JSON(http.StatusOK, gin.H{"status": "ok", "pinned": pin})
}
