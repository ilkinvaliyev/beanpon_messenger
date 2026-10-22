package watch

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// ── Config + accounts ─────────────────────────────────────────────────────

func (m *Module) handleConfig(c *gin.Context) {
	uid := currentUserID(c)
	cfg := m.settings.get()
	providers := make([]ProviderJSON, 0)
	for _, p := range cfg.activeProviders() {
		providers = append(providers, p.toJSON())
	}
	accounts := map[string]bool{}
	var rows []struct {
		Provider   string
		HasAccount bool
	}
	m.db.Raw(`SELECT provider, has_account FROM watch_provider_accounts WHERE user_id = ?`, uid).Scan(&rows)
	for _, r := range rows {
		accounts[r.Provider] = r.HasAccount
	}
	respond(c, gin.H{
		"enabled":              cfg.s.Enabled,
		"home_preview_enabled": cfg.s.Enabled && cfg.s.HomePreviewEnabled,
		"providers":            providers,
		"limits": gin.H{
			"max_members":         cfg.s.MaxMembers,
			"chat_max_length":     cfg.s.ChatMaxLength,
			"break_minutes":       cfg.s.BreakMinutes,
			"max_schedule_days":   cfg.s.MaxScheduleDays,
			"request_ttl_minutes": cfg.s.RequestTTLMinutes,
			"next_poll_seconds":   cfg.s.NextPollSeconds,
			"name_max_length":     40,
			"code_length":         6,
		},
		"accounts":                 accounts,
		"youtube_search_available": cfg.s.YouTubeSearchEnabled && m.yt.enabled(),
	})
}

type accountReq struct {
	HasAccount bool `json:"has_account"`
}

func (m *Module) handleSetAccount(c *gin.Context) {
	v, err := m.guardWrite(c)
	if err != nil {
		fail(c, err)
		return
	}
	key := strings.TrimSpace(c.Param("provider"))
	p := m.settings.get().provider(key)
	if p == nil || p.isSync() {
		fail(c, errProvider)
		return
	}
	var req accountReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, errInvalid)
		return
	}
	if err := m.db.Exec(`INSERT INTO watch_provider_accounts (user_id, provider, has_account, created_at, updated_at)
		VALUES (?, ?, ?, NOW(), NOW())
		ON CONFLICT (user_id, provider) DO UPDATE SET has_account = EXCLUDED.has_account, updated_at = NOW()`,
		v.ID, key, req.HasAccount).Error; err != nil {
		fail(c, errServer)
		return
	}
	// Rooms the user is in learn it at once (the lobby shows "no account").
	for _, r := range m.hub.snapshot() {
		r.locked(func() {
			if r.gone || r.row.Provider != key || r.clients[v.ID] == nil {
				return
			}
			st := "loading"
			if !req.HasAccount {
				st = "no_account"
			}
			if r.ready[v.ID] != "ready" && r.ready[v.ID] != st {
				r.ready[v.ID] = st
				r.broadcast("ready", map[string]any{"user_id": v.ID, "state": st}, nil)
			}
		})
	}
	respond(c, gin.H{"provider": key, "has_account": req.HasAccount})
}

// ── Visible rooms ─────────────────────────────────────────────────────────

// visibleOpenRooms — lobby / live rooms the viewer may find in lists.
func (m *Module) visibleOpenRooms(v viewer) []roomRow {
	var rows []roomRow
	m.db.Where("status IN ?", []string{"lobby", "live"}).
		Order("watching_count DESC, member_count DESC, id DESC").Limit(500).Find(&rows)
	if len(rows) == 0 {
		return rows
	}
	hosts := make([]int64, 0, len(rows))
	for _, r := range rows {
		hosts = append(hosts, r.HostID)
	}
	hostRows := m.loadUserRows(hosts)
	blocked := m.blockedWith(v.ID)
	hidden := m.hiddenFor(v.ID, hosts)
	followsMe := m.followersAmong(v.ID, hosts)
	out := rows[:0]
	for _, r := range rows {
		if r.HostID == v.ID {
			out = append(out, r)
			continue
		}
		h, ok := hostRows[r.HostID]
		if !ok || h.Gone || blocked[r.HostID] || hidden[r.HostID] {
			continue
		}
		if r.Access == "code" || r.Locked {
			continue
		}
		if r.FollowersOnly && (v.IsGuest || !followsMe[r.HostID]) {
			continue
		}
		if v.IsGuest && r.Access != "public" {
			continue
		}
		out = append(out, r)
	}
	return out
}

