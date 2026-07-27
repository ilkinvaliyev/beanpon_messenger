package handlers

import (
	"beanpon_messenger/database"
	"beanpon_messenger/models"
	"beanpon_messenger/services"
	"beanpon_messenger/utils"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// moderationEnqueuer — şübhəli mesaj analizi üçün queue-ya iş qoymaq üçün
// minimal interfeys. services.ModerationQueue bunu ödəyir. Qeyri-bloklayıcıdır.
type moderationEnqueuer interface {
	Enqueue(job services.ModerationJob)
}

type MessageHandler struct {
	encryptionService interface {
		EncryptMessage(plainText string) (string, error)
		DecryptMessage(encryptedText string) (string, error)
	}
	wsHub interface {
		HandleNewMessage(senderID, receiverID uint, messageID, content, msgType string, createdAt time.Time, replyToMessageID *string, storyID *uint, conversationStatus string, silent bool) // conversationStatus + silent eklendi
		HandleMessageRead(messageID string, senderID, readerID uint)
		IsUserOnline(userID uint) bool
		SendToUser(userID uint, messageType string, data interface{})
		// Issue 19: oxundu yolunda TEK aqreqat unread yeniləməsi üçün.
		SendUnreadCountUpdate(userID uint)
	}
	// moderationQueue — opsional. nil olduqda moderasiya sakitcə atlanır.
	moderationQueue moderationEnqueuer
}

func NewMessageHandler(encryptionService interface {
	EncryptMessage(plainText string) (string, error)
	DecryptMessage(encryptedText string) (string, error)
}, wsHub interface {
	HandleNewMessage(senderID, receiverID uint, messageID, content, msgType string, createdAt time.Time, replyToMessageID *string, storyID *uint, conversationStatus string, silent bool) // conversationStatus + silent eklendi
	HandleMessageRead(messageID string, senderID, readerID uint)
	IsUserOnline(userID uint) bool
	SendToUser(userID uint, messageType string, data interface{})
	SendUnreadCountUpdate(userID uint)
}) *MessageHandler {
	return &MessageHandler{
		encryptionService: encryptionService,
		wsHub:             wsHub,
	}
}

// SetModerationQueue — moderasiya queue-sunu handler-a bağlayır.
// main.go-da wsHub və queue qurulduqdan sonra çağırılır.
func (h *MessageHandler) SetModerationQueue(q moderationEnqueuer) {
	h.moderationQueue = q
}

type wsHubForConversation interface {
	IsUserOnline(userID uint) bool
	SendToUser(userID uint, messageType string, data interface{})
	BroadcastScreenshotProtectionChange(user1ID, user2ID uint, isDisabled bool, changedByUserID uint)
}

// SendMessage mesaj gönder
func (h *MessageHandler) SendMessage(c *gin.Context) {
	// JWT'den user ID al
	senderID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	var req struct {
		ReceiverID       uint    `json:"receiver_id" binding:"required"`
		Text             string  `json:"text" binding:"required"`
		Type             string  `json:"type,omitempty"`
		StoryID          *uint   `json:"story_id,omitempty"` // BU SATIRI EKLE
		ReplyToMessageID *string `json:"reply_to_message_id,omitempty"`
		// Səssiz göndərmə: true olduqda qarşı tərəfə push notification GETMİR
		// (mesaj normal çatır, WS yayılır). Opsional — köhnə client-lər
		// göndərməsə false olur (adi davranış).
		Silent bool `json:"silent,omitempty"`
		// Issue 9: istemçi tərəfli idempotentlik açarı (UUID). Bax
		// handlers/idempotency.go. Opsional — verilməzsə server UUID yaradır.
		ClientMessageID *string `json:"client_message_id,omitempty"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Issue 9: mesaj ID-si — istemçi verdisə onu işlət (təkrar göndərmə
	// dublikat yaratmasın), yoxsa server UUID-i.
	messageID, _, err := resolveMessageID(req.ClientMessageID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Kendi kendine mesaj göndermesini engelle
	//if senderID.(uint) == req.ReceiverID {
	//	c.JSON(http.StatusBadRequest, gin.H{"error": "Kendi kendinize mesaj gönderemezsiniz"})
	//	return
	//}

	// Block kontrolü ekle
	if models.IsBlocked(database.DB, senderID.(uint), req.ReceiverID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Bu kullanıcıya mesaj gönderemezsiniz"})
		return
	}

	// 🚫 SPAM SHADOW-BAN — GLOBAL (yeni VƏ mövcud conversation üçün).
	//
	// Yalnız `actions` sütununa baxılır. Qaydalar:
	//   • actions = NULL                          → mesaj BLOKLANIR
	//   • actions massivində "message" var        → mesaj BLOKLANIR
	//   • actions = ["post"], ["story"], ["post","story"] və s. (message yox)
	//                                              → mesaj GEDƏ BİLƏR
	//   • spam_bans-da aktiv qeyd yoxdursa        → mesaj GEDƏ BİLƏR
	//
	// Yəni admin "post" və ya "story" üçün ban verə bilər, bu mesajlaşmaya
	// təsir etmir. Yalnız `actions`-da açıq şəkildə "message" varsa və ya
	// `actions` heç təyin olunmayıbsa (NULL — "hamısı") mesajlaşma dayanır.
	//
	// Davranış: shadow-ban
	//   • Göndərənə 201 sahte response qaytarılır (uydurma UUID ilə)
	//   • DB-yə YAZILMIR
	//   • WebSocket ilə qarşı tərəfə YAYILMIR
	//   • Push notification GETMİR
	//   • Moderasiya queue-ya QOYULMUR
	//   • Conversation yaradılmır / yenilənmir
	// 🟢 İSTİSNA: receiver_id == 1 olan istifadəçiyə HƏMİŞƏ mesaj gedə bilər.
	// Spam/shadow-ban olsa belə bu istifadəçiyə yazmağa icazə verilir.
	if req.ReceiverID != 1 && models.IsMessagingBannedByActions(database.DB, senderID.(uint)) {
		log.Printf("🚫 SPAM SHADOW-BAN: sender_id=%d → receiver_id=%d mesajı bloklandı (DB yazılmadı, WS yayılmadı, push yox)",
			senderID.(uint), req.ReceiverID)
		c.JSON(http.StatusCreated, gin.H{
			"message": "Mesaj başarıyla gönderildi",
			"data": gin.H{
				"id":          uuid.New().String(),
				"sender_id":   senderID.(uint),
				"receiver_id": req.ReceiverID,
				"text":        req.Text,
				"read":        false,
				"created_at":  time.Now().UTC(),
				"is_online":   h.wsHub.IsUserOnline(req.ReceiverID),
			},
		})
		return
	}

	wsHubForConv := h.wsHub.(wsHubForConversation)

	// Conversation kontrolü - mesaj gönderebilir mi?
	// 🟢 İSTİSNA: receiver_id == 1 olduqda heç bir göndərmə yoxlaması (spam,
	// verified, follow, limit, restricted) tətbiq olunmur — mesaj həmişə gedir.
	conversationHandler := NewConversationHandler(wsHubForConv, h.encryptionService)
	canSend, reason, err := true, "", error(nil)
	if req.ReceiverID != 1 {
		canSend, reason, err = conversationHandler.CanSendMessage(senderID.(uint), req.ReceiverID)
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Conversation kontrolü başarısız"})
		return
	}

	if !canSend {
		// 🚫 SPAM: spam'lı kullanıcı conversation başlatamaz/mesaj gönderemez.
		// Sessizce başarısız ol — kullanıcıya 403/hata gösterme, ama mesaj da
		// kaydetme. Gönderene başarılıymış gibi 201 dön (shadow-ban davranışı).
		// CanSendMessage spam durumunda reason="" döner; diğer red sebepleri
		// (verified, follow, limit, restricted) dolu bir reason ile gelir.
		if reason == "" {
			c.JSON(http.StatusCreated, gin.H{
				"message": "Mesaj başarıyla gönderildi",
				"data": gin.H{
					"id":          uuid.New().String(),
					"sender_id":   senderID.(uint),
					"receiver_id": req.ReceiverID,
					"text":        req.Text,
					"read":        false,
					"created_at":  time.Now().UTC(),
					"is_online":   h.wsHub.IsUserOnline(req.ReceiverID),
				},
			})
			return
		}

		c.JSON(http.StatusForbidden, gin.H{"error": reason})
		return
	}

	// Mesajı şifrele
	encryptedText, err := h.encryptionService.EncryptMessage(req.Text)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Mesaj şifrelenirken hata oluştu"})
		return
	}

	if req.StoryID != nil {
		var story models.Story
		err := database.DB.Where("id = ?", *req.StoryID).First(&story).Error
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Story bulunamadı"})
			return
		}

		if story.UserID != req.ReceiverID {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Story ve alıcı uyuşmuyor"})
			return
		}

		if story.UserID == senderID.(uint) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Kendi story'nize mesaj gönderemezsiniz"})
			return
		}
	}

	// Issue 56: bütün icazə qapılarından (blok, spam-ban, CanSendMessage,
	// story yoxlaması) SONRA — mətn hələ ŞİFRƏLƏNMƏYİB, S3 media açarlarını
	// məhz burada çıxarıb "istifadə olunub" işarələyirik. Şifrələmədən sonra
	// bu mümkün deyil. Rədd edilən göndərmələrdə işarələməmək vacibdir: əks
	// halda heç vaxt istifadə olunmayan media əbədi qalardı.
	services.MarkMediaReferenced(req.Text)

	// Veritabanına kaydet
	message := models.Message{
		ID:               messageID,
		SenderID:         senderID.(uint),
		ReceiverID:       &req.ReceiverID,
		EncryptedText:    encryptedText,
		ReplyToMessageID: req.ReplyToMessageID,
		StoryID:          req.StoryID,
		Read:             false,
		CreatedAt:        time.Now().UTC(),
		UpdatedAt:        time.Now().UTC(),
	}

	// Issue 40: mesaj insert-i + conversation indeks yeniləməsi TEK
	// TRANSACTION. Əvvəl ayrı-ayrı idi və conversation yeniləməsinin xətası
	// yalnız log-lanıb UDULURDU: mesaj sətri komit olunur, amma söhbət
	// siyahısındakı `last_message_at`/sayğaclar/status köhnə qalırdı →
	// siyahı yenilənmir, pending→active keçmir. İndi ya hər ikisi, ya heç biri.
	// Redis I/O (mesaj banı yoxlaması) transaction-dan KƏNARDA — açıq bir
	// transaction-ı şəbəkə gözləməsi ilə saxlamaq hovuzu kilidləyir.
	skipConvCreate := conversationHandler.ShouldSkipConversationCreate(senderID.(uint), req.ReceiverID)

	// Issue 9: `duplicate` — istemçi eyni `client_message_id` ilə TƏKRAR
	// göndərdi. Sətir onsuz da var; sayğac/yayım/push TƏKRARLANMAMALIDIR.
	var duplicate *models.Message
	// Issue 10: push qapısı üçün GERÇƏK söhbət statusu. Aşağıda
	// `HandleNewMessage`-ə əvvəl SABİT `"active"` ötürülürdü — WS yolu isə
	// həqiqi statusu ötürür. Bu uyğunsuzluq eyni mesajın nəqliyyatdan asılı
	// olaraq push doğurub-doğurmamasına səbəb olurdu.
	conversationStatus := ""
	if err := database.DB.Transaction(func(tx *gorm.DB) error {
		res := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "id"}},
			DoNothing: true,
		}).Create(&message)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			// Yumşaq silinmiş sətir də tapılmalıdır — əks halda silinmiş
			// mesajın ID-si ilə INSERT sonsuz "tapılmadı" döngüsünə düşərdi.
			var existing models.Message
			if err := tx.Unscoped().Where("id = ?", message.ID).First(&existing).Error; err != nil {
				return err
			}
			duplicate = &existing
			return nil
		}
		status, uErr := conversationHandler.UpdateConversationOnMessageTx(tx, senderID.(uint), req.ReceiverID, skipConvCreate)
		if uErr != nil {
			return uErr
		}
		conversationStatus = status
		return nil
	}); err != nil {
		log.Printf("Mesaj/conversation transaction xətası: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Mesaj kaydedilemedi"})
		return
	}

	if duplicate != nil {
		// ── Təkrar cəhdin ƏSL təkrar olduğunu TAM YOXLA ────────────────────
		// Yalnız `sender_id`-ni yoxlamaq KİFAYƏT DEYİL: eyni istifadəçi eyni
		// `client_message_id`-ni FƏRQLİ bir söhbətdə (qrupda, ya da BAŞQA
		// alıcıya DM-də) işlətsə — istemçi səhvi, offline növbədə id təkrar
		// istifadəsi və s. — server 200 `duplicate:true` qaytarardı və YENİ
		// mesaj HEÇ VAXT yaradılmazdı. Yəni istifadəçi "göndərildi" görür,
		// mesaj isə heç yerə çatmır (SƏSSİZ İTKİ). Ona görə alıcı VƏ söhbət
		// növü də uyğun gəlməlidir; gəlmirsə 409.
		sameSender := duplicate.SenderID == senderID.(uint)
		sameReceiver := duplicate.ReceiverID != nil && *duplicate.ReceiverID == req.ReceiverID
		isDM := duplicate.ConversationID == nil
		if !sameSender || !sameReceiver || !isDM {
			c.JSON(http.StatusConflict, gin.H{
				"error": "client_message_id artıq başqa mesaj üçün istifadə olunub",
				"code":  "CLIENT_MESSAGE_ID_TAKEN",
			})
			return
		}

		// Eyni göndərənin eyni alıcıya təkrar cəhdi → EYNİ nəticə, yan effekt YOX.
		// Mətn DB-dəki SAXLANMIŞ nüsxədən qaytarılır (istemçinin bu dəfə
		// göndərdiyi mətndən DEYİL) — əks halda cavab serverdə olmayan bir
		// məzmunu təsdiqləmiş olardı.
		dupText, decErr := h.encryptionService.DecryptMessage(duplicate.EncryptedText)
		if decErr != nil {
			dupText = req.Text
		}
		c.JSON(http.StatusOK, gin.H{
			"message":   "Mesaj başarıyla gönderildi",
			"duplicate": true,
			"data": gin.H{
				"id":                  duplicate.ID,
				"sender_id":           duplicate.SenderID,
				"receiver_id":         duplicate.ReceiverID,
				"reply_to_message_id": duplicate.ReplyToMessageID,
				"text":                dupText,
				"read":                duplicate.Read,
				"created_at":          duplicate.CreatedAt,
				"is_online":           h.wsHub.IsUserOnline(req.ReceiverID),
			},
		})
		return
	}

	// WebSocket üzerinden real-time yayınla (hem gönderen hem alıcıya)
	h.wsHub.HandleNewMessage(
		message.SenderID,
		*message.ReceiverID,
		message.ID,
		req.Text,
		req.Type,
		message.CreatedAt,
		req.ReplyToMessageID, // YENİ parametre
		req.StoryID,
		conversationStatus, // Issue 10: sabit "active" DEYİL, gerçək status
		req.Silent,         // səssiz göndərmə → push getməsin
	)

	// 🔍 MODERASIYA — mesaj şifrələnib göndərildi, indi arxa planda analizə
	// qoyuruq. Enqueue() qeyri-bloklayıcıdır: bu sətir mesaj göndərmə
	// sürətinə HEÇ təsir etmir. Yalnız text tipli mesajları analiz edirik
	// (image/video/voice mətn daşımır).
	if h.moderationQueue != nil && (req.Type == "" || req.Type == "text") {
		h.moderationQueue.Enqueue(services.ModerationJob{
			MessageID:  message.ID,
			SenderID:   message.SenderID,
			ReceiverID: req.ReceiverID,
			PlainText:  req.Text,
			CreatedAt:  message.CreatedAt,
		})
	}

	// API response
	c.JSON(http.StatusCreated, gin.H{
		"message": "Mesaj başarıyla gönderildi",
		"data": gin.H{
			"id":                  message.ID,
			"sender_id":           message.SenderID,
			"receiver_id":         message.ReceiverID,
			"reply_to_message_id": message.ReplyToMessageID,
			"text":                req.Text,
			"read":                message.Read,
			"created_at":          message.CreatedAt,
			"is_online":           h.wsHub.IsUserOnline(req.ReceiverID),
		},
	})
}

// BroadcastMessage — eyni mətni bir neçə (maks 20) istifadəçiyə TOPLU göndərir.
// Hər alıcı üçün ayrıca mesaj yaradılır (SendMessage ilə eyni addımlar: icazə
// yoxlaması, şifrələmə, conversation update, WS yayımı, push, moderasiya).
// Bir alıcı uğursuz olsa (məs. spam/icazə yox) digərləri davam edir.
func (h *MessageHandler) BroadcastMessage(c *gin.Context) {
	senderIDVal, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	senderID := senderIDVal.(uint)

	var req struct {
		ReceiverIDs []uint `json:"receiver_ids" binding:"required"`
		Text        string `json:"text" binding:"required"`
		Type        string `json:"type,omitempty"`
		Silent      bool   `json:"silent,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Dedup + özünü çıxar + maks 20 limiti.
	seen := map[uint]bool{}
	var targets []uint
	for _, id := range req.ReceiverIDs {
		if id == 0 || id == senderID || seen[id] {
			continue
		}
		seen[id] = true
		targets = append(targets, id)
		if len(targets) >= 20 {
			break
		}
	}
	if len(targets) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçerli alıcı yok"})
		return
	}

	wsHubForConv := h.wsHub.(wsHubForConversation)
	conversationHandler := NewConversationHandler(wsHubForConv, h.encryptionService)

	var sentTo []uint
	for _, receiverID := range targets {
		if models.IsBlocked(database.DB, senderID, receiverID) {
			continue
		}
		if receiverID != 1 && models.IsMessagingBannedByActions(database.DB, senderID) {
			continue // shadow-ban: səssizcə atla
		}
		if receiverID != 1 {
			canSend, _, cErr := conversationHandler.CanSendMessage(senderID, receiverID)
			if cErr != nil || !canSend {
				continue
			}
		}

		encryptedText, encErr := h.encryptionService.EncryptMessage(req.Text)
		if encErr != nil {
			continue
		}
		// Issue 56: media istinadını işarələ (şifrələmədən əvvəlki mətndən).
		services.MarkMediaReferenced(req.Text)

		rid := receiverID
		message := models.Message{
			ID:            uuid.New().String(),
			SenderID:      senderID,
			ReceiverID:    &rid,
			EncryptedText: encryptedText,
			Read:          false,
			CreatedAt:     time.Now().UTC(),
			UpdatedAt:     time.Now().UTC(),
		}
		if err := database.DB.Create(&message).Error; err != nil {
			continue
		}

		// Issue 10: push qapısı GERÇƏK statusdan qidalansın (sabit "active"
		// deyil) — REST/WS arasında bildiriş davranışı fərqlənməsin.
		convStatus, cuErr := conversationHandler.UpdateConversationOnMessageTx(
			database.DB, senderID, receiverID,
			conversationHandler.ShouldSkipConversationCreate(senderID, receiverID),
		)
		if cuErr != nil {
			// Issue 10: FAIL-CLOSED. Əvvəl burada `convStatus = "active"`
			// yazılırdı — yəni statusu OXUYA BİLMƏDİYİMİZ halda push qapısı
			// TAM AÇILIRDI. Nəticədə `restricted` (tək tərəfli spam limiti
			// aşılmış) və ya heç yaradılmamış söhbətə belə bildiriş gedirdi;
			// üstəlik bu, xətanı BİLƏRƏKDƏN tetikləyən spam üçün asan bir
			// keçid idi. İndi statusu bilmiriksə push GÖNDƏRİLMİR: boş status
			// `HandleNewMessage`-in switch-ində heç bir budağa düşmür
			// (bax websocket/hub.go). Canlı WS çatdırma isə davam edir —
			// alıcı çatı açıqdırsa mesajı yenə də görür.
			log.Printf("Broadcast conversation güncellemesi başarısız — push GÖNDƏRİLMİR (rcv=%d): %v",
				receiverID, cuErr)
			convStatus = ""
		}

		h.wsHub.HandleNewMessage(
			message.SenderID,
			*message.ReceiverID,
			message.ID,
			req.Text,
			req.Type,
			message.CreatedAt,
			nil,
			nil,
			convStatus,
			req.Silent,
		)

		if h.moderationQueue != nil && (req.Type == "" || req.Type == "text") {
			h.moderationQueue.Enqueue(services.ModerationJob{
				MessageID:  message.ID,
				SenderID:   message.SenderID,
				ReceiverID: receiverID,
				PlainText:  req.Text,
				CreatedAt:  message.CreatedAt,
			})
		}

		sentTo = append(sentTo, receiverID)
	}

	c.JSON(http.StatusCreated, gin.H{
		"message": "Toplu mesaj gönderildi",
		"sent_to": sentTo,
		"count":   len(sentTo),
	})
}

func (h *MessageHandler) GetMessages(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	// Soft-throttle: bad_traffic flag-lı user-in mesaj siyahısı gecikir.
	throttleBadTraffic(int64(userID.(uint)))

	otherUserID, err := strconv.ParseUint(c.Param("user_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz kullanıcı ID"})
		return
	}

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	// Sanitizasiya: `strconv.Atoi` xətası yeyilirdi → `?limit=abc` limit=0
	// verirdi. Bu, `has_more = len(rows) >= limit` şərtini BOŞ səhifədə belə
	// həmişə true edir (istemçidə sonsuz döngü); mənfi dəyər isə
	// `LIMIT -1 / OFFSET -50` ilə Postgres xətası → 500.
	if limit < 1 || limit > 100 {
		limit = 50
	}
	if page < 1 {
		page = 1
	}
	// Yuxarı hədd: `(page-1)*limit` int daşması ilə MƏNFİ offset verə bilir
	// (`?page=200000000000000000`) → Postgres `OFFSET must not be negative` → 500.
	if page > 100000 {
		page = 100000
	}
	offset := (page - 1) * limit

	// peek=true — ÖNİZLƏMƏ rejimi (uzun-bas preview): mesajlar OXUNDU
	// İŞARƏLƏNMİR. WhatsApp davranışı — peek görüldü sayılmır.
	peek := c.DefaultQuery("peek", "false") == "true"

	// ── Issue 73/20: KEYSET (cursor) sayfalama — `before=<message_id>` ──────
	//
	// OFFSET sayfalaması iki cür pozulurdu:
	//   • Canlı mesaj gəldikcə siyahının BAŞI böyüyür → "səhifə 2" sürüşür,
	//     istemçiyə tamamilə TANIŞ id-lər qayıdır. iOS ChatViewModel.loadOlder
	//     bunu "yeni heç nə yoxdur" sayıb `page`-i ARTIRMIRDI → eyni səhifə
	//     sonsuza qədər yenidən çəkilirdi (spinner heç bitmirdi).
	//   • Server tərəfdə silinmə offset-ləri sürüşdürür → ARADA mesaj ATLANIR.
	//
	// `before` verilibsə həmin mesajdan KÖHNƏ olanlar qaytarılır. Müqayisə
	// TAM sıralamadır — `(created_at, id)` cütü — çünki `created_at` unikal
	// deyil (sürətli/toplu göndərmə eyni ms-i paylaşır) və yalnız
	// `created_at <` istifadə etsək sərhəddəki sətirlər HƏMİŞƏLİK düşərdi.
	//
	// Geriyə uyğunluq: `before` YOXDURSA davranış tamamilə əvvəlki kimidir
	// (page/offset) — köhnə istemçilər təsirlənmir.
	// `before_ms` — anchor sətri server tərəfdə SİLİNİBSƏ istifadə olunan
	// ehtiyat kursor (istemçidəki `created_at`, epoch ms). Onsuz stale kursor
	// halında offset-ə düşərdik və HƏMİŞƏ ən yeni səhifə qayıdardı → istemçi
	// heç vaxt geriyə gedə bilməzdi (eyni sonsuz döngü).
	beforeID := strings.TrimSpace(c.Query("before"))
	beforeMs, _ := strconv.ParseInt(c.DefaultQuery("before_ms", "0"), 10, 64)
	// `before_ms` üçün ağlabatan pəncərə (1970 … 2100). Hədsiz dəyər
	// `time.UnixMilli` ilə timestamptz sərhədini aşıb driver-də 500 verir.
	const maxBeforeMs int64 = 4102444800000 // 2100-01-01
	if beforeMs < 0 || beforeMs > maxBeforeMs {
		beforeMs = 0
	}

	// `messages.id` UUID sütunudur (models/message.go). Kursor sentinel-ləri
	// də UUID OLMALIDIR — boş sətir `invalid input syntax for type uuid` verir.
	const maxUUID = "ffffffff-ffff-ffff-ffff-ffffffffffff"

	var beforeCreatedAt *time.Time
	// DİQQƏT: keyset İŞLƏNMƏSƏ BELƏ bu dəyər HƏMİŞƏ etibarlı UUID olmalıdır.
	// Sorquda `?::uuid` var; Postgres sabit qatlamada (`''::uuid`) DƏRHAL
	// `invalid input syntax for type uuid` verir — `?::timestamptz IS NULL`
	// qısa-qapanması buna ZƏMANƏT vermir.
	beforeCursorID := maxUUID
	if beforeID != "" {
		var anchor struct {
			CreatedAt time.Time `gorm:"column:created_at"`
		}
		// Anchor yalnız BU söhbətdən ola bilər (IDOR qapalı). `id = ?::uuid`
		// olduğu üçün formatı pozuq `before` Postgres xətası verir — Scan xətası
		// kimi tutulur və aşağıdakı fallback işə düşür.
		e := database.DB.Raw(`
			SELECT created_at FROM messages
			WHERE id = ?::uuid
			  AND ((sender_id = ? AND receiver_id = ?) OR (sender_id = ? AND receiver_id = ?))
			LIMIT 1
		`, beforeID, userID, otherUserID, otherUserID, userID).Scan(&anchor).Error
		if e == nil && !anchor.CreatedAt.IsZero() {
			ts := anchor.CreatedAt
			beforeCreatedAt = &ts
			beforeCursorID = beforeID
		}
	}
	if beforeCreatedAt == nil && beforeMs > 0 {
		// Anchor tapılmadı (sətir silinib / format pozuq) → istemçinin
		// bildirdiyi vaxta düş.
		//
		// DİQQƏT — MİLLİSANİYƏ YUVARLAQLAŞMASI: `created_at` timestamptz-dir
		// (mikrosaniyə). `before_ms` ms-ə kəsildiyi üçün strict `<` işlətsək
		// eyni ms içindəki DAHA KÖHNƏ sətirlər (məs. .000200 < .000750) heç
		// vaxt qayıtmazdı — Issue 20-nin eyni sinif xətası. Ona görə pəncərəni
		// bir ms İRƏLİ sürüb sentinel kimi ƏN BÖYÜK uuid-i veririk: bütün ms
		// dilimi daxil olur. Təkrar sətirlər zərərsizdir (istemçi id ilə dedup
		// edir); İTKİ isə geri qaytarıla bilməzdir.
		ts := time.UnixMilli(beforeMs).UTC().Add(time.Millisecond)
		beforeCreatedAt = &ts
		beforeCursorID = maxUUID
	} else if beforeCreatedAt == nil && beforeID != "" {
		// `before` göndərilib, amma nə anchor tapıldı, nə də `before_ms` var.
		// SESSİZCƏ offset-ə düşmək TƏHLÜKƏLİDİR: page=1 ilə ƏN YENİ səhifə
		// "daha köhnə tarixçə" kimi qaytarılardı → dublikat + sonsuz döngü.
		// Boş, "daha yoxdur" səhifəsi qaytarmaq təhlükəsizdir.
		c.JSON(http.StatusOK, gin.H{
			"data":                    []gin.H{},
			"page":                    page,
			"limit":                   limit,
			"total":                   0,
			"has_more":                false,
			"next_before":             nil,
			"first_unread_message_id": nil,
			"is_online":               h.wsHub.IsUserOnline(uint(otherUserID)),
			"cursor_invalid":          true,
		})
		return
	}
	if beforeCreatedAt != nil {
		offset = 0 // keyset rejimində offset MƏNASIZDIR
	}
	// keysetMode — cavabdakı `has_more`/`first_unread` davranışını seçir.
	keysetMode := beforeCreatedAt != nil

	var messages []struct {
		ID                   string     `gorm:"column:id"`
		SenderID             uint       `gorm:"column:sender_id"`
		ReceiverID           uint       `gorm:"column:receiver_id"`
		StoryID              *uint      `gorm:"column:story_id"`
		StoryMetadata        *string    `gorm:"column:story_metadata"`
		ReplyToMessageID     *string    `gorm:"column:reply_to_message_id"`
		EncryptedText        string     `gorm:"column:encrypted_text"`
		Read                 bool       `gorm:"column:read"`
		Delivered            bool       `gorm:"column:delivered"` // ← YENİ (iki tick)
		IsEdited             bool       `gorm:"column:is_edited"` // ← YENİ
		SenderReaction       *string    `gorm:"column:sender_reaction"`
		ReceiverReaction     *string    `gorm:"column:receiver_reaction"`
		StarredBySender      bool       `gorm:"column:starred_by_sender"`
		StarredByReceiver    bool       `gorm:"column:starred_by_receiver"`
		CreatedAt            time.Time  `gorm:"column:created_at"`
		UpdatedAt            time.Time  `gorm:"column:updated_at"`
		ReplyToMessageText   *string    `gorm:"column:reply_to_message_text"`
		ReplyToMessageSender *uint      `gorm:"column:reply_to_message_sender"`
		ReplyToCreatedAt     *time.Time `gorm:"column:reply_to_created_at"`
		StoryType            *string    `gorm:"column:story_type"`
		StoryMediaURL        *string    `gorm:"column:story_media_url"`
		StoryContent         *string    `gorm:"column:story_content"`
		StoryUserID          *uint      `gorm:"column:story_user_id"`
		StoryCreatedAt       *time.Time `gorm:"column:story_created_at"`
	}

	query := `
        SELECT 
            m.*,
            reply.encrypted_text as reply_to_message_text,
            reply.sender_id as reply_to_message_sender,
            reply.created_at as reply_to_created_at,
            s.type as story_type,
            s.media_url as story_media_url,
            s.content as story_content,
            s.media_metadata as story_metadata,
            s.user_id as story_user_id,
            s.created_at as story_created_at
        FROM messages m
        LEFT JOIN messages reply ON m.reply_to_message_id = reply.id
        LEFT JOIN stories s ON m.story_id = s.id
        WHERE ((m.sender_id = ? AND m.receiver_id = ?) OR (m.sender_id = ? AND m.receiver_id = ?))
        -- Issue 20: deleted_at süzgəci BU sorğuda ÇATIŞMIRDI — qardaş
        -- sorğular (SearchMessages, SyncMessages, first_unread) onu tətbiq
        -- edir. Nəticədə "hamı üçün sil" edilmiş mesaj çat siyahısında
        -- görünməyə davam edirdi.
        AND m.deleted_at IS NULL
        AND (
            CASE
                WHEN m.sender_id = ? THEN m.is_deleted_by_sender = false
                ELSE m.is_deleted_by_receiver = false
            END
        )
        -- Issue 73: keyset cursor. before YOXDURSA (NULL) bu şərt həmişə TRUE.
        AND (
            ?::timestamptz IS NULL
            OR (m.created_at, m.id) < (?::timestamptz, ?::uuid)
        )
        -- Issue 20: created_at unikal deyil → id ilə TAM sıralama.
        ORDER BY m.created_at DESC, m.id DESC
        LIMIT ? OFFSET ?
    `

	err = database.DB.Raw(query,
		userID, otherUserID, otherUserID, userID,
		userID,
		beforeCreatedAt, beforeCreatedAt, beforeCursorID,
		limit, offset,
	).Scan(&messages).Error

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Mesajlar alınamadı"})
		return
	}

	// DİQQƏT: `var x []gin.H` (nil slice) JSON-da `null` olur. İstemçi
	// `data`-nı massiv kimi parse edir və `null` gördükdə cavabı BOZUQ sayıb
	// atır (Issue 5 qoruması) → `has_more:false` siqnalı ÇATMIR və sonsuz
	// spinner qayıdır. Boş massiv `[]` olaraq serialize olunmalıdır.
	responseMessages := make([]gin.H, 0, len(messages))
	for _, msg := range messages {
		decryptedText, err := h.encryptionService.DecryptMessage(msg.EncryptedText)
		if err != nil {
			decryptedText = "Mesaj çözülemedi"
		}

		responseMessage := gin.H{
			"id":                  msg.ID,
			"sender_id":           msg.SenderID,
			"receiver_id":         msg.ReceiverID,
			"story_id":            msg.StoryID,
			"reply_to_message_id": msg.ReplyToMessageID,
			"text":                decryptedText,
			"read":                msg.Read,
			"delivered":           msg.Delivered, // ← YENİ (iki tick)
			"is_edited":           msg.IsEdited,  // ← YENİ
			"sender_reaction":     msg.SenderReaction,
			"receiver_reaction":   msg.ReceiverReaction,
			"is_starred_by_me":    starredByUser(userID.(uint), msg.SenderID, msg.StarredBySender, msg.StarredByReceiver),
			"created_at":          msg.CreatedAt,
			"updated_at":          msg.UpdatedAt,
		}

		if msg.StoryID != nil {
			if msg.StoryType != nil {
				storyResponse := gin.H{
					"id":         *msg.StoryID,
					"type":       *msg.StoryType,
					"media_url":  utils.PrependS3URL(msg.StoryMediaURL),
					"content":    msg.StoryContent,
					"user_id":    *msg.StoryUserID,
					"created_at": msg.StoryCreatedAt,
					"available":  true,
				}

				if *msg.StoryType == "video" && msg.StoryMetadata != nil {
					var metadata map[string]interface{}
					if err := json.Unmarshal([]byte(*msg.StoryMetadata), &metadata); err == nil {
						if thumbnailURL, exists := metadata["thumbnail_url"].(string); exists && thumbnailURL != "" {
							storyResponse["thumbnail_url"] = utils.PrependS3URL(&thumbnailURL)
						}
					}
				}

				responseMessage["story"] = storyResponse
			} else {
				responseMessage["story"] = gin.H{
					"id":        *msg.StoryID,
					"available": false,
					"message":   "Bu story artık mevcut değil",
				}
			}
		}

		if msg.ReplyToMessageID != nil && msg.ReplyToMessageText != nil {
			replyDecryptedText, err := h.encryptionService.DecryptMessage(*msg.ReplyToMessageText)
			if err != nil {
				replyDecryptedText = "Mesaj çözülemedi"
			}

			responseMessage["reply_to_message"] = gin.H{
				"id":         *msg.ReplyToMessageID,
				"sender_id":  msg.ReplyToMessageSender,
				"text":       replyDecryptedText,
				"created_at": msg.ReplyToCreatedAt,
			}
		}

		responseMessages = append(responseMessages, responseMessage)
	}

	// 🆕 İLK OXUNMAMIŞ mesaj id-si — MARK-DAN ƏVVƏL (mark sonra read=true
	// edəcək). Qarşı tərəfdən gələn, oxunmamış (read=false) ən KÖHNƏ mesaj.
	// Flutter açılışda buna konumlanıb "Yeni mesajlar" ayracı qoyur.
	// Yalnız 1-ci səhifədə + peek deyil.
	// Issue 73: keyset (before) səhifəsində "ilk oxunmamış" ayracı hesablanmır
	// — o yalnız çatın İLK açılışına aiddir (qrup handler-i ilə eyni davranış).
	var firstUnreadID *string
	if !peek && page == 1 && !keysetMode {
		var unreadRow struct {
			ID string `gorm:"column:id"`
		}
		e := database.DB.Raw(`
			SELECT id FROM messages
			WHERE sender_id = ? AND receiver_id = ?
			  AND read = false
			  AND is_deleted_by_receiver = false
			  AND deleted_at IS NULL
			ORDER BY created_at ASC
			LIMIT 1
		`, otherUserID, userID).Scan(&unreadRow).Error
		if e == nil && unreadRow.ID != "" {
			firstUnreadID = &unreadRow.ID
		}
	}

	// peek rejimində OXUNDU işarələnmir (önizləmə görüldü sayılmır).
	if !peek {
		go h.markReceivedMessagesAsRead(userID.(uint), uint(otherUserID))
	}

	var totalCount int64
	countQuery := `
        SELECT COUNT(*) 
        FROM messages 
        WHERE ((sender_id = ? AND receiver_id = ?) OR (sender_id = ? AND receiver_id = ?))
        -- Issue 20: siyahı sorğusu ilə EYNİ süzgəc. Bu olmadan total sərt
        -- silinmiş sətirləri də sayırdı → son səhifədə has_more true qalır,
        -- data isə boş gəlirdi (istemçi page-i artırmır → sonsuz spinner).
        AND deleted_at IS NULL
        AND (
            CASE
                WHEN sender_id = ? THEN is_deleted_by_sender = false
                ELSE is_deleted_by_receiver = false
            END
        )
    `

	err = database.DB.Raw(countQuery,
		userID, otherUserID, otherUserID, userID,
		userID,
	).Count(&totalCount).Error

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Toplam sayı alınamadı"})
		return
	}

	// Issue 73: `has_more` — istemçi artıq `total` ilə yerli mesaj sayını
	// müqayisə etməyə məcbur deyil (o müqayisə optimistik/canlı mesajları da
	// sayırdı və vaxtından əvvəl "son səhifə" deyirdi).
	//   • keyset (before) rejimi: tam səhifə gəldisə daha var say;
	//   • offset rejimi: oxunan+offset < total.
	hasMore := false
	if keysetMode {
		hasMore = len(responseMessages) >= limit
	} else {
		hasMore = int64(offset+len(responseMessages)) < totalCount
	}

	// Issue 73: növbəti keyset kursoru — ən KÖHNƏ (siyahının sonuncu) mesajın
	// id-si. Siyahı `created_at DESC, id DESC` sıralıdır.
	var nextBefore *string
	if len(messages) > 0 {
		last := messages[len(messages)-1].ID
		nextBefore = &last
	}

	c.JSON(http.StatusOK, gin.H{
		"data":                    responseMessages,
		"page":                    page,
		"limit":                   limit,
		"total":                   int(totalCount),
		"has_more":                hasMore,
		"next_before":             nextBefore,
		"first_unread_message_id": firstUnreadID,
		"is_online":               h.wsHub.IsUserOnline(uint(otherUserID)),
	})
}

