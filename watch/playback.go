package watch

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	errBreakActive = errf(http.StatusConflict, "BREAK_ACTIVE", "Fasilə davam edir")
	errPauseOff    = errf(http.StatusForbidden, "PAUSE_REQUESTS_OFF", "Fasilə istəkləri bağlıdır")
	errBadEmoji    = errf(http.StatusUnprocessableEntity, "INVALID_EMOJI", "Emoji düzgün deyil")
	errMediaEnded  = errf(http.StatusConflict, "MEDIA_ENDED", "Film bitib")
)

var allowedRates = map[float64]bool{0.5: true, 0.75: true, 1: true, 1.25: true, 1.5: true, 2: true}

// ── Controls ──────────────────────────────────────────────────────────────

type controlReq struct {
	Action     string   `json:"action"`
	PositionMs *int64   `json:"position_ms"`
	Rate       *float64 `json:"rate"`
}

func (m *Module) wsControlLocked(r *liveRoom, uid int64, raw json.RawMessage) error {
	var req controlReq
	if json.Unmarshal(raw, &req) != nil {
		return errInvalid
	}
	if !r.isController(uid) {
		return errNotController
	}
	if !r.userLimits(uid).ctrl.allow(time.Now(), 10, 10) {
		return errRateLimited
	}
	if r.row.Status != "live" {
		return errNotLive
	}
	if r.row.BreakUntilMs != nil {
		return errBreakActive
	}
	now := nowMs()
	pos := r.expected(now)
	// A countdown is pending while the clock runs from a future instant.
	countdown := r.row.IsPlaying && r.row.PositionUpdatedMs > now
	switch req.Action {
	case "play":
		if r.row.IsPlaying && !countdown {
			return nil
		}
		// Replay from the start: at (or past) the known end, or after the
		// film ended when its length is unknown.
		if d := r.durationMs(); (d > 0 && pos >= d) || (d <= 0 && r.sessionEnded) {
			pos = 0
		}
		r.countdownS = 0
		m.clearPauseRequestLocked(r)
		if r.sessionEnded {
			// Watching again after the end: a new viewing session, the
			// "what next" vote is dropped and everyone starts together.
			if r.poll != nil && r.poll.Kind == "next" {
				m.closePollLocked(r, nil)
			}
			if err := m.openSessionLocked(r); err != nil {
				return err
			}
			return m.startCountdownLocked(r, pos, "play", uid)
		}
		return m.setPlaybackLocked(r, pos, true, r.row.PlaybackRate, now, "play", uid)
	case "pause":
		r.countdownS = 0
		m.clearPauseRequestLocked(r)
		return m.setPlaybackLocked(r, pos, false, r.row.PlaybackRate, now, "pause", uid)
	case "seek":
		if req.PositionMs == nil {
			return errInvalid
		}
		target := *req.PositionMs
		if target < 0 {
			target = 0
		}
		if countdown {
			// Moving the start point keeps the running countdown.
			return m.setPlaybackLocked(r, target, true, r.row.PlaybackRate, r.row.PositionUpdatedMs, "seek", uid)
		}
		r.countdownS = 0
		if !r.sessionEnded {
			return m.setPlaybackLocked(r, target, r.row.IsPlaying, r.row.PlaybackRate, now, "seek", uid)
		}
		return m.setPlaybackLocked(r, target, false, r.row.PlaybackRate, now, "seek", uid)
	case "rate":
		if req.Rate == nil || !allowedRates[*req.Rate] {
			return errInvalid
		}
		p := m.settings.get().provider(r.row.Provider)
		if p == nil || !p.isSync() {
			return errInvalid // companion apps can't follow a rate change
		}
		if countdown {
			// The countdown keeps its start instant; only the speed changes.
			return m.setPlaybackLocked(r, r.row.PositionMs, true, *req.Rate, r.row.PositionUpdatedMs, "rate", uid)
		}
		return m.setPlaybackLocked(r, pos, r.row.IsPlaying, *req.Rate, now, "rate", uid)
	case "resync_all":
		r.sync = map[int64]syncState{}
		pb := r.playbackJSON(now)
		r.broadcast("playback", struct {
			PlaybackJSON
			Action  string `json:"action"`
			ActorID int64  `json:"actor_id"`
		}{pb, "resync", uid}, nil)
		return nil
	}
	return errInvalid
}

