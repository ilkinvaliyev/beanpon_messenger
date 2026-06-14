package websocket

import (
	"encoding/json"
	"net/http"
	"time"

	"beanpon_messenger/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// 1:1 sesli arama sinyalleşməsi.
//
// Laravel (api.beanpon.com) çağrı qurur, LiveKit token üretir, sonra bu
// internal endpoint-i çağırır → online callee/caller-ə WS ilə call_* event
// çatdırılır. Ses isə LiveKit-dən (livekit.beanpon.com) axır — bura yalnız
// siqnal daşıyır.
//
// Route (main.go /internal qrupunda):
//
//	internal.POST("/calls/signal", wsHub.HandleCallSignal)
type callSignalRequest struct {
	Event    string                 `json:"event" binding:"required"`
	ToUserID uint                   `json:"to_user_id" binding:"required"`
	Data     map[string]interface{} `json:"data"`
}

// HandleCallSignal — Laravel-dən gələn çağrı siqnalını hədəf istifadəçiyə ötür.
// Cavabda `delivered`: 1 (online, WS ilə çatdı) / 0 (offline → Laravel push atır).
func (h *Hub) HandleCallSignal(c *gin.Context) {
	var req callSignalRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	// Yalnız call_* event-lərinə icazə (təhlükəsizlik).
	switch req.Event {
	case "call_incoming", "call_accepted", "call_rejected",
		"call_canceled", "call_ended", "call_busy":
		// ok
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown event"})
		return
	}

	online := h.IsUserOnline(req.ToUserID)
	if online {
		h.SendToUser(req.ToUserID, req.Event, req.Data)
	}

	delivered := 0
	if online {
		delivered = 1
	}
	c.JSON(http.StatusOK, gin.H{"delivered": delivered})
}

// ── Çağrı conversation mesajı ─────────────────────────────────────────────
//
// Çağrı bitdikdə (Laravel `end`) conversation-da KALICI bir "call" mesajı
// yaranır (WhatsApp/Instagram-vari "Sesli arama / Buraxılmış zəng"). Mesaj
// mətni JSON-dur: {"type":"call","status":"...","duration":N,"call_id":"..."}
// Flutter ChatMessage bunu parse edib xüsusi balon göstərir.
//
// Route: internal.POST("/calls/message", wsHub.HandleCallMessage)
type callMessageRequest struct {
	CallerID uint   `json:"caller_id" binding:"required"`
	CalleeID uint   `json:"callee_id" binding:"required"`
	Status   string `json:"status" binding:"required"` // ended | missed | rejected | canceled
	Duration int    `json:"duration"`
	CallID   string `json:"call_id"`
}

func (h *Hub) HandleCallMessage(c *gin.Context) {
	var req callMessageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	// Mesaj mətni: JSON (Flutter type=call kimi parse edir).
	payload := map[string]interface{}{
		"type":     "call",
		"status":   req.Status,
		"duration": req.Duration,
		"call_id":  req.CallID,
	}
	textBytes, _ := json.Marshal(payload)
	text := string(textBytes)

	encryptedText, err := h.encryptionService.EncryptMessage(text)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt failed"})
		return
	}

	now := time.Now().UTC()
	receiverID := req.CalleeID
	// Yalnız BURAXILMIŞ (missed) zəng oxunmamış qalır (unread badge). Bitən
	// (ended) və rədd edilən (rejected) çağrılar oxunmuş sayılır — istifadəçi
	// onsuz da çağrı ekranında idi.
	isRead := req.Status != "missed"
	message := models.Message{
		ID:            uuid.New().String(),
		SenderID:      req.CallerID,
		ReceiverID:    &receiverID,
		EncryptedText: encryptedText,
		Read:          isRead,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := h.db.Create(&message).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db failed"})
		return
	}

	// Hər iki tərəfə WS ilə çatdır + conversation update.
	// silent=true → çağrı mesajı üçün ayrıca push GETMƏSİN (çağrı bildirişi
	// onsuz da getdi). msgType="call".
	h.HandleNewMessage(
		req.CallerID,
		receiverID,
		message.ID,
		text,
		"call",
		now,
		nil,
		nil,
		"active",
		true,
	)

	c.JSON(http.StatusOK, gin.H{"message_id": message.ID})
}