// SearchMessages — in-chat DM text search (WhatsApp-style).
// GET /api/v1/messages/:user_id/search?q=<text>&limit=<1..50, default 25>&before_ms=<int64, optional>
//
// Messages are stored AES-encrypted (encrypted_text) with a random IV, so a
// SQL ILIKE on the text is impossible. Instead the server scans rows in
// batches with EXACTLY the same visibility conditions as GetMessages
// (pair match + is_deleted_by_sender/receiver CASE), plus the DM marker
// used by SyncMessages (conversation_id IS NULL) and the defensive
// deleted_at IS NULL guard, decrypts each row and filters with a
// case-insensitive contains. A single request scans at most
// searchScanCap rows; the client continues with next_before_ms
// (keyset paging on created_at DESC). Media/voice/call JSON payloads
// carry no user text and are skipped; view-once payloads are always
// excluded. Fully additive — old clients never call this endpoint.
func (h *MessageHandler) SearchMessages(c *gin.Context) {
	userIDVal, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	userID := userIDVal.(uint)

	otherUserID, err := strconv.ParseUint(c.Param("user_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz kullanıcı ID"})
		return
	}

	q := strings.TrimSpace(c.Query("q"))
	if q == "" {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "Geçersiz q"})
		return
	}
	qLower := strings.ToLower(q)

	limit, err := strconv.Atoi(c.DefaultQuery("limit", "25"))
	if err != nil || limit <= 0 {
		limit = 25
	}
	if limit > 50 {
		limit = 50
	}

	beforeMs, err := strconv.ParseInt(c.DefaultQuery("before_ms", "0"), 10, 64)
	if err != nil || beforeMs < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "Geçersiz before_ms"})
		return
	}

	// Scan guards: batch page size and a per-request total-rows cap so one
	// request can never walk an entire huge history in a single call.
	const searchBatchSize = 200
	const searchScanCap = 2000

	// Same row shape as GetMessages (reply + story enrichment via joins).
	type searchRow struct {
		ID                   string     `gorm:"column:id"`
		SenderID             uint       `gorm:"column:sender_id"`
		ReceiverID           uint       `gorm:"column:receiver_id"`
		StoryID              *uint      `gorm:"column:story_id"`
		StoryMetadata        *string    `gorm:"column:story_metadata"`
		ReplyToMessageID     *string    `gorm:"column:reply_to_message_id"`
		EncryptedText        string     `gorm:"column:encrypted_text"`
		Read                 bool       `gorm:"column:read"`
		Delivered            bool       `gorm:"column:delivered"`
		IsEdited             bool       `gorm:"column:is_edited"`
		SenderReaction       *string    `gorm:"column:sender_reaction"`
		ReceiverReaction     *string    `gorm:"column:receiver_reaction"`
		StarredBySender      bool       `gorm:"column:starred_by_sender"`
		StarredByReceiver    bool       `gorm:"column:starred_by_receiver"`
		CreatedAt            time.Time  `gorm:"column:created_at"`
		UpdatedAt            time.Time  `gorm:"column:updated_at"`
		ReplyToMessageText   *string    `gorm:"column:reply_to_message_text"`
		ReplyToMessageSender *uint      `gorm:"column:reply_to_message_sender"`
		ReplyToCreatedAt     *time.Time `gorm:"column:reply_to_created_at"`
		StoryType            *string    `gorm:"column:story_type"`
		StoryMediaURL        *string    `gorm:"column:story_media_url"`
		StoryContent         *string    `gorm:"column:story_content"`
		StoryUserID          *uint      `gorm:"column:story_user_id"`
		StoryCreatedAt       *time.Time `gorm:"column:story_created_at"`
	}

	// VISIBILITY: pair + is_deleted_by_* CASE copied verbatim from GetMessages,
	// so search can never return a message the user would not see in the chat.
	// conversation_id IS NULL = DM marker (mirrors SyncMessages); deleted_at
	// guard mirrors SyncMessages/first-unread classification (group-only flow,
	// defensive here).
	baseQuery := `
        SELECT
            m.*,
            reply.encrypted_text as reply_to_message_text,
            reply.sender_id as reply_to_message_sender,
            reply.created_at as reply_to_created_at,
            s.type as story_type,
            s.media_url as story_media_url,
            s.content as story_content,
            s.media_metadata as story_metadata,
            s.user_id as story_user_id,
            s.created_at as story_created_at
        FROM messages m
        LEFT JOIN messages reply ON m.reply_to_message_id = reply.id
        LEFT JOIN stories s ON m.story_id = s.id
        WHERE ((m.sender_id = ? AND m.receiver_id = ?) OR (m.sender_id = ? AND m.receiver_id = ?))
        AND (
            CASE
                WHEN m.sender_id = ? THEN m.is_deleted_by_sender = false
                ELSE m.is_deleted_by_receiver = false
            END
        )
        AND m.conversation_id IS NULL
        AND m.deleted_at IS NULL
        AND m.created_at < ?
        ORDER BY m.created_at DESC
        LIMIT ?
    `

	cursor := time.Now().UTC().Add(time.Hour) // "no cursor" → include everything
	if beforeMs > 0 {
		cursor = time.UnixMilli(beforeMs).UTC()
	}

	matches := make([]gin.H, 0, limit)
	scanned := 0
	exhausted := false

	for scanned < searchScanCap && len(matches) < limit {
		var rows []searchRow
		if err := database.DB.Raw(baseQuery,
			userID, otherUserID, otherUserID, userID,
			userID,
			cursor, searchBatchSize,
		).Scan(&rows).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Mesajlar alınamadı"})
			return
		}

		if len(rows) == 0 {
			exhausted = true
			break
		}

		for _, msg := range rows {
			cursor = msg.CreatedAt
			scanned++

			decryptedText, decErr := h.encryptionService.DecryptMessage(msg.EncryptedText)
			if decErr != nil {
				continue // undecryptable rows are unsearchable
			}

			// Structured payloads (voice/image/video/gif/sound/call/view-once)
			// carry no free text → not searchable; view_once always excluded.
			searchable := decryptedText
			trimmed := strings.TrimSpace(decryptedText)
			if strings.HasPrefix(trimmed, "{") {
				var payload map[string]interface{}
				if json.Unmarshal([]byte(trimmed), &payload) == nil {
					if payload["view_once"] == true {
						continue
					}
					if _, isTyped := payload["type"]; isTyped {
						continue
					}
				}
			}
			if !strings.Contains(strings.ToLower(searchable), qLower) {
				continue
			}

			// Row JSON — same shape as GetMessages.
			responseMessage := gin.H{
				"id":                  msg.ID,
				"sender_id":           msg.SenderID,
				"receiver_id":         msg.ReceiverID,
				"story_id":            msg.StoryID,
				"reply_to_message_id": msg.ReplyToMessageID,
				"text":                decryptedText,
				"read":                msg.Read,
				"delivered":           msg.Delivered,
				"is_edited":           msg.IsEdited,
				"sender_reaction":     msg.SenderReaction,
				"receiver_reaction":   msg.ReceiverReaction,
				"is_starred_by_me":    starredByUser(userID, msg.SenderID, msg.StarredBySender, msg.StarredByReceiver),
				"created_at":          msg.CreatedAt,
				"updated_at":          msg.UpdatedAt,
			}

			if msg.StoryID != nil {
				if msg.StoryType != nil {
					storyResponse := gin.H{
						"id":         *msg.StoryID,
						"type":       *msg.StoryType,
						"media_url":  utils.PrependS3URL(msg.StoryMediaURL),
						"content":    msg.StoryContent,
						"user_id":    *msg.StoryUserID,
						"created_at": msg.StoryCreatedAt,
						"available":  true,
					}
					if *msg.StoryType == "video" && msg.StoryMetadata != nil {
						var metadata map[string]interface{}
						if err := json.Unmarshal([]byte(*msg.StoryMetadata), &metadata); err == nil {
							if thumbnailURL, exists := metadata["thumbnail_url"].(string); exists && thumbnailURL != "" {
								storyResponse["thumbnail_url"] = utils.PrependS3URL(&thumbnailURL)
							}
						}
					}
					responseMessage["story"] = storyResponse
				} else {
					responseMessage["story"] = gin.H{
						"id":        *msg.StoryID,
						"available": false,
						"message":   "Bu story artık mevcut değil",
					}
				}
			}

			if msg.ReplyToMessageID != nil && msg.ReplyToMessageText != nil {
				replyDecryptedText, replyErr := h.encryptionService.DecryptMessage(*msg.ReplyToMessageText)
				if replyErr != nil {
					replyDecryptedText = "Mesaj çözülemedi"
				}
				responseMessage["reply_to_message"] = gin.H{
					"id":         *msg.ReplyToMessageID,
					"sender_id":  msg.ReplyToMessageSender,
					"text":       replyDecryptedText,
					"created_at": msg.ReplyToCreatedAt,
				}
			}

			matches = append(matches, responseMessage)
			if len(matches) >= limit {
				break
			}
		}

		// Stop on a full page of matches FIRST — if the limit filled midway
		// through this batch, the remaining rows were not evaluated yet, so
		// the history must NOT be marked exhausted (has_more stays true).
		if len(matches) >= limit {
			break
		}
		if len(rows) < searchBatchSize {
			exhausted = true
			break
		}
	}

	// next_before_ms — created_at of the LAST SCANNED row (ms). The client
	// resumes scanning from there whether or not this page found matches.
	nextBeforeMs := beforeMs
	if scanned > 0 {
		nextBeforeMs = cursor.UnixMilli()
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"messages":       matches,
			"has_more":       !exhausted,
			"next_before_ms": nextBeforeMs,
		},
	})
}

