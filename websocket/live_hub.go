package websocket

import (
	"beanpon_messenger/database"
	"beanpon_messenger/models"
	"beanpon_messenger/utils"
	"encoding/json"
	"log"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type LiveRoomClient struct {
	Hub        *LiveHub
	Conn       *websocket.Conn
	UserID     uint
	RoomID     uint
	Role       string
	Name       string
	Avatar     *string
	AvatarType *string
	Send       chan []byte
}

type LiveHub struct {
	rooms          map[uint]map[uint]*LiveRoomClient
	reactionBuffer map[uint]map[uint]map[string]uint64
	Register       chan *LiveRoomClient
	Unregister     chan *LiveRoomClient
	Broadcast      chan *LiveMessageEvent
	mu             sync.RWMutex
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
			go func(e *LiveMessageEvent) { h.Broadcast <- e }(&LiveMessageEvent{
				Type: "viewer_count_update", RoomID: client.RoomID,
				Data: eventData,
			})

			joinData, _ := json.Marshal(map[string]interface{}{
				"user_id":   client.UserID,
				"user_name": client.Name,
			})
			go func(roomID uint, senderID uint, data json.RawMessage) {
				h.mu.RLock()
				clients := h.rooms[roomID]
				h.mu.RUnlock()
				payload, _ := json.Marshal(map[string]interface{}{
					"type":      "user_joined",
					"sender_id": senderID,
					"room_id":   roomID,
					"data":      data,
				})
				for _, c := range clients {
					select {
					case c.Send <- payload:
					default:
					}
				}
			}(client.RoomID, client.UserID, joinData)

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
					delete(h.reactionBuffer, client.RoomID)
					roomExists = false
				}
			}
			h.mu.Unlock()

			if roomExists {
				eventData, _ := json.Marshal(map[string]interface{}{"count": count})
				go func(e *LiveMessageEvent) { h.Broadcast <- e }(&LiveMessageEvent{
					Type: "viewer_count_update", RoomID: client.RoomID,
					Data: eventData,
				})
			}

			if client.Role == "host" {
				err := database.DB.Exec("UPDATE live_rooms SET status = 'ended', ended_at = NOW() WHERE id = ?", client.RoomID).Error
				if err != nil {
					log.Printf("❌ DB Update Error (Room Ended): %v", err)
				}

				database.DB.Exec("UPDATE live_room_participants SET status = 'left', left_at = NOW() WHERE live_room_id = ?", client.RoomID)

				endedPayload, _ := json.Marshal(map[string]interface{}{
					"type": "ended",
					"data": map[string]interface{}{},
				})

				h.mu.RLock()
				if roomClients, ok := h.rooms[client.RoomID]; ok {
					for _, c := range roomClients {
						select {
						case c.Send <- endedPayload:
						default:
						}
					}
				}
				h.mu.RUnlock()
			}

		case event := <-h.Broadcast:
			h.handleEvent(event)

		case <-reactionTicker.C:
			h.flushReactions()
		}
	}
}

