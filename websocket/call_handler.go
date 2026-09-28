package websocket

import (
	"encoding/json"
	"log"
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
		log.Printf("[call-msg] bind failed: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	log.Printf("[call-msg] received caller=%d callee=%d status=%s dur=%d call_id=%s",
		req.CallerID, req.CalleeID, req.Status, req.Duration, req.CallID)

	// Mesaj mətni: JSON (Flutter/iOS/Android type=call kimi parse edir).
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
		log.Printf("[call-msg] encrypt failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt failed"})
		return
	}

	now := time.Now().UTC()
	receiverID := req.CalleeID

	// ── UPSERT: eyni call_id üçün mesaj varsa YENİLƏ, yoxsa YARAT ─────────────
	// Zəng BAŞLAYANDA (status=ringing) mesaj yaranır → "Səsli zəng" dərhal düşür.
	// Zəng BİTƏNDƏ (ended/missed/rejected/canceled) HƏMİN mesaj yenilənir
	// (yeni sətir yaranmır) — müddət/status dəyişir. Eyniləşdirmə call_id → id
	// xəritəsi ilə (bir zəng sessiyası boyu). Xəritədə yoxdursa DB-də tekst
	// içindəki call_id ilə axtar (messenger restart olubsa).
	existingID := h.lookupCallMessageID(req.CallID)

	if existingID != "" {
		// ── YENİLƏ (zəng bitdi) ──────────────────────────────────────────────
		// missed → oxunmamış qalsın (unread badge callee üçün). Digərləri oxunmuş.
		updates := map[string]interface{}{
			"encrypted_text": encryptedText,
			"updated_at":     now,
		}
		if req.Status == "missed" {
			updates["read"] = false
		}
		if err := h.db.Model(&models.Message{}).Where("id = ?", existingID).
			Updates(updates).Error; err != nil {
			log.Printf("[call-msg] db update failed id=%s: %v", existingID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "db failed"})
			return
		}
		log.Printf("[call-msg] updated existing id=%s status=%s — broadcasting message_edited to [%d,%d]",
			existingID, req.Status, req.CallerID, receiverID)

		// Hər iki tərəfə balonu YERİNDƏ yenilə (iOS/Android `message_edited`
		// dinləyir → mövcud balonu dəyişir, yeni sətir yaratmır). PUSH YOX.
		h.broadcastCallMessageEdited(req.CallerID, receiverID, existingID, text)

		// Zəng bitdi → xəritədən sil (yaddaş şişməsin).
		h.forgetCallMessageID(req.CallID)

		c.JSON(http.StatusOK, gin.H{"message_id": existingID, "updated": true})
		return
	}

	// ── YARAT (zəng başladı) ────────────────────────────────────────────────
	// Zəng başlayanda düşən mesaj HƏMİŞƏ oxunmuş sayılır (callee onsuz da zəng
	// bildirişi/ekranı görür; unread badge şişməsin). Ayrıca PUSH GETMİR
	// (HandleNewMessage silent=true) — istifadəçinin dediyi kimi "yeni mesaj
	// bildirişi getməsin, zəng onsuz da gedir".
	message := models.Message{
		ID:            uuid.New().String(),
		SenderID:      req.CallerID,
		ReceiverID:    &receiverID,
		EncryptedText: encryptedText,
		Read:          true,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := h.db.Create(&message).Error; err != nil {
		log.Printf("[call-msg] db create failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db failed"})
		return
	}
	h.rememberCallMessageID(req.CallID, message.ID)
	log.Printf("[call-msg] db row created id=%s — broadcasting new_message to [%d,%d]",
		message.ID, req.CallerID, receiverID)

	// Hər iki tərəfə WS ilə çatdır + conversation update. silent=true → ayrıca
	// push GETMƏSİN (zəng bildirişi onsuz da getdi). msgType="call".
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
		nil, nil, nil,
	)

	c.JSON(http.StatusOK, gin.H{"message_id": message.ID})
}

// ── call_id → message_id xəritəsi köməkçiləri ─────────────────────────────────
func (h *Hub) rememberCallMessageID(callID, messageID string) {
	if callID == "" {
		return
	}
	h.callMsgMu.Lock()
	h.callMsgIDs[callID] = messageID
	h.callMsgMu.Unlock()
}

func (h *Hub) forgetCallMessageID(callID string) {
	if callID == "" {
		return
	}
	h.callMsgMu.Lock()
	delete(h.callMsgIDs, callID)
	h.callMsgMu.Unlock()
}

// lookupCallMessageID — əvvəlcə yaddaş xəritəsindən; tapılmasa DB-də şifrəli
// mesajlar arasında call_id-ni axtarır (messenger restart-a qarşı fallback).
// DB axtarışı: son 200 mesajı deşifrə edib call_id uyğunluğunu yoxlayır (nadir
// yol; yalnız xəritə boşdursa). Tapmasa "" qaytarır → yeni mesaj yaranar.
func (h *Hub) lookupCallMessageID(callID string) string {
	if callID == "" {
		return ""
	}
	h.callMsgMu.Lock()
	id := h.callMsgIDs[callID]
	h.callMsgMu.Unlock()
	if id != "" {
		return id
	}
	// Fallback: son çağrı mesajlarını deşifrə edib call_id tap (restart halı).
	var rows []models.Message
	if err := h.db.Where("encrypted_text IS NOT NULL").
		Order("created_at DESC").Limit(200).Find(&rows).Error; err != nil {
		return ""
	}
	for _, m := range rows {
		plain, derr := h.encryptionService.DecryptMessage(m.EncryptedText)
		if derr != nil {
			continue
		}
		if !json.Valid([]byte(plain)) {
			continue
		}
		var obj map[string]interface{}
		if json.Unmarshal([]byte(plain), &obj) != nil {
			continue
		}
		if t, _ := obj["type"].(string); t != "call" {
			continue
		}
		if cid, _ := obj["call_id"].(string); cid == callID {
			return m.ID
		}
	}
	return ""
}

// broadcastCallMessageEdited — çağrı balonunu YERİNDƏ yeniləmək üçün hər iki
// tərəfə `message_edited` WS event göndərir (yeni mesaj/push YOX). iOS/Android
// `handleEdited` mövcud balonun mətnini dəyişir → status/müddət yenilənir.
func (h *Hub) broadcastCallMessageEdited(senderID, receiverID uint, messageID, text string) {
	data := map[string]interface{}{
		"message_id": messageID,
		"text":       text,
		"is_edited":  false, // "düzəldildi" etiketi göstərmə — bu sistem yeniləməsidir
	}
	h.SendToMultipleUsers([]uint{senderID, receiverID}, "message_edited", data)
}
