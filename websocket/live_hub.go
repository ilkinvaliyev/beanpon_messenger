package websocket

import (
	"beanpon_messenger/database"
	"beanpon_messenger/models"
	"beanpon_messenger/utils"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type LiveRoomClient struct {
	Hub    *LiveHub
	Conn   *websocket.Conn
	UserID uint
	RoomID uint
	Role   string
	Name   string
	Avatar *string
	Send   chan []byte
}

type LiveHub struct {
	rooms map[uint]map[uint]*LiveRoomClient

	// reactionBuffer[roomID][reactionName] = toplam sayı
	// Her 1.5 saniyede bir flush edilir
	reactionBuffer map[uint]map[string]uint64

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
		rooms:          make(map[uint]map[uint]*LiveRoomClient),
		reactionBuffer: make(map[uint]map[string]uint64),
		Register:       make(chan *LiveRoomClient),
		Unregister:     make(chan *LiveRoomClient),
		Broadcast:      make(chan *LiveMessageEvent),
	}
}

func (h *LiveHub) Run() {
	// Her 1.5 saniyede bir reactionBuffer'ı flush et
	reactionTicker := time.NewTicker(1500 * time.Millisecond)
	defer reactionTicker.Stop()

	for {
		select {
		case client := <-h.Register:
			h.mu.Lock()
			if _, ok := h.rooms[client.RoomID]; !ok {
				h.rooms[client.RoomID] = make(map[uint]*LiveRoomClient)
			}
			h.rooms[client.RoomID][client.UserID] = client
			count := len(h.rooms[client.RoomID])
			h.mu.Unlock()

			log.Printf("User %d joined Live Room %d as %s", client.UserID, client.RoomID, client.Role)

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
					delete(h.reactionBuffer, client.RoomID) // Buffer-ı da temizle
					roomExists = false
				}
			}
			h.mu.Unlock()

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

		case <-reactionTicker.C:
			h.flushReactions()
		}
	}
}

// flushReactions - Buffer'daki reaksiyonları toplu olarak broadcast eder ve DB'ye yazar
func (h *LiveHub) flushReactions() {
	h.mu.Lock()

	// Göndermek için snapshot al, sonra buffer'ı sıfırla
	type reactionSnapshot struct {
		roomID    uint
		reactions map[string]uint64
	}

	var snapshots []reactionSnapshot

	for roomID, reactions := range h.reactionBuffer {
		if len(reactions) == 0 {
			continue
		}
		snapshot := reactionSnapshot{
			roomID:    roomID,
			reactions: make(map[string]uint64),
		}
		for reactionName, count := range reactions {
			snapshot.reactions[reactionName] = count
		}
		snapshots = append(snapshots, snapshot)
	}

	// Buffer'ı sıfırla
	h.reactionBuffer = make(map[uint]map[string]uint64)

	h.mu.Unlock()

	// Snapshot'ları broadcast et ve DB'ye yaz
	for _, snapshot := range snapshots {
		h.mu.RLock()
		roomClients, ok := h.rooms[snapshot.roomID]
		h.mu.RUnlock()

		if !ok {
			continue
		}

		// Flutter'a gönderilecek payload
		// {"type": "reaction_update", "data": {"heart": 5, "like": 2}}
		eventData, _ := json.Marshal(map[string]interface{}{
			"reactions": snapshot.reactions,
		})
		payload, _ := json.Marshal(map[string]interface{}{
			"type":    "reaction_update",
			"room_id": snapshot.roomID,
			"data":    json.RawMessage(eventData),
		})

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

		// DB'ye async upsert — her emoji için count'u artır
		go func(roomID uint, reactions map[string]uint64) {
			for reactionName, count := range reactions {
				err := database.DB.Exec(`
					INSERT INTO live_room_reactions (live_room_id, reaction_name, count, created_at, updated_at)
					VALUES (?, ?, ?, NOW(), NOW())
					ON DUPLICATE KEY UPDATE count = count + ?, updated_at = NOW()
				`, roomID, reactionName, count, count).Error
				if err != nil {
					log.Printf("💥 Reaction DB upsert hatası: %v", err)
				}
			}
		}(snapshot.roomID, snapshot.reactions)
	}
}

