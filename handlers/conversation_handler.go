package handlers

import (
	"beanpon_messenger/database"
	"beanpon_messenger/models"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type ConversationHandler struct {
	wsHub interface {
		IsUserOnline(userID uint) bool
		SendToUser(userID uint, messageType string, data interface{})
		BroadcastScreenshotProtectionChange(user1ID, user2ID uint, isDisabled bool, changedByUserID uint) // ✅ YENİ
	}
	encryptionService interface {
		EncryptMessage(plainText string) (string, error)
		DecryptMessage(encryptedText string) (string, error)
	}
}

func NewConversationHandler(wsHub interface {
	IsUserOnline(userID uint) bool
	SendToUser(userID uint, messageType string, data interface{})
	BroadcastScreenshotProtectionChange(user1ID, user2ID uint, isDisabled bool, changedByUserID uint)
}, encryptionService interface {
	EncryptMessage(plainText string) (string, error)
	DecryptMessage(encryptedText string) (string, error)
}) *ConversationHandler {
	return &ConversationHandler{
		wsHub:             wsHub,
		encryptionService: encryptionService,
	}
}

// GetOrCreateConversation iki kullanıcı arasında conversation getir veya oluştur
func (h *ConversationHandler) GetOrCreateConversation(user1ID, user2ID uint) (*models.Conversation, error) {
	return h.getOrCreateConversationTx(database.DB, user1ID, user2ID)
}

// getOrCreateConversationTx — Issue 40: verilən handle (adi bağlantı VƏ YA
// transaction) üzərində işləyir ki, mesaj insert-i ilə eyni transaction-a
// qoşula bilsin.
func (h *ConversationHandler) getOrCreateConversationTx(db *gorm.DB, user1ID, user2ID uint) (*models.Conversation, error) {
	// Küçük ID'yi user1, büyük ID'yi user2 yap (tutarlılık için)
	if user1ID > user2ID {
		user1ID, user2ID = user2ID, user1ID
	}

	var conversation models.Conversation

	// Önce mevcut conversation'ı ara
	err := db.Where("user1_id = ? AND user2_id = ?", user1ID, user2ID).First(&conversation).Error

	if errors.Is(err, gorm.ErrRecordNotFound) {
		// Yeni conversation oluştur
		conversation = models.Conversation{
			User1ID:                 user1ID,
			User2ID:                 user2ID,
			Status:                  "pending",
			Type:                    "request_based",
			User1MessageCount:       0,
			User2MessageCount:       0,
			MaxPendingMessages:      3,
			User1FollowsUser2:       false,
			User2FollowsUser1:       false,
			MutualFollow:            false,
			HasPreviousConversation: false,
			User1Muted:              false,
			User2Muted:              false,
			User1Restricted:         false,
			User2Restricted:         false,
			TotalMessagesCount:      0,
		}

		// Follow ilişkilerini kontrol et
		h.updateFollowRelations(db, &conversation)

		// 🆕 YENİ: Screenshot protection kontrolü
		// User1'in ayarlarını kontrol et
		var user1Settings models.UserSettings
		if err := db.Where("user_id = ?", user1ID).First(&user1Settings).Error; err == nil {
			if user1Settings.ConversationScreenshotDisabled {
				conversation.User1ScreenshotDisabled = true
				now := time.Now()
				conversation.User1ScreenshotDisabledAt = &now
			}
		}

		// User2'nin ayarlarını kontrol et
		var user2Settings models.UserSettings
		if err := db.Where("user_id = ?", user2ID).First(&user2Settings).Error; err == nil {
			if user2Settings.ConversationScreenshotDisabled {
				conversation.User2ScreenshotDisabled = true
				now := time.Now()
				conversation.User2ScreenshotDisabledAt = &now
			}
		}

		// Issue 13: SELECT-sonra-INSERT yarışı. İki paralel "ilk mesaj" hər
		// ikisi də yuxarıdakı SELECT-də tapmır və hər ikisi INSERT edirdi →
		// eyni cüt üçün İKİ conversation. İndi konflikt sükutla udulur və
		// qazanan sətir yenidən oxunur (upsert semantikası).
		// DEPLOY TƏHLÜKƏSİZLİYİ: hədəf sütunlar YAZILMIR — `ON CONFLICT (a,b)`
		// uyğun UNIQUE indeks yoxdursa Postgres XƏTA verir. Hədəfsiz forma
		// indekssiz adi INSERT kimi davranır (köhnə davranış, yarış qalır),
		// indeks yaradıldıqdan sonra isə upsert olur.
		// Bax: MIGRATION_conversations_pair_unique.md
		if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&conversation).Error; err != nil {
			return nil, err
		}
		if conversation.ID == 0 {
			// Konflikt oldu (başqa istək bizi qabaqladı) → qazananı oxu.
			if err := db.Where("user1_id = ? AND user2_id = ?", user1ID, user2ID).
				First(&conversation).Error; err != nil {
				return nil, err
			}
		}
	} else if err != nil {
		return nil, err
	}

	return &conversation, nil
}

// updateFollowRelations follow ilişkilerini güncelle.
//
// Issue 40 (KRİTİK): `db` PARAMETRİK olmalıdır. Bu funksiya artıq transaction
// içindən çağırıla bilir; qlobal `database.DB` işlətsəydi transaction öz
// bağlantısını TUTARKƏN hovuzdan İKİNCİ bağlantı istəyərdi. Hovuz 25 ilə
// məhduddur (database.go): 25 paralel "ilk mesaj" hər biri bir bağlantı tutub
// 26-cını gözləyər → heç biri buraxa bilməz → bütün DB girişi kilidlənər.
// Sorğularda kontekst/timeout yoxdur, yəni kilid ƏBƏDİDİR.
func (h *ConversationHandler) updateFollowRelations(db *gorm.DB, conversation *models.Conversation) {
	// follows tablosunu kontrol et (eğer varsa)
	var count1, count2 int64

	// User1 -> User2 follow kontrolü
	db.Table("follows").Where("follower_id = ? AND following_id = ?",
		conversation.User1ID, conversation.User2ID).Count(&count1)

	// User2 -> User1 follow kontrolü
	db.Table("follows").Where("follower_id = ? AND following_id = ?",
		conversation.User2ID, conversation.User1ID).Count(&count2)

	conversation.User1FollowsUser2 = count1 > 0
	conversation.User2FollowsUser1 = count2 > 0
	conversation.MutualFollow = conversation.User1FollowsUser2 && conversation.User2FollowsUser1

	// Type'ı güncelle
	if conversation.MutualFollow {
		conversation.Type = "follow_based"
	} else {
		conversation.Type = "request_based"
	}
}

