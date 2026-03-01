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
	// Özel mesajlaşma soketi
	router.GET("/ws", middleware.JWTMiddlewareForWebSocket(cfg.JWTSecret), wsHub.HandleWebSocket)

	// Canlı yayın odaları soketi (DÜZELTİLDİ: Artık doğrudan liveHub kullanılıyor)
	router.GET("/ws/live", middleware.JWTMiddlewareForWebSocket(cfg.JWTSecret), liveHub.HandleWebSocket)

	// Mesajlaşma API route'ları (JWT korumalı)
	api := router.Group("/api/v1")
	api.Use(middleware.JWTMiddleware(cfg.JWTSecret))
	{
		// Mesaj operasyonları
		api.POST("/messages", messageHandler.SendMessage)
		api.GET("/messages/:user_id", messageHandler.GetMessages)
		api.PUT("/messages/:message_id/read", messageHandler.MarkAsRead)
		api.DELETE("/messages/:message_id", messageHandler.DeleteMessage)

		api.DELETE("/conversations/:other_user_id/clear", messageHandler.ClearConversation)
		api.DELETE("/conversations/clear-all", messageHandler.ClearAllMyMessages)

		// Sohbet operasyonları
		api.GET("/conversations", messageHandler.GetConversations)
		api.GET("/unread-count", messageHandler.GetUnreadCount)

		// Conversation request yönetimi
		api.GET("/conversation-requests", conversationHandler.GetPendingRequests)
		api.GET("/conversation-requests/count", conversationHandler.GetPendingRequestCount)
		api.POST("/conversation-requests/:requester_id/accept", conversationHandler.AcceptConversationRequest)
		api.POST("/conversation-requests/:requester_id/reject", conversationHandler.RejectConversationRequest)

		// Conversation yönetimi
		api.GET("/conversations/:user_id/details", conversationHandler.GetConversationDetails)
		api.POST("/conversations/:user_id/mute", conversationHandler.MuteConversation)
		api.POST("/conversations/:user_id/unmute", conversationHandler.UnmuteConversation)
		api.POST("/conversations/:user_id/screenshot-protection", conversationHandler.ToggleScreenshotProtection)
		api.GET("/conversations/:user_id/screenshot-protection", conversationHandler.GetScreenshotProtectionStatus)

		// WebSocket bilgi endpoint'leri
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
		c.JSON(http.StatusOK, gin.H{
			"message": "pong running",
		})
	})

	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status":    "healthy",
			"database":  "connected",
			"websocket": "running",
		})
	})

	// WebSocket status endpoint
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
				"pong",
			},
		})
	})

	// HTTP sunucusunu başlat
	log.Printf("Sunucu %s portunda başlatılıyor...", cfg.Port)
	log.Println("🚀 API Endpoints:")
	log.Println("  POST /api/v1/messages - Mesaj gönder")
	log.Println("  GET  /api/v1/messages/:user_id - Mesajları getir")
	log.Println("  PUT  /api/v1/messages/:message_id/read - Okundu işaretle")
	log.Println("  DELETE /api/v1/messages/:message_id - Mesajı sil")
	log.Println("  GET  /api/v1/conversations - Sohbet listesi")
	log.Println("  GET  /api/v1/unread-count - Okunmamış mesaj sayısı")
	log.Println("  GET  /api/v1/online-users - Online kullanıcılar")
	log.Println()
	log.Println("🔌 WebSocket Bağlantıları:")
	log.Println("  GET  /ws?token=JWT_TOKEN - Özel Chat WebSocket'i")
	log.Println("  GET  /ws/live?room_id=ID&token=JWT_TOKEN - Canlı Yayın WebSocket'i")
	log.Println("  GET  /ws-status - WebSocket durumu")
	log.Println()
	log.Println("📚 Dokümantasyon:")
	log.Println("  GET  /api-docs - API dokümantasyonu")
	log.Println("  GET  /health - Sistem durumu")

	if err := router.Run(":" + cfg.Port); err != nil {
		log.Fatalf("Sunucu başlatma hatası: %v", err)
	}
}
