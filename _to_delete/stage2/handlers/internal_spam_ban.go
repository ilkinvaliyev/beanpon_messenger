package handlers

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"beanpon_messenger/cache"
)

// ── Issue 62 — spam-ban keşini kənardan təmizləmək mümkün deyildi ────────────
//
// PROBLEM
// `IsMessagingBanned` / `IsMessagingBannedByActions` əvvəlcə paylaşılan Redis
// keşinə baxır və MISS olduqda DB-dən oxuyub nəticəni TTL ilə geri yazır
// (`writeSpamBanCache`). `cache.InvalidateSpamBan` funksiyası MÖVCUD idi,
// amma repo-da HEÇ BİR çağırış yeri yox idi və onu tetikləyəcək endpoint də
// yox idi. Yəni messenger tərəfdə keşi məcburi təmizləməyin YOLU YOX İDİ.
//
// Nəticə iki istiqamətdə də zərərli:
//   • BAN QALDIRILANDA — Laravel keşi yeniləyə bilməsə (Redis anlıq əlçatmaz,
//     circuit breaker açıq, deploy anı) istifadəçi TTL boyu YANLIŞ ŞƏKİLDƏ
//     susdurulmuş qalırdı; dəstək komandasının əlində heç bir alət yox idi.
//   • BAN QOYULANDA — eyni səbəbdən spam göndərən istifadəçi TTL boyu yazmağa
//     DAVAM edirdi. Üstəlik messenger özü `Banned:false` dəyərini keşə yazdığı
//     üçün ban məhz spam burst-ünün ortasında gecikirdi.
//
// HƏLL
// İki internal endpoint (Laravel `X-Internal-Secret` ilə çağırır):
//
//	POST /internal/users/:user_id/spam-ban/invalidate
//	  Body YOXDURSA  → key silinir; növbəti oxuma DB-dən gəlir (təhlükəsiz
//	                   default — mənbə həqiqəti DB-dir).
//	  Body VARSA     → {"banned":true,"actions":["message"]} dəyəri BİRBAŞA
//	                   keşə yazılır (DB gedişi olmadan dərhal effekt).
//
// Endpoint idempotentdir və Redis söndürülübsə səssizcə uğur qaytarır
// (`cache.InvalidateSpamBan` / `SetSpamBan` bu halda no-op-dur).

// InvalidateSpamBanCache — POST /internal/users/:user_id/spam-ban/invalidate
func InvalidateSpamBanCache(c *gin.Context) {
	raw := c.Param("user_id")
	parsed, err := strconv.ParseUint(raw, 10, 32)
	if err != nil || parsed == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Geçersiz user_id"})
		return
	}
	userID := uint(parsed)

	// Body opsionaldır. Verilirsə dəyər birbaşa yazılır; verilmirsə key silinir.
	var body struct {
		Banned  *bool     `json:"banned"`
		Actions *[]string `json:"actions"`
	}
	// ShouldBindJSON boş gövdədə xəta qaytarır — bu, "body yoxdur" deməkdir,
	// xəta sayılmır.
	hasBody := c.ShouldBindJSON(&body) == nil && body.Banned != nil

	if hasBody {
		payload := cache.SpamBanPayload{
			Banned:  *body.Banned,
			Actions: body.Actions,
		}
		if err := cache.SetSpamBan(c.Request.Context(), userID, payload); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "cache yazılamadı"})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"message": "spam ban cache güncellendi",
			"user_id": userID,
			"mode":    "set",
			"banned":  payload.Banned,
		})
		return
	}

	if err := cache.InvalidateSpamBan(c.Request.Context(), userID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "cache temizlenemedi"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"message": "spam ban cache temizlendi",
		"user_id": userID,
		"mode":    "invalidate",
	})
}
