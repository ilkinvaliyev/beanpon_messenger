package watch

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

type reactionBin struct {
	Count    int     `json:"count"`
	TopEmoji *string `json:"top_emoji"`
}

type reactionPoint struct {
	Emoji      string `json:"emoji"`
	Count      int    `json:"count,omitempty"`
	PositionMs int64  `json:"position_ms"`
}

type sessionJSON struct {
	ID        int64      `json:"id"`
	Media     MediaJSON  `json:"media"`
	StartedAt *time.Time `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at"`
	WatchedMs int64      `json:"watched_ms"`
}

const reactionBins = 48

// handleSummary — "Film bitdi": the session's numbers, who watched, and the
// reaction map over the film's timeline.
func (m *Module) handleSummary(c *gin.Context) {
	uid := currentUserID(c)
	id, ok := paramID(c, "id")
	if !ok {
		fail(c, errNotFound)
		return
	}
	var sessionID int64
	if v, err := strconv.ParseInt(c.Query("session_id"), 10, 64); err == nil && v > 0 {
		sessionID = v
	}

	var (
		room      RoomDetail
		watchedMs int64
		live      bool
		nextPoll  *PollJSON
	)
	err := m.withRoom(id, func(r *liveRoom) error {
		mb := r.members[uid]
		if mb == nil || mb.State == "kicked" {
			return errNotMember
		}
		if sessionID == 0 && r.row.CurrentSessionID != nil {
			sessionID = *r.row.CurrentSessionID
		}
		if r.row.CurrentSessionID != nil && sessionID == *r.row.CurrentSessionID && !r.sessionEnded {
			live = true
			watchedMs = r.watchedMs
			m.flushReactionsLocked(r)
		}
		if r.poll != nil && r.poll.Kind == "next" {
			nextPoll = m.pollJSONLocked(r, uid)
		}
		room = m.detailLocked(r, uid)
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	var s sessionRow
	q := m.db.Where("room_id = ?", id)
	if sessionID > 0 {
		q = q.Where("id = ?", sessionID)
	}
	if q.Order("id DESC").Limit(1).Find(&s).RowsAffected == 0 {
		fail(c, errf(http.StatusNotFound, "SESSION_NOT_FOUND", "Seans tapılmadı"))
		return
	}
	if !live {
		watchedMs = s.WatchedMs
	}

	var stats struct {
		Viewers   int64
		Messages  int64
		Reactions int64
	}
	m.db.Raw(`SELECT
		(SELECT COUNT(*) FROM watch_session_viewers WHERE session_id = ?) AS viewers,
		(SELECT COUNT(*) FROM watch_room_messages WHERE session_id = ? AND deleted_at IS NULL
			AND type IN ('text','voice','gif','photo','sound','moment')) AS messages,
		(SELECT COALESCE(SUM(count), 0) FROM watch_room_reaction_stats WHERE session_id = ?) AS reactions`,
		s.ID, s.ID, s.ID).Scan(&stats)

	var viewerIDs []int64
	m.db.Raw(`SELECT user_id FROM watch_session_viewers WHERE session_id = ? ORDER BY first_seen_at ASC LIMIT 60`, s.ID).Scan(&viewerIDs)
	blocked := m.blockedWith(uid)
	ordered := make([]int64, 0, len(viewerIDs))
	hostID := room.Host.ID
	for _, vid := range viewerIDs {
		if vid == hostID {
			ordered = append([]int64{vid}, ordered...)
		} else if !blocked[vid] {
			ordered = append(ordered, vid)
		}
	}
	if len(ordered) > 8 {
		ordered = ordered[:8]
	}
	users := m.loadUsers(ordered)
	participants := make([]UserLite, 0, len(ordered))
	for _, pid := range ordered {
		if u, ok := users[pid]; ok {
			participants = append(participants, u)
		}
	}

	// Reaction map.
	var buckets []struct {
		Bucket int
		Emoji  string
		Count  int
	}
	m.db.Raw(`SELECT bucket, emoji, count FROM watch_room_reaction_stats WHERE session_id = ?`, s.ID).Scan(&buckets)
	duration := int64(0)
	if s.MediaDurationMs != nil {
		duration = *s.MediaDurationMs
	}
	for _, b := range buckets {
		if end := int64(b.Bucket+1) * 30000; end > duration {
			duration = end
		}
	}
	bins := make([]reactionBin, reactionBins)
	binEmoji := make([]map[string]int, reactionBins)
	if duration > 0 {
		binSize := duration / reactionBins
		if binSize <= 0 {
			binSize = 1
		}
		for _, b := range buckets {
			pos := int64(b.Bucket)*30000 + 15000
			i := int(pos / binSize)
			if i >= reactionBins {
				i = reactionBins - 1
			}
			bins[i].Count += b.Count
			if binEmoji[i] == nil {
				binEmoji[i] = map[string]int{}
			}
			binEmoji[i][b.Emoji] += b.Count
		}
	}
	var peak *reactionPoint
	peakBin := -1
	for i := range bins {
		best, bestN := "", 0
		for e, n := range binEmoji[i] {
			if n > bestN || (n == bestN && e < best) {
				best, bestN = e, n
			}
		}
		if best != "" {
			e := best
			bins[i].TopEmoji = &e
		}
		if bins[i].Count > 0 && (peakBin < 0 || bins[i].Count > bins[peakBin].Count) {
			peakBin = i
		}
	}
	if peakBin >= 0 && duration > 0 {
		binSize := duration / reactionBins
		emoji := ""
		if bins[peakBin].TopEmoji != nil {
			emoji = *bins[peakBin].TopEmoji
		}
		peak = &reactionPoint{Emoji: emoji, Count: bins[peakBin].Count, PositionMs: int64(peakBin)*binSize + binSize/2}
	}
	sort.SliceStable(buckets, func(i, j int) bool { return buckets[i].Count > buckets[j].Count })
	markers := make([]reactionPoint, 0, 5)
	for _, b := range buckets {
		if len(markers) >= 5 {
			break
		}
		markers = append(markers, reactionPoint{Emoji: b.Emoji, PositionMs: int64(b.Bucket)*30000 + 15000})
	}
	sort.SliceStable(markers, func(i, j int) bool { return markers[i].PositionMs < markers[j].PositionMs })

	cfg := m.settings.get()
	snap := roomRow{
		Provider: s.Provider, TitleID: s.TitleID, MediaRef: s.MediaRef, MediaTitle: s.MediaTitle,
		MediaPosterURL: s.MediaPosterURL, MediaDurationMs: s.MediaDurationMs,
	}
	respond(c, gin.H{
		"session": sessionJSON{
			ID: s.ID, Media: mediaFromRoom(cfg, &snap, m.loadTitle(s.TitleID)),
			StartedAt: s.StartedAt, EndedAt: s.EndedAt, WatchedMs: watchedMs,
		},
		"room": room,
		"stats": gin.H{
			"viewers":     stats.Viewers,
			"messages":    stats.Messages,
			"reactions":   stats.Reactions,
			"breaks":      s.BreaksCount,
			"together_ms": watchedMs,
		},
		"participants":       participants,
		"participants_count": stats.Viewers,
		"reaction_map": gin.H{
			"bins":        bins,
			"duration_ms": duration,
			"peak":        peak,
			"markers":     markers,
		},
		"next_poll": nextPoll,
	})
}