// SyncMessages — delta sinxronizasiya (reconnect gap-fill).
// GET /api/v1/messages/sync?since_ms=<int64>&limit=<int, default 200, max 500>
//
// Client (yeni iOS) reconnect-də son bildiyi server_time_ms ilə çağırır və
// qayıdan mesajları id-yə görə upsert edir. Köhnə client-lər bu endpoint-i
// heç vaxt çağırmır (tam additiv).
//
// Mexanizm — niyə `updated_at > since` hər dəyişikliyi tutur:
//   - yeni mesaj: INSERT-də updated_at = created_at yazılır;
//   - edit: EditMessage updated_at-ı açıq yeniləyir;
//   - read/delivered: GORM Model().Update updated_at-ı avtomatik yeniləyir;
//   - silinmə: DM-də HARD DELETE YOXDUR — DeleteMessage/ClearConversation
//     is_deleted_by_sender/receiver flag-larını qoyub updated_at-ı yeniləyir.
//     Ona görə mənim üçün silinmiş mesajlar da bu pəncərəyə düşür və
//     `deleted_message_ids` siyahısında qaytarılır. (messages.deleted_at DM
//     axınında heç vaxt yazılmır — yalnız qrup mesajı silinməsi istifadə edir;
//     yenə də qoruyucu olaraq deleted_at dolu sətir silinmiş sayılır.)
func (h *MessageHandler) SyncMessages(c *gin.Context) {
	userIDVal, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	userID := userIDVal.(uint)

	sinceMs, err := strconv.ParseInt(c.DefaultQuery("since_ms", "0"), 10, 64)
	if err != nil || sinceMs < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz since_ms"})
		return
	}

	limit, err := strconv.Atoi(c.DefaultQuery("limit", "200"))
	if err != nil || limit <= 0 {
		limit = 200
	}
	if limit > 500 {
		limit = 500
	}

	since := time.UnixMilli(sinceMs).UTC()

	// ── Issue 2: SKALER WATERMARK → (updated_at, id) KEYSET ─────────────────
	//
	// Köhnə sorğu `WHERE m.updated_at > ?` idi və watermark yalnız son sətrin
	// `updated_at`-ı olurdu. İKİ ayrı itki mexanizmi vardı:
	//
	//  1. EYNİ MİLLİSANİYƏDƏKİ BƏRABƏRLİK. `updated_at` millisaniyə
	//     dəqiqliyində eyni ola bilər. Səhifə sərhədi məhz bərabər dəyərlər
	//     arasına düşsə (`hasMore` budağı) watermark tam həmin dəyərə qoyulur
	//     və `> since` növbəti dəfə həmin millisaniyədəki QALAN sətirləri
	//     HƏMİŞƏLİK atır. `ORDER BY ... , m.id ASC` sıralamanı sabitləyirdi,
	//     amma imleç `id` daşımadığı üçün bu itkini QARŞILAMIRDI.
	//
	//  2. SIRA-DIŞI COMMIT. `updated_at` commit-dən ƏVVƏL ştamplanır. T anında
	//     ştamplanan uzun transaction, T+1-də ştamplananın SONRA commit oluna
	//     bilər. Watermark T+1-ə çatıbsa T sətri heç vaxt görünmür.
	//     Köhnə kod buna qarşı 5 saniyəlik geri-sarma qoymuşdu, AMMA yalnız
	//     SON səhifədə (`!hasMore`). Səhifələmə davam edərkən qoruma YOX idi —
	//     yəni məhz çox dəyişiklik olan (ən riskli) halda işləmirdi.
	//
	// İNDİ:
	//  • Sorğu həqiqi keyset: `(m.updated_at, m.id) > (?, ?)`. Bərabərlik itkisi
	//    tamamilə aradan qalxır.
	//  • Watermark HEÇ VAXT "təhlükə zonasına" (son `syncSafetyMs`) girmir —
	//    səhifələmə olsun-olmasın. Orada hələ commit olmamış transaction ola
	//    bilər, ona görə imleç zonanın BAŞINDA saxlanılır və növbəti sinxron
	//    o pəncərəni yenidən tarayır. İstemçi id ilə dedup etdiyi üçün təkrar
	//    sətirlər zərərsizdir və pəncərə yalnız 5 saniyəlikdir.
	//
	// Geriyə uyğunluq: `since_id` opsionaldır. Köhnə istemçi göndərməzsə sıfır
	// UUID işlədilir — davranış əvvəlki kimi (yalnız `updated_at` müqayisəsi).
	const zeroUUID = "00000000-0000-0000-0000-000000000000"
	sinceID := strings.TrimSpace(c.DefaultQuery("since_id", ""))
	if _, perr := uuid.Parse(sinceID); perr != nil {
		sinceID = zeroUUID
	}

	// GetMessages ilə eyni sütun dəsti + delete flag-ları (təsnifat üçün).
	var rows []struct {
		ID                   string     `gorm:"column:id"`
		SenderID             uint       `gorm:"column:sender_id"`
		ReceiverID           uint       `gorm:"column:receiver_id"`
		StoryID              *uint      `gorm:"column:story_id"`
		StoryMetadata        *string    `gorm:"column:story_metadata"`
		ReplyToMessageID     *string    `gorm:"column:reply_to_message_id"`
		EncryptedText        string     `gorm:"column:encrypted_text"`
		Read                 bool       `gorm:"column:read"`
		Delivered            bool       `gorm:"column:delivered"`
		IsEdited             bool       `gorm:"column:is_edited"`
		IsDeletedBySender    bool       `gorm:"column:is_deleted_by_sender"`
		IsDeletedByReceiver  bool       `gorm:"column:is_deleted_by_receiver"`
		DeletedAt            *time.Time `gorm:"column:deleted_at"`
		SenderReaction       *string    `gorm:"column:sender_reaction"`
		ReceiverReaction     *string    `gorm:"column:receiver_reaction"`
		StarredBySender      bool       `gorm:"column:starred_by_sender"`
		StarredByReceiver    bool       `gorm:"column:starred_by_receiver"`
		CreatedAt            time.Time  `gorm:"column:created_at"`
		UpdatedAt            time.Time  `gorm:"column:updated_at"`
		ReplyToMessageText   *string    `gorm:"column:reply_to_message_text"`
		ReplyToMessageSender *uint      `gorm:"column:reply_to_message_sender"`
		ReplyToCreatedAt     *time.Time `gorm:"column:reply_to_created_at"`
		StoryType            *string    `gorm:"column:story_type"`
		StoryMediaURL        *string    `gorm:"column:story_media_url"`
		StoryContent         *string    `gorm:"column:story_content"`
		StoryUserID          *uint      `gorm:"column:story_user_id"`
		StoryCreatedAt       *time.Time `gorm:"column:story_created_at"`
	}

	// conversation_id IS NULL → yalnız DM (qrup mesajları ayrı axındır).
	// LIMIT limit+1 → has_more hesablamaq üçün.
	query := `
        SELECT
            m.*,
            reply.encrypted_text as reply_to_message_text,
            reply.sender_id as reply_to_message_sender,
            reply.created_at as reply_to_created_at,
            s.type as story_type,
            s.media_url as story_media_url,
            s.content as story_content,
            s.media_metadata as story_metadata,
            s.user_id as story_user_id,
            s.created_at as story_created_at
        FROM messages m
        LEFT JOIN messages reply ON m.reply_to_message_id = reply.id
        LEFT JOIN stories s ON m.story_id = s.id
        WHERE (m.sender_id = ? OR m.receiver_id = ?)
        AND m.conversation_id IS NULL
        AND (m.updated_at, m.id) > (?, ?::uuid)
        ORDER BY m.updated_at ASC, m.id ASC
        LIMIT ?
    `

	if err := database.DB.Raw(query, userID, userID, since, sinceID, limit+1).Scan(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Mesajlar alınamadı"})
		return
	}

	hasMore := false
	if len(rows) > limit {
		hasMore = true
		rows = rows[:limit]
	}

	// Issue 2: TƏHLÜKƏSİZLİK PƏNCƏRƏSİ (yuxarıdakı geniş şərhə bax).
	// Watermark bu pəncərəyə HEÇ VAXT girmir — nə son səhifədə, nə səhifələmə
	// əsnasında. Beləliklə hələ commit olmamış transaction-lar növbəti
	// sinxronda mütləq tutulur.
	const syncSafetyMs int64 = 5000

	nextSinceMs := sinceMs
	nextSinceID := sinceID
	var lastRowUpdatedMs int64 = sinceMs
	lastRowID := sinceID
	hadRows := len(rows) > 0
	responseMessages := make([]gin.H, 0, len(rows))
	deletedMessageIDs := make([]string, 0)

	for _, msg := range rows {
		lastRowUpdatedMs = msg.UpdatedAt.UnixMilli()
		lastRowID = msg.ID

		// Mənim üçün silinmiş → mətn qaytarılmır, yalnız id (client silsin).
		deletedForMe := msg.DeletedAt != nil ||
			(msg.SenderID == userID && msg.IsDeletedBySender) ||
			(msg.SenderID != userID && msg.IsDeletedByReceiver)
		if deletedForMe {
			deletedMessageIDs = append(deletedMessageIDs, msg.ID)
			continue
		}

		decryptedText, err := h.encryptionService.DecryptMessage(msg.EncryptedText)
		if err != nil {
			decryptedText = "Mesaj çözülemedi"
		}

		responseMessage := gin.H{
			"id":                  msg.ID,
			"sender_id":           msg.SenderID,
			"receiver_id":         msg.ReceiverID,
			"story_id":            msg.StoryID,
			"reply_to_message_id": msg.ReplyToMessageID,
			"text":                decryptedText,
			"read":                msg.Read,
			"delivered":           msg.Delivered,
			"is_edited":           msg.IsEdited,
			"sender_reaction":     msg.SenderReaction,
			"receiver_reaction":   msg.ReceiverReaction,
			"is_starred_by_me":    starredByUser(userID, msg.SenderID, msg.StarredBySender, msg.StarredByReceiver),
			"created_at":          msg.CreatedAt,
			"updated_at":          msg.UpdatedAt,
		}

		if msg.StoryID != nil {
			if msg.StoryType != nil {
				storyResponse := gin.H{
					"id":         *msg.StoryID,
					"type":       *msg.StoryType,
					"media_url":  utils.PrependS3URL(msg.StoryMediaURL),
					"content":    msg.StoryContent,
					"user_id":    *msg.StoryUserID,
					"created_at": msg.StoryCreatedAt,
					"available":  true,
				}

				if *msg.StoryType == "video" && msg.StoryMetadata != nil {
					var metadata map[string]interface{}
					if err := json.Unmarshal([]byte(*msg.StoryMetadata), &metadata); err == nil {
						if thumbnailURL, exists := metadata["thumbnail_url"].(string); exists && thumbnailURL != "" {
							storyResponse["thumbnail_url"] = utils.PrependS3URL(&thumbnailURL)
						}
					}
				}

				responseMessage["story"] = storyResponse
			} else {
				responseMessage["story"] = gin.H{
					"id":        *msg.StoryID,
					"available": false,
					"message":   "Bu story artık mevcut değil",
				}
			}
		}

		if msg.ReplyToMessageID != nil && msg.ReplyToMessageText != nil {
			replyDecryptedText, err := h.encryptionService.DecryptMessage(*msg.ReplyToMessageText)
			if err != nil {
				replyDecryptedText = "Mesaj çözülemedi"
			}

			responseMessage["reply_to_message"] = gin.H{
				"id":         *msg.ReplyToMessageID,
				"sender_id":  msg.ReplyToMessageSender,
				"text":       replyDecryptedText,
				"created_at": msg.ReplyToCreatedAt,
			}
		}

		responseMessages = append(responseMessages, responseMessage)
	}

	// ── Watermark hesabı (Issue 2) ──────────────────────────────────────────
	//
	// İmleç `(updated_at, id)` cütüdür və "təhlükə zonasına" (son
	// `syncSafetyMs` millisaniyə) HEÇ VAXT girmir. İki hal:
	//
	//  • Son sətir zonadan KƏNARDADIR → imleci dəqiq ora qoy: `(ts, id)`.
	//    Bərabər `updated_at` dəyərləri id ilə ayrıldığı üçün heç nə itmir.
	//  • Son sətir zonanın İÇİNDƏDİR → imleci zonanın BAŞINA qoy və id-ni
	//    sıfırla. Növbəti sinxron həmin 5 saniyəni yenidən tarayır; istemçi
	//    id ilə dedup edir. Beləliklə gec commit olan sətir mütləq görünür.
	//
	// Sətir yoxdursa imleç dəyişmir (boş cavab irəli sürükləməməlidir).
	if hadRows {
		safeCeilingMs := time.Now().UTC().UnixMilli() - syncSafetyMs
		if lastRowUpdatedMs <= safeCeilingMs {
			nextSinceMs = lastRowUpdatedMs
			nextSinceID = lastRowID
		} else {
			if safeCeilingMs < 0 {
				safeCeilingMs = 0
			}
			// Zonanın başına çəkilirik — amma MÖVCUD imlecdən GERİ getmirik
			// (əks halda hər sinxron eyni sətirləri sonsuz təkrar edərdi).
			if safeCeilingMs > nextSinceMs {
				nextSinceMs = safeCeilingMs
				nextSinceID = zeroUUID
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"messages":            responseMessages,
			"deleted_message_ids": deletedMessageIDs,
			"server_time_ms":      time.Now().UTC().UnixMilli(),
			"has_more":            hasMore,
			"next_since_ms":       nextSinceMs,
			// Issue 2: keyset imlecinin ikinci hissəsi. Köhnə istemçilər bunu
			// oxumur və yalnız `next_since_ms` işlədir (əvvəlki davranış).
			"next_since_id": nextSinceID,
		},
	})
}

