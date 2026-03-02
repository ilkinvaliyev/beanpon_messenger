package websocket

import (
	"beanpon_messenger/database"
	"beanpon_messenger/models"
	"beanpon_messenger/utils"
	"encoding/json"
	"log"
	"sync"

	"github.com/gorilla/websocket"
)

// YENİ: Name ve Avatar eklendi. (HandleWebSocket tarafında doldurulmalı)
type LiveRoomClient struct {
	Hub    *LiveHub
	Conn   *websocket.Conn
	UserID uint
	RoomID uint
	Role   string
	Name   string  // RAM'de tutulacak isim
	Avatar *string // RAM'de tutulacak avatar URL
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
			count := len(h.rooms[client.RoomID]) // Güncel sayıyı al
			h.mu.Unlock()

			log.Printf("User %d joined Live Room %d as %s", client.UserID, client.RoomID, client.Role)

			// Eventi standart Broadcast kanalına asenkron olarak yolluyoruz.
			eventData, _ := json.Marshal(map[string]interface{}{"count": count})
			event := &LiveMessageEvent{
				Type:     "viewer_count_update",
				SenderID: 0,
				RoomID:   client.RoomID,
				Data:     json.RawMessage(eventData),
			}
			go func(e *LiveMessageEvent) {
				h.Broadcast <- e
			}(event)

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

			// Odada kalan varsa, güncel sayıyı Broadcast'e yolla
			if roomExists {
				eventData, _ := json.Marshal(map[string]interface{}{"count": count})
				event := &LiveMessageEvent{
					Type:     "viewer_count_update",
					SenderID: 0,
					RoomID:   client.RoomID,
					Data:     json.RawMessage(eventData),
				}
				go func(e *LiveMessageEvent) {
					h.Broadcast <- e
				}(event)
			}

		case event := <-h.Broadcast:
			h.handleEvent(event)
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

	// Sadece chat_message DEĞİLSE önceden payload'ı marshal ediyoruz.
	// chat_message kendi payload'ını zenginleştirecek.
	var payload []byte
	if event.Type != "chat_message" {
		payload, _ = json.Marshal(event)
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
			return
		}

		// 1. PERFORMANS: DB sorgusu yok! Göndericinin bilgilerini RAM'den çekiyoruz.
		h.mu.RLock()
		senderClient, senderExists := h.rooms[event.RoomID][event.SenderID]
		h.mu.RUnlock()

		senderName := "User"
		var senderAvatar *string = nil

		if senderExists {
			senderName = senderClient.Name
			if senderClient.Avatar != nil {
				avatar := *senderClient.Avatar // pointer dereference
				senderAvatar = &avatar
			}
		}

		// 2. Mesaj Datasini Zenginleştirme
		updatedData, _ := json.Marshal(map[string]interface{}{
			// id'yi simdilik bos veya random verebilirsin cunku flutter tarafi id'yi genelde listelemede kullanir.
			// DB id'si anlik onemsizdir (UI guncellemesi icin).
			"text":          textData,
			"sender_id":     event.SenderID,
			"sender_name":   senderName,
			"sender_avatar": utils.PrependBaseURL(senderAvatar),
		})
		event.Data = updatedData

		chatPayload, _ := json.Marshal(event)

		// 3. ANINDA BROADCAST (Bekleme yok!)
		for _, client := range roomClients {
			select {
			case client.Send <- chatPayload:
			default:
				close(client.Send)
				go func(c *LiveRoomClient) {
					h.Unregister <- c
				}(client)
			}
		}

		// 4. PERFORMANS: DB KAYDINI ASENKRON YAP (Arka planda çalışır, kimseyi bekletmez)
		go func(roomID uint, senderID uint, text string) {
			chatMsg := models.LiveRoomMessage{
				LiveRoomID: roomID,
				SenderID:   senderID,
				Text:       text,
			}
			if err := database.DB.Create(&chatMsg).Error; err != nil {
				log.Printf("💥 DB ASYNC KAYIT HATASI: %v", err)
			}
		}(event.RoomID, event.SenderID, textData)

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

	case "viewer_count_update":
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
			return
		}

		targetUserID := uint(targetUserIDFloat)
		if targetClient, exists := roomClients[targetUserID]; exists {
			targetClient.Role = "audience" // Rolünü geri al (Dinleyici yap)

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
