package handlers

import (
	"beanpon_messenger/database"
	"beanpon_messenger/utils"
	"beanpon_messenger/websocket"
	"log"
	"net/http"
	"strconv"
	"time"
	_ "time"

	"github.com/gin-gonic/gin"
	gorillaws "github.com/gorilla/websocket"
)

var raveUpgrader = gorillaws.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

type RaveHandler struct {
	hub *websocket.RaveHub
}

func NewRaveHandler(hub *websocket.RaveHub) *RaveHandler {
	return &RaveHandler{hub: hub}
}

func (h *RaveHandler) HandleWebSocket(c *gin.Context) {
	raveIDStr := c.Query("rave_id")
	if raveIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "rave_id required"})
		return
	}
	raveIDParsed, err := strconv.ParseUint(raveIDStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid rave_id"})
		return
	}
	raveID := uint(raveIDParsed)

	userIDVal, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	userID := userIDVal.(uint)

	type RaveRow struct {
		ID     uint   `gorm:"column:id"`
		UserID uint   `gorm:"column:user_id"`
		Status string `gorm:"column:status"`
	}
	var rave RaveRow
	if err := database.DB.Table("raves").
		Select("id, user_id, status").
		Where("id = ? AND deleted_at IS NULL", raveID).
		First(&rave).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "rave not found"})
		return
	}

	if rave.Status != "active" {
		c.JSON(http.StatusForbidden, gin.H{"error": "rave is not active"})
		return
	}

	type UserRow struct {
		Name   string  `gorm:"column:name"`
		Avatar *string `gorm:"column:profile_image"`
	}
	var user UserRow
	database.DB.Table("users u").
		Select("u.name, p.profile_image").
		Joins("LEFT JOIN profiles p ON p.user_id = u.id").
		Where("u.id = ?", userID).
		Scan(&user)

	conn, err := raveUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("❌ Rave WS upgrade error: %v", err)
		return
	}

	client := &websocket.RaveClient{
		Hub:    h.hub,
		Conn:   conn,
		UserID: userID,
		RaveID: raveID,
		IsHost: rave.UserID == userID,
		Name:   user.Name,
		Avatar: user.Avatar,
		Send:   make(chan []byte, 256),
	}

	h.hub.Register <- client

	go client.WritePump()
	client.ReadPump()
}

func (h *RaveHandler) GetMessages(c *gin.Context) {
	raveIDStr := c.Param("rave_id")
	raveIDParsed, err := strconv.ParseUint(raveIDStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid rave_id"})
		return
	}
	raveID := uint(raveIDParsed)

	limitStr := c.DefaultQuery("limit", "20")
	cursorStr := c.Query("cursor")

	limit, err := strconv.Atoi(limitStr)
	if err != nil || limit <= 0 || limit > 50 {
		limit = 20
	}

	type MessageRow struct {
		ID           uint      `json:"id"`
		Text         string    `json:"text"`
		SenderID     uint      `json:"sender_id"`
		SenderName   string    `json:"sender_name"`
		SenderAvatar *string   `json:"sender_avatar"`
		CreatedAt    time.Time `json:"created_at"`

		ReplyToID         *uint   `json:"reply_to_id"`
		ReplyText         *string `json:"reply_text"`
		ReplySenderID     *uint   `json:"reply_sender_id"`
		ReplySenderName   *string `json:"reply_sender_name"`
		ReplySenderAvatar *string `json:"reply_sender_avatar"`
	}

	query := database.DB.Table("rave_messages rm").
		Select(`rm.id, rm.text, rm.user_id as sender_id, rm.reply_to_id, rm.created_at,
			u.name as sender_name,
			p.profile_image as sender_avatar,
			rep.text as reply_text,
			rep.user_id as reply_sender_id,
			ru.name as reply_sender_name,
			rp.profile_image as reply_sender_avatar`).
		Joins("LEFT JOIN users u ON u.id = rm.user_id").
		Joins("LEFT JOIN profiles p ON p.user_id = rm.user_id").
		Joins("LEFT JOIN rave_messages rep ON rep.id = rm.reply_to_id").
		Joins("LEFT JOIN users ru ON ru.id = rep.user_id").
		Joins("LEFT JOIN profiles rp ON rp.user_id = rep.user_id").
		Where("rm.rave_id = ? AND rm.deleted_at IS NULL", raveID).
		Order("rm.id DESC").
		Limit(limit + 1)

	if cursorStr != "" {
		cursor, err := strconv.ParseUint(cursorStr, 10, 64)
		if err == nil {
			query = query.Where("rm.id < ?", cursor)
		}
	}

	var messages []MessageRow
	if err := query.Scan(&messages).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "messages fetch failed"})
		return
	}

	hasMore := len(messages) > limit
	if hasMore {
		messages = messages[:limit]
	}

	// Avatar URL'lerini düzelt
	for i := range messages {
		if messages[i].SenderAvatar != nil {
			url := utils.PrependBaseURL(messages[i].SenderAvatar)
			messages[i].SenderAvatar = url
		}
		if messages[i].ReplySenderAvatar != nil {
			url := utils.PrependBaseURL(messages[i].ReplySenderAvatar)
			messages[i].ReplySenderAvatar = url
		}
	}

	var nextCursor *uint
	if hasMore && len(messages) > 0 {
		last := messages[len(messages)-1].ID
		nextCursor = &last
	}

	c.JSON(http.StatusOK, gin.H{
		"messages":    messages,
		"has_more":    hasMore,
		"next_cursor": nextCursor,
	})
}