// GetMessages belirli kullanıcı ile mesajları getir
func (h *MessageHandler) GetMessagesOld(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	otherUserID, err := strconv.ParseUint(c.Param("user_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz kullanıcı ID"})
		return
	}

	// Sayfa parametreleri
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	offset := (page - 1) * limit

	var messages []models.Message

	// 🆕 Silinmiş mesajları filter et - user'a görə
	query := `
		SELECT * FROM messages 
		WHERE ((sender_id = ? AND receiver_id = ?) OR (sender_id = ? AND receiver_id = ?))
		AND (
			CASE 
				WHEN sender_id = ? THEN is_deleted_by_sender = false
				ELSE is_deleted_by_receiver = false
			END
		)
		ORDER BY created_at DESC 
		LIMIT ? OFFSET ?
	`

	err = database.DB.Raw(query,
		userID, otherUserID, otherUserID, userID, // mesaj filtri
		userID, // delete filtri üçün
		limit, offset,
	).Find(&messages).Error

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Mesajlar alınamadı"})
		return
	}

	// Mesajları çöz ve response'a hazırla
	var responseMessages []gin.H
	for _, msg := range messages {
		decryptedText, err := h.encryptionService.DecryptMessage(msg.EncryptedText)
		if err != nil {
			decryptedText = "Mesaj çözülemedi"
		}

		responseMessages = append(responseMessages, gin.H{
			"id":                  msg.ID,
			"sender_id":           msg.SenderID,
			"receiver_id":         msg.ReceiverID,
			"reply_to_message_id": msg.ReplyToMessageID,
			"text":                decryptedText,
			"read":                msg.Read,
			"sender_reaction":     msg.SenderReaction,
			"receiver_reaction":   msg.ReceiverReaction,
			"created_at":          msg.CreatedAt,
			"updated_at":          msg.UpdatedAt,
		})
	}

	// Okunmamış mesajları okundu olarak işaretle (sadece gelen mesajlar)
	go h.markReceivedMessagesAsRead(userID.(uint), uint(otherUserID))

	c.JSON(http.StatusOK, gin.H{
		"data":      responseMessages, // "messages" deyil "data" olacaq
		"page":      page,
		"limit":     limit,
		"total":     len(responseMessages),
		"is_online": h.wsHub.IsUserOnline(uint(otherUserID)),
	})
}

