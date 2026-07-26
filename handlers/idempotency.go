package handlers

import (
	"errors"
	"strings"

	"github.com/google/uuid"
)

// ── Issue 9 — Göndərmə idempotentliyi (client_message_id) ────────────────────
//
// PROBLEM
// Hər göndərmə yolu (REST `SendMessage`, REST qrup `SendGroupMessage`, WS
// `send_message`, WS qrup) server tərəfdə `uuid.New()` çağırırdı. Yəni mesajın
// kimliyini SERVER təyin edirdi. Nəticə: istemçi cavabı ala bilmədikdə
// (şəbəkə qopdu, timeout, tətbiq öldürüldü, WS reconnect) təkrar göndərməkdən
// başqa çarəsi yox idi və HƏR TƏKRAR YENİ SƏTİR yaradırdı → söhbətdə eyni
// mesaj 2–3 dəfə görünürdü. Bu, "zəif şəbəkədə mesaj təkrarlanır" şikayətinin
// birbaşa səbəbidir.
//
// HƏLL (WhatsApp/Signal modeli — sxem miqrasiyası TƏLƏB ETMİR)
// Mesajın UUID-ni İSTEMÇİ yaradır və `client_message_id` sahəsində göndərir.
// Server onu birbaşa `messages.id` (PRIMARY KEY) kimi istifadə edir və
// `ON CONFLICT (id) DO NOTHING` ilə yazır:
//   - ilk cəhd → sətir yaranır, normal axın (WS yayımı, push, sayğaclar);
//   - təkrar cəhd → `RowsAffected == 0`, mövcud sətir oxunur və EYNİ cavab
//     qaytarılır; yayım/push/sayğac TƏKRARLANMIR.
// PRIMARY KEY-in unikal indeksi HƏMİŞƏ mövcuddur, ona görə hədəfli
// `ON CONFLICT (id)` burada təhlükəsizdir (bax MIGRATION_* sənədlərindəki
// hədəfsiz forma müzakirəsi — o, indeksi OLMAYAN sütunlara aiddir).
//
// GERİYƏ UYĞUNLUQ
// Sahə opsionaldır. Göndərilməyəndə server əvvəlki kimi `uuid.New()` işlədir —
// köhnə istemçilər heç nə hiss etmir.

// errInvalidClientMessageID — `client_message_id` göndərilib, amma UUID deyil.
var errInvalidClientMessageID = errors.New("client_message_id UUID formatında olmalıdır")

// resolveMessageID — istemçinin verdiyi `client_message_id`-ni mesaj ID-sinə
// çevirir. Boş/verilməyibsə yeni server UUID-i qaytarır.
//
// Qaytarır: (messageID, istemçiTərəfindənVerildi, xəta).
func resolveMessageID(clientMessageID *string) (string, bool, error) {
	if clientMessageID == nil {
		return uuid.New().String(), false, nil
	}
	raw := strings.TrimSpace(*clientMessageID)
	if raw == "" {
		return uuid.New().String(), false, nil
	}
	parsed, err := uuid.Parse(raw)
	if err != nil {
		return "", false, errInvalidClientMessageID
	}
	// Normallaşdırılmış (kiçik hərf, defisli) forma — istemçi böyük hərf və ya
	// mötərizəli forma göndərsə belə eyni sətrə düşsün.
	return parsed.String(), true, nil
}
