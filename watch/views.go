package watch

import (
	"encoding/json"
	"strings"
	"time"

	"beanpon_messenger/utils"
)

// ── Media ─────────────────────────────────────────────────────────────────

// mediaURL — catalog paths are S3 keys; full URLs pass through.
func mediaURL(key *string) *string {
	if key == nil || strings.TrimSpace(*key) == "" {
		return nil
	}
	k := strings.TrimSpace(*key)
	if strings.HasPrefix(k, "http://") || strings.HasPrefix(k, "https://") {
		return &k
	}
	u := utils.FilePathS3(k)
	return &u
}

func (m *Module) loadTitles(ids []int64) map[int64]titleRow {
	out := map[int64]titleRow{}
	ids = uniqueIDs(ids)
	if len(ids) == 0 {
		return out
	}
	var rows []titleRow
	m.db.Where("id IN ?", ids).Find(&rows)
	for _, t := range rows {
		out[t.ID] = t
	}
	return out
}

func (m *Module) loadTitle(id *int64) *titleRow {
	if id == nil {
		return nil
	}
	if t, ok := m.loadTitles([]int64{*id})[*id]; ok {
		return &t
	}
	return nil
}

func mediaFromRoom(cfg *config, row *roomRow, t *titleRow) MediaJSON {
	mj := MediaJSON{
		Provider:    row.Provider,
		TitleID:     row.TitleID,
		MediaRef:    row.MediaRef,
		Title:       row.MediaTitle,
		Subtitle:    row.MediaSubtitle,
		Kind:        row.MediaKind,
		PosterURL:   row.MediaPosterURL,
		BackdropURL: row.MediaBackdropURL,
		DurationMs:  row.MediaDurationMs,
		Season:      row.Season,
		Episode:     row.Episode,
		Genres:      []string{},
	}
	if t != nil {
		mj.Year = t.Year
		mj.AgeRating = t.AgeRating
		mj.Genres = parseJSONStrings(t.Genres)
		if t.ProviderURL != nil && strings.TrimSpace(*t.ProviderURL) != "" {
			u := strings.TrimSpace(*t.ProviderURL)
			mj.OpenURL = &u
		}
	}
	if mj.OpenURL == nil {
		mj.OpenURL = cfg.provider(row.Provider).openURL(ptrStr(row.MediaRef))
	}
	return mj
}

func titleResultFromRow(cfg *config, t titleRow) TitleResult {
	var dur *int64
	if t.DurationSec != nil && *t.DurationSec > 0 {
		d := *t.DurationSec * 1000
		dur = &d
	}
	res := TitleResult{
		Provider:     t.Provider,
		MediaRef:     t.ProviderRef,
		Title:        t.Title,
		Kind:         t.Kind,
		Year:         t.Year,
		DurationMs:   dur,
		SeasonsCount: t.SeasonsCount,
		AgeRating:    t.AgeRating,
		Genres:       parseJSONStrings(t.Genres),
		PosterURL:    mediaURL(t.PosterPath),
		BackdropURL:  mediaURL(t.BackdropPath),
		Embeddable:   true,
	}
	id := t.ID
	res.TitleID = &id
	if t.ProviderURL != nil && strings.TrimSpace(*t.ProviderURL) != "" {
		u := strings.TrimSpace(*t.ProviderURL)
		res.OpenURL = &u
	} else {
		res.OpenURL = cfg.provider(t.Provider).openURL(ptrStr(t.ProviderRef))
	}
	return res
}

// titleFromRoom — the room's media as a TitleResult (hub trending rows).
func titleFromRoom(cfg *config, row *roomRow, t *titleRow) TitleResult {
	if t != nil {
		res := titleResultFromRow(cfg, *t)
		if res.PosterURL == nil {
			res.PosterURL = row.MediaPosterURL
		}
		return res
	}
	kind := "video"
	if row.MediaKind != nil && *row.MediaKind != "" {
		kind = *row.MediaKind
	}
	return TitleResult{
		Provider:    row.Provider,
		MediaRef:    row.MediaRef,
		Title:       row.MediaTitle,
		Kind:        kind,
		DurationMs:  row.MediaDurationMs,
		Genres:      []string{},
		PosterURL:   row.MediaPosterURL,
		BackdropURL: row.MediaBackdropURL,
		Channel:     row.MediaSubtitle,
		OpenURL:     cfg.provider(row.Provider).openURL(ptrStr(row.MediaRef)),
		Embeddable:  true,
	}
}