// markReceivedMessagesAsRead alınan mesajları okundu olarak işaretle
func (h *MessageHandler) markReceivedMessagesAsRead(currentUserID, otherUserID uint) {
	// WebSocket bildirişi üçün yalnız id + sender_id lazımdır — əvvəllər bütün
	// mesaj sətirləri tam çəkilirdi (encrypted_text daxil, lazımsız böyük).
	// İndi yalnız iki sütun seçirik. Davranış eyni: oxunmamışlar tapılır,
	// read=true edilir, hər biri üçün HandleMessageRead çağırılır.
	type unreadRow struct {
		ID       string `gorm:"column:id"`
		SenderID uint   `gorm:"column:sender_id"`
	}
	var unreadMessages []unreadRow

	// Karşı taraftan gelen okunmamış mesajları bul (yalnız lazımi sütunlar)
	//
	// Issue 19: bu SELECT LİMİTSİZ idi — aylarla açılmamış bir çatda on
	// minlərlə sətir bir anda yaddaşa çəkilirdi (və hər çat açılışında
	// təkrar). Üstəlik `is_deleted_by_receiver` süzgəci yox idi: alıcının
	// ÖZÜ ÜÇÜN sildiyi mesajlar da çəkilib göndərənə `message_read`
	// bildirişi doğururdu.
	//
	// LIMIT yalnız BİLDİRİŞ siyahısına aiddir; aşağıdakı UPDATE onsuz da
	// şərtə uyan BÜTÜN sətirləri oxundu edir — yəni heç bir mesaj oxunmamış
	// qalmır, sadəcə göndərənə göndərilən id massivi sərhədlənir.
	const unreadNotifyCap = 500
	err := database.DB.Model(&models.Message{}).
		Select("id, sender_id").
		Where("sender_id = ? AND receiver_id = ? AND read = false AND is_deleted_by_receiver = false",
			otherUserID, currentUserID).
		Order("created_at DESC").
		Limit(unreadNotifyCap).
		Scan(&unreadMessages).Error

	if err != nil {
		return
	}

	// Okundu olarak işaretle. Oxundu = çatdırıldı da (read implies delivered).
	database.DB.Model(&models.Message{}).Where(
		"sender_id = ? AND receiver_id = ? AND read = false",
		otherUserID, currentUserID,
	).Updates(map[string]interface{}{"read": true, "delivered": true})

	// Issue 19: TOPLU bildiriş. Əvvəl hər mesaj üçün ayrıca
	// `HandleMessageRead` çağırılırdı; onların HƏR BİRİ göndərənə bir
	// `message_read` event-i yollayır VƏ `SendUnreadCountUpdate` ilə tam bir
	// `COUNT(*)` işə salırdı. N oxunmamış mesajlı bir çatı açmaq = N event +
	// N eyni nəticəli COUNT sorğusu (25-bağlantılıq hovuzda paralel goroutine
	// olaraq). İndi: göndərən başına TEK event (message_ids massivi ilə —
	// qrup yolundakı `group_message_read` ilə eyni forma) və TEK unread
	// yeniləməsi.
	if len(unreadMessages) == 0 {
		return
	}
	bySender := make(map[uint][]string, 4)
	for _, msg := range unreadMessages {
		bySender[msg.SenderID] = append(bySender[msg.SenderID], msg.ID)
	}
	readAt := time.Now().UTC()
	for senderID, ids := range bySender {
		h.wsHub.SendToUser(senderID, "message_read", map[string]interface{}{
			// Köhnə istemçilər tək `message_id` gözləyir → geriyə uyğunluq
			// üçün ilk id-ni də göndəririk; yenilər `message_ids`-i oxuyur.
			"message_id":  ids[0],
			"message_ids": ids,
			"reader_id":   currentUserID,
			"read_at":     readAt,
		})
	}
	// Oxuyanın öz rozeti — bir dəfə (N dəfə deyil).
	h.wsHub.SendUnreadCountUpdate(currentUserID)
}

// MarkAsRead mesajı okundu olarak işaretle
func (h *MessageHandler) MarkAsRead(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	messageID := c.Param("message_id")

	var message models.Message
	err := database.DB.Where("id = ?", messageID).First(&message).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "Mesaj bulunamadı"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Veritabanı hatası"})
		}
		return
	}

	// Sadece alıcı mesajı okundu olarak işaretleyebilir
	if message.ReceiverID == nil || *message.ReceiverID != userID.(uint) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Bu mesajı okundu olarak işaretleme yetkiniz yok"})
		return
	}

	// Zaten okunmuşsa
	if message.Read {
		c.JSON(http.StatusOK, gin.H{"message": "Mesaj zaten okunmuş"})
		return
	}

	// Okundu olarak işaretle. Oxundu = çatdırıldı da (read implies delivered).
	message.Read = true
	message.Delivered = true
	message.UpdatedAt = time.Now().UTC()

	if err := database.DB.Save(&message).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Mesaj güncellenemedi"})
		return
	}

	// WebSocket üzerinden gönderene bildir
	h.wsHub.HandleMessageRead(message.ID, message.SenderID, userID.(uint))

	c.JSON(http.StatusOK, gin.H{
		"message": "Mesaj okundu olarak işaretlendi",
		"data": gin.H{
			"message_id": message.ID,
			"read":       message.Read,
			"read_at":    message.UpdatedAt,
		},
	})
}

// MarkConversationAsRead — A↔B söhbətində bütün okunmamış mesajları (where
// sender_id=other AND receiver_id=current AND read=false) toplu okundu et.
//
// İstifadə yeri: native (iOS/Android) inline reply (Quick Reply) — kullanıcı
// bildirimi aşağı çekib reply göndərdiyi anda qarşı tərəfin mesajlarını
// avtomatik okundu hesab edirik. Eyni zamanda chat-page açılışında batch
// işarələmə üçün də istifadə oluna bilər. WebSocket-dən aynı işi yapan
// `mark_read` event-i mevcuddur; bu HTTP wrapper-i WS bağlantısı yoxsa
// fallback edir.
func (h *MessageHandler) MarkConversationAsRead(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	readerID := userID.(uint)

	otherStr := c.Param("other_user_id")
	otherUint64, err := strconv.ParseUint(otherStr, 10, 64)
	if err != nil || otherUint64 == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz other_user_id"})
		return
	}
	otherID := uint(otherUint64)

	if otherID == readerID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "other_user_id self ola bilməz"})
		return
	}

	now := time.Now().UTC()
	// Oxundu = çatdırıldı da (read implies delivered) — ayrıca event lazım deyil.
	result := database.DB.Model(&models.Message{}).
		Where("sender_id = ? AND receiver_id = ? AND read = false", otherID, readerID).
		Updates(map[string]interface{}{
			"read":       true,
			"delivered":  true,
			"updated_at": now,
		})

	if result.Error != nil {
		log.Printf("MarkConversationAsRead DB error: %v", result.Error)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Veritabanı hatası"})
		return
	}

	updatedCount := result.RowsAffected

	// WebSocket üzərindən qarşı tərəfə (mesajları göndərənə) bildir
	// — UI tick'lərini "görüldü" olaraq dəyişdirmək üçün. Hub-da eyni
	// event format-ı (`message_read`) istifadə olunur.
	if updatedCount > 0 {
		readData := map[string]interface{}{
			"reader_id":     readerID,
			"other_user_id": otherID,
			"read_count":    updatedCount,
		}
		h.wsHub.SendToUser(otherID, "message_read", readData)
	}

	c.JSON(http.StatusOK, gin.H{
		"message":    "Conversation okundu olarak işaretlendi",
		"read_count": updatedCount,
		"read_at":    now,
	})
}

