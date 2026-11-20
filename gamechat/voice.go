package gamechat

// Voice chat of a game table ("masada səsli söhbət").
//
// One voice session per table, started by someone who PLAYS at it (host /
// player); everyone at the table — spectators too — sees that it is on and
// joins only when they ask to. Nobody is ever connected automatically.
//
// The audio itself flows through LiveKit; this file only keeps "who is in" and
// hands out LiveKit access tokens. The LiveKit identity is the member's `sid`
// (the table-scoped key), so no account id reaches the wire here either.
//
// Contract: docs/GAME_CHAT.md, section "Səsli söhbət".

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

const (
	voiceMax = 30 // people in one voice session
	// voiceGrace — a member whose chat socket dropped keeps the place this long
	// (a reconnect is normal on mobile networks).
	voiceGrace     = 25 * time.Second
	voiceTokenLife = 3 * time.Hour
)

type voiceMember struct {
	uid    uint64
	sid    string
	name   string
	avatar string
	role   string
	muted  bool
	gone   bool   // the chat socket is down (grace running)
	seq    uint64 // bumped on every drop, so an old grace timer never fires late
}

type voiceSession struct {
	by     string // who started it (name)
	bySID  string
	byUID  uint64
	at     int64 // ms
	order  []uint64
	people map[uint64]*voiceMember
	kicked map[uint64]bool
}

// voiceOn — LiveKit is configured for this process.
func (m *Module) voiceOn() bool {
	return m.deps.LiveKitURL != "" && m.deps.LiveKitKey != "" && m.deps.LiveKitSecret != ""
}

// voiceRoom — the LiveKit room of a table: stable for the table, not guessable.
func (m *Module) voiceRoom(rid string) string {
	mac := hmac.New(sha256.New, []byte(m.deps.Secret))
	mac.Write([]byte("voice|" + rid))
	return "gv-" + hex.EncodeToString(mac.Sum(nil)[:12])
}

// ---------------------------------------------------------------------------
// Wire form
// ---------------------------------------------------------------------------

// voiceWire — the voice state as `c` sees it. `blocked` marks members `c` has
// a block with (either way): the client keeps them silent.
func (m *Module) voiceWire(r *room, c *client, event map[string]any) map[string]any {
	out := map[string]any{"available": m.voiceOn(), "active": false, "count": 0, "members": []any{}}
	if event != nil {
		out["event"] = event
	}
	v := r.voice
	if v == nil {
		return out
	}
	list := make([]map[string]any, 0, len(v.order))
	for _, uid := range v.order {
		p := v.people[uid]
		if p == nil {
			continue
		}
		item := map[string]any{"sid": p.sid, "name": p.name, "role": p.role, "muted": p.muted}
		if p.avatar != "" {
			item["avatar"] = p.avatar
		}
		if c != nil && c.blocked(p.uid) {
			item["blocked"] = true
		}
		list = append(list, item)
	}
	out["active"] = true
	out["by"] = v.by
	out["by_sid"] = v.bySID
	out["at"] = v.at
	out["count"] = len(list)
	out["members"] = list
	return out
}

// sendVoice — the fresh state to everyone at the table (room lock held).
func (m *Module) sendVoice(r *room, event map[string]any) {
	for c := range r.members {
		c.push(frame("voice", m.voiceWire(r, c, event)))
	}
}

// ---------------------------------------------------------------------------
// Frames (room lock held, `c` is a member of the room)
// ---------------------------------------------------------------------------

