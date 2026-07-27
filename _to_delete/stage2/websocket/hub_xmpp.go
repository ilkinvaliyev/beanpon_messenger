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
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"

	"beanpon_messenger/models"
	"beanpon_messenger/services"
	"beanpon_messenger/utils"
	"beanpon_messenger/xmpp"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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
			if err := applyConversationMessageUpdateDB(tx, conversation, senderID); err != nil {
				return err
			}
		}
		// Issue 56: `text` hələ AÇIQ mətndir — S3 media açarlarını "istifadə
		// olunub" işarələ. Bu sətir olmasa XMPP istemçisindən gələn media 24
		// saat sonra sahibsiz sayılıb S3-dən silinərdi, halbuki mesaj hələ ona
		// işarə edir. İşarələmə mesajla EYNİ transaction-dadır: yazma geri
		// qayıdarsa media da işarəsiz qalır və GC onu təmizləyə bilir.
		services.MarkMediaReferenced(tx, text)
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

// ── Qrup icazələri (websocket paketindəki nüsxə) ────────────────────────────
//
// `handlers.parseGroupPermissions` ixrac olunmayıb və `handlers` paketi ARTIQ
// `websocket`-i import edir (handlers/rave_handler.go) — yəni əks istiqamətdə
// import DÖVR yaradardı. Ona görə məntiq burada BİREBİR təkrarlanır.
// Dəyişiklik olduqda hər iki nüsxə yenilənməlidir.

// defaultGroupPerms — bütün əməliyyatlar AÇIQ.
func defaultGroupPerms() map[string]bool {
	return map[string]bool{
		"allow_text":         true,
		"allow_media":        true,
		"allow_gif":          true,
		"allow_voice":        true,
		"allow_circle_video": true,
		"allow_sound":        true,
	}
}

// parseGroupPerms — jsonb sütununu map-ə açır; NULL/bozuq → default (hamısı
// açıq). Çatışmayan açar = true.
func parseGroupPerms(raw *string) map[string]bool {
	perms := defaultGroupPerms()
	if raw == nil || *raw == "" {
		return perms
	}
	var parsed map[string]bool
	if err := json.Unmarshal([]byte(*raw), &parsed); err != nil {
		return perms
	}
	for k, v := range parsed {
		perms[k] = v
	}
	return perms
}

// groupPermKeyForText — mesaj gövdəsindən tələb olunan icazə açarını təyin
// edir. REST `SendGroupMessage` ilə eyni cədvəl: mesaj tipi şifrələnməmiş
// JSON payload-dan oxunur, adi mətn üçün `allow_text`.
func groupPermKeyForText(text string) string {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return "allow_text"
	}
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &payload); err != nil {
		return "allow_text"
	}
	switch payload["type"] {
	case "image":
		return "allow_media"
	case "video":
		if payload["is_circular_video"] == true {
			return "allow_circle_video"
		}
		return "allow_media"
	case "gif":
		return "allow_gif"
	case "voice":
		return "allow_voice"
	case "sound":
		return "allow_sound"
	}
	return "allow_text"
}

