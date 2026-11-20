package gamechat

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const secret = "s3cret"

type rig struct {
	t   *testing.T
	m   *Module
	srv *httptest.Server
}

func newRig(t *testing.T, d Deps) *rig {
	d.Secret = secret
	if d.FullURL == nil {
		d.FullURL = func(p string) string { return "https://api.beanpon.com/api/s3-storage/" + p }
	}
	m := New(d)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid, _ := strconv.ParseUint(r.Header.Get("X-UID"), 10, 64)
		m.ServeWS(w, r, uid)
	}))
	t.Cleanup(srv.Close)
	return &rig{t: t, m: m, srv: srv}
}

type peer struct {
	t    *testing.T
	conn *websocket.Conn
	in   chan map[string]any
}

func ticket(room string, uid uint64, role, name string) string {
	return Ticket(secret, Claims{Room: room, Game: "pio", Code: "AB12C", UserID: uid, Role: role, Name: name})
}

func (r *rig) dial(uid uint64, tk string) (*peer, int) {
	h := http.Header{}
	h.Set("X-UID", strconv.FormatUint(uid, 10))
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(r.srv.URL, "http")+"/?ticket="+tk, h)
	if err != nil {
		if resp != nil {
			return nil, resp.StatusCode
		}
		r.t.Fatalf("bağlantı: %v", err)
	}
	p := &peer{t: r.t, conn: conn, in: make(chan map[string]any, 64)}
	go func() {
		defer close(p.in)
		for {
			_, b, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var m map[string]any
			if json.Unmarshal(b, &m) == nil {
				m["_raw"] = string(b)
				p.in <- m
			}
		}
	}()
	r.t.Cleanup(func() { _ = conn.Close() })
	return p, 101
}

func (r *rig) join(room string, uid uint64, role, name string) (*peer, map[string]any) {
	p, code := r.dial(uid, ticket(room, uid, role, name))
	if p == nil {
		r.t.Fatalf("qoşulmadı: %d", code)
	}
	return p, p.want("joined")
}

// want — növü `kind` olan növbəti kadrın `data`-sı (başqa kadrlar ötürülür).
func (p *peer) want(kind string) map[string]any {
	p.t.Helper()
	for {
		select {
		case m, ok := <-p.in:
			if !ok {
				p.t.Fatalf("%q gözlənilirdi, bağlantı bağlandı", kind)
			}
			if m["type"] == kind {
				d, _ := m["data"].(map[string]any)
				if d == nil {
					d = map[string]any{}
				}
				d["_raw"] = m["_raw"]
				return d
			}
		case <-time.After(2 * time.Second):
			p.t.Fatalf("%q gəlmədi", kind)
		}
	}
}

// quiet — qısa müddətdə `kind` gəlməməlidir.
func (p *peer) quiet(kind string) {
	p.t.Helper()
	deadline := time.After(120 * time.Millisecond)
	for {
		select {
		case m, ok := <-p.in:
			if ok && m["type"] == kind {
				p.t.Fatalf("%q gəlməməli idi: %v", kind, m["_raw"])
			}
			if !ok {
				return
			}
		case <-deadline:
			return
		}
	}
}

// drain — indiyə qədər gələn kadrları atır.
func (p *peer) drain() {
	time.Sleep(60 * time.Millisecond)
	for {
		select {
		case _, ok := <-p.in:
			if !ok {
				return
			}
		default:
			return
		}
	}
}

func (p *peer) send(kind string, data any) {
	b, _ := json.Marshal(map[string]any{"type": kind, "data": data})
	_ = p.conn.WriteMessage(websocket.TextMessage, b)
}

func (p *peer) say(text string) { p.send("chat_message", map[string]any{"text": text}) }

func gap() { time.Sleep(minGap + 30*time.Millisecond) }

func TestTicketGuardsTheDoor(t *testing.T) {
	r := newRig(t, Deps{})
	if _, code := r.dial(7, "garbage"); code != http.StatusForbidden {
		t.Fatalf("pozuq bilet: %d", code)
	}
	// Başqasının bileti.
	if _, code := r.dial(8, ticket("room", 7, RolePlayer, "ilkin")); code != http.StatusForbidden {
		t.Fatalf("başqasının bileti keçdi: %d", code)
	}
	// Başqa sirrlə imzalanmış bilet.
	bad := Ticket("other", Claims{Room: "room", UserID: 7, Role: RoleHost, Name: "x"})
	if _, code := r.dial(7, bad); code != http.StatusForbidden {
		t.Fatalf("yad imza keçdi: %d", code)
	}
	_, j := r.join("room", 7, RoleHost, "ilkin")
	if j["role"] != RoleHost || j["enabled"] != true || j["watchers"] != true || j["sid"] == "" {
		t.Fatalf("joined: %v", j)
	}
}

