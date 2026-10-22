package watch

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"beanpon_messenger/models"
	"beanpon_messenger/utils"

	"github.com/gin-gonic/gin"
)

var (
	errChatOff     = errf(http.StatusForbidden, "CHAT_OFF", "Çat bağlıdır")
	errChatMuted   = errf(http.StatusForbidden, "CHAT_MUTED", "Aparıcı səni çatda susdurub")
	errBlockedWord = errf(http.StatusUnprocessableEntity, "MESSAGE_BLOCKED", "Mesajda icazə verilməyən söz var")
	errPollActive  = errf(http.StatusConflict, "POLL_ACTIVE", "Artıq açıq səsvermə var")
	errPollClosed  = errf(http.StatusConflict, "POLL_CLOSED", "Səsvermə bağlanıb")
	errQueued      = errf(http.StatusConflict, "ALREADY_QUEUED", "Bu artıq növbədədir")
	errQueueFull   = errf(http.StatusConflict, "QUEUE_FULL", "Növbə doludur")
	errNoMessage   = errf(http.StatusNotFound, "MESSAGE_NOT_FOUND", "Mesaj tapılmadı")
	errRestricted  = errf(http.StatusForbidden, "ACTION_RESTRICTED", "Bu əməliyyat hazırda sənin üçün bağlıdır")
)

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

var clientMessageTypes = map[string]bool{"text": true, "voice": true, "gif": true, "photo": true, "sound": true}

const (
	maxQueue     = 20
	maxMetaBytes = 4096
)

// canChatLocked — chat_mode + chat mute (a muted cohost is muted too; only
// the host can't be muted).
func canChatLocked(r *liveRoom, uid int64) error {
	switch r.row.ChatMode {
	case "off":
		if !r.isHost(uid) {
			return errChatOff
		}
	case "hosts":
		if !r.isController(uid) {
			return errChatOff
		}
	}
	if mb := r.member(uid); mb != nil && mb.ChatMuted && !r.isHost(uid) {
		return errChatMuted
	}
	return nil
}

// shadowBanned — a spam-banned sender (messaging action): their own lines
// reach only themselves; room-wide things (polls, queue) are refused.
func (m *Module) shadowBanned(uid int64) bool {
	return models.IsMessagingBannedByActions(m.db, uint(uid))
}

// ── History ───────────────────────────────────────────────────────────────

