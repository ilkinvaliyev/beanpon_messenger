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
	reactionBuffer map[uint]map[uint]map[string]uint64

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
		reactionBuffer: make(map[uint]map[uint]map[string]uint64),
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

	// Buffer'ı snapshot olarak al ve anında sıfırla (Thread-safe)
	snapshots := h.reactionBuffer
	h.reactionBuffer = make(map[uint]map[uint]map[string]uint64)
	h.mu.Unlock()

	for roomID, usersReactions := range snapshots {
		h.mu.RLock()
		roomClients, ok := h.rooms[roomID]
		h.mu.RUnlock()

		if !ok {
			continue
		}

		// 1. ADIM: DB'ye yazmak için odadaki TÜM reaksiyonların toplamını hesapla
		roomTotals := make(map[string]uint64)
		for _, reactions := range usersReactions {
			for name, count := range reactions {
				roomTotals[name] += count
			}
		}

		// DB'ye async upsert
		go func(rID uint, totals map[string]uint64) {
			for reactionName, count := range totals {
				err := database.DB.Exec(`
						INSERT INTO live_room_reactions (live_room_id, reaction_name, count, created_at, updated_at)
						VALUES (?, ?, ?, NOW(), NOW())
						ON CONFLICT (live_room_id, reaction_name)
						DO UPDATE SET count = live_room_reactions.count + EXCLUDED.count, updated_at = NOW()
					`, rID, reactionName, count).Error
				if err != nil {
					log.Printf("💥 Reaction DB upsert hatası: %v", err)
				}
			}
		}(roomID, roomTotals)

		// 2. ADIM: Her kullanıcıya KENDİ GÖNDERDİĞİ HARİÇ olan sayıyı yolla
		for clientID, client := range roomClients {
			personalizedTotals := make(map[string]uint64)

			// Diğer tüm kullanıcıların gönderdiklerini topla
			for senderID, reactions := range usersReactions {
				if senderID == clientID {
					continue // ÇÖZÜM BURADA: Kendi gönderdiklerini es geç!
				}
				for name, count := range reactions {
					personalizedTotals[name] += count
				}
			}

			// Eğer başkalarından gelen reaksiyon yoksa (Sadece kendisi tıklamışsa),
			// bu kullanıcıya boşuna socket mesajı atıp Flutter'ı yorma
			if len(personalizedTotals) == 0 {
				continue
			}

			eventData, _ := json.Marshal(map[string]interface{}{
				"reactions": personalizedTotals,
			})
			payload, _ := json.Marshal(map[string]interface{}{
				"type":    "reaction_update",
				"room_id": roomID,
				"data":    json.RawMessage(eventData),
			})

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
		var dataMap map[string]interface{}
		if err := json.Unmarshal(event.Data, &dataMap); err != nil {
			log.Printf("❌ reaction data parse hatası: %v", err)
			return
		}

		reactionName, ok := dataMap["name"].(string)
		if !ok || reactionName == "" {
			return
		}

		validReactions := map[string]bool{
			"heart": true, "like": true, "laugh": true,
			"withyou": true, "cry": true, "angry": true, "dislike": true,
		}
		if !validReactions[reactionName] {
			return
		}

		h.mu.Lock()
		// YENİ: Oda yoksa oluştur
		if h.reactionBuffer[event.RoomID] == nil {
			h.reactionBuffer[event.RoomID] = make(map[uint]map[string]uint64)
		}
		// YENİ: Kullanıcı yoksa oluştur
		if h.reactionBuffer[event.RoomID][event.SenderID] == nil {
			h.reactionBuffer[event.RoomID][event.SenderID] = make(map[string]uint64)
		}
		// O kullanıcının attığı emojiyi 1 artır
		h.reactionBuffer[event.RoomID][event.SenderID][reactionName]++
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