type scoredRoom struct {
	row     roomRow
	friends int
	hostFav bool
}

func (m *Module) scoreRooms(ctx *cardCtx, rows []roomRow) []scoredRoom {
	out := make([]scoredRoom, 0, len(rows))
	for _, r := range rows {
		out = append(out, scoredRoom{row: r, friends: ctx.friends(&r).Count, hostFav: ctx.following[r.HostID]})
	}
	return out
}

func watchingOf(ctx *cardCtx, r *roomRow) int {
	if online, ok := ctx.online[r.ID]; ok {
		return len(online)
	}
	return r.WatchingCount
}

func (m *Module) handleHome(c *gin.Context) {
	uid := currentUserID(c)
	cfg := m.settings.get()
	if !cfg.s.Enabled || !cfg.s.HomePreviewEnabled {
		respond(c, gin.H{"enabled": false, "live_count": 0, "friends_watching": 0, "rooms": []RoomCard{}})
		return
	}
	v := m.loadViewer(uid)
	rows := m.visibleOpenRooms(v)
	ctx := m.newCardCtx(uid, rows)
	scored := m.scoreRooms(ctx, rows)
	sort.SliceStable(scored, func(i, j int) bool {
		a, b := scored[i], scored[j]
		if a.friends != b.friends {
			return a.friends > b.friends
		}
		if a.hostFav != b.hostFav {
			return a.hostFav
		}
		return watchingOf(ctx, &a.row) > watchingOf(ctx, &b.row)
	})
	friendSet := map[int64]bool{}
	for _, r := range rows {
		for _, id := range ctx.online[r.ID] {
			if id != uid && ctx.following[id] {
				friendSet[id] = true
			}
		}
	}
	cards := make([]RoomCard, 0, 8)
	for _, s := range scored {
		if len(cards) >= 8 {
			break
		}
		row := s.row
		cards = append(cards, ctx.card(&row))
	}
	respond(c, gin.H{"enabled": true, "live_count": len(rows), "friends_watching": len(friendSet), "rooms": cards})
}

// filterRooms — hub / list filter: all | friends | movies | series | <provider>.
func filterRooms(ctx *cardCtx, rows []roomRow, filter string) []roomRow {
	if filter == "" || filter == "all" {
		return rows
	}
	out := make([]roomRow, 0, len(rows))
	for _, r := range rows {
		kind := ""
		if r.MediaKind != nil {
			kind = *r.MediaKind
		}
		keep := false
		switch filter {
		case "friends":
			rr := r
			keep = ctx.following[r.HostID] || ctx.friends(&rr).Count > 0
		case "movies":
			keep = kind == "movie" || kind == "documentary"
		case "series":
			keep = kind == "series" || kind == "show"
		default:
			keep = r.Provider == filter
		}
		if keep {
			out = append(out, r)
		}
	}
	return out
}

func sortRooms(ctx *cardCtx, rows []roomRow, sortBy string) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := &rows[i], &rows[j]
		if sortBy == "new" {
			return a.CreatedAt.After(b.CreatedAt)
		}
		wa, wb := watchingOf(ctx, a), watchingOf(ctx, b)
		if wa != wb {
			return wa > wb
		}
		if a.MemberCount != b.MemberCount {
			return a.MemberCount > b.MemberCount
		}
		return a.ID > b.ID
	})
}

func pageRooms(ctx *cardCtx, rows []roomRow, cursor string, size int) ([]RoomCard, *string) {
	offset, _ := strconv.Atoi(strings.TrimPrefix(cursor, "o:"))
	if offset < 0 || offset > len(rows) {
		offset = 0
	}
	end := offset + size
	if end > len(rows) {
		end = len(rows)
	}
	cards := make([]RoomCard, 0, end-offset)
	for i := offset; i < end; i++ {
		cards = append(cards, ctx.card(&rows[i]))
	}
	var next *string
	if end < len(rows) {
		n := "o:" + strconv.Itoa(end)
		next = &n
	}
	return cards, next
}

