package websocket

import (
	"encoding/json"
	"net/http"
	"strconv"

	"beanpon_messenger/database"
	"beanpon_messenger/models"

	"github.com/gin-gonic/gin"
)

// resetQuestionVotes — otağın Ok-Nok səslərini sıfırlayır (yeni sual gələndə).
func (h *LiveHub) resetQuestionVotes(roomID uint) {
	h.mu.Lock()
	delete(h.questionVotes, roomID)
	h.mu.Unlock()
}

// tallyQuestionVotes — verilmiş sual üçün ok/nok saylarını qaytarır.
// Çağıran mu-nu tutmamalıdır (öz kilidini alır).
func (h *LiveHub) tallyQuestionVotes(roomID, questionID uint) (ok int, nok int) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, vote := range h.questionVotes[roomID][questionID] {
		if vote == "ok" {
			ok++
		} else if vote == "nok" {
			nok++
		}
	}
	return ok, nok
}

// CastQuestionVote — hər izləyici Ok-Nok sualına səs verə bilər (dəyişə bilər).
// Səslər yalnız yaddaşda saxlanır (WS-only), DB-yə yazılmır.
//
// POST /api/v1/live-rooms/:room_id/question-vote
// Body: { "question_id": 123, "vote": "ok" | "nok" }
func (h *LiveHub) CastQuestionVote(c *gin.Context) {
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

	var body struct {
		QuestionID uint   `json:"question_id"`
		Vote       string `json:"vote"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.QuestionID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid body"})
		return
	}
	if body.Vote != "ok" && body.Vote != "nok" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid vote"})
		return
	}

	// Otaq aktiv olmalıdır
	var room models.LiveRoom
	if err := database.DB.Select("id, status").First(&room, roomID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Otaq tapılmadı"})
		return
	}
	if room.Status != "live" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Otaq aktiv deyil"})
		return
	}

	// Səsi yaddaşda upsert et (dəyişə bilər)
	h.mu.Lock()
	if h.questionVotes[roomID] == nil {
		h.questionVotes[roomID] = make(map[uint]map[uint]string)
	}
	if h.questionVotes[roomID][body.QuestionID] == nil {
		h.questionVotes[roomID][body.QuestionID] = make(map[uint]string)
	}
	h.questionVotes[roomID][body.QuestionID][userID] = body.Vote
	h.mu.Unlock()

	ok, nok := h.tallyQuestionVotes(roomID, body.QuestionID)

	// Otaqdakı hər kəsə yeni sayları yay
	eventData, _ := json.Marshal(map[string]interface{}{
		"question_id": body.QuestionID,
		"ok":          ok,
		"nok":         nok,
	})
	go func(rID uint, senderID uint, data json.RawMessage) {
		h.Broadcast <- &LiveMessageEvent{
			Type:     "question_vote_update",
			SenderID: senderID,
			RoomID:   rID,
			Data:     data,
		}
	}(roomID, userID, eventData)

	c.JSON(http.StatusOK, gin.H{
		"question_id": body.QuestionID,
		"vote":        body.Vote,
		"ok":          ok,
		"nok":         nok,
	})
}
