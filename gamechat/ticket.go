// Bilet — oyun masasının söhbətinə giriş icazəsi. Onu OYUN SERVERİ verir
// (piokio_game `roomchat/ticket.go` — bu faylın əkizi): messenger oyun masalarını
// tanımır, "bu adam bu masadadır, rolu budur" sözünü yalnız o deyə bilər.
//
// Bilet = base64url(JSON) + "." + base64url(HMAC-SHA256(sirr, "gchat1." + JSON-b64)).
// Sirr servislərin ortaq JWT sirridir; "gchat1." ön şəkilçisi bileti istifadəçi
// JWT-si ilə qarışdırmağı mümkünsüz edir. Formatı dəyişəndə ikisini birlikdə dəyiş.

package gamechat

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const (
	RoleHost    = "host"
	RolePlayer  = "player"
	RoleWatcher = "watcher"

	prefix = "gchat1."
	// TicketLife — bilet bu qədər keçərlidir. Bağlantı qurulandan sonra bilet
	// yoxlanmır; müddət yalnız "köhnə bileti sonradan işlətmək" pəncərəsidir.
	TicketLife = 12 * time.Hour
)

// Claims — biletin içi. Sahə adları qısadır (bilet hər qoşulmada simdən keçir).
type Claims struct {
	Room   string `json:"rid"` // masanın söhbət otağı (masa başına təsadüfi)
	Game   string `json:"g"`   // domino | pio | loto
	Code   string `json:"c"`   // masanın kodu (yalnız göstərmək / log üçün)
	UserID uint64 `json:"u"`
	Role   string `json:"r"`
	Name   string `json:"n"`
	Avatar string `json:"a,omitempty"`
	Exp    int64  `json:"e"`
}

func sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(prefix + body))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Ticket — imzalı bilet. `Exp` boşdursa indidən `TicketLife` sonraya qoyulur.
func Ticket(secret string, c Claims) string {
	if c.Exp == 0 {
		c.Exp = time.Now().Add(TicketLife).Unix()
	}
	raw, _ := json.Marshal(&c)
	body := base64.RawURLEncoding.EncodeToString(raw)
	return body + "." + sign(secret, body)
}

var ErrTicket = errors.New("bad_ticket")

// Verify — bileti yoxlayır (imza, müddət, məcburi sahələr).
func Verify(secret, ticket string, now time.Time) (Claims, error) {
	var c Claims
	body, sig, ok := strings.Cut(ticket, ".")
	if !ok || secret == "" || len(ticket) > 2048 {
		return c, ErrTicket
	}
	if !hmac.Equal([]byte(sig), []byte(sign(secret, body))) {
		return c, ErrTicket
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || json.Unmarshal(raw, &c) != nil {
		return c, ErrTicket
	}
	if c.Room == "" || c.UserID == 0 || c.Exp < now.Unix() {
		return c, ErrTicket
	}
	switch c.Role {
	case RoleHost, RolePlayer, RoleWatcher:
	default:
		return c, ErrTicket
	}
	return c, nil
}
