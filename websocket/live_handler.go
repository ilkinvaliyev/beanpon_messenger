package websocket

import (
	"beanpon_messenger/utils"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"beanpon_messenger/database"
	"beanpon_messenger/models"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

var liveUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func (h *LiveHub) HandleWebSocket(c *gin.Context) {
	userIDVal, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	userID := userIDVal.(uint)

	roomIDStr := c.Query("room_id")
	roomID, err := strconv.ParseUint(roomIDStr, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid room_id"})
		return
	}

	var participant models.LiveRoomParticipant
	if err := database.DB.Where("live_room_id = ? AND user_id = ? AND status = 'active'", roomID, userID).First(&participant).Error; err != nil {
		// Participant yoxdur — admin-dirsə icazə ver
		var user models.User
		if dbErr := database.DB.Select("is_admin").First(&user, userID).Error; dbErr != nil || !user.IsAdmin {
			c.JSON(http.StatusForbidden, gin.H{"error": "Odaya erişim izniniz yok"})
			return
		}
		participant = models.LiveRoomParticipant{
			LiveRoomID: uint(roomID),
			UserID:     userID,
			Role:       "broadcaster",
			Status:     "active",
		}
		database.DB.Create(&participant)
	}

	var room models.LiveRoom
	if err := database.DB.Select("host_user_id").First(&room, roomID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Oda tapılmadı"})
		return
	}

	if models.IsBlocked(database.DB, room.HostUserID, userID) || models.IsBlocked(database.DB, userID, room.HostUserID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Bu yayına qoşula bilməzsiniz (Bloklanıb)"})
		return
	}

	conn, err := liveUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Println("Live WebSocket Upgrade Error:", err)
		return
	}

	type SenderInfo struct {
		Name             string
		ProfileImage     *string
		ProfileImageType *string
	}
	var senderInfo SenderInfo
	database.DB.Table("users").
		Select("users.name, profiles.profile_image, profiles.profile_image_type").
		Joins("left join profiles on profiles.user_id = users.id").
		Where("users.id = ?", userID).
		Scan(&senderInfo)

	client := &LiveRoomClient{
		Hub:        h,
		Conn:       conn,
		UserID:     userID,
		RoomID:     uint(roomID),
		Role:       participant.Role,
		Name:       senderInfo.Name,
		Avatar:     senderInfo.ProfileImage,
		AvatarType: senderInfo.ProfileImageType,
		Send:       make(chan []byte, 256),
	}

	client.Hub.Register <- client

	// Aktif oyun varsa sadece bu client'a gönder
	var activeGame json.RawMessage
	dbErr := database.DB.
		Table("live_rooms").
		Select("active_game").
		Where("id = ?", uint(roomID)).
		Scan(&activeGame).Error

	if dbErr == nil && len(activeGame) > 0 && string(activeGame) != "null" {
		joinGamePayload, _ := json.Marshal(map[string]interface{}{
			"type":    "game_current_state",
			"room_id": uint(roomID),
			"data":    json.RawMessage(activeGame),
		})
		select {
		case client.Send <- joinGamePayload:
		default:
		}
	}

	go client.writePump()
	go client.readPump()
}

func (c *LiveRoomClient) readPump() {
	defer func() {
		c.Hub.Unregister <- c
		err := c.Conn.Close()
		if err != nil {
			return
		}
	}()

	for {
		_, message, err := c.Conn.ReadMessage()
		if err != nil {
			log.Printf("❌ ReadMessage hatası: %v", err)
			break
		}

		log.Printf("📥 HAM MESAJ: %s", string(message))

		var event LiveMessageEvent
		if err := json.Unmarshal(message, &event); err != nil {
			log.Printf("❌ JSON PARSE HATASI: %v", err)
			continue
		}

		log.Printf("✅ PARSE EDILDI: type=%s, senderID=%d, roomID=%d, data=%s",
			event.Type, event.SenderID, event.RoomID, string(event.Data))

		event.SenderID = c.UserID
		event.RoomID = c.RoomID

		log.Printf("📤 BROADCAST'E GONDERILIYOR: type=%s", event.Type)
		c.Hub.Broadcast <- &event
		log.Printf("✅ BROADCAST'E GONDERILDI")
	}
}

