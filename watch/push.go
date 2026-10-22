package watch

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"strings"
)

// pushText — title + body per language. Placeholders: {host} {user} {room}
// {provider}. Usernames are passed without "@".
type pushText struct{ title, body string }

var watchTitles = map[string]string{
	"az": "Birlikdə izlə",
	"en": "Watch together",
	"tr": "Birlikte izle",
	"ru": "Смотрим вместе",
	"de": "Gemeinsam schauen",
	"es": "Ver juntos",
	"uz": "Birga tomosha",
}

var pushBodies = map[string]map[string]string{
	"watch_invite": {
		"az": "{host} səni “{room}” otağına dəvət etdi",
		"en": "{host} invited you to “{room}”",
		"tr": "{host} seni “{room}” odasına davet etti",
		"ru": "{host} приглашает вас в комнату «{room}»",
		"de": "{host} hat dich zu „{room}“ eingeladen",
		"es": "{host} te invitó a «{room}»",
		"uz": "{host} sizni “{room}” xonasiga taklif qildi",
	},
	"watch_request": {
		"az": "{user} “{room}” otağına qoşulmaq istəyir",
		"en": "{user} wants to join “{room}”",
		"tr": "{user} “{room}” odasına katılmak istiyor",
		"ru": "{user} хочет присоединиться к «{room}»",
		"de": "{user} möchte „{room}“ beitreten",
		"es": "{user} quiere unirse a «{room}»",
		"uz": "{user} “{room}” xonasiga qo‘shilmoqchi",
	},
	"watch_request_accepted": {
		"az": "{host} istəyini qəbul etdi — otağa qoşul",
		"en": "{host} accepted your request — join the room",
		"tr": "{host} isteğini kabul etti — odaya katıl",
		"ru": "{host} принял(а) ваш запрос — заходите в комнату",
		"de": "{host} hat deine Anfrage angenommen – komm in den Raum",
		"es": "{host} aceptó tu solicitud: únete a la sala",
		"uz": "{host} so‘rovingizni qabul qildi — xonaga qo‘shiling",
	},
	"watch_room_opened": {
		"az": "“{room}” başladı — {host} səni gözləyir",
		"en": "“{room}” has started — {host} is waiting for you",
		"tr": "“{room}” başladı — {host} seni bekliyor",
		"ru": "«{room}» началась — {host} ждёт вас",
		"de": "„{room}“ hat begonnen – {host} wartet auf dich",
		"es": "«{room}» ha comenzado: {host} te espera",
		"uz": "“{room}” boshlandi — {host} sizni kutmoqda",
	},
	"watch_open_reminder": {
		"az": "Otağını açmaq vaxtıdır: “{room}”",
		"en": "Time to open your room: “{room}”",
		"tr": "Odanı açma zamanı: “{room}”",
		"ru": "Пора открыть вашу комнату: «{room}»",
		"de": "Zeit, deinen Raum zu öffnen: „{room}“",
		"es": "Es hora de abrir tu sala: «{room}»",
		"uz": "Xonangizni ochish vaqti keldi: “{room}”",
	},
	"watch_remind": {
		"az": "“{room}” başlamaq üzrədir — hazır ol",
		"en": "“{room}” is about to start — get ready",
		"tr": "“{room}” başlamak üzere — hazır ol",
		"ru": "«{room}» вот-вот начнётся — приготовьтесь",
		"de": "„{room}“ startet gleich – mach dich bereit",
		"es": "«{room}» está por empezar: prepárate",
		"uz": "“{room}” boshlanish arafasida — tayyor turing",
	},
	"watch_help": {
		"az": "{host}: {provider} tətbiqini öz hesabınla aç",
		"en": "{host}: open the {provider} app with your own account",
		"tr": "{host}: {provider} uygulamasını kendi hesabınla aç",
		"ru": "{host}: откройте приложение {provider} со своим аккаунтом",
		"de": "{host}: Öffne die {provider}-App mit deinem eigenen Konto",
		"es": "{host}: abre la app de {provider} con tu propia cuenta",
		"uz": "{host}: {provider} ilovasini o‘z hisobingiz bilan oching",
	},
}

var supportedLangs = map[string]bool{"az": true, "en": true, "tr": true, "ru": true, "de": true, "es": true, "uz": true}

// normalizeLang — same rule as Laravel NotificationLocale::normalize.
func normalizeLang(raw string) string {
	code := strings.ToLower(strings.TrimSpace(raw))
	if i := strings.IndexAny(code, "-_"); i >= 0 {
		code = code[:i]
	}
	if supportedLangs[code] {
		return code
	}
	return "en"
}

func renderPush(typ, lang string, params map[string]string) (string, string, bool) {
	bodies, ok := pushBodies[typ]
	if !ok {
		return "", "", false
	}
	body := bodies[lang]
	if body == "" {
		body = bodies["en"]
	}
	for k, v := range params {
		body = strings.ReplaceAll(body, "{"+k+"}", v)
	}
	title := watchTitles[lang]
	if title == "" {
		title = watchTitles["en"]
	}
	return title, body, true
}

// push — one push via Laravel FcmController (/notification/send, no history).
// Blocking: call it from a goroutine (after.run does).
func (m *Module) push(uid int64, typ string, params map[string]string, data map[string]string) {
	if uid <= 0 || m.cfg.CloudToken == "" || m.cfg.BackendURL == "" {
		return
	}
	m.pushSem <- struct{}{}
	defer func() { <-m.pushSem }()

	var paused bool
	m.db.Raw(`SELECT EXISTS (SELECT 1 FROM user_notification_pauses WHERE user_id = ? AND paused_until > NOW())`, uid).Scan(&paused)
	if paused {
		return
	}
	var lang string
	m.db.Raw(`SELECT COALESCE(lang, '') FROM fcm_tokens WHERE user_id = ? AND is_active = true ORDER BY id DESC LIMIT 1`, uid).Scan(&lang)
	title, body, ok := renderPush(typ, normalizeLang(lang), params)
	if !ok {
		return
	}
	payloadData := map[string]string{"route": "watch_room", "notification_type": typ, "type": typ}
	for k, v := range data {
		payloadData[k] = v
	}
	payload := map[string]any{
		"user_id":      uid,
		"title":        title,
		"body":         body,
		"data":         payloadData,
		"save_history": false,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(m.cfg.BackendURL, "/")+"/notification/send", bytes.NewReader(b))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-api-key", m.cfg.CloudToken)
	resp, err := m.http.Do(req)
	if err != nil {
		log.Printf("[watch] push %s → %d: %v", typ, uid, err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusInternalServerError {
		// 500 = the user has no active device (FcmService throws) — normal.
		log.Printf("[watch] push %s → %d: HTTP %d", typ, uid, resp.StatusCode)
	}
}
