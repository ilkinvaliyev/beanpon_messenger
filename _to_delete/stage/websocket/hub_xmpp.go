package websocket

// hub_xmpp.go — adapter that lets the Hub satisfy the xmpp.Bridge interfaces
// (xmpp.LegacyDelivery + xmpp.IngressSink) and own XMPP presence in the
// registry. Counterpart: none — this is server-only transport plumbing.
//
// Scope: chat_page (1:1) and group_chat_page (group) only.
//
// The bridge calls back here on the INGRESS path (a NEW XMPP client sent a
// message): IngestDM / IngestGroup re-enter the SAME business pipeline a legacy
// send uses, so permission / spam / moderation / persistence / fan-out all run
// unchanged. The fan-out then uses the EGRESS seam in hub.go, which may deliver
// the message back out to an OLD recipient over legacy WS — closing the loop.

import (
	"log"
	"time"

	"beanpon_messenger/models"
	"beanpon_messenger/services"
	"beanpon_messenger/xmpp"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// AttachXMPP stores the bridge handle on the Hub. Called from main.go after the
// bridge is constructed. nil-safe everywhere (guarded by h.xmpp != nil).
func (h *Hub) AttachXMPP(b *xmpp.Bridge) { h.xmpp = b }

// ── xmpp.LegacyDelivery ─────────────────────────────────────────────────────

// SendToUserLegacy delivers to a single user over the legacy WS channel. This
// is exactly the call the Hub already uses internally.
func (h *Hub) SendToUserLegacy(userID uint, messageType string, data interface{}) {
	h.SendToUser(userID, messageType, data)
}

// ── xmpp.IngressSink ────────────────────────────────────────────────────────

// IngestDM handles a 1:1 message that arrived over XMPP from a NEW client. It
// runs the SAME path as the WS "send_message" case: spam shadow-ban check,
// conversation permission, persistence + moderation enqueue, then fan-out via
// HandleNewMessage (which hits the XMPP egress seam for the recipient).
func (h *Hub) IngestDM(senderID, receiverID uint, text, kind, replyToID, storyID string) {
	if receiverID == 0 || text == "" {
		return
	}
	if kind == "" {
		kind = "text"
	}

	// 🚫 SPAM SHADOW-BAN — same global check as the WS path.
	if models.IsMessagingBannedByActions(h.db, senderID) {
		log.Printf("🚫 SPAM SHADOW-BAN (XMPP): sender=%d → receiver=%d bloklandı", senderID, receiverID)
		return
	}

	// Permission + conversation (reuses the exact WS-path helper).
	conversation, canSend, errorMsg, err := h.getOrCreateConversationWithPermission(senderID, receiverID)
	if err != nil || !canSend {
		if errorMsg == spamSilentReason {
			return // shadow-ban: swallow silently
		}
		// NEW client gets the same error event shape as legacy.
		h.SendToUser(senderID, "message_error", map[string]interface{}{
			"error": errorMsg,
			"code":  "SEND_NOT_ALLOWED",
		})
		return
	}

	messageID := uuid.New().String()
	// Issue 30: REST/WS ilə eyni — UTC. Sütun `timestamp without time zone`
	// olduqda yerli vaxt yazmaq yolları arasında sıralamanı pozurdu.
	createdAt := time.Now().UTC()

	var replyPtr *string
	if replyToID != "" {
		replyPtr = &replyToID
	}

	// ── Issue 64 (+1, +8, +40): ƏVVƏL PERSİST, SONRA YAY ────────────────────
	//
	// Bu yol WS-in KÖHNƏ (səhv) sırasını təkrarlayırdı: `HandleNewMessage` ilə
	// yayım, ARDINDAN fire-and-forget goroutine-də `db.Create`. Şifrələmə və
	// ya yazma uğursuz olsa, mesaj hər iki ekranda görünüb DB-də heç vaxt
	// yaranmırdı — yenidən açanda YOX olurdu (Issue 1-in eyni sinifi).
	// Üstəlik `updateConversationOnMessage` heç çağırılmırdı: sayğaclar,
	// pending→active keçidi və `last_message_at` yenilənmirdi (Issue 8).
	//
	// İndi WS yolu ilə birebir: şifrələ → (insert + conversation yeniləməsi
	// TEK transaction) → yalnız uğurda yay.
	// Issue 56: `text` hələ AÇIQ mətndir — S3 media açarlarını burada
	// "istifadə olunub" işarələ. Bu sətir olmasa XMPP istemçisindən gələn
	// media 24 saat sonra sahibsiz sayılıb S3-dən silinərdi, halbuki mesaj
	// hələ ona işarə edir (XMPP açılan gün "şəkil yüklənmir" kimi görünərdi).
	services.MarkMediaReferenced(text)

	encryptedText, encErr := h.encryptionService.EncryptMessage(text)
	if encErr != nil {
		log.Printf("XMPP IngestDM encrypt failed: %v", encErr)
		h.SendToUser(senderID, "message_error", map[string]interface{}{
			"error": "message_encrypt_failed", "code": "SEND_FAILED",
		})
		return
	}
	msg := models.Message{
		ID:               messageID,
		SenderID:         senderID,
		ReceiverID:       &receiverID,
		ReplyToMessageID: replyPtr,
		EncryptedText:    encryptedText,
		Read:             false,
		CreatedAt:        createdAt,
		UpdatedAt:        createdAt,
	}
	if dbErr := h.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&msg).Error; err != nil {
			return err
		}
		if conversation != nil {
			return applyConversationMessageUpdateDB(tx, conversation, senderID)
		}
		return nil
	}); dbErr != nil {
		log.Printf("XMPP IngestDM DB write failed: %v", dbErr)
		h.SendToUser(senderID, "message_error", map[string]interface{}{
			"error": "message_persist_failed", "code": "SEND_FAILED",
		})
		return
	}

	// Status yeniləndikdən SONRA oxu (pending→active keçmiş ola bilər) —
	// push qapısı bunu istifadə edir.
	conversationStatus := "new"
	if conversation != nil {
		conversationStatus = conversation.Status
	}

	// Komit olundu — indi yay. silent=false.
	h.HandleNewMessage(senderID, receiverID, messageID, text, kind, createdAt, replyPtr, nil, conversationStatus, false)

	if h.moderationEnqueue != nil && kind == "text" {
		h.moderationEnqueue(messageID, senderID, receiverID, text, createdAt)
	}
}

