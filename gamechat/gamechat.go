// Package gamechat — oyun masalarının (domino, Pio, loto) söhbəti.
//
// Canlı otaqların söhbəti ilə eyni imkanlar: mətn, GIF, şəkil, Piound, cavab
// (reply), @ad, söyüş süzgəci, kölgə ban (live_spam), bloklar, söhbəti təmizləmə.
// Fərqlər:
//   - Mesajlar ŞİFRƏLƏNMİR və bazaya YAZILMIR: masa keçicidir, söhbəti də. Otağın
//     son `historySize` mesajı yaddaşda durur, otaq boşalandan `emptyRoomLife`
//     sonra silinir. Miqrasiya yoxdur.
//   - Giriş icazəsini oyun serveri verir (bilet — bax `ticket.go`); rol (ev
//     sahibi / oyunçu / tamaşaçı) biletdən gəlir.
//   - Simdə hesab nömrəsi YOXDUR (oyunların qaydası): göndərən `sid` ilə tanınır —
//     otağa xas, geri çevrilməyən qısa açar.
//
// Soket: GET /ws/game-chat?ticket=… (qarşısında JWT middleware). Müqavilə:
// `docs/GAME_CHAT.md`.
//
// Paket yalnız standart kitabxanaya və gorilla/websocket-ə söykənir; layihənin
// qalanı (`Deps`) funksiyalarla verilir — ona görə ayrıca test olunur.
package gamechat

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

const (
	historySize   = 200 // otaqda saxlanan mesaj
	joinHistory   = 80  // qoşulana göndərilən son mesajlar
	maxMembers    = 400
	maxText       = 500 // rune
	maxURL        = 600
	minGap        = 600 * time.Millisecond // bir adamın iki mesajı arasında
	emptyRoomLife = 15 * time.Minute
	idleRoomLife  = 12 * time.Hour

	readLimit  = 8 << 10
	pongWait   = 60 * time.Second
	pingPeriod = 25 * time.Second
	writeWait  = 10 * time.Second
	sendQueue  = 128
)

// Deps — layihənin qalanı ilə bağlar. Boş (nil) funksiya "yoxdur" deməkdir.
type Deps struct {
	Secret string // oyun serveri ilə ortaq sirr (JWT sirri)
	// BadWord — mətn qadağan söz daşıyır (canlı söhbətin süzgəci).
	BadWord func(text string) bool
	// Blocked — iki istifadəçi bir-birini bloklayıb (hər iki istiqamət).
	Blocked func(a, b uint64) bool
	// Shadow — kölgə ban (users.live_spam): yazdıqlarını yalnız özü görür.
	Shadow func(uid uint64) bool
	// FullURL — nisbi yaddaş yolunu tam ünvana çevirir (S3).
	FullURL func(path string) string
	// MediaHosts — tam ünvanlı GIF / şəkil / səs yalnız bu sahələrdən qəbul olunur.
	MediaHosts []string

	// LiveKit — the table's voice chat (voice.go). Any of them empty = no voice.
	LiveKitURL    string // wss://…
	LiveKitKey    string
	LiveKitSecret string

	// LiveKitCall — test hook: replaces the RoomService HTTP call.
	LiveKitCall func(method string, body map[string]any)
}

// DefaultMediaHosts — GIF-lər Giphy-dən, qalanı öz yaddaşımızdan gəlir.
var DefaultMediaHosts = []string{"beanpon.com", "piokio.app", "giphy.com"}

type Module struct {
	deps     Deps
	upgrader websocket.Upgrader

	mu    sync.Mutex // yalnız `rooms` xəritəsi; otağın içi öz kilidindədir
	rooms map[string]*room
}

func New(d Deps) *Module {
	if len(d.MediaHosts) == 0 {
		d.MediaHosts = DefaultMediaHosts
	}
	return &Module{
		deps:  d,
		rooms: make(map[string]*room),
		upgrader: websocket.Upgrader{
			ReadBufferSize: 1024, WriteBufferSize: 1024,
			CheckOrigin:  func(*http.Request) bool { return true },
			Subprotocols: []string{"bearer"},
		},
	}
}

// Start — boş otaqları yığışdıran saat. `ctx` bitəndə dayanır.
func (m *Module) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				m.Sweep(now)
			}
		}
	}()
}