// ── Cards ─────────────────────────────────────────────────────────────────

type cardCtx struct {
	cfg       *config
	viewerID  int64
	users     map[int64]UserLite
	titles    map[int64]titleRow
	online    map[int64][]int64
	following map[int64]bool
}

// newCardCtx — everything the cards of these rooms need, in 3 queries.
func (m *Module) newCardCtx(viewerID int64, rows []roomRow) *cardCtx {
	ctx := &cardCtx{cfg: m.settings.get(), viewerID: viewerID, online: m.hub.onlineByRoom()}
	userIDs := make([]int64, 0, len(rows)*2)
	titleIDs := make([]int64, 0, len(rows))
	for i := range rows {
		userIDs = append(userIDs, rows[i].HostID)
		userIDs = append(userIDs, ctx.online[rows[i].ID]...)
		if rows[i].TitleID != nil {
			titleIDs = append(titleIDs, *rows[i].TitleID)
		}
	}
	ctx.following = m.followingAmong(viewerID, userIDs)
	friendIDs := make([]int64, 0)
	for _, row := range rows {
		friendIDs = append(friendIDs, row.HostID)
		for _, uid := range ctx.online[row.ID] {
			if ctx.following[uid] {
				friendIDs = append(friendIDs, uid)
			}
		}
	}
	ctx.users = m.loadUsers(friendIDs)
	ctx.titles = m.loadTitles(titleIDs)
	return ctx
}

func (ctx *cardCtx) friends(row *roomRow) FriendsJSON {
	f := FriendsJSON{Users: []UserLite{}}
	for _, uid := range ctx.online[row.ID] {
		if uid == ctx.viewerID || !ctx.following[uid] {
			continue
		}
		f.Count++
		if len(f.Users) < 3 {
			if u, ok := ctx.users[uid]; ok {
				f.Users = append(f.Users, u)
			}
		}
	}
	return f
}

func (ctx *cardCtx) card(row *roomRow) RoomCard {
	var t *titleRow
	if row.TitleID != nil {
		if tr, ok := ctx.titles[*row.TitleID]; ok {
			t = &tr
		}
	}
	host, ok := ctx.users[row.HostID]
	if !ok {
		host = UserLite{ID: row.HostID}
	}
	watching := row.WatchingCount
	if online, ok := ctx.online[row.ID]; ok {
		watching = len(online)
	}
	rr := *row
	pb := playbackFromRow(&rr, nowMs())
	return RoomCard{
		ID:            row.ID,
		Name:          row.Name,
		Status:        row.Status,
		Access:        row.Access,
		FollowersOnly: row.FollowersOnly,
		Locked:        row.Locked,
		Media:         mediaFromRoom(ctx.cfg, row, t),
		Playback:      pb,
		WatchingCount: watching,
		MemberCount:   row.MemberCount,
		Host:          host,
		Friends:       ctx.friends(row),
		ScheduledAt:   row.ScheduledAt,
		StartedAt:     row.StartedAt,
		CreatedAt:     row.CreatedAt,
	}
}

func playbackFromRow(row *roomRow, now int64) PlaybackJSON {
	rate := row.PlaybackRate
	if rate <= 0 {
		rate = 1
	}
	return PlaybackJSON{
		PositionMs:   row.PositionMs,
		IsPlaying:    row.IsPlaying,
		Rate:         rate,
		UpdatedAtMs:  row.PositionUpdatedMs,
		ServerMs:     now,
		BreakUntilMs: row.BreakUntilMs,
		Version:      row.StateVersion,
	}
}

// ── Room detail (room lock held) ──────────────────────────────────────────

type meExtras struct {
	RequestStatus    *string
	RequestExpiresAt *time.Time
	Invited          bool
	Reminder         bool
	HasAccount       *bool
	HostFollowsMe    bool
}

