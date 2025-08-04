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
)

func main() {
	// Konfigürasyonu yükle
	cfg := config.LoadConfig()

	// GORM ile PostgreSQL'e bağlan
	database.InitializePostgreSQL(cfg)

	// Auto migration çalıştır
	//database.AutoMigrate()

	// Servisleri başlat
	encryptionService := services.NewEncryptionService(cfg.AESKey)
	wsHub := websocket.NewHub(database.DB, encryptionService)
	go wsHub.Run()

	// Handler'ları oluştur
	messageHandler := handlers.NewMessageHandler(encryptionService, wsHub)

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

	// WebSocket endpoint (JWT middleware ile)
	router.GET("/ws", middleware.JWTMiddlewareForWebSocket(cfg.JWTSecret), wsHub.HandleWebSocket)

	// Mesajlaşma API route'ları (JWT korumalı)
	api := router.Group("/api/v1")
	api.Use(middleware.JWTMiddleware(cfg.JWTSecret))
	{
		// Mesaj operasyonları
		api.POST("/messages", messageHandler.SendMessage)
		api.GET("/messages/:user_id", messageHandler.GetMessages)
		api.PUT("/messages/:message_id/read", messageHandler.MarkAsRead)
		api.DELETE("/messages/:message_id", messageHandler.DeleteMessage)

		// Sohbet operasyonları
		api.GET("/conversations", messageHandler.GetConversations)
		api.GET("/unread-count", messageHandler.GetUnreadCount)

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
			// String'i uint'e çevir
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

	// Test endpoint (JWT test için)
	router.GET("/test-jwt", middleware.JWTMiddleware(cfg.JWTSecret), func(c *gin.Context) {
		userID, _ := c.Get("user_id")
		userEmail, _ := c.Get("user_email")

		c.JSON(200, gin.H{
			"user_id": userID,
			"email":   userEmail,
			"message": "JWT çalışıyor!",
		})
	})

	// API dokümantasyonu endpoint'i
	router.GET("/api-docs", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"title":   "Beanpon Messenger API",
			"version": "1.0.0",
			"endpoints": gin.H{
				"websocket": gin.H{
					"url":         "/ws?token=JWT_TOKEN",
					"description": "WebSocket bağlantısı",
					"auth":        "JWT Token gerekli",
					"client_messages": []string{
						"ping - Sunucuya ping gönder",
						"typing - Yazıyor durumunu bildir",
						"typing_stop - Yazmayı bıraktı durumunu bildir",
						"get_online_users - Online kullanıcı listesini al",
					},
					"server_messages": []string{
						"new_message - Yeni mesaj bildirimi",
						"message_read - Mesaj okundu bildirimi",
						"message_deleted - Mesaj silindi bildirimi",
						"user_status - Kullanıcı online/offline durumu",
						"user_typing - Kullanıcı yazıyor durumu",
						"history_message - Geçmiş mesaj",
						"history_loaded - Geçmiş yükleme tamamlandı",
						"online_users - Online kullanıcı listesi",
						"pong - Ping'e cevap",
					},
				},
				"api": gin.H{
					"base_url": "/api/v1",
					"auth":     "JWT Bearer Token gerekli",
					"routes": []gin.H{
						{
							"method":      "POST",
							"path":        "/messages",
							"description": "Yeni mesaj gönder",
							"body": gin.H{
								"receiver_id": "uint (zorunlu)",
								"text":        "string (zorunlu)",
							},
						},
						{
							"method":      "GET",
							"path":        "/messages/:user_id",
							"description": "Belirli kullanıcı ile mesajları getir",
							"params": gin.H{
								"user_id": "uint (zorunlu)",
								"page":    "int (opsiyonel, varsayılan: 1)",
								"limit":   "int (opsiyonel, varsayılan: 50)",
							},
						},
						{
							"method":      "PUT",
							"path":        "/messages/:message_id/read",
							"description": "Mesajı okundu olarak işaretle",
						},
						{
							"method":      "DELETE",
							"path":        "/messages/:message_id",
							"description": "Mesajı sil (sadece gönderen)",
						},
						{
							"method":      "GET",
							"path":        "/conversations",
							"description": "Sohbet listesi ve son mesajlar",
						},
						{
							"method":      "GET",
							"path":        "/unread-count",
							"description": "Okunmamış mesaj sayısı",
						},
						{
							"method":      "GET",
							"path":        "/online-users",
							"description": "Online kullanıcı listesi",
						},
						{
							"method":      "GET",
							"path":        "/user-online/:user_id",
							"description": "Belirli kullanıcının online durumu",
						},
					},
				},
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
	log.Println("🔌 WebSocket:")
	log.Println("  GET  /ws?token=JWT_TOKEN - WebSocket bağlantısı")
	log.Println("  GET  /ws-status - WebSocket durumu")
	log.Println()
	log.Println("📚 Dokümantasyon:")
	log.Println("  GET  /api-docs - API dokümantasyonu")
	log.Println("  GET  /health - Sistem durumu")

	if err := router.Run(":" + cfg.Port); err != nil {
		log.Fatalf("Sunucu başlatma hatası: %v", err)
	}
}