type trendingJSON struct {
	Title         TitleResult `json:"title"`
	RoomsCount    int         `json:"rooms_count"`
	WatchingCount int         `json:"watching_count"`
	Rank          int         `json:"rank"`
}

func titleKey(r *roomRow) string {
	if r.TitleID != nil {
		return r.Provider + ":t" + itoa(*r.TitleID)
	}
	if r.MediaRef != nil && *r.MediaRef != "" {
		return r.Provider + ":r" + *r.MediaRef
	}
	return r.Provider + ":n" + strings.ToLower(r.MediaTitle)
}

func (m *Module) handleHub(c *gin.Context) {
	uid := currentUserID(c)
	cfg := m.settings.get()
	if !cfg.s.Enabled {
		fail(c, errDisabled)
		return
	}
	v := m.loadViewer(uid)
	all := m.visibleOpenRooms(v)
	ctx := m.newCardCtx(uid, all)
	filter := strings.TrimSpace(c.Query("filter"))
	rows := filterRooms(ctx, all, filter)

	watching, friendSet := 0, map[int64]bool{}
	for i := range all {
		watching += watchingOf(ctx, &all[i])
		for _, id := range ctx.online[all[i].ID] {
			if id != uid && ctx.following[id] {
				friendSet[id] = true
			}
		}
	}

	scored := m.scoreRooms(ctx, rows)
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].friends != scored[j].friends {
			return scored[i].friends > scored[j].friends
		}
		return watchingOf(ctx, &scored[i].row) > watchingOf(ctx, &scored[j].row)
	})
	friendsRooms := make([]RoomCard, 0, 5)
	for _, s := range scored {
		if len(friendsRooms) >= 5 {
			break
		}
		if s.friends == 0 && !s.hostFav {
			continue
		}
		row := s.row
		friendsRooms = append(friendsRooms, ctx.card(&row))
	}

	// Trending titles: live rooms grouped by what they watch.
	type agg struct {
		first    roomRow
		rooms    int
		watching int
	}
	groups := map[string]*agg{}
	order := []string{}
	for i := range rows {
		if rows[i].Status != "live" {
			continue
		}
		k := titleKey(&rows[i])
		g := groups[k]
		if g == nil {
			g = &agg{first: rows[i]}
			groups[k] = g
			order = append(order, k)
		}
		g.rooms++
		g.watching += watchingOf(ctx, &rows[i])
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := groups[order[i]], groups[order[j]]
		if a.watching != b.watching {
			return a.watching > b.watching
		}
		return a.rooms > b.rooms
	})
	trending := make([]trendingJSON, 0, 10)
	for i, k := range order {
		if i >= 10 {
			break
		}
		g := groups[k]
		var t *titleRow
		if g.first.TitleID != nil {
			if tr, ok := ctx.titles[*g.first.TitleID]; ok {
				t = &tr
			}
		}
		trending = append(trending, trendingJSON{
			Title: titleFromRoom(cfg, &g.first, t), RoomsCount: g.rooms, WatchingCount: g.watching, Rank: i + 1,
		})
	}

	sortRooms(ctx, rows, c.Query("sort"))
	cards, next := pageRooms(ctx, rows, c.Query("cursor"), 20)
	respond(c, gin.H{
		"live_count":     len(all),
		"watching_count": watching,
		"friends_count":  len(friendSet),
		"friends_rooms":  friendsRooms,
		"trending":       trending,
		"rooms":          cards,
		"next_cursor":    next,
	})
}

func (m *Module) handleRoomsList(c *gin.Context) {
	uid := currentUserID(c)
	if !m.settings.get().s.Enabled {
		fail(c, errDisabled)
		return
	}
	v := m.loadViewer(uid)
	all := m.visibleOpenRooms(v)
	ctx := m.newCardCtx(uid, all)
	rows := filterRooms(ctx, all, strings.TrimSpace(c.Query("filter")))
	if tid, err := strconv.ParseInt(c.Query("title_id"), 10, 64); err == nil && tid > 0 {
		out := rows[:0:0]
		for _, r := range rows {
			if r.TitleID != nil && *r.TitleID == tid {
				out = append(out, r)
			}
		}
		rows = out
	}
	if ref := strings.TrimSpace(c.Query("media_ref")); ref != "" {
		out := rows[:0:0]
		for _, r := range rows {
			if r.MediaRef != nil && *r.MediaRef == ref {
				out = append(out, r)
			}
		}
		rows = out
	}
	sortRooms(ctx, rows, c.Query("sort"))
	cards, next := pageRooms(ctx, rows, c.Query("cursor"), 20)
	respond(c, gin.H{"rooms": cards, "next_cursor": next, "total": len(rows)})
}