func (m *Module) meExtras(roomID, viewerID, hostID int64, providerKey string) meExtras {
	var x meExtras
	m.db.Raw(`SELECT
		(SELECT status FROM watch_room_requests WHERE room_id = ? AND user_id = ?) AS request_status,
		(SELECT expires_at FROM watch_room_requests WHERE room_id = ? AND user_id = ?) AS request_expires_at,
		EXISTS (SELECT 1 FROM watch_room_invites WHERE room_id = ? AND invitee_id = ?) AS invited,
		EXISTS (SELECT 1 FROM watch_room_reminders WHERE room_id = ? AND user_id = ?) AS reminder,
		(SELECT has_account FROM watch_provider_accounts WHERE user_id = ? AND provider = ?) AS has_account,
		EXISTS (SELECT 1 FROM follows WHERE follower_id = ? AND following_id = ? AND status = 'accepted') AS host_follows_me`,
		roomID, viewerID, roomID, viewerID, roomID, viewerID, roomID, viewerID,
		viewerID, providerKey, hostID, viewerID).Scan(&x)
	return x
}

func (m *Module) detailLocked(r *liveRoom, viewerID int64) RoomDetail {
	cfg := m.settings.get()
	row := &r.row
	t := m.loadTitle(row.TitleID)

	online := r.onlineIDs()
	following := m.followingAmong(viewerID, online)
	friends := FriendsJSON{Users: []UserLite{}}
	for _, uid := range online {
		if uid == viewerID || !following[uid] {
			continue
		}
		friends.Count++
		if len(friends.Users) < 3 {
			friends.Users = append(friends.Users, m.ensureUserLocked(r, uid))
		}
	}

	card := RoomCard{
		ID:            row.ID,
		Name:          row.Name,
		Status:        row.Status,
		Access:        row.Access,
		FollowersOnly: row.FollowersOnly,
		Locked:        row.Locked,
		Media:         mediaFromRoom(cfg, row, t),
		Playback:      r.playbackJSON(nowMs()),
		WatchingCount: len(r.clients),
		MemberCount:   r.joinedCount(),
		Host:          m.ensureUserLocked(r, row.HostID),
		Friends:       friends,
		ScheduledAt:   row.ScheduledAt,
		StartedAt:     row.StartedAt,
		CreatedAt:     row.CreatedAt,
	}

	x := m.meExtras(r.id, viewerID, row.HostID, row.Provider)
	me := MeJSON{
		RequestStatus:    x.RequestStatus,
		RequestExpiresAt: x.RequestExpiresAt,
		Invited:          x.Invited,
		Reminder:         x.Reminder,
		HostFollowsMe:    x.HostFollowsMe,
		IsHost:           row.HostID == viewerID,
		CanControl:       r.isController(viewerID) && row.Status != "ended",
	}
	if mb := r.members[viewerID]; mb != nil {
		role, state := mb.Role, mb.State
		me.Role, me.State = &role, &state
		me.ChatMuted = mb.ChatMuted
	}
	p := cfg.provider(row.Provider)
	if p != nil && !p.isSync() {
		has := x.HasAccount != nil && *x.HasAccount
		me.HasAccount = &has
	}

	var code *string
	if r.isController(viewerID) {
		c := row.Code
		code = &c
	}
	var pj *ProviderJSON
	if p != nil {
		j := p.toJSON()
		pj = &j
	}
	return RoomDetail{
		RoomCard:               card,
		ChatMode:               row.ChatMode,
		PauseRequestsEnabled:   row.PauseRequestsEnabled,
		FlyingReactionsEnabled: row.FlyingReactionsEnabled,
		Code:                   code,
		PinnedMessage:          m.pinnedForLocked(r, viewerID),
		CurrentSessionID:       row.CurrentSessionID,
		MaxMembers:             cfg.s.MaxMembers,
		EndReason:              row.EndReason,
		Me:                     me,
		ProviderInfo:           pj,
	}
}

// ── Messages ──────────────────────────────────────────────────────────────

func messageJSON(row messageRow, users map[int64]UserLite) MessageJSON {
	mj := MessageJSON{
		ID:         row.ID,
		ClientID:   row.ClientID,
		Type:       row.Type,
		Text:       row.Text,
		MediaURL:   row.MediaURL,
		PositionMs: row.PositionMs,
		CreatedAt:  row.CreatedAt,
		Deleted:    row.DeletedAt != nil,
		Meta:       json.RawMessage("null"),
	}
	if row.Meta != nil && strings.TrimSpace(*row.Meta) != "" && json.Valid([]byte(*row.Meta)) {
		mj.Meta = json.RawMessage(*row.Meta)
	}
	if row.UserID != nil {
		if u, ok := users[*row.UserID]; ok {
			uu := u
			mj.User = &uu
		} else {
			mj.User = &UserLite{ID: *row.UserID}
		}
	}
	if mj.Deleted {
		mj.Text = nil
		mj.MediaURL = nil
		mj.Meta = json.RawMessage("null")
	}
	return mj
}