// voiceFrame — handles a voice_* frame; false = not a voice frame.
func (m *Module) voiceFrame(r *room, c *client, kind string, data json.RawMessage) bool {
	var d struct {
		Muted bool   `json:"muted"`
		SID   string `json:"sid"`
	}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &d)
	}
	switch kind {
	case "voice_start":
		m.voiceJoin(r, c, d.Muted, true)
	case "voice_join":
		m.voiceJoin(r, c, d.Muted, false)
	case "voice_leave":
		m.voiceRemove(r, c.uid, nil)
	case "voice_mute":
		if v := r.voice; v != nil {
			if p := v.people[c.uid]; p != nil && p.muted != d.Muted {
				p.muted = d.Muted
				m.sendVoice(r, nil)
			}
		}
	case "voice_end":
		v := r.voice
		if v == nil || (c.role != RoleHost && v.byUID != c.uid) {
			return true
		}
		m.voiceClose(r, map[string]any{"kind": "ended", "sid": c.sid, "name": c.name})
	case "voice_mute_user":
		v := r.voice
		if v == nil || c.role != RoleHost {
			return true
		}
		for _, p := range v.people {
			if p.sid == d.SID && p.uid != c.uid && !p.muted {
				p.muted = true
				m.sendVoice(r, map[string]any{"kind": "muted", "sid": p.sid, "name": c.name})
				break
			}
		}
	case "voice_kick":
		v := r.voice
		if v == nil || c.role != RoleHost {
			return true
		}
		for _, p := range v.people {
			if p.sid == d.SID && p.uid != c.uid {
				v.kicked[p.uid] = true
				m.liveKitCall("RemoveParticipant", r.id, map[string]any{"room": m.voiceRoom(r.id), "identity": p.sid})
				m.voiceRemove(r, p.uid, map[string]any{"kind": "kicked", "sid": p.sid, "name": c.name})
				break
			}
		}
	default:
		return false
	}
	return true
}

// voiceJoin — puts `c` into the table's voice session and sends it a token.
// `start` may open a new session (players only); a plain join never does.
func (m *Module) voiceJoin(r *room, c *client, muted, start bool) {
	if !m.voiceOn() || c.shadow {
		c.fail("voice_unavailable", "")
		return
	}
	v := r.voice
	var event map[string]any
	if v == nil {
		if !start {
			c.fail("voice_off", "")
			return
		}
		if c.role != RoleHost && c.role != RolePlayer {
			c.fail("voice_players_only", "")
			return
		}
		v = &voiceSession{
			by: c.name, bySID: c.sid, byUID: c.uid, at: time.Now().UnixMilli(),
			people: make(map[uint64]*voiceMember), kicked: make(map[uint64]bool),
		}
		r.voice = v
		event = map[string]any{"kind": "started", "sid": c.sid, "name": c.name}
	}
	if v.kicked[c.uid] {
		c.fail("voice_kicked", "")
		return
	}
	p := v.people[c.uid]
	if p == nil {
		if len(v.people) >= voiceMax {
			c.fail("voice_full", "")
			return
		}
		p = &voiceMember{uid: c.uid}
		v.people[c.uid] = p
		v.order = append(v.order, c.uid)
	}
	p.sid, p.name, p.avatar, p.role = c.sid, c.name, c.avatar, c.role
	p.muted, p.gone = muted, false
	p.seq++
	r.active = time.Now()

	room := m.voiceRoom(r.id)
	c.push(frame("voice_token", map[string]any{
		"url":   m.deps.LiveKitURL,
		"room":  room,
		"token": m.liveKitToken(room, p.sid, p.name, false),
		"muted": muted,
	}))
	m.sendVoice(r, event)
}

// voiceRemove — takes one member out; the session ends with its last member.
func (m *Module) voiceRemove(r *room, uid uint64, event map[string]any) {
	v := r.voice
	if v == nil || v.people[uid] == nil {
		return
	}
	delete(v.people, uid)
	for i, id := range v.order {
		if id == uid {
			v.order = append(v.order[:i], v.order[i+1:]...)
			break
		}
	}
	if len(v.people) == 0 {
		if event == nil {
			event = map[string]any{"kind": "ended"}
		}
		m.voiceClose(r, event)
		return
	}
	m.sendVoice(r, event)
}

// voiceClose — ends the session for everyone (also drops the LiveKit room, so
// a client that missed the frame is disconnected too).
func (m *Module) voiceClose(r *room, event map[string]any) {
	if r.voice == nil {
		return
	}
	r.voice = nil
	m.liveKitCall("DeleteRoom", r.id, map[string]any{"room": m.voiceRoom(r.id)})
	m.sendVoice(r, event)
}

