package watch

import (
	"encoding/json"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ── WebSocket client ──────────────────────────────────────────────────────

type wsClient struct {
	m       *Module
	room    *liveRoom
	userID  int64
	conn    *websocket.Conn
	send    chan []byte
	done    chan struct{}
	once    sync.Once
	blocked map[int64]bool // users with a block either way (loaded on connect)
	since   time.Time
}

// close — idempotent stop signal. `send` is never closed (no send-on-closed panics).
func (c *wsClient) close() {
	c.once.Do(func() { close(c.done) })
}

// enqueue — non-blocking; a full buffer means a stuck client → drop it (it
// reconnects and gets a fresh `state`).
func (c *wsClient) enqueue(b []byte) bool {
	select {
	case <-c.done:
		return false
	default:
	}
	select {
	case c.send <- b:
		return true
	default:
		c.close()
		return false
	}
}

type frame struct {
	Type     string `json:"type"`
	RoomID   int64  `json:"room_id"`
	Data     any    `json:"data"`
	ServerMs int64  `json:"server_ms"`
}

func encodeFrame(roomID int64, typ string, data any) []byte {
	b, err := json.Marshal(frame{Type: typ, RoomID: roomID, Data: data, ServerMs: nowMs()})
	if err != nil {
		log.Printf("[watch] encode %s: %v", typ, err)
		return nil
	}
	return b
}

// ── Room state ────────────────────────────────────────────────────────────

type syncState struct {
	State   string
	DriftMs int64
	At      time.Time
}

type pauseRequest struct {
	UserID     int64
	Supporters []int64
	CreatedAt  time.Time
}

type reactKey struct {
	Session int64
	Bucket  int
	Emoji   string
}

type limits struct {
	msg, react, ctrl, frames tokenBucket
}

// liveRoom — one room in memory. Every field is guarded by mu. DB writes
// happen under mu so the in-memory copy and the row never diverge; pushes and
// other HTTP calls never run under mu.
type liveRoom struct {
	mu  sync.Mutex
	id  int64
	row roomRow

	members map[int64]*memberRow
	users   map[int64]UserLite
	clients map[int64]*wsClient
	ready   map[int64]string
	sync    map[int64]syncState

	pause      *pauseRequest
	pauseAsked map[int64]time.Time

	pinned     *MessageJSON
	poll       *pollRow // the open poll (nil = none)
	pollVotes  map[int64]string
	pollOpts   []pollOption
	countdownS int64 // start_at_ms of the running countdown (0 = none)

	reactBuf     map[reactKey]int
	sessionEnded bool
	watchedMs    int64
	peakOnline   int

	lim        map[int64]*limits
	remindAt   time.Time
	helpAt     map[int64]time.Time
	lastTick   time.Time
	lastFlush  time.Time
	lastReport time.Time
	lastTouch  time.Time
	lastClient time.Time
	hostAway   time.Time // host offline while others are online (auto transfer)
	countDirty bool
	gone       bool // evicted — holders must reload
}

func (r *liveRoom) userLimits(uid int64) *limits {
	l := r.lim[uid]
	if l == nil {
		l = &limits{}
		r.lim[uid] = l
	}
	return l
}

func (r *liveRoom) member(uid int64) *memberRow {
	if mb := r.members[uid]; mb != nil && mb.State == "joined" {
		return mb
	}
	return nil
}

func (r *liveRoom) isHost(uid int64) bool { return r.row.HostID == uid }

func (r *liveRoom) isController(uid int64) bool {
	if r.row.HostID == uid {
		return true
	}
	mb := r.member(uid)
	return mb != nil && mb.Role == "cohost"
}

func (r *liveRoom) joinedCount() int {
	n := 0
	for _, mb := range r.members {
		if mb.State == "joined" {
			n++
		}
	}
	return n
}

func (r *liveRoom) controllerIDs() []int64 {
	ids := []int64{r.row.HostID}
	for uid, mb := range r.members {
		if mb.State == "joined" && mb.Role == "cohost" && uid != r.row.HostID {
			ids = append(ids, uid)
		}
	}
	return ids
}

func (r *liveRoom) onlineIDs() []int64 {
	ids := make([]int64, 0, len(r.clients))
	for uid := range r.clients {
		ids = append(ids, uid)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// broadcast — every connected client passing keep (nil = all).
func (r *liveRoom) broadcast(typ string, data any, keep func(c *wsClient) bool) {
	if len(r.clients) == 0 {
		return
	}
	b := encodeFrame(r.id, typ, data)
	if b == nil {
		return
	}
	for _, c := range r.clients {
		if keep == nil || keep(c) {
			c.enqueue(b)
		}
	}
}

// broadcastFrom — like broadcast, but skips clients that have a block with
// the sender (chat, reactions).
func (r *liveRoom) broadcastFrom(sender int64, typ string, data any) {
	r.broadcast(typ, data, func(c *wsClient) bool { return sender == 0 || !c.blocked[sender] })
}

func (r *liveRoom) sendTo(uid int64, typ string, data any) {
	if c := r.clients[uid]; c != nil {
		if b := encodeFrame(r.id, typ, data); b != nil {
			c.enqueue(b)
		}
	}
}

func (r *liveRoom) toControllers(typ string, data any) {
	r.broadcast(typ, data, func(c *wsClient) bool { return r.isController(c.userID) })
}

// ── Hub ───────────────────────────────────────────────────────────────────

// Hub — the rooms loaded in this process. Like the live hub it is
// per-instance (messenger runs one replica); the DB stays the source of
// truth, so a reload always rebuilds the same state.
type Hub struct {
	m     *Module
	mu    sync.Mutex
	rooms map[int64]*liveRoom

	lastExternalCheck time.Time
}

func newHub(m *Module) *Hub {
	return &Hub{m: m, rooms: map[int64]*liveRoom{}}
}

func (h *Hub) loaded(id int64) *liveRoom {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.rooms[id]
}

func (h *Hub) get(id int64) (*liveRoom, error) {
	if r := h.loaded(id); r != nil {
		return r, nil
	}
	r, err := h.m.loadRoom(id)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if existing := h.rooms[id]; existing != nil {
		return existing, nil
	}
	h.rooms[id] = r
	return r, nil
}

func (h *Hub) snapshot() []*liveRoom {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*liveRoom, 0, len(h.rooms))
	for _, r := range h.rooms {
		out = append(out, r)
	}
	return out
}

// forget — drop an evicted room from the map (only if it is still that object).
func (h *Hub) forget(r *liveRoom) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.rooms[r.id] == r {
		delete(h.rooms, r.id)
	}
}

// locked runs fn with the room lock held; a panic in fn can never leave the
// room locked (that would freeze the room, the tick and every list).
func (r *liveRoom) locked(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn()
}

// onlineByRoom — connected user ids per loaded room (discovery cards). A
// room busy right now is skipped (its card falls back to the row's
// watching_count) — one slow room must never stall every list.
func (h *Hub) onlineByRoom() map[int64][]int64 {
	out := map[int64][]int64{}
	for _, r := range h.snapshot() {
		if !r.mu.TryLock() {
			continue
		}
		func() {
			defer r.mu.Unlock()
			if !r.gone && len(r.clients) > 0 {
				out[r.id] = r.onlineIDs()
			}
		}()
	}
	return out
}

// withRoom runs fn under the room lock (loading the room on demand).
func (m *Module) withRoom(id int64, fn func(r *liveRoom) error) error {
	for attempt := 0; attempt < 3; attempt++ {
		r, err := m.hub.get(id)
		if err != nil {
			return err
		}
		gone := false
		r.locked(func() {
			if r.gone {
				gone = true
				return
			}
			err = fn(r)
		})
		if gone {
			m.hub.forget(r)
			continue
		}
		return err
	}
	return errServer
}

// loadRoom — builds a liveRoom from the DB (no lock needed: not shared yet).
func (m *Module) loadRoom(id int64) (*liveRoom, error) {
	var row roomRow
	res := m.db.Where("id = ?", id).Limit(1).Find(&row)
	if res.Error != nil {
		return nil, errServer
	}
	if res.RowsAffected == 0 {
		return nil, errNotFound
	}
	r := &liveRoom{
		id:         id,
		row:        row,
		members:    map[int64]*memberRow{},
		users:      map[int64]UserLite{},
		clients:    map[int64]*wsClient{},
		ready:      map[int64]string{},
		sync:       map[int64]syncState{},
		pauseAsked: map[int64]time.Time{},
		pollVotes:  map[int64]string{},
		reactBuf:   map[reactKey]int{},
		lim:        map[int64]*limits{},
		helpAt:     map[int64]time.Time{},
		lastTick:   time.Now(),
		lastFlush:  time.Now(),
		lastTouch:  row.CreatedAt,
		lastClient: time.Now(),
	}
	if row.LastActiveAt != nil {
		r.lastTouch = *row.LastActiveAt
	}
	var members []memberRow
	if err := m.db.Where("room_id = ?", id).Find(&members).Error; err != nil {
		return nil, errServer
	}
	ids := []int64{row.HostID}
	for i := range members {
		mb := members[i]
		r.members[mb.UserID] = &mb
		if mb.State == "joined" {
			ids = append(ids, mb.UserID)
		}
	}
	r.users = m.loadUsers(ids)

	if row.CurrentSessionID != nil {
		var s sessionRow
		if m.db.Where("id = ?", *row.CurrentSessionID).Limit(1).Find(&s).RowsAffected > 0 {
			r.sessionEnded = s.EndedAt != nil
			r.watchedMs = s.WatchedMs
			r.peakOnline = s.PeakViewers
		} else {
			r.sessionEnded = true
		}
	}
	if row.PinnedMessageID != nil {
		r.pinned = m.loadMessageJSON(id, *row.PinnedMessageID, r.users)
	}
	m.loadOpenPoll(r)

	// A break that ran out while nobody had the room loaded.
	if row.BreakUntilMs != nil && *row.BreakUntilMs <= nowMs() {
		r.row.BreakUntilMs = nil
		m.db.Exec(`UPDATE watch_rooms SET break_until_ms = NULL WHERE id = ?`, id)
	}
	return r, nil
}

// ensureUser — UserLite for a member (cached in the room).
func (m *Module) ensureUserLocked(r *liveRoom, uid int64) UserLite {
	if u, ok := r.users[uid]; ok {
		return u
	}
	u, ok := m.loadUser(uid)
	if !ok {
		u = UserLite{ID: uid}
	}
	r.users[uid] = u
	return u
}

// ── Clock ─────────────────────────────────────────────────────────────────

func (r *liveRoom) durationMs() int64 {
	if r.row.MediaDurationMs != nil {
		return *r.row.MediaDurationMs
	}
	return 0
}

// expected — the room clock at server time now (ms).
func (r *liveRoom) expected(now int64) int64 {
	pos := r.row.PositionMs
	if r.row.IsPlaying {
		elapsed := now - r.row.PositionUpdatedMs
		if elapsed > 0 {
			rate := r.row.PlaybackRate
			if rate <= 0 {
				rate = 1
			}
			pos += int64(float64(elapsed) * rate)
		}
	}
	if pos < 0 {
		pos = 0
	}
	if d := r.durationMs(); d > 0 && pos > d {
		pos = d
	}
	return pos
}

func (r *liveRoom) playbackJSON(now int64) PlaybackJSON {
	rate := r.row.PlaybackRate
	if rate <= 0 {
		rate = 1
	}
	return PlaybackJSON{
		PositionMs:   r.row.PositionMs,
		IsPlaying:    r.row.IsPlaying,
		Rate:         rate,
		UpdatedAtMs:  r.row.PositionUpdatedMs,
		ServerMs:     now,
		BreakUntilMs: r.row.BreakUntilMs,
		Version:      r.row.StateVersion,
	}
}

// setPlayback — new clock state; persisted + broadcast. On a DB error the
// previous state is restored.
func (m *Module) setPlaybackLocked(r *liveRoom, pos int64, playing bool, rate float64, updatedMs int64, action string, actor int64) error {
	prev := r.row
	if d := r.durationMs(); d > 0 && pos > d {
		pos = d
	}
	if pos < 0 {
		pos = 0
	}
	if rate <= 0 {
		rate = 1
	}
	r.row.PositionMs = pos
	r.row.IsPlaying = playing
	r.row.PlaybackRate = rate
	r.row.PositionUpdatedMs = updatedMs
	r.row.StateVersion++
	err := m.db.Exec(`UPDATE watch_rooms SET position_ms = ?, is_playing = ?, playback_rate = ?,
			position_updated_ms = ?, state_version = ?, break_until_ms = ?, last_active_at = NOW(), updated_at = NOW()
		WHERE id = ?`,
		r.row.PositionMs, r.row.IsPlaying, r.row.PlaybackRate, r.row.PositionUpdatedMs,
		r.row.StateVersion, r.row.BreakUntilMs, r.id).Error
	if err != nil {
		r.row = prev
		log.Printf("[watch] room %d playback save: %v", r.id, err)
		return errServer
	}
	r.lastTouch = time.Now()
	pb := r.playbackJSON(nowMs())
	r.broadcast("playback", struct {
		PlaybackJSON
		Action  string `json:"action"`
		ActorID int64  `json:"actor_id"`
	}{pb, action, actor}, nil)
	return nil
}

// startCountdownLocked — everyone starts playing at now+3 s from pos.
func (m *Module) startCountdownLocked(r *liveRoom, pos int64, action string, actor int64) error {
	return m.startCountdownAfterLocked(r, pos, 3, action, actor)
}

// startCountdownAfterLocked — the same with a longer countdown (the host's
// "start in N minutes" from the lobby). seconds is clamped to 3…600.
func (m *Module) startCountdownAfterLocked(r *liveRoom, pos int64, seconds int, action string, actor int64) error {
	if seconds < 3 {
		seconds = 3
	} else if seconds > 600 {
		seconds = 600
	}
	start := nowMs() + int64(seconds)*1000
	if err := m.setPlaybackLocked(r, pos, true, r.row.PlaybackRate, start, action, actor); err != nil {
		return err
	}
	r.countdownS = start
	r.broadcast("countdown", map[string]any{"start_at_ms": start, "position_ms": pos, "seconds": seconds}, nil)
	return nil
}

// ── Sessions ──────────────────────────────────────────────────────────────

func (m *Module) openSessionLocked(r *liveRoom) error {
	now := dbNow()
	s := sessionRow{
		RoomID:          r.id,
		Provider:        r.row.Provider,
		TitleID:         r.row.TitleID,
		MediaRef:        r.row.MediaRef,
		MediaTitle:      r.row.MediaTitle,
		MediaPosterURL:  r.row.MediaPosterURL,
		MediaDurationMs: r.row.MediaDurationMs,
		StartedAt:       &now,
		PeakViewers:     len(r.clients),
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := m.db.Create(&s).Error; err != nil {
		log.Printf("[watch] room %d open session: %v", r.id, err)
		return errServer
	}
	if err := m.db.Exec(`UPDATE watch_rooms SET current_session_id = ?, updated_at = NOW() WHERE id = ?`, s.ID, r.id).Error; err != nil {
		return errServer
	}
	r.row.CurrentSessionID = &s.ID
	r.sessionEnded = false
	r.watchedMs = 0
	r.peakOnline = len(r.clients)
	if ids := r.onlineIDs(); len(ids) > 0 {
		sid := s.ID
		go m.addSessionViewers(sid, ids)
	}
	return nil
}

func (m *Module) addSessionViewers(sessionID int64, ids []int64) {
	defer func() { _ = recover() }()
	for _, uid := range ids {
		m.db.Exec(`INSERT INTO watch_session_viewers (session_id, user_id, first_seen_at)
			VALUES (?, ?, NOW()) ON CONFLICT DO NOTHING`, sessionID, uid)
	}
}

// closeSessionLocked — the current session is over (media ended / changed /
// room ended). Idempotent.
func (m *Module) closeSessionLocked(r *liveRoom) {
	if r.row.CurrentSessionID == nil || r.sessionEnded {
		return
	}
	sid := *r.row.CurrentSessionID
	m.flushReactionsLocked(r)
	m.db.Exec(`UPDATE watch_room_sessions s SET
			ended_at = NOW(),
			watched_ms = ?,
			peak_viewers = GREATEST(s.peak_viewers, ?),
			viewers_count = (SELECT COUNT(*) FROM watch_session_viewers v WHERE v.session_id = s.id),
			messages_count = (SELECT COUNT(*) FROM watch_room_messages mm
				WHERE mm.session_id = s.id AND mm.deleted_at IS NULL
				AND mm.type IN ('text','voice','gif','photo','sound','moment')),
			updated_at = NOW()
		WHERE s.id = ?`, r.watchedMs, r.peakOnline, sid)
	r.sessionEnded = true
	r.broadcast("session_ended", map[string]any{"session_id": sid}, nil)
}

// ── Reactions ─────────────────────────────────────────────────────────────

func (m *Module) flushReactionsLocked(r *liveRoom) {
	r.lastFlush = time.Now()
	if len(r.reactBuf) == 0 {
		return
	}
	perSession := map[int64]int{}
	for k, n := range r.reactBuf {
		m.db.Exec(`INSERT INTO watch_room_reaction_stats (session_id, bucket, emoji, count)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (session_id, bucket, emoji) DO UPDATE SET count = watch_room_reaction_stats.count + EXCLUDED.count`,
			k.Session, k.Bucket, k.Emoji, n)
		perSession[k.Session] += n
	}
	for sid, n := range perSession {
		m.db.Exec(`UPDATE watch_room_sessions SET reactions_count = reactions_count + ?, updated_at = NOW() WHERE id = ?`, n, sid)
	}
	r.reactBuf = map[reactKey]int{}
}

// ── Lifecycle helpers ─────────────────────────────────────────────────────

// endRoomLocked — the room is over for everyone. Idempotent.
func (m *Module) endRoomLocked(r *liveRoom, reason string) error {
	if r.row.Status == "ended" {
		return nil
	}
	now := nowMs()
	pos := r.expected(now)
	m.closeSessionLocked(r)
	t := dbNow()
	err := m.db.Exec(`UPDATE watch_rooms SET status = 'ended', ended_at = NOW(), end_reason = ?,
			is_playing = false, position_ms = ?, position_updated_ms = ?, break_until_ms = NULL,
			watching_count = 0, updated_at = NOW()
		WHERE id = ? AND status <> 'ended'`, reason, pos, now, r.id).Error
	if err != nil {
		log.Printf("[watch] room %d end: %v", r.id, err)
		return errServer
	}
	m.db.Exec(`UPDATE watch_room_requests SET status = 'cancelled', updated_at = NOW()
		WHERE room_id = ? AND status = 'pending'`, r.id)
	if online := r.onlineIDs(); len(online) > 0 {
		m.db.Exec(`UPDATE watch_room_members SET last_position_ms = ?, last_seen_at = NOW(), updated_at = NOW()
			WHERE room_id = ? AND user_id IN ?`, pos, r.id, online)
	}
	if r.poll != nil {
		m.db.Exec(`UPDATE watch_room_polls SET closed_at = NOW(), updated_at = NOW() WHERE id = ? AND closed_at IS NULL`, r.poll.ID)
		r.poll = nil
	}
	r.row.Status = "ended"
	r.row.EndedAt = &t
	r.row.EndReason = &reason
	r.row.IsPlaying = false
	r.row.PositionMs = pos
	r.row.PositionUpdatedMs = now
	r.row.BreakUntilMs = nil
	r.pause = nil
	r.broadcast("ended", map[string]any{"end_reason": reason, "session_id": r.row.CurrentSessionID}, nil)
	for _, c := range r.clients {
		c.close()
	}
	return nil
}

// persistCounts — watching / member / peak counters (cheap, coalesced).
func (m *Module) persistCountsLocked(r *liveRoom) {
	r.countDirty = false
	watching := len(r.clients)
	members := r.joinedCount()
	if watching > r.row.PeakCount {
		r.row.PeakCount = watching
	}
	if watching > r.peakOnline {
		r.peakOnline = watching
	}
	if r.row.WatchingCount == watching && r.row.MemberCount == members {
		return
	}
	r.row.WatchingCount = watching
	r.row.MemberCount = members
	m.db.Exec(`UPDATE watch_rooms SET watching_count = ?, member_count = ?, peak_count = GREATEST(peak_count, ?),
			last_active_at = NOW() WHERE id = ?`, watching, members, r.row.PeakCount, r.id)
}

// ── Tick (every second) ───────────────────────────────────────────────────

func (h *Hub) tick(now time.Time) {
	rooms := h.snapshot()
	for _, r := range rooms {
		h.tickRoom(r, now)
	}
	if now.Sub(h.lastExternalCheck) >= 30*time.Second {
		h.lastExternalCheck = now
		h.checkExternalEnds(rooms)
		h.evictIdle(now)
	}
}

// rawPosition — the clock without the duration clamp (end detection).
func (r *liveRoom) rawPosition(now int64) int64 {
	pos := r.row.PositionMs
	if r.row.IsPlaying {
		if elapsed := now - r.row.PositionUpdatedMs; elapsed > 0 {
			rate := r.row.PlaybackRate
			if rate <= 0 {
				rate = 1
			}
			pos += int64(float64(elapsed) * rate)
		}
	}
	return pos
}

func (h *Hub) tickRoom(r *liveRoom, now time.Time) {
	m := h.m
	// A room busy with a slow request is skipped this second (the elapsed-time
	// math below tolerates it) — one room must never delay all the others.
	if !r.mu.TryLock() {
		return
	}
	defer r.mu.Unlock()
	if r.gone {
		return
	}
	nms := now.UnixMilli()
	elapsed := now.Sub(r.lastTick)
	r.lastTick = now
	if len(r.clients) > 0 {
		r.lastClient = now
	}
	if r.row.Status == "ended" {
		return
	}

	if r.row.Status == "live" {
		breakOn := r.row.BreakUntilMs != nil
		atEnd := r.durationMs() > 0 && r.expected(nms) >= r.durationMs()
		if r.row.IsPlaying && !breakOn && !atEnd && !r.sessionEnded && nms >= r.row.PositionUpdatedMs && elapsed > 0 && elapsed < 10*time.Second {
			r.watchedMs += elapsed.Milliseconds()
		}
		if breakOn && nms >= *r.row.BreakUntilMs {
			m.endBreakLocked(r, 0)
		}
		if r.countdownS != 0 && nms >= r.countdownS {
			r.countdownS = 0
		}
		if r.pause != nil && now.Sub(r.pause.CreatedAt) > 2*time.Minute {
			r.pause = nil
			r.broadcast("pause_request", nil, nil)
		}
		// Companion rooms have no player to report the end: the clock does.
		// Sync rooms wait for the players (media_ended) but end anyway 15 s
		// after the clock passed the duration (nobody reported it).
		if !r.sessionEnded && r.row.IsPlaying && atEnd {
			p := m.settings.get().provider(r.row.Provider)
			if p == nil || !p.isSync() || r.rawPosition(nms) >= r.durationMs()+15000 {
				m.mediaEndedLocked(r, 0)
			}
		}
		if now.Sub(r.lastReport) >= 5*time.Second {
			r.lastReport = now
			m.sendSyncReportLocked(r, now)
		}
	}
	if len(r.reactBuf) > 0 && now.Sub(r.lastFlush) >= 3*time.Second {
		m.flushReactionsLocked(r)
	}
	if r.countDirty {
		m.persistCountsLocked(r)
	}
	if len(r.clients) > 0 && now.Sub(r.lastTouch) >= 30*time.Second {
		r.lastTouch = now
		m.db.Exec(`UPDATE watch_rooms SET last_active_at = NOW() WHERE id = ?`, r.id)
		if r.row.CurrentSessionID != nil && !r.sessionEnded {
			m.db.Exec(`UPDATE watch_room_sessions SET watched_ms = ?, peak_viewers = GREATEST(peak_viewers, ?), updated_at = NOW() WHERE id = ?`,
				r.watchedMs, r.peakOnline, *r.row.CurrentSessionID)
		}
	}
	m.autoTransferHostLocked(r, now)
}

// sendSyncReportLocked — who is in sync (host + cohosts only).
func (m *Module) sendSyncReportLocked(r *liveRoom, now time.Time) {
	type item struct {
		UserID  int64  `json:"user_id"`
		State   string `json:"state"`
		DriftMs int64  `json:"drift_ms"`
	}
	items := make([]item, 0, len(r.clients))
	inSync := 0
	for _, uid := range r.onlineIDs() {
		st := r.sync[uid]
		state := st.State
		if state == "" || now.Sub(st.At) > 15*time.Second {
			state = "unknown"
		}
		if state == "in_sync" {
			inSync++
		}
		items = append(items, item{UserID: uid, State: state, DriftMs: st.DriftMs})
	}
	r.toControllers("sync_report", map[string]any{"in_sync": inSync, "total": len(items), "members": items})
}

// autoTransferHostLocked — the host has been offline for 5 minutes while
// other members watch: hand the room over (cohost first, then the earliest
// joined online member).
func (m *Module) autoTransferHostLocked(r *liveRoom, now time.Time) {
	if r.row.Status == "ended" || r.row.Status == "scheduled" {
		r.hostAway = time.Time{}
		return
	}
	_, hostOnline := r.clients[r.row.HostID]
	if hostOnline || len(r.clients) == 0 {
		r.hostAway = time.Time{}
		return
	}
	if r.hostAway.IsZero() {
		r.hostAway = now
		return
	}
	if now.Sub(r.hostAway) < 5*time.Minute {
		return
	}
	if next := m.nextHostLocked(r, true); next != 0 {
		_ = m.transferHostLocked(r, next, "auto")
	}
	r.hostAway = time.Time{}
}

// checkExternalEnds — a room ended from outside (Filament fallback writes
// the DB directly) is closed here within 30 s.
func (h *Hub) checkExternalEnds(rooms []*liveRoom) {
	ids := make([]int64, 0, len(rooms))
	for _, r := range rooms {
		ids = append(ids, r.id)
	}
	if len(ids) == 0 {
		return
	}
	var rows []struct {
		ID        int64
		Status    string
		EndReason *string
	}
	if err := h.m.db.Raw(`SELECT id, status, end_reason FROM watch_rooms WHERE id IN ?`, ids).Scan(&rows).Error; err != nil {
		return
	}
	status := map[int64]*struct {
		ID        int64
		Status    string
		EndReason *string
	}{}
	for i := range rows {
		status[rows[i].ID] = &rows[i]
	}
	for _, r := range rooms {
		row, exists := status[r.id]
		if exists && row.Status != "ended" {
			continue
		}
		// Runs on the tick goroutine: a busy room is retried next round.
		if !r.mu.TryLock() {
			continue
		}
		func() {
			defer r.mu.Unlock()
			if r.gone {
				return
			}
			if !exists {
				// The row is gone (host hard-deleted → cascade): drop the room.
				r.broadcast("ended", map[string]any{"end_reason": "admin", "session_id": nil}, nil)
				for _, c := range r.clients {
					c.close()
				}
				r.row.Status = "ended"
				r.gone = true
				return
			}
			if r.row.Status == "ended" {
				return
			}
			reason := "admin"
			if row.EndReason != nil && *row.EndReason != "" {
				reason = *row.EndReason
			}
			h.m.closeSessionLocked(r)
			r.row.Status = "ended"
			r.row.EndReason = &reason
			r.row.IsPlaying = false
			r.broadcast("ended", map[string]any{"end_reason": reason, "session_id": r.row.CurrentSessionID}, nil)
			for _, c := range r.clients {
				c.close()
			}
		}()
		if !exists {
			h.forget(r)
		}
	}
}

// evictIdle — rooms nobody is connected to leave memory (after flushing).
// Hub.mu is never held while a room lock is taken: busy rooms are skipped
// (TryLock) and come back next round.
func (h *Hub) evictIdle(now time.Time) {
	for _, r := range h.snapshot() {
		if !r.mu.TryLock() {
			continue
		}
		evict := false
		func() {
			defer r.mu.Unlock()
			if r.gone {
				evict = true
				return
			}
			idle := len(r.clients) == 0 &&
				(now.Sub(r.lastClient) > 2*time.Minute || (r.row.Status == "ended" && now.Sub(r.lastClient) > 10*time.Second))
			if !idle {
				return
			}
			h.m.flushReactionsLocked(r)
			if r.countDirty {
				h.m.persistCountsLocked(r)
			}
			if r.row.CurrentSessionID != nil && !r.sessionEnded {
				h.m.db.Exec(`UPDATE watch_room_sessions SET watched_ms = ?, peak_viewers = GREATEST(peak_viewers, ?), updated_at = NOW() WHERE id = ?`,
					r.watchedMs, r.peakOnline, *r.row.CurrentSessionID)
			}
			r.gone = true
			evict = true
		}()
		if evict {
			h.forget(r)
		}
	}
}
