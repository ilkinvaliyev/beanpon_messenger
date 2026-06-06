package websocket

import (
	"beanpon_messenger/database"
	"beanpon_messenger/models"
	"beanpon_messenger/utils"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"log"
	"net/http"
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
	IsGhost    bool
	// LiveSpam — shadow ban. Əgər true-dursa, bu user-in göndərdiyi
	// chat_message / broadcast_request / reaction event-ləri otaqdaki
	// digər istifadəçilərə YAYIMLANMIR. User isə özü mesajları görür və
	// heç bir error / status almır (silent drop) — özünü bloklanmış kimi
	// hiss etməsin deyə.
	LiveSpam bool
	// IsAdmin — platforma admini (users.is_admin). Admin otağın host-u
	// OLMASA belə host-səviyyəli moderasiya səlahiyyətinə malikdir:
	// chat təmizləmə, speaker/viewer çıxarma, mute, oyun idarəsi.
	// Bağlantı qurulan an bir dəfə oxunur (HandleWebSocket). Laravel
	// LiveResource-dakı admin yoxlamasının real-time WS qarşılığıdır.
	IsAdmin bool
	Send    chan []byte
}

// canModerate — bu client otaqda host-səviyyəli moderasiya əməliyyatı
// (chat_clear, kick, mute, transfer, oyun idarəsi) edə bilərmi?
// Otağın əsl host-u VƏ YA platforma admini icazəlidir. nil client
// (məs. obyekt tapılmadı) heç vaxt icazəli deyil.
func (c *LiveRoomClient) canModerate() bool {
	return c != nil && (c.Role == "host" || c.IsAdmin)
}