// CanSendMessage kullanıcının mesaj gönderip gönderemeyeceğini kontrol et
func (h *ConversationHandler) CanSendMessageOld(senderID, receiverID uint) (bool, string, error) {
	// Önce block kontrolü
	if models.IsBlocked(database.DB, senderID, receiverID) {
		return false, "Bu istifadəçiyə mesaj göndərə bilməzsiniz (blokladınız)", nil
	}

	// Conversation'ı bul
	var conversation models.Conversation
	err := database.DB.Where(
		"(user1_id = ? AND user2_id = ?) OR (user1_id = ? AND user2_id = ?)",
		senderID, receiverID, receiverID, senderID,
	).First(&conversation).Error

	// Conversation yoksa, yeni conversation oluşturulacak - verified kontrolü yap
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// 🆕 SADECE YENİ CONVERSATION İÇİN VERIFIED KONTROLÜ
			var receiverSettings models.UserSettings
			if err := database.DB.Where("user_id = ?", receiverID).First(&receiverSettings).Error; err == nil {
				// Eğer ONLY_VERIFIED ise, gönderende verified kontrolü yap
				if receiverSettings.MessageRequests == "ONLY_VERIFIED" {
					var sender models.User
					if err := database.DB.Where("id = ?", senderID).First(&sender).Error; err != nil {
						return false, "İstifadəçi tapılmadı", err
					}

					if !sender.IsVerified {
						return false, "Bu istifadəçiyə mesaj göndərmək üçün təsdiqlənmiş hesab tələb olunur", nil
					}
				}
			}
			// Eğer user_settings kaydı yoksa veya ALL ise, izin ver
			return true, "", nil
		}
		return false, "Verilənlər bazası xətası", err
	}

	// 🎯 Conversation VARSA (daha önce mesajlaşmışlarsa), verified kontrolü YOK
	// Sadece conversation durumunu kontrol et

	switch conversation.Status {
	case "active":
		// Active ise her şey tamam
		return true, "", nil

	case "pending":
		// Pending durumda, gönderen kullanıcının mesaj limitini kontrol et
		var senderMessageCount int
		if conversation.User1ID == senderID {
			senderMessageCount = conversation.User1MessageCount
		} else {
			senderMessageCount = conversation.User2MessageCount
		}

		if senderMessageCount >= conversation.MaxPendingMessages {
			return false, "Mesaj limiti doldu. Qarşı tərəf cavab verməlidir", nil
		}

		return true, "", nil

	case "restricted":
		// Restricted durumda kimse mesaj gönderemez
		return false, "Bu söhbət məhdudlaşdırılıb", nil

	default:
		return false, "Naməlum söhbət statusu", nil
	}
}

func (h *ConversationHandler) CanSendMessage(senderID, receiverID uint) (bool, string, error) {
	_, canSend, errorMsg, err := h.GetOrCreateConversationWithPermission(senderID, receiverID)
	return canSend, errorMsg, err
}

// GetOrCreateConversationWithPermission conversation'ı getirir veya oluşturur ve izin kontrolü yapar
func (h *ConversationHandler) GetOrCreateConversationWithPermission(senderID, receiverID uint) (*models.Conversation, bool, string, error) {
	// Block kontrolü
	if models.IsBlocked(database.DB, senderID, receiverID) {
		return nil, false, "Bu istifadəçiyə mesaj göndərə bilməzsiniz (blokladınız)", nil
	}

	// Gizli Mod: gizli kullanıcıya (close-friend olmayan) DM engellenir; gizli
	// kullanıcı da yalnız close-friends'e yazabilir. Bu, bütün REST göndərmə
	// yollarının keçdiyi ortaq nöqtədir (CanSendMessage → burası). "Bulunamadı"
	// kimi davranırıq ki, gizli durum sızmasın.
	if models.DMHiddenBlocked(database.DB, senderID, receiverID) {
		return nil, false, "İstifadəçi tapılmadı", nil
	}

	// Conversation'ı bul
	var conversation models.Conversation
	err := database.DB.Where(
		"(user1_id = ? AND user2_id = ?) OR (user1_id = ? AND user2_id = ?)",
		senderID, receiverID, receiverID, senderID,
	).First(&conversation).Error

	// Conversation yoksa yeni conversation için verified kontrolü
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// NOT: Spam (`spam_bans`) yoxlaması artıq burada DEYİL.
			// MessageHandler.SendMessage handler-in ən başında, conversation
			// lookup-dan əvvəl global shadow-ban guard işləyir. Beləliklə
			// spam istifadəçi nə yeni söhbət başlada bilir, nə də mövcud
			// söhbətdə davam edə bilir. Bu blok yalnız verified/follow kimi
			// qarşı tərəfin tənzimləmələrini yoxlayır.

			var receiverSettings models.UserSettings
			if err := database.DB.Where("user_id = ?", receiverID).First(&receiverSettings).Error; err == nil {
				if receiverSettings.MessageRequests == "ONLY_VERIFIED" {
					var sender models.User
					if err := database.DB.Where("id = ?", senderID).First(&sender).Error; err != nil {
						return nil, false, "İstifadəçi tapılmadı", err
					}

					if !sender.IsVerified {
						return nil, false, "Bu istifadəçiyə mesaj göndərmək üçün təsdiqlənmiş hesab tələb olunur", nil
					}
				} else if receiverSettings.MessageRequests == "FOLLOWING" {
					// Receiver yalnız özü izlədiyi hesablardan mesaj qəbul edir.
					// Yəni: receiver -> sender follow əlaqəsi olmalıdır.
					var followCount int64
					database.DB.Table("follows").
						Where("follower_id = ? AND following_id = ?", receiverID, senderID).
						Count(&followCount)
					if followCount == 0 {
						return nil, false, "Bu istifadəçi yalnız izlədiyi hesablardan mesaj qəbul edir", nil
					}
				}
			}
			// Conversation yok ama izin var - nil conversation döndür
			return nil, true, "", nil
		}
		return nil, false, "Verilənlər bazası xətası", err
	}

	// 🚫 ADMIN BLOK — söhbət admin (Filament) tərəfindən bloklanıbsa, BU
	// söhbətdə heç kim (nə user1, nə user2) yeni mesaj göndərə bilməz.
	// Köhnə mesajlar görünür qalır; yalnız yeni göndərmə dayanır.
	//
	// reason="" QAYTARILIR — bu, spam shadow-ban ilə eyni davranışı verir:
	//   • REST: göndərənə saxta 201 "uğurlu" cavabı gedir, amma mesaj DB-yə
	//     yazılmır və qarşı tərəfə çatmır (admin blokunu bilməsin).
	//   • WebSocket/broadcast: mesaj sakitcə atlanır (continue).
	// Beləliklə nə göndərən, nə qəbul edən admin blokunu hiss etmir.
	if conversation.Blocked {
		return &conversation, false, "", nil
	}

	// Conversation var - izin kontrolü yap
	canSend, errorMsg := h.checkConversationPermission(&conversation, senderID)
	return &conversation, canSend, errorMsg, nil
}

func (h *ConversationHandler) checkConversationPermission(conversation *models.Conversation, senderID uint) (bool, string) {
	switch conversation.Status {
	case "active":
		return true, ""

	case "pending":
		var senderMessageCount int
		if conversation.User1ID == senderID {
			senderMessageCount = conversation.User1MessageCount
		} else {
			senderMessageCount = conversation.User2MessageCount
		}

		if senderMessageCount >= conversation.MaxPendingMessages {
			return false, "Mesaj limiti doldu. Qarşı tərəf cavab verməlidir"
		}
		return true, ""

	case "restricted":
		return false, "Bu söhbət məhdudlaşdırılıb"

	default:
		return false, "Naməlum söhbət statusu"
	}
}

