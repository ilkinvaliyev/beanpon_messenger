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