// GetConversations sohbet listesi
func (h *MessageHandler) GetConversations(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	// Soft-throttle: bad_traffic flag-lı user-in conversations siyahısı gecikir.
	throttleBadTraffic(int64(userID.(uint)))

	statusFilter := c.DefaultQuery("status", "all")
	// archived filtri: "false" (default) → yalnız arxivlənməmiş; "true" →
	// yalnız arxivlənmiş; "all" → hamısı. Per-user (cari istifadəçiyə görə).
	archivedFilter := c.DefaultQuery("archived", "false")

	var conversations []struct {
		OtherUserID     uint      `json:"other_user_id"`
		LastMessageID   string    `json:"last_message_id"`
		LastMessageText string    `json:"last_message_text"`
		LastMessageTime time.Time `json:"last_message_time"`
		IsLastFromMe    bool      `json:"is_last_from_me"`
		LastMessageRead *bool     `json:"last_message_read"`
		// ← YENİ (additiv): son mesaj məndəndirsə, çatdırılıb-çatdırılmadığı
		// (iki tick). Köhnə client-lər bu sahəni sadəcə görmür.
		LastMessageDelivered *bool  `json:"last_message_delivered"`
		UnreadCount          int    `json:"unread_count"`
		ConversationStatus   string `json:"conversation_status"`

		ConversationID     *uint      `json:"conversation_id"`
		MyMessageCount     *int       `json:"my_message_count"`
		OtherMessageCount  *int       `json:"other_message_count"`
		AmIMuted           *bool      `json:"am_i_muted"`
		AmIArchived        *bool      `json:"am_i_archived"`
		AmIPinned          *bool      `json:"am_i_pinned"`
		PinnedAt           *time.Time `json:"pinned_at"`
		MyNickname         *string    `json:"my_nickname"`
		MyWallpaperID      *uint      `json:"my_wallpaper_id"`
		AmIRestricted      *bool      `json:"am_i_restricted"`
		IsOtherMuted       *bool      `json:"is_other_muted"`
		IsOtherRestricted  *bool      `json:"is_other_restricted"`
		MaxPendingMessages *int       `json:"max_pending_messages"`
		Blocked            *bool      `json:"blocked"`

		OtherUserName       string `json:"other_user_name"`
		OtherUserUsername   string `json:"other_user_username"`
		OtherUserIsVerified bool   `json:"other_user_is_verified"`
		// YENİ (additiv): verified user-in seçilmiş/sahib olduğu special badge icon URL-i.
		// is_verified=false olduqda LATERAL boş qalır → null.
		OtherUserSpecialBadgeIconURL *string `json:"other_user_special_badge_icon_url" gorm:"column:other_user_special_badge_icon_url"`
		AccountTypeID                int     `json:"account_type_id"`
		ProfileImage                 *string `json:"profile_image"`

		AllowVoiceMessages bool `json:"allow_voice_messages"`
		ShowReadReceipts   bool `json:"show_read_receipts"`

		// ✅ YENİ: son reaksiya
		LastReactionEmoji    *string    `json:"last_reaction_emoji"`
		LastReactionAt       *time.Time `json:"last_reaction_at"`
		LastReactionByUserID *uint      `json:"last_reaction_by_user_id"`
	}

	statusWhereClause := ""
	var extraParams []interface{}

	if statusFilter == "pending" {
		statusWhereClause = `
        AND COALESCE(conv.status, 'active') = 'pending'
        AND CASE 
            WHEN conv.user1_id = ? THEN conv.user2_message_count > 0 
            ELSE conv.user1_message_count > 0 
        END
    `
		extraParams = append(extraParams, userID)
	} else if statusFilter == "active" {
		statusWhereClause = `
        AND (
            COALESCE(conv.status, 'active') = 'active'
            OR (
                COALESCE(conv.status, 'active') = 'pending'
                AND CASE 
                    WHEN conv.user1_id = ? THEN conv.user1_message_count > 0
                    ELSE conv.user2_message_count > 0
                END
            )
        )
    `
		extraParams = append(extraParams, userID)
	} else if statusFilter == "restricted" {
		statusWhereClause = "AND COALESCE(conv.status, 'active') = 'restricted'"
	}

	// Arxiv filtri (per-user). Default: arxivlənmişləri GİZLƏT.
	//   archived=false → yalnız arxivlənməmiş (əsas siyahı)
	//   archived=true  → yalnız arxivlənmiş (arxiv səhifəsi)
	//   archived=all   → filtr yox
	// QEYD: Bu WHERE statusWhereClause-dan SONRA query-yə əlavə olunur, ona görə
	// parametrləri də extraParams-a status parametrindən SONRA əlavə edirik.
	archivedWhereClause := ""
	if archivedFilter == "true" {
		archivedWhereClause = `
        AND CASE
            WHEN conv.user1_id = ? THEN conv.user1_archived
            WHEN conv.user2_id = ? THEN conv.user2_archived
            ELSE FALSE
        END = TRUE
    `
		extraParams = append(extraParams, userID, userID)
	} else if archivedFilter != "all" {
		// default "false": arxivlənmişləri çıxar
		archivedWhereClause = `
        AND COALESCE(CASE
            WHEN conv.user1_id = ? THEN conv.user1_archived
            WHEN conv.user2_id = ? THEN conv.user2_archived
            ELSE FALSE
        END, FALSE) = FALSE
    `
		extraParams = append(extraParams, userID, userID)
	}

	query := `
    WITH latest_messages AS (
        SELECT 
            CASE 
                WHEN sender_id = ? THEN receiver_id 
                ELSE sender_id 
            END as other_user_id,
            id,
            encrypted_text,
            created_at,
            sender_id = ? as is_from_me,
            read,
            delivered,
            ROW_NUMBER() OVER (
                PARTITION BY CASE WHEN sender_id = ? THEN receiver_id ELSE sender_id END 
                ORDER BY created_at DESC
            ) as rn
        FROM messages
        WHERE (sender_id = ? OR receiver_id = ?)
        AND conversation_id IS NULL
        AND (
            CASE
                WHEN sender_id = ? THEN is_deleted_by_sender = false
                ELSE is_deleted_by_receiver = false
            END
        )
    ),
    unread_counts AS (
        SELECT
            sender_id as other_user_id,
            COUNT(*) as unread_count
        FROM messages
        WHERE receiver_id = ? AND read = false
        AND is_deleted_by_receiver = false
        AND conversation_id IS NULL
        GROUP BY sender_id
    )
    SELECT 
        lm.other_user_id,
        lm.id as last_message_id,
        lm.encrypted_text as last_message_text,
        lm.created_at as last_message_time,
        lm.is_from_me,
        CASE
            WHEN lm.is_from_me = true THEN lm.read
            ELSE NULL
        END as last_message_read,
        CASE
            WHEN lm.is_from_me = true THEN lm.delivered
            ELSE NULL
        END as last_message_delivered,
        COALESCE(uc.unread_count, 0) as unread_count,
        COALESCE(conv.status, 'active') as conversation_status,
        conv.id as conversation_id,
        CASE 
            WHEN conv.user1_id = ? THEN conv.user1_message_count
            WHEN conv.user2_id = ? THEN conv.user2_message_count
            ELSE NULL
        END as my_message_count,
        CASE 
            WHEN conv.user1_id = ? THEN conv.user2_message_count
            WHEN conv.user2_id = ? THEN conv.user1_message_count
            ELSE NULL
        END as other_message_count,
        CASE
            WHEN conv.user1_id = ? THEN conv.user1_muted
            WHEN conv.user2_id = ? THEN conv.user2_muted
            ELSE NULL
        END as am_i_muted,
        CASE
            WHEN conv.user1_id = ? THEN conv.user1_archived
            WHEN conv.user2_id = ? THEN conv.user2_archived
            ELSE FALSE
        END as am_i_archived,
        CASE
            WHEN conv.user1_id = ? THEN (conv.user1_pinned_at IS NOT NULL)
            WHEN conv.user2_id = ? THEN (conv.user2_pinned_at IS NOT NULL)
            ELSE FALSE
        END as am_i_pinned,
        CASE
            WHEN conv.user1_id = ? THEN conv.user1_pinned_at
            WHEN conv.user2_id = ? THEN conv.user2_pinned_at
            ELSE NULL
        END as pinned_at,
        CASE
            WHEN conv.user1_id = ? THEN conv.user1_nickname
            WHEN conv.user2_id = ? THEN conv.user2_nickname
            ELSE NULL
        END as my_nickname,
        CASE
            WHEN conv.user1_id = ? THEN conv.user1_wallpaper_id
            WHEN conv.user2_id = ? THEN conv.user2_wallpaper_id
            ELSE NULL
        END as my_wallpaper_id,
        CASE
            WHEN conv.user1_id = ? THEN conv.user1_restricted
            WHEN conv.user2_id = ? THEN conv.user2_restricted
            ELSE NULL
        END as am_i_restricted,
        CASE 
            WHEN conv.user1_id = ? THEN conv.user2_muted
            WHEN conv.user2_id = ? THEN conv.user1_muted
            ELSE NULL
        END as is_other_muted,
        CASE 
            WHEN conv.user1_id = ? THEN conv.user2_restricted
            WHEN conv.user2_id = ? THEN conv.user1_restricted
            ELSE NULL
        END as is_other_restricted,
        conv.max_pending_messages,
        conv.blocked,
        u.name as other_user_name,
        u.username as other_user_username,
        u.account_type_id,
        u.is_verified as other_user_is_verified,
        other_badge.icon_url as other_user_special_badge_icon_url,
        p.profile_image,
        COALESCE(us.allow_voice_messages, true) as allow_voice_messages,
        COALESCE(us.show_read_receipts, true) as show_read_receipts,
        conv.last_reaction_emoji,
        conv.last_reaction_at,
        conv.last_reaction_by_user_id
    FROM latest_messages lm
    LEFT JOIN unread_counts uc ON lm.other_user_id = uc.other_user_id
    LEFT JOIN users u ON u.id = lm.other_user_id
    LEFT JOIN LATERAL (
        SELECT b.icon_url
        FROM badges b
        WHERE b.is_special
          AND b.id = u.selected_badge_id
        ORDER BY b.priority DESC
        LIMIT 1
    ) other_badge ON u.is_verified = true
    LEFT JOIN profiles p ON p.user_id = lm.other_user_id
    LEFT JOIN user_settings us ON us.user_id = lm.other_user_id
    LEFT JOIN conversations conv ON (
        (conv.user1_id = LEAST(?, lm.other_user_id) AND conv.user2_id = GREATEST(?, lm.other_user_id))
    )
    WHERE lm.rn = 1 ` + statusWhereClause + archivedWhereClause + `
    ORDER BY
        CASE
            WHEN conv.user1_id = ? THEN (conv.user1_pinned_at IS NOT NULL)
            WHEN conv.user2_id = ? THEN (conv.user2_pinned_at IS NOT NULL)
            ELSE FALSE
        END DESC,
        CASE
            WHEN conv.user1_id = ? THEN conv.user1_pinned_at
            WHEN conv.user2_id = ? THEN conv.user2_pinned_at
            ELSE NULL
        END DESC NULLS LAST,
        lm.created_at DESC
    `

	// Parametr sırası query-dəki ? ardıcıllığı ilə DƏQİQ uyğundur:
	//  CTE: other_user_id CASE, is_from_me, PARTITION, WHERE sender, WHERE recv,
	//       is_deleted CASE  → 6
	//  unread_counts WHERE receiver  → 1 (cəmi yuxarıda 5+1 kimi yazılıb)
	//  SELECT CASE-lər: my_count(2), other_count(2), am_i_muted(2),
	//       am_i_archived(2), am_i_pinned(2) ← YENİ, pinned_at(2) ← YENİ,
	//       am_i_restricted(2), is_other_muted(2), is_other_restricted(2) = 18
	//  SELECT CASE-lər: my_count(2), other_count(2), am_i_muted(2),
	//       am_i_archived(2), am_i_pinned(2), pinned_at(2), my_nickname(2),
	//       my_wallpaper_id(2) ← YENİ, am_i_restricted(2), is_other_muted(2),
	//       is_other_restricted(2) = 22
	//  CTE(6)+unread(1)+SELECT(22)+JOIN(2) = 31 static
	//  (sonra: extraParams = status+archived WHERE)
	//  ORDER BY: pin CASE(2) + pinned_at CASE(2) = 4
	params := []interface{}{
		userID, userID, userID, userID, userID,
		userID,
		userID,
		userID, userID, userID, userID, userID, userID, userID, userID, userID, userID, userID, userID, userID, userID, userID, userID, userID, userID, userID, userID, userID, userID,
		userID, userID,
	}
	params = append(params, extraParams...)
	// ORDER BY parametrləri (WHERE/extraParams-dan SONRA query mətnində gəlir).
	params = append(params, userID, userID, userID, userID)

	err := database.DB.Raw(query, params...).Scan(&conversations).Error
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Konuşmalar alınamadı"})
		return
	}

	// 🔧 N+1 DÜZƏLİŞİ: əvvəllər döngü içində hər söhbət üçün ayrıca
	// models.IsBlocked(...) çağırılırdı — 50 söhbət = 50 ayrı DB sorğusu.
	// DB uzaq host-da olduğu üçün (hər sorğu ~100ms RTT) bu tək başına
	// saniyələrlə gecikmə yaradırdı. İndi bütün block əlaqələrini BİR sorğuda
	// çəkib map-ə qoyuruq; döngüdə yoxlama yaddaşda (O(1)) edilir.
	blockedUserIDs := models.GetBlockedUserIDs(database.DB, userID.(uint))

	var responseConversations []gin.H
	for _, conv := range conversations {
		decryptedText, err := h.encryptionService.DecryptMessage(conv.LastMessageText)
		if err != nil {
			decryptedText = "Mesaj çözülemedi"
		}

		canSendMessage := true
		conversationActive := true
		conversationType := "normal"

		if conv.ConversationStatus != "" && conv.ConversationStatus != "active" {
			conversationActive = false

			switch conv.ConversationStatus {
			case "pending":
				conversationType = "pending"
				if conv.MyMessageCount != nil && conv.MaxPendingMessages != nil {
					if *conv.MyMessageCount >= *conv.MaxPendingMessages {
						canSendMessage = false
					}
				}
			case "restricted":
				conversationType = "restricted"
				canSendMessage = false
			}
		}

		if conv.AmIRestricted != nil && *conv.AmIRestricted {
			canSendMessage = false
		}

		// Admin (Filament) blok — söhbət bağlıdırsa heç kim göndərə bilməz.
		isBlocked := conv.Blocked != nil && *conv.Blocked
		if isBlocked {
			canSendMessage = false
		}

		isMutedByMe := conv.AmIMuted != nil && *conv.AmIMuted
		isArchivedByMe := conv.AmIArchived != nil && *conv.AmIArchived
		// Pin = pinned_at dolu (NULL deyil). AmIPinned (*bool) bəzi hallarda
		// GORM Raw scan-da nil qaldığı üçün birbaşa PinnedAt-dan hesablayırıq —
		// PinnedAt həmişə düzgün scan olunur (dolu/NULL).
		isPinnedByMe := conv.PinnedAt != nil

		// Əgər tərəflərdən biri digərini bloklayıbsa, online statusu göstərilməməlidir.
		// N+1 DÜZƏLİŞİ: DB sorğusu yox — yuxarıda bir dəfə çəkilmiş map-dən yoxlanır.
		isOnline := false
		if !blockedUserIDs[conv.OtherUserID] {
			isOnline = h.wsHub.IsUserOnline(conv.OtherUserID)
		}

		responseData := gin.H{
			"other_user_id":                     conv.OtherUserID,
			"other_user_name":                   conv.OtherUserName,
			"other_user_username":               conv.OtherUserUsername,
			"other_user_is_verified":            conv.OtherUserIsVerified,
			"other_user_special_badge_icon_url": conv.OtherUserSpecialBadgeIconURL,
			"account_type_id":                   conv.AccountTypeID,
			"last_reaction_emoji":               conv.LastReactionEmoji,
			"last_reaction_at":                  conv.LastReactionAt,
			"last_reaction_by_user_id":          conv.LastReactionByUserID,
			"profile_image":                     utils.PrependBaseURL(conv.ProfileImage),
			"last_message_id":                   conv.LastMessageID,
			"last_message_text":                 decryptedText,
			"last_message_time":                 conv.LastMessageTime,
			"is_last_from_me":                   conv.IsLastFromMe,
			"last_message_read":                 conv.LastMessageRead,
			"last_message_delivered":            conv.LastMessageDelivered,
			"unread_count":                      conv.UnreadCount,
			"is_online":                         isOnline,
			"conversation_active":               conversationActive,
			"is_archived_by_me":                 isArchivedByMe,
			"is_pinned_by_me":                   isPinnedByMe,
			"pinned_at":                         conv.PinnedAt,
			"my_nickname":                       conv.MyNickname,
			"my_wallpaper_id":                   conv.MyWallpaperID,
			"conversation": gin.H{
				"id":                   conv.ConversationID,
				"status":               conv.ConversationStatus,
				"type":                 conversationType,
				"can_send_message":     canSendMessage,
				"is_muted_by_me":       isMutedByMe,
				"is_archived_by_me":    isArchivedByMe,
				"is_pinned_by_me":      isPinnedByMe,
				"my_nickname":          conv.MyNickname,
				"my_wallpaper_id":      conv.MyWallpaperID,
				"am_i_restricted":      conv.AmIRestricted,
				"is_other_muted":       conv.IsOtherMuted,
				"is_other_restricted":  conv.IsOtherRestricted,
				"my_message_count":     conv.MyMessageCount,
				"other_message_count":  conv.OtherMessageCount,
				"max_pending_messages": conv.MaxPendingMessages,
				"allow_voice_messages": conv.AllowVoiceMessages,
				"show_read_receipts":   conv.ShowReadReceipts,
				"blocked":              isBlocked,
			},
		}

		responseConversations = append(responseConversations, responseData)
	}

	c.JSON(http.StatusOK, gin.H{
		"conversations": responseConversations,
		"total":         len(responseConversations),
		"status_filter": statusFilter,
	})
}