// Sweep — boş və ya çoxdan susan otaqları silir.
func (m *Module) Sweep(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, r := range m.rooms {
		r.mu.Lock()
		if (len(r.members) == 0 && now.Sub(r.active) > emptyRoomLife) || now.Sub(r.active) > idleRoomLife {
			for c := range r.members {
				c.close()
			}
			r.members = make(map[*client]struct{})
			m.voiceClose(r, nil)
			delete(m.rooms, id)
		}
		r.mu.Unlock()
	}
}

// Rooms — açıq otaqların sayı (metrika / test üçün).
func (m *Module) Rooms() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rooms)
}

// ---------------------------------------------------------------------------
// Otaq və üzv
// ---------------------------------------------------------------------------

// room — bir masanın söhbəti. Bütün sahələr `mu` altındadır: bir otağın işi
// (blok sorğusu daxil) başqa otaqları gözlətmir.
type room struct {
	mu      sync.Mutex
	id      string
	members map[*client]struct{}
	msgs    []*Message
	nextID  uint64
	// enabled — söhbət açıqdır (ev sahibi bağlaya bilir: hamı oxuyur, heç kim yazmır).
	enabled bool
	// watchers — tamaşaçılar da yaza bilir.
	watchers bool
	active   time.Time
	// voice — the table's voice session (nil = none). See voice.go.
	voice *voiceSession
}

type client struct {
	m    *Module
	conn *websocket.Conn
	send chan []byte
	done chan struct{}
	once sync.Once

	room   *room // qoşulanda bir dəfə yazılır; üzvlük `room.members`-dədir
	uid    uint64
	sid    string
	role   string
	name   string
	avatar string
	shadow bool
	last   time.Time
	blocks map[uint64]bool // bu bağlantı üçün yadda saxlanan blok cavabları
}

func (c *client) close() { c.once.Do(func() { close(c.done) }) }

// push — kadrı növbəyə qoyur; yavaş müştəri kəsilir (kadr atılmır, sıra pozulmur).
func (c *client) push(b []byte) {
	select {
	case c.send <- b:
	case <-c.done:
	default:
		c.close()
	}
}

// Reply — cavab verilən mesajın qısa görünüşü.
type Reply struct {
	ID   uint64 `json:"id"`
	Name string `json:"name"`
	Text string `json:"text"`
	Kind string `json:"kind"` // text | gif | image | sound
}

// Message — bir söhbət mesajı (simdəki forma). Hesab nömrəsi yoxdur.
type Message struct {
	ID         uint64   `json:"id"`
	CID        string   `json:"cid,omitempty"` // göndərənin öz nömrəsi (optimist baloncuğu tanımaq üçün)
	SID        string   `json:"sid"`
	Name       string   `json:"name"`
	Avatar     string   `json:"avatar,omitempty"`
	Role       string   `json:"role"`
	Text       string   `json:"text"`
	GifURL     string   `json:"gif_url,omitempty"`
	ImageURL   string   `json:"image_url,omitempty"`
	SoundURL   string   `json:"sound_url,omitempty"`
	SoundID    uint64   `json:"sound_id,omitempty"`
	SoundTitle string   `json:"sound_title,omitempty"`
	SoundDur   int      `json:"sound_dur,omitempty"` // ms
	Color      string   `json:"color,omitempty"`     // Piound görünüşü
	CoverType  string   `json:"cover_type,omitempty"`
	CoverURL   string   `json:"cover_url,omitempty"`
	ReplyTo    *Reply   `json:"reply_to,omitempty"`
	Mentions   []string `json:"mentions,omitempty"`
	At         int64    `json:"at"` // ms

	uid    uint64
	shadow bool
}

func (msg *Message) kind() string {
	switch {
	case msg.SoundURL != "":
		return "sound"
	case msg.ImageURL != "":
		return "image"
	case msg.GifURL != "":
		return "gif"
	}
	return "text"
}

func frame(kind string, data any) []byte {
	b, _ := json.Marshal(map[string]any{"type": kind, "data": data})
	return b
}

func (c *client) fail(code, cid string) {
	d := map[string]string{"code": code}
	if cid != "" {
		d["cid"] = cid
	}
	c.push(frame("error", d))
}

// sidFor — otağa xas üzv açarı: eyni adam eyni otaqda həmişə eyni açarı alır,
// açardan hesab nömrəsi çıxarıla bilmir, başqa otaqda açar başqadır.
func (m *Module) sidFor(rid string, uid uint64) string {
	mac := hmac.New(sha256.New, []byte(m.deps.Secret))
	mac.Write([]byte("sid|" + rid + "|" + strconv.FormatUint(uid, 10)))
	return hex.EncodeToString(mac.Sum(nil)[:6])
}