func TestChatFlow(t *testing.T) {
	r := newRig(t, Deps{})
	a, ja := r.join("room", 111222, RoleHost, "ilkin")
	b, _ := r.join("room", 333444, RolePlayer, "fidan")
	w, _ := r.join("room", 555666, RoleWatcher, "murad")
	other, _ := r.join("room2", 999, RoleHost, "kənar")

	a.send("chat_message", map[string]any{"text": "  salam @fidan  ", "cid": "c-1"})
	for _, p := range []*peer{a, b, w} {
		m := p.want("chat_message")
		if m["text"] != "salam @fidan" || m["name"] != "ilkin" || m["role"] != RoleHost || m["sid"] != ja["sid"] || m["cid"] != "c-1" {
			t.Fatalf("mesaj: %v", m)
		}
		if m["mentions"].([]any)[0] != "fidan" {
			t.Fatalf("mentions: %v", m["mentions"])
		}
		// Hesab nömrəsi simdə yoxdur.
		for _, id := range []string{"111222", "333444", "555666"} {
			if strings.Contains(m["_raw"].(string), id) {
				t.Fatal("hesab nömrəsi simdə göründü")
			}
		}
	}
	other.quiet("chat_message") // başqa otaq eşitmir

	// Cavab (reply) + tamaşaçı da yazır.
	w.send("chat_message", map[string]any{"text": "əla", "reply_to_id": 1})
	m := b.want("chat_message")
	rp := m["reply_to"].(map[string]any)
	if m["role"] != RoleWatcher || rp["name"] != "ilkin" || rp["text"] != "salam @fidan" || rp["kind"] != "text" {
		t.Fatalf("cavab: %v", m)
	}

	// Tez-tez yazmaq olmur.
	b.say("bir")
	b.say("iki")
	if e := b.want("error"); e["code"] != "too_fast" {
		t.Fatalf("sürət həddi: %v", e)
	}

	// Sonradan qoşulan tarixçəni görür.
	_, jl := r.join("room", 777, RolePlayer, "leyla")
	if n := len(jl["messages"].([]any)); n != 3 {
		t.Fatalf("tarixçə: %d", n)
	}
}

func TestHostControls(t *testing.T) {
	r := newRig(t, Deps{})
	a, _ := r.join("room", 1, RoleHost, "ilkin")
	b, _ := r.join("room", 2, RolePlayer, "fidan")
	w, _ := r.join("room", 3, RoleWatcher, "murad")

	// Yalnız ev sahibi.
	b.send("clear_chat", nil)
	b.send("chat_settings", map[string]any{"enabled": false})
	a.quiet("chat_settings")

	// Tamaşaçıların yazması bağlanır.
	a.send("chat_settings", map[string]any{"watchers": false})
	if s := w.want("chat_settings"); s["watchers"] != false || s["enabled"] != true || s["name"] != "ilkin" {
		t.Fatalf("ayarlar: %v", s)
	}
	w.say("yaza bilirəm?")
	if e := w.want("error"); e["code"] != "watchers_off" {
		t.Fatalf("tamaşaçı yazdı: %v", e)
	}
	b.say("mən yaza bilirəm")
	a.want("chat_message")

	// Söhbət bağlanır: heç kim yazmır, sonra açılır.
	a.send("chat_settings", map[string]any{"enabled": false})
	b.want("chat_settings")
	gap()
	b.say("bağlıdır")
	if e := b.want("error"); e["code"] != "chat_off" {
		t.Fatalf("bağlı söhbətə yazıldı: %v", e)
	}
	a.send("chat_settings", map[string]any{"enabled": true, "watchers": true})
	if s := b.want("chat_settings"); s["enabled"] != true || s["watchers"] != true {
		t.Fatalf("ayarlar: %v", s)
	}

	// Silmək: öz mesajını hər kəs, başqasınınkını yalnız ev sahibi.
	gap()
	b.say("silinəcək")
	id := a.want("chat_message")["id"]
	w.want("chat_message")
	w.send("delete_message", map[string]any{"id": id})
	a.quiet("message_deleted")
	a.send("delete_message", map[string]any{"id": id})
	if d := b.want("message_deleted"); d["id"] != id {
		t.Fatalf("silinmə: %v", d)
	}

	// Təmizləmə: hamıda boşalır, yeni qoşulan da heç nə görmür.
	a.send("clear_chat", nil)
	if c := w.want("chat_cleared"); c["name"] != "ilkin" {
		t.Fatalf("təmizləmə: %v", c)
	}
	_, j := r.join("room", 9, RolePlayer, "leyla")
	if len(j["messages"].([]any)) != 0 {
		t.Fatal("təmizlənmiş söhbət göründü")
	}

	// Ev sahibliyi keçir: təzə biletlə rol yenilənir.
	b.send("ticket", map[string]any{"ticket": ticket("room", 2, RoleHost, "fidan")})
	if ro := b.want("role"); ro["role"] != RoleHost {
		t.Fatalf("rol: %v", ro)
	}
	b.send("clear_chat", nil)
	a.want("chat_cleared")
	// Başqa otağın bileti rolu dəyişmir.
	w.send("ticket", map[string]any{"ticket": ticket("room2", 3, RoleHost, "murad")})
	if e := w.want("error"); e["code"] != "bad_ticket" {
		t.Fatalf("yad otağın bileti keçdi: %v", e)
	}
}