// ── Reports / ready ───────────────────────────────────────────────────────

var syncStates = map[string]bool{"in_sync": true, "behind": true, "ahead": true, "unknown": true, "detached": true}

type reportReq struct {
	PositionMs *int64 `json:"position_ms"`
	IsPlaying  *bool  `json:"is_playing"`
	DriftMs    *int64 `json:"drift_ms"`
	State      string `json:"state"`
}

func (m *Module) wsReportLocked(r *liveRoom, uid int64, raw json.RawMessage) {
	var req reportReq
	if json.Unmarshal(raw, &req) != nil {
		return
	}
	st := syncState{At: time.Now(), State: req.State}
	if req.DriftMs != nil {
		d := *req.DriftMs
		if d > 24*3600*1000 || d < -24*3600*1000 {
			d = 0
		}
		st.DriftMs = d
	} else if req.PositionMs != nil {
		st.DriftMs = *req.PositionMs - r.expected(nowMs())
	}
	if !syncStates[st.State] {
		switch {
		case st.DriftMs > 1500:
			st.State = "ahead"
		case st.DriftMs < -1500:
			st.State = "behind"
		default:
			st.State = "in_sync"
		}
	}
	r.sync[uid] = st
}

var readyStates = map[string]bool{"ready": true, "loading": true, "no_account": true, "on_the_way": true}

func (m *Module) wsReadyLocked(r *liveRoom, uid int64, raw json.RawMessage) {
	var req struct {
		State string `json:"state"`
	}
	if json.Unmarshal(raw, &req) != nil || !readyStates[req.State] {
		return
	}
	if r.ready[uid] == req.State {
		return
	}
	r.ready[uid] = req.State
	r.broadcast("ready", map[string]any{"user_id": uid, "state": req.State}, nil)
}

func (m *Module) wsResyncUserLocked(r *liveRoom, uid int64, raw json.RawMessage) error {
	if !r.isController(uid) {
		return errNotController
	}
	var req struct {
		UserID int64 `json:"user_id"`
	}
	if json.Unmarshal(raw, &req) != nil || req.UserID == 0 {
		return errInvalid
	}
	r.sendTo(req.UserID, "resync", map[string]any{"playback": r.playbackJSON(nowMs()), "actor_id": uid})
	return nil
}

// ── Reactions ─────────────────────────────────────────────────────────────

// emojiBase — a rune that is an emoji on its own (pictographs, flags,
// symbols the emoji keyboards offer).
func emojiBase(c rune) bool {
	switch {
	case c >= 0x1F000 && c <= 0x1FAFF: // pictographs, emoticons, transport, flags, supplemental
		return true
	case c >= 0x2600 && c <= 0x27BF: // misc symbols + dingbats (☀ ❤ ✅)
		return true
	case c >= 0x2300 && c <= 0x23FF: // misc technical (⌚ ⏰ ⏳)
		return true
	case c >= 0x2B00 && c <= 0x2BFF: // ⭐ ⬛ ⬆
		return true
	case c >= 0x2190 && c <= 0x21FF, c >= 0x2934 && c <= 0x2935: // arrows
		return true
	case c >= 0x25A0 && c <= 0x25FF: // ▶ ◀ ◻
		return true
	}
	switch c {
	case 0x00A9, 0x00AE, 0x203C, 0x2049, 0x2122, 0x2139, 0x24C2, 0x3030, 0x303D, 0x3297, 0x3299:
		return true
	}
	return false
}