// ── Titles ────────────────────────────────────────────────────────────────

// ytRegions — regions the YouTube lists are fetched for (the app's
// languages + neighbours). Every distinct region is its own cache entry and
// quota cost, so anything else falls back to AZ.
var ytRegions = map[string]bool{
	"AZ": true, "TR": true, "RU": true, "UZ": true, "KZ": true, "GE": true, "UA": true,
	"US": true, "GB": true, "DE": true, "ES": true,
}

func regionOf(c *gin.Context) string {
	r := strings.ToUpper(strings.TrimSpace(c.Query("region")))
	if !ytRegions[r] {
		return "AZ"
	}
	return r
}

func (m *Module) handleTitlesFeatured(c *gin.Context) {
	uid := currentUserID(c)
	cfg := m.settings.get()
	key := strings.TrimSpace(c.Query("provider"))
	if key == "" {
		key = "youtube"
	}
	p := cfg.activeProvider(key)
	if p == nil {
		fail(c, errProvider)
		return
	}
	var items []TitleResult
	if p.isSync() {
		// Cached per region for an hour; the bucket only stops scripts.
		if m.yt.enabled() && m.limiter.allow("ytfeatured:"+itoa(uid), 0.5, 10) {
			vids, err := m.yt.trending(regionOf(c))
			if err == nil {
				items = ytResults(vids)
			}
		}
	} else {
		var rows []titleRow
		m.db.Where("provider = ? AND is_active = true", key).
			Order("is_featured DESC, sort ASC, id DESC").Limit(40).Find(&rows)
		for _, t := range rows {
			items = append(items, titleResultFromRow(cfg, t))
		}
	}
	if items == nil {
		items = []TitleResult{}
	}
	m.attachSocial(uid, items)
	respond(c, gin.H{"provider": key, "items": items})
}

var (
	errSearchOff   = errf(http.StatusServiceUnavailable, "SEARCH_UNAVAILABLE", "Axtarış hazırda əlçatan deyil")
	errSearchLimit = errf(http.StatusServiceUnavailable, "SEARCH_LIMIT", "Bu gün üçün axtarış limiti doldu — videonun linkini yapışdır")
)

func (m *Module) handleTitlesSearch(c *gin.Context) {
	uid := currentUserID(c)
	cfg := m.settings.get()
	key := strings.TrimSpace(c.Query("provider"))
	if key == "" {
		key = "youtube"
	}
	p := cfg.activeProvider(key)
	if p == nil {
		fail(c, errProvider)
		return
	}
	raw := strings.TrimSpace(c.Query("q"))
	q := foldSearch(raw)
	if q == "" {
		m.handleTitlesFeatured(c)
		return
	}
	var items []TitleResult
	if p.isSync() {
		if !cfg.s.YouTubeSearchEnabled || !m.yt.enabled() {
			fail(c, errSearchOff)
			return
		}
		// Every new query costs 100 quota units: signed-in users only, and
		// a slow per-user bucket on top of the daily cap.
		if m.loadViewer(uid).IsGuest {
			fail(c, errGuest)
			return
		}
		if !m.limiter.allow("ytsearch:"+itoa(uid), 1.0/30, 20) {
			fail(c, errRateLimited)
			return
		}
		vids, err := m.yt.search(uid, clampRunes(raw, 100), regionOf(c))
		if errors.Is(err, errYouTubeBudget) {
			fail(c, errSearchLimit)
			return
		}
		if err != nil {
			fail(c, errSearchOff)
			return
		}
		items = ytResults(vids)
	} else {
		like := "%" + escapeLike(clampRunes(q, 100)) + "%"
		prefix := escapeLike(clampRunes(q, 100)) + "%"
		var rows []titleRow
		m.db.Raw(`SELECT * FROM watch_titles
			WHERE provider = ? AND is_active = true AND search_text LIKE ?
			ORDER BY (search_text LIKE ?) DESC, is_featured DESC, sort ASC, id DESC
			LIMIT 40`, key, like, prefix).Scan(&rows)
		for _, t := range rows {
			items = append(items, titleResultFromRow(cfg, t))
		}
	}
	if items == nil {
		items = []TitleResult{}
	}
	m.attachSocial(uid, items)
	respond(c, gin.H{"provider": key, "items": items})
}

