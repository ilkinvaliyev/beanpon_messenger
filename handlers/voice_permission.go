package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"beanpon_messenger/database"
	"beanpon_messenger/models"
)

// EffectiveAllowVoice — ownerID (sesi ALAN) senderID'dən (sesi GÖNDƏRƏN) sesli
// mesaj qəbul edirmi? Override (voice_message_overrides) varsa onu, yoxsa
// ownerGlobal (user_settings.allow_voice_messages) qaytarır.
func EffectiveAllowVoice(ownerID, senderID uint, ownerGlobal bool) bool {
	if ownerID == 0 || senderID == 0 {
		return ownerGlobal
	}
	var ov models.VoiceMessageOverride
	if err := database.DB.
		Where("user_id = ? AND target_user_id = ?", ownerID, senderID).
		First(&ov).Error; err == nil {
		return ov.Allowed
	}
	return ownerGlobal
}

// SetVoiceOverride — cari user (ayar sahibi) verilmiş target_user_id üçün sesli
// mesaj iznini açır/bağlayır (global-dən asılı olmayaraq). POST body:
// {"target_user_id": <uint>, "allowed": <bool>}. upsert.
func (h *MessageHandler) SetVoiceOverride(c *gin.Context) {
	uidRaw, ok := c.Get("user_id")
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"success": false, "message": "Unauthorized"})
		return
	}
	userID := uidRaw.(uint)

	var body struct {
		TargetUserID uint `json:"target_user_id"`
		Allowed      bool `json:"allowed"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.TargetUserID == 0 || body.TargetUserID == userID {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid target_user_id"})
		return
	}

	// Upsert (user_id, target_user_id) üzrə — unikal index var.
	ov := models.VoiceMessageOverride{UserID: userID, TargetUserID: body.TargetUserID}
	if err := database.DB.
		Where("user_id = ? AND target_user_id = ?", userID, body.TargetUserID).
		Assign(map[string]interface{}{"allowed": body.Allowed}).
		FirstOrCreate(&ov).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": err.Error()})
		return
	}
	// Assign, mövcud sətirdə həmişə persist etmir → hədəflənmiş update.
	database.DB.Model(&models.VoiceMessageOverride{}).
		Where("user_id = ? AND target_user_id = ?", userID, body.TargetUserID).
		Update("allowed", body.Allowed)

	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{
		"target_user_id": body.TargetUserID,
		"allowed":        body.Allowed,
	}})
}