// emojiJoiner — the glue inside one emoji: ZWJ, VS16, keycap, skin tones
// (inside the 1F000 block) and tag letters (subdivision flags).
func emojiJoiner(c rune) bool {
	return c == 0x200D || c == 0xFE0F || c == 0x20E3 || (c >= 0xE0020 && c <= 0xE007F)
}

// validEmoji — emoji only (whitelisted ranges), ≤ 12 runes / 48 bytes
// (a skin-tone ZWJ couple is 10). ASCII only as a keycap base (#️⃣ 1️⃣).
func validEmoji(s string) bool {
	if s == "" || len(s) > 48 || utf8.RuneCountInString(s) > 12 {
		return false
	}
	hasBase, keycap := false, strings.ContainsRune(s, 0x20E3)
	for _, c := range s {
		switch {
		case emojiBase(c):
			hasBase = true
		case emojiJoiner(c):
		case keycap && (c == '#' || c == '*' || (c >= '0' && c <= '9')):
			hasBase = true
		default:
			return false
		}
	}
	return hasBase
}

func (m *Module) wsReactionLocked(r *liveRoom, uid int64, raw json.RawMessage) error {
	var req struct {
		Emoji string `json:"emoji"`
	}
	if json.Unmarshal(raw, &req) != nil {
		return errInvalid
	}
	emoji := strings.TrimSpace(req.Emoji)
	if !validEmoji(emoji) {
		return errBadEmoji
	}
	if !r.userLimits(uid).react.allow(time.Now(), 8, 15) {
		return errRateLimited
	}
	if r.row.FlyingReactionsEnabled {
		r.broadcast("reaction", map[string]any{"emoji": emoji, "user_id": uid},
			func(c *wsClient) bool { return c.userID != uid && !c.blocked[uid] })
	}
	if r.row.Status == "live" && r.row.CurrentSessionID != nil && !r.sessionEnded {
		bucket := int(r.expected(nowMs()) / 30000)
		r.reactBuf[reactKey{Session: *r.row.CurrentSessionID, Bucket: bucket, Emoji: emoji}]++
	}
	return nil
}

// ── Pause requests ────────────────────────────────────────────────────────

func (m *Module) pauseJSONLocked(r *liveRoom) map[string]any {
	if r.pause == nil {
		return nil
	}
	sup := make([]UserLite, 0, len(r.pause.Supporters))
	for _, id := range r.pause.Supporters {
		sup = append(sup, m.ensureUserLocked(r, id))
	}
	return map[string]any{
		"user":          m.ensureUserLocked(r, r.pause.UserID),
		"supporters":    sup,
		"created_at_ms": r.pause.CreatedAt.UnixMilli(),
	}
}

func (m *Module) clearPauseRequestLocked(r *liveRoom) {
	if r.pause != nil {
		r.pause = nil
		r.broadcast("pause_request", nil, nil)
	}
}

func (m *Module) wsPauseRequestLocked(r *liveRoom, uid int64) error {
	if !r.row.PauseRequestsEnabled {
		return errPauseOff
	}
	if r.row.Status != "live" || !r.row.IsPlaying {
		return errNotLive
	}
	if r.isController(uid) {
		return errInvalid // controllers just pause
	}
	if r.pause != nil {
		return m.wsPauseSupportLocked(r, uid)
	}
	if t, ok := r.pauseAsked[uid]; ok && time.Since(t) < time.Minute {
		return errRateLimited
	}
	r.pauseAsked[uid] = time.Now()
	r.pause = &pauseRequest{UserID: uid, CreatedAt: time.Now()}
	r.broadcast("pause_request", m.pauseJSONLocked(r), nil)
	return nil
}

