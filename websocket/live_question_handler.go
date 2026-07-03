package websocket

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"beanpon_messenger/database"
	"beanpon_messenger/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// AskQuestion - Host "sual ver" düyməsinə basanda çağrılır.
// 1) İstifadəçinin host olduğunu yoxlayır
// 2) Bu otaqda hələ istifadə olunmamış random sual seçir (lang filter ilə)
// 3) Transaction içində usage yazır + shown_count++
// 4) LiveHub.Broadcast vasitəsilə otaqdakı hər kəsə "question" event-i yayır
//
// POST /api/v1/live-rooms/:room_id/ask-question
// Body (optional): { "lang": "az" }
func (h *LiveHub) AskQuestion(c *gin.Context) {
	userIDVal, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	userID := userIDVal.(uint)

	roomIDStr := c.Param("room_id")
	roomID64, err := strconv.ParseUint(roomIDStr, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid room_id"})
		return
	}
	roomID := uint(roomID64)

	// Body — optional lang + type
	var body struct {
		Lang string `json:"lang"`
		Type string `json:"type"`
	}
	_ = c.ShouldBindJSON(&body)
	if body.Lang == "" {
		body.Lang = "az"
	}
	// type göndərilməyibsə həmişə "normal" suallar seçilir
	if body.Type == "" {
		body.Type = "normal"
	}

	// 1) Otağı yoxla — yalnız host sual verə bilər
	var room models.LiveRoom
	if err := database.DB.Select("id, host_user_id, status").First(&room, roomID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Otaq tapılmadı"})
		return
	}
	if room.HostUserID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "Yalnız host sual verə bilər"})
		return
	}
	if room.Status != "live" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Otaq aktiv deyil"})
		return
	}

	// 2) Bu otaqda hələ göstərilməmiş random sual seç (lang + type filter ilə)
	var question models.LiveRoomQuestion
	err = database.DB.
		Table("live_room_questions q").
		Where("q.lang = ?", body.Lang).
		Where("q.type = ?", body.Type).
		Where("NOT EXISTS (SELECT 1 FROM live_room_question_usages u WHERE u.question_id = q.id AND u.live_room_id = ?)", roomID).
		Order("RANDOM()").
		Limit(1).
		Scan(&question).Error

	if err != nil || question.ID == 0 {
		c.JSON(http.StatusNotFound, gin.H{
			"error": "Bu otaq üçün yeni sual qalmadı",
			"code":  "NO_QUESTIONS_LEFT",
		})
		return
	}

	// 3) Transaction: usage yaz + shown_count++
	now := time.Now().UTC()
	txErr := database.DB.Transaction(func(tx *gorm.DB) error {
		usage := models.LiveRoomQuestionUsage{
			LiveRoomID:    roomID,
			QuestionID:    question.ID,
			AskedByUserID: &userID,
			ShownAt:       now,
		}
		if err := tx.Create(&usage).Error; err != nil {
			return err
		}
		if err := tx.Exec(
			"UPDATE live_room_questions SET shown_count = shown_count + 1, updated_at = NOW() WHERE id = ?",
			question.ID,
		).Error; err != nil {
			return err
		}
		return nil
	})

	if txErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Sual yazıla bilmədi"})
		return
	}

	// Yeni sual → əvvəlki sualın Ok-Nok səsləri sıfırlanır (WS-only, keçici).
	h.resetQuestionVotes(roomID)

	// 4) WebSocket vasitəsilə yay
	eventData, _ := json.Marshal(map[string]interface{}{
		"question_id": question.ID,
		"text":        question.Text,
		"lang":        question.Lang,
		"type":        question.Type,
		"asked_by":    userID,
		"shown_at":    now,
	})

	go func(rID uint, senderID uint, data json.RawMessage) {
		h.Broadcast <- &LiveMessageEvent{
			Type:     "question",
			SenderID: senderID,
			RoomID:   rID,
			Data:     data,
		}
	}(roomID, userID, eventData)

	c.JSON(http.StatusOK, gin.H{
		"question_id": question.ID,
		"text":        question.Text,
		"lang":        question.Lang,
		"type":        question.Type,
		"shown_at":    now,
	})
}