// UpdateConversationOnMessage mesaj gönderildikten sonra conversation güncelle
func (h *ConversationHandler) UpdateConversationOnMessage(senderID, receiverID uint) error {
	_, err := h.UpdateConversationOnMessageTx(database.DB, senderID, receiverID,
		h.ShouldSkipConversationCreate(senderID, receiverID))
	return err
}

// ShouldSkipConversationCreate — Issue 40: mesaj banı yoxlaması REDİS I/O edir
// (`models.IsMessagingBanned` → cache, 1 sn timeout). Transaction İÇİNDƏ
// çağırılsaydı yavaş/əlçatmaz Redis açıq bir Postgres transaction-ını (və onun
// `messages` sətir kilidini) saniyələrlə tutardı. Ona görə çağıran bunu
// transaction-dan ƏVVƏL çağırır və nəticəni ötürür.
//
// `true` → söhbət YOXDURSA yaratma (banlı istifadəçi yeni söhbət aça bilməz).
func (h *ConversationHandler) ShouldSkipConversationCreate(senderID, receiverID uint) bool {
	if !models.IsMessagingBanned(database.DB, senderID) {
		return false
	}
	var existing models.Conversation
	err := database.DB.Where(
		"(user1_id = ? AND user2_id = ?) OR (user1_id = ? AND user2_id = ?)",
		senderID, receiverID, receiverID, senderID,
	).First(&existing).Error
	// Söhbət yoxdursa → yaratma. Varsa → normal davam (ban yalnız YENİ söhbətə).
	return errors.Is(err, gorm.ErrRecordNotFound)
}

// UpdateConversationOnMessageTx — Issue 40: mesaj insert-i ilə EYNİ
// transaction içində çağırıla bilsin deyə handle parametrik.
//
// `skipCreate` — `ShouldSkipConversationCreate`-in transaction-dan ƏVVƏL
// hesablanmış nəticəsi (Redis I/O transaction içində olmasın deyə).
//
// ── Issue 10: push qapısı üçün GERÇƏK status qaytarılır ──────────────────────
// Əvvəl bu funksiya yalnız `error` qaytarırdı və REST `SendMessage` push
// qapısına SABİT `"active"` ötürürdü. WS yolu isə `conversation.Status`-un
// həqiqi dəyərini ötürür. Nəticə: EYNİ mesaj REST ilə göndərildikdə push
// GEDİR, WS ilə göndərildikdə (status `pending`/`restricted` olanda) push
// GETMİR — yəni bildiriş davranışı NƏQLİYYATDAN asılı olurdu. iOS mətn
// mesajlarını əsasən WS, media mesajlarını REST ilə göndərdiyi üçün
// istifadəçi "bəzi mesajlarda bildiriş gəlir, bəzilərində yox" görürdü.
// Üstəlik mesajlaşma istəyini (pending) hələ qəbul etməmiş adama və
// məhdudlaşdırılmış (restricted) söhbətdə REST üzərindən push GEDİRDİ —
// spam qapısının birbaşa deşiyi.
//
// İndi hər iki yol eyni mənbədən (`conversation.Status`) qidalanır.
func (h *ConversationHandler) UpdateConversationOnMessageTx(db *gorm.DB, senderID, receiverID uint, skipCreate bool) (string, error) {
	// 🚫 SPAM KORUMASI: mesaj banlı kullanıcı YENİ conversation başlatamaz.
	// Conversation yoksa ve gönderenin mesaj banı varsa sessizce çık
	// (conversation oluşturulmaz, hata da dönülmez). Conversation zaten
	// varsa dokunma — bu kontrol yalnızca ilk conversation create için.
	if skipCreate {
		// Söhbət yaradılmadı → aktiv söhbət yoxdur → push da getmir.
		return "", nil
	}

	conversation, err := h.getOrCreateConversationTx(db, senderID, receiverID)
	if err != nil {
		return "", err
	}

	if err := applyConversationMessageUpdate(db, conversation, senderID); err != nil {
		return "", err
	}
	return conversation.Status, nil
}

// applyConversationMessageUpdate — Issue 14: sayğac artımı ATOMİK SQL ilə.
//
// Əvvəl artım Go-da yaddaşdakı nüsxə üzərində edilir, sonra `Save` BÜTÜN
// sütunları geri yazırdı. İki nəticəsi vardı:
//  1. İTMİŞ ARTIM — paralel iki göndərmə eyni dəyəri oxuyub eyni nəticəyə
//     artırırdı (sayğac 2 yerinə 1 artır) → pending limiti yanlış işləyir.
//  2. EZİLƏN AYARLAR — sətir oxunduqdan sonra `Save`-ə qədər dəyişən ƏLAQƏSİZ
//     sütunlar (mute/pin/archive/nickname/wallpaper) köhnə dəyərlərlə geri
//     yazılırdı.
//
// İndi: sayğaclar `col = col + 1` ilə DB tərəfində artır; status keçidləri
// KOMİT OLUNMUŞ dəyərlər yenidən oxunaraq, yalnız status sütunları yenilənərək
// tətbiq olunur. `db` transaction ola bilər (Issue 40).
func applyConversationMessageUpdate(db *gorm.DB, conversation *models.Conversation, senderID uint) error {
	now := time.Now().UTC()

	updates := map[string]interface{}{
		"last_message_at":      now,
		"total_messages_count": gorm.Expr("total_messages_count + 1"),
		// COALESCE → yalnız ilk dəfə yazılır (yarışa dayanıqlı).
		"first_message_at": gorm.Expr("COALESCE(first_message_at, ?)", now),
	}
	if senderID == conversation.User1ID {
		updates["user1_message_count"] = gorm.Expr("user1_message_count + 1")
	} else {
		updates["user2_message_count"] = gorm.Expr("user2_message_count + 1")
	}
	if err := db.Model(&models.Conversation{}).
		Where("id = ?", conversation.ID).
		Updates(updates).Error; err != nil {
		return err
	}

	// Status keçidi üçün KOMİT OLUNMUŞ dəyərləri oxu (yaddaşdakı nüsxə köhnədir).
	var fresh models.Conversation
	if err := db.Select("id", "status", "user1_message_count", "user2_message_count", "max_pending_messages", "total_messages_count", "has_previous_conversation").
		Where("id = ?", conversation.ID).First(&fresh).Error; err != nil {
		// Sayğaclar artıq yazıldı — status keçidini növbəti mesaj tətbiq edər.
		return nil
	}

	// Çağıranın nüsxəsi də təzələnsin (push qapısı statusu oxuyur — Issue 10).
	conversation.User1MessageCount = fresh.User1MessageCount
	conversation.User2MessageCount = fresh.User2MessageCount
	conversation.Status = fresh.Status
	// Köhnə `Save` yolu bunları da sinxron saxlayırdı — parite üçün.
	conversation.LastMessageAt = &now
	conversation.TotalMessagesCount = fresh.TotalMessagesCount
	if conversation.FirstMessageAt == nil {
		conversation.FirstMessageAt = &now
	}
	// `has_previous_conversation` köhnə kodda hər iki sayğac >0 olan HƏR
	// mesajda yazılırdı; yalnız pending→active keçidində yazsaq, `active`-ə
	// başqa yolla (updateConversationStatus) keçmiş söhbətlərdə bayraq heç
	// vaxt qalxmazdı.
	if fresh.User1MessageCount > 0 && fresh.User2MessageCount > 0 && !conversation.HasPreviousConversation {
		if err := db.Model(&models.Conversation{}).
			Where("id = ? AND has_previous_conversation = ?", conversation.ID, false).
			Update("has_previous_conversation", true).Error; err != nil {
			return err
		}
		conversation.HasPreviousConversation = true
	}

	switch {
	case fresh.User1MessageCount > 0 && fresh.User2MessageCount > 0 && fresh.Status != "active":
		// Şərtli WHERE: paralel dəyişikliyi əzmə.
		if err := db.Model(&models.Conversation{}).
			Where("id = ? AND status <> ?", conversation.ID, "active").
			Updates(map[string]interface{}{
				"status":                    "active",
				"has_previous_conversation": true,
				"status_changed_at":         now,
			}).Error; err != nil {
			return err
		}
		conversation.Status = "active"

	case fresh.Status == "pending":
		maxCount := fresh.User1MessageCount
		if fresh.User2MessageCount > maxCount {
			maxCount = fresh.User2MessageCount
		}
		if (fresh.User1MessageCount == 0 || fresh.User2MessageCount == 0) &&
			maxCount > fresh.MaxPendingMessages {
			if err := db.Model(&models.Conversation{}).
				Where("id = ? AND status = ?", conversation.ID, "pending").
				Updates(map[string]interface{}{
					"status":             "restricted",
					"status_changed_at":  now,
					"restriction_reason": "Tek taraflı mesaj limiti aşıldı",
				}).Error; err != nil {
				return err
			}
			conversation.Status = "restricted"
		}
	}
	return nil
}

