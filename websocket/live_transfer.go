package websocket

// live_transfer.go — TƏK-CANLI-PER-USER + cihaz köçürməsi (device handoff).
//
// Bir istifadəçi eyni anda YALNIZ bir canlı yayımda ola bilər (cihaz/platforma
// fərq etməz). Reyestr `h.liveUsers` (userID → cari canlı client) messenger WS
// register/unregister-də saxlanılır (bax live_hub.go).
//
// Axış (piokio_live Join daxili API ilə bunları çağırır):
//
//	precheck  — user başqa cihazda canlıdırmı?
//	            allow    → sərbəst qoşul
//	            transfer → EYNİ otaq, BAŞQA cihaz → köçürmə tələb olunur
//	            blocked  → BAŞQA otaq → "artıq canlısınız" (+ keçid seçimi)
//	request   — yeni cihaz köçürmə istəyir → 4-rəqəmli kod yaradılır və AKTİV
//	            cihaza WS ilə göndərilir (live_xfer_code). İstifadəçi kodu aktiv
//	            cihazın ekranından oxuyub yeni cihaza yazır.
//	confirm   — yeni cihaz kodu göndərir → doğrudursa aktiv cihaza
//	            live_force_leave gedir (otaq host üçün grace ilə davam edir),
//	            rol qaytarılır (piokio_live həmin rolla token verir).
//	switch    — "cari canlını bağlayıb keç": istifadəçinin cari canlısı
//	            məcburən bağlanır (host isə otaq normal qaydada bitir), sonra
//	            piokio_live yeni otağa token verir.

import (
	"crypto/rand"
	"encoding/json"
	"math/big"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

const xferCodeTTL = 120 * time.Second

// activeLiveInfo — precheck cavabı üçün istifadəçinin cari canlı sessiyası.
type activeLiveInfo struct {
	RoomID   uint
	Role     string
	Platform string
	DeviceID string
}

// snapshotActiveLive — h.liveUsers[userID]-in kilid altında sabit nüsxəsi.
func (h *LiveHub) snapshotActiveLive(userID uint) (*LiveRoomClient, *activeLiveInfo) {
	h.mu.RLock()
	cur := h.liveUsers[userID]
	h.mu.RUnlock()
	if cur == nil {
		return nil, nil
	}
	return cur, &activeLiveInfo{
		RoomID:   cur.RoomID,
		Role:     cur.Role,
		Platform: cur.Platform,
		DeviceID: cur.DeviceID,
	}
}

// LivePrecheck — user roomID-ə qoşulmağa çalışır. Cari canlı sessiyasına görə
// nəticə: "allow" | "transfer" | "blocked".
func (h *LiveHub) LivePrecheck(userID, roomID uint, deviceID string) (string, *activeLiveInfo) {
	_, active := h.snapshotActiveLive(userID)
	if active == nil {
		return "allow", nil
	}
	// Eyni cihaz yenidən qoşulur (app restart / reconnect) → sərbəst burax.
	if deviceID != "" && active.DeviceID != "" && deviceID == active.DeviceID {
		return "allow", active
	}
	// Eyni otaq, BAŞQA cihaz → köçürmə.
	if active.RoomID == roomID {
		return "transfer", active
	}
	// BAŞQA otaq → blok (client "cari canlını bağlayıb keç" təklif edir).
	return "blocked", active
}

// GenerateXferCode — yeni cihaz köçürmə istəyir. Aktiv cihaz roomID-də canlı
// olmalıdır. 4-rəqəmli kod yaradılıb aktiv cihaza (live_xfer_code) göndərilir.
func (h *LiveHub) GenerateXferCode(userID, roomID uint) bool {
	client, active := h.snapshotActiveLive(userID)
	if client == nil || active.RoomID != roomID {
		// Aktiv sessiya yoxdur və ya fərqli otaqdadır — köçürmə mümkün deyil.
		return false
	}

	code := gen4DigitCode()

	h.xferMu.Lock()
	h.xfers[userID] = &xferEntry{
		Code:      code,
		RoomID:    active.RoomID,
		Role:      active.Role,
		ExpiresAt: time.Now().Add(xferCodeTTL),
	}
	h.xferMu.Unlock()

	// Kodu AKTİV cihaza WS ilə göndər (o cihazın ekranında göstərilir; X ilə
	// bağlana bilər → live_xfer_cancel).
	payload, _ := json.Marshal(map[string]interface{}{
		"type":    "live_xfer_code",
		"room_id": active.RoomID,
		"data": map[string]interface{}{
			"code":       code,
			"expires_in": int(xferCodeTTL.Seconds()),
		},
	})
	h.sendToUser(active.RoomID, userID, payload)
	return true
}

// ConfirmXfer — yeni cihaz kodu göndərir. Doğrudursa: aktiv cihaza
// live_force_leave göndərilir (transferring=true → otaq host üçün grace ilə
// davam edir), rol + otaq qaytarılır.
func (h *LiveHub) ConfirmXfer(userID uint, code string) (bool, uint, string) {
	h.xferMu.Lock()
	e := h.xfers[userID]
	if e == nil || e.Code != code || time.Now().After(e.ExpiresAt) {
		if e != nil && time.Now().After(e.ExpiresAt) {
			delete(h.xfers, userID)
		}
		h.xferMu.Unlock()
		return false, 0, ""
	}
	roomID := e.RoomID
	role := e.Role
	delete(h.xfers, userID)
	h.xferMu.Unlock()

	// Köhnə cihazı canlıdan çıxar. transferring=true → host üçün otaq DƏRHAL
	// bağlanmır; yeni cihaz host kimi qoşulanda (Register) grace ləğv olunur.
	h.forceLeaveUser(userID, roomID, "transfer", true)
	return true, roomID, role
}

// CancelXfer — gözləyən köçürmə kodunu ləğv edir (aktiv cihaz "X" etdi).
func (h *LiveHub) CancelXfer(userID uint) {
	h.xferMu.Lock()
	delete(h.xfers, userID)
	h.xferMu.Unlock()
}

// SwitchLiveAway — "cari canlını bağlayıb keç": istifadəçinin cari canlısını
// məcburən bağlayır. transferring=false → host isə köhnə otaq normal qaydada
// bitir (istifadəçi onu tərk edir). Sonra piokio_live yeni otağa token verir.
func (h *LiveHub) SwitchLiveAway(userID uint) {
	_, active := h.snapshotActiveLive(userID)
	if active == nil {
		return
	}
	h.forceLeaveUser(userID, active.RoomID, "switch", false)
}

// forceLeaveUser — verilən istifadəçinin roomID-dəki canlı client-inə
// live_force_leave göndərir və (client cavab verməsə belə) qısa gecikmədən
// sonra WS-i serverdən bağlayır. transferring bayrağı Unregister-in host
// davranışını təyin edir (bax live_hub.go).
func (h *LiveHub) forceLeaveUser(userID, roomID uint, reason string, transferring bool) {
	h.mu.RLock()
	client := h.liveUsers[userID]
	h.mu.RUnlock()
	if client == nil || client.RoomID != roomID {
		return
	}

	if transferring {
		client.transferring.Store(true)
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"type":    "live_force_leave",
		"room_id": roomID,
		"data":    map[string]interface{}{"reason": reason},
	})
	h.sendToUser(roomID, userID, payload)

	// Client hadisəni işləyib öz-özünü bağlamalıdır; cavabsız qalarsa server
	// 3s sonra bağlayır (idempotent). Yalnız hələ də EYNİ client-dirsə bağla —
	// bu arada yeni cihaz qoşulub liveUsers-i əvəz edə bilər.
	time.AfterFunc(3*time.Second, func() {
		h.mu.RLock()
		still := h.liveUsers[userID] == client
		h.mu.RUnlock()
		if still {
			client.closeSend()
		}
	})
}