// visibleCount — otaqdakı ghost və live_spam olmayan istifadəçilərin
// sayını qaytarır. Çağıran tərəf h.mu lock-u tutmalıdır (RLock kifayətdir).
func (h *LiveHub) visibleCount(roomID uint) int {
	count := 0
	if room, ok := h.rooms[roomID]; ok {
		for _, c := range room {
			if !c.IsGhost && !c.LiveSpam {
				count++
			}
		}
	}
	return count
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
			count := h.visibleCount(client.RoomID)
			h.mu.Unlock()

			log.Printf("User %d joined Live Room %d as %s (ghost=%v)", client.UserID, client.RoomID, client.Role, client.IsGhost)

			// Ghost user-lər və live_spam (shadow ban) user-ləri üçün
			// qoşulma eventi və viewer count yenilənməsi yayılmır — heç kim
			// onun otaqda olduğunu bilməməlidir. User isə bunu hiss etmir,
			// çünki connect uğurlu qaytarılmışdı.
			if !client.IsGhost && !client.LiveSpam {
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
			}

			// MAFIA: otaqda aktiv mafia oyunu varsa, qoşulan adama fərdi
			// maskalanmış vəziyyəti göndər (reconnect olan oyunçu öz rolunu,
			// yeni girən izləyici yalnız ümumi şəkli görür — buildPublicState).
			go func(roomID, userID uint) {
				if game, ok := loadGame(roomID); ok && game.Phase != MafiaPhaseEnded {
					state := game.buildPublicState(userID)
					data, _ := json.Marshal(state)
					payload, _ := json.Marshal(map[string]interface{}{
						"type":    "mafia_state_sync",
						"room_id": roomID,
						"data":    json.RawMessage(data),
					})
					h.sendToUser(roomID, userID, payload)
				}
			}(client.RoomID, client.UserID)

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
				count = h.visibleCount(client.RoomID)
				roomExists = true
				if len(room) == 0 {
					delete(h.rooms, client.RoomID)
					delete(h.reactionBuffer, client.RoomID)
					roomExists = false
				}
			}
			h.mu.Unlock()

			// Ghost / live_spam user otaqdan ayrılanda viewer count yenilənməsi
			// broadcast olunmur — onun olub-olmaması heç kim üçün
			// görünməməlidir, ona görə sayğac da dəyişməməlidir.
			if roomExists && !client.IsGhost && !client.LiveSpam {
				eventData, _ := json.Marshal(map[string]interface{}{"count": count})
				go func(e *LiveMessageEvent) { h.Broadcast <- e }(&LiveMessageEvent{
					Type: "viewer_count_update", RoomID: client.RoomID,
					Data: eventData,
				})
			}

			// MAFIA: oyunçu canlıdan çıxsa/çıxarılsa → oyunda ölü sayılır,
			// kartı hamıya açılır. (Host çıxanda oyun onsuz da bitir — aşağıda.)
			if client.Role != "host" {
				go h.mafiaHandlePlayerLeft(client.RoomID, client.UserID)
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

	// MAFIA: oyun event-ləri ayrı handler-ə yönləndirilir (mafia_flow.go).
	switch event.Type {
	case "mafia_ready", "mafia_night_action", "mafia_vote",
		"mafia_defense_end", "mafia_cancel":
		h.handleMafiaEvent(event)
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

		// Shadow ban: live_spam istifadəçisinin reaksiyaları
		// agregata yığılmır və başqalarına yayılmır. Sender heç nə
		// hiss etmir — UI öz client-də artıq oynayır.
		h.mu.RLock()
		senderClient, senderExists := roomClients[event.SenderID]
		h.mu.RUnlock()
		if senderExists && senderClient.LiveSpam {
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

		// Shadow ban: əgər sender live_spam-dırsa, mesaj otağa
		// broadcast OLUNMUR. Yalnız sender özünə echo alır ki, mesajın
		// göndərildiyini düşünsün. Heç bir error qaytarılmır.
		senderIsSpam := senderExists && senderClient.LiveSpam

		for _, client := range roomClients {
			if senderIsSpam && client.UserID != event.SenderID {
				continue
			}
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

		// Shadow ban: live_spam istifadəçisinin live-ə qoşulma /
		// danışma istəyi HEÇ KİMƏ göndərilmir (host da daxil olmaqla).
		// Sender-ə də heç bir cavab dönmür — sanki istək çıxıb gedib.
		if senderExists && senderClient.LiveSpam {
			return
		}

		// MAFIA: oyun davam edərkən kimsə yayıma qoşula bilməz. Host-a
		// təklif GETMİR; istəyən adama "oyun davam edir" bildirişi gedir.
		if game, ok := loadGame(event.RoomID); ok && game.Phase != MafiaPhaseEnded {
			denyData, _ := json.Marshal(map[string]interface{}{
				"reason": "Hazırda mafia oyunu davam edir, qoşula bilməzsiniz.",
				"code":   "MAFIA_IN_PROGRESS",
			})
			denyPayload, _ := json.Marshal(map[string]interface{}{
				"type":    "broadcast_request_denied",
				"room_id": event.RoomID,
				"data":    json.RawMessage(denyData),
			})
			if senderExists {
				select {
				case senderClient.Send <- denyPayload:
				default:
				}
			}
			return
		}

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

	case "question":
		// Host tarafindan tetiklenen icebreaker sorusu — otaqdaki hər kəsə eyni anda göstərilir
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
		h.mu.RLock()
		ksSender, ksExists := roomClients[event.SenderID]
		h.mu.RUnlock()
		if !ksExists || !ksSender.canModerate() {
			return
		}

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
		if !exists || !sender.canModerate() {
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
		if !exists || !sender.canModerate() {
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

	// HOST canlı otaqdakı chat-i hər kəs üçün təmizləyir.
	// Yalnız host icazəlidir. Mesajlar DB-dən SİLİNMİR — yalnız
	// live_rooms.chat_cleared_at = NOW() yazılır. Tarixçə endpoint-i
	// bu vaxtdan sonrakı mesajları qaytarır, ona görə pull-to-refresh
	// edənlər də boş chat görür, data isə DB-də qalır.
	case "clear_chat":
		h.mu.RLock()
		sender, exists := roomClients[event.SenderID]
		h.mu.RUnlock()
		if !exists || !sender.canModerate() {
			return
		}

		// Soft-clear: kəsmə nöqtəsini qeyd et (DELETE yox).
		go database.DB.Exec(
			"UPDATE live_rooms SET chat_cleared_at = NOW() WHERE id = ?",
			event.RoomID,
		)

		clearPayload, _ := json.Marshal(map[string]interface{}{
			"type":    "chat_cleared",
			"room_id": event.RoomID,
			"data": map[string]interface{}{
				"cleared_by": event.SenderID,
			},
		})

		h.mu.RLock()
		for _, c := range roomClients {
			select {
			case c.Send <- clearPayload:
			default:
			}
		}
		h.mu.RUnlock()

	// Speaker özünü mute edib/açıb. Otaqdakı hər kəsə yaymaq lazımdır
	// ki, onlar da o iştirakçının avatarında self-mute badge-i görsünlər.
	// Host icazəsindən fərqlidir — burada yalnız özünü mute etmə.
	case "self_mute_change":
		var dataMap map[string]interface{}
		if err := json.Unmarshal(event.Data, &dataMap); err != nil {
			log.Printf("❌ self_mute_change data parse hatası: %v", err)
			return
		}
		isMuted, _ := dataMap["is_muted"].(bool)

		mutePayload, _ := json.Marshal(map[string]interface{}{
			"type":    "self_mute_change",
			"room_id": event.RoomID,
			"data": map[string]interface{}{
				"user_id":  event.SenderID,
				"is_muted": isMuted,
			},
		})

		h.mu.RLock()
		for _, c := range roomClients {
			// Göndərənə də göndərmək lazım deyil — onun lokal UI-i
			// onsuz da güncəllidir. Amma göndərmək ziyan da vermir,
			// idempotent: lokal state ilə eyni olacaq.
			select {
			case c.Send <- mutePayload:
			default:
			}
		}
		h.mu.RUnlock()

	// Push-to-talk (anlıq mikrofon): audience istifadəçi mic düyməsini
	// basıb-tutanda `active:true`, buraxanda `active:false` göndərir.
	// Server bunu sadəcə bütün iştirakçılara ötürür — digər client-lər
	// bu istifadəçini grid-ə qoymur, yalnız səsini eşidir. Rol DƏYİŞMİR.
	case "ptt_change":
		var pttData map[string]interface{}
		if err := json.Unmarshal(event.Data, &pttData); err != nil {
			log.Printf("❌ ptt_change data parse hatası: %v", err)
			return
		}
		pttActive, _ := pttData["active"].(bool)

		// Shadow ban: live_spam istifadəçisinin PTT-si başqalarına yayılmasın.
		h.mu.RLock()
		pttSender, pttSenderExists := roomClients[event.SenderID]
		h.mu.RUnlock()
		if pttSenderExists && pttSender.LiveSpam {
			return
		}

		pttPayload, _ := json.Marshal(map[string]interface{}{
			"type":    "ptt_change",
			"room_id": event.RoomID,
			"data": map[string]interface{}{
				"user_id": event.SenderID,
				"active":  pttActive,
			},
		})

		h.mu.RLock()
		for _, c := range roomClients {
			select {
			case c.Send <- pttPayload:
			default:
			}
		}
		h.mu.RUnlock()

	case "transfer_host":
		h.mu.RLock()
		sender, exists := roomClients[event.SenderID]
		h.mu.RUnlock()
		if !exists || !sender.canModerate() {
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

		// Köçürməni edən əsl host idisə öz rolunu broadcaster-ə endirir
		// (otaq tək host-ludur). Admin (host olmayan) başqasını host
		// təyin edəndə isə öz rolu DƏYİŞMİR — o, host deyildi, sadəcə
		// moderator idi və moderator qalır.
		senderWasHost := sender.Role == "host"

		h.mu.Lock()
		if targetClient, exists := roomClients[targetUserID]; exists {
			targetClient.Role = "host"
			if senderWasHost {
				sender.Role = "broadcaster" // ← audience deyil
			}
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
		if senderWasHost {
			go database.DB.Exec(
				"UPDATE live_room_participants SET role = 'broadcaster' WHERE live_room_id = ? AND user_id = ?", // ← audience deyil
				event.RoomID, event.SenderID,
			)
		}

		hasBlocked := models.IsBlocked(database.DB, targetUserID, event.SenderID)
		transferPayload, _ := json.Marshal(map[string]interface{}{
			"type":    "host_transferred",
			"room_id": event.RoomID,
			"data": map[string]interface{}{
				"new_host_id": targetUserID,
				"old_host_id": event.SenderID,
				"has_blocked": hasBlocked,
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

	case "global_mute_user":
		h.mu.RLock()
		sender, ok := roomClients[event.SenderID]
		h.mu.RUnlock()
		if !ok || !sender.canModerate() {
			return
		}

		var d map[string]interface{}
		if err := json.Unmarshal(event.Data, &d); err != nil {
			return
		}
		targetID := uint(d["target_user_id"].(float64))

		payload, _ := json.Marshal(map[string]interface{}{
			"type":    "user_global_muted",
			"room_id": event.RoomID,
			"data":    map[string]interface{}{"target_user_id": targetID},
		})
		h.mu.RLock()
		for _, c := range roomClients {
			select {
			case c.Send <- payload:
			default:
			}
		}
		h.mu.RUnlock()

	// Host bir istifadəçinin global-mute-unu açır. Hədəf otağa "user_global_unmuted"
	// alır və öz tərəfində mikrofonunu yenidən aktiv edir. Hostdan başqa kimsə
	// bu əməliyyatı edə bilməz. (Filament admin endpoint-i `/internal/live-rooms/
	// :room_id/unmute/:user_id` da paralel olaraq mövcuddur — burası real-time
	// WS yoludur.)
	case "global_unmute_user":
		h.mu.RLock()
		sender, ok := roomClients[event.SenderID]
		h.mu.RUnlock()
		if !ok || !sender.canModerate() {
			return
		}

		var d map[string]interface{}
		if err := json.Unmarshal(event.Data, &d); err != nil {
			return
		}
		targetID := uint(d["target_user_id"].(float64))

		payload, _ := json.Marshal(map[string]interface{}{
			"type":    "user_global_unmuted",
			"room_id": event.RoomID,
			"data":    map[string]interface{}{"target_user_id": targetID},
		})
		h.mu.RLock()
		for _, c := range roomClients {
			select {
			case c.Send <- payload:
			default:
			}
		}
		h.mu.RUnlock()

	case "kick_from_live":
		h.mu.RLock()
		sender, senderExists := roomClients[event.SenderID]
		h.mu.RUnlock()
		if !senderExists || !sender.canModerate() {
			return
		}

		var d map[string]interface{}
		if err := json.Unmarshal(event.Data, &d); err != nil {
			return
		}
		targetID := uint(d["target_user_id"].(float64))

		h.mu.RLock()
		tClient, tExists := roomClients[targetID]
		h.mu.RUnlock()
		if !tExists {
			return
		}

		go func() {
			if err := database.DB.Exec(`
				INSERT INTO live_room_bans (live_room_id, user_id, created_at)
				VALUES (?, ?, NOW())
				ON CONFLICT (live_room_id, user_id) DO NOTHING
			`, event.RoomID, targetID).Error; err != nil {
				log.Printf("❌ live_room_bans insert hatası: %v", err)
			}
		}()

		payload, _ := json.Marshal(map[string]interface{}{
			"type":    "kicked_from_live",
			"room_id": event.RoomID,
			"data":    map[string]interface{}{"target_user_id": targetID},
		})
		h.mu.RLock()
		for _, c := range roomClients {
			select {
			case c.Send <- payload:
			default:
			}
		}
		h.mu.RUnlock()

		go func(c *LiveRoomClient) { h.Unregister <- c }(tClient)

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

func (h *LiveHub) ForceEndRoom(c *gin.Context) {
	roomIDStr := c.Param("room_id")
	roomID, err := strconv.ParseUint(roomIDStr, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid room_id"})
		return
	}

	endedPayload, _ := json.Marshal(map[string]interface{}{
		"type": "ended",
		"data": map[string]interface{}{},
	})

	h.mu.RLock()
	roomClients, ok := h.rooms[uint(roomID)]
	h.mu.RUnlock()

	if ok {
		for _, client := range roomClients {
			select {
			case client.Send <- endedPayload:
			default:
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{"message": "Room force ended."})
}

// ClearChat — Filament admin paneldən çağrılır.
// Canlı otaqdakı chat-i hər kəs üçün təmizləyir.
// Host-un WS üzərindən göndərdiyi "clear_chat" event-i ilə eyni nəticə:
// mesajlar DB-dən SİLİNMİR — yalnız live_rooms.chat_cleared_at = NOW()
// yazılır və otağa "chat_cleared" yayılır. Client-lər yerli state-lərini
// sıfırlayır, tarixçə endpoint-i isə bu vaxtdan sonrakı mesajları qaytarır.
func (h *LiveHub) ClearChat(c *gin.Context) {
	roomIDStr := c.Param("room_id")
	roomID, err := strconv.ParseUint(roomIDStr, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid room_id"})
		return
	}

	// Soft-clear: kəsmə nöqtəsini qeyd et (DELETE yox). Sinxron icra
	// edirik ki, cavab qaytarmadan əvvəl DB həqiqətən yenilənsin.
	if err := database.DB.Exec(
		"UPDATE live_rooms SET chat_cleared_at = NOW() WHERE id = ?",
		uint(roomID),
	).Error; err != nil {
		log.Printf("❌ ClearChat DB yeniləmə xətası (room %d): %v", roomID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to clear chat"})
		return
	}

	clearPayload, _ := json.Marshal(map[string]interface{}{
		"type":    "chat_cleared",
		"room_id": uint(roomID),
		"data": map[string]interface{}{
			"cleared_by": 0, // 0 = admin/Filament (host SenderID deyil)
		},
	})

	h.mu.RLock()
	roomClients, ok := h.rooms[uint(roomID)]
	if ok {
		for _, client := range roomClients {
			select {
			case client.Send <- clearPayload:
			default:
			}
		}
	}
	h.mu.RUnlock()

	c.JSON(http.StatusOK, gin.H{"message": "Live room chat cleared."})
}

// KickUser — Filament admin paneldən çağrılır.
// İstifadəçini canlı otaqdan çıxarır və bütün otağa "kicked_from_live" yayır.
func (h *LiveHub) KickUser(c *gin.Context) {
	roomIDStr := c.Param("room_id")
	roomID, err := strconv.ParseUint(roomIDStr, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid room_id"})
		return
	}

	userIDStr := c.Param("user_id")
	userID, err := strconv.ParseUint(userIDStr, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user_id"})
		return
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"type":    "kicked_from_live",
		"room_id": uint(roomID),
		"data":    map[string]interface{}{"target_user_id": uint(userID)},
	})

	h.mu.RLock()
	roomClients, ok := h.rooms[uint(roomID)]
	var targetClient *LiveRoomClient
	if ok {
		for _, client := range roomClients {
			select {
			case client.Send <- payload:
			default:
			}
		}
		if tc, exists := roomClients[uint(userID)]; exists {
			targetClient = tc
		}
	}
	h.mu.RUnlock()

	if targetClient != nil {
		go func(c *LiveRoomClient) { h.Unregister <- c }(targetClient)
	}

	c.JSON(http.StatusOK, gin.H{"message": "User kicked from live room."})
}

// SetLiveSpam — Filament admin paneldən çağrılır.
// İstifadəçinin shadow ban (live_spam) statusunu BÜTÜN aktiv canlı
// otaqlardakı açıq client obyektlərində REAL-TIME yeniləyir.
//
// Problem: HandleWebSocket live_spam dəyərini yalnız WS bağlantısı
// qurulan an bir dəfə oxuyub client obyektinə "dondurur". İstifadəçi
// otaqda ikən admin onu live_spam = true etsə, yaddaşdakı client
// obyekti köhnə (false) dəyəri saxladığı üçün shadow ban yalnız
// reconnect-dən sonra işə düşürdü. Bu handler həmin boşluğu bağlayır.
//
// live_spam = true olduqda: əgər user əvvəl görünən idisə, otağa onun
// "ayrıldığı" effektini veririk — viewer count azalır və user_left
// yayılır ki, qalan iştirakçılar üçün də görünməz olsun.
// live_spam = false olduqda: əksinə, user "qoşulmuş" kimi yenidən
// görünür — viewer count artır və user_joined yayılır.
//
// Qeyd: Filament users.live_spam sütununu DB-də onsuz da yeniləyir;
// bu endpoint yalnız yaddaşdakı canlı vəziyyəti sinxronlaşdırır.
func (h *LiveHub) SetLiveSpam(c *gin.Context) {
	userIDStr := c.Param("user_id")
	userID, err := strconv.ParseUint(userIDStr, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user_id"})
		return
	}

	// Body: {"live_spam": true}  — yoxdursa true qəbul edirik.
	var body struct {
		LiveSpam *bool `json:"live_spam"`
	}
	_ = c.ShouldBindJSON(&body)
	newVal := true
	if body.LiveSpam != nil {
		newVal = *body.LiveSpam
	}

	type affectedRoom struct {
		roomID   uint
		count    int
		wasGhost bool
		userName string
	}
	var affected []affectedRoom

	h.mu.Lock()
	for roomID, clients := range h.rooms {
		client, exists := clients[uint(userID)]
		if !exists {
			continue
		}
		// Real dəyişiklik yoxdursa keç.
		if client.LiveSpam == newVal {
			continue
		}
		client.LiveSpam = newVal

		ar := affectedRoom{
			roomID:   roomID,
			wasGhost: client.IsGhost,
			userName: client.Name,
		}
		// Yeni dəyər tətbiq olunduqdan sonra görünən izləyici sayı.
		ar.count = h.visibleCount(roomID)
		affected = append(affected, ar)
	}
	h.mu.Unlock()

	// Ghost user üçün heç bir görünürlük eventi yaymırıq — o, live_spam
	// statusundan asılı olmayaraq onsuz da daimi görünməzdir.
	for _, ar := range affected {
		if ar.wasGhost {
			continue
		}

		h.mu.RLock()
		roomClients, ok := h.rooms[ar.roomID]
		h.mu.RUnlock()
		if !ok {
			continue
		}

		// Viewer count yenilənməsi — hər iki istiqamətdə.
		countData, _ := json.Marshal(map[string]interface{}{"count": ar.count})
		countPayload, _ := json.Marshal(map[string]interface{}{
			"type":    "viewer_count_update",
			"room_id": ar.roomID,
			"data":    json.RawMessage(countData),
		})

		var visibilityPayload []byte
		if newVal {
			// Artıq shadow-ban: user otaqda qalanlar üçün "ayrıldı".
			d, _ := json.Marshal(map[string]interface{}{
				"user_id": uint(userID),
			})
			visibilityPayload, _ = json.Marshal(map[string]interface{}{
				"type":    "user_left",
				"room_id": ar.roomID,
				"data":    json.RawMessage(d),
			})
		} else {
			// Shadow-ban götürüldü: user yenidən "qoşuldu".
			d, _ := json.Marshal(map[string]interface{}{
				"user_id":   uint(userID),
				"user_name": ar.userName,
			})
			visibilityPayload, _ = json.Marshal(map[string]interface{}{
				"type":    "user_joined",
				"room_id": ar.roomID,
				"data":    json.RawMessage(d),
			})
		}

		for cid, client := range roomClients {
			// Hədəf user-in özünə görünürlük eventi göndərmirik —
			// o, banlandığını/açıldığını hiss etməməlidir (silent).
			if cid == uint(userID) {
				continue
			}
			select {
			case client.Send <- countPayload:
			default:
			}
			select {
			case client.Send <- visibilityPayload:
			default:
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"message":        "Live spam status synced.",
		"user_id":        uint(userID),
		"live_spam":      newVal,
		"rooms_affected": len(affected),
	})
}

// UnmuteUser — Filament admin paneldən çağrılır.
// İstifadəçinin sesini admin tərəfindən geri açır.
func (h *LiveHub) UnmuteUser(c *gin.Context) {
	roomIDStr := c.Param("room_id")
	roomID, err := strconv.ParseUint(roomIDStr, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid room_id"})
		return
	}

	userIDStr := c.Param("user_id")
	userID, err := strconv.ParseUint(userIDStr, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user_id"})
		return
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"type":    "user_global_unmuted",
		"room_id": uint(roomID),
		"data":    map[string]interface{}{"target_user_id": uint(userID)},
	})

	h.mu.RLock()
	roomClients, ok := h.rooms[uint(roomID)]
	if ok {
		for _, client := range roomClients {
			select {
			case client.Send <- payload:
			default:
			}
		}
	}
	h.mu.RUnlock()

	c.JSON(http.StatusOK, gin.H{"message": "User unmuted in live room."})
}

// MuteUser — Filament admin paneldən çağrılır.
// İstifadəçini bu otaq üçün susturur. Yalnız admin Filament üzərindən geri aça bilər.
func (h *LiveHub) MuteUser(c *gin.Context) {
	roomIDStr := c.Param("room_id")
	roomID, err := strconv.ParseUint(roomIDStr, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid room_id"})
		return
	}

	userIDStr := c.Param("user_id")
	userID, err := strconv.ParseUint(userIDStr, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user_id"})
		return
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"type":    "user_global_muted",
		"room_id": uint(roomID),
		"data":    map[string]interface{}{"target_user_id": uint(userID)},
	})

	h.mu.RLock()
	roomClients, ok := h.rooms[uint(roomID)]
	if ok {
		for _, client := range roomClients {
			select {
			case client.Send <- payload:
			default:
			}
		}
	}
	h.mu.RUnlock()

	c.JSON(http.StatusOK, gin.H{"message": "User muted in live room."})
}