func (h *LiveHub) handleEvent(event *LiveMessageEvent) {
	h.mu.RLock()
	roomClients, ok := h.rooms[event.RoomID]
	h.mu.RUnlock()

	if !ok {
		return
	}

	var payload []byte
	if event.Type != "chat_message" && event.Type != "broadcast_request" && event.Type != "reaction" {
		payload, _ = json.Marshal(event)
	}

	switch event.Type {

	case "reaction":
		// Flutter'dan gelen: {"type": "reaction", "data": {"name": "heart"}}
		var dataMap map[string]interface{}
		if err := json.Unmarshal(event.Data, &dataMap); err != nil {
			log.Printf("❌ reaction data parse hatası: %v", err)
			return
		}

		reactionName, ok := dataMap["name"].(string)
		if !ok || reactionName == "" {
			return
		}

		// Geçerli emoji adı kontrolü
		validReactions := map[string]bool{
			"heart": true, "like": true, "laugh": true,
			"withyou": true, "cry": true, "angry": true, "dislike": true,
		}
		if !validReactions[reactionName] {
			return
		}

		// Buffer'a ekle (DB'ye veya broadcast'e gitmez, ticker bekler)
		h.mu.Lock()
		if h.reactionBuffer[event.RoomID] == nil {
			h.reactionBuffer[event.RoomID] = make(map[string]uint64)
		}
		h.reactionBuffer[event.RoomID][reactionName]++
		h.mu.Unlock()

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

		h.mu.RLock()
		senderClient, senderExists := roomClients[event.SenderID]
		h.mu.RUnlock()

		senderName := "User"
		var senderAvatar *string = nil

		if senderExists {
			senderName = senderClient.Name
			if senderClient.Avatar != nil {
				avatar := *senderClient.Avatar
				senderAvatar = &avatar
			}
		}

		updatedData, _ := json.Marshal(map[string]interface{}{
			"text":          textData,
			"sender_id":     event.SenderID,
			"sender_name":   senderName,
			"sender_avatar": utils.PrependBaseURL(senderAvatar),
		})
		event.Data = updatedData

		chatPayload, _ := json.Marshal(event)

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

	case "broadcast_request":
		h.mu.RLock()
		senderClient, senderExists := roomClients[event.SenderID]
		h.mu.RUnlock()

		senderName := "User"
		var senderAvatar *string = nil

		if senderExists {
			senderName = senderClient.Name
			if senderClient.Avatar != nil {
				avatar := *senderClient.Avatar
				senderAvatar = &avatar
			}
		}

		updatedData, _ := json.Marshal(map[string]interface{}{
			"sender_id":     event.SenderID,
			"sender_name":   senderName,
			"sender_avatar": utils.PrependBaseURL(senderAvatar),
		})
		event.Data = updatedData

		requestPayload, _ := json.Marshal(event)

		for _, client := range roomClients {
			if client.Role == "host" {
				select {
				case client.Send <- requestPayload:
				default:
					close(client.Send)
					go func(c *LiveRoomClient) {
						h.Unregister <- c
					}(client)
				}
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
			targetClient.Role = "audience"
			select {
			case targetClient.Send <- payload:
			default:
				close(targetClient.Send)
				go func(c *LiveRoomClient) {
					h.Unregister <- c
				}(targetClient)
			}
		}

	case "trigger_block_kick":
		var dataMap map[string]interface{}
		if err := json.Unmarshal(event.Data, &dataMap); err != nil {
			log.Printf("❌ trigger_block_kick data parse hatası: %v", err)
			return
		}

		targetUserIDFloat, ok := dataMap["target_user_id"].(float64)
		if !ok {
			return
		}

		h.EnforceBlock(event.SenderID, uint(targetUserIDFloat))
	}
}

func (h *LiveHub) EnforceBlock(blockerID, blockedID uint) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for _, clients := range h.rooms {
		if _, blockerExists := clients[blockerID]; blockerExists {
			if blockedClient, blockedExists := clients[blockedID]; blockedExists {
				kickEvent, _ := json.Marshal(map[string]interface{}{
					"type": "kicked_by_block",
					"data": map[string]string{
						"message": "Bu yayından kənarlaşdırıldınız.",
					},
				})

				select {
				case blockedClient.Send <- kickEvent:
				default:
				}

				go func(c *LiveRoomClient) {
					h.Unregister <- c
				}(blockedClient)
			}
		}
	}
}