// gen4DigitCode — 0000–9999 arası 4-rəqəmli kod (crypto/rand).
func gen4DigitCode() string {
	n, err := rand.Int(rand.Reader, big.NewInt(10000))
	if err != nil {
		// Praktikada baş vermir; fallback determinist deyil, sadəcə vaxt əsaslı.
		return "0000"
	}
	digits := []byte("0123456789")
	v := n.Int64()
	return string([]byte{
		digits[(v/1000)%10],
		digits[(v/100)%10],
		digits[(v/10)%10],
		digits[v%10],
	})
}

// ─── Daxili HTTP qatı (piokio_live → messenger, X-Internal-Secret) ──────────

// PrecheckLive — POST /internal/live/precheck {user_id, room_id, device_id}.
func (h *LiveHub) PrecheckLive(c *gin.Context) {
	var b struct {
		UserID   uint   `json:"user_id"`
		RoomID   uint   `json:"room_id"`
		DeviceID string `json:"device_id"`
	}
	if err := c.ShouldBindJSON(&b); err != nil || b.UserID == 0 || b.RoomID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	status, active := h.LivePrecheck(b.UserID, b.RoomID, b.DeviceID)
	resp := gin.H{"status": status}
	if active != nil {
		resp["active_room_id"] = active.RoomID
		resp["role"] = active.Role
		resp["platform"] = active.Platform
	}
	c.JSON(http.StatusOK, resp)
}

// RequestXfer — POST /internal/live/transfer/request {user_id, room_id}.
func (h *LiveHub) RequestXfer(c *gin.Context) {
	var b struct {
		UserID uint `json:"user_id"`
		RoomID uint `json:"room_id"`
	}
	if err := c.ShouldBindJSON(&b); err != nil || b.UserID == 0 || b.RoomID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if !h.GenerateXferCode(b.UserID, b.RoomID) {
		c.JSON(http.StatusConflict, gin.H{"success": false, "message": "no active session in this room"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "expires_in": int(xferCodeTTL.Seconds())})
}

// ConfirmXferInternal — POST /internal/live/transfer/confirm {user_id, code}.
func (h *LiveHub) ConfirmXferInternal(c *gin.Context) {
	var b struct {
		UserID uint   `json:"user_id"`
		Code   string `json:"code"`
	}
	if err := c.ShouldBindJSON(&b); err != nil || b.UserID == 0 || b.Code == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	ok, roomID, role := h.ConfirmXfer(b.UserID, b.Code)
	if !ok {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": "invalid or expired code"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "room_id": roomID, "role": role})
}

// SwitchLiveInternal — POST /internal/live/switch {user_id}.
func (h *LiveHub) SwitchLiveInternal(c *gin.Context) {
	var b struct {
		UserID uint `json:"user_id"`
	}
	if err := c.ShouldBindJSON(&b); err != nil || b.UserID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	h.SwitchLiveAway(b.UserID)
	c.JSON(http.StatusOK, gin.H{"success": true})
}