type resolveReq struct {
	URL string `json:"url"`
}

func (m *Module) handleTitlesResolve(c *gin.Context) {
	v, err := m.guardWrite(c)
	if err != nil {
		fail(c, err)
		return
	}
	uid := v.ID
	var req resolveReq
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.URL) == "" {
		fail(c, errInvalid)
		return
	}
	if !m.limiter.allow("resolve:"+itoa(uid), 0.5, 10) {
		fail(c, errRateLimited)
		return
	}
	cfg := m.settings.get()
	p, ref := detectProvider(cfg, req.URL)
	if p == nil {
		fail(c, errLink)
		return
	}
	var res TitleResult
	if p.isSync() {
		res = TitleResult{
			Provider: p.Key, MediaRef: strPtr(ref), Kind: "video", Genres: []string{},
			PosterURL: youtubePoster(ref), BackdropURL: youtubeBackdrop(ref), Embeddable: true,
			OpenURL: p.openURL(ref),
		}
		if m.yt.enabled() {
			vid, ok, err := m.yt.video(ref)
			switch {
			case err != nil:
				fail(c, errYouTubeDown)
				return
			case !ok || !vid.Embeddable || vid.Live || vid.AgeRestricted:
				fail(c, errNotEmbeddable)
				return
			}
			res = ytResult(vid)
		} else if vid, ok, err := m.yt.oembed(ref); err == nil {
			// No API key: the keyless oEmbed answer names the video (best-effort —
			// a network error keeps the bare result).
			if !ok {
				fail(c, errNotEmbeddable)
				return
			}
			res = ytResult(vid)
		}
	} else {
		var t titleRow
		if m.db.Where("provider = ? AND provider_ref = ? AND is_active = true", p.Key, ref).Limit(1).Find(&t).RowsAffected > 0 {
			res = titleResultFromRow(cfg, t)
		} else {
			res = TitleResult{
				Provider: p.Key, MediaRef: strPtr(ref), Kind: "movie", Genres: []string{},
				OpenURL: p.openURL(ref), Embeddable: true,
			}
		}
	}
	items := []TitleResult{res}
	m.attachSocial(uid, items)
	respond(c, gin.H{"provider": p.Key, "result": items[0]})
}

func ytResult(v ytVideo) TitleResult {
	id := v.ID
	var dur *int64
	if v.DurationMs > 0 {
		d := v.DurationMs
		dur = &d
	}
	poster := youtubePoster(id)
	if v.Thumbnail != "" {
		poster = strPtr(v.Thumbnail)
	}
	open := "https://www.youtube.com/watch?v=" + id
	return TitleResult{
		Provider: "youtube", MediaRef: &id, Title: v.Title, Kind: "video", DurationMs: dur,
		Genres: []string{}, PosterURL: poster, BackdropURL: youtubeBackdrop(id),
		Channel: nonEmptyPtr(v.Channel), OpenURL: &open, Embeddable: v.Embeddable,
	}
}

func ytResults(vids []ytVideo) []TitleResult {
	out := make([]TitleResult, 0, len(vids))
	for _, v := range vids {
		if !v.Embeddable || v.Live || v.AgeRestricted {
			continue
		}
		out = append(out, ytResult(v))
	}
	return out
}