// IngestGroup handles a group message that arrived over XMPP.
//
// ── NİYƏ YENİDƏN YAZILDI ────────────────────────────────────────────────────
// Bu funksiya əvvəl YALNIZ "phase 1-də söndürülüb" log-u yazıb mesajı ATIRDI.
// `xmpp/bridge.go` isə hər `groupchat` stanzası üçün onu çağırırdı və
// `IngressSink` müqaviləsi (bridge.go:35-38) açıq şəkildə "REST
// SendGroupMessage ilə EYNİ icazə/persist/fan-out yolu işlədilməlidir"
// deyirdi. Nəticə: XMPP ilə göndərilən HƏR qrup mesajı SƏSSİZCƏ İTİRDİ —
// göndərən uğur görür, mesaj nə DB-yə düşür, nə də bir kimsəyə çatır.
//
// İndi yol `IngestDM` ilə eyni quruluşdadır və REST qrup yolunun qapılarını
// təkrarlayır:
//  1. üzvlük (yalnız `invite_status='active'`, `left_at IS NULL`) + restricted
//  2. spam shadow-ban (səssiz atım)
//  3. qrup icazə matrisi (owner/admin istisna)
//  4. şifrələmə
//  5. TEK TRANSACTION: insert (`ON CONFLICT (id) DO NOTHING`) + göndərənin
//     avto-oxundu qeydi + `message_count` + `last_message_at` + media işarəsi
//  6. yalnız komitdən SONRA: AKTİV üzvlərə fan-out, avto-oxundu, push
func (h *Hub) IngestGroup(senderID, conversationID uint, text, kind, clientMessageID string) {
	if conversationID == 0 || text == "" {
		return
	}
	if kind == "" {
		kind = "text"
	}

	sendErr := func(code string) {
		h.SendToUser(senderID, "message_error", map[string]interface{}{
			"error": code,
			"code":  "SEND_NOT_ALLOWED",
		})
	}

	// 1) Üzvlük. PENDING dəvət (hələ qəbul etməyib) mesaj YAZA BİLMƏZ —
	//    REST yolundakı eyni şərt.
	var participant models.ConversationParticipant
	if err := h.db.Where(
		"conversation_id = ? AND user_id = ? AND left_at IS NULL AND deleted_at IS NULL AND COALESCE(invite_status, 'active') = 'active'",
		conversationID, senderID,
	).First(&participant).Error; err != nil {
		sendErr("group_membership_required")
		return
	}
	if participant.IsRestricted {
		sendErr("group_send_restricted")
		return
	}

	// 2) 🚫 SPAM SHADOW-BAN — REST/WS ilə eyni. Göndərənə heç nə deyilmir.
	if models.IsMessagingBannedByActions(h.db, senderID) {
		log.Printf("🚫 SPAM SHADOW-BAN (XMPP group): sender=%d → conv=%d bloklandı", senderID, conversationID)
		return
	}

	// 3) 🔒 Qrup icazələri (admin ayarları) — owner/admin istisnadır.
	var conv models.Conversation
	if err := h.db.Where("id = ?", conversationID).First(&conv).Error; err != nil {
		sendErr("group_not_found")
		return
	}
	if participant.Role != "owner" && participant.Role != "admin" {
		permKey := groupPermKeyForText(text)
		if !parseGroupPerms(conv.GroupPermissions)[permKey] {
			h.SendToUser(senderID, "message_error", map[string]interface{}{
				"error": "Bu işlem grup yöneticisi tarafından kapatıldı",
				"code":  "permission_denied",
				"perm":  permKey,
			})
			return
		}
	}

	// İdempotentlik açarı: stanza id-si UUID-dirsə ONU işlət (təkrar göndərilən
	// stanza ikinci sətir yaratmasın), yoxsa server UUID-i. İstemçidən gələn
	// dəyər ETİBARSIZ girişdir — format yoxlaması məcburidir.
	messageID := ""
	if trimmed := strings.TrimSpace(clientMessageID); trimmed != "" {
		if parsed, err := uuid.Parse(trimmed); err == nil {
			messageID = parsed.String()
		}
	}
	if messageID == "" {
		messageID = uuid.New().String()
	}

	// DİQQƏT — burada QƏSDƏN `time.Now()` var, `time.Now().UTC()` YOX.
	// DM yolları Issue 30 ilə UTC-yə keçirildi, amma REST `SendGroupMessage`
	// hələ də yerli saatla yazır. Bu yol EYNİ `messages` sətirlərini eyni
	// qrupa yazır: iki fərqli saat qurşağı qarışsa `ORDER BY created_at`
	// qrup tarixçəsini SƏHV sıralayardı. Ona görə REST qrup yolu ilə eyni
	// saat mənbəyi saxlanılır; hər ikisi birlikdə UTC-yə keçirilməlidir.
	now := time.Now()

	encryptedText, encErr := h.encryptionService.EncryptMessage(text)
	if encErr != nil {
		log.Printf("XMPP IngestGroup encrypt failed (conv=%d): %v", conversationID, encErr)
		h.SendToUser(senderID, "message_error", map[string]interface{}{
			"error": "message_encrypt_failed", "code": "SEND_FAILED",
		})
		return
	}

	message := models.Message{
		ID:             messageID,
		SenderID:       senderID,
		ConversationID: &conversationID,
		EncryptedText:  encryptedText,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	// ── ƏVVƏL PERSİST, SONRA YAY (IngestDM ilə eyni) ────────────────────────
	duplicate := false
	if dbErr := h.db.Transaction(func(tx *gorm.DB) error {
		res := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "id"}},
			DoNothing: true,
		}).Create(&message)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			// Yumşaq silinmiş sətir də tapılmalıdır.
			var existing models.Message
			if err := tx.Unscoped().Where("id = ?", message.ID).First(&existing).Error; err != nil {
				return err
			}
			// Yalnız `sender_id` yoxlamaq KİFAYƏT DEYİL — eyni açar BAŞQA
			// qrupda (və ya DM-də) işlədilibsə mesaj səssizcə yaradılmazdı.
			sameConv := existing.ConversationID != nil && *existing.ConversationID == conversationID
			if existing.SenderID != senderID || !sameConv {
				return errClientMessageIDTaken
			}
			duplicate = true
			return nil
		}

		// Göndərənin öz avto-oxundu qeydi (REST yolu ilə eyni, idempotent).
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).
			Create(&models.MessageRead{
				MessageID:      messageID,
				UserID:         senderID,
				ConversationID: conversationID,
				ReadAt:         now,
				CreatedAt:      now,
			}).Error; err != nil {
			return err
		}

		// Sayğac + söhbət indeksi. REST-də bunlar transaction-sız idi; burada
		// mesajla EYNİ transaction-dadır (Issue 40 sinfi).
		if err := tx.Model(&models.ConversationParticipant{}).
			Where("id = ?", participant.ID).
			Update("message_count", gorm.Expr("message_count + 1")).Error; err != nil {
			return err
		}
		if err := tx.Table("conversations").
			Where("id = ?", conversationID).
			Update("last_message_at", now).Error; err != nil {
			return err
		}

		// Issue 56: `text` hələ AÇIQ mətndir — media istinadını EYNİ
		// transaction-da işarələ.
		services.MarkMediaReferenced(tx, text)
		return nil
	}); dbErr != nil {
		if errors.Is(dbErr, errClientMessageIDTaken) {
			h.SendToUser(senderID, "message_error", map[string]interface{}{
				"error": "client_message_id artıq istifadə olunub",
				"code":  "CLIENT_MESSAGE_ID_TAKEN",
			})
			return
		}
		log.Printf("XMPP IngestGroup DB write failed (conv=%d): %v", conversationID, dbErr)
		h.SendToUser(senderID, "message_error", map[string]interface{}{
			"error": "message_persist_failed", "code": "SEND_FAILED",
		})
		return
	}

	if duplicate {
		// Təkrar stanza — sayğac/yayım/push TƏKRARLANMIR.
		h.SendToUser(senderID, "message_duplicate", map[string]interface{}{
			"id":              messageID,
			"conversation_id": conversationID,
		})
		return
	}

	// ── Komit olundu — indi yay ─────────────────────────────────────────────

	// Göndərən haqqında məlumat (REST `new_group_message` payload-u ilə eyni
	// sahələr — istemçilər hər iki yoldan eyni formatı gözləyir).
	var senderInfo struct {
		Name                string  `gorm:"column:name"`
		Username            string  `gorm:"column:username"`
		IsVerified          bool    `gorm:"column:is_verified"`
		ProfileImage        *string `gorm:"column:profile_image"`
		SpecialBadgeIconURL *string `gorm:"column:special_badge_icon_url"`
	}
	h.db.Raw(`
		SELECT u.name, u.username, u.is_verified, p.profile_image,
			sender_badge.icon_url AS special_badge_icon_url
		FROM users u
		LEFT JOIN LATERAL (
			SELECT b.icon_url
			FROM badges b
			WHERE b.is_special
			  AND b.id = u.selected_badge_id
			ORDER BY b.priority DESC
			LIMIT 1
		) sender_badge ON u.is_verified = true
		LEFT JOIN profiles p ON p.user_id = u.id
		WHERE u.id = ?
	`, senderID).Scan(&senderInfo)

	// Issue 7: YALNIZ tam qoşulmuş (invite_status='active') üzvlər. `pending`
	// dəvətli mesaj MƏZMUNUNU almamalıdır.
	memberIDs := h.GroupMemberIDs(conversationID)
	wsPayload := map[string]interface{}{
		"id":                            messageID,
		"conversation_id":               conversationID,
		"chat_type":                     "group",
		"sender_id":                     senderID,
		"sender_name":                   senderInfo.Name,
		"sender_username":               senderInfo.Username,
		"sender_is_verified":            senderInfo.IsVerified,
		"sender_special_badge_icon_url": senderInfo.SpecialBadgeIconURL,
		"sender_avatar":                 utils.PrependBaseURL(senderInfo.ProfileImage),
		"text":                          text,
		"type":                          kind,
		"reply_to_message_id":           nil,
		"reply_to_message":              nil,
		"is_edited":                     false,
		"is_starred_by_me":              false,
		"reactions":                     []interface{}{},
		"created_at":                    now.UTC().Format(time.RFC3339),
	}
	h.SendToMultipleUsers(memberIDs, "new_group_message", wsPayload)

	// 📖 AVTO-OKUNDU — qrup səhifəsi hazırda AÇIQ olan üzvlər üçün (REST yolu
	// ilə eyni toplu naxış: tək kilid + 2 sorğu).
	go func() {
		readNow := time.Now()
		targets := make([]uint, 0, len(memberIDs))
		for _, mid := range h.FilterUsersInGroupChat(memberIDs, conversationID) {
			if mid == senderID {
				continue
			}
			targets = append(targets, mid)
		}
		if len(targets) == 0 {
			return
		}
		reads := make([]models.MessageRead, 0, len(targets))
		for _, mid := range targets {
			reads = append(reads, models.MessageRead{
				MessageID:      messageID,
				UserID:         mid,
				ConversationID: conversationID,
				ReadAt:         readNow,
				CreatedAt:      readNow,
			})
		}
		if err := h.db.Clauses(clause.OnConflict{DoNothing: true}).
			CreateInBatches(&reads, 500).Error; err != nil {
			log.Printf("XMPP qrup avto-oxundu toplu insert xətası (conv=%d): %v", conversationID, err)
		}
		if err := h.db.Model(&models.ConversationParticipant{}).
			Where("conversation_id = ? AND user_id IN ?", conversationID, targets).
			Updates(map[string]interface{}{
				"last_read_at":         readNow,
				"last_read_message_id": messageID,
			}).Error; err != nil {
			log.Printf("XMPP qrup avto-oxundu last_read yeniləmə xətası (conv=%d): %v", conversationID, err)
		}
	}()

	// 🔔 Gecikməli qrup push-u — sessizə almayan AKTİV üzvlərə. REST yolundakı
	// `ScheduleGroupPushNotification` çağırışı ilə eyni (10 s + oxundu/açıq
	// səhifə yoxlaması onun içindədir).
	var pushTargets []uint
	h.db.Model(&models.ConversationParticipant{}).
		Where("conversation_id = ? AND user_id != ? AND left_at IS NULL AND deleted_at IS NULL", conversationID, senderID).
		Where("COALESCE(invite_status, 'active') = 'active'").
		Where("is_muted = false").
		Pluck("user_id", &pushTargets)
	if len(pushTargets) > 0 {
		groupName := ""
		if conv.GroupName != nil {
			groupName = *conv.GroupName
		}
		h.ScheduleGroupPushNotification(
			conversationID, senderID, groupName, text, messageID,
			pushTargets, 10*time.Second,
		)
	}

	// QEYD: moderasiya növbəsi QƏSDƏN çağırılmır — REST `SendGroupMessage` də
	// çağırmır (moderasiya hazırda yalnız 1:1 axınındadır, iş strukturu tək
	// `receiver_id` gözləyir). Qrup moderasiyası açılarsa hər iki yol birlikdə
	// dəyişməlidir.
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