func (m *Module) handleMessages(c *gin.Context) {
	uid := currentUserID(c)
	id, ok := paramID(c, "id")
	if !ok {
		fail(c, errNotFound)
		return
	}
	limit := queryInt(c, "limit", 30, 1, 50)
	beforeID, _ := strconv.ParseInt(strings.TrimSpace(c.Query("before_id")), 10, 64)
	if beforeID < 0 {
		beforeID = 0
	}
	var users map[int64]UserLite
	err := m.withRoom(id, func(r *liveRoom) error {
		if r.member(uid) == nil {
			return errNotMember
		}
		users = make(map[int64]UserLite, len(r.users))
		for k, v := range r.users {
			users[k] = v
		}
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	q := m.db.Where("room_id = ? AND deleted_at IS NULL", id).
		Where("(meta IS NULL OR COALESCE(meta->>'shadow', '') <> 'true' OR user_id = ?)", uid)
	if beforeID > 0 {
		q = q.Where("id < ?", beforeID)
	}
	blocked := m.blockedWith(uid)
	if len(blocked) > 0 {
		ids := make([]int64, 0, len(blocked))
		for bid := range blocked {
			ids = append(ids, bid)
		}
		q = q.Where("(user_id IS NULL OR user_id NOT IN ?)", ids)
	}
	var rows []messageRow
	if err := q.Order("id DESC").Limit(limit + 1).Find(&rows).Error; err != nil {
		fail(c, errServer)
		return
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	var missing []int64
	for _, row := range rows {
		if row.UserID != nil {
			if _, ok := users[*row.UserID]; !ok {
				missing = append(missing, *row.UserID)
			}
		}
	}
	for k, v := range m.loadUsers(missing) {
		users[k] = v
	}
	out := make([]MessageJSON, 0, len(rows))
	for _, row := range rows {
		out = append(out, messageJSON(row, users))
	}
	respond(c, gin.H{"messages": out, "has_more": hasMore})
}

// ── Send ──────────────────────────────────────────────────────────────────

type sendMessageReq struct {
	ClientID string          `json:"client_id"`
	Type     string          `json:"type"`
	Text     *string         `json:"text"`
	MediaURL *string         `json:"media_url"`
	Meta     json.RawMessage `json:"meta"`
}

// mediaDomains — chat media comes from our own storage (uploads return
// APP_URL/api/s3-storage/…) or the GIF providers the apps use; any other
// host could track who opened the room (IP / read receipts).
var mediaDomains = []string{"beanpon.com", "piokio.app", "piokio.az", "giphy.com", "tenor.com"}

func validMediaURL(raw string) bool {
	if len(raw) > 1000 {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return false
	}
	if b, err := url.Parse(utils.BaseURL); err == nil && strings.EqualFold(b.Hostname(), host) {
		return true
	}
	for _, d := range mediaDomains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// cleanMeta — a JSON object ≤ 4 KB (anything else is dropped); shadow marks
// a spam-banned sender's line (visible only to them).
func cleanMeta(raw json.RawMessage, shadow bool) *string {
	obj := map[string]any{}
	if len(raw) > 0 && len(raw) <= maxMetaBytes && string(raw) != "null" {
		if json.Unmarshal(raw, &obj) != nil {
			obj = map[string]any{}
		}
	}
	delete(obj, "shadow")
	if shadow {
		obj["shadow"] = true
	}
	if len(obj) == 0 {
		return nil
	}
	b, err := json.Marshal(obj)
	if err != nil || len(b) > maxMetaBytes+64 {
		return nil
	}
	s := string(b)
	return &s
}

func (m *Module) handleSendMessage(c *gin.Context) {
	v, err := m.guardWrite(c)
	if err != nil {
		fail(c, err)
		return
	}
	id, ok := paramID(c, "id")
	if !ok {
		fail(c, errNotFound)
		return
	}
	var req sendMessageReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, errInvalid)
		return
	}
	typ := strings.TrimSpace(req.Type)
	if typ == "" {
		typ = "text"
	}
	if !clientMessageTypes[typ] {
		fail(c, errInvalid)
		return
	}
	var clientID *string
	if req.ClientID != "" {
		if !uuidRe.MatchString(req.ClientID) {
			fail(c, errInvalid)
			return
		}
		cid := strings.ToLower(req.ClientID)
		clientID = &cid
	}
	cfg := m.settings.get()
	var text *string
	if req.Text != nil {
		t := strings.TrimSpace(*req.Text)
		if t != "" {
			if len([]rune(t)) > cfg.s.ChatMaxLength {
				fail(c, errf(http.StatusUnprocessableEntity, "MESSAGE_TOO_LONG", "Mesaj çox uzundur"))
				return
			}
			if utils.ContainsBadWord(t) {
				fail(c, errBlockedWord)
				return
			}
			text = &t
		}
	}
	var mediaURLPtr *string
	if req.MediaURL != nil && strings.TrimSpace(*req.MediaURL) != "" {
		mu := strings.TrimSpace(*req.MediaURL)
		if !validMediaURL(mu) {
			fail(c, errInvalid)
			return
		}
		mediaURLPtr = &mu
	}
	if typ == "text" && text == nil {
		fail(c, errInvalid)
		return
	}
	if typ != "text" && mediaURLPtr == nil {
		fail(c, errInvalid)
		return
	}
	shadow := m.shadowBanned(v.ID)
	meta := cleanMeta(req.Meta, shadow)

	var out MessageJSON
	err = m.withRoom(id, func(r *liveRoom) error {
		if r.member(v.ID) == nil {
			return errNotMember
		}
		if r.row.Status == "ended" {
			return errEnded
		}
		if err := canChatLocked(r, v.ID); err != nil {
			return err
		}
		if clientID != nil {
			// A retry of an already stored line (only the sender's own ids).
			var existing messageRow
			if m.db.Where("room_id = ? AND client_id = ? AND user_id = ?", r.id, *clientID, v.ID).Limit(1).Find(&existing).RowsAffected > 0 {
				out = messageJSON(existing, r.users)
				return nil
			}
		}
		if !r.userLimits(v.ID).msg.allow(time.Now(), 2.5, 5) {
			return errRateLimited
		}
		pos := r.expected(nowMs())
		now := dbNow()
		row := messageRow{
			RoomID:     r.id,
			SessionID:  r.row.CurrentSessionID,
			UserID:     &v.ID,
			Type:       typ,
			Text:       text,
			MediaURL:   mediaURLPtr,
			Meta:       meta,
			PositionMs: &pos,
			ClientID:   clientID,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		if err := m.db.Create(&row).Error; err != nil {
			if isPgUniqueViolation(err) && clientID != nil {
				var existing messageRow
				if m.db.Where("room_id = ? AND client_id = ? AND user_id = ?", r.id, *clientID, v.ID).Limit(1).Find(&existing).RowsAffected > 0 {
					out = messageJSON(existing, r.users)
					return nil
				}
				return errInvalid // someone else's client id
			}
			return errServer
		}
		m.ensureUserLocked(r, v.ID)
		out = messageJSON(row, r.users)
		r.lastTouch = time.Now()
		if shadow {
			r.sendTo(v.ID, "message", out)
		} else {
			r.broadcastFrom(v.ID, "message", out)
		}
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, out)
}

func (m *Module) handleDeleteMessage(c *gin.Context) {
	v, err := m.guardWrite(c)
	if err != nil {
		fail(c, err)
		return
	}
	id, ok1 := paramID(c, "id")
	mid, ok2 := paramID(c, "mid")
	if !ok1 || !ok2 {
		fail(c, errNotFound)
		return
	}
	err = m.withRoom(id, func(r *liveRoom) error {
		if r.member(v.ID) == nil {
			return errNotMember
		}
		var row messageRow
		if m.db.Where("id = ? AND room_id = ? AND deleted_at IS NULL", mid, r.id).Limit(1).Find(&row).RowsAffected == 0 {
			return errNoMessage
		}
		own := row.UserID != nil && *row.UserID == v.ID && row.Type != "system"
		if !own && !r.isController(v.ID) {
			return errNotController
		}
		if err := m.db.Exec(`UPDATE watch_room_messages SET deleted_at = NOW(), updated_at = NOW() WHERE id = ?`, mid).Error; err != nil {
			return errServer
		}
		r.broadcast("message_deleted", map[string]any{"id": mid}, nil)
		if r.row.PinnedMessageID != nil && *r.row.PinnedMessageID == mid {
			m.db.Exec(`UPDATE watch_rooms SET pinned_message_id = NULL, updated_at = NOW() WHERE id = ?`, r.id)
			r.row.PinnedMessageID = nil
			r.pinned = nil
			m.broadcastPinnedLocked(r)
		}
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, gin.H{"ok": true})
}

type pinReq struct {
	MessageID *int64 `json:"message_id"`
}

func (m *Module) handlePin(c *gin.Context) {
	v, err := m.guardWrite(c)
	if err != nil {
		fail(c, err)
		return
	}
	id, ok := paramID(c, "id")
	if !ok {
		fail(c, errNotFound)
		return
	}
	var req pinReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, errInvalid)
		return
	}
	err = m.withRoom(id, func(r *liveRoom) error {
		if !r.isController(v.ID) {
			return errNotController
		}
		if req.MessageID == nil || *req.MessageID <= 0 {
			m.db.Exec(`UPDATE watch_rooms SET pinned_message_id = NULL, updated_at = NOW() WHERE id = ?`, r.id)
			r.row.PinnedMessageID = nil
			r.pinned = nil
			m.broadcastPinnedLocked(r)
			return nil
		}
		// Shadow lines are never found here (only their sender sees them).
		msg := m.loadMessageJSON(r.id, *req.MessageID, r.users)
		if msg == nil || msg.Type == "system" {
			return errNoMessage
		}
		if err := m.db.Exec(`UPDATE watch_rooms SET pinned_message_id = ?, updated_at = NOW() WHERE id = ?`, msg.ID, r.id).Error; err != nil {
			return errServer
		}
		mid := msg.ID
		r.row.PinnedMessageID = &mid
		r.pinned = msg
		m.broadcastPinnedLocked(r)
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, gin.H{"ok": true})
}

// broadcastPinnedLocked — the pinned line to every client; one with a block
// with its author gets "nothing pinned" instead.
func (m *Module) broadcastPinnedLocked(r *liveRoom) {
	if len(r.clients) == 0 {
		return
	}
	var author int64
	if r.pinned != nil && r.pinned.User != nil {
		author = r.pinned.User.ID
	}
	full := encodeFrame(r.id, "pinned", map[string]any{"message": r.pinned})
	empty := full
	if author != 0 {
		empty = encodeFrame(r.id, "pinned", map[string]any{"message": nil})
	}
	for _, c := range r.clients {
		b := full
		if author != 0 && c.blocked[author] {
			b = empty
		}
		if b != nil {
			c.enqueue(b)
		}
	}
}

// pinnedForLocked — the pinned line as viewerID may see it: members only,
// nothing when a block stands between the viewer and its author.
func (m *Module) pinnedForLocked(r *liveRoom, viewerID int64) *MessageJSON {
	if r.pinned == nil || r.member(viewerID) == nil {
		return nil
	}
	if u := r.pinned.User; u != nil && u.ID != viewerID {
		if c := r.clients[viewerID]; c != nil {
			if c.blocked[u.ID] {
				return nil
			}
		} else if m.isBlocked(viewerID, u.ID) {
			return nil
		}
	}
	return r.pinned
}

type momentReq struct {
	Note *string `json:"note"`
}

func (m *Module) handleMoment(c *gin.Context) {
	v, err := m.guardWrite(c)
	if err != nil {
		fail(c, err)
		return
	}
	id, ok := paramID(c, "id")
	if !ok {
		fail(c, errNotFound)
		return
	}
	var req momentReq
	_ = c.ShouldBindJSON(&req)
	var note *string
	if req.Note != nil {
		n := clampRunes(strings.TrimSpace(*req.Note), 140)
		if n != "" {
			if utils.ContainsBadWord(n) {
				fail(c, errBlockedWord)
				return
			}
			note = &n
		}
	}
	shadow := m.shadowBanned(v.ID)
	var out MessageJSON
	err = m.withRoom(id, func(r *liveRoom) error {
		if r.member(v.ID) == nil {
			return errNotMember
		}
		if r.row.Status != "live" {
			return errNotLive
		}
		if err := canChatLocked(r, v.ID); err != nil {
			return err
		}
		if !r.userLimits(v.ID).msg.allow(time.Now(), 2.5, 5) {
			return errRateLimited
		}
		pos := r.expected(nowMs())
		metaObj := map[string]any{"position_ms": pos}
		if shadow {
			metaObj["shadow"] = true
		}
		b, _ := json.Marshal(metaObj)
		meta := string(b)
		now := dbNow()
		row := messageRow{
			RoomID: r.id, SessionID: r.row.CurrentSessionID, UserID: &v.ID, Type: "moment",
			Text: note, Meta: &meta, PositionMs: &pos, CreatedAt: now, UpdatedAt: now,
		}
		if err := m.db.Create(&row).Error; err != nil {
			return errServer
		}
		m.ensureUserLocked(r, v.ID)
		out = messageJSON(row, r.users)
		if shadow {
			r.sendTo(v.ID, "message", out)
		} else {
			r.broadcastFrom(v.ID, "message", out)
		}
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, out)
}

// ── Polls ─────────────────────────────────────────────────────────────────

type pollOptionReq struct {
	mediaInput
	Label string `json:"label"`
}

type createPollReq struct {
	Question *string         `json:"question"`
	Options  []pollOptionReq `json:"options"`
}

var optionIDs = []string{"a", "b", "c", "d"}

func (m *Module) handleCreatePoll(c *gin.Context) {
	v, err := m.guardWrite(c)
	if err != nil {
		fail(c, err)
		return
	}
	id, ok := paramID(c, "id")
	if !ok {
		fail(c, errNotFound)
		return
	}
	var req createPollReq
	if err := c.ShouldBindJSON(&req); err != nil || len(req.Options) < 2 || len(req.Options) > 4 {
		fail(c, errInvalid)
		return
	}
	var question *string
	if req.Question != nil {
		q := clampRunes(strings.TrimSpace(*req.Question), 200)
		if q != "" {
			if utils.ContainsBadWord(q) {
				fail(c, errBlockedWord)
				return
			}
			question = &q
		}
	}
	// A poll reaches the whole room: no polls from a spam-banned account.
	if m.shadowBanned(v.ID) {
		fail(c, errRestricted)
		return
	}
	// Room checks first; the media options (YouTube lookups cost API quota
	// and run outside the room lock) only after them.
	err = m.withRoom(id, func(r *liveRoom) error {
		if r.member(v.ID) == nil {
			return errNotMember
		}
		if r.row.Status == "ended" {
			return errEnded
		}
		if err := canChatLocked(r, v.ID); err != nil {
			return err
		}
		if r.poll != nil {
			return errPollActive
		}
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	if !m.limiter.allow("poll:"+itoa(v.ID), 1.0/20, 3) {
		fail(c, errRateLimited)
		return
	}
	cfg := m.settings.get()
	opts := make([]pollOption, 0, len(req.Options))
	for i, o := range req.Options {
		opt := pollOption{ID: optionIDs[i]}
		label := clampRunes(strings.TrimSpace(o.Label), 60)
		if o.Provider != "" || o.TitleID != nil || o.URL != nil {
			media, err := m.resolveMedia(cfg, o.mediaInput)
			if err != nil {
				fail(c, err)
				return
			}
			opt.Provider = media.Provider
			opt.TitleID = media.TitleID
			opt.MediaRef = media.Ref
			opt.PosterURL = media.Poster
			opt.Backdrop = media.Backdrop
			opt.Subtitle = media.Subtitle
			opt.Kind = media.Kind
			opt.DurationMs = media.DurationMs
			opt.Title = media.Title
			if label == "" {
				label = clampRunes(media.Title, 60)
			}
		}
		if label == "" {
			fail(c, errInvalid)
			return
		}
		if utils.ContainsBadWord(label) {
			fail(c, errBlockedWord)
			return
		}
		opt.Label = label
		opts = append(opts, opt)
	}
	var out *PollJSON
	err = m.withRoom(id, func(r *liveRoom) error {
		if r.member(v.ID) == nil {
			return errNotMember
		}
		if r.row.Status == "ended" {
			return errEnded
		}
		if err := canChatLocked(r, v.ID); err != nil {
			return err
		}
		if r.poll != nil {
			return errPollActive
		}
		if !r.userLimits(v.ID).msg.allow(time.Now(), 2.5, 5) {
			return errRateLimited
		}
		closes := dbNow().Add(5 * time.Minute)
		p, err := m.insertPollLocked(r, &v.ID, "custom", question, opts, &closes)
		if err != nil {
			return err
		}
		out = pollJSON(p, opts, r.pollVotes, v.ID, r.users)
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, out)
}

// insertPollLocked — poll row + its chat line; becomes the room's open poll.
func (m *Module) insertPollLocked(r *liveRoom, creator *int64, kind string, question *string, opts []pollOption, closes *time.Time) (*pollRow, error) {
	b, _ := json.Marshal(opts)
	now := dbNow()
	p := pollRow{
		RoomID: r.id, SessionID: r.row.CurrentSessionID, CreatedBy: creator, Kind: kind,
		Question: question, Options: string(b), ClosesAt: closes, CreatedAt: now, UpdatedAt: now,
	}
	if err := m.db.Create(&p).Error; err != nil {
		return nil, errServer
	}
	meta := `{"poll_id":` + itoa(p.ID) + `,"kind":"` + kind + `"}`
	pos := r.expected(nowMs())
	msg := messageRow{
		RoomID: r.id, SessionID: r.row.CurrentSessionID, UserID: creator, Type: "poll",
		Text: question, Meta: &meta, PositionMs: &pos, CreatedAt: now, UpdatedAt: now,
	}
	if err := m.db.Create(&msg).Error; err == nil {
		m.db.Exec(`UPDATE watch_room_polls SET message_id = ? WHERE id = ?`, msg.ID, p.ID)
		p.MessageID = &msg.ID
		var author int64
		if creator != nil {
			author = *creator
			m.ensureUserLocked(r, author)
		}
		r.broadcastFrom(author, "message", messageJSON(msg, r.users))
	}
	r.poll = &p
	r.pollOpts = opts
	r.pollVotes = map[int64]string{}
	m.broadcastPollLocked(r)
	return &p, nil
}

// broadcastPollLocked — each client gets its own my_option.
func (m *Module) broadcastPollLocked(r *liveRoom) {
	if r.poll == nil {
		return
	}
	for uid, cl := range r.clients {
		if b := encodeFrame(r.id, "poll", pollJSON(r.poll, r.pollOpts, r.pollVotes, uid, r.users)); b != nil {
			cl.enqueue(b)
		}
	}
}

// roomOfPoll — the poll's room id (poll endpoints are keyed by poll id).
func (m *Module) roomOfPoll(pid int64) int64 {
	var roomID int64
	m.db.Raw(`SELECT room_id FROM watch_room_polls WHERE id = ?`, pid).Scan(&roomID)
	return roomID
}

type voteReq struct {
	OptionID string `json:"option_id"`
}

func (m *Module) handlePollVote(c *gin.Context) {
	v, err := m.guardWrite(c)
	if err != nil {
		fail(c, err)
		return
	}
	pid, ok := paramID(c, "pid")
	if !ok {
		fail(c, errNotFound)
		return
	}
	var req voteReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, errInvalid)
		return
	}
	roomID := m.roomOfPoll(pid)
	if roomID == 0 {
		fail(c, errNotFound)
		return
	}
	var out *PollJSON
	err = m.withRoom(roomID, func(r *liveRoom) error {
		if r.member(v.ID) == nil {
			return errNotMember
		}
		if r.poll == nil || r.poll.ID != pid {
			return errPollClosed
		}
		valid := false
		for _, o := range r.pollOpts {
			if o.ID == req.OptionID {
				valid = true
				break
			}
		}
		if !valid {
			return errInvalid
		}
		if err := m.db.Exec(`INSERT INTO watch_room_poll_votes (poll_id, user_id, option_id, created_at, updated_at)
			VALUES (?, ?, ?, NOW(), NOW())
			ON CONFLICT (poll_id, user_id) DO UPDATE SET option_id = EXCLUDED.option_id, updated_at = NOW()`,
			pid, v.ID, req.OptionID).Error; err != nil {
			return errServer
		}
		r.pollVotes[v.ID] = req.OptionID
		m.broadcastPollLocked(r)
		out = pollJSON(r.poll, r.pollOpts, r.pollVotes, v.ID, r.users)
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, out)
}

func (m *Module) handlePollPick(c *gin.Context) {
	v, err := m.guardWrite(c)
	if err != nil {
		fail(c, err)
		return
	}
	pid, ok := paramID(c, "pid")
	if !ok {
		fail(c, errNotFound)
		return
	}
	var req voteReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, errInvalid)
		return
	}
	roomID := m.roomOfPoll(pid)
	if roomID == 0 {
		fail(c, errNotFound)
		return
	}
	err = m.withRoom(roomID, func(r *liveRoom) error {
		if !r.isController(v.ID) {
			return errNotController
		}
		if r.poll == nil || r.poll.ID != pid {
			return errPollClosed
		}
		var picked *pollOption
		for i := range r.pollOpts {
			if r.pollOpts[i].ID == req.OptionID {
				picked = &r.pollOpts[i]
			}
		}
		if picked == nil || !picked.hasMedia() {
			return errInvalid
		}
		opt := *picked
		m.closePollLocked(r, &opt.ID)
		return m.playOptionLocked(r, opt, v.ID)
	})
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, gin.H{"ok": true})
}

func (m *Module) handlePollClose(c *gin.Context) {
	v, err := m.guardWrite(c)
	if err != nil {
		fail(c, err)
		return
	}
	pid, ok := paramID(c, "pid")
	if !ok {
		fail(c, errNotFound)
		return
	}
	roomID := m.roomOfPoll(pid)
	if roomID == 0 {
		fail(c, errNotFound)
		return
	}
	err = m.withRoom(roomID, func(r *liveRoom) error {
		if r.poll == nil || r.poll.ID != pid {
			return errPollClosed
		}
		// The creator may close their own poll while still in the room.
		creator := r.poll.CreatedBy != nil && *r.poll.CreatedBy == v.ID && r.member(v.ID) != nil
		if !r.isController(v.ID) && !creator {
			return errNotController
		}
		m.finishPollLocked(r, v.ID)
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, gin.H{"ok": true})
}

// finishPollLocked — close with the most voted option; a "next" poll then
// plays its winner (when the room is still on).
func (m *Module) finishPollLocked(r *liveRoom, actor int64) {
	if r.poll == nil {
		return
	}
	kind := r.poll.Kind
	winner := topOption(r.pollOpts, r.pollVotes, kind == "next")
	var picked *pollOption
	if winner != nil {
		for i := range r.pollOpts {
			if r.pollOpts[i].ID == *winner {
				o := r.pollOpts[i]
				picked = &o
			}
		}
	}
	m.closePollLocked(r, winner)
	if kind == "next" && picked != nil && picked.hasMedia() && (r.row.Status == "live" || r.row.Status == "lobby") {
		_ = m.playOptionLocked(r, *picked, actor)
	}
}

// topOption — most votes; ties go to the earlier option. With no votes a
// "next" poll falls back to its first option (the queue's top item).
func topOption(opts []pollOption, votes map[int64]string, fallbackFirst bool) *string {
	counts := map[string]int{}
	for _, o := range votes {
		counts[o]++
	}
	best, bestN := "", 0
	for _, o := range opts {
		if counts[o.ID] > bestN {
			best, bestN = o.ID, counts[o.ID]
		}
	}
	if best == "" && fallbackFirst && len(opts) > 0 {
		best = opts[0].ID
	}
	if best == "" {
		return nil
	}
	return &best
}

func (m *Module) closePollLocked(r *liveRoom, winner *string) {
	if r.poll == nil {
		return
	}
	m.db.Exec(`UPDATE watch_room_polls SET closed_at = NOW(), winner_option_id = ?, updated_at = NOW() WHERE id = ? AND closed_at IS NULL`,
		winner, r.poll.ID)
	now := dbNow()
	r.poll.ClosedAt = &now
	r.poll.WinnerOptionID = winner
	m.broadcastPollLocked(r)
	r.poll = nil
	r.pollOpts = nil
	r.pollVotes = map[int64]string{}
}

// playOptionLocked — a poll / queue option becomes the room's media.
func (m *Module) playOptionLocked(r *liveRoom, opt pollOption, actor int64) error {
	if err := m.applyMediaLocked(r, opt.media(), actor); err != nil {
		return err
	}
	if opt.QueueID != nil {
		m.db.Exec(`UPDATE watch_room_queue SET status = 'played', updated_at = NOW() WHERE id = ? AND room_id = ?`, *opt.QueueID, r.id)
		m.broadcastQueueLocked(r)
	}
	return nil
}

// openNextPollLocked — after the film: "what next?" from the top of the
// queue (2–4 items) for next_poll_seconds.
func (m *Module) openNextPollLocked(r *liveRoom) {
	if r.poll != nil {
		return
	}
	var items []queueRow
	m.db.Where("room_id = ? AND status = 'queued'", r.id).Order("votes_count DESC, id ASC").Limit(4).Find(&items)
	if len(items) < 2 {
		return
	}
	opts := make([]pollOption, 0, len(items))
	for i, q := range items {
		qid := q.ID
		opts = append(opts, pollOption{
			ID: optionIDs[i], Label: clampRunes(q.MediaTitle, 60), Title: q.MediaTitle, Provider: q.Provider, TitleID: q.TitleID,
			MediaRef: q.MediaRef, PosterURL: q.MediaPosterURL, Subtitle: q.MediaSubtitle, Kind: q.MediaKind,
			DurationMs: q.MediaDurationMs, QueueID: &qid,
		})
	}
	closes := dbNow().Add(time.Duration(m.settings.get().s.NextPollSeconds) * time.Second)
	_, _ = m.insertPollLocked(r, nil, "next", nil, opts, &closes)
}

// ── Queue ─────────────────────────────────────────────────────────────────

func (m *Module) queueItemsLocked(r *liveRoom) ([]queueRow, map[int64]map[int64]bool) {
	var items []queueRow
	m.db.Where("room_id = ? AND status = 'queued'", r.id).Order("votes_count DESC, id ASC").Limit(maxQueue).Find(&items)
	votes := map[int64]map[int64]bool{}
	if len(items) == 0 {
		return items, votes
	}
	ids := make([]int64, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	var rows []struct {
		QueueItemID int64
		UserID      int64
	}
	m.db.Raw(`SELECT queue_item_id, user_id FROM watch_room_queue_votes WHERE queue_item_id IN ?`, ids).Scan(&rows)
	for _, v := range rows {
		if votes[v.QueueItemID] == nil {
			votes[v.QueueItemID] = map[int64]bool{}
		}
		votes[v.QueueItemID][v.UserID] = true
	}
	return items, votes
}

func (m *Module) queueJSONLocked(r *liveRoom, items []queueRow, votes map[int64]map[int64]bool, viewerID int64) []QueueItemJSON {
	cfg := m.settings.get()
	out := make([]QueueItemJSON, 0, len(items))
	for _, it := range items {
		row := roomRow{
			Provider: it.Provider, TitleID: it.TitleID, MediaRef: it.MediaRef, MediaTitle: it.MediaTitle,
			MediaSubtitle: it.MediaSubtitle, MediaKind: it.MediaKind, MediaPosterURL: it.MediaPosterURL,
			MediaDurationMs: it.MediaDurationMs,
		}
		var by *UserLite
		if it.SuggestedBy != nil {
			u := m.ensureUserLocked(r, *it.SuggestedBy)
			by = &u
		}
		out = append(out, QueueItemJSON{
			ID: it.ID, Media: mediaFromRoom(cfg, &row, nil), SuggestedBy: by,
			Votes: it.VotesCount, Voted: votes[it.ID][viewerID], CreatedAt: it.CreatedAt,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Votes > out[j].Votes })
	return out
}

func (m *Module) broadcastQueueLocked(r *liveRoom) {
	if len(r.clients) == 0 {
		return
	}
	items, votes := m.queueItemsLocked(r)
	for uid, cl := range r.clients {
		if b := encodeFrame(r.id, "queue", map[string]any{"items": m.queueJSONLocked(r, items, votes, uid)}); b != nil {
			cl.enqueue(b)
		}
	}
}

func (m *Module) handleQueue(c *gin.Context) {
	uid := currentUserID(c)
	id, ok := paramID(c, "id")
	if !ok {
		fail(c, errNotFound)
		return
	}
	var out []QueueItemJSON
	err := m.withRoom(id, func(r *liveRoom) error {
		if r.member(uid) == nil {
			return errNotMember
		}
		items, votes := m.queueItemsLocked(r)
		out = m.queueJSONLocked(r, items, votes, uid)
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, gin.H{"items": out})
}

func (m *Module) handleQueueAdd(c *gin.Context) {
	v, err := m.guardWrite(c)
	if err != nil {
		fail(c, err)
		return
	}
	id, ok := paramID(c, "id")
	if !ok {
		fail(c, errNotFound)
		return
	}
	var in mediaInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, errInvalid)
		return
	}
	// Suggestions reach the whole room: none from a spam-banned account.
	if m.shadowBanned(v.ID) {
		fail(c, errRestricted)
		return
	}
	// Room checks before the (quota-costing) media lookup.
	err = m.withRoom(id, func(r *liveRoom) error {
		if r.member(v.ID) == nil {
			return errNotMember
		}
		if r.row.Status == "ended" {
			return errEnded
		}
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	if !m.limiter.allow("queue:"+itoa(v.ID), 0.2, 5) {
		fail(c, errRateLimited)
		return
	}
	media, err := m.resolveMedia(m.settings.get(), in)
	if err != nil {
		fail(c, err)
		return
	}
	var out []QueueItemJSON
	err = m.withRoom(id, func(r *liveRoom) error {
		if r.member(v.ID) == nil {
			return errNotMember
		}
		if r.row.Status == "ended" {
			return errEnded
		}
		if !r.userLimits(v.ID).msg.allow(time.Now(), 2.5, 5) {
			return errRateLimited
		}
		var n int64
		m.db.Raw(`SELECT COUNT(*) FROM watch_room_queue WHERE room_id = ? AND status = 'queued'`, r.id).Scan(&n)
		if n >= maxQueue {
			return errQueueFull
		}
		var dup int64
		if media.TitleID != nil {
			m.db.Raw(`SELECT COUNT(*) FROM watch_room_queue WHERE room_id = ? AND status = 'queued' AND title_id = ?`, r.id, *media.TitleID).Scan(&dup)
		} else if media.Ref != nil {
			m.db.Raw(`SELECT COUNT(*) FROM watch_room_queue WHERE room_id = ? AND status = 'queued' AND provider = ? AND media_ref = ?`, r.id, media.Provider, *media.Ref).Scan(&dup)
		}
		if dup > 0 {
			return errQueued
		}
		now := dbNow()
		row := queueRow{
			RoomID: r.id, SuggestedBy: &v.ID, Provider: media.Provider, TitleID: media.TitleID, MediaRef: media.Ref,
			MediaTitle: media.Title, MediaSubtitle: media.Subtitle, MediaKind: media.Kind, MediaPosterURL: media.Poster,
			MediaDurationMs: media.DurationMs, Status: "queued", VotesCount: 1, CreatedAt: now, UpdatedAt: now,
		}
		if err := m.db.Create(&row).Error; err != nil {
			return errServer
		}
		m.db.Exec(`INSERT INTO watch_room_queue_votes (queue_item_id, user_id, created_at, updated_at) VALUES (?, ?, NOW(), NOW())
			ON CONFLICT DO NOTHING`, row.ID, v.ID)
		m.broadcastQueueLocked(r)
		items, votes := m.queueItemsLocked(r)
		out = m.queueJSONLocked(r, items, votes, v.ID)
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, gin.H{"items": out})
}

// roomOfQueueItem — (room id, suggested_by) of a queued item.
func (m *Module) roomOfQueueItem(qid int64) (int64, *int64, string) {
	var row struct {
		RoomID      int64
		SuggestedBy *int64
		Status      string
	}
	m.db.Raw(`SELECT room_id, suggested_by, status FROM watch_room_queue WHERE id = ?`, qid).Scan(&row)
	return row.RoomID, row.SuggestedBy, row.Status
}

func (m *Module) handleQueueVote(c *gin.Context) {
	v, err := m.guardWrite(c)
	if err != nil {
		fail(c, err)
		return
	}
	qid, ok := paramID(c, "qid")
	if !ok {
		fail(c, errNotFound)
		return
	}
	roomID, _, status := m.roomOfQueueItem(qid)
	if roomID == 0 || status != "queued" {
		fail(c, errNotFound)
		return
	}
	var out []QueueItemJSON
	err = m.withRoom(roomID, func(r *liveRoom) error {
		if r.member(v.ID) == nil {
			return errNotMember
		}
		res := m.db.Exec(`DELETE FROM watch_room_queue_votes WHERE queue_item_id = ? AND user_id = ?`, qid, v.ID)
		if res.Error != nil {
			return errServer
		}
		if res.RowsAffected == 0 {
			m.db.Exec(`INSERT INTO watch_room_queue_votes (queue_item_id, user_id, created_at, updated_at) VALUES (?, ?, NOW(), NOW())
				ON CONFLICT DO NOTHING`, qid, v.ID)
		}
		m.db.Exec(`UPDATE watch_room_queue SET votes_count = (SELECT COUNT(*) FROM watch_room_queue_votes WHERE queue_item_id = ?),
			updated_at = NOW() WHERE id = ?`, qid, qid)
		m.broadcastQueueLocked(r)
		items, votes := m.queueItemsLocked(r)
		out = m.queueJSONLocked(r, items, votes, v.ID)
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, gin.H{"items": out})
}

func (m *Module) handleQueueRemove(c *gin.Context) {
	v, err := m.guardWrite(c)
	if err != nil {
		fail(c, err)
		return
	}
	qid, ok := paramID(c, "qid")
	if !ok {
		fail(c, errNotFound)
		return
	}
	roomID, by, status := m.roomOfQueueItem(qid)
	if roomID == 0 || status != "queued" {
		fail(c, errNotFound)
		return
	}
	err = m.withRoom(roomID, func(r *liveRoom) error {
		// The suggester may take back their own item while still in the room.
		own := by != nil && *by == v.ID && r.member(v.ID) != nil
		if !own && !r.isController(v.ID) {
			return errNotController
		}
		m.db.Exec(`UPDATE watch_room_queue SET status = 'removed', updated_at = NOW() WHERE id = ?`, qid)
		m.broadcastQueueLocked(r)
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, gin.H{"ok": true})
}

func (m *Module) handleQueuePlay(c *gin.Context) {
	v, err := m.guardWrite(c)
	if err != nil {
		fail(c, err)
		return
	}
	qid, ok := paramID(c, "qid")
	if !ok {
		fail(c, errNotFound)
		return
	}
	roomID, _, status := m.roomOfQueueItem(qid)
	if roomID == 0 || status != "queued" {
		fail(c, errNotFound)
		return
	}
	err = m.withRoom(roomID, func(r *liveRoom) error {
		if !r.isController(v.ID) {
			return errNotController
		}
		if r.row.Status == "ended" {
			return errEnded
		}
		var it queueRow
		if m.db.Where("id = ? AND room_id = ? AND status = 'queued'", qid, r.id).Limit(1).Find(&it).RowsAffected == 0 {
			return errNotFound
		}
		opt := pollOption{
			Label: it.MediaTitle, Title: it.MediaTitle, Provider: it.Provider, TitleID: it.TitleID, MediaRef: it.MediaRef,
			PosterURL: it.MediaPosterURL, Subtitle: it.MediaSubtitle, Kind: it.MediaKind, DurationMs: it.MediaDurationMs,
			QueueID: &it.ID,
		}
		if r.poll != nil && r.poll.Kind == "next" {
			m.closePollLocked(r, nil)
		}
		return m.playOptionLocked(r, opt, v.ID)
	})
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, gin.H{"ok": true})
}
