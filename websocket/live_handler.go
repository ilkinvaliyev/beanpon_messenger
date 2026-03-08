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

// Diğer upgrader ile çakışmaması için ismini değiştirdik
var liveUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true // CORS izinleri
	},
}

// HandleWebSocket - /ws/live endpoint'ini doğrudan Hub üzerinden karşılar
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
		c.JSON(http.StatusForbidden, gin.H{"error": "Odaya erişim izniniz yok"})
		return
	}

	// --- YENİ EKLENEN BLOK KONTROLÜ BAŞLANGICI ---
	// Odanın Host'unu (Yayıncısını) buluyoruz
	var room models.LiveRoom
	if err := database.DB.Select("host_user_id").First(&room, roomID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Oda tapılmadı"})
		return
	}

	// Eğer Host beni bloklamışsa VEYA ben Host'u bloklamışsam giremem
	if models.IsBlocked(database.DB, room.HostUserID, userID) || models.IsBlocked(database.DB, userID, room.HostUserID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Bu yayına qoşula bilməzsiniz (Bloklanıb)"})
		return
	}
	// --- YENİ EKLENEN BLOK KONTROLÜ BİTİŞİ ---

	// liveUpgrader kullanıyoruz
	conn, err := liveUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Println("Live WebSocket Upgrade Error:", err)
		return
	}

	// Kullanıcının ismini ve resmini SADECE ODAYA GİRERKEN DB'den 1 kez çekiyoruz
	type SenderInfo struct {
		Name         string
		ProfileImage *string
	}
	var senderInfo SenderInfo
	database.DB.Table("users").
		Select("users.name, profiles.profile_image").
		Joins("left join profiles on profiles.user_id = users.id").
		Where("users.id = ?", userID).
		Scan(&senderInfo)

	client := &LiveRoomClient{
		Hub:    h,
		Conn:   conn,
		UserID: userID,
		RoomID: uint(roomID),
		Role:   participant.Role,
		Name:   senderInfo.Name,         // RAM'e yazıldı
		Avatar: senderInfo.ProfileImage, // RAM'e yazıldı
		Send:   make(chan []byte, 256),
	}

	client.Hub.Register <- client

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

// writePump - Flutter'a mesaj gönderir
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

// GetLiveRoomMessages - Canlı yayın mesaj tarixçəsi (pagination ilə)
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

	// İstifadəçi bu odaya aid olmalıdır
	var participant models.LiveRoomParticipant
	if err := database.DB.Where("live_room_id = ? AND user_id = ?", roomID, userID).First(&participant).Error; err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "Bu odaya girişiniz yoxdur"})
		return
	}

	// Pagination
	limitStr := c.DefaultQuery("limit", "20")
	cursorStr := c.Query("cursor") // Son mesajın ID-si (növbəti səhifə üçün)

	limit, err := strconv.Atoi(limitStr)
	if err != nil || limit <= 0 || limit > 50 {
		limit = 20
	}

	type MessageRow struct {
		ID           uint      `json:"id"`
		Text         string    `json:"text"`
		SenderID     uint      `json:"sender_id"`
		SenderName   string    `json:"sender_name"`
		SenderAvatar *string   `json:"sender_avatar"`
		CreatedAt    time.Time `json:"created_at"`
	}

	query := database.DB.Table("live_room_messages lm").
		Select(`lm.id, lm.text, lm.sender_id, lm.created_at,
			u.name as sender_name,
			p.profile_image as sender_avatar`).
		Joins("LEFT JOIN users u ON u.id = lm.sender_id").
		Joins("LEFT JOIN profiles p ON p.user_id = lm.sender_id").
		Where("lm.live_room_id = ?", roomID).
		Order("lm.id DESC").
		Limit(limit + 1) // +1 ile "has_more" anlayırıq

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

	// Avatar URL-lərini düzəlt
	for i := range messages {
		if messages[i].SenderAvatar != nil {
			url := utils.PrependBaseURL(messages[i].SenderAvatar)
			messages[i].SenderAvatar = url
		}
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

// GetLiveRoomReactions - Odanın reaksiya statistikası
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

	// Toplam say
	var total uint64
	for _, r := range reactions {
		total += r.Count
	}

	// Top 2
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
