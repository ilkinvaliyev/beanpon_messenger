package handlers

import (
	"os"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// ── REST TƏRƏFİNDƏ İSTEMÇİ YETENEK PAZARLIĞI ────────────────────────────────
//
// NİYƏ LAZIMDIR
// Bu servisə eyni anda ÜÇ istemçi gəlir: canlı Flutter tətbiqi (`beanpon_app`),
// App Store-dakı KÖHNƏ `piokio_ios` buraxılışları və yeni iOS buraxılışı.
// Cavabın formasını və ya bir yan effekti dəyişmək köhnə istemçini SESSİZCƏ
// sındıra bilər. Ona görə heç bir yeni davranış "avtomatik" deyil — istemçi
// özünü tanıtmalıdır.
//
// NECƏ
// Yeni istemçi hər sorğuya `X-Chat-Proto: 2` başlığı qoyur. Başlıq yoxdursa
// (Flutter, köhnə iOS, hər hansı üçüncü tərəf) `protoLegacy` qayıdır və kod
// BUGÜNKÜ yolu seçir.
//
// WebSocket tərəfindəki əkizi `websocket/hub.go`-dakı `?cv=` query parametridir
// (orada başlıq işlətmirik: `sendRecentMessages` qeydiyyat anında, istemçi hələ
// heç nə göndərməmiş işə düşür — yəni qərar upgrade sorğusunda verilməlidir).
//
// NİYƏ `X-App-Version` DEYİL
// `X-App-Version` onsuz da göndərilir, amma "1.4.12" kimi bir sətri müqayisə
// etmək kövrəkdir (semver ardıcıllığı, beta suffiksləri, platformalar arası
// fərqli nömrələmə). Açıq bir protokol nömrəsi niyyəti birmənalı bildirir.
const (
	// protoLegacy — başlıq göndərməyən istemçi: Flutter + köhnə iOS.
	protoLegacy = 1
	// protoV2 — yeni iOS: `message_ack` anlayır, `total` sahəsinə güvənmir.
	protoV2 = 2
	// protoMax — serverin tanıdığı ən yüksək versiya.
	protoMax = protoV2
)

// chatProto — sorğunun istemçi protokol versiyası.
// Başlıq yoxdursa / pozuqdursa `protoLegacy` (yəni köhnə davranış).
func chatProto(c *gin.Context) int {
	raw := strings.TrimSpace(c.GetHeader("X-Chat-Proto"))
	if raw == "" {
		return protoLegacy
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < protoLegacy {
		return protoLegacy
	}
	if v > protoMax {
		return protoMax
	}
	return v
}

// ── SORĞU FORMASI KİLL-SWITCH-İ ─────────────────────────────────────────────
//
// `CHAT_QUERY_LEGACY=true` → `GetMessages` KÖHNƏ (`OR` formalı) sorğunu
// işlədir. Yeni `UNION ALL` forması ilə eyni nəticəni verir (11 ssenarida
// sətir-sətir yoxlanıb), amma production-da gözlənilməz bir plan/nəticə fərqi
// çıxarsa REDEPLOY OLMADAN geri qayıtmaq üçün açar saxlanılır.
//
// Default: false (yeni, sürətli yol).
var chatQueryLegacy = func() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("CHAT_QUERY_LEGACY")), "true")
}()