func TestMediaAndFilters(t *testing.T) {
	r := newRig(t, Deps{
		BadWord: func(s string) bool { return strings.Contains(s, "söyüş") },
		Blocked: func(a, b uint64) bool { return (a == 2 && b == 3) || (a == 3 && b == 2) },
		Shadow:  func(uid uint64) bool { return uid == 4 },
	})
	a, _ := r.join("room", 1, RoleHost, "ilkin")
	b, _ := r.join("room", 2, RolePlayer, "fidan")
	c, _ := r.join("room", 3, RolePlayer, "murad")
	d, _ := r.join("room", 4, RolePlayer, "spam")

	a.send("chat_message", map[string]any{"text": "bu söyüş", "cid": "x"})
	if blk := a.want("message_blocked"); blk["cid"] != "x" {
		t.Fatalf("süzgəc: %v", blk)
	}
	b.quiet("chat_message")

	// Media: nisbi yol öz yaddaşımıza çevrilir, Giphy keçir, yad sahə keçmir.
	gap()
	a.send("chat_message", map[string]any{"image_url": "chat/a.jpg"})
	if m := b.want("chat_message"); m["image_url"] != "https://api.beanpon.com/api/s3-storage/chat/a.jpg" {
		t.Fatalf("şəkil: %v", m)
	}
	gap()
	a.send("chat_message", map[string]any{"gif_url": "https://media2.giphy.com/media/x/giphy.gif"})
	b.want("chat_message")
	for _, bad := range []string{"https://evil.example/a.gif", "http://api.beanpon.com/a.gif", "//evil.example/a", "../../etc/passwd", "https://beanpon.com.evil.io/a.gif"} {
		gap()
		a.send("chat_message", map[string]any{"gif_url": bad})
		if e := a.want("error"); e["code"] != "bad_media" {
			t.Fatalf("%q keçdi: %v", bad, e)
		}
	}
	gap()
	a.send("chat_message", map[string]any{"sound_url": "https://api.beanpon.com/s/1.mp3", "sound_id": "17", "sound_title": "gülüş", "sound_dur": 2400, "color": "mint"})
	if m := b.want("chat_message"); m["sound_id"].(float64) != 17 || m["sound_title"] != "gülüş" || m["color"] != "mint" {
		t.Fatalf("Piound: %v", m)
	}

	// Blok: 2 və 3 bir-birini görmür (canlıda da, tarixçədə də).
	gap()
	a.drain()
	c.drain()
	b.say("fidandan")
	a.want("chat_message")
	c.quiet("chat_message")
	// Kölgə ban: yazan özü görür, başqaları yox.
	d.drain()
	a.drain()
	d.say("reklam")
	if m := d.want("chat_message"); m["text"] != "reklam" {
		t.Fatalf("kölgə: %v", m)
	}
	a.quiet("chat_message")
	_, j := r.join("room", 3, RolePlayer, "murad")
	for _, raw := range j["messages"].([]any) {
		if txt := raw.(map[string]any)["text"]; txt == "fidandan" || txt == "reklam" {
			t.Fatalf("tarixçədə görünməməli idi: %v", txt)
		}
	}

	// Çox uzun mətn.
	gap()
	a.say(strings.Repeat("a", maxText+1))
	if e := a.want("error"); e["code"] != "too_long" {
		t.Fatalf("uzun mətn: %v", e)
	}
}

func TestReconnectReplacesAndSweep(t *testing.T) {
	r := newRig(t, Deps{})
	a1, _ := r.join("room", 1, RoleHost, "ilkin")
	a2, _ := r.join("room", 1, RoleHost, "ilkin")
	b, _ := r.join("room", 2, RolePlayer, "fidan")
	b.say("salam")
	a2.want("chat_message")
	// Köhnə bağlantı bağlanıb.
	select {
	case _, ok := <-a1.in:
		if ok {
			a1.quiet("chat_message")
		}
	case <-time.After(time.Second):
		t.Fatal("köhnə bağlantı açıq qaldı")
	}
	_ = a2.conn.Close()
	_ = b.conn.Close()
	time.Sleep(80 * time.Millisecond)
	r.m.Sweep(time.Now().Add(emptyRoomLife / 2))
	if r.m.Rooms() != 1 {
		t.Fatal("otaq tez silindi")
	}
	r.m.Sweep(time.Now().Add(emptyRoomLife + time.Minute))
	if r.m.Rooms() != 0 {
		t.Fatal("boş otaq silinmədi")
	}
}
