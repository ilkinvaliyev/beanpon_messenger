package watch

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
	Subprotocols:    []string{"bearer"},
}

const (
	wsReadLimit  = 16 << 10
	wsPongWait   = 60 * time.Second
	wsPingPeriod = 25 * time.Second
	wsWriteWait  = 10 * time.Second
)

type clientFrame struct {
	Type string          `json:"type"`
	ID   string          `json:"id"`
	Data json.RawMessage `json:"data"`
}

// HandleWS — GET /ws/watch?room_id=&token= (JWT middleware in front).
func (m *Module) HandleWS(c *gin.Context) {
	uid := currentUserID(c)
	roomID, err := strconv.ParseInt(c.Query("room_id"), 10, 64)
	if uid == 0 || err != nil || roomID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "room_id gerekli", "code": "INVALID_INPUT"})
		return
	}
	if !m.settings.get().s.Enabled {
		fail(c, errDisabled)
		return
	}
	// Pre-check before the upgrade (a plain HTTP error is easier for clients).
	err = m.withRoom(roomID, func(r *liveRoom) error {
		if r.row.Status == "ended" {
			return errEnded
		}
		if r.member(uid) == nil {
			return errNotMember
		}
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	blocked := m.blockedWith(uid)

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("[watch] upgrade: %v", err)
		return
	}
	conn.SetReadLimit(wsReadLimit)

	cl := &wsClient{
		m:       m,
		userID:  uid,
		conn:    conn,
		send:    make(chan []byte, 256),
		done:    make(chan struct{}),
		blocked: blocked,
		since:   time.Now(),
	}
	go cl.writePump()

	err = m.withRoom(roomID, func(r *liveRoom) error {
		if r.row.Status == "ended" {
			return errEnded
		}
		if r.member(uid) == nil {
			return errNotMember
		}
		m.registerLocked(r, cl)
		return nil
	})
	if err != nil {
		if b := encodeFrame(roomID, "error", gin.H{"code": codeOf(err)}); b != nil {
			cl.enqueue(b)
		}
		cl.close()
		return
	}
	go cl.readPump()
}

func codeOf(err error) string {
	if ae, ok := err.(*apiError); ok {
		return ae.Code
	}
	return "SERVER_ERROR"
}

// registerLocked — attach the client, replace an older socket of the same
// user, send the snapshot, tell the others.
func (m *Module) registerLocked(r *liveRoom, cl *wsClient) {
	uid := cl.userID
	cl.room = r
	if old := r.clients[uid]; old != nil && old != cl {
		if b := encodeFrame(r.id, "replaced", map[string]any{}); b != nil {
			old.enqueue(b)
		}
		old.close()
	}
	wasOnline := r.clients[uid] != nil
	r.clients[uid] = cl
	if _, ok := r.ready[uid]; !ok {
		r.ready[uid] = "loading"
	}
	r.countDirty = true
	r.lastClient = time.Now()
	if r.row.Status == "live" && r.row.CurrentSessionID != nil && !r.sessionEnded {
		sid := *r.row.CurrentSessionID
		go m.addSessionViewers(sid, []int64{uid})
	}
	go m.db.Exec(`UPDATE watch_room_members SET last_seen_at = NOW() WHERE room_id = ? AND user_id = ?`, r.id, uid)

	if b := encodeFrame(r.id, "state", m.stateSnapshotLocked(r, uid)); b != nil {
		cl.enqueue(b)
	}
	if !wasOnline {
		r.broadcast("presence", map[string]any{"user_id": uid, "online": true}, func(c *wsClient) bool { return c.userID != uid })
		if mb := r.members[uid]; mb != nil {
			r.broadcast("member", map[string]any{"member": m.memberJSONLocked(r, mb, time.Now()), "change": "updated"},
				func(c *wsClient) bool { return c.userID != uid })
		}
	}
}

func (m *Module) stateSnapshotLocked(r *liveRoom, uid int64) map[string]any {
	ready := map[string]string{}
	for id, st := range r.ready {
		if _, online := r.clients[id]; online {
			ready[strconv.FormatInt(id, 10)] = st
		}
	}
	var pause any
	if r.pause != nil {
		pause = m.pauseJSONLocked(r)
	}
	var countdown any
	if r.countdownS != 0 && r.countdownS > nowMs() {
		countdown = r.countdownS
	}
	return map[string]any{
		"room":               m.detailLocked(r, uid),
		"playback":           r.playbackJSON(nowMs()),
		"online_ids":         r.onlineIDs(),
		"ready":              ready,
		"pause_request":      pause,
		"active_poll":        m.pollJSONLocked(r, uid),
		"countdown_start_ms": countdown,
		"session_id":         r.row.CurrentSessionID,
		"session_ended":      r.sessionEnded,
	}
}

