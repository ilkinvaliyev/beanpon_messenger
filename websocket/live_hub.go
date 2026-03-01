package websocket

import (
	"encoding/json"
	"log"
	"sync"
	// Sadece bu paketi import etmemiz gerekiyordu
	"github.com/gorilla/websocket"
)

// LiveRoomClient - Canlı yayındaki bir WebSocket bağlantısı
type LiveRoomClient struct {
	Hub    *LiveHub
	Conn   *websocket.Conn // HATA BURADAYDI: *WebSocketConnection yerine *websocket.Conn olmalı
	UserID uint
	RoomID uint
	Role   string // "host", "broadcaster", "audience"
	Send   chan []byte
}

// LiveHub - Tüm canlı yayın odalarını yönetecek merkez
type LiveHub struct {
	// rooms: RoomID -> UserID -> Client
	rooms map[uint]map[uint]*LiveRoomClient

	Register   chan *LiveRoomClient
	Unregister chan *LiveRoomClient
	Broadcast  chan *LiveMessageEvent
	mu         sync.RWMutex
}

// LiveMessageEvent - İstemciler arası gidip gelecek JSON formatı
type LiveMessageEvent struct {
	Type     string      `json:"type"` // "chat_message", "broadcast_request", "request_approved"
	RoomID   uint        `json:"room_id"`
	SenderID uint        `json:"sender_id"`
	Data     interface{} `json:"data"`
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
		// Biri odaya katıldığında
		case client := <-h.Register:
			h.mu.Lock()
			if _, ok := h.rooms[client.RoomID]; !ok {
				h.rooms[client.RoomID] = make(map[uint]*LiveRoomClient)
			}
			h.rooms[client.RoomID][client.UserID] = client
			h.mu.Unlock()

			log.Printf("User %d joined Live Room %d as %s", client.UserID, client.RoomID, client.Role)

		// Biri odadan çıktığında (veya soketi koptuğunda)
		case client := <-h.Unregister:
			h.mu.Lock()
			if room, ok := h.rooms[client.RoomID]; ok {
				if _, ok := room[client.UserID]; ok {
					delete(room, client.UserID)
					close(client.Send)
					log.Printf("User %d left Live Room %d", client.UserID, client.RoomID)
				}
				// Oda boşaldıysa memory'den sil
				if len(room) == 0 {
					delete(h.rooms, client.RoomID)
				}
			}
			h.mu.Unlock()

		// Bir mesaj/event geldiğinde
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
		return // Oda aktif değilse bir şey yapma
	}

	payload, err := json.Marshal(event)
	if err != nil {
		log.Println("LiveHub marshal error:", err)
		return
	}

	// Event tipine göre dağıtım mantığı
	switch event.Type {
	case "chat_message":
		// Chat mesajı odadaki HERKESE gider
		for _, client := range roomClients {
			select {
			case client.Send <- payload:
			default:
				close(client.Send)
				h.Unregister <- client
			}
		}

	case "broadcast_request":
		// Dinleyici söz istiyor. SADECE HOST'A gider!
		for _, client := range roomClients {
			if client.Role == "host" {
				client.Send <- payload
			}
		}

	case "request_approved":
		// Host onayladı. SADECE İSTEĞİ YAPAN KİŞİYE gider (mikrofonunu açsın diye)
		targetUserID := event.Data.(map[string]interface{})["target_user_id"].(float64)
		if targetClient, exists := roomClients[uint(targetUserID)]; exists {
			targetClient.Role = "broadcaster"
			targetClient.Send <- payload
		}
	}
}