func (c *LiveRoomClient) writePump() {
	ticker := time.NewTicker(54 * time.Second)
	defer func() {
		ticker.Stop()
		err := c.Conn.Close()
		if err != nil {
			return
		}
	}()

	for {
		select {
		case message, ok := <-c.Send:
			if !ok {
				err := c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				if err != nil {
					return
				}
				return
			}

			w, err := c.Conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(message)

			n := len(c.Send)
			for i := 0; i < n; i++ {
				w.Write([]byte{'\n'})
				w.Write(<-c.Send)
			}

			if err := w.Close(); err != nil {
				return
			}

		case <-ticker.C:
			if err := c.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (h *LiveHub) GetLiveRoomMessages(c *gin.Context) {
	userIDVal, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	userID := userIDVal.(uint)

	roomIDStr := c.Param("room_id")
	roomID, err := strconv.ParseUint(roomIDStr, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid room_id"})
		return
	}

	var participant models.LiveRoomParticipant
	if err := database.DB.Where("live_room_id = ? AND user_id = ?", roomID, userID).First(&participant).Error; err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "Bu odaya girişiniz yoxdur"})
		return
	}

	limitStr := c.DefaultQuery("limit", "20")
	cursorStr := c.Query("cursor")

	limit, err := strconv.Atoi(limitStr)
	if err != nil || limit <= 0 || limit > 50 {
		limit = 20
	}

	type MessageRow struct {
		ID                    uint                    `json:"id"`
		Text                  string                  `json:"text"`
		GifURL                *string                 `json:"gif_url"`
		ImageURL              *string                 `json:"image_url"`
		SenderID              uint                    `json:"sender_id"`
		SenderName            string                  `json:"sender_name"`
		SenderAvatar          *string                 `json:"sender_avatar"`
		SenderAvatarType      *string                 `json:"sender_avatar_type"`
		CreatedAt             time.Time               `json:"created_at"`
		ReplyToID             *uint                   `json:"reply_to_id"`
		ReplyText             *string                 `json:"reply_text"`
		ReplySenderID         *uint                   `json:"reply_sender_id"`
		ReplySenderName       *string                 `json:"reply_sender_name"`
		ReplySenderAvatar     *string                 `json:"reply_sender_avatar"`
		ReplySenderAvatarType *string                 `json:"reply_sender_avatar_type"`
		Mentions              []utils.MentionResponse `json:"mentions"`
	}

	query := database.DB.Table("live_room_messages lm").
		Select(`lm.id, lm.text, lm.gif_url, lm.image_url, lm.sender_id, lm.created_at, lm.reply_to_id,
		u.name as sender_name,
		p.profile_image as sender_avatar,
		p.profile_image_type as sender_avatar_type,
		rm.text as reply_text,
		rm.sender_id as reply_sender_id,
		ru.name as reply_sender_name,
		rp.profile_image as reply_sender_avatar,
		rp.profile_image_type as reply_sender_avatar_type`).
		Joins("LEFT JOIN users u ON u.id = lm.sender_id").
		Joins("LEFT JOIN profiles p ON p.user_id = lm.sender_id").
		Joins("LEFT JOIN live_room_messages rm ON rm.id = lm.reply_to_id").
		Joins("LEFT JOIN users ru ON ru.id = rm.sender_id").
		Joins("LEFT JOIN profiles rp ON rp.user_id = rm.sender_id").
		Where("lm.live_room_id = ?", roomID).
		Order("lm.id DESC").
		Limit(limit + 1)

	if cursorStr != "" {
		cursor, err := strconv.ParseUint(cursorStr, 10, 64)
		if err == nil {
			query = query.Where("lm.id < ?", cursor)
		}
	}

	var messages []MessageRow
	if err := query.Scan(&messages).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Mesajlar gətirilə bilmədi"})
		return
	}

	hasMore := len(messages) > limit
	if hasMore {
		messages = messages[:limit]
	}

	for i := range messages {
		if messages[i].SenderAvatarType != nil && *messages[i].SenderAvatarType == "gif" {
			// gif — olduğu kimi qal
		} else if messages[i].SenderAvatar != nil {
			messages[i].SenderAvatar = utils.PrependBaseURL(messages[i].SenderAvatar)
		}

		if messages[i].ReplySenderAvatarType != nil && *messages[i].ReplySenderAvatarType == "gif" {
			// gif — olduğu kimi qal
		} else if messages[i].ReplySenderAvatar != nil {
			messages[i].ReplySenderAvatar = utils.PrependBaseURL(messages[i].ReplySenderAvatar)
		}

		if messages[i].ImageURL != nil {
			messages[i].ImageURL = utils.PrependS3URL(messages[i].ImageURL)
		}
		if messages[i].GifURL != nil {
			messages[i].GifURL = utils.PrependS3URL(messages[i].GifURL)
		}

		messages[i].Mentions = utils.ParseMentions(messages[i].Text)

	}

	var nextCursor *uint
	if hasMore && len(messages) > 0 {
		last := messages[len(messages)-1].ID
		nextCursor = &last
	}

	c.JSON(http.StatusOK, gin.H{
		"messages":    messages,
		"has_more":    hasMore,
		"next_cursor": nextCursor,
	})
}

func (h *LiveHub) GetLiveRoomReactions(c *gin.Context) {
	roomIDStr := c.Param("room_id")
	roomID, err := strconv.ParseUint(roomIDStr, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid room_id"})
		return
	}

	type ReactionRow struct {
		ReactionName string `json:"reaction_name"`
		Count        uint64 `json:"count"`
	}

	var reactions []ReactionRow
	if err := database.DB.Table("live_room_reactions").
		Select("reaction_name, count").
		Where("live_room_id = ?", roomID).
		Order("count DESC").
		Scan(&reactions).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Reaksiyalar gətirilə bilmədi"})
		return
	}

	var total uint64
	for _, r := range reactions {
		total += r.Count
	}

	top := reactions
	if len(top) > 2 {
		top = top[:2]
	}

	c.JSON(http.StatusOK, gin.H{
		"reactions":     reactions,
		"top_reactions": top,
		"total":         total,
	})
}