func (h *LiveHub) flushReactions() {
	h.mu.Lock()
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

		roomTotals := make(map[string]uint64)
		for _, reactions := range usersReactions {
			for name, count := range reactions {
				roomTotals[name] += count
			}
		}

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

		for clientID, client := range roomClients {
			personalizedTotals := make(map[string]uint64)

			for senderID, reactions := range usersReactions {
				if senderID == clientID {
					continue
				}
				for name, count := range reactions {
					personalizedTotals[name] += count
				}
			}

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
		if h.reactionBuffer[event.RoomID] == nil {
			h.reactionBuffer[event.RoomID] = make(map[uint]map[string]uint64)
		}
		if h.reactionBuffer[event.RoomID][event.SenderID] == nil {
			h.reactionBuffer[event.RoomID][event.SenderID] = make(map[string]uint64)
		}
		h.reactionBuffer[event.RoomID][event.SenderID][reactionName]++
		h.mu.Unlock()

	case "chat_message":
		var dataMap map[string]interface{}
		if err := json.Unmarshal(event.Data, &dataMap); err != nil {
			log.Printf("❌ chat_message data parse hatası: %v", err)
			return
		}

		textData, _ := dataMap["text"].(string)
		if utils.ContainsBadWord(textData) {
			// göndərənə xəbər ver
			errPayload, _ := json.Marshal(map[string]interface{}{
				"type": "message_blocked",
				"data": map[string]string{
					"reason": "Mesajınız uygunsuz içerik nedeniyle göndərilmədi.",
				},
			})
			h.mu.RLock()
			if sender, ok := roomClients[event.SenderID]; ok {
				select {
				case sender.Send <- errPayload:
				default:
				}
			}
			h.mu.RUnlock()
			return
		}

		gifURL, _ := dataMap["gif_url"].(string)
		imageURL, _ := dataMap["image_url"].(string)

		if textData == "" && gifURL == "" && imageURL == "" {
			return
		}

		var replyToID *uint
		if replyVal, exists := dataMap["reply_to_id"]; exists && replyVal != nil {
			switch v := replyVal.(type) {
			case float64:
				if v > 0 {
					id := uint(v)
					replyToID = &id
				}
			case string:
				if parsed, err := strconv.ParseUint(v, 10, 64); err == nil && parsed > 0 {
					id := uint(parsed)
					replyToID = &id
				}
			}
			//log.Printf("🔍 reply_to_id: raw=%v, parsed=%v", replyVal, replyToID)
		}

		h.mu.RLock()
		senderClient, senderExists := roomClients[event.SenderID]
		h.mu.RUnlock()

		senderName := "User"
		var senderAvatar *string
		if senderExists {
			senderName = senderClient.Name
			if senderClient.Avatar != nil {
				avatar := *senderClient.Avatar
				senderAvatar = &avatar
			}
		}

		var replyPreview interface{} = nil
		if replyToID != nil {
			type replyRow struct {
				ID           uint
				Text         string
				SenderID     uint
				SenderName   string
				SenderAvatar *string
			}
			var rr replyRow
			err := database.DB.Table("live_room_messages lm").
				Select("lm.id, lm.text, lm.sender_id, u.name as sender_name, p.profile_image as sender_avatar").
				Joins("LEFT JOIN users u ON u.id = lm.sender_id").
				Joins("LEFT JOIN profiles p ON p.user_id = lm.sender_id").
				Where("lm.id = ?", *replyToID).
				Scan(&rr).Error
			if err == nil && rr.ID != 0 {
				replyPreview = map[string]interface{}{
					"id":          rr.ID,
					"text":        rr.Text,
					"sender_id":   rr.SenderID,
					"sender_name": rr.SenderName,
					"sender_avatar": func() *string {
						if senderExists && senderClient.AvatarType != nil && *senderClient.AvatarType == "gif" {
							return senderAvatar
						}
						return utils.PrependBaseURL(senderAvatar)
					}(),
					"sender_avatar_type": func() *string {
						if senderExists {
							return senderClient.AvatarType
						}
						return nil
					}(),
				}
			}
		}

		// URL-ləri tam et
		var fullImageURL string
		if imageURL != "" {
			if w := utils.PrependS3URL(&imageURL); w != nil {
				fullImageURL = *w
			}
		}
		var fullGifURL string
		if gifURL != "" {
			if w := utils.PrependS3URL(&gifURL); w != nil {
				fullGifURL = *w
			}
		}

		// DB kayıt
		chatMsg := models.LiveRoomMessage{
			LiveRoomID: event.RoomID,
			SenderID:   event.SenderID,
			Text:       textData,
			ReplyToID:  replyToID,
		}
		if fullImageURL != "" {
			chatMsg.ImageURL = &fullImageURL
		}
		if fullGifURL != "" {
			chatMsg.GifURL = &fullGifURL
		}

		if err := database.DB.Create(&chatMsg).Error; err != nil {
			log.Printf("💥 DB KAYIT HATASI: %v", err)
		}

		// DB-dən sonra marshal et — ID artıq doludur
		mentions := utils.ParseMentions(textData)

		updatedData, _ := json.Marshal(map[string]interface{}{
			"id":          chatMsg.ID,
			"text":        textData,
			"gif_url":     fullGifURL,
			"image_url":   fullImageURL,
			"sender_id":   event.SenderID,
			"sender_name": senderName,
			"sender_avatar": func() *string {
				if senderClient.AvatarType != nil && *senderClient.AvatarType == "gif" {
					return senderAvatar
				}
				return utils.PrependBaseURL(senderAvatar)
			}(),
			"sender_avatar_type": func() *string {
				if senderExists {
					return senderClient.AvatarType
				}
				return nil
			}(),
			"reply_to": replyPreview,
			"mentions": mentions,
		})
		event.Data = updatedData

		chatPayload, _ := json.Marshal(event)

		for _, client := range roomClients {
			if client.UserID != event.SenderID {
				if models.IsBlocked(database.DB, event.SenderID, client.UserID) {
					continue
				}
			}
			select {
			case client.Send <- chatPayload:
			default:
				close(client.Send)
				go func(c *LiveRoomClient) { h.Unregister <- c }(client)
			}
		}

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

	case "game_spin":
		h.mu.RLock()
		sender, exists := roomClients[event.SenderID]
		h.mu.RUnlock()
		if !exists || sender.Role != "host" {
			return
		}

		h.mu.RLock()
		var eligible []map[string]interface{}
		for _, c := range roomClients {
			if c.Role == "host" || c.Role == "broadcaster" {
				eligible = append(eligible, map[string]interface{}{
					"user_id": c.UserID,
					"name":    c.Name,
					"avatar": func() *string {
						if c.AvatarType != nil && *c.AvatarType == "gif" {
							return c.Avatar
						}
						return utils.PrependBaseURL(c.Avatar)
					}(),
					"avatar_type": c.AvatarType,
					"role":        c.Role,
				})
			}
		}
		h.mu.RUnlock()

		if len(eligible) == 0 {
			return
		}

		// user_id'ye göre sırala — tüm cihazlarda aynı pozisyon
		sort.Slice(eligible, func(i, j int) bool {
			iID := eligible[i]["user_id"].(uint)
			jID := eligible[j]["user_id"].(uint)
			return iID < jID
		})

		selected := eligible[time.Now().UnixNano()%int64(len(eligible))]
		durationMs := 5000 + int(time.Now().UnixNano()%4000)
		targetAngle := float64(durationMs) * 0.8

		gameState := map[string]interface{}{
			"type": "bottle",
			"state": map[string]interface{}{
				"selected_user":   selected,
				"target_angle":    targetAngle,
				"duration_ms":     durationMs,
				"ordered_players": eligible,
			},
			"started_at": time.Now().UTC(),
		}
		gameStateJSON, _ := json.Marshal(gameState)

		database.DB.Exec(
			"UPDATE live_rooms SET active_game = ? WHERE id = ?",
			string(gameStateJSON), event.RoomID,
		)

		resultData, _ := json.Marshal(map[string]interface{}{
			"type":  "bottle",
			"state": gameState["state"],
		})

		spinPayload, _ := json.Marshal(map[string]interface{}{
			"type":    "game_spin_result",
			"room_id": event.RoomID,
			"data":    json.RawMessage(resultData),
		})

		h.mu.RLock()
		for _, c := range roomClients {
			select {
			case c.Send <- spinPayload:
			default:
			}
		}
		h.mu.RUnlock()

	case "game_stop":
		h.mu.RLock()
		sender, exists := roomClients[event.SenderID]
		h.mu.RUnlock()
		if !exists || sender.Role != "host" {
			return
		}

		database.DB.Exec("UPDATE live_rooms SET active_game = NULL WHERE id = ?", event.RoomID)

		stopPayload, _ := json.Marshal(map[string]interface{}{
			"type":    "game_stopped",
			"room_id": event.RoomID,
			"data":    map[string]interface{}{},
		})

		h.mu.RLock()
		for _, c := range roomClients {
			select {
			case c.Send <- stopPayload:
			default:
			}
		}
		h.mu.RUnlock()

	case "transfer_host":
		h.mu.RLock()
		sender, exists := roomClients[event.SenderID]
		h.mu.RUnlock()
		if !exists || sender.Role != "host" {
			return
		}

		var dataMap map[string]interface{}
		if err := json.Unmarshal(event.Data, &dataMap); err != nil {
			return
		}
		targetUserIDFloat, ok := dataMap["target_user_id"].(float64)
		if !ok {
			return
		}
		targetUserID := uint(targetUserIDFloat)

		h.mu.Lock()
		if targetClient, exists := roomClients[targetUserID]; exists {
			targetClient.Role = "host"
			sender.Role = "broadcaster" // ← audience deyil
		}
		h.mu.Unlock()

		go database.DB.Exec(
			"UPDATE live_rooms SET host_user_id = ? WHERE id = ?",
			targetUserID, event.RoomID,
		)
		go database.DB.Exec(
			"UPDATE live_room_participants SET role = 'host' WHERE live_room_id = ? AND user_id = ?",
			event.RoomID, targetUserID,
		)
		go database.DB.Exec(
			"UPDATE live_room_participants SET role = 'broadcaster' WHERE live_room_id = ? AND user_id = ?", // ← audience deyil
			event.RoomID, event.SenderID,
		)

		transferPayload, _ := json.Marshal(map[string]interface{}{
			"type":    "host_transferred",
			"room_id": event.RoomID,
			"data": map[string]interface{}{
				"new_host_id": targetUserID,
				"old_host_id": event.SenderID,
			},
		})

		h.mu.RLock()
		for _, c := range roomClients {
			select {
			case c.Send <- transferPayload:
			default:
			}
		}
		h.mu.RUnlock()

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
