package websocket

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"beanpon_messenger/database"
	"beanpon_messenger/models"
	"beanpon_messenger/utils"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// MAFIA OYUNU — Handler + WebSocket event-ləri
//
// REST:  POST /api/v1/live-rooms/:room_id/mafia/start  (yalnız host)
// WS:    mafia_ready, mafia_night_action, mafia_vote, mafia_defense_end,
//        mafia_cancel  →  live_hub.handleEvent içindən bura yönləndirilir.
// ============================================================================

// --- DB köməkçiləri ---

// saveGame — MafiaGame-i live_rooms.active_game jsonb sahəsinə yazır.
func saveGame(roomID uint, g *MafiaGame) {
	raw, err := MarshalGame(g)
	if err != nil {
		return
	}
	database.DB.Exec("UPDATE live_rooms SET active_game = ? WHERE id = ?", string(raw), roomID)
}

// loadGame — live_rooms.active_game-dən MafiaGame oxuyur.
func loadGame(roomID uint) (*MafiaGame, bool) {
	var room models.LiveRoom
	if err := database.DB.Select("id, active_game").First(&room, roomID).Error; err != nil {
		return nil, false
	}
	if room.ActiveGame == nil {
		return nil, false
	}
	return UnmarshalGame(*room.ActiveGame)
}

// clearGame — oyunu bitirib active_game = NULL edir.
func clearGame(roomID uint) {
	database.DB.Exec("UPDATE live_rooms SET active_game = NULL WHERE id = ?", roomID)
}

// systemChatMessage — sistem adından otaqdakı hər kəsə elan göndərir
// (gecə/gündüz, ölüm, qalib və s. — plan §6). DB-yə yazılmır (sender_id=0
// users FK-nı pozardı); bütün hadisələr onsuz da game.History-də saxlanır,
// reconnect olan adam state_sync ilə tarixçəni alır. Flutter bu event-i
// chat-da "Sistem" mesajı kimi göstərir.
func (h *LiveHub) systemChatMessage(roomID uint, text string) {
	data, _ := json.Marshal(map[string]interface{}{
		"text":       text,
		"sender_id":  0,
		"is_system":  true,
		"created_at": time.Now().UTC(),
	})
	payload, _ := json.Marshal(map[string]interface{}{
		"type":    "mafia_system_message",
		"room_id": roomID,
		"data":    json.RawMessage(data),
	})
	h.broadcastRaw(roomID, payload)
}

// broadcastRaw — hazır payload-u otaqdakı hər kəsə göndərir.
func (h *LiveHub) broadcastRaw(roomID uint, payload []byte) {
	h.mu.RLock()
	roomClients, ok := h.rooms[roomID]
	h.mu.RUnlock()
	if !ok {
		return
	}
	for _, c := range roomClients {
		select {
		case c.Send <- payload:
		default:
		}
	}
}

// broadcastPublicState — hər oyunçuya/izləyiciyə maskalanmış vəziyyəti
// fərdi göndərir (mafia komandasını görsün, başqası yox).
func (h *LiveHub) broadcastPublicState(roomID uint, g *MafiaGame, eventType string) {
	h.mu.RLock()
	roomClients, ok := h.rooms[roomID]
	h.mu.RUnlock()
	if !ok {
		return
	}
	for _, c := range roomClients {
		state := g.buildPublicState(c.UserID)
		data, _ := json.Marshal(state)
		payload, _ := json.Marshal(map[string]interface{}{
			"type":    eventType,
			"room_id": roomID,
			"data":    json.RawMessage(data),
		})
		select {
		case c.Send <- payload:
		default:
		}
	}
}

// sendToUser — yalnız müəyyən user-ə fərdi mesaj (rol/dedektiv nəticəsi).
func (h *LiveHub) sendToUser(roomID, userID uint, payload []byte) {
	h.mu.RLock()
	roomClients, ok := h.rooms[roomID]
	var c *LiveRoomClient
	if ok {
		c = roomClients[userID]
	}
	h.mu.RUnlock()
	if c != nil {
		select {
		case c.Send <- payload:
		default:
		}
	}
}