// updateConversationStatus conversation durumunu güncelle
func (h *ConversationHandler) updateConversationStatus(conversationID uint, status string) error {
	now := time.Now()
	return database.DB.Model(&models.Conversation{}).
		Where("id = ?", conversationID).
		Updates(map[string]interface{}{
			"status":            status,
			"status_changed_at": &now,
		}).Error
}

// MuteConversation konuşmayı sessize al
func (h *ConversationHandler) MuteConversation(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	otherUserID, err := strconv.ParseUint(c.Param("other_user_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz kullanıcı ID"})
		return
	}

	var requestBody struct {
		MuteDuration int `json:"muteDuration"` // dakika cinsinden
	}

	if err := c.ShouldBindJSON(&requestBody); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz request body"})
		return
	}

	conversation, err := h.GetOrCreateConversation(userID.(uint), uint(otherUserID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Conversation bulunamadı"})
		return
	}

	now := time.Now()

	// Hangi kullanıcı mute ediyor?
	if userID.(uint) == conversation.User1ID {
		conversation.User1Muted = true
		conversation.User1MutedAt = &now

		// MuteDuration kontrolü
		if requestBody.MuteDuration > 0 {
			mutedUntil := now.Add(time.Duration(requestBody.MuteDuration) * time.Minute)
			conversation.User1MutedUntil = &mutedUntil
		} else {
			// Always mute (0 geldiyse null olsun)
			conversation.User1MutedUntil = nil
		}
	} else {
		conversation.User2Muted = true
		conversation.User2MutedAt = &now

		// MuteDuration kontrolü
		if requestBody.MuteDuration > 0 {
			mutedUntil := now.Add(time.Duration(requestBody.MuteDuration) * time.Minute)
			conversation.User2MutedUntil = &mutedUntil
		} else {
			// Always mute (0 geldiyse null olsun)
			conversation.User2MutedUntil = nil
		}
	}

	if err := database.DB.Save(conversation).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Mute işlemi başarısız"})
		return
	}

	response := gin.H{
		"message":  "Konuşma sessize alındı",
		"muted_at": now,
	}

	// Eğer süre belirtilmişse response'a ekle
	if requestBody.MuteDuration > 0 {
		mutedUntil := now.Add(time.Duration(requestBody.MuteDuration) * time.Minute)
		response["muted_until"] = mutedUntil
	}

	c.JSON(http.StatusOK, response)
}

// UnmuteConversation konuşma sesini aç
func (h *ConversationHandler) UnmuteConversation(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	otherUserID, err := strconv.ParseUint(c.Param("other_user_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz kullanıcı ID"})
		return
	}

	conversation, err := h.GetOrCreateConversation(userID.(uint), uint(otherUserID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Conversation bulunamadı"})
		return
	}

	// Hangi kullanıcı unmute ediyor?
	if userID.(uint) == conversation.User1ID {
		conversation.User1Muted = false
		conversation.User1MutedAt = nil
		conversation.User1MutedUntil = nil
	} else {
		conversation.User2Muted = false
		conversation.User2MutedAt = nil
		conversation.User2MutedUntil = nil
	}

	if err := database.DB.Save(conversation).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Unmute işlemi başarısız"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "Konuşma sesi açıldı",
	})
}

// ArchiveConversation — söhbəti istifadəçinin ÖZÜ üçün arxivləyir (per-user).
// A arxivləyəndə yalnız A-nın siyahısından gizlənir; B-də normal qalır.
// Arxivləyən şəxsə gələn mesajlar üçün push notification göndərilmir
// (bax: message_handler / hub.go push məntiqi).
func (h *ConversationHandler) ArchiveConversation(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	otherUserID, err := strconv.ParseUint(c.Param("other_user_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz kullanıcı ID"})
		return
	}

	conversation, err := h.GetOrCreateConversation(userID.(uint), uint(otherUserID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Conversation bulunamadı"})
		return
	}

	now := time.Now()
	if userID.(uint) == conversation.User1ID {
		conversation.User1Archived = true
		conversation.User1ArchivedAt = &now
	} else {
		conversation.User2Archived = true
		conversation.User2ArchivedAt = &now
	}

	if err := database.DB.Save(conversation).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Arşivleme başarısız"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message":     "Konuşma arşivlendi",
		"archived":    true,
		"archived_at": now,
	})
}

// UnarchiveConversation — söhbəti arxivdən çıxarır (per-user).
func (h *ConversationHandler) UnarchiveConversation(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	otherUserID, err := strconv.ParseUint(c.Param("other_user_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz kullanıcı ID"})
		return
	}

	conversation, err := h.GetOrCreateConversation(userID.(uint), uint(otherUserID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Conversation bulunamadı"})
		return
	}

	if userID.(uint) == conversation.User1ID {
		conversation.User1Archived = false
		conversation.User1ArchivedAt = nil
	} else {
		conversation.User2Archived = false
		conversation.User2ArchivedAt = nil
	}

	if err := database.DB.Save(conversation).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Arşivden çıkarma başarısız"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message":  "Konuşma arşivden çıkarıldı",
		"archived": false,
	})
}

