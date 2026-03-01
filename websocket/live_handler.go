package websocket

import (
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

	// liveUpgrader kullanıyoruz
	conn, err := liveUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Println("Live WebSocket Upgrade Error:", err)
		return
	}

	client := &LiveRoomClient{
		Hub:    h,
		Conn:   conn,
		UserID: userID,
		RoomID: uint(roomID),
		Role:   participant.Role,
		Send:   make(chan []byte, 256),
	}

	client.Hub.Register <- client

	go client.writePump()
	go client.readPump()
}

// readPump - Flutter'dan gelen mesajları okur
func (c *LiveRoomClient) readPump() {
	defer func() {
		c.Hub.Unregister <- c
		c.Conn.Close()
	}()

	for {
		_, message, err := c.Conn.ReadMessage()
		if err != nil {
			break
		}

		var event LiveMessageEvent
		if err := json.Unmarshal(message, &event); err != nil {
			continue
		}

		event.SenderID = c.UserID
		event.RoomID = c.RoomID

		if event.Type == "chat_message" {
			if dataMap, ok := event.Data.(map[string]interface{}); ok {
				if textData, exists := dataMap["text"].(string); exists && textData != "" {
					chatMsg := models.LiveRoomMessage{
						LiveRoomID: c.RoomID,
						SenderID:   c.UserID,
						Text:       textData,
					}
					database.DB.Create(&chatMsg)
					event.Data = gin.H{
						"id":         chatMsg.ID,
						"text":       textData,
						"created_at": chatMsg.CreatedAt,
					}
				}
			}
		}

		c.Hub.Broadcast <- &event
	}
}

// writePump - Flutter'a mesaj gönderir
func (c *LiveRoomClient) writePump() {
	ticker := time.NewTicker(54 * time.Second)
	defer func() {
		ticker.Stop()
		c.Conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.Send:
			if !ok {
				c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
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