func (m *Module) wsPauseSupportLocked(r *liveRoom, uid int64) error {
	if r.pause == nil {
		return errInvalid
	}
	if r.pause.UserID == uid {
		return nil
	}
	for _, id := range r.pause.Supporters {
		if id == uid {
			return nil
		}
	}
	if len(r.pause.Supporters) >= 50 {
		return nil
	}
	r.pause.Supporters = append(r.pause.Supporters, uid)
	r.broadcast("pause_request", m.pauseJSONLocked(r), nil)
	return nil
}

func (m *Module) wsPauseCancelLocked(r *liveRoom, uid int64) error {
	if r.pause == nil {
		return nil
	}
	if r.pause.UserID != uid && !r.isController(uid) {
		return errNotController
	}
	m.clearPauseRequestLocked(r)
	return nil
}

// ── Breaks ────────────────────────────────────────────────────────────────

func (m *Module) wsBreakLocked(r *liveRoom, uid int64, raw json.RawMessage) error {
	if !r.isController(uid) {
		return errNotController
	}
	if r.row.Status != "live" {
		return errNotLive
	}
	if r.sessionEnded {
		return errMediaEnded // nothing to pause: the film is over
	}
	if r.row.BreakUntilMs != nil {
		return errBreakActive
	}
	var req struct {
		Minutes int `json:"minutes"`
	}
	_ = json.Unmarshal(raw, &req)
	minutes := req.Minutes
	if minutes <= 0 {
		minutes = m.settings.get().s.BreakMinutes
	}
	if minutes > 15 {
		minutes = 15
	}
	now := nowMs()
	until := now + int64(minutes)*60000
	pos := r.expected(now)
	r.countdownS = 0
	m.clearPauseRequestLocked(r)
	r.row.BreakUntilMs = &until
	if err := m.setPlaybackLocked(r, pos, false, r.row.PlaybackRate, now, "break", uid); err != nil {
		r.row.BreakUntilMs = nil
		return err
	}
	if r.row.CurrentSessionID != nil {
		m.db.Exec(`UPDATE watch_room_sessions SET breaks_count = breaks_count + 1, updated_at = NOW() WHERE id = ?`, *r.row.CurrentSessionID)
	}
	return nil
}

// endBreakLocked — the break is over (timer or a controller): a 3 s
// countdown and everyone plays again from where they stopped.
func (m *Module) endBreakLocked(r *liveRoom, actor int64) error {
	if r.row.BreakUntilMs == nil {
		return nil
	}
	r.row.BreakUntilMs = nil
	return m.startCountdownLocked(r, r.row.PositionMs, "break_end", actor)
}

// ── Media end ─────────────────────────────────────────────────────────────

func (m *Module) wsMediaEndedLocked(r *liveRoom, uid int64) error {
	if r.row.Status != "live" || r.sessionEnded {
		return nil
	}
	// The player's word counts only when the clock agrees (±20 s) — a seek
	// to the end by one viewer must not end the film for everybody.
	if d := r.durationMs(); d > 0 && r.expected(nowMs()) < d-20000 {
		return nil
	}
	controllerOnline := false
	for _, id := range r.controllerIDs() {
		if _, ok := r.clients[id]; ok {
			controllerOnline = true
			break
		}
	}
	if controllerOnline && !r.isController(uid) {
		return nil
	}
	m.mediaEndedLocked(r, uid)
	return nil
}

// mediaEndedLocked — the film is over: the clock stops at the end, the
// session closes (clients show "Film bitdi") and, when the queue has
// candidates, a "what next" poll opens.
func (m *Module) mediaEndedLocked(r *liveRoom, actor int64) {
	if r.sessionEnded {
		return
	}
	end := r.durationMs()
	if end <= 0 {
		end = r.expected(nowMs())
	}
	r.countdownS = 0
	r.row.BreakUntilMs = nil // no break outlives the film (setPlayback saves it)
	m.clearPauseRequestLocked(r)
	_ = m.setPlaybackLocked(r, end, false, r.row.PlaybackRate, nowMs(), "ended", actor)
	m.closeSessionLocked(r)
	m.openNextPollLocked(r)
}
