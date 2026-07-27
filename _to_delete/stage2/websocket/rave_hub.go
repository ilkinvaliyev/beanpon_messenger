package websocket

import (
	"beanpon_messenger/database"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type RaveClient struct {
	Hub    *RaveHub
	Conn   *websocket.Conn
	UserID uint
	RaveID uint
	IsHost bool
	Name   string
	Avatar *string
	Send   chan []byte

	// closeOnce/done — Issue 23 naxışı (bax websocket/hub.go Client).
	// `Send` HEÇ VAXT bağlanmır; kapanış siqnalı `done`-dur. Əvvəl
	// `close(client.Send)` həm `Unregister` budağında (yazma kilidi altında),
	// həm də `broadcastToRoom`-un "yavaş istehlakçı" budağında (KİLİDSİZ)
	// çağırılırdı → ikiqat close və/və ya bağlı kanala yazı → proses çöküşü.
	closeOnce sync.Once
	done      chan struct{}
}

// NewRaveClient — RaveClient-i düzgün qurur. `done` ixrac olunmadığı üçün
// paket XARİCİNDƏ struct literal ilə qurulan client-də nil qalardı (nil kanal
// bağlamaq panic verir, ondan oxumaq isə əbədi bloklayır) — ona görə qurma
// buradan keçir.
func NewRaveClient(hub *RaveHub, conn *websocket.Conn, userID, raveID uint, isHost bool, name string, avatar *string) *RaveClient {
	return &RaveClient{
		Hub:    hub,
		Conn:   conn,
		UserID: userID,
		RaveID: raveID,
		IsHost: isHost,
		Name:   name,
		Avatar: avatar,
		Send:   make(chan []byte, 256),
		done:   make(chan struct{}),
	}
}

// closeSend — idempotent kapanış siqnalı (`Send` bağlanmır).
func (c *RaveClient) closeSend() {
	if c == nil {
		return
	}
	c.closeOnce.Do(func() {
		if c.done != nil {
			close(c.done)
		}
	})
}

// raveRoomSnapshot — otaq client-lərinin KİLİD ALTINDA götürülmüş nüsxəsi.
// Fan-out `h.mu` altında edilməməlidir: bir client üçün gözləmə bütün hub-ı
// (register/unregister daxil) dondurur. Bax hub.go `statusTargetsLocked`.
func (h *RaveHub) raveRoomSnapshot(raveID uint) []*RaveClient {
	h.mu.RLock()
	room, ok := h.rooms[raveID]
	if !ok {
		h.mu.RUnlock()
		return nil
	}
	out := make([]*RaveClient, 0, len(room))
	for _, c := range room {
		out = append(out, c)
	}
	h.mu.RUnlock()
	return out
}

type RaveHub struct {
	rooms      map[uint]map[uint]*RaveClient
	Register   chan *RaveClient
	Unregister chan *RaveClient
	Broadcast  chan *RaveEvent
	mu         sync.RWMutex
}

type RaveEvent struct {
	Type     string          `json:"type"`
	SenderID uint            `json:"sender_id"`
	RaveID   uint            `json:"rave_id"`
	Data     json.RawMessage `json:"data"`
}

func NewRaveHub() *RaveHub {
	return &RaveHub{
		rooms:      make(map[uint]map[uint]*RaveClient),
		Register:   make(chan *RaveClient),
		Unregister: make(chan *RaveClient),
		Broadcast:  make(chan *RaveEvent),
	}
}

func (h *RaveHub) Run() {
	for {
		select {
		case client := <-h.Register:
			h.mu.Lock()
			if _, ok := h.rooms[client.RaveID]; !ok {
				h.rooms[client.RaveID] = make(map[uint]*RaveClient)
			}
			h.rooms[client.RaveID][client.UserID] = client
			h.mu.Unlock()

			log.Printf("User %d joined Rave %d (host=%v)", client.UserID, client.RaveID, client.IsHost)

			// Yeni katılan host'tan sync ister
			h.sendSyncRequest(client)

		case client := <-h.Unregister:
			h.mu.Lock()
			if room, ok := h.rooms[client.RaveID]; ok {
				if _, ok := room[client.UserID]; ok {
					delete(room, client.UserID)
					client.closeSend()
				}
				if len(room) == 0 {
					delete(h.rooms, client.RaveID)
				}
			}
			h.mu.Unlock()

			log.Printf("User %d left Rave %d", client.UserID, client.RaveID)

		case event := <-h.Broadcast:
			h.handleRaveEvent(event)
		}
	}
}

func (h *RaveHub) sendSyncRequest(newClient *RaveClient) {
	if newClient.IsHost {
		return
	}

	// Host'u bul — snapshot üzərində (map-i kiliddən kənarda gəzmək racedir).
	var host *RaveClient
	for _, c := range h.raveRoomSnapshot(newClient.RaveID) {
		if c.IsHost {
			host = c
			break
		}
	}

	if host == nil {
		return
	}

	// Host'a sync_request gönder
	payload, _ := json.Marshal(map[string]interface{}{
		"type":    "sync_request",
		"rave_id": newClient.RaveID,
		"data": map[string]interface{}{
			"requester_id": newClient.UserID,
		},
	})

	select {
	case host.Send <- payload:
	default:
	}
}

func (h *RaveHub) handleRaveEvent(event *RaveEvent) {
	h.mu.RLock()
	room, ok := h.rooms[event.RaveID]
	h.mu.RUnlock()

	if !ok {
		return
	}

	// Sender'ı bul
	h.mu.RLock()
	sender, senderExists := room[event.SenderID]
	h.mu.RUnlock()

	switch event.Type {

	case "video_play", "video_pause", "video_seek":
		// Sadece host gönderebilir
		if !senderExists || !sender.IsHost {
			return
		}

		var dataMap map[string]interface{}
		if err := json.Unmarshal(event.Data, &dataMap); err != nil {
			return
		}

		positionMs, _ := dataMap["position_ms"].(float64)
		isPlaying := event.Type == "video_play"

		// DB'ye yaz
		go database.DB.Exec(
			`UPDATE raves SET position_ms = ?, is_playing = ?, position_updated_at = NOW() WHERE id = ?`,
			uint64(positionMs), isPlaying, event.RaveID,
		)

		// Timestamp ekle — client drift hesabı için
		dataMap["timestamp"] = time.Now().UnixMilli()
		updatedData, _ := json.Marshal(dataMap)

		payload, _ := json.Marshal(map[string]interface{}{
			"type":      event.Type,
			"rave_id":   event.RaveID,
			"sender_id": event.SenderID,
			"data":      json.RawMessage(updatedData),
		})

		h.broadcastToRoom(event.RaveID, event.SenderID, payload, false)

	case "video_change":
		// Sadece host gönderebilir
		if !senderExists || !sender.IsHost {
			return
		}

		var dataMap map[string]interface{}
		if err := json.Unmarshal(event.Data, &dataMap); err != nil {
			return
		}

		provider, _ := dataMap["provider"].(string)
		videoID, _ := dataMap["video_id"].(string)
		title, _ := dataMap["title"].(string)

		if provider == "" || videoID == "" {
			return
		}

		// DB'ye yaz
		go database.DB.Exec(
			`UPDATE raves SET provider = ?, provider_video_id = ?, title = ?, position_ms = 0, is_playing = true, position_updated_at = NOW() WHERE id = ?`,
			provider, videoID, title, event.RaveID,
		)

		dataMap["timestamp"] = time.Now().UnixMilli()
		updatedData, _ := json.Marshal(dataMap)

		payload, _ := json.Marshal(map[string]interface{}{
			"type":      "video_change",
			"rave_id":   event.RaveID,
			"sender_id": event.SenderID,
			"data":      json.RawMessage(updatedData),
		})

		h.broadcastToRoom(event.RaveID, event.SenderID, payload, true)

	case "video_sync":
		// Host sync_request'e cevap veriyor
		if !senderExists || !sender.IsHost {
			return
		}

		var dataMap map[string]interface{}
		if err := json.Unmarshal(event.Data, &dataMap); err != nil {
			return
		}

		requesterIDFloat, ok := dataMap["requester_id"].(float64)
		if !ok {
			return
		}
		requesterID := uint(requesterIDFloat)

		dataMap["timestamp"] = time.Now().UnixMilli()
		updatedData, _ := json.Marshal(dataMap)

		payload, _ := json.Marshal(map[string]interface{}{
			"type":      "video_sync",
			"rave_id":   event.RaveID,
			"sender_id": event.SenderID,
			"data":      json.RawMessage(updatedData),
		})

		// Sadece requester'a gönder.
		// Issue 23: hədəf kilid altında TAPILIR, kanal yazısı kilid
		// BURAXILDIQDAN sonra edilir.
		h.mu.RLock()
		target, exists := room[requesterID]
		h.mu.RUnlock()
		if exists {
			select {
			case target.Send <- payload:
			default:
			}
		}

	case "chat_message":
		var dataMap map[string]interface{}
		if err := json.Unmarshal(event.Data, &dataMap); err != nil {
			return
		}

		text, _ := dataMap["text"].(string)
		if text == "" {
			return
		}

		senderName := "User"
		var senderAvatar *string
		if senderExists {
			senderName = sender.Name
			if sender.Avatar != nil {
				a := *sender.Avatar
				senderAvatar = &a
			}
		}

		// DB'ye kaydet
		type RaveMessage struct {
			ID        uint   `gorm:"primaryKey;autoIncrement"`
			RaveID    uint   `gorm:"column:rave_id"`
			UserID    uint   `gorm:"column:user_id"`
			Text      string `gorm:"column:text"`
			CreatedAt time.Time
		}

		msg := RaveMessage{
			RaveID: event.RaveID,
			UserID: event.SenderID,
			Text:   text,
		}
		database.DB.Table("rave_messages").Create(&msg)

		updatedData, _ := json.Marshal(map[string]interface{}{
			"id":            msg.ID,
			"text":          text,
			"sender_id":     event.SenderID,
			"sender_name":   senderName,
			"sender_avatar": senderAvatar,
		})

		payload, _ := json.Marshal(map[string]interface{}{
			"type":      "chat_message",
			"rave_id":   event.RaveID,
			"sender_id": event.SenderID,
			"data":      json.RawMessage(updatedData),
		})

		h.broadcastToRoom(event.RaveID, 0, payload, true)

	case "rave_kick":
		// Sadece host kick edebilir
		if !senderExists || !sender.IsHost {
			return
		}

		var dataMap map[string]interface{}
		if err := json.Unmarshal(event.Data, &dataMap); err != nil {
			return
		}

		targetIDFloat, ok := dataMap["target_user_id"].(float64)
		if !ok {
			return
		}
		targetID := uint(targetIDFloat)

		kickPayload, _ := json.Marshal(map[string]interface{}{
			"type":    "rave_kicked",
			"rave_id": event.RaveID,
			"data":    map[string]interface{}{},
		})

		h.mu.RLock()
		target, exists := room[targetID]
		h.mu.RUnlock()

		if exists {
			select {
			case target.Send <- kickPayload:
			default:
			}
			go func(c *RaveClient) {
				h.Unregister <- c
			}(target)
		}

	case "ping":
		if !senderExists {
			return
		}
		pong, _ := json.Marshal(map[string]string{"type": "pong"})
		select {
		case sender.Send <- pong:
		default:
		}
	}
}

// broadcastToRoom — includeSender=true ise sender'a da gönder.
//
// Issue 23: əvvəl `h.rooms[raveID]` map-i kilid altında OXUNUB, sonra kiliddən
// KƏNARDA gəzilirdi — paralel `Register`/`Unregister` həmin map-ə yazdığı üçün
// bu, sırf data race idi (Go runtime "concurrent map iteration and map write"
// ilə prosesi öldürə bilər). İndi kilid altında SNAPSHOT alınır.
func (h *RaveHub) broadcastToRoom(raveID uint, senderID uint, payload []byte, includeSender bool) {
	for _, client := range h.raveRoomSnapshot(raveID) {
		if !includeSender && client.UserID == senderID {
			continue
		}
		select {
		case client.Send <- payload:
		default:
			client.closeSend()
			go func(c *RaveClient) { h.Unregister <- c }(client)
		}
	}
}

func (c *RaveClient) ReadPump() {
	defer func() {
		c.Hub.Unregister <- c
		c.Conn.Close()
	}()

	c.Conn.SetReadLimit(4096)
	c.Conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	c.Conn.SetPongHandler(func(string) error {
		c.Conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		return nil
	})

	for {
		_, message, err := c.Conn.ReadMessage()
		if err != nil {
			break
		}

		var incoming map[string]interface{}
		if err := json.Unmarshal(message, &incoming); err != nil {
			continue
		}

		eventType, _ := incoming["type"].(string)
		if eventType == "" {
			continue
		}

		var dataRaw json.RawMessage
		if data, exists := incoming["data"]; exists {
			dataRaw, _ = json.Marshal(data)
		} else {
			dataRaw = json.RawMessage("{}")
		}

		c.Hub.Broadcast <- &RaveEvent{
			Type:     eventType,
			SenderID: c.UserID,
			RaveID:   c.RaveID,
			Data:     dataRaw,
		}
	}
}

func (c *RaveClient) WritePump() {
	ticker := time.NewTicker(30 * time.Second)
	defer func() {
		ticker.Stop()
		err := c.Conn.Close()
		if err != nil {
			return
		}
	}()

	for {
		select {
		// `Send` bağlanmadığı üçün kapanış siqnalı buradan gəlir.
		case <-c.done:
			_ = c.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			_ = c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
			return

		case message, ok := <-c.Send:
			err := c.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err != nil {
				return
			}
			if !ok {
				err := c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				if err != nil {
					return
				}
				return
			}
			if err := c.Conn.WriteMessage(websocket.TextMessage, message); err != nil {
				return
			}

		case <-ticker.C:
			err := c.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err != nil {
				return
			}
			if err := c.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
