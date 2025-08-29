package websocket

import (
	"beanpon_messenger/config"
	"beanpon_messenger/models"
	"bytes"
	"encoding/json"
	"github.com/google/uuid"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"gorm.io/gorm"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// Client WebSocket bağlantısını temsil eder
type Client struct {
	UserID uint
	Conn   *websocket.Conn
	Send   chan []byte
	Hub    *Hub
}

// Hub tüm client'ları yönetir
type Hub struct {
	clients           map[uint]*Client
	register          chan *Client
	unregister        chan *Client
	broadcast         chan *Message
	mutex             sync.RWMutex
	db                *gorm.DB
	encryptionService interface {
		EncryptMessage(plainText string) (string, error)
		DecryptMessage(encryptedText string) (string, error)
	}
	httpClient *http.Client   // ← YENI
	config     *config.Config // ← YENI
}

// IncomingMessage client'tan gelen mesaj yapısı
type IncomingMessage struct {
	Type       string      `json:"type"`
	ReceiverID uint        `json:"receiver_id,omitempty"`
	Content    string      `json:"content,omitempty"`
	Data       interface{} `json:"data,omitempty"`
}

// OutgoingMessage client'a gönderilen mesaj yapısı
type OutgoingMessage struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

// Message WebSocket mesaj yapısı (broadcast için)
type Message struct {
	Type       string      `json:"type"`
	ReceiverID uint        `json:"receiver_id"`
	Data       interface{} `json:"data"`
}

// MessageData veritabanı mesaj yapısı
type MessageData struct {
	ID         string    `json:"id"`
	SenderID   uint      `json:"sender_id"`
	ReceiverID uint      `json:"receiver_id"`
	Text       string    `json:"text"`
	Read       bool      `json:"read"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// NewHub yeni hub oluştur
func NewHub(db *gorm.DB, encryptionService interface {
	EncryptMessage(plainText string) (string, error)
	DecryptMessage(encryptedText string) (string, error)
}, config *config.Config) *Hub { // ← config parametri əlavə
	return &Hub{
		clients:           make(map[uint]*Client),
		register:          make(chan *Client),
		unregister:        make(chan *Client),
		broadcast:         make(chan *Message),
		db:                db,
		encryptionService: encryptionService,
		httpClient:        &http.Client{Timeout: 10 * time.Second}, // ← YENI
		config:            config,                                  // ← YENI
	}
}

// Run hub'ı çalıştır
func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.registerClient(client)

		case client := <-h.unregister:
			h.unregisterClient(client)

		case message := <-h.broadcast:
			h.broadcastMessage(message)
		}
	}
}

// registerClient client'ı kaydet
func (h *Hub) registerClient(client *Client) {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	if existingClient, exists := h.clients[client.UserID]; exists {
		delete(h.clients, existingClient.UserID)
		select {
		case <-existingClient.Send:
		default:
			close(existingClient.Send)
		}
		existingClient.Conn.Close()
		log.Printf("Kullanıcı %d eski bağlantısı temizlendi", client.UserID)
	}

	h.clients[client.UserID] = client
	log.Printf("Kullanıcı %d WebSocket'e bağlandı", client.UserID)

	// Kullanıcı online durumunu diğer kullanıcılara bildir
	h.broadcastUserStatus(client.UserID, "online")

	//İlk bağlantıda okunmamış mesaj sayısını gönder
	go h.SendUnreadCountUpdate(client.UserID)

	// Bağlandıktan sonra son 30 mesajı gönder
	go h.sendRecentMessages(client)
}

// unregisterClient client'ı çıkar
func (h *Hub) unregisterClient(client *Client) {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	if _, exists := h.clients[client.UserID]; exists {
		delete(h.clients, client.UserID)

		select {
		case <-client.Send:
		default:
			close(client.Send)
		}

		client.Conn.Close()
		log.Printf("Kullanıcı %d WebSocket'ten ayrıldı", client.UserID)

		// Kullanıcı offline durumunu diğer kullanıcılara bildir
		h.broadcastUserStatus(client.UserID, "offline")
	}
}

// broadcastUserStatus kullanıcı durumunu yayınla
func (h *Hub) broadcastUserStatus(userID uint, status string) {
	statusMessage := &Message{
		Type: "user_status",
		Data: map[string]interface{}{
			"user_id": userID,
			"status":  status,
		},
	}

	// Tüm bağlı kullanıcılara gönder
	for _, client := range h.clients {
		if client.UserID != userID { // Kendisi hariç
			select {
			case client.Send <- h.messageToBytes(statusMessage):
			default:
				go func(c *Client) {
					h.unregister <- c
				}(client)
			}
		}
	}
}

// broadcastMessage mesajı yayınla
func (h *Hub) broadcastMessage(message *Message) {
	h.mutex.RLock()
	defer h.mutex.RUnlock()

	if client, exists := h.clients[message.ReceiverID]; exists {
		select {
		case client.Send <- h.messageToBytes(message):
		default:
			go func() {
				h.unregister <- client
			}()
		}
	}
}

// SendToUser belirli kullanıcıya mesaj gönder
func (h *Hub) SendToUser(userID uint, messageType string, data interface{}) {
	message := &Message{
		Type:       messageType,
		ReceiverID: userID,
		Data:       data,
	}
	h.broadcast <- message
}

// SendToMultipleUsers birden fazla kullanıcıya mesaj gönder
func (h *Hub) SendToMultipleUsers(userIDs []uint, messageType string, data interface{}) {
	for _, userID := range userIDs {
		h.SendToUser(userID, messageType, data)
	}
}

// HandleNewMessage yeni mesajı handle et ve WebSocket üzerinden yayınla
func (h *Hub) HandleNewMessage(senderID, receiverID uint, messageID, content, msgType string, createdAt time.Time, replyToMessageID *string) {
	messageData := map[string]interface{}{
		"id":                  messageID,
		"sender_id":           senderID,
		"receiver_id":         receiverID,
		"reply_to_message_id": replyToMessageID, // YENİ
		"text":                content,
		"type":                msgType,
		"read":                false,
		"created_at":          createdAt.Format(time.RFC3339),
		"is_history":          false,
	}

	if replyToMessageID != nil {
		var replyMessage models.Message
		if err := h.db.Where("id = ?", *replyToMessageID).First(&replyMessage).Error; err == nil {
			replyDecryptedText, err := h.encryptionService.DecryptMessage(replyMessage.EncryptedText)
			if err != nil {
				replyDecryptedText = "Mesaj çözülemedi"
			}

			messageData["reply_to_message"] = map[string]interface{}{
				"id":        replyMessage.ID,
				"sender_id": replyMessage.SenderID,
				"text":      replyDecryptedText,
				//"type":       replyMessage.Type,
				"created_at": replyMessage.CreatedAt,
			}
		}
	}

	// Hem gönderen hem de alıcıya gönder
	userIDs := []uint{senderID, receiverID}
	h.SendToMultipleUsers(userIDs, "new_message", messageData)

	h.sendConversationUpdate(senderID, receiverID, messageData)

	go h.SendUnreadCountUpdate(receiverID)

	if !h.IsUserOnline(receiverID) {
		h.sendPushNotification(senderID, receiverID, content, msgType)
	}

	log.Printf("Yeni mesaj WebSocket üzerinden yayınlandı: %s -> %d", messageID, receiverID)
}

// Bu yeni fonksiyonu ekle
func (h *Hub) sendConversationUpdate(senderID, receiverID uint, messageData map[string]interface{}) {
	// Gönderen ve alıcının conversation listelerini güncelle
	conversationData := map[string]interface{}{
		"type":              "conversation_update",
		"message_data":      messageData,
		"other_user_id":     receiverID, // Gönderende receiver görünür
		"last_message":      messageData["text"],
		"last_message_time": messageData["created_at"],
		"is_from_me":        true,
	}

	// Gönderene
	h.SendToUser(senderID, "conversation_update", conversationData)

	// Alıcıya (onun için other_user_id sender olacak)
	conversationDataForReceiver := map[string]interface{}{
		"type":              "conversation_update",
		"message_data":      messageData,
		"other_user_id":     senderID, // Alıcıda sender görünür
		"last_message":      messageData["text"],
		"last_message_time": messageData["created_at"],
		"is_from_me":        false,
	}

	h.SendToUser(receiverID, "conversation_update", conversationDataForReceiver)
}

// HandleMessageRead mesaj okundu durumunu handle et
func (h *Hub) HandleMessageRead(messageID string, senderID, readerID uint) {
	readData := map[string]interface{}{
		"message_id": messageID,
		"reader_id":  readerID,
		"read_at":    time.Now(),
	}

	// Sadece gönderene bildir (alıcı zaten okudu)
	h.SendToUser(senderID, "message_read", readData)

	go h.SendUnreadCountUpdate(readerID)

	log.Printf("Mesaj okundu WebSocket üzerinden yayınlandı: %s", messageID)
}

// IsUserOnline kullanıcının online olup olmadığını kontrol et
func (h *Hub) IsUserOnline(userID uint) bool {
	h.mutex.RLock()
	defer h.mutex.RUnlock()
	_, exists := h.clients[userID]
	return exists
}

// GetConnectedUsersCount bağlı kullanıcı sayısı
func (h *Hub) GetConnectedUsersCount() int {
	h.mutex.RLock()
	defer h.mutex.RUnlock()
	return len(h.clients)
}

// GetConnectedUsers bağlı kullanıcı listesi
func (h *Hub) GetConnectedUsers() []uint {
	h.mutex.RLock()
	defer h.mutex.RUnlock()

	users := make([]uint, 0, len(h.clients))
	for userID := range h.clients {
		users = append(users, userID)
	}
	return users
}

func (h *Hub) messageToBytes(message *Message) []byte {
	outgoing := &OutgoingMessage{
		Type: message.Type,
		Data: message.Data,
	}

	data, err := json.Marshal(outgoing)
	if err != nil {
		log.Printf("JSON marshal hatası: %v", err)
		return []byte(`{"type":"error","data":"Message format error"}`)
	}
	return data
}

// sendRecentMessages kullanıcıya son 30 mesajı gönder
func (h *Hub) sendRecentMessages(client *Client) {
	var messages []struct {
		ID               string  `json:"id"`
		SenderID         uint    `json:"sender_id"`
		ReceiverID       uint    `json:"receiver_id"`
		ReplyToMessageID *string `json:"reply_to_message_id"`
		Text             string  `json:"text"`
		Read             bool    `json:"read"`
		SenderReaction   *string `json:"sender_reaction"`
		ReceiverReaction *string `json:"receiver_reaction"`
		CreatedAt        string  `json:"created_at"`
		// YENİ: Reply mesajı bilgileri
		ReplyToMessageText   *string `json:"reply_to_message_text"`
		ReplyToMessageSender *uint   `json:"reply_to_message_sender"`
		ReplyToMessageType   *string `json:"reply_to_message_type"`
		ReplyToCreatedAt     *string `json:"reply_to_created_at"`
	}

	query := `
        SELECT 
            m.id, 
            m.sender_id, 
            m.receiver_id,
            m.reply_to_message_id,
            m.encrypted_text as text,
            m.read,
            m.sender_reaction,
            m.receiver_reaction,
            m.created_at,
            reply.encrypted_text as reply_to_message_text,
            reply.sender_id as reply_to_message_sender,
            reply.type as reply_to_message_type,
            reply.created_at as reply_to_created_at
        FROM messages m
        LEFT JOIN messages reply ON m.reply_to_message_id = reply.id
        WHERE (m.sender_id = ? OR m.receiver_id = ?)
        AND (
            CASE 
                WHEN m.sender_id = ? THEN m.is_deleted_by_sender = false
                ELSE m.is_deleted_by_receiver = false
            END
        )
        ORDER BY m.created_at ASC 
        LIMIT 30
    `

	if err := h.db.Raw(query, client.UserID, client.UserID, client.UserID).Scan(&messages).Error; err != nil {
		log.Printf("Son mesajlar alınamadı: %v", err)
		return
	}

	for i := 0; i < len(messages); i++ {
		msg := messages[i]

		decryptedText, err := h.encryptionService.DecryptMessage(msg.Text)
		if err != nil {
			decryptedText = "Mesaj çözülemedi"
		}

		messageData := map[string]interface{}{
			"id":                  msg.ID,
			"sender_id":           msg.SenderID,
			"receiver_id":         msg.ReceiverID,
			"reply_to_message_id": msg.ReplyToMessageID,
			"text":                decryptedText,
			"read":                msg.Read,
			"sender_reaction":     msg.SenderReaction,
			"receiver_reaction":   msg.ReceiverReaction,
			"created_at":          msg.CreatedAt,
			"is_history":          true,
		}

		// Reply mesajı varsa ekle
		if msg.ReplyToMessageID != nil && msg.ReplyToMessageText != nil {
			replyDecryptedText, err := h.encryptionService.DecryptMessage(*msg.ReplyToMessageText)
			if err != nil {
				replyDecryptedText = "Mesaj çözülemedi"
			}

			messageData["reply_to_message"] = map[string]interface{}{
				"id":         *msg.ReplyToMessageID,
				"sender_id":  msg.ReplyToMessageSender,
				"text":       replyDecryptedText,
				"type":       msg.ReplyToMessageType,
				"created_at": msg.ReplyToCreatedAt,
			}
		}

		outgoingMessage := &OutgoingMessage{
			Type: "history_message",
			Data: messageData,
		}

		select {
		case client.Send <- h.messageToBytes(&Message{Type: outgoingMessage.Type, Data: outgoingMessage.Data}):
		default:
			log.Printf("Kullanıcı %d için mesaj geçmişi gönderilemedi", client.UserID)
			return
		}
	}

	completedMessage := &OutgoingMessage{
		Type: "history_loaded",
		Data: map[string]interface{}{
			"message": "Son 30 mesaj yüklendi",
			"count":   len(messages),
		},
	}

	select {
	case client.Send <- h.messageToBytes(&Message{Type: completedMessage.Type, Data: completedMessage.Data}):
	default:
		log.Printf("Kullanıcı %d için tamamlanma bildirimi gönderilemedi", client.UserID)
	}
}

// HandleWebSocket WebSocket bağlantısını handle et
func (h *Hub) HandleWebSocket(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		log.Printf("WebSocket: user_id context'te bulunamadı")
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	log.Printf("WebSocket: Context'ten alınan userID: %v (tip: %T)", userID, userID)

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("WebSocket upgrade hatası: %v", err)
		return
	}

	client := &Client{
		UserID: userID.(uint),
		Conn:   conn,
		Send:   make(chan []byte, 256),
		Hub:    h,
	}

	h.register <- client

	go client.writePump()
	go client.readPump()
}

// readPump client'tan mesaj oku ve işle
func (c *Client) readPump() {
	defer func() {
		c.Hub.unregister <- c
	}()

	// Ping/Pong setup
	c.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.Conn.SetPongHandler(func(string) error {
		c.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		_, messageBytes, err := c.Conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("WebSocket hatası: %v", err)
			}
			break
		}

		var incomingMsg IncomingMessage
		if err := json.Unmarshal(messageBytes, &incomingMsg); err != nil {
			log.Printf("Mesaj parse hatası: %v", err)
			continue
		}

		// Gelen mesajları işle
		c.handleIncomingMessage(&incomingMsg)
	}
}

// handleIncomingMessage gelen mesajları işle
func (c *Client) handleIncomingMessage(msg *IncomingMessage) {
	switch msg.Type {
	case "ping":
		// Ping mesajına pong ile cevap ver
		response := &OutgoingMessage{
			Type: "pong",
			Data: map[string]interface{}{
				"timestamp": time.Now().Unix(),
			},
		}
		c.sendMessage(response)

	case "typing":
		// Yazıyor durumunu karşı tarafa bildir
		if msg.ReceiverID > 0 {
			c.Hub.SendToUser(msg.ReceiverID, "user_typing", map[string]interface{}{
				"user_id": c.UserID,
				"typing":  true,
			})
		}

	case "typing_stop":
		// Yazmayı bıraktı durumunu karşı tarafa bildir
		if msg.ReceiverID > 0 {
			c.Hub.SendToUser(msg.ReceiverID, "user_typing", map[string]interface{}{
				"user_id": c.UserID,
				"typing":  false,
			})
		}

	case "get_online_users":
		// Online kullanıcı listesini gönder
		onlineUsers := c.Hub.GetConnectedUsers()
		response := &OutgoingMessage{
			Type: "online_users",
			Data: map[string]interface{}{
				"users": onlineUsers,
				"count": len(onlineUsers),
			},
		}
		c.sendMessage(response)
	case "add_reaction":
		dataMap, ok := msg.Data.(map[string]interface{})
		if !ok {
			log.Printf("Reaction data parse edilemedi")
			return
		}

		messageID, ok1 := dataMap["message_id"].(string)
		emoji, ok2 := dataMap["emoji"].(string)
		if !ok1 || !ok2 {
			log.Printf("MessageID veya emoji eksik")
			return
		}

		c.Hub.handleAddReaction(c.UserID, messageID, emoji)

	case "remove_reaction":
		dataMap, ok := msg.Data.(map[string]interface{})
		if !ok {
			return
		}

		messageID, ok1 := dataMap["message_id"].(string)
		if !ok1 {
			return
		}

		c.Hub.handleRemoveReaction(c.UserID, messageID)
	case "send_message":
		// Mevcut kodu güncelle - reply desteği ekle
		dataMap, ok := msg.Data.(map[string]interface{})
		if !ok {
			log.Printf("Mesaj data parse edilemedi")
			return
		}

		receiverIDFloat, ok1 := dataMap["receiver_id"].(float64)
		content, ok2 := dataMap["text"].(string)
		if !ok1 || !ok2 {
			log.Printf("Geçersiz mesaj verisi")
			return
		}

		receiverID := uint(receiverIDFloat)
		var replyToMessageID *string
		var msgType string

		// Reply kontrolü
		if replyID, exists := dataMap["reply_to_message_id"].(string); exists && replyID != "" {
			replyToMessageID = &replyID
		}

		if typeStr, exists := dataMap["type"].(string); exists {
			msgType = typeStr
		} else {
			msgType = "text" // Default type
		}

		if receiverID == 0 || content == "" {
			return
		}

		messageID := uuid.New().String()
		createdAt := time.Now()

		c.Hub.HandleNewMessage(c.UserID, receiverID, messageID, content, msgType, createdAt, replyToMessageID)

		// DB yazma
		go func() {
			encryptedText, err := c.Hub.encryptionService.EncryptMessage(content)
			if err != nil {
				log.Printf("Mesaj şifreleme hatası: %v", err)
				return
			}

			message := models.Message{
				ID:               messageID,
				SenderID:         c.UserID,
				ReceiverID:       receiverID,
				ReplyToMessageID: replyToMessageID,
				EncryptedText:    encryptedText,
				Read:             false,
				CreatedAt:        createdAt,
				UpdatedAt:        createdAt,
			}

			if err := c.Hub.db.Create(&message).Error; err != nil {
				log.Printf("Mesaj DB'ye yazılamadı: %v", err)
			}
		}()

	case "mark_read":
		// Mesajları okundu olarak işaretle
		dataMap, ok := msg.Data.(map[string]interface{})
		if !ok {
			log.Printf("mark_read data parse edilemedi")
			return
		}

		otherUserIDFloat, ok := dataMap["other_user_id"].(float64)
		if !ok {
			log.Printf("other_user_id eksik veya geçersiz")
			return
		}

		otherUserID := uint(otherUserIDFloat)
		c.Hub.handleMarkRead(c.UserID, otherUserID)

	case "call_offer":
		if msg.ReceiverID > 0 {
			c.Hub.SendToUser(msg.ReceiverID, "call_offer", map[string]interface{}{
				"from":        c.UserID,
				"receiver_id": msg.ReceiverID, // ✅ Bu satırı ekle
				"data":        msg.Data,
			})
			log.Printf("📞 Call offer gönderildi: %d -> %d", c.UserID, msg.ReceiverID)
		}

	case "call_answer":
		if msg.ReceiverID > 0 {
			c.Hub.SendToUser(msg.ReceiverID, "call_answer", map[string]interface{}{
				"from":        c.UserID,
				"receiver_id": msg.ReceiverID, // ✅ Bu satırı ekle
				"data":        msg.Data,
			})
			log.Printf("📞 Call answer gönderildi: %d -> %d", c.UserID, msg.ReceiverID)
		}

	case "ice_candidate":
		if msg.ReceiverID > 0 {
			c.Hub.SendToUser(msg.ReceiverID, "ice_candidate", map[string]interface{}{
				"from":        c.UserID,
				"receiver_id": msg.ReceiverID, // ✅ Bu satırı ekle
				"data":        msg.Data,
			})
			log.Printf("🧊 ICE candidate gönderildi: %d -> %d", c.UserID, msg.ReceiverID)
		}

	case "call_end":
		if msg.ReceiverID > 0 {
			c.Hub.SendToUser(msg.ReceiverID, "call_end", map[string]interface{}{
				"from":        c.UserID,
				"receiver_id": msg.ReceiverID, // ✅ Bu satırı ekle
				"data":        "Call ended",
			})
			log.Printf("📞 Call end gönderildi: %d -> %d", c.UserID, msg.ReceiverID)
		}

	case "call_reject":
		if msg.ReceiverID > 0 {
			c.Hub.SendToUser(msg.ReceiverID, "call_reject", map[string]interface{}{
				"from":        c.UserID,
				"receiver_id": msg.ReceiverID, // ✅ Bu satırı ekle
				"data":        "Call rejected",
			})
			log.Printf("📞 Call reject gönderildi: %d -> %d", c.UserID, msg.ReceiverID)
		}

	case "call_busy":
		if msg.ReceiverID > 0 {
			c.Hub.SendToUser(msg.ReceiverID, "call_busy", map[string]interface{}{
				"from":        c.UserID,
				"receiver_id": msg.ReceiverID, // ✅ Bu satırı ekle
				"data":        "User is busy",
			})
			log.Printf("📞 Call busy gönderildi: %d -> %d", c.UserID, msg.ReceiverID)
		}

	case "get_unread_count":
		// ✅ YENİ: Client'ın talep ettiği durumda okunmamış sayıyı gönder
		count := c.Hub.GetUnreadCount(c.UserID)
		response := &OutgoingMessage{
			Type: "unread_count",
			Data: map[string]interface{}{
				"count": count,
			},
		}
		c.sendMessage(response)

	default:
		log.Printf("Bilinmeyen mesaj tipi: %s", msg.Type)
	}
}

// sendMessage client'a mesaj gönder
func (c *Client) sendMessage(msg *OutgoingMessage) {
	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("JSON marshal hatası: %v", err)
		return
	}

	select {
	case c.Send <- data:
	default:
		log.Printf("Client %d için mesaj gönderilemedi", c.UserID)
	}
}

// writePump client'a mesaj yaz
func (c *Client) writePump() {
	ticker := time.NewTicker(54 * time.Second)
	defer func() {
		ticker.Stop()
		c.Conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.Send:
			c.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			if err := c.Conn.WriteMessage(websocket.TextMessage, message); err != nil {
				log.Printf("Mesaj yazma hatası: %v", err)
				return
			}

		case <-ticker.C:
			c.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// sendPushNotification push notification göndər (async)
func (h *Hub) sendPushNotification(senderID, receiverID uint, message, msgType string) {
	//log.Printf("🔔 sendPushNotification çağrıldı: %d -> %d, message: %s", senderID, receiverID, message)

	go func() {
		//log.Printf("🔔 Goroutine başladı")

		url := h.config.BackendUrl + "/notification/new-message"
		//log.Printf("🔔 URL: %s", url)

		var notificationMessage string
		switch msgType {
		case "image":
			notificationMessage = "Image"
		case "video":
			notificationMessage = "Video"
		case "voice":
			notificationMessage = "Voice"
		default:
			notificationMessage = message // Normal text mesaj
		}

		// Config yoxlayın
		if h.config.CloudToken == "" {
			log.Printf("❌ CloudToken boş!")
			return
		}
		if h.config.BackendUrl == "" {
			log.Printf("❌ BackendUrl boş!")
			return
		}

		payload := map[string]interface{}{
			"receiver_id": receiverID,
			"sender_id":   senderID,
			"message":     notificationMessage, // Type'a göre ayarlanmış mesaj
		}

		//log.Printf("🔔 Payload: %+v", payload)

		jsonData, err := json.Marshal(payload)
		if err != nil {
			log.Printf("❌ Notification payload marshal hatası: %v", err)
			return
		}

		//log.Printf("🔔 JSON Data: %s", string(jsonData))

		req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
		if err != nil {
			log.Printf("❌ Notification request oluşturma hatası: %v", err)
			return
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", h.config.CloudToken)

		//log.Printf("🔔 Headers set edildi, CloudToken: %s", h.config.CloudToken[:10]+"...")

		//log.Printf("🔔 HTTP request göndəriliyor...")
		resp, err := h.httpClient.Do(req)
		if err != nil {
			log.Printf("❌ Push notification gönderme hatası: %v", err)
			return
		}
		defer func(Body io.ReadCloser) {
			err := Body.Close()
			if err != nil {

			}
		}(resp.Body)

		// Response body-ni oxuyun
		//bodyBytes, _ := io.ReadAll(resp.Body)
		//log.Printf("🔔 Response Status: %d, Body: %s", resp.StatusCode, string(bodyBytes))

		if resp.StatusCode == 200 {
			//log.Printf("✅ Push notification gönderildi: %d -> %d", senderID, receiverID)
		} else {
			log.Printf("❌ Push notification başarısız, status: %d", resp.StatusCode)
		}
	}()
}

// GetUnreadCount kullanıcının okunmamış mesaj sayısını getir
func (h *Hub) GetUnreadCount(userID uint) int {
	var count int64

	query := `
		SELECT COUNT(*) 
		FROM messages 
		WHERE receiver_id = ? 
		AND read = false 
		AND is_deleted_by_receiver = false
	`

	if err := h.db.Raw(query, userID).Scan(&count).Error; err != nil {
		log.Printf("Okunmamış mesaj sayısı alınamadı: %v", err)
		return 0
	}

	return int(count)
}

// SendUnreadCountUpdate kullanıcıya okunmamış mesaj sayısını gönder
func (h *Hub) SendUnreadCountUpdate(userID uint) {
	count := h.GetUnreadCount(userID)

	h.SendToUser(userID, "unread_count_update", map[string]interface{}{
		"count": count,
	})

	log.Printf("Okunmamış mesaj sayısı gönderildi: User %d, Count: %d", userID, count)
}

// handleAddReaction mesaja reaction ekle
func (h *Hub) handleAddReaction(userID uint, messageID, emoji string) {
	var message models.Message
	if err := h.db.Where("id = ?", messageID).First(&message).Error; err != nil {
		log.Printf("Mesaj bulunamadı: %v", err)
		return
	}

	// Kullanıcının bu mesaja reaction verebilir mi kontrol et
	if userID != message.SenderID && userID != message.ReceiverID {
		log.Printf("Kullanıcı %d bu mesaja reaction veremez", userID)
		return
	}

	// Reaction güncelle
	if userID == message.SenderID {
		message.SenderReaction = &emoji
	} else {
		message.ReceiverReaction = &emoji
	}

	message.UpdatedAt = time.Now()

	if err := h.db.Save(&message).Error; err != nil {
		log.Printf("Reaction kaydedilemedi: %v", err)
		return
	}

	// WebSocket ile bildir
	reactionData := map[string]interface{}{
		"message_id": messageID,
		"user_id":    userID,
		"emoji":      emoji,
		"action":     "added",
	}

	h.SendToUser(message.SenderID, "reaction_updated", reactionData)
	h.SendToUser(message.ReceiverID, "reaction_updated", reactionData)

	log.Printf("Reaction eklendi: User %d, Message %s, Emoji %s", userID, messageID, emoji)
}

// handleRemoveReaction mesajdan reaction kaldır
func (h *Hub) handleRemoveReaction(userID uint, messageID string) {
	var message models.Message
	if err := h.db.Where("id = ?", messageID).First(&message).Error; err != nil {
		log.Printf("Mesaj bulunamadı: %v", err)
		return
	}

	if userID != message.SenderID && userID != message.ReceiverID {
		return
	}

	// Reaction kaldır
	if userID == message.SenderID {
		message.SenderReaction = nil
	} else {
		message.ReceiverReaction = nil
	}

	message.UpdatedAt = time.Now()

	if err := h.db.Save(&message).Error; err != nil {
		log.Printf("Reaction kaldırılamadı: %v", err)
		return
	}

	// WebSocket ile bildir
	reactionData := map[string]interface{}{
		"message_id": messageID,
		"user_id":    userID,
		"action":     "removed",
	}

	h.SendToUser(message.SenderID, "reaction_updated", reactionData)
	h.SendToUser(message.ReceiverID, "reaction_updated", reactionData)

	log.Printf("Reaction kaldırıldı: User %d, Message %s", userID, messageID)
}

// handleMarkRead kullanıcının mesajlarını okundu olarak işaretle
func (h *Hub) handleMarkRead(readerID, otherUserID uint) {
	// Bu conversation'daki okunmamış mesajları okundu olarak işaretle
	result := h.db.Model(&models.Message{}).
		Where("sender_id = ? AND receiver_id = ? AND read = false", otherUserID, readerID).
		Update("read", true)

	if result.Error != nil {
		log.Printf("Mesajları okundu olarak işaretleme hatası: %v", result.Error)
		return
	}

	// Kaç mesaj okundu olarak işaretlendi
	updatedCount := result.RowsAffected

	if updatedCount > 0 {
		// Mesaj gönderende unread count güncelle
		go h.SendUnreadCountUpdate(otherUserID)

		// Mesaj gönderen kişiye bildir (message_read event)
		readData := map[string]interface{}{
			"reader_id":     readerID,
			"other_user_id": otherUserID,
			"read_count":    updatedCount,
		}

		h.SendToUser(otherUserID, "message_read", readData)

		log.Printf("Mesajlar okundu olarak işaretlendi: %d mesaj, reader: %d, sender: %d",
			updatedCount, readerID, otherUserID)
	}
}
