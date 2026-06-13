package websocket

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// 1:1 sesli arama sinyalleşməsi.
//
// Laravel (api.beanpon.com) çağrı qurur, LiveKit token üretir, sonra bu
// internal endpoint-i çağırır → online callee/caller-ə WS ilə call_* event
// çatdırılır. Ses isə LiveKit-dən (livekit.beanpon.com) axır — bura yalnız
// siqnal daşıyır.
//
// Route (main.go /internal qrupunda):
//
//	internal.POST("/calls/signal", wsHub.HandleCallSignal)
type callSignalRequest struct {
	Event    string                 `json:"event" binding:"required"`
	ToUserID uint                   `json:"to_user_id" binding:"required"`
	Data     map[string]interface{} `json:"data"`
}

// HandleCallSignal — Laravel-dən gələn çağrı siqnalını hədəf istifadəçiyə ötür.
// Cavabda `delivered`: 1 (online, WS ilə çatdı) / 0 (offline → Laravel push atır).
func (h *Hub) HandleCallSignal(c *gin.Context) {
	var req callSignalRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	// Yalnız call_* event-lərinə icazə (təhlükəsizlik).
	switch req.Event {
	case "call_incoming", "call_accepted", "call_rejected",
		"call_canceled", "call_ended", "call_busy":
		// ok
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown event"})
		return
	}

	online := h.IsUserOnline(req.ToUserID)
	if online {
		h.SendToUser(req.ToUserID, req.Event, req.Data)
	}

	delivered := 0
	if online {
		delivered = 1
	}
	c.JSON(http.StatusOK, gin.H{"delivered": delivered})
}