// attachSocial — the small line under a title: "you stopped at …",
// "Leyla, Nigar watched", or "live in N rooms".
func (m *Module) attachSocial(viewerID int64, items []TitleResult) {
	if len(items) == 0 {
		return
	}
	var titleIDs []int64
	var refs []string
	for _, it := range items {
		if it.TitleID != nil {
			titleIDs = append(titleIDs, *it.TitleID)
		} else if it.MediaRef != nil {
			refs = append(refs, *it.MediaRef)
		}
	}
	if len(titleIDs) == 0 {
		titleIDs = []int64{0}
	}
	if len(refs) == 0 {
		refs = []string{""}
	}
	keyOf := func(tid *int64, ref *string) string {
		if tid != nil {
			return "t" + itoa(*tid)
		}
		if ref != nil {
			return "r" + *ref
		}
		return ""
	}

	// Own unfinished viewing.
	resume := map[string]int64{}
	var rows []struct {
		TitleID         *int64
		MediaRef        *string
		MediaDurationMs *int64
		LastPositionMs  *int64
	}
	m.db.Raw(`SELECT r.title_id, r.media_ref, r.media_duration_ms, mb.last_position_ms
		FROM watch_room_members mb JOIN watch_rooms r ON r.id = mb.room_id
		WHERE mb.user_id = ? AND mb.last_position_ms > 60000
		  AND (r.title_id IN ? OR r.media_ref IN ?)
		ORDER BY mb.updated_at DESC LIMIT 100`, viewerID, titleIDs, refs).Scan(&rows)
	for _, r := range rows {
		if r.MediaDurationMs == nil || r.LastPositionMs == nil {
			continue
		}
		left := *r.MediaDurationMs - *r.LastPositionMs
		k := keyOf(r.TitleID, r.MediaRef)
		if _, seen := resume[k]; !seen && left > 5*60000 {
			resume[k] = left
		}
	}

	// Friends who watched it in the last 30 days — open rooms only: what
	// someone watched in a code / followers-only room is not for everyone.
	friends := map[string][]int64{}
	var fr []struct {
		TitleID  *int64
		MediaRef *string
		UserID   int64
	}
	m.db.Raw(`SELECT DISTINCT s.title_id, s.media_ref, v.user_id
		FROM watch_session_viewers v
		JOIN watch_room_sessions s ON s.id = v.session_id
		JOIN watch_rooms r ON r.id = s.room_id AND r.access <> 'code' AND r.followers_only = false
		JOIN follows f ON f.following_id = v.user_id AND f.follower_id = ? AND f.status = 'accepted'
		WHERE v.first_seen_at > NOW() - INTERVAL '30 days' AND v.user_id <> ?
		  AND (s.title_id IN ? OR s.media_ref IN ?)
		LIMIT 400`, viewerID, viewerID, titleIDs, refs).Scan(&fr)
	var candidates []int64
	for _, f := range fr {
		candidates = append(candidates, f.UserID)
	}
	blocked := m.blockedWith(viewerID)
	hidden := m.hiddenFor(viewerID, candidates)
	var friendIDs []int64
	for _, f := range fr {
		if blocked[f.UserID] || hidden[f.UserID] {
			continue
		}
		k := keyOf(f.TitleID, f.MediaRef)
		if len(friends[k]) < 3 {
			friends[k] = append(friends[k], f.UserID)
			friendIDs = append(friendIDs, f.UserID)
		}
	}
	users := m.loadUserRows(friendIDs)

	// Live right now.
	live := map[string]bool{}
	var lr []struct {
		TitleID  *int64
		MediaRef *string
	}
	m.db.Raw(`SELECT DISTINCT title_id, media_ref FROM watch_rooms
		WHERE status = 'live' AND access <> 'code' AND locked = false
		  AND (title_id IN ? OR media_ref IN ?)`, titleIDs, refs).Scan(&lr)
	for _, l := range lr {
		live[keyOf(l.TitleID, l.MediaRef)] = true
	}

	for i := range items {
		k := keyOf(items[i].TitleID, items[i].MediaRef)
		if left, ok := resume[k]; ok {
			l := left
			items[i].Social = &SocialJSON{Type: "resume", Users: []UserLite{}, RemainingMs: &l}
			continue
		}
		if ids := friends[k]; len(ids) > 0 {
			s := &SocialJSON{Type: "friends_watched", Users: []UserLite{}}
			for _, id := range ids {
				if u, ok := users[id]; ok && !u.Gone {
					s.Users = append(s.Users, u.lite())
				}
			}
			if len(s.Users) > 0 {
				items[i].Social = s
				continue
			}
		}
		if live[k] {
			items[i].Social = &SocialJSON{Type: "trending", Users: []UserLite{}}
		}
	}
}