// loadMessageJSON — a visible line of the room (deleted and shadow lines are
// never returned: those are not for everyone).
func (m *Module) loadMessageJSON(roomID, msgID int64, users map[int64]UserLite) *MessageJSON {
	var row messageRow
	if m.db.Where("id = ? AND room_id = ? AND deleted_at IS NULL", msgID, roomID).
		Where("(meta IS NULL OR COALESCE(meta->>'shadow', '') <> 'true')").
		Limit(1).Find(&row).RowsAffected == 0 {
		return nil
	}
	if row.UserID != nil {
		if _, ok := users[*row.UserID]; !ok {
			for id, u := range m.loadUsers([]int64{*row.UserID}) {
				users[id] = u
			}
		}
	}
	mj := messageJSON(row, users)
	return &mj
}

// ── Members ───────────────────────────────────────────────────────────────

func (m *Module) memberJSONLocked(r *liveRoom, mb *memberRow, now time.Time) MemberJSON {
	_, online := r.clients[mb.UserID]
	ready := r.ready[mb.UserID]
	if !online {
		ready = "offline"
	} else if ready == "" {
		ready = "loading"
	}
	st := r.sync[mb.UserID]
	state := st.State
	if !online || state == "" || now.Sub(st.At) > 15*time.Second {
		state = "unknown"
	}
	return MemberJSON{
		User:      m.ensureUserLocked(r, mb.UserID),
		Role:      mb.Role,
		ChatMuted: mb.ChatMuted,
		Online:    online,
		Ready:     ready,
		Sync:      SyncJSON{State: state, DriftMs: st.DriftMs},
		JoinedAt:  mb.JoinedAt,
	}
}

// ── Polls ─────────────────────────────────────────────────────────────────

func (m *Module) loadOpenPoll(r *liveRoom) {
	var p pollRow
	if m.db.Where("room_id = ? AND closed_at IS NULL", r.id).Order("id DESC").Limit(1).Find(&p).RowsAffected == 0 {
		r.poll = nil
		return
	}
	r.poll = &p
	r.pollOpts = nil
	_ = json.Unmarshal([]byte(p.Options), &r.pollOpts)
	r.pollVotes = map[int64]string{}
	var votes []struct {
		UserID   int64
		OptionID string
	}
	m.db.Raw(`SELECT user_id, option_id FROM watch_room_poll_votes WHERE poll_id = ?`, p.ID).Scan(&votes)
	for _, v := range votes {
		r.pollVotes[v.UserID] = v.OptionID
	}
}

// pollJSONLocked — the open poll as seen by viewerID.
func (m *Module) pollJSONLocked(r *liveRoom, viewerID int64) *PollJSON {
	if r.poll == nil {
		return nil
	}
	return pollJSON(r.poll, r.pollOpts, r.pollVotes, viewerID, r.users)
}

func pollJSON(p *pollRow, opts []pollOption, votes map[int64]string, viewerID int64, users map[int64]UserLite) *PollJSON {
	counts := map[string]int{}
	for _, opt := range votes {
		counts[opt]++
	}
	out := &PollJSON{
		ID:             p.ID,
		Kind:           p.Kind,
		Question:       p.Question,
		Options:        make([]PollOptionJSON, 0, len(opts)),
		TotalVotes:     len(votes),
		Closed:         p.ClosedAt != nil,
		WinnerOptionID: p.WinnerOptionID,
	}
	if mine, ok := votes[viewerID]; ok {
		mm := mine
		out.MyOption = &mm
	}
	if p.ClosesAt != nil {
		ms := p.ClosesAt.UnixMilli()
		out.ClosesAtMs = &ms
	}
	if p.CreatedBy != nil {
		if u, ok := users[*p.CreatedBy]; ok {
			uu := u
			out.CreatedBy = &uu
		}
	}
	for _, o := range opts {
		out.Options = append(out.Options, PollOptionJSON{
			ID: o.ID, Label: o.Label, Provider: o.Provider, TitleID: o.TitleID, MediaRef: o.MediaRef,
			PosterURL: o.PosterURL, Subtitle: o.Subtitle, Kind: o.Kind, DurationMs: o.DurationMs,
			Votes: counts[o.ID],
		})
	}
	return out
}