// blocked — göndərənlə alıcı arasında blok varmı (bağlantı başına yadda saxlanır).
func (c *client) blocked(other uint64) bool {
	if other == c.uid || c.m.deps.Blocked == nil {
		return false
	}
	if v, ok := c.blocks[other]; ok {
		return v
	}
	v := c.m.deps.Blocked(c.uid, other)
	if len(c.blocks) < 2048 {
		c.blocks[other] = v
	}
	return v
}

// sees — bu üzv mesajı görürmü (kölgə ban + blok).
func (c *client) sees(msg *Message) bool {
	if msg.uid == c.uid {
		return true
	}
	return !msg.shadow && !c.blocked(msg.uid)
}

// ---------------------------------------------------------------------------
// Bağlantı
// ---------------------------------------------------------------------------

// ServeWS — GET /ws/game-chat?ticket=… . `userID` JWT middleware-dən gəlir və
// biletdəki ilə eyni olmalıdır (bilet başqasına verilə bilməz).
func (m *Module) ServeWS(w http.ResponseWriter, r *http.Request, userID uint64) {
	claims, err := Verify(m.deps.Secret, r.URL.Query().Get("ticket"), time.Now())
	if err != nil || userID == 0 || claims.UserID != userID {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"bad_ticket"}`))
		return
	}
	shadow := m.deps.Shadow != nil && m.deps.Shadow(userID)

	conn, err := m.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(readLimit)
	c := &client{
		m: m, conn: conn, send: make(chan []byte, sendQueue), done: make(chan struct{}),
		uid: userID, sid: m.sidFor(claims.Room, userID), role: claims.Role,
		name: cleanName(claims.Name), avatar: m.mediaURL(claims.Avatar),
		shadow: shadow, blocks: make(map[uint64]bool),
	}
	if !m.enter(c, claims.Room) {
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "full"), time.Now().Add(time.Second))
		_ = conn.Close()
		return
	}
	go c.writePump()
	go c.readPump()
}

// enter — üzvü otağa salır və ona otağın vəziyyətini göndərir. Eyni hesabın
// köhnə bağlantısı (başqa cihaz, yenidən qoşulma) bağlanır.
func (m *Module) enter(c *client, rid string) bool {
	m.mu.Lock()
	r := m.rooms[rid]
	if r == nil {
		r = &room{id: rid, members: make(map[*client]struct{}), enabled: true, watchers: true, active: time.Now()}
		m.rooms[rid] = r
	}
	r.mu.Lock() // sıra həmişə: modul → otaq
	m.mu.Unlock()
	defer r.mu.Unlock()
	for old := range r.members {
		if old.uid == c.uid {
			delete(r.members, old)
			old.close()
		}
	}
	if len(r.members) >= maxMembers {
		return false
	}
	r.members[c] = struct{}{}
	r.active = time.Now()
	c.room = r
	m.voiceSocketBack(r, c)

	msgs := make([]*Message, 0, joinHistory)
	for _, msg := range r.msgs {
		if c.sees(msg) {
			msgs = append(msgs, msg)
		}
	}
	if len(msgs) > joinHistory {
		msgs = msgs[len(msgs)-joinHistory:]
	}
	c.push(frame("joined", map[string]any{
		"rid": r.id, "sid": c.sid, "role": c.role, "name": c.name, "avatar": c.avatar,
		"enabled": r.enabled, "watchers": r.watchers, "messages": msgs,
		"voice": m.voiceWire(r, c, nil),
	}))
	return true
}

func (m *Module) leave(c *client) {
	r := c.room
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.members[c]; ok {
		delete(r.members, c)
		r.active = time.Now()
		m.voiceSocketGone(r, c.uid)
	}
}

type inFrame struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

func (c *client) readPump() {
	defer func() {
		_ = recover() // bir bağlantının xətası prosesi yıxmasın
		c.m.leave(c)
		c.close()
		_ = c.conn.Close()
	}()
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(pongWait))
	})
	for {
		kind, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		select {
		case <-c.done:
			return
		default:
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
		if kind != websocket.TextMessage {
			continue
		}
		var f inFrame
		if json.Unmarshal(data, &f) != nil {
			continue
		}
		c.m.handle(c, &f)
	}
}

func (c *client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		_ = recover()
		ticker.Stop()
		_ = c.conn.Close()
	}()
	for {
		select {
		case b := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if c.conn.WriteMessage(websocket.TextMessage, b) != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if c.conn.WriteMessage(websocket.PingMessage, nil) != nil {
				return
			}
		case <-c.done:
			// Növbədə qalanları (məs. səbəbi deyən son kadr) göndərib bağlayırıq.
			for {
				select {
				case b := <-c.send:
					_ = c.conn.SetWriteDeadline(time.Now().Add(time.Second))
					if c.conn.WriteMessage(websocket.TextMessage, b) != nil {
						return
					}
					continue
				default:
				}
				break
			}
			_ = c.conn.SetWriteDeadline(time.Now().Add(time.Second))
			_ = c.conn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Kadrlar
// ---------------------------------------------------------------------------

type chatIn struct {
	CID        string          `json:"cid"`
	Text       string          `json:"text"`
	GifURL     string          `json:"gif_url"`
	ImageURL   string          `json:"image_url"`
	SoundURL   string          `json:"sound_url"`
	SoundID    json.RawMessage `json:"sound_id"`
	SoundTitle string          `json:"sound_title"`
	SoundDur   int             `json:"sound_dur"`
	Color      string          `json:"color"`
	CoverType  string          `json:"cover_type"`
	CoverURL   string          `json:"cover_url"`
	ReplyToID  json.RawMessage `json:"reply_to_id"`
}

func (m *Module) handle(c *client, f *inFrame) {
	switch f.Type {
	case "ping":
		c.push(frame("pong", map[string]any{}))
		return
	case "chat_message":
		var in chatIn
		if json.Unmarshal(f.Data, &in) != nil {
			return
		}
		m.say(c, &in)
		return
	}

	r := c.room
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.members[c]; !ok {
		return
	}
	if m.voiceFrame(r, c, f.Type, f.Data) {
		return
	}
	switch f.Type {
	case "ticket":
		// Rol dəyişdi (ev sahibliyi keçdi): müştəri oyun serverindən təzə bilet
		// alıb gətirir. Otaq və hesab eyni olmalıdır.
		var d struct {
			Ticket string `json:"ticket"`
		}
		if json.Unmarshal(f.Data, &d) != nil {
			return
		}
		claims, err := Verify(m.deps.Secret, d.Ticket, time.Now())
		if err != nil || claims.UserID != c.uid || claims.Room != r.id {
			c.fail("bad_ticket", "")
			return
		}
		c.role = claims.Role
		c.push(frame("role", map[string]any{"role": c.role}))
		m.voiceRole(r, c)

	case "delete_message":
		var d struct {
			ID uint64 `json:"id"`
		}
		if json.Unmarshal(f.Data, &d) != nil || d.ID == 0 {
			return
		}
		for i, msg := range r.msgs {
			if msg.ID != d.ID {
				continue
			}
			if msg.uid != c.uid && c.role != RoleHost {
				return
			}
			r.msgs = append(r.msgs[:i], r.msgs[i+1:]...)
			r.sendAll(frame("message_deleted", map[string]any{"id": d.ID}))
			return
		}

	case "clear_chat":
		if c.role != RoleHost {
			return
		}
		r.msgs = nil
		r.active = time.Now()
		r.sendAll(frame("chat_cleared", map[string]any{"name": c.name}))

	case "chat_settings":
		if c.role != RoleHost {
			return
		}
		var d struct {
			Enabled  *bool `json:"enabled"`
			Watchers *bool `json:"watchers"`
		}
		if json.Unmarshal(f.Data, &d) != nil || (d.Enabled == nil && d.Watchers == nil) {
			return
		}
		if d.Enabled != nil {
			r.enabled = *d.Enabled
		}
		if d.Watchers != nil {
			r.watchers = *d.Watchers
		}
		r.sendAll(frame("chat_settings", map[string]any{"enabled": r.enabled, "watchers": r.watchers, "name": c.name}))
	}
}

func (r *room) sendAll(b []byte) {
	for c := range r.members {
		c.push(b)
	}
}

var mentionRe = regexp.MustCompile(`@([\w][\w.]*[\w])`)

// say — bir mesaj. Süzgəc kilidsiz işləyir, otağa yazmaq otağın kilidi altında.
func (m *Module) say(c *client, in *chatIn) {
	text := strings.TrimSpace(in.Text)
	if utf8.RuneCountInString(text) > maxText {
		c.fail("too_long", in.CID)
		return
	}
	gif, okG := m.cleanMedia(in.GifURL)
	img, okI := m.cleanMedia(in.ImageURL)
	snd, okS := m.cleanMedia(in.SoundURL)
	cover, okC := m.cleanMedia(in.CoverURL)
	if !okG || !okI || !okS {
		c.fail("bad_media", in.CID)
		return
	}
	if !okC {
		cover = ""
	}
	if text == "" && gif == "" && img == "" && snd == "" {
		return
	}
	if text != "" && m.deps.BadWord != nil && m.deps.BadWord(text) {
		d := map[string]string{"reason": "bad_word"}
		if in.CID != "" {
			d["cid"] = in.CID
		}
		c.push(frame("message_blocked", d))
		return
	}

	r := c.room
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.members[c]; !ok {
		return
	}
	now := time.Now()
	switch {
	case !r.enabled:
		c.fail("chat_off", in.CID)
		return
	case c.role == RoleWatcher && !r.watchers:
		c.fail("watchers_off", in.CID)
		return
	case now.Sub(c.last) < minGap:
		c.fail("too_fast", in.CID)
		return
	}
	c.last = now

	r.nextID++
	msg := &Message{
		ID: r.nextID, CID: cleanToken(in.CID, 64), SID: c.sid, Name: c.name, Avatar: c.avatar, Role: c.role,
		Text: text, GifURL: gif, ImageURL: img, SoundURL: snd, At: now.UnixMilli(),
		uid: c.uid, shadow: c.shadow,
	}
	if snd != "" {
		msg.SoundID = rawUint(in.SoundID)
		msg.SoundTitle = cleanLine(in.SoundTitle, 60)
		msg.SoundDur = min(max(in.SoundDur, 0), 15_000)
		msg.Color, msg.CoverType, msg.CoverURL = cleanToken(in.Color, 24), cleanToken(in.CoverType, 12), cover
	}
	if id := rawUint(in.ReplyToID); id != 0 {
		for _, old := range r.msgs {
			if old.ID == id && c.sees(old) {
				msg.ReplyTo = &Reply{ID: old.ID, Name: old.Name, Text: cleanLine(old.Text, 120), Kind: old.kind()}
				break
			}
		}
	}
	for _, mm := range mentionRe.FindAllStringSubmatch(text, 8) {
		msg.Mentions = append(msg.Mentions, mm[1])
	}

	r.msgs = append(r.msgs, msg)
	if len(r.msgs) > historySize {
		r.msgs = append([]*Message(nil), r.msgs[len(r.msgs)-historySize:]...)
	}
	r.active = now

	b := frame("chat_message", msg)
	for other := range r.members {
		if other.sees(msg) {
			other.push(b)
		}
	}
}

// ---------------------------------------------------------------------------
// Təmizləyicilər
// ---------------------------------------------------------------------------

// mediaURL — bilətdəki şəkil ünvanı: keçərli deyilsə boş.
func (m *Module) mediaURL(raw string) string {
	u, _ := m.cleanMedia(raw)
	return u
}

// cleanMedia — media ünvanı. Boş → ("", true). Nisbi yol öz yaddaşımıza
// çevrilir; tam ünvan yalnız https və icazəli sahədən ola bilər. Ünvanı müştəri
// göndərir və o, BAŞQALARININ ekranında yüklənir — ixtiyari ünvan buraxmaq hər
// oyunçunun IP-sini yad serverə vermək olardı.
func (m *Module) cleanMedia(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", true
	}
	if len(raw) > maxURL || strings.ContainsAny(raw, " \t\r\n\"<>\\") {
		return "", false
	}
	if !strings.Contains(raw, "://") {
		if strings.HasPrefix(raw, "//") || strings.Contains(raw, "..") || m.deps.FullURL == nil {
			return "", false
		}
		raw = m.deps.FullURL(strings.TrimLeft(raw, "/"))
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	for _, h := range m.deps.MediaHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return raw, true
		}
	}
	return "", false
}

func rawUint(raw json.RawMessage) uint64 {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" || s == "null" {
		return 0
	}
	if n, err := strconv.ParseUint(s, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && f > 0 {
		return uint64(f)
	}
	return 0
}

// cleanLine — bir sətirlik mətn: idarəedici simvollar atılır, uzunluq kəsilir.
func cleanLine(s string, limit int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, strings.TrimSpace(s))
	if utf8.RuneCountInString(s) > limit {
		s = string([]rune(s)[:limit])
	}
	return s
}

// cleanToken — açar söz / nömrə: yalnız hərf, rəqəm, `-`, `_`.
func cleanToken(s string, limit int) string {
	if len(s) > limit {
		return ""
	}
	for _, r := range s {
		if !(r == '-' || r == '_' || r == '#' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return ""
		}
	}
	return s
}

func cleanName(s string) string {
	if s = cleanLine(s, 40); s == "" {
		return "?"
	}
	return s
}