// PinConversation — söhbəti istifadəçinin ÖZÜ üçün pin edir (per-user).
// A pin edəndə yalnız A-nın siyahısında ən yuxarı gəlir; B-də normal sırada
// qalır. Bir neçə söhbət eyni anda pin oluna bilər (pinned_at-a görə sıralanır).
func (h *ConversationHandler) PinConversation(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	otherUserID, err := strconv.ParseUint(c.Param("other_user_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz kullanıcı ID"})
		return
	}

	conversation, err := h.GetOrCreateConversation(userID.(uint), uint(otherUserID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Conversation bulunamadı"})
		return
	}

	now := time.Now()
	if userID.(uint) == conversation.User1ID {
		conversation.User1Pinned = true
		conversation.User1PinnedAt = &now
	} else {
		conversation.User2Pinned = true
		conversation.User2PinnedAt = &now
	}

	if err := database.DB.Save(conversation).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Sabitleme başarısız"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message":   "Konuşma sabitlendi",
		"pinned":    true,
		"pinned_at": now,
	})
}

// UnpinConversation — söhbəti pin-dən çıxarır (per-user).
func (h *ConversationHandler) UnpinConversation(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	otherUserID, err := strconv.ParseUint(c.Param("other_user_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz kullanıcı ID"})
		return
	}

	conversation, err := h.GetOrCreateConversation(userID.(uint), uint(otherUserID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Conversation bulunamadı"})
		return
	}

	if userID.(uint) == conversation.User1ID {
		conversation.User1Pinned = false
		conversation.User1PinnedAt = nil
	} else {
		conversation.User2Pinned = false
		conversation.User2PinnedAt = nil
	}

	if err := database.DB.Save(conversation).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Sabitlemeden çıkarma başarısız"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "Konuşma sabitlemeden çıkarıldı",
		"pinned":  false,
	})
}

// SetNickname — istifadəçi qarşı tərəf üçün ləqəb təyin edir (per-user,
// birtərəfli). YALNIZ təyin edən şəxs bu ləqəbi görür. Body: {"nickname":"..."}.
// Boş və ya yalnız boşluqdursa → ləqəb təmizlənir (əsl ada qayıdır).
func (h *ConversationHandler) SetNickname(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	otherUserID, err := strconv.ParseUint(c.Param("other_user_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz kullanıcı ID"})
		return
	}

	var body struct {
		Nickname string `json:"nickname"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz istek"})
		return
	}

	// Trim + uzunluq limiti (sütun varchar(60)).
	name := strings.TrimSpace(body.Nickname)
	if len([]rune(name)) > 60 {
		name = string([]rune(name)[:60])
	}

	conversation, err := h.GetOrCreateConversation(userID.(uint), uint(otherUserID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Conversation bulunamadı"})
		return
	}

	// Boşdursa nil (təmizlə), əks halda dəyər.
	var val *string
	if name != "" {
		val = &name
	}
	if userID.(uint) == conversation.User1ID {
		conversation.User1Nickname = val
	} else {
		conversation.User2Nickname = val
	}

	if err := database.DB.Save(conversation).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Takma ad kaydedilemedi"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message":  "Takma ad güncellendi",
		"nickname": val, // null → təmizləndi
	})
}

// ClearNickname — ləqəbi silir (əsl ada qaytarır). SetNickname boş body ilə
// də eyni işi görür; bu ayrıca endpoint rahatlıq üçündür.
func (h *ConversationHandler) ClearNickname(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	otherUserID, err := strconv.ParseUint(c.Param("other_user_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz kullanıcı ID"})
		return
	}

	conversation, err := h.GetOrCreateConversation(userID.(uint), uint(otherUserID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Conversation bulunamadı"})
		return
	}

	if userID.(uint) == conversation.User1ID {
		conversation.User1Nickname = nil
	} else {
		conversation.User2Nickname = nil
	}

	if err := database.DB.Save(conversation).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Takma ad silinemedi"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message":  "Takma ad silindi",
		"nickname": nil,
	})
}

// SetWallpaper — istifadəçi BU söhbət üçün çat fonu (wallpaper) seçir
// (per-user). Body: {"wallpaper_id": 5}. Yalnız seçim ID-si saxlanır;
// wallpaper-in özü Laravel-də. YALNIZ seçən şəxsin çatına təsir edir.
func (h *ConversationHandler) SetWallpaper(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	otherUserID, err := strconv.ParseUint(c.Param("other_user_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz kullanıcı ID"})
		return
	}

	var body struct {
		WallpaperID uint `json:"wallpaper_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.WallpaperID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz istek"})
		return
	}

	conversation, err := h.GetOrCreateConversation(userID.(uint), uint(otherUserID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Conversation bulunamadı"})
		return
	}

	wid := body.WallpaperID
	if userID.(uint) == conversation.User1ID {
		conversation.User1WallpaperID = &wid
	} else {
		conversation.User2WallpaperID = &wid
	}

	if err := database.DB.Save(conversation).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Duvar kağıdı kaydedilemedi"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message":      "Duvar kağıdı güncellendi",
		"wallpaper_id": wid,
	})
}

// ClearWallpaper — bu söhbət üçün wallpaper seçimini sıfırlayır (qlobal/default
// görünür).
func (h *ConversationHandler) ClearWallpaper(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	otherUserID, err := strconv.ParseUint(c.Param("other_user_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz kullanıcı ID"})
		return
	}

	conversation, err := h.GetOrCreateConversation(userID.(uint), uint(otherUserID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Conversation bulunamadı"})
		return
	}

	if userID.(uint) == conversation.User1ID {
		conversation.User1WallpaperID = nil
	} else {
		conversation.User2WallpaperID = nil
	}

	if err := database.DB.Save(conversation).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Duvar kağıdı silinemedi"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message":      "Duvar kağıdı sıfırlandı",
		"wallpaper_id": nil,
	})
}