// unregister — the socket closed (read error, replaced, kicked, room end).
func (m *Module) unregister(cl *wsClient) {
	r := cl.room
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.clients[cl.userID] != cl {
		return // replaced by a newer socket or already removed
	}
	uid := cl.userID
	delete(r.clients, uid)
	delete(r.ready, uid)
	delete(r.sync, uid)
	r.countDirty = true
	if r.row.Status != "ended" {
		pos := r.expected(nowMs())
		go m.db.Exec(`UPDATE watch_room_members SET last_seen_at = NOW(), last_position_ms = ? WHERE room_id = ? AND user_id = ?`, pos, r.id, uid)
		r.broadcast("presence", map[string]any{"user_id": uid, "online": false}, nil)
		if mb := r.members[uid]; mb != nil && mb.State == "joined" {
			r.broadcast("member", map[string]any{"member": m.memberJSONLocked(r, mb, time.Now()), "change": "updated"}, nil)
		}
	}
}

func (cl *wsClient) readPump() {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[watch] readPump panic (user %d): %v", cl.userID, rec)
		}
		cl.m.unregister(cl)
		cl.close()
	}()
	_ = cl.conn.SetReadDeadline(time.Now().Add(wsPongWait))
	cl.conn.SetPongHandler(func(string) error {
		return cl.conn.SetReadDeadline(time.Now().Add(wsPongWait))
	})
	for {
		_, data, err := cl.conn.ReadMessage()
		if err != nil {
			return
		}
		_ = cl.conn.SetReadDeadline(time.Now().Add(wsPongWait))
		var f clientFrame
		if json.Unmarshal(data, &f) != nil || f.Type == "" {
			continue
		}
		cl.m.handleFrame(cl, f)
	}
}

func (cl *wsClient) writePump() {
	ticker := time.NewTicker(wsPingPeriod)
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[watch] writePump panic (user %d): %v", cl.userID, rec)
		}
		ticker.Stop()
		_ = cl.conn.Close()
	}()
	for {
		select {
		case <-cl.done:
			// Flush what is queued (e.g. "ended" / "kicked") before closing.
			for {
				select {
				case msg := <-cl.send:
					_ = cl.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
					if cl.conn.WriteMessage(websocket.TextMessage, msg) != nil {
						return
					}
				default:
					_ = cl.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
					_ = cl.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
					return
				}
			}
		case msg := <-cl.send:
			_ = cl.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if err := cl.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			_ = cl.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if err := cl.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// ── Frame dispatch ────────────────────────────────────────────────────────

func (m *Module) ack(cl *wsClient, id string, err error) {
	if id == "" {
		return
	}
	data := map[string]any{"id": id, "ok": err == nil}
	if err != nil {
		data["code"] = codeOf(err)
	}
	if b := encodeFrame(cl.room.id, "ack", data); b != nil {
		cl.enqueue(b)
	}
}

func (m *Module) handleFrame(cl *wsClient, f clientFrame) {
	r := cl.room
	if f.Type == "ping" {
		var p struct {
			T json.Number `json:"t"`
		}
		_ = json.Unmarshal(f.Data, &p)
		if b := encodeFrame(r.id, "pong", map[string]any{"t": p.T, "server_ms": nowMs()}); b != nil {
			cl.enqueue(b)
		}
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gone || r.clients[cl.userID] != cl {
		return
	}
	uid := cl.userID
	if r.member(uid) == nil {
		return
	}
	// A flood of frames (bug or abuse) is dropped before any work.
	if !r.userLimits(uid).frames.allow(time.Now(), 20, 40) {
		m.ack(cl, f.ID, errRateLimited)
		return
	}
	var err error
	switch f.Type {
	case "control":
		err = m.wsControlLocked(r, uid, f.Data)
	case "report":
		m.wsReportLocked(r, uid, f.Data)
	case "ready":
		m.wsReadyLocked(r, uid, f.Data)
	case "reaction":
		err = m.wsReactionLocked(r, uid, f.Data)
	case "pause_request":
		err = m.wsPauseRequestLocked(r, uid)
	case "pause_support":
		err = m.wsPauseSupportLocked(r, uid)
	case "pause_cancel":
		err = m.wsPauseCancelLocked(r, uid)
	case "break":
		err = m.wsBreakLocked(r, uid, f.Data)
	case "break_end":
		if !r.isController(uid) {
			err = errNotController
		} else {
			err = m.endBreakLocked(r, uid)
		}
	case "resync_user":
		err = m.wsResyncUserLocked(r, uid, f.Data)
	case "media_ended":
		err = m.wsMediaEndedLocked(r, uid)
	default:
		err = errUnsupported // a newer client's frame: it learns, nothing breaks
	}
	m.ack(cl, f.ID, err)
}

var errUnsupported = errf(http.StatusBadRequest, "UNSUPPORTED", "Bu əməliyyat dəstəklənmir")