// voiceSocketGone — the member's chat socket closed: keep the place for
// `voiceGrace`, then take it away unless the member came back.
func (m *Module) voiceSocketGone(r *room, uid uint64) {
	v := r.voice
	if v == nil {
		return
	}
	p := v.people[uid]
	if p == nil {
		return
	}
	p.gone = true
	p.seq++
	seq := p.seq
	time.AfterFunc(voiceGrace, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.voice != v {
			return
		}
		if q := v.people[uid]; q != nil && q.gone && q.seq == seq {
			m.liveKitCall("RemoveParticipant", r.id, map[string]any{"room": m.voiceRoom(r.id), "identity": q.sid})
			m.voiceRemove(r, uid, nil)
		}
	})
}

// voiceSocketBack — the member's socket is here again (reconnect).
func (m *Module) voiceSocketBack(r *room, c *client) {
	if v := r.voice; v != nil {
		if p := v.people[c.uid]; p != nil {
			p.gone = false
			p.seq++
			p.role = c.role
		}
	}
}

// voiceRole — the member's role changed (a fresh ticket).
func (m *Module) voiceRole(r *room, c *client) {
	if v := r.voice; v != nil {
		if p := v.people[c.uid]; p != nil && p.role != c.role {
			p.role = c.role
			m.sendVoice(r, nil)
		}
	}
}

// ---------------------------------------------------------------------------
// LiveKit
// ---------------------------------------------------------------------------

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// liveKitToken — a LiveKit access token (HS256 JWT, the standard claim set).
// Members may publish the microphone only; `admin` tokens are for our own
// RoomService calls.
func (m *Module) liveKitToken(room, identity, name string, admin bool) string {
	now := time.Now()
	grant := map[string]any{"room": room}
	life := voiceTokenLife
	if admin {
		grant["roomAdmin"] = true
		grant["roomCreate"] = true
		life = time.Minute
	} else {
		grant["roomJoin"] = true
		grant["canPublish"] = true
		grant["canSubscribe"] = true
		grant["canPublishData"] = false
		grant["canPublishSources"] = []string{"microphone"}
	}
	claims := map[string]any{
		"iss":   m.deps.LiveKitKey,
		"nbf":   now.Add(-10 * time.Second).Unix(),
		"exp":   now.Add(life).Unix(),
		"video": grant,
	}
	if identity != "" {
		claims["sub"] = identity
		claims["jti"] = identity
	}
	if name != "" {
		claims["name"] = name
	}
	body, _ := json.Marshal(claims)
	head := b64([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + b64(body)
	mac := hmac.New(sha256.New, []byte(m.deps.LiveKitSecret))
	mac.Write([]byte(head))
	return head + "." + b64(mac.Sum(nil))
}

var liveKitHTTP = &http.Client{Timeout: 5 * time.Second}

// liveKitCall — a best-effort RoomService call (RemoveParticipant / DeleteRoom).
// It never blocks the room: the request runs on its own goroutine and a failure
// is ignored (the member list is the source of truth for the clients).
func (m *Module) liveKitCall(method, rid string, body map[string]any) {
	if m.deps.LiveKitCall != nil {
		m.deps.LiveKitCall(method, body)
		return
	}
	if !m.voiceOn() {
		return
	}
	base := m.deps.LiveKitURL
	switch {
	case strings.HasPrefix(base, "wss://"):
		base = "https://" + strings.TrimPrefix(base, "wss://")
	case strings.HasPrefix(base, "ws://"):
		base = "http://" + strings.TrimPrefix(base, "ws://")
	}
	token := m.liveKitToken(m.voiceRoom(rid), "", "", true)
	payload, _ := json.Marshal(body)
	go func() {
		defer func() { _ = recover() }()
		req, err := http.NewRequest(http.MethodPost,
			strings.TrimRight(base, "/")+"/twirp/livekit.RoomService/"+method, bytes.NewReader(payload))
		if err != nil {
			return
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		if resp, err := liveKitHTTP.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
}
