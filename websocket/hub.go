package websocket

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"gorm.io/gorm"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		// CORS için - production'da daha güvenli yapılmalı
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
	// Kayıtlı client'lar
	clients map[uint]*Client

	// Client kayıt kanalı
	register chan *Client

	// Client çıkış kanalı
	unregister chan *Client

	// Mesaj broadcast kanalı
	broadcast chan *Message

	// Mutex thread safety için
	mutex sync.RWMutex

	// Database bağlantısı
	db *gorm.DB

	// Şifreleme servisi
	encryptionService interface {
		DecryptMessage(encryptedText string) (string, error)
	}
}

// Message WebSocket mesaj yapısı
type Message struct {
	Type       string      `json:"type"`
	ReceiverID uint        `json:"receiver_id"`
	Data       interface{} `json:"data"`
}

// NewHub yeni hub oluştur
func NewHub(db *gorm.DB, encryptionService interface {
	DecryptMessage(encryptedText string) (string, error)
}) *Hub {
	return &Hub{
		clients:           make(map[uint]*Client),
		register:          make(chan *Client),
		unregister:        make(chan *Client),
		broadcast:         make(chan *Message),
		db:                db,
		encryptionService: encryptionService,
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

	// Eğer kullanıcı zaten bağlıysa, eski bağlantıyı temizle
	if existingClient, exists := h.clients[client.UserID]; exists {
		// Eski client'ı map'ten çıkar
		delete(h.clients, existingClient.UserID)
		// Channel'ı güvenli şekilde kapat
		select {
		case <-existingClient.Send:
			// Channel zaten kapalı
		default:
			close(existingClient.Send)
		}
		existingClient.Conn.Close()
		log.Printf("Kullanıcı %d eski bağlantısı temizlendi", client.UserID)
	}

	h.clients[client.UserID] = client
	log.Printf("Kullanıcı %d WebSocket'e bağlandı", client.UserID)

	// Bağlandıktan sonra son 30 mesajı gönder
	go h.sendRecentMessages(client)
}

// unregisterClient client'ı çıkar
func (h *Hub) unregisterClient(client *Client) {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	if _, exists := h.clients[client.UserID]; exists {
		delete(h.clients, client.UserID)

		// Channel'ı güvenli şekilde kapat
		select {
		case <-client.Send:
			// Channel zaten kapalı
		default:
			close(client.Send)
		}

		client.Conn.Close()
		log.Printf("Kullanıcı %d WebSocket'ten ayrıldı", client.UserID)
	}
}

// broadcastMessage mesajı belirli kullanıcıya gönder
func (h *Hub) broadcastMessage(message *Message) {
	h.mutex.RLock()
	defer h.mutex.RUnlock()

	if client, exists := h.clients[message.ReceiverID]; exists {
		select {
		case client.Send <- h.messageToBytes(message):
			// Mesaj başarıyla gönderildi
		default:
			// Channel dolu veya kapalı, client'ı güvenli şekilde çıkar
			go func() {
				h.unregister <- client
			}()
		}
	}
}

// messageToBytes mesajı JSON bytes'a çevir
func (h *Hub) messageToBytes(message *Message) []byte {
	data, err := json.Marshal(message)
	if err != nil {
		log.Printf("JSON marshal hatası: %v", err)
		return []byte(`{"type":"error","data":"Mesaj formatı hatası"}`)
	}
	return data
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

// GetConnectedUsersCount bağlı kullanıcı sayısı
func (h *Hub) GetConnectedUsersCount() int {
	h.mutex.RLock()
	defer h.mutex.RUnlock()
	return len(h.clients)
}

// sendRecentMessages kullanıcıya son 30 mesajı gönder
func (h *Hub) sendRecentMessages(client *Client) {
	// Kullanıcının tüm sohbetlerinden son 30 mesajı al
	var messages []struct {
		ID         string `json:"id"`
		SenderID   uint   `json:"sender_id"`
		ReceiverID uint   `json:"receiver_id"`
		Text       string `json:"text"`
		CreatedAt  string `json:"created_at"`
	}

	// Raw SQL sorgusu - En eski mesajları al ve doğru sırada gönder
	query := `
		SELECT 
			id, 
			sender_id, 
			receiver_id, 
			encrypted_text as text,
			created_at
		FROM messages 
		WHERE sender_id = ? OR receiver_id = ?
		ORDER BY created_at ASC 
		LIMIT 30
	`

	if err := h.db.Raw(query, client.UserID, client.UserID).Scan(&messages).Error; err != nil {
		log.Printf("Son mesajlar alınamadı: %v", err)
		return
	}

	// Mesajları doğru sırada gönder (eski -> yeni)
	for i := 0; i < len(messages); i++ {
		msg := messages[i]

		// Mesajı çöz
		decryptedText, err := h.encryptionService.DecryptMessage(msg.Text)
		if err != nil {
			log.Printf("Mesaj çözme hatası: %v", err)
			decryptedText = "Mesaj çözülemedi"
		}

		messageData := &Message{
			Type:       "history_message",
			ReceiverID: client.UserID,
			Data: map[string]interface{}{
				"id":          msg.ID,
				"sender_id":   msg.SenderID,
				"receiver_id": msg.ReceiverID,
				"text":        decryptedText, // Artık çözülmüş metin
				"created_at":  msg.CreatedAt,
				"is_history":  true,
			},
		}

		// Direkt client'a gönder (broadcast kullanma)
		select {
		case client.Send <- h.messageToBytes(messageData):
		default:
			log.Printf("Kullanıcı %d için mesaj geçmişi gönderilemedi", client.UserID)
			return
		}
	}

	// Son olarak "mesaj geçmişi tamamlandı" bildirimi gönder
	completedMessage := &Message{
		Type:       "history_loaded",
		ReceiverID: client.UserID,
		Data: map[string]interface{}{
			"message": "Son 30 mesaj yüklendi",
			"count":   len(messages),
		},
	}

	select {
	case client.Send <- h.messageToBytes(completedMessage):
	default:
		log.Printf("Kullanıcı %d için tamamlanma bildirimi gönderilemedi", client.UserID)
	}
}

// HandleWebSocket WebSocket bağlantısını handle et
func (h *Hub) HandleWebSocket(c *gin.Context) {
	// JWT'den user ID al (middleware'den gelecek)
	userID, exists := c.Get("user_id")
	if !exists {
		log.Printf("WebSocket: user_id context'te bulunamadı")
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	log.Printf("WebSocket: Context'ten alınan userID: %v (tip: %T)", userID, userID)

	// WebSocket'e upgrade et
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("WebSocket upgrade hatası: %v", err)
		return
	}

	// Client oluştur
	client := &Client{
		UserID: userID.(uint),
		Conn:   conn,
		Send:   make(chan []byte, 256),
		Hub:    h,
	}

	// Client'ı kaydet
	h.register <- client

	// Goroutine'leri başlat
	go client.writePump()
	go client.readPump()
}

// readPump client'tan mesaj oku
func (c *Client) readPump() {
	defer func() {
		c.Hub.unregister <- c
	}()

	for {
		_, _, err := c.Conn.ReadMessage()
		if err != nil {
			break
		}
		// Gelen mesajları işle (şimdilik sadece ping/pong)
	}
}

// writePump client'a mesaj yaz
func (c *Client) writePump() {
	defer c.Conn.Close()

	for {
		select {
		case message, ok := <-c.Send:
			if !ok {
				c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			c.Conn.WriteMessage(websocket.TextMessage, message)
		}
	}
}
