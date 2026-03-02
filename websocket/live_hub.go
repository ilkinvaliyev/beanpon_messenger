package websocket

import (
	"beanpon_messenger/database"
	"beanpon_messenger/models"
	"encoding/json"
	"log"
	"sync"

	"github.com/gorilla/websocket"
)

type LiveRoomClient struct {
	Hub    *LiveHub
	Conn   *websocket.Conn
	UserID uint
	RoomID uint
	Role   string
	Send   chan []byte
}

type LiveHub struct {
	rooms map[uint]map[uint]*LiveRoomClient

	Register   chan *LiveRoomClient
	Unregister chan *LiveRoomClient
	Broadcast  chan *LiveMessageEvent
	mu         sync.RWMutex
}

type LiveMessageEvent struct {
	Type     string          `json:"type"`
	SenderID uint            `json:"sender_id"`
	RoomID   uint            `json:"room_id"`
	Data     json.RawMessage `json:"data"`
}

func NewLiveHub() *LiveHub {
	return &LiveHub{
		rooms:      make(map[uint]map[uint]*LiveRoomClient),
		Register:   make(chan *LiveRoomClient),
		Unregister: make(chan *LiveRoomClient),
		Broadcast:  make(chan *LiveMessageEvent),
	}
}

func (h *LiveHub) Run() {
	for {
		select {
		case client := <-h.Register:
			h.mu.Lock()
			if _, ok := h.rooms[client.RoomID]; !ok {
				h.rooms[client.RoomID] = make(map[uint]*LiveRoomClient)
			}
			h.rooms[client.RoomID][client.UserID] = client

			// Odadaki güncel kişi sayısını al
			count := len(h.rooms[client.RoomID])
			h.mu.Unlock()

			log.Printf("User %d joined Live Room %d as %s. Total in room: %d", client.UserID, client.RoomID, client.Role, count)

			// YENİ: Herkese güncel sayıyı gönder
			h.broadcastViewerCount(client.RoomID, count)

		case client := <-h.Unregister:
			h.mu.Lock()
			var count int
			roomExists := false

			if room, ok := h.rooms[client.RoomID]; ok {
				if _, ok := room[client.UserID]; ok {
					delete(room, client.UserID)
					close(client.Send)
					log.Printf("User %d left Live Room %d", client.UserID, client.RoomID)
				}
				count = len(room)
				roomExists = true
				if count == 0 {
					delete(h.rooms, client.RoomID)
					roomExists = false
				}
			}
			h.mu.Unlock()

			// YENİ: Eğer oda hala varsa, kalanlara güncel sayıyı gönder
			if roomExists {
				h.broadcastViewerCount(client.RoomID, count)
			}

		case event := <-h.Broadcast:
			h.handleEvent(event)
		}
	}
}

// YENİ YARDIMCI FONKSİYON: Odadaki herkese izleyici sayısını fırlatır
func (h *LiveHub) broadcastViewerCount(roomID uint, count int) {
	h.mu.RLock()
	roomClients, ok := h.rooms[roomID]
	h.mu.RUnlock()

	if !ok {
		return
	}

	// 'viewer_count_update' tipinde bir event oluşturuyoruz
	eventData, _ := json.Marshal(map[string]interface{}{
		"count": count,
	})

	event := &LiveMessageEvent{
		Type:   "viewer_count_update",
		RoomID: roomID,
		Data:   json.RawMessage(eventData),
	}

	payload, err := json.Marshal(event)
	if err != nil {
		log.Println("Viewer count marshal error:", err)
		return
	}

	for _, client := range roomClients {
		select {
		case client.Send <- payload:
		default:
			close(client.Send)
			go func(c *LiveRoomClient) {
				h.Unregister <- c
			}(client)
		}
	}
}

func (h *LiveHub) handleEvent(event *LiveMessageEvent) {
	h.mu.RLock()
	roomClients, ok := h.rooms[event.RoomID]
	h.mu.RUnlock()

	if !ok {
		return
	}

	payload, err := json.Marshal(event)
	if err != nil {
		log.Println("LiveHub marshal error:", err)
		return
	}

	switch event.Type {

	case "chat_message":
		var dataMap map[string]interface{}
		if err := json.Unmarshal(event.Data, &dataMap); err != nil {
			log.Printf("❌ chat_message data parse hatası: %v", err)
			return
		}

		textData, ok := dataMap["text"].(string)
		if !ok || textData == "" {
			log.Println("⚠️ text alanı bulunamadı")
			return
		}

		chatMsg := models.LiveRoomMessage{
			LiveRoomID: event.RoomID,
			SenderID:   event.SenderID,
			Text:       textData,
		}

		if err := database.DB.Create(&chatMsg).Error; err != nil {
			log.Printf("💥 DB KAYIT HATASI: %v", err)
			return
		}

		log.Printf("✅ DB KAYIT BAŞARILI: Mesaj ID %d", chatMsg.ID)

		updatedData, _ := json.Marshal(map[string]interface{}{
			"id":         chatMsg.ID,
			"text":       textData,
			"created_at": chatMsg.CreatedAt,
			"sender_id":  event.SenderID,
		})
		event.Data = json.RawMessage(updatedData)

		payload, _ = json.Marshal(event)

		for _, client := range roomClients {
			select {
			case client.Send <- payload:
			default:
				close(client.Send)
				go func(c *LiveRoomClient) {
					h.Unregister <- c
				}(client)
			}
		}

	case "ping":
		h.mu.RLock()
		client, exists := roomClients[event.SenderID]
		h.mu.RUnlock()
		if exists {
			pong, _ := json.Marshal(map[string]string{"type": "pong"})
			select {
			case client.Send <- pong:
			default:
			}
		}

	case "broadcast_request":
		for _, client := range roomClients {
			if client.Role == "host" {
				select {
				case client.Send <- payload:
				default:
					close(client.Send)
					go func(c *LiveRoomClient) {
						h.Unregister <- c
					}(client)
				}
			}
		}

	case "request_approved":
		var dataMap map[string]interface{}
		if err := json.Unmarshal(event.Data, &dataMap); err != nil {
			log.Printf("❌ request_approved data parse hatası: %v", err)
			return
		}

		targetUserIDFloat, ok := dataMap["target_user_id"].(float64)
		if !ok {
			log.Println("⚠️ target_user_id bulunamadı veya geçersiz")
			return
		}

		targetUserID := uint(targetUserIDFloat)
		if targetClient, exists := roomClients[targetUserID]; exists {
			targetClient.Role = "broadcaster"
			select {
			case targetClient.Send <- payload:
			default:
				close(targetClient.Send)
				go func(c *LiveRoomClient) {
					h.Unregister <- c
				}(targetClient)
			}
		}

	case "kick_speaker":
		var dataMap map[string]interface{}
		if err := json.Unmarshal(event.Data, &dataMap); err != nil {
			log.Printf("❌ kick_speaker data parse hatası: %v", err)
			return
		}

		targetUserIDFloat, ok := dataMap["target_user_id"].(float64)
		if !ok {
			log.Println("⚠️ kick_speaker target_user_id bulunamadı veya geçersiz")
			return
		}

		targetUserID := uint(targetUserIDFloat)
		if targetClient, exists := roomClients[targetUserID]; exists {
			targetClient.Role = "audience" // Rolünü geri al (Dinleyici yap)

			// Güvenli gönderim (Kanal doluysa/kopmuşsa crash olmaması için)
			select {
			case targetClient.Send <- payload:
			default:
				close(targetClient.Send)
				go func(c *LiveRoomClient) {
					h.Unregister <- c
				}(targetClient)
			}
		}
	}
}
