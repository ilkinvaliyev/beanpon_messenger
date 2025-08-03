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
	wsHub := websocket.NewHub(database.DB, encryptionService) // Database ve encryption service ver
	go wsHub.Run()

	// Handler'ları oluştur
	messageHandler := handlers.NewMessageHandler(encryptionService, wsHub)

	// Gin router'ını oluştur
	router := gin.Default()

	// CORS middleware (gerekirse)
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
		c.JSON(http.StatusOK, gin.H{
			"connected_users": wsHub.GetConnectedUsersCount(),
			"status":          "running",
			"websocket_url":   "/ws",
		})
	})

	// WebSocket endpoint (JWT middleware ile)
	router.GET("/ws", middleware.JWTMiddlewareForWebSocket(cfg.JWTSecret), wsHub.HandleWebSocket)

	// Mesajlaşma API route'ları (JWT korumalı)
	api := router.Group("/api/v1")
	api.Use(middleware.JWTMiddleware(cfg.JWTSecret))
	{
		// Mesaj gönder
		api.POST("/messages", messageHandler.SendMessage)

		// Belirli kullanıcı ile mesajları getir
		api.GET("/messages/:user_id", messageHandler.GetMessages)

		// Mesajı okundu olarak işaretle
		api.PUT("/messages/:message_id/read", messageHandler.MarkAsRead)

		// Sohbet listesi
		api.GET("/conversations", messageHandler.GetConversations)
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

	// HTTP sunucusunu başlat
	log.Printf("Sunucu %s portunda başlatılıyor...", cfg.Port)
	log.Println("API Endpoints:")
	log.Println("  POST /api/v1/messages - Mesaj gönder")
	log.Println("  GET  /api/v1/messages/:user_id - Mesajları getir")
	log.Println("  PUT  /api/v1/messages/:message_id/read - Okundu işaretle")
	log.Println("  GET  /api/v1/conversations - Sohbet listesi")
	log.Println("  GET  /ws?token=JWT_TOKEN - WebSocket bağlantısı")
	log.Println("  GET  /ws-status - WebSocket durumu")

	if err := router.Run(":" + cfg.Port); err != nil {
		log.Fatalf("Sunucu başlatma hatası: %v", err)
	}
}