// GetConversationDetails conversation detaylarını getir
func (h *ConversationHandler) GetConversationDetails(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	otherUserID, err := strconv.ParseUint(c.Param("other_user_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz kullanıcı ID"})
		return
	}

	// 🕵️ GİZLİ MOD — 1:1 söhbət detalını açmaq. Qarşı tərəf gizli olub viewer
	// onun close-friend'i deyilsə (və ya viewer gizli olub qarşı tərəf onun
	// close-friend'i deyilsə) söhbət ƏLÇATMAZDIR. Gizli olduğunu ifşa etməmək
	// üçün "istifadəçi tapılmadı".
	if models.DMHiddenBlocked(database.DB, userID.(uint), uint(otherUserID)) {
		c.JSON(http.StatusNotFound, gin.H{"error": "İstifadəçi tapılmadı"})
		return
	}

	conversation, err := h.GetOrCreateConversation(userID.(uint), uint(otherUserID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Conversation bulunamadı"})
		return
	}

	canSendMessage := true
	conversationType := "normal"
	var stopMessageReason *string = nil

	switch conversation.Status {
	case "pending":
		conversationType = "pending"
		var myMessageCount int
		if userID.(uint) == conversation.User1ID {
			myMessageCount = conversation.User1MessageCount
		} else {
			myMessageCount = conversation.User2MessageCount
		}
		if myMessageCount >= conversation.MaxPendingMessages {
			canSendMessage = false
		}
	case "restricted":
		conversationType = "restricted"
		canSendMessage = false
	case "active":
		conversationType = "normal"
	}

	var myMessageCount, otherMessageCount int
	var isMutedByMe, amIRestricted, isOtherMuted, isOtherRestricted bool

	if userID.(uint) == conversation.User1ID {
		myMessageCount = conversation.User1MessageCount
		otherMessageCount = conversation.User2MessageCount
		isMutedByMe = conversation.User1Muted
		amIRestricted = conversation.User1Restricted
		isOtherMuted = conversation.User2Muted
		isOtherRestricted = conversation.User2Restricted
	} else {
		myMessageCount = conversation.User2MessageCount
		otherMessageCount = conversation.User1MessageCount
		isMutedByMe = conversation.User2Muted
		amIRestricted = conversation.User2Restricted
		isOtherMuted = conversation.User1Muted
		isOtherRestricted = conversation.User1Restricted
	}

	if amIRestricted {
		canSendMessage = false
	}

	// Admin (Filament) blok — BU söhbətdə heç kim mesaj göndərə bilməz.
	// stop_message_reason burada dəyişdirilmir; client `blocked` sahəsinə görə
	// ayrıca "söhbət bağlandı" banner-i göstərir (restriction banner deyil).
	if conversation.Blocked {
		canSendMessage = false
	}

	// User-blok (`user_blocks`) — indiyədək detala DÜŞMÜRDÜ (yalnız send
	// yollarında models.IsBlocked ilə yoxlanırdı). Client-ə əlavə olaraq ötürülür:
	//   blocked        → admin bloku VƏ YA istənilən istiqamətdə user bloku
	//   blocked_by_me  → bloklayan MƏNƏM (client "engeli kaldır" göstərə bilsin).
	// Qarşı tərəf məni bloklayıbsa yalnız generik "bağlandı" vəziyyəti görünür —
	// səbəb açıqlanmır (WhatsApp davranışı). blocked_by_me YENİ sahədir; köhnə
	// clientlər onu tanımır və davranışları dəyişmir.
	var userBlocks []models.UserBlock
	database.DB.Where(
		"(blocker_id = ? AND blocked_id = ?) OR (blocker_id = ? AND blocked_id = ?)",
		userID, otherUserID, otherUserID, userID,
	).Find(&userBlocks)
	blockedByMe := false
	for _, b := range userBlocks {
		if b.BlockerID == userID.(uint) {
			blockedByMe = true
		}
	}
	if len(userBlocks) > 0 {
		canSendMessage = false
	}

	// Other user settings — scope'u buraya çek ki aşağıda da erişebilelim
	allowVoiceMessages := true
	showReadReceipts := true

	totalMessages := conversation.User1MessageCount + conversation.User2MessageCount

	if totalMessages == 0 {
		var otherUserSettings models.UserSettings
		if err := database.DB.Where("user_id = ?", otherUserID).First(&otherUserSettings).Error; err == nil {

			allowVoiceMessages = EffectiveAllowVoice(uint(otherUserID), userID.(uint), otherUserSettings.AllowVoiceMessages)
			showReadReceipts = otherUserSettings.ShowReadReceipts

			if otherUserSettings.MessageRequests == "ONLY_VERIFIED" {
				var myUser models.User
				if err := database.DB.Where("id = ?", userID).First(&myUser).Error; err == nil {
					if !myUser.IsVerified {
						reason := "ONLY_VERIFIED"
						stopMessageReason = &reason
						canSendMessage = false
					} else {
						reason := "ALL"
						stopMessageReason = &reason
					}
				}
			} else if otherUserSettings.MessageRequests == "FOLLOWING" {
				// Qarşı tərəf (otherUser) yalnız özü izlədiyi hesablardan mesaj qəbul edir.
				// Yəni: otherUser -> me (userID) follow əlaqəsi olmalıdır.
				var followCount int64
				database.DB.Table("follows").
					Where("follower_id = ? AND following_id = ?", otherUserID, userID).
					Count(&followCount)
				if followCount == 0 {
					reason := "FOLLOWING"
					stopMessageReason = &reason
					canSendMessage = false
				} else {
					reason := "ALL"
					stopMessageReason = &reason
				}
			} else {
				reason := "ALL"
				stopMessageReason = &reason
			}
		}
	} else {
		// Mövcud conversation — settings yenə də lazımdır
		var otherUserSettings models.UserSettings
		if err := database.DB.Where("user_id = ?", otherUserID).First(&otherUserSettings).Error; err == nil {
			allowVoiceMessages = EffectiveAllowVoice(uint(otherUserID), userID.(uint), otherUserSettings.AllowVoiceMessages)
			showReadReceipts = otherUserSettings.ShowReadReceipts
		}

		reason := "PREVIOUS_CONVERSATION"
		stopMessageReason = &reason
	}

	// Cari user-in (A) qarşı tərəf (B=otherUser) üçün sesli mesaj izni — konuşma
	// detayındaki toggle bunu gösterir: override(A→B) ?? A.global.
	myVoiceGlobal := true
	{
		var mySettings models.UserSettings
		if err := database.DB.Where("user_id = ?", userID.(uint)).First(&mySettings).Error; err == nil {
			myVoiceGlobal = mySettings.AllowVoiceMessages
		}
	}
	voicePermissionForOther := EffectiveAllowVoice(userID.(uint), uint(otherUserID), myVoiceGlobal)

	responseData := gin.H{
		"conversation": gin.H{
			"id":                   conversation.ID,
			"status":               conversation.Status,
			"type":                 conversationType,
			"can_send_message":     canSendMessage,
			"stop_message_reason":  stopMessageReason,
			"is_muted_by_me":       isMutedByMe,
			"am_i_restricted":      amIRestricted,
			"is_other_muted":       isOtherMuted,
			"is_other_restricted":  isOtherRestricted,
			"my_message_count":     myMessageCount,
			"other_message_count":  otherMessageCount,
			"max_pending_messages": conversation.MaxPendingMessages,
			"allow_voice_messages": allowVoiceMessages,
			// A'nın B için sesli mesaj iznini kabul edip etmediği (toggle durumu).
			"voice_permission_for_other": voicePermissionForOther,
			"show_read_receipts":         showReadReceipts,
			"blocked":                    conversation.Blocked || len(userBlocks) > 0,
			"blocked_by_me":              blockedByMe,
			// Admin (Filament) bloku info — client vaxtı + cəza ilə açılma
			// düyməsini göstərmək üçün. Yalnız admin bloku üçün mənalı.
			"is_admin_block":               conversation.Blocked,
			"blocked_until":                conversation.BlockedUntil,
			"penalty_unlock_enabled":       conversation.Blocked && conversation.PenaltyUnlockEnabled,
			"unlock_coin_price":            conversation.UnlockCoinPrice,
			"unlock_money_price":           conversation.UnlockMoneyPrice,
			"unlock_revenuecat_product_id": conversation.UnlockRevenueCatProductID,
		},
	}

	c.JSON(http.StatusOK, responseData)
}