// GetUnreadCount okunmamış mesaj sayısı
func (h *MessageHandler) GetUnreadCount(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	var count int64
	err := database.DB.Model(&models.Message{}).
		Joins("JOIN users ON users.id = messages.sender_id").
		// Issue 59: `conversation_id IS NULL` — yalnız DM. Siyahıdakı
		// `unread_counts` CTE-si bu şərti tətbiq edir, bu sayğac isə etmirdi →
		// rozet ilə sətirlərin cəmi bir-birini tutmurdu.
		Where("messages.receiver_id = ? AND messages.read = false AND messages.is_deleted_by_receiver = false AND messages.conversation_id IS NULL AND users.deleted_at IS NULL", userID).
		Count(&count).Error

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Sayım yapılamadı"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"unread_count": count,
	})
}

// DeleteMessage mesajı sil (yalnız özündən və ya hər iki tərəfdən)
func (h *MessageHandler) DeleteMessage(c *gin.Context) {
	userIDVal, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	userID := userIDVal.(uint)

	messageID := c.Param("message_id")

	var body struct {
		DeleteType string `json:"delete_type" binding:"required"` // "me" və ya "both"
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "delete_type: 'me' və ya 'both' olmalıdır"})
		return
	}

	var message models.Message
	err := database.DB.Where("id = ?", messageID).First(&message).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Mesaj tapılmadı"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Veritabanı xətası"})
		}
		return
	}

	now := time.Now().UTC()

	// Silmə növünə görə işləmə
	switch body.DeleteType {
	case "me":
		if userID == message.SenderID {
			message.IsDeletedBySender = true
		} else if message.ReceiverID != nil && userID == *message.ReceiverID {
			message.IsDeletedByReceiver = true
		} else {
			c.JSON(http.StatusForbidden, gin.H{"error": "Bu mesajı silmək icazən yoxdur"})
			return
		}

	case "both":
		// Yalnız göndərən hər iki tərəfdən silə bilər
		if userID != message.SenderID {
			c.JSON(http.StatusForbidden, gin.H{"error": "Yalnız göndərən hər iki tərəfdən silə bilər"})
			return
		}
		message.IsDeletedBySender = true
		message.IsDeletedByReceiver = true

	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçərsiz delete_type. 'me' və ya 'both' olmalıdır"})
		return
	}

	message.UpdatedAt = now
	if err := database.DB.Save(&message).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Silinmə uğursuz oldu"})
		return
	}

	// 🔔 WebSocket bildirimi
	deletePayload := map[string]interface{}{
		"message_id":  message.ID,
		"deleted_by":  userID,
		"delete_type": body.DeleteType,
		"deleted_at":  now,
	}

	// Issue 11 (vacib): `delete_type == "me"` YALNIZ silən istifadəçini
	// maraqlandırır — sətir qarşı tərəf üçün hələ də görünür və
	// `GetMessages`-də qayıdır. Əvvəllər bu event HƏR İKİ tərəfə göndərilirdi;
	// istemçi tərəfdə silmə artıq KALICI keş "mezar daşı"na yazıldığı üçün bu,
	// qarşı tərəfin mesajı HƏMİŞƏLİK itirməsi demək olardı (istifadəçi heç nə
	// silmədiyi halda). İndi "me" yalnız silənə (öz digər cihazlarına), "both"
	// isə hər ikisinə gedir.
	h.wsHub.SendToUser(userID, "message_deleted", deletePayload)
	if body.DeleteType == "both" {
		if userID != message.SenderID {
			h.wsHub.SendToUser(message.SenderID, "message_deleted", deletePayload)
		}
		if message.ReceiverID != nil && *message.ReceiverID != userID {
			h.wsHub.SendToUser(*message.ReceiverID, "message_deleted", deletePayload)
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "Mesaj silindi",
		"data":    deletePayload,
	})
}

// ToggleStar — mesajı ulduzla/ulduzdan çıxar (per-user, toggle).
// POST /api/v1/messages/:message_id/star
func (h *MessageHandler) ToggleStar(c *gin.Context) {
	userIDVal, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	userID := userIDVal.(uint)

	messageID := c.Param("message_id")

	var message models.Message
	if err := database.DB.Where("id = ?", messageID).First(&message).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Mesaj tapılmadı"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Veritabanı xətası"})
		}
		return
	}

	// Yalnız söhbətin tərəfi ulduzlaya bilər.
	isSender := userID == message.SenderID
	isReceiver := message.ReceiverID != nil && userID == *message.ReceiverID
	if !isSender && !isReceiver {
		c.JSON(http.StatusForbidden, gin.H{"error": "İcazən yoxdur"})
		return
	}

	// Toggle per-user.
	var newVal bool
	if isSender {
		message.StarredBySender = !message.StarredBySender
		newVal = message.StarredBySender
	} else {
		message.StarredByReceiver = !message.StarredByReceiver
		newVal = message.StarredByReceiver
	}
	message.UpdatedAt = time.Now().UTC()

	if err := database.DB.Save(&message).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Ulduzlama uğursuz oldu"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message":    "OK",
		"message_id": message.ID,
		"is_starred": newVal,
	})
}

// GetStarredMessages — bu söhbətdə MƏNİM ulduzladığım mesajlar.
// GET /api/v1/conversations/:other_user_id/starred
//
// KRİTİK fallback: mesaj göstərilir yalnız əgər (1) mən ulduzlamışam,
// (2) mən silməmişəm, VƏ (3) GÖNDƏRƏN onu silməyib (mesajın sahibi silsə,
// ulduzlayan da görməməlidir). Şəkil/sender info da qaytarılır.
func (h *MessageHandler) GetStarredMessages(c *gin.Context) {
	userIDVal, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	userID := userIDVal.(uint)

	otherUserID, err := strconv.ParseUint(c.Param("other_user_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz kullanıcı ID"})
		return
	}

	var rows []struct {
		ID                 string    `gorm:"column:id"`
		SenderID           uint      `gorm:"column:sender_id"`
		ReceiverID         uint      `gorm:"column:receiver_id"`
		EncryptedText      string    `gorm:"column:encrypted_text"`
		IsEdited           bool      `gorm:"column:is_edited"`
		CreatedAt          time.Time `gorm:"column:created_at"`
		SenderName         string    `gorm:"column:sender_name"`
		SenderUsername     string    `gorm:"column:sender_username"`
		SenderProfileImage *string   `gorm:"column:sender_profile_image"`
	}

	// Per-user ulduz + per-user silmə + GÖNDƏRƏN silməyib.
	query := `
		SELECT
			m.id, m.sender_id, m.receiver_id, m.encrypted_text, m.is_edited, m.created_at,
			u.name as sender_name, u.username as sender_username, p.profile_image as sender_profile_image
		FROM messages m
		LEFT JOIN users u ON u.id = m.sender_id
		LEFT JOIN profiles p ON p.user_id = m.sender_id
		WHERE ((m.sender_id = ? AND m.receiver_id = ?) OR (m.sender_id = ? AND m.receiver_id = ?))
		  AND (
		    CASE WHEN m.sender_id = ? THEN m.starred_by_sender ELSE m.starred_by_receiver END
		  ) = TRUE
		  AND (
		    CASE WHEN m.sender_id = ? THEN m.is_deleted_by_sender ELSE m.is_deleted_by_receiver END
		  ) = FALSE
		  AND m.is_deleted_by_sender = FALSE
		  AND m.deleted_at IS NULL
		ORDER BY m.created_at DESC
	`

	if err := database.DB.Raw(query,
		userID, otherUserID, otherUserID, userID,
		userID, // ulduz CASE
		userID, // mənim silmə CASE
	).Scan(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Ulduzlu mesajlar alınamadı"})
		return
	}

	var result []gin.H
	for _, r := range rows {
		decrypted, derr := h.encryptionService.DecryptMessage(r.EncryptedText)
		if derr != nil {
			decrypted = "Mesaj çözülemedi"
		}
		result = append(result, gin.H{
			"id":                   r.ID,
			"sender_id":            r.SenderID,
			"receiver_id":          r.ReceiverID,
			"text":                 decrypted,
			"is_edited":            r.IsEdited,
			"is_starred_by_me":     true,
			"created_at":           r.CreatedAt,
			"sender_name":          r.SenderName,
			"sender_username":      r.SenderUsername,
			"sender_profile_image": utils.PrependBaseURL(r.SenderProfileImage),
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"messages": result,
		"total":    len(result),
	})
}

// DELETE /api/v1/conversations/:other_user_id/clear  body: { "delete_type": "me" | "both" }
func (h *MessageHandler) ClearConversation(c *gin.Context) {
	u, ok := c.Get("user_id")
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	currentUserID := u.(uint)

	otherStr := c.Param("other_user_id")
	otherU64, err := strconv.ParseUint(otherStr, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz user_id"})
		return
	}
	otherUserID := uint(otherU64)

	var body struct {
		DeleteType string `json:"delete_type" binding:"required"` // "me" | "both"
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "delete_type 'me' veya 'both' olmalıdır"})
		return
	}

	now := time.Now().UTC()
	tx := database.DB.Begin()
	if tx.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Transaction açılamadı"})
		return
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	var sentRes, recvRes int64

	switch body.DeleteType {
	case "me":
		// Benim GÖNDERDİKLERİM → sender tarafında gizle
		r1 := tx.Model(&models.Message{}).
			Where("sender_id = ? AND receiver_id = ? AND is_deleted_by_sender = FALSE", currentUserID, otherUserID).
			Updates(map[string]interface{}{"is_deleted_by_sender": true, "updated_at": now})
		sentRes = r1.RowsAffected
		if r1.Error != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Gönderdiğin mesajlar gizlenemedi"})
			return
		}

		// Benim ALDIKLARIM → receiver tarafında gizle
		r2 := tx.Model(&models.Message{}).
			Where("receiver_id = ? AND sender_id = ? AND is_deleted_by_receiver = FALSE", currentUserID, otherUserID).
			Updates(map[string]interface{}{"is_deleted_by_receiver": true, "updated_at": now})
		recvRes = r2.RowsAffected
		if r2.Error != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Aldığın mesajlar gizlenemedi"})
			return
		}

	case "both":
		// SADECE BENİM GÖNDERDİKLERİM → iki taraf için de gizle
		r1 := tx.Model(&models.Message{}).
			Where("sender_id = ? AND receiver_id = ? AND (is_deleted_by_sender = FALSE OR is_deleted_by_receiver = FALSE)", currentUserID, otherUserID).
			Updates(map[string]interface{}{"is_deleted_by_sender": true, "is_deleted_by_receiver": true, "updated_at": now})
		sentRes = r1.RowsAffected
		if r1.Error != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "İki taraftan gizleme (senin gönderdiklerin) başarısız"})
			return
		}

		// Karşı tarafın GÖNDERDİKLERİ → yalnızca benim tarafımda gizle (etik/izin gereği)
		r2 := tx.Model(&models.Message{}).
			Where("receiver_id = ? AND sender_id = ? AND is_deleted_by_receiver = FALSE", currentUserID, otherUserID).
			Updates(map[string]interface{}{"is_deleted_by_receiver": true, "updated_at": now})
		recvRes = r2.RowsAffected
		if r2.Error != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Karşıdan gelenleri gizleme başarısız"})
			return
		}

	default:
		tx.Rollback()
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz delete_type"})
		return
	}

	if err := tx.Commit().Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Commit başarısız"})
		return
	}

	payload := gin.H{
		"cleared_by":    currentUserID,
		"other_user_id": otherUserID,
		"delete_type":   body.DeleteType,
		"cleared_at":    now,
		"affected_sent": sentRes,
		"affected_recv": recvRes,
		"scope":         "conversation",
	}
	// UI’nin senkron olması için event
	h.wsHub.SendToUser(currentUserID, "conversation_cleared", payload)
	h.wsHub.SendToUser(otherUserID, "peer_conversation_cleared", payload)

	c.JSON(http.StatusOK, gin.H{
		"message": "Konuşma temizlendi",
		"data":    payload,
	})
}