// IngestGroup handles a group message that arrived over XMPP. Phase 1 keeps the
// authoritative group send in the existing REST handler; group ingress over
// XMPP is wired in a later phase. For now we log and drop so nothing is
// silently mis-delivered without the full group permission matrix.
func (h *Hub) IngestGroup(senderID, conversationID uint, text, kind string) {
	log.Printf("[xmpp] group ingress not yet enabled (sender=%d conv=%d) — ignored in phase 1",
		senderID, conversationID)
}

// GroupMemberIDs exposes group membership for legacy fan-out. Mirrors the
// handlers-package getActiveGroupMemberIDs query, using the Hub's own db handle
// (that helper is unexported in the handlers package).
//
// Issue 7: this list feeds MESSAGE CONTENT delivery, so it must exclude
// `invite_status='pending'` rows — an invitee who has not accepted must not
// receive live group messages (they cannot load history either). COALESCE
// keeps legacy rows with a NULL invite_status treated as active.
func (h *Hub) GroupMemberIDs(conversationID uint) []uint {
	var ids []uint
	h.db.Model(&models.ConversationParticipant{}).
		Where("conversation_id = ? AND left_at IS NULL AND deleted_at IS NULL AND COALESCE(invite_status,'active') = 'active'", conversationID).
		Pluck("user_id", &ids)
	return ids
}

// markLegacyPresence records that a user connected over the legacy WS. Called
// from registerClient when the bridge is present so the registry knows this
// user is reachable on an OLD client.
func (h *Hub) markLegacyPresence(userID uint) {
	if h.xmpp != nil && h.xmpp.Enabled() {
		h.xmpp.Registry().MarkLegacy(userID)
	}
}

// derefStr returns the pointed-to string or "" if nil. Small helper used by the
// egress seam in hub.go.
func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