// buildConversationResponse kullanıcıya göre response oluştur
func (h *ConversationHandler) buildConversationResponse(conv *models.Conversation, currentUserID uint) models.ConversationResponse {
	var otherUserID uint
	var myMessageCount, otherMessageCount int
	var isMutedByMe, isRestrictedForMe bool
	var myScreenshotDisabled, otherScreenshotDisabled bool // ✅ YENİ

	if currentUserID == conv.User1ID {
		otherUserID = conv.User2ID
		myMessageCount = conv.User1MessageCount
		otherMessageCount = conv.User2MessageCount
		isMutedByMe = conv.User1Muted
		isRestrictedForMe = conv.User1Restricted
		myScreenshotDisabled = conv.User1ScreenshotDisabled    // ✅ YENİ
		otherScreenshotDisabled = conv.User2ScreenshotDisabled // ✅ YENİ
	} else {
		otherUserID = conv.User1ID
		myMessageCount = conv.User2MessageCount
		otherMessageCount = conv.User1MessageCount
		isMutedByMe = conv.User2Muted
		isRestrictedForMe = conv.User2Restricted
		myScreenshotDisabled = conv.User2ScreenshotDisabled    // ✅ YENİ
		otherScreenshotDisabled = conv.User1ScreenshotDisabled // ✅ YENİ
	}

	canSend, _, _ := h.CanSendMessage(currentUserID, otherUserID)

	return models.ConversationResponse{
		ID:                      conv.ID,
		OtherUserID:             otherUserID,
		Status:                  conv.Status,
		Type:                    conv.Type,
		MyMessageCount:          myMessageCount,
		OtherMessageCount:       otherMessageCount,
		IsMutedByMe:             isMutedByMe,
		IsRestrictedForMe:       isRestrictedForMe,
		CanSendMessage:          canSend,
		MaxPendingMessages:      conv.MaxPendingMessages,
		HasPreviousConversation: conv.HasPreviousConversation,
		LastMessageAt:           conv.LastMessageAt,

		// ✅ YENİ: Screenshot bilgileri
		IsScreenshotDisabled:    myScreenshotDisabled || otherScreenshotDisabled,
		MyScreenshotDisabled:    myScreenshotDisabled,
		OtherScreenshotDisabled: otherScreenshotDisabled,

		CreatedAt: conv.CreatedAt,
	}
}

// StringPtr string pointer helper
func StringPtr(s string) *string {
	return &s
}

// GetPendingRequests bekleyen istekleri getir
func (h *ConversationHandler) GetPendingRequests(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	// Issue 57: LIMIT-siz idi — minlərlə pending isteği olan hesab (spam
	// hədəfi) hər çağırışda bütün sətirləri çəkib deşifrə etdirirdi.
	// GERİYƏ UYĞUNLUQ: parametr göndərməyən köhnə istemçi işləyir (default 50)
	// və `has_more` ilə sonradan səhifələyə bilər. Mövcud sahələr dəyişmir.
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	// Həqiqi clamp: 1-dən kiçik/yararsız dəyər → default 50; tavandan böyük
	// dəyər tavana ENDİRİLİR (səssizcə 50-yə düşmür).
	if limit < 1 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	if offset > 100000 {
		offset = 100000
	}

	var requests []struct {
		ConversationID    uint      `json:"conversation_id"`
		RequesterID       uint      `json:"requester_id"`
		RequesterName     string    `json:"requester_name"`
		RequesterUsername string    `json:"requester_username"`
		ProfileImage      *string   `json:"profile_image"`
		MessageCount      int       `json:"message_count"`
		LastMessageText   string    `json:"last_message_text"`
		LastMessageTime   time.Time `json:"last_message_time"`
		CreatedAt         time.Time `json:"created_at"`
	}

	query := `
        SELECT 
            c.id as conversation_id,
            CASE 
                WHEN c.user1_id = ? THEN c.user2_id 
                ELSE c.user1_id 
            END as requester_id,
            u.name as requester_name,
            u.username as requester_username,
            p.profile_image,
            CASE 
                WHEN c.user1_id = ? THEN c.user2_message_count 
                ELSE c.user1_message_count 
            END as message_count,
            '' as last_message_text,
            COALESCE(c.last_message_at, c.created_at) as last_message_time,
            c.created_at
        FROM conversations c
        JOIN users u ON u.id = CASE WHEN c.user1_id = ? THEN c.user2_id ELSE c.user1_id END
        LEFT JOIN profiles p ON p.user_id = u.id
        WHERE (c.user1_id = ? OR c.user2_id = ?)
        AND c.status = 'pending'
        AND CASE 
            WHEN c.user1_id = ? THEN c.user2_message_count > 0 
            ELSE c.user1_message_count > 0 
        END
        -- LIMIT/OFFSET ilə sıralama STABİL olmalıdır: last_message_at unikal
        -- deyil (və NULL-lar DESC-də ÖNDƏ gəlir). Unikal tiebreaker olmadan
        -- eyni açara sahib sətirlər iki səhifədə görünə və ya heç birində
        -- görünməyə bilər.
        ORDER BY c.last_message_at DESC, c.id DESC
        LIMIT ? OFFSET ?
    `

	// Issue 57: `limit+1` — əlavə sətir "daha var" siqnalıdır (ayrıca COUNT yox).
	err := database.DB.Raw(query, userID, userID, userID, userID, userID, userID, limit+1, offset).Scan(&requests).Error
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "İstekler alınamadı"})
		return
	}

	hasMore := len(requests) > limit
	if hasMore {
		requests = requests[:limit]
	}

	// Son mesajları al.
	// N+1 DÜZƏLİŞİ: əvvəllər döngü içində hər requester üçün ayrıca son-mesaj
	// sorğusu gedirdi (N istək = N sorğu). İndi bütün requester-lərin son
	// mesajını BİR sorğuda (DISTINCT ON) çəkirik. Davranış birebir eyni:
	// hər (requester_id -> userID) cütü üçün is_deleted_by_receiver=false olan
	// ən son mesaj. Response sahələri dəyişmir.
	if len(requests) > 0 {
		requesterIDs := make([]uint, len(requests))
		for i := range requests {
			requesterIDs[i] = requests[i].RequesterID
		}

		type lastMsgRow struct {
			SenderID      uint   `gorm:"column:sender_id"`
			EncryptedText string `gorm:"column:encrypted_text"`
		}
		var lastRows []lastMsgRow
		database.DB.Raw(`
            SELECT DISTINCT ON (sender_id) sender_id, encrypted_text
            FROM messages
            WHERE sender_id IN ? AND receiver_id = ?
            AND is_deleted_by_receiver = false
            ORDER BY sender_id, created_at DESC
        `, requesterIDs, userID).Scan(&lastRows)

		lastBySender := make(map[uint]string, len(lastRows))
		for _, r := range lastRows {
			lastBySender[r.SenderID] = r.EncryptedText
		}

		for i := range requests {
			if enc, ok := lastBySender[requests[i].RequesterID]; ok && enc != "" {
				if decrypted, err := h.encryptionService.DecryptMessage(enc); err == nil {
					requests[i].LastMessageText = decrypted
				}
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"requests": requests,
		"count":    len(requests),
		// Issue 57 (additiv sahələr — köhnə istemçilər görməzdən gəlir).
		"limit":    limit,
		"offset":   offset,
		"has_more": hasMore,
	})
}

