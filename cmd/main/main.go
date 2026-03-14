package main

import (
	"beanpon_messenger/config"
	"beanpon_messenger/database"
	"beanpon_messenger/handlers"
	"beanpon_messenger/middleware"
	"beanpon_messenger/services"
	"beanpon_messenger/websocket"
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

	// GORM ile PostgreSQL'e bağlan
	database.InitializePostgreSQL(cfg)

	// Servisleri başlat
	encryptionService := services.NewEncryptionService(cfg.AESKey)

	// 1. MEVCUT SİSTEM: Özel Mesajlaşma (Chat) Hub'ı
	wsHub := websocket.NewHub(database.DB, encryptionService, cfg)
	go wsHub.Run()

	// 2. YENİ SİSTEM: Canlı Yayın Odaları (Live) Hub'ı
	liveHub := websocket.NewLiveHub()
	go liveHub.Run()

	// Handler'ları oluştur
	messageHandler := handlers.NewMessageHandler(encryptionService, wsHub)
	conversationHandler := handlers.NewConversationHandler(wsHub, encryptionService)
	groupHandler := handlers.NewGroupHandler(wsHub)
	groupMsgHandler := handlers.NewGroupMessageHandler(encryptionService, wsHub)

	// Gin router'ını oluştur
	router := gin.Default()

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

	// 🔌 WEBSOCKET ENDPOINT'LERİ
	router.GET("/ws", middleware.JWTMiddlewareForWebSocket(cfg.JWTSecret), wsHub.HandleWebSocket)
	router.GET("/ws/live", middleware.JWTMiddlewareForWebSocket(cfg.JWTSecret), liveHub.HandleWebSocket)

	// Mesajlaşma API route'ları (JWT korumalı)
	api := router.Group("/api/v1")
	api.Use(middleware.JWTMiddleware(cfg.JWTSecret))
	{
		// ── DM: Mesaj operasyonları ──────────────────────────────────────
		api.POST("/messages", messageHandler.SendMessage)
		api.GET("/messages/:user_id", messageHandler.GetMessages)
		api.PUT("/messages/:message_id/read", messageHandler.MarkAsRead)
		api.DELETE("/messages/:message_id", messageHandler.DeleteMessage)

		api.DELETE("/conversations/:other_user_id/clear", messageHandler.ClearConversation)
		api.DELETE("/conversations/clear-all", messageHandler.ClearAllMyMessages)

		// ── DM: Sohbet operasyonları ─────────────────────────────────────
		api.GET("/conversations", messageHandler.GetConversations)
		api.GET("/unread-count", messageHandler.GetUnreadCount)

		// ── DM: Conversation request yönetimi ───────────────────────────
		api.GET("/conversation-requests", conversationHandler.GetPendingRequests)
		api.GET("/conversation-requests/count", conversationHandler.GetPendingRequestCount)
		api.POST("/conversation-requests/:requester_id/accept", conversationHandler.AcceptConversationRequest)
		api.POST("/conversation-requests/:requester_id/reject", conversationHandler.RejectConversationRequest)

		// ── DM: Conversation yönetimi ────────────────────────────────────
		api.GET("/conversations/:user_id/details", conversationHandler.GetConversationDetails)
		api.POST("/conversations/:user_id/mute", conversationHandler.MuteConversation)
		api.POST("/conversations/:user_id/unmute", conversationHandler.UnmuteConversation)
		api.POST("/conversations/:user_id/screenshot-protection", conversationHandler.ToggleScreenshotProtection)
		api.GET("/conversations/:user_id/screenshot-protection", conversationHandler.GetScreenshotProtectionStatus)

		// ── GROUP: Grup yönetimi ─────────────────────────────────────────
		api.POST("/groups", groupHandler.CreateGroup)
		api.GET("/groups", groupHandler.GetMyGroups)
		api.POST("/groups/join/:token", groupHandler.JoinByToken)
		api.POST("/groups/:conversation_id/leave", groupHandler.LeaveGroup)
		api.POST("/groups/:conversation_id/kick/:user_id", groupHandler.KickMember)
		api.PUT("/groups/:conversation_id/role/:user_id", groupHandler.ChangeRole)
		api.GET("/groups/:conversation_id/members", groupHandler.GetMembers)
		api.POST("/groups/:conversation_id/invite-token/refresh", groupHandler.RefreshInviteToken)

		api.GET("/groups/:conversation_id", groupHandler.GetGroupDetail)
		api.POST("/groups/:conversation_id/members", groupHandler.AddMembers)
		api.PUT("/groups/:conversation_id", groupHandler.UpdateGroup)

		// ── GROUP: Mesajlar ──────────────────────────────────────────────
		api.POST("/groups/:conversation_id/messages", groupMsgHandler.SendGroupMessage)
		api.GET("/groups/:conversation_id/messages", groupMsgHandler.GetGroupMessages)
		api.GET("/groups/messages/:message_id/reads", groupMsgHandler.GetMessageReads)

		// ── Live yayın ───────────────────────────────────────────────────
		api.GET("/live-rooms/:room_id/messages", liveHub.GetLiveRoomMessages)
		api.GET("/live-rooms/:room_id/reactions", liveHub.GetLiveRoomReactions)

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