// ============================================================================
// REST: Oyunu başlat (yalnız host)
// POST /api/v1/live-rooms/:room_id/mafia/start
// ============================================================================
func (h *LiveHub) StartMafia(c *gin.Context) {
	userIDVal, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	userID := userIDVal.(uint)

	roomID64, err := strconv.ParseUint(c.Param("room_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid room_id"})
		return
	}
	roomID := uint(roomID64)

	// Otağı yoxla — yalnız host
	var room models.LiveRoom
	if err := database.DB.Select("id, host_user_id, status, active_game").First(&room, roomID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Otaq tapılmadı"})
		return
	}
	if room.HostUserID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "Yalnız host oyunu başlada bilər"})
		return
	}
	if room.Status != "live" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Otaq aktiv deyil"})
		return
	}
	// Artıq oyun varsa
	if room.ActiveGame != nil && string(*room.ActiveGame) != "null" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Hazırda oyun davam edir"})
		return
	}

	// Oyunçuları yığ: yalnız host + broadcaster (active), otaqda online olanlar
	h.mu.RLock()
	roomClients := h.rooms[roomID]
	var players []*MafiaPlayer
	for _, cl := range roomClients {
		if cl.IsGhost || cl.LiveSpam {
			continue
		}
		if cl.Role == "host" || cl.Role == "broadcaster" {
			avatar := cl.Avatar
			if cl.AvatarType == nil || *cl.AvatarType != "gif" {
				avatar = utils.PrependBaseURL(cl.Avatar)
			}
			players = append(players, &MafiaPlayer{
				UserID: cl.UserID,
				Name:   cl.Name,
				Avatar: avatar,
				Alive:  true,
			})
		}
	}
	h.mu.RUnlock()

	if len(players) < MafiaMinPlayers {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Minimum 5 nəfər lazımdır",
			"code":  "NOT_ENOUGH_PLAYERS",
		})
		return
	}
	if len(players) > MafiaMaxPlayers {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Maksimum 12 nəfər ola bilər",
			"code":  "TOO_MANY_PLAYERS",
		})
		return
	}

	// Rolları gizli payla
	mafiaTeam, err := assignRoles(players)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	game := &MafiaGame{
		Type:      "mafia",
		DayNumber: 0,
		Players:   players,
		MafiaTeam: mafiaTeam,
		Votes:     make(map[uint]uint),
		History:   []MafiaHistoryEntry{},
	}
	game.setPhase(MafiaPhaseIntro, mafiaIntroSeconds)
	saveGame(roomID, game)

	// 1) Hər oyunçuya öz rolunu fərdi göndər (gizli)
	h.sendRoleAssignments(roomID, game)
	// 2) Hamıya maskalanmış vəziyyət (kart mərhələsi başladı)
	h.broadcastPublicState(roomID, game, "mafia_phase_changed")
	// 3) Sistem elanı
	h.systemChatMessage(roomID, "Mafia oyunu başladı. Kartınıza baxın və 'Hazırım'a basın.")

	c.JSON(http.StatusOK, gin.H{
		"ok":           true,
		"player_count": len(players),
		"phase":        game.Phase,
	})
}

// roleImages — mafia_roles table-ından key→image_path xəritəsini oxuyur
// və S3 tam URL-ə çevirir (PrependS3URL). Şəkil yoxdursa key buraxılır.
func roleImages() map[string]string {
	var rows []struct {
		Key       string
		ImagePath *string
	}
	database.DB.
		Table("mafia_roles").
		Select("key, image_path").
		Where("is_active = ?", true).
		Scan(&rows)

	out := make(map[string]string, len(rows))
	for _, r := range rows {
		if r.ImagePath == nil || *r.ImagePath == "" {
			continue
		}
		if full := utils.PrependS3URL(r.ImagePath); full != nil {
			out[r.Key] = *full
		}
	}
	return out
}

// sendRoleAssignments — hər oyunçuya öz kartını fərdi göndərir.
// Mafialara komanda yoldaşları da bildirilir (plan §3).
func (h *LiveHub) sendRoleAssignments(roomID uint, g *MafiaGame) {
	images := roleImages()

	for _, p := range g.Players {
		payloadMap := map[string]interface{}{
			"role":        p.Role,
			"role_label":  roleLabel(p.Role),
			"description": roleDescription(p.Role),
			"image":       images[p.Role], // kart şəkli (S3 URL, yoxdursa "")
		}
		// Mafiadırsa komanda yoldaşlarını da göndər
		if g.isMafiaRole(p.Role) {
			var teammates []map[string]interface{}
			for _, tid := range g.MafiaTeam {
				tp := g.findPlayer(tid)
				if tp == nil {
					continue
				}
				teammates = append(teammates, map[string]interface{}{
					"user_id": tp.UserID,
					"name":    tp.Name,
					"role":    tp.Role, // mafia / don
					"image":   images[tp.Role],
				})
			}
			payloadMap["mafia_team"] = teammates
		}
		data, _ := json.Marshal(payloadMap)
		payload, _ := json.Marshal(map[string]interface{}{
			"type":    "mafia_role_assigned",
			"room_id": roomID,
			"data":    json.RawMessage(data),
		})
		h.sendToUser(roomID, p.UserID, payload)
	}
}

// roleDescription — kart altında göstərilən qısa izah.
func roleDescription(role string) string {
	switch role {
	case RoleDon:
		return "Mafia lideri. Gecə bir nəfəri vurursan. Komandanı tanıyırsan."
	case RoleMafia:
		return "Gecə Don kimi vurur. Komanda yoldaşlarını tanıyırsan."
	case RoleDetective:
		return "Gecə bir nəfəri seç. Mafia olsa çıxarırsan, sakin olsa boşa gedir."
	case RoleCitizen:
		return "Gücün yoxdur. Gündüz müzakirə et və düzgün səs ver."
	default:
		return ""
	}
}