// GetPendingRequestCount bekleyen istek sayısı
func (h *ConversationHandler) GetPendingRequestCount(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	var count int64

	query := `
        SELECT COUNT(c.id) 
        FROM conversations c
        INNER JOIN users u ON u.id = CASE 
            WHEN c.user1_id = ? THEN c.user2_id 
            ELSE c.user1_id 
        END
        WHERE (c.user1_id = ? OR c.user2_id = ?)
        AND c.status = 'pending'
        AND CASE 
            WHEN c.user1_id = ? THEN c.user2_message_count > 0 
            ELSE c.user1_message_count > 0 
        END
        AND u.deleted_at IS NULL 
        AND u.deactivated_at IS NULL
        AND EXISTS (
            SELECT 1 FROM messages m
            WHERE m.sender_id = CASE WHEN c.user1_id = ? THEN c.user2_id ELSE c.user1_id END
            AND m.receiver_id = ?
            AND m.is_deleted_by_sender = false
            AND m.is_deleted_by_receiver = false
        )
    `

	database.DB.Raw(query, userID, userID, userID, userID, userID, userID).Scan(&count)

	c.JSON(http.StatusOK, gin.H{
		"pending_requests_count": count,
	})
}

// AcceptConversationRequest conversation isteğini kabul et
func (h *ConversationHandler) AcceptConversationRequest(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	requesterID, err := strconv.ParseUint(c.Param("requester_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz requester ID"})
		return
	}

	conversation, err := h.GetOrCreateConversation(userID.(uint), uint(requesterID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Conversation bulunamadı"})
		return
	}

	if conversation.Status != "pending" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Bu conversation zaten kabul edilmiş"})
		return
	}

	// Conversation'ı active yap
	now := time.Now()
	conversation.Status = "active"
	conversation.HasPreviousConversation = true
	conversation.StatusChangedAt = &now

	if err := database.DB.Save(conversation).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "İstek kabul edilemedi"})
		return
	}

	// WebSocket bildirimi gönder
	h.wsHub.SendToUser(uint(requesterID), "conversation_accepted", map[string]interface{}{
		"conversation_id": conversation.ID,
		"accepted_by":     userID,
		"accepted_at":     now,
	})

	c.JSON(http.StatusOK, gin.H{
		"message": "Conversation isteği kabul edildi",
		"data": gin.H{
			"conversation_id": conversation.ID,
			"status":          conversation.Status,
			"accepted_at":     now,
		},
	})
}

// RejectConversationRequest conversation isteğini reddet
func (h *ConversationHandler) RejectConversationRequest(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	requesterID, err := strconv.ParseUint(c.Param("requester_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz requester ID"})
		return
	}

	conversation, err := h.GetOrCreateConversation(userID.(uint), uint(requesterID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Conversation bulunamadı"})
		return
	}

	if conversation.Status != "pending" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Bu conversation zaten işlenmiş"})
		return
	}

	// Conversation'ı restricted yap
	now := time.Now()
	conversation.Status = "restricted"
	conversation.StatusChangedAt = &now
	conversation.RestrictionReason = StringPtr("İstek reddedildi")

	if err := database.DB.Save(conversation).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "İstek reddedilemedi"})
		return
	}

	// WebSocket bildirimi gönder
	h.wsHub.SendToUser(uint(requesterID), "conversation_rejected", map[string]interface{}{
		"conversation_id": conversation.ID,
		"rejected_by":     userID,
		"rejected_at":     now,
	})

	c.JSON(http.StatusOK, gin.H{
		"message": "Conversation isteği reddedildi",
		"data": gin.H{
			"conversation_id": conversation.ID,
			"status":          conversation.Status,
			"rejected_at":     now,
		},
	})
}

// ToggleScreenshotProtection - Screenshot korumayı aç/kapat
func (h *ConversationHandler) ToggleScreenshotProtection(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	otherUserID, err := strconv.ParseUint(c.Param("other_user_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz kullanıcı ID"})
		return
	}

	var requestBody struct {
		Enabled bool `json:"enabled"` // true = screenshot kapalı, false = screenshot açık
	}

	if err := c.ShouldBindJSON(&requestBody); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz request body"})
		return
	}

	conversation, err := h.GetOrCreateConversation(userID.(uint), uint(otherUserID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Conversation bulunamadı"})
		return
	}

	now := time.Now()

	// Hangi kullanıcı değiştiriyor?
	if userID.(uint) == conversation.User1ID {
		conversation.User1ScreenshotDisabled = requestBody.Enabled
		if requestBody.Enabled {
			conversation.User1ScreenshotDisabledAt = &now
		} else {
			conversation.User1ScreenshotDisabledAt = nil
		}
	} else {
		conversation.User2ScreenshotDisabled = requestBody.Enabled
		if requestBody.Enabled {
			conversation.User2ScreenshotDisabledAt = &now
		} else {
			conversation.User2ScreenshotDisabledAt = nil
		}
	}

	if err := database.DB.Save(conversation).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Screenshot ayarı değiştirilemedi"})
		return
	}

	// ✅ Her iki taraftan biri de disable ettiyse true
	bothDisabled := conversation.User1ScreenshotDisabled || conversation.User2ScreenshotDisabled

	// ✅ WebSocket üzerinden HER İKİ kullanıcıya da bildir
	// wsHub interface'ini WebSocketHub'a cast et
	if wsHubTyped, ok := h.wsHub.(interface {
		BroadcastScreenshotProtectionChange(user1ID, user2ID uint, isDisabled bool, changedByUserID uint)
	}); ok {
		wsHubTyped.BroadcastScreenshotProtectionChange(
			conversation.User1ID,
			conversation.User2ID,
			bothDisabled,
			userID.(uint),
		)
	} else {
		// Fallback - eski yöntem (sadece karşı tarafa gönder)
		h.wsHub.SendToUser(uint(otherUserID), "screenshot_protection_changed", map[string]interface{}{
			"conversation_id":        conversation.ID,
			"changed_by":             userID,
			"is_screenshot_disabled": bothDisabled,
			"changed_at":             now,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"message":                "Screenshot ayarı güncellendi",
		"my_screenshot_disabled": requestBody.Enabled,
		"is_screenshot_disabled": bothDisabled, // Genel durum
	})
}

// GetScreenshotProtectionStatus - Screenshot durumunu getir
func (h *ConversationHandler) GetScreenshotProtectionStatus(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	otherUserID, err := strconv.ParseUint(c.Param("other_user_id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz kullanıcı ID"})
		return
	}

	conversation, err := h.GetOrCreateConversation(userID.(uint), uint(otherUserID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Conversation bulunamadı"})
		return
	}

	var myDisabled, otherDisabled bool

	if userID.(uint) == conversation.User1ID {
		myDisabled = conversation.User1ScreenshotDisabled
		otherDisabled = conversation.User2ScreenshotDisabled
	} else {
		myDisabled = conversation.User2ScreenshotDisabled
		otherDisabled = conversation.User1ScreenshotDisabled
	}

	c.JSON(http.StatusOK, gin.H{
		"my_screenshot_disabled":    myDisabled,
		"other_screenshot_disabled": otherDisabled,
		"is_screenshot_disabled":    myDisabled || otherDisabled, // Her iki taraftan biri disable ettiyse true
	})
}