// DELETE /api/v1/conversations/clear-all  body: { "delete_type": "me" | "both" }
func (h *MessageHandler) ClearAllMyMessages(c *gin.Context) {
	u, ok := c.Get("user_id")
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	currentUserID := u.(uint)

	var body struct {
		DeleteType string `json:"delete_type" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "delete_type 'me' veya 'both' olmalıdır"})
		return
	}

	now := time.Now().UTC()
	tx := database.DB.Begin()
	if tx.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Transaction açılamadı"})
		return
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	var sentRes, recvRes int64

	switch body.DeleteType {
	case "me":
		r1 := tx.Model(&models.Message{}).
			Where("sender_id = ? AND is_deleted_by_sender = FALSE", currentUserID).
			Updates(map[string]interface{}{"is_deleted_by_sender": true, "updated_at": now})
		sentRes = r1.RowsAffected
		if r1.Error != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Gönderdiğin mesajlar gizlenemedi"})
			return
		}

		r2 := tx.Model(&models.Message{}).
			Where("receiver_id = ? AND is_deleted_by_receiver = FALSE", currentUserID).
			Updates(map[string]interface{}{"is_deleted_by_receiver": true, "updated_at": now})
		recvRes = r2.RowsAffected
		if r2.Error != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Aldığın mesajlar gizlenemedi"})
			return
		}

	case "both":
		// Sadece benim GÖNDERDİKLERİM iki taraftan gizlenir
		r1 := tx.Model(&models.Message{}).
			Where("sender_id = ? AND (is_deleted_by_sender = FALSE OR is_deleted_by_receiver = FALSE)", currentUserID).
			Updates(map[string]interface{}{"is_deleted_by_sender": true, "is_deleted_by_receiver": true, "updated_at": now})
		sentRes = r1.RowsAffected
		if r1.Error != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "İki taraf için gizleme (gönderdiğin) başarısız"})
			return
		}

		// Aldıkların benim tarafımda gizlenir
		r2 := tx.Model(&models.Message{}).
			Where("receiver_id = ? AND is_deleted_by_receiver = FALSE", currentUserID).
			Updates(map[string]interface{}{"is_deleted_by_receiver": true, "updated_at": now})
		recvRes = r2.RowsAffected
		if r2.Error != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Aldığın mesajlar gizlenemedi"})
			return
		}

	default:
		tx.Rollback()
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz delete_type"})
		return
	}

	if err := tx.Commit().Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Commit başarısız"})
		return
	}

	payload := gin.H{
		"cleared_by":    currentUserID,
		"delete_type":   body.DeleteType,
		"cleared_at":    now,
		"affected_sent": sentRes,
		"affected_recv": recvRes,
		"scope":         "all",
	}
	h.wsHub.SendToUser(currentUserID, "all_messages_cleared", payload)

	c.JSON(http.StatusOK, gin.H{
		"message": "Tüm mesaj geçmişin temizlendi",
		"data":    payload,
	})

}

func (h *MessageHandler) EditMessage(c *gin.Context) {
	userIDVal, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	userID := userIDVal.(uint)

	messageID := c.Param("message_id")

	var body struct {
		Text string `json:"text" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var message models.Message
	if err := database.DB.Where("id = ?", messageID).First(&message).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Mesaj tapılmadı"})
		return
	}

	// Yalnız göndərən edit edə bilər
	if message.SenderID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "Yalnız öz mesajını edit edə bilərsən"})
		return
	}

	// Yalnız text tipli mesajlar edit edilə bilər
	//if message.Type != nil && *message.Type != "text" && *message.Type != "" {
	//	c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "Yalnız text mesajlar edit edilə bilər"})
	//	return
	//}

	encryptedText, err := h.encryptionService.EncryptMessage(body.Text)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Şifrələmə xətası"})
		return
	}

	now := time.Now().UTC()
	if err := database.DB.Model(&message).Updates(map[string]interface{}{
		"encrypted_text": encryptedText,
		"is_edited":      true,
		"updated_at":     now,
	}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Mesaj yenilənə bilmədi"})
		return
	}

	// WebSocket ilə hər iki tərəfə bildir
	editPayload := map[string]interface{}{
		"message_id": messageID,
		"text":       body.Text,
		"is_edited":  true,
		"edited_at":  now,
	}

	h.wsHub.SendToUser(message.SenderID, "message_edited", editPayload)
	if message.ReceiverID != nil {
		h.wsHub.SendToUser(*message.ReceiverID, "message_edited", editPayload)
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "Mesaj yeniləndi",
		"data":    editPayload,
	})
}

// MarkViewOnceOpened — ① "bir dəfə bax" media AÇILDI (DM mesajı).
// POST /api/v1/messages/:message_id/view-once-opened
//
// Axın:
//  1. Mesaj decrypt edilir, JSON-da `view_once: true` yoxlanır.
//  2. Çağıran user `view_once_opened_by` massivinə əlavə edilir (idempotent —
//     artıq varsa 200 + already_opened qaytarılır, təkrar yazılmır).
//  3. Yenilənmiş JSON yenidən şifrələnib saxlanır.
//  4. Hər iki tərəfə MÖVCUD `message_edited` WS event-i göndərilir — client-lər
//     onsuz da bu event-də mesaj mətnini yeniləyib cache-ə yazır, beləliklə
//     "Opened" statusu real-time sinxronlaşır və fetch-də qalıcı olur.
//
// Yalnız DM iştirakçıları (sender VƏ YA receiver) aça bilər. Göndərən öz
// göndərdiyinə də bir dəfə baxa bilir (tələbə uyğun).
func (h *MessageHandler) MarkViewOnceOpened(c *gin.Context) {
	userIDVal, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	userID := userIDVal.(uint)
	messageID := c.Param("message_id")

	var message models.Message
	if err := database.DB.Where("id = ?", messageID).First(&message).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Mesaj tapılmadı"})
		return
	}

	// Yalnız DM mesajı (qrup üçün ayrı endpoint — üzvlük yoxlanışı fərqlidir).
	if message.ConversationID != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Qrup mesajı üçün qrup endpoint-indən istifadə edin"})
		return
	}

	// Yalnız iştirakçılar: sender və ya receiver.
	isParticipant := message.SenderID == userID ||
		(message.ReceiverID != nil && *message.ReceiverID == userID)
	if !isParticipant {
		c.JSON(http.StatusForbidden, gin.H{"error": "Bu mesaja giriş icazəniz yoxdur"})
		return
	}

	newText, status, errMsg, alreadyOpened := h.applyViewOnceOpened(&message, userID)
	if errMsg != "" {
		c.JSON(status, gin.H{"error": errMsg})
		return
	}
	if alreadyOpened {
		c.JSON(http.StatusOK, gin.H{"message": "Artıq açılıb", "already_opened": true})
		return
	}

	// WS — mövcud message_edited axını (client _handleMessageEdited bunu
	// text + cache update kimi emal edir; is_edited UI etiketi view-once
	// pill-də onsuz da görünmür).
	editPayload := map[string]interface{}{
		"message_id":       messageID,
		"text":             newText,
		"is_edited":        message.IsEdited,
		"view_once_opened": true,
		"opened_by":        userID,
	}
	h.wsHub.SendToUser(message.SenderID, "message_edited", editPayload)
	if message.ReceiverID != nil && *message.ReceiverID != message.SenderID {
		h.wsHub.SendToUser(*message.ReceiverID, "message_edited", editPayload)
	}

	c.JSON(http.StatusOK, gin.H{"message": "Açıldı", "data": editPayload})
}

// applyViewOnceOpened — mesaj JSON-unu decrypt edib `view_once_opened_by`
// massivinə userID əlavə edir, yenidən şifrələyib DB-yə yazır.
// Qaytarır: (yeniText, httpStatus, errorMsg, alreadyOpened).
// DM və qrup handler-ları paylaşır.
func (h *MessageHandler) applyViewOnceOpened(message *models.Message, userID uint) (string, int, string, bool) {
	decrypted, err := h.encryptionService.DecryptMessage(message.EncryptedText)
	if err != nil {
		return "", http.StatusInternalServerError, "Mesaj çözülemedi", false
	}

	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(decrypted), &payload); err != nil {
		return "", http.StatusBadRequest, "Bu mesaj view-once media deyil", false
	}
	if payload["view_once"] != true {
		return "", http.StatusBadRequest, "Bu mesaj view-once media deyil", false
	}

	// Mövcud opened siyahısı.
	openedBy := []uint{}
	if raw, ok := payload["view_once_opened_by"].([]interface{}); ok {
		for _, v := range raw {
			if f, ok := v.(float64); ok {
				openedBy = append(openedBy, uint(f))
			}
		}
	}
	for _, id := range openedBy {
		if id == userID {
			return decrypted, http.StatusOK, "", true // idempotent
		}
	}
	openedBy = append(openedBy, userID)
	payload["view_once_opened_by"] = openedBy

	newTextBytes, err := json.Marshal(payload)
	if err != nil {
		return "", http.StatusInternalServerError, "JSON xətası", false
	}
	newText := string(newTextBytes)

	encrypted, err := h.encryptionService.EncryptMessage(newText)
	if err != nil {
		return "", http.StatusInternalServerError, "Şifrələmə xətası", false
	}

	if err := database.DB.Model(message).Updates(map[string]interface{}{
		"encrypted_text": encrypted,
		"updated_at":     time.Now().UTC(),
	}).Error; err != nil {
		return "", http.StatusInternalServerError, "Mesaj yenilənə bilmədi", false
	}

	return newText, http.StatusOK, "", false
}

// GetShareRecipients — share modal-da göstəriləcək tövsiyə olunan istifadəçilər.
// Wave/post share zamanı ilkin auditoriya `follow-list`-dən deyil, **chat
// keçmişindən** alınır: ən son danışdığım VƏ ən çox danışdığım dostlar
// üstə çıxsın. Boş chat tarixi olan user üçün caller (Flutter tərəf)
// follow-list-ə düşür.
//
// Score: w_recency * recency_score + w_freq * freq_score
//
//	recency_score = 1 / (1 + days_since_last_message)
//	freq_score    = LN(my_count + other_count + 1) / 5.0  (cap ~ ln(150)/5 = 1.0)
//
// Default w_recency=0.6, w_freq=0.4 — son danışıq əhəmiyyətli, amma ümumi
// münasibət sıxlığı da nəzərə alınır.
//
// Response: [{ user_id, name, username, profile_image, is_verified, score }]
func (h *MessageHandler) GetShareRecipients(c *gin.Context) {
	userIDRaw, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	userID, ok := userIDRaw.(uint)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid user"})
		return
	}

	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "10"))
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if offset < 0 {
		offset = 0
	}

	type Row struct {
		UserID       uint    `json:"user_id"`
		Name         string  `json:"name"`
		Username     string  `json:"username"`
		ProfileImage *string `json:"profile_image"`
		IsVerified   bool    `json:"is_verified"`
		// YENİ (additiv): verified user-in special badge icon URL-i (null ola bilər).
		SpecialBadgeIconURL *string `json:"special_badge_icon_url" gorm:"column:special_badge_icon_url"`
		Score               float64 `json:"score"`
	}

	// Birbaşa messages cədvəlindən: current user-in göndərdiyi mesajlar
	// (sender_id = current). Receiver-ə görə qruplaşdırılır:
	//   • Primary sort: ən son mesajın tarixi (MAX(created_at) DESC)
	//   • Secondary sort: ümumi mesaj sayı (COUNT(*) DESC)
	// Bu halda son danışılan kişi öndə olur, eyni gündə danışdığı bir
	// neçə nəfər varsa daha çox yazışdığı öndə.
	// `profile_image` users-də deyil, `profiles` cədvəlindədir (eyni
	// pattern GetConversations-dakı kimi). LEFT JOIN istifadə edirik ki,
	// profile satırı olmayan user-lər də ekrandan çıxmasın.
	const stmt = `
SELECT
    m.receiver_id AS user_id,
    u.name,
    u.username,
    p.profile_image,
    u.is_verified,
    sender_badge.icon_url AS special_badge_icon_url,
    EXTRACT(EPOCH FROM MAX(m.created_at)) AS score
FROM messages m
INNER JOIN users u ON u.id = m.receiver_id
LEFT JOIN LATERAL (
    SELECT b.icon_url
    FROM badges b
    WHERE b.is_special
      AND b.id = u.selected_badge_id
    ORDER BY b.priority DESC
    LIMIT 1
) sender_badge ON u.is_verified = true
LEFT JOIN profiles p ON p.user_id = m.receiver_id
WHERE m.sender_id = ?
  AND m.receiver_id IS NOT NULL
  AND m.deleted_at IS NULL
  AND COALESCE(m.is_deleted_by_sender, false) = false
GROUP BY m.receiver_id, u.name, u.username, p.profile_image, u.is_verified, sender_badge.icon_url
ORDER BY MAX(m.created_at) DESC, COUNT(*) DESC
LIMIT ? OFFSET ?`

	var rows []Row
	if err := database.GetDB().Raw(stmt, userID, limit, offset).
		Scan(&rows).Error; err != nil {
		log.Printf("GetShareRecipients query error (userID=%d): %v", userID, err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "DB error",
			"detail":  err.Error(),
			"user_id": userID,
		})
		return
	}

	// profile_image relative key kimi gəlir (məs. "profile_images/abc.jpg") —
	// frontend-ə tam URL göndər. GetConversations da məhz `PrependBaseURL`
	// (default StorageLocal → /storage/...) işlədir, S3 storage tipi deyil.
	for i := range rows {
		rows[i].ProfileImage = utils.PrependBaseURL(rows[i].ProfileImage)
	}

	c.JSON(http.StatusOK, gin.H{
		"data": rows,
	})
}

// starredByUser — istifadəçi bu mesajı ulduzlayıbmı (per-user). userID mesajın
// göndəricisidirsə StarredBySender, yoxsa (alıcıdırsa) StarredByReceiver.
func starredByUser(userID, senderID uint, starredBySender, starredByReceiver bool) bool {
	if userID == senderID {
		return starredBySender
	}
	return starredByReceiver
}
