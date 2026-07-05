package main

import (
	"beanpon_messenger/cache"
	"beanpon_messenger/config"
	"beanpon_messenger/database"
	"beanpon_messenger/handlers"
	"beanpon_messenger/middleware"
	"beanpon_messenger/services"
	"beanpon_messenger/utils"
	"beanpon_messenger/websocket"
	"beanpon_messenger/xmpp"
	"github.com/Depado/ginprom"
	"github.com/gin-gonic/gin"
	"log"
	"net/http"
	"strconv"
	"time"
)

func main() {
	// 🕒 Global timezone UTC olarak set et
	time.Local = time.UTC
	log.Printf("✅ Global timezone UTC olarak ayarlandı")

	// Konfigürasyonu yükle
	cfg := config.LoadConfig()

	// Media full URL prefiksi (Laravel APP_URL) — filePathS3 üçün.
	utils.SetBaseURL(cfg.AppURL)

	// GORM ile PostgreSQL'e bağlan
	database.InitializePostgreSQL(cfg)

	// Bandwidth tracking — async writer (request axınını bloklamır).
	go middleware.StartBandwidthWriter(database.GetDB())

	// 🚀 Redis cache layer — Laravel ilə paylaşılan spam_ban statusu üçün.
	// REDIS_ENABLED=false olsa belə tətbiq işləyir (cache çağırışları no-op
	// olur, DB-yə düşür). Boot dayanmır.
	cache.Initialize(cfg.Cache)

	// ✅ Conversations cədvəlinə reaksiya sütunlarını əlavə et (idempotent)
	if err := database.DB.Exec(`
		ALTER TABLE conversations
		ADD COLUMN IF NOT EXISTS last_reaction_emoji VARCHAR(16),
		ADD COLUMN IF NOT EXISTS last_reaction_at TIMESTAMPTZ,
		ADD COLUMN IF NOT EXISTS last_reaction_by_user_id BIGINT
	`).Error; err != nil {
		log.Printf("⚠️ conversations reaction sütunları əlavə edilə bilmədi: %v", err)
	} else {
		log.Printf("✅ conversations reaction sütunları hazırdır")
	}

	// ✅ Conversations cədvəlinə arxiv sütunlarını əlavə et (idempotent, per-user)
	if err := database.DB.Exec(`
		ALTER TABLE conversations
		ADD COLUMN IF NOT EXISTS user1_archived BOOLEAN NOT NULL DEFAULT FALSE,
		ADD COLUMN IF NOT EXISTS user2_archived BOOLEAN NOT NULL DEFAULT FALSE,
		ADD COLUMN IF NOT EXISTS user1_archived_at TIMESTAMPTZ,
		ADD COLUMN IF NOT EXISTS user2_archived_at TIMESTAMPTZ
	`).Error; err != nil {
		log.Printf("⚠️ conversations arxiv sütunları əlavə edilə bilmədi: %v", err)
	} else {
		log.Printf("✅ conversations arxiv sütunları hazırdır")
	}

	// ✅ Conversations cədvəlinə pin (sabitləmə) sütunlarını əlavə et
	// (idempotent, per-user). Pin olunmuş söhbət istifadəçinin siyahısında ən
	// yuxarı gəlir; bir neçə söhbət pin oluna bilər, pinned_at-a görə sıralanır.
	if err := database.DB.Exec(`
		ALTER TABLE conversations
		ADD COLUMN IF NOT EXISTS user1_pinned BOOLEAN NOT NULL DEFAULT FALSE,
		ADD COLUMN IF NOT EXISTS user2_pinned BOOLEAN NOT NULL DEFAULT FALSE,
		ADD COLUMN IF NOT EXISTS user1_pinned_at TIMESTAMPTZ,
		ADD COLUMN IF NOT EXISTS user2_pinned_at TIMESTAMPTZ
	`).Error; err != nil {
		log.Printf("⚠️ conversations pin sütunları əlavə edilə bilmədi: %v", err)
	} else {
		log.Printf("✅ conversations pin sütunları hazırdır")
	}

	// ✅ Conversations cədvəlinə nickname (ləqəb) sütunları — per-user,
	// birtərəfli. user1_nickname = user1-in user2 üçün qoyduğu ad (yalnız
	// user1 görür). idempotent.
	if err := database.DB.Exec(`
		ALTER TABLE conversations
		ADD COLUMN IF NOT EXISTS user1_nickname VARCHAR(60),
		ADD COLUMN IF NOT EXISTS user2_nickname VARCHAR(60)
	`).Error; err != nil {
		log.Printf("⚠️ conversations nickname sütunları əlavə edilə bilmədi: %v", err)
	} else {
		log.Printf("✅ conversations nickname sütunları hazırdır")
	}

	// ✅ Conversations cədvəlinə per-conversation çat fonu (wallpaper) seçimi.
	// user1_wallpaper_id = user1-in BU söhbət üçün seçdiyi Laravel wallpaper ID.
	// NULL → qlobal seçim (user_settings), o da NULL → default. idempotent.
	if err := database.DB.Exec(`
		ALTER TABLE conversations
		ADD COLUMN IF NOT EXISTS user1_wallpaper_id BIGINT,
		ADD COLUMN IF NOT EXISTS user2_wallpaper_id BIGINT
	`).Error; err != nil {
		log.Printf("⚠️ conversations wallpaper sütunları əlavə edilə bilmədi: %v", err)
	} else {
		log.Printf("✅ conversations wallpaper sütunları hazırdır")
	}

	// ✅ Qrup üzv limiti: 5000 (yaradan daxil). Boot-da KÖHNƏ limitli
	// (50/150/256/500) qruplar 5000-ə qaldırılır. İdempotent (yalnız 5000-dən
	// fərqli olanlar dəyişir).
	if err := database.DB.Exec(`
		UPDATE conversations SET max_members = 5000
		WHERE chat_type = 'group' AND max_members <> 5000
	`).Error; err != nil {
		log.Printf("⚠️ qrup max_members 5000-ə qaldırıla bilmədi: %v", err)
	} else {
		log.Printf("✅ qrup max_members limiti 5000")
	}

	// ✅ Messages cədvəlinə ulduzlu mesaj (star) sütunları — per-user.
	// starred_by_sender/receiver. idempotent.
	if err := database.DB.Exec(`
		ALTER TABLE messages
		ADD COLUMN IF NOT EXISTS starred_by_sender BOOLEAN NOT NULL DEFAULT FALSE,
		ADD COLUMN IF NOT EXISTS starred_by_receiver BOOLEAN NOT NULL DEFAULT FALSE
	`).Error; err != nil {
		log.Printf("⚠️ messages star sütunları əlavə edilə bilmədi: %v", err)
	} else {
		log.Printf("✅ messages star sütunları hazırdır")
	}

	// Servisleri başlat
	encryptionService := services.NewEncryptionService(cfg.AESKey)

	// 1. MEVCUT SİSTEM: Özel Mesajlaşma (Chat) Hub'ı
	wsHub := websocket.NewHub(database.DB, encryptionService, cfg)
	database.DB.Exec("UPDATE user_presences SET is_online = false, last_seen_at = NOW() WHERE is_online = true")
	go wsHub.Run()

	// 1b. XMPP BRIDGE (transport migration for chat_page / group_chat_page).
	// No-op unless XMPP_ENABLED=true. The Hub implements both xmpp.LegacyDelivery
	// and xmpp.IngressSink (see websocket/hub_xmpp.go). When enabled, the Hub's
	// egress seam routes messages to NEW (XMPP) recipients while OLD recipients
	// keep the legacy WS path. See xmpp/WIRING.md and xmpp/CLAUDE.md.
	xmppCfg := xmpp.LoadConfig()
	xmppReg := xmpp.NewRegistry(2 * time.Minute)
	xmppBridge := xmpp.NewBridge(xmppCfg, xmppReg, wsHub, wsHub)
	wsHub.AttachXMPP(xmppBridge)
	xmppBridge.Start() // no-op when disabled
	if xmppCfg.Enabled {
		log.Printf("✅ XMPP bridge enabled (component=%s domain=%s)", xmppCfg.ComponentName, xmppCfg.Domain)
	} else {
		log.Printf("ℹ️  XMPP bridge disabled (XMPP_ENABLED=false) — legacy WebSocket only")
	}

	// 🔍 MODERASIYA SİSTEMİ
	// Mesajlar şifrələnmədən əvvəl arxa planda AI (OpenAI gpt-4o-mini) ilə
	// analiz edilir. Mesaj göndərmə HEÇ bloklanmır — analiz queue + worker
	// pool ilə tamamilə asinxron baş verir. Risk aşkar edilərsə nəticə
	// message_moderation_logs tablosuna yazılır; off_platform kateqoriyasında
	// mesajı YAZAN adama xəbərdarlıq notification-ı gedir.
	moderationAI := services.NewModerationAIService(cfg.OpenAIAPIKey)
	moderationQueue := services.NewModerationQueue(moderationAI, database.DB, cfg, wsHub)
	moderationQueue.Start()
	// WS axını üçün enqueue callback-ini hub-a bağla.
	wsHub.SetModerationEnqueue(func(messageID string, senderID, receiverID uint, plainText string, createdAt time.Time) {
		moderationQueue.Enqueue(services.ModerationJob{
			MessageID:  messageID,
			SenderID:   senderID,
			ReceiverID: receiverID,
			PlainText:  plainText,
			CreatedAt:  createdAt,
		})
	})

	// 2. YENİ SİSTEM: Canlı Yayın Odaları (Live) Hub'ı
	liveHub := websocket.NewLiveHub()
	go liveHub.Run()
	go liveHub.RunMafiaTimer() // Mafia oyunu mərhələ taymeri

	// 3. RAVE Hub'ı
	raveHub := websocket.NewRaveHub()
	go raveHub.Run()

	// Handler'ları oluştur
	messageHandler := handlers.NewMessageHandler(encryptionService, wsHub)
	// HTTP axını üçün moderasiya queue-sunu handler-a bağla.
	messageHandler.SetModerationQueue(moderationQueue)
	conversationHandler := handlers.NewConversationHandler(wsHub, encryptionService)
	groupHandler := handlers.NewGroupHandler(wsHub, encryptionService)
	groupMsgHandler := handlers.NewGroupMessageHandler(encryptionService, wsHub)
	raveHandler := handlers.NewRaveHandler(raveHub) // ← YENİ

	// Voice/media upload — Laravel MessageController::uploadVoice/uploadMedia portu.
	s3Uploader := services.NewS3Uploader(cfg.S3.Bucket, cfg.S3.Region, cfg.S3.Endpoint, cfg.S3.Key, cfg.S3.Secret, cfg.S3.PathStyle)
	uploadHandler := handlers.NewUploadHandler(s3Uploader)

	// Gin router'ını oluştur
	router := gin.Default()

	p := ginprom.New(
		ginprom.Engine(router),
		ginprom.Namespace("beanpon"),
		ginprom.Subsystem("messenger"),
	)
	router.Use(p.Instrument())

	// CORS middleware
	router.Use(func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
		c.Header("Access-Control-Allow-Headers", "authorization,content-type")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}

		c.Next()
	})

	// Bandwidth tracking — WS xaric bütün route-ları ölçür, response-u
	// gözlətmir (qeydi async kanala atır).
	router.Use(middleware.BandwidthMiddleware())

	// 🔌 WEBSOCKET ENDPOINT'LERİ
	router.GET("/ws", middleware.JWTMiddlewareForWebSocket(cfg.JWTSecret), wsHub.HandleWebSocket)
	router.GET("/ws/live", middleware.JWTMiddlewareForWebSocket(cfg.JWTSecret), liveHub.HandleWebSocket)
	router.GET("/ws/rave", middleware.JWTMiddlewareForWebSocket(cfg.JWTSecret), raveHandler.HandleWebSocket) // ← YENİ

	// Mesajlaşma API route'ları (JWT korumalı)
	api := router.Group("/api/v1")
	api.Use(middleware.JWTMiddleware(cfg.JWTSecret))
	{
		// ── Voice/media upload (Laravel messenger/upload-*) ──────────────
		api.POST("/messenger/upload-voice", uploadHandler.UploadVoice)
		api.POST("/messenger/upload-media", uploadHandler.UploadMedia)

		// ── DM: Mesaj operasyonları ──────────────────────────────────────
		api.POST("/messages", messageHandler.SendMessage)
		api.POST("/messages/broadcast", messageHandler.BroadcastMessage)
		api.GET("/messages/:user_id", messageHandler.GetMessages)
		// In-chat DM search (WhatsApp-style) — additive, only new clients call it.
		api.GET("/messages/:user_id/search", messageHandler.SearchMessages)
		api.PUT("/messages/:message_id/read", messageHandler.MarkAsRead)
		api.PUT("/messages/:message_id", messageHandler.EditMessage)
		api.DELETE("/messages/:message_id", messageHandler.DeleteMessage)
		api.POST("/messages/:message_id/star", messageHandler.ToggleStar)
		// ① "Bir dəfə bax" media açıldı (DM) — opened_by JSON-a yazılır,
		// message_edited WS event-i ilə hər iki tərəfə yayılır.
		api.POST("/messages/:message_id/view-once-opened", messageHandler.MarkViewOnceOpened)

		// Batch mark-as-read — bir söhbətdəki bütün okunmamış mesajları
		// (qarşı tərəfdən gələnləri) eyni anda okundu işarələ. Native
		// inline reply (Quick Reply) bunu chağırır.
		api.POST("/conversations/:other_user_id/mark-read", messageHandler.MarkConversationAsRead)

		// Delta sync — reconnect zamanı buraxılmış DM dəyişikliklərini doldur
		// (yeni mesaj / edit / read-delivered / silinmə). Yalnız yeni client-lər
		// çağırır; köhnə client-lərə təsiri yoxdur (tam additiv).
		api.GET("/messages/sync", messageHandler.SyncMessages)

		api.DELETE("/conversations/:other_user_id/clear", messageHandler.ClearConversation)
		api.DELETE("/conversations/clear-all", messageHandler.ClearAllMyMessages)

		// ── XMPP: istemci giriş token'ı ──────────────────────────────────
		// İstemci (yeni sürüm) XMPP'ye bağlanmadan önce buradan jid'li
		// kısa-ömürlü token alır. ejabberd yerleşik JWT auth'u doğrular.
		api.POST("/xmpp/token", xmppBridge.TokenHandler(cfg.JWTSecret))

		// ── DM: Sohbet operasyonları ─────────────────────────────────────
		api.GET("/conversations", messageHandler.GetConversations)
		api.GET("/unread-count", messageHandler.GetUnreadCount)
		// Share modal — recency+frequency-əsaslı tövsiyə list (wave/post share).
		api.GET("/conversations/share-recipients", messageHandler.GetShareRecipients)

		// ── DM: Conversation request yönetimi ───────────────────────────
		api.GET("/conversation-requests", conversationHandler.GetPendingRequests)
		api.GET("/conversation-requests/count", conversationHandler.GetPendingRequestCount)
		api.POST("/conversation-requests/:requester_id/accept", conversationHandler.AcceptConversationRequest)
		api.POST("/conversation-requests/:requester_id/reject", conversationHandler.RejectConversationRequest)

		// ── DM: Conversation yönetimi ────────────────────────────────────
		api.GET("/conversations/:other_user_id/details", conversationHandler.GetConversationDetails)
		api.POST("/conversations/:other_user_id/archive", conversationHandler.ArchiveConversation)
		api.POST("/conversations/:other_user_id/unarchive", conversationHandler.UnarchiveConversation)
		api.POST("/conversations/:other_user_id/pin", conversationHandler.PinConversation)
		api.POST("/conversations/:other_user_id/unpin", conversationHandler.UnpinConversation)
		api.POST("/conversations/:other_user_id/nickname", conversationHandler.SetNickname)
		api.DELETE("/conversations/:other_user_id/nickname", conversationHandler.ClearNickname)
		api.POST("/conversations/:other_user_id/wallpaper", conversationHandler.SetWallpaper)
		api.DELETE("/conversations/:other_user_id/wallpaper", conversationHandler.ClearWallpaper)
		api.GET("/conversations/:other_user_id/starred", messageHandler.GetStarredMessages)
		api.POST("/conversations/:other_user_id/mute", conversationHandler.MuteConversation)

		api.POST("/conversations/:other_user_id/unmute", conversationHandler.UnmuteConversation)
		api.POST("/conversations/:other_user_id/screenshot-protection", conversationHandler.ToggleScreenshotProtection)
		api.GET("/conversations/:other_user_id/screenshot-protection", conversationHandler.GetScreenshotProtectionStatus)

		// ── GROUP: Grup yönetimi ─────────────────────────────────────────
		api.POST("/groups", groupHandler.CreateGroup)
		api.GET("/groups", groupHandler.GetMyGroups)
		api.POST("/groups/join/:token", groupHandler.JoinByToken)
		// Dəvət linki önizləməsi — qoşulmazdan əvvəl qrup adı/avatar/üzvlər.
		api.GET("/groups/join/:token/preview", groupHandler.PreviewByToken)
		// Pending dəvəti qəbul et / rədd et (üzv əlavə etmə onay axını).
		api.POST("/groups/:conversation_id/invite/accept", groupHandler.AcceptInvite)
		api.POST("/groups/:conversation_id/invite/decline", groupHandler.DeclineInvite)
		api.POST("/groups/:conversation_id/leave", groupHandler.LeaveGroup)
		api.POST("/groups/:conversation_id/kick/:user_id", groupHandler.KickMember)
		api.PUT("/groups/:conversation_id/role/:user_id", groupHandler.ChangeRole)
		api.GET("/groups/:conversation_id/members", groupHandler.GetMembers)
		api.POST("/groups/:conversation_id/invite-token/refresh", groupHandler.RefreshInviteToken)

		api.GET("/groups/:conversation_id", groupHandler.GetGroupDetail)
		api.POST("/groups/:conversation_id/members", groupHandler.AddMembers)
		api.POST("/groups/:conversation_id/mute", groupHandler.MuteGroup)
		api.POST("/groups/:conversation_id/unmute", groupHandler.UnmuteGroup)
		api.POST("/groups/:conversation_id/pin", groupHandler.PinGroup)
		api.POST("/groups/:conversation_id/unpin", groupHandler.UnpinGroup)
		api.POST("/groups/:conversation_id/archive", groupHandler.ArchiveGroup)
		api.POST("/groups/:conversation_id/unarchive", groupHandler.UnarchiveGroup)
		api.POST("/groups/:conversation_id/wallpaper", groupHandler.SetGroupWallpaper)
		api.POST("/groups/:conversation_id/clear", groupHandler.ClearGroupChat)
		api.PUT("/groups/:conversation_id", groupHandler.UpdateGroup)
		// Qrup icazələri (admin: mesaj/media/gif/səs/circle-video aç-bağla)
		api.GET("/groups/:conversation_id/permissions", groupHandler.GetGroupPermissions)
		api.PUT("/groups/:conversation_id/permissions", groupHandler.UpdateGroupPermissions)

		// ── GROUP: Mesajlar ──────────────────────────────────────────────
		api.POST("/groups/:conversation_id/messages", groupMsgHandler.SendGroupMessage)
		api.GET("/groups/:conversation_id/messages", groupMsgHandler.GetGroupMessages)
		// Qrupu REST ilə oxundu işarələ — WS/GetGroupMessages oxundu yolu ilə
		// eyni iş (message_reads + last_read + group_message_read event-i).
		api.POST("/groups/:conversation_id/mark-read", groupMsgHandler.MarkGroupConversationRead)
		api.GET("/groups/:conversation_id/starred", groupMsgHandler.GetGroupStarred)
		api.GET("/groups/messages/:message_id/reads", groupMsgHandler.GetMessageReads)
		api.DELETE("/groups/messages/:message_id", groupMsgHandler.DeleteGroupMessage)
		api.PUT("/groups/messages/:message_id", groupMsgHandler.EditGroupMessage)
		api.POST("/groups/messages/:message_id/star", groupMsgHandler.ToggleGroupStar)
		api.POST("/groups/messages/:message_id/reaction", groupMsgHandler.SetGroupReaction)
		// 📌 Mesaj sabitləmə (yalnız admin; qrupda TƏK pin — yenisi əvəz edir)
		api.POST("/groups/messages/:message_id/pin", groupMsgHandler.PinGroupMessage)
		api.POST("/groups/messages/:message_id/unpin", groupMsgHandler.UnpinGroupMessage)
		// ① "Bir dəfə bax" media açıldı (qrup) — hər üzv bir dəfə;
		// group_message_edited WS event-i ilə bütün üzvlərə yayılır.
		api.POST("/groups/messages/:message_id/view-once-opened", groupMsgHandler.MarkGroupViewOnceOpened)

		// ── Live yayın ───────────────────────────────────────────────────
		api.GET("/live-rooms/:room_id/messages", liveHub.GetLiveRoomMessages)
		api.GET("/live-rooms/:room_id/reactions", liveHub.GetLiveRoomReactions)
		api.POST("/live-rooms/:room_id/ask-question", liveHub.AskQuestion)
		api.POST("/live-rooms/:room_id/question-vote", liveHub.CastQuestionVote)
		api.POST("/live-rooms/:room_id/mafia/start", liveHub.StartMafia)

		// ── WebSocket bilgi endpoint'leri ────────────────────────────────
		api.GET("/online-users", func(c *gin.Context) {
			onlineUsers := wsHub.GetConnectedUsers()
			c.JSON(http.StatusOK, gin.H{
				"online_users": onlineUsers,
				"count":        len(onlineUsers),
			})
		})

		api.GET("/user-online/:user_id", func(c *gin.Context) {
			userID := c.Param("user_id")
			var targetUserID uint64
			if parsedID, err := strconv.ParseUint(userID, 10, 32); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz kullanıcı ID"})
				return
			} else {
				targetUserID = parsedID
			}

			isOnline := wsHub.IsUserOnline(uint(targetUserID))
			c.JSON(http.StatusOK, gin.H{
				"user_id":   uint(targetUserID),
				"is_online": isOnline,
			})
		})
	}

	// Internal routes (Laravel-dən çağrılır)
	internal := router.Group("/internal")
	internal.Use(func(c *gin.Context) {
		secret := c.GetHeader("X-Internal-Secret")
		if secret != cfg.InternalSecret {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
			c.Abort()
			return
		}
		c.Next()
	})
	{
		internal.POST("/live-rooms/:room_id/force-end", liveHub.ForceEndRoom)
		internal.POST("/live-rooms/:room_id/clear-chat", liveHub.ClearChat)
		internal.POST("/live-rooms/:room_id/kick/:user_id", liveHub.KickUser)
		internal.POST("/live-rooms/:room_id/mute/:user_id", liveHub.MuteUser)
		internal.POST("/live-rooms/:room_id/unmute/:user_id", liveHub.UnmuteUser)
		// Shadow ban (live_spam) statusunu real-time sinxronlaşdırır.
		// Body: {"live_spam": true|false}. Filament users.live_spam-ı
		// dəyişdikdən sonra bu endpoint-i çağırmalıdır ki, otaqda
		// olan client-lər dərhal yenilənsin (reconnect gözləmədən).
		internal.POST("/users/:user_id/live-spam", liveHub.SetLiveSpam)

		// 1:1 sesli arama sinyali (Laravel → online callee/caller WS).
		internal.POST("/calls/signal", wsHub.HandleCallSignal)
		// Çağrı bitdikdə conversation-a kalıcı "call" mesajı.
		internal.POST("/calls/message", wsHub.HandleCallMessage)

		// XMPP client auth callback (ejabberd → Go, validates the JWT). Used in
		// PHASE 2 when native XMPP clients connect; harmless when XMPP disabled.
		internal.POST("/xmpp/auth", xmppBridge.AuthHandler(cfg.JWTSecret))
	}

	// Public routes
	router.GET("/ping", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"message": "pong running"})
	})

	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status":    "healthy",
			"database":  "connected",
			"websocket": "running",
		})
	})

	router.GET("/ws-status", func(c *gin.Context) {
		onlineUsers := wsHub.GetConnectedUsers()
		c.JSON(http.StatusOK, gin.H{
			"connected_users": wsHub.GetConnectedUsersCount(),
			"online_user_ids": onlineUsers,
			"status":          "running",
			"websocket_url":   "/ws",
			"live_websocket":  "/ws/live",
			"supported_message_types": []string{
				"ping",
				"typing",
				"typing_stop",
				"recording",
				"recording_stop",
				"get_online_users",
			},
			"broadcasted_message_types": []string{
				"new_message",
				"message_read",
				"message_deleted",
				"user_status",
				"user_typing",
				"history_message",
				"history_loaded",
				"online_users",
				"new_group_message",
				"group_message_read",
				"group_message_deleted",
				"group_message_edited",
				"group_reaction_updated",
				"group_member_joined",
				"group_member_left",
				"group_member_kicked",
				"group_kicked",
				"group_added",
				"group_role_changed",
				"pong",
			},
		})
	})

	log.Printf("Sunucu %s portunda başlatılıyor...", cfg.Port)
	log.Println("🚀 API Endpoints:")
	log.Println("  POST   /api/v1/messages                              - DM mesaj gönder")
	log.Println("  GET    /api/v1/messages/:user_id                     - DM mesajları getir")
	log.Println("  PUT    /api/v1/messages/:message_id/read             - Okundu işaretle")
	log.Println("  DELETE /api/v1/messages/:message_id                  - Mesajı sil")
	log.Println("  GET    /api/v1/conversations                         - DM sohbet listesi")
	log.Println("  GET    /api/v1/unread-count                          - Okunmamış sayısı")
	log.Println("  POST   /api/v1/groups                                - Grup oluştur")
	log.Println("  GET    /api/v1/groups                                - Gruplarım")
	log.Println("  POST   /api/v1/groups/join/:token                    - Token ile katıl")
	log.Println("  POST   /api/v1/groups/:id/leave                      - Gruptan ayrıl")
	log.Println("  POST   /api/v1/groups/:id/kick/:user_id              - Üye çıkar")
	log.Println("  PUT    /api/v1/groups/:id/role/:user_id              - Rol değiştir")
	log.Println("  GET    /api/v1/groups/:id/members                    - Üye listesi")
	log.Println("  POST   /api/v1/groups/:id/invite-token/refresh       - Token yenile")
	log.Println("  POST   /api/v1/groups/:id/messages                   - Grup mesaj gönder")
	log.Println("  GET    /api/v1/groups/:id/messages                   - Grup mesajları")
	log.Println("  GET    /api/v1/groups/messages/:message_id/reads     - Kimler gördü")
	log.Println()
	log.Println("🔌 WebSocket:")
	log.Println("  GET  /ws?token=JWT_TOKEN")
	log.Println("  GET  /ws/live?room_id=ID&token=JWT_TOKEN")

	if err := router.Run(":" + cfg.Port); err != nil {
		log.Fatalf("Sunucu başlatma hatası: %v", err)
	}
}
