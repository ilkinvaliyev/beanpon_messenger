package watch

import (
	"log"
	"net/http"
	"strings"
	"time"

	"beanpon_messenger/models"
	"beanpon_messenger/utils"

	"github.com/gin-gonic/gin"
)

var (
	errNotStarted     = errf(http.StatusConflict, "ROOM_NOT_STARTED", "Otaq hələ başlamayıb")
	errKicked         = errf(http.StatusForbidden, "KICKED", "Bu otağa qoşula bilməzsən")
	errLocked         = errf(http.StatusForbidden, "ROOM_LOCKED", "Otaq bağlıdır")
	errFull           = errf(http.StatusForbidden, "ROOM_FULL", "Otaq doludur")
	errFollowersOnly  = errf(http.StatusForbidden, "FOLLOWERS_ONLY", "Bu otaq yalnız aparıcının izlədiyi insanlar üçündür")
	errCodeRequired   = errf(http.StatusForbidden, "CODE_REQUIRED", "Otaq kodu lazımdır")
	errRejected       = errf(http.StatusForbidden, "REQUEST_REJECTED", "İstəyin qəbul edilmədi")
	errRoomLimit      = errf(http.StatusForbidden, "ROOM_LIMIT", "Eyni anda açıq otaq limitinə çatdın")
	errDailyLimit     = errf(http.StatusTooManyRequests, "DAILY_LIMIT", "Bu gün üçün otaq limitinə çatdın")
	errNotLive        = errf(http.StatusConflict, "NOT_LIVE", "Otaq hələ canlı deyil")
	errScheduleWindow = errf(http.StatusUnprocessableEntity, "SCHEDULE_INVALID", "Vaxt düzgün deyil")
)

// after — side effects (pushes) collected under the room lock, run after it.
type after []func()

func (a *after) add(f func()) { *a = append(*a, f) }

func (a after) run() {
	for _, f := range a {
		go func(f func()) {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[watch] side effect panic: %v", r)
				}
			}()
			f()
		}(f)
	}
}

// guardWrite — feature switch + guests.
func (m *Module) guardWrite(c *gin.Context) (viewer, error) {
	uid := currentUserID(c)
	if uid == 0 {
		return viewer{}, errf(http.StatusUnauthorized, "UNAUTHORIZED", "Unauthorized")
	}
	if !m.settings.get().s.Enabled {
		return viewer{}, errDisabled
	}
	v := m.loadViewer(uid)
	if v.IsGuest {
		return v, errGuest
	}
	if v.Gone {
		return v, errf(http.StatusForbidden, "ACCOUNT_INACTIVE", "Hesab aktiv deyil")
	}
	return v, nil
}

// hostVisibleTo — the host is reachable for the viewer (not gone, no block,
// Hidden Mode allows it).
func (m *Module) hostVisibleTo(hostID, viewerID int64) bool {
	if hostID == viewerID {
		return true
	}
	if m.loadViewer(hostID).Gone {
		return false
	}
	if m.isBlocked(hostID, viewerID) {
		return false
	}
	return models.CanViewHidden(m.db, uint(viewerID), uint(hostID))
}

func (m *Module) canSeeLocked(r *liveRoom, viewerID int64) bool {
	if mb := r.members[viewerID]; mb != nil && mb.State == "joined" {
		return true
	}
	return m.hostVisibleTo(r.row.HostID, viewerID)
}

// canOpenByIDLocked — GET /rooms/:id. Joined members always; everyone else
// only while the host is visible to them (blocks, Hidden Mode, gone hosts),
// and a code room only through its code, an invite, an own request or an
// earlier membership.
func (m *Module) canOpenByIDLocked(r *liveRoom, viewerID int64) bool {
	if r.member(viewerID) != nil || r.isHost(viewerID) {
		return true
	}
	if !m.hostVisibleTo(r.row.HostID, viewerID) {
		return false
	}
	if r.row.Access != "code" || r.members[viewerID] != nil {
		return true
	}
	var known bool
	m.db.Raw(`SELECT EXISTS (SELECT 1 FROM watch_room_invites WHERE room_id = ? AND invitee_id = ?)
		OR EXISTS (SELECT 1 FROM watch_room_requests WHERE room_id = ? AND user_id = ?)`,
		r.id, viewerID, r.id, viewerID).Scan(&known)
	return known
}

// ── Create ────────────────────────────────────────────────────────────────

type createRoomReq struct {
	mediaInput
	Name          string  `json:"name"`
	Access        string  `json:"access"`
	FollowersOnly bool    `json:"followers_only"`
	ScheduledAt   *string `json:"scheduled_at"`
}

func validAccess(a string) bool { return a == "public" || a == "approval" || a == "code" }

func (m *Module) handleCreateRoom(c *gin.Context) {
	v, err := m.guardWrite(c)
	if err != nil {
		fail(c, err)
		return
	}
	var req createRoomReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, errInvalid)
		return
	}
	cfg := m.settings.get()
	name := clampRunes(strings.TrimSpace(req.Name), 40)
	if name != "" && utils.ContainsBadWord(name) {
		fail(c, errBlockedWord)
		return
	}
	access := strings.TrimSpace(req.Access)
	if access == "" {
		access = "approval"
	}
	if !validAccess(access) {
		fail(c, errInvalid)
		return
	}

	status := "lobby"
	var scheduledAt *time.Time
	if req.ScheduledAt != nil && strings.TrimSpace(*req.ScheduledAt) != "" {
		t, perr := time.Parse(time.RFC3339, strings.TrimSpace(*req.ScheduledAt))
		if perr != nil {
			fail(c, errScheduleWindow)
			return
		}
		t = t.UTC()
		if t.After(time.Now().Add(time.Duration(cfg.s.MaxScheduleDays) * 24 * time.Hour)) {
			fail(c, errScheduleWindow)
			return
		}
		if t.After(time.Now().Add(2 * time.Minute)) {
			status = "scheduled"
			scheduledAt = &t
		}
	}

	var active, today int64
	m.db.Raw(`SELECT COUNT(*) FROM watch_rooms WHERE host_id = ? AND status <> 'ended'`, v.ID).Scan(&active)
	if int(active) >= cfg.s.MaxActiveRoomsPerUser {
		fail(c, errRoomLimit)
		return
	}
	m.db.Raw(`SELECT COUNT(*) FROM watch_rooms WHERE created_by = ? AND created_at > NOW() - INTERVAL '24 hours'`, v.ID).Scan(&today)
	if int(today) >= cfg.s.MaxRoomsPerDay {
		fail(c, errDailyLimit)
		return
	}
	// Media last: a YouTube lookup costs API quota, so only after every check
	// (failed creates don't count toward the daily room limit — the bucket does).
	if !m.limiter.allow("media:"+itoa(v.ID), 0.2, 5) {
		fail(c, errRateLimited)
		return
	}
	media, err := m.resolveMedia(cfg, req.mediaInput)
	if err != nil {
		fail(c, err)
		return
	}
	if name == "" {
		name = clampRunes(media.Title, 40)
	}

	now := dbNow()
	row := roomRow{
		HostID:                 v.ID,
		CreatedBy:              v.ID,
		Name:                   name,
		Provider:               media.Provider,
		TitleID:                media.TitleID,
		MediaRef:               media.Ref,
		MediaTitle:             media.Title,
		MediaSubtitle:          media.Subtitle,
		MediaKind:              media.Kind,
		MediaPosterURL:         media.Poster,
		MediaBackdropURL:       media.Backdrop,
		MediaDurationMs:        media.DurationMs,
		Season:                 media.Season,
		Episode:                media.Episode,
		Access:                 access,
		FollowersOnly:          req.FollowersOnly,
		ChatMode:               "everyone",
		PauseRequestsEnabled:   true,
		FlyingReactionsEnabled: true,
		Status:                 status,
		ScheduledAt:            scheduledAt,
		PlaybackRate:           1,
		PositionUpdatedMs:      nowMs(),
		MemberCount:            1,
		LastActiveAt:           &now,
		CreatedAt:              now,
		UpdatedAt:              now,
	}
	if status == "lobby" {
		row.OpenedAt = &now
	}

	var created bool
	for attempt := 0; attempt < 6 && !created; attempt++ {
		row.ID = 0
		row.Code = newRoomCode()
		tx := m.db.Begin()
		if err := tx.Create(&row).Error; err != nil {
			tx.Rollback()
			if isPgUniqueViolation(err) {
				continue
			}
			log.Printf("[watch] create room: %v", err)
			fail(c, errServer)
			return
		}
		if err := tx.Exec(`INSERT INTO watch_room_members (room_id, user_id, role, state, chat_muted, joined_at, created_at, updated_at)
			VALUES (?, ?, 'host', 'joined', false, NOW(), NOW(), NOW())`, row.ID, v.ID).Error; err != nil {
			tx.Rollback()
			log.Printf("[watch] create room member: %v", err)
			fail(c, errServer)
			return
		}
		if err := tx.Commit().Error; err != nil {
			fail(c, errServer)
			return
		}
		created = true
	}
	if !created {
		fail(c, errServer)
		return
	}

	var out RoomDetail
	err = m.withRoom(row.ID, func(r *liveRoom) error {
		out = m.detailLocked(r, v.ID)
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, out)
}

// ── Read ──────────────────────────────────────────────────────────────────

func (m *Module) handleGetRoom(c *gin.Context) {
	uid := currentUserID(c)
	id, ok := paramID(c, "id")
	if !ok {
		fail(c, errNotFound)
		return
	}
	var out RoomDetail
	err := m.withRoom(id, func(r *liveRoom) error {
		if !m.canOpenByIDLocked(r, uid) {
			return errNotFound
		}
		out = m.detailLocked(r, uid)
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, out)
}

func (m *Module) handleCodeLookup(c *gin.Context) {
	uid := currentUserID(c)
	if !m.limiter.allow("code:"+itoa(uid), 0.2, 10) {
		fail(c, errRateLimited)
		return
	}
	code := normalizeCode(c.Param("code"))
	if len(code) != 6 {
		fail(c, errf(http.StatusNotFound, "CODE_NOT_FOUND", "Bu kodla aktiv otaq yoxdur"))
		return
	}
	var id int64
	m.db.Raw(`SELECT id FROM watch_rooms WHERE code = ? AND status <> 'ended' LIMIT 1`, code).Scan(&id)
	if id == 0 {
		fail(c, errf(http.StatusNotFound, "CODE_NOT_FOUND", "Bu kodla aktiv otaq yoxdur"))
		return
	}
	var out RoomDetail
	err := m.withRoom(id, func(r *liveRoom) error {
		if !m.canSeeLocked(r, uid) || r.row.Status == "ended" {
			return errf(http.StatusNotFound, "CODE_NOT_FOUND", "Bu kodla aktiv otaq yoxdur")
		}
		out = m.detailLocked(r, uid)
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, gin.H{"room": out})
}

// ── Settings (host) ───────────────────────────────────────────────────────

type patchRoomReq struct {
	Name                   *string `json:"name"`
	Access                 *string `json:"access"`
	FollowersOnly          *bool   `json:"followers_only"`
	Locked                 *bool   `json:"locked"`
	ChatMode               *string `json:"chat_mode"`
	PauseRequestsEnabled   *bool   `json:"pause_requests_enabled"`
	FlyingReactionsEnabled *bool   `json:"flying_reactions_enabled"`
}

func (m *Module) handlePatchRoom(c *gin.Context) {
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
	var req patchRoomReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, errInvalid)
		return
	}
	var out RoomDetail
	err = m.withRoom(id, func(r *liveRoom) error {
		if !r.isHost(v.ID) {
			return errNotHost
		}
		if r.row.Status == "ended" {
			return errEnded
		}
		fields := map[string]any{}
		changed := map[string]any{}
		if req.Name != nil {
			n := clampRunes(strings.TrimSpace(*req.Name), 40)
			if n == "" {
				return errInvalid
			}
			if utils.ContainsBadWord(n) {
				return errBlockedWord
			}
			fields["name"], changed["name"] = n, n
		}
		if req.Access != nil {
			if !validAccess(*req.Access) {
				return errInvalid
			}
			fields["access"], changed["access"] = *req.Access, *req.Access
		}
		if req.FollowersOnly != nil {
			fields["followers_only"], changed["followers_only"] = *req.FollowersOnly, *req.FollowersOnly
		}
		if req.Locked != nil {
			fields["locked"], changed["locked"] = *req.Locked, *req.Locked
		}
		if req.ChatMode != nil {
			cm := *req.ChatMode
			if cm != "everyone" && cm != "hosts" && cm != "off" {
				return errInvalid
			}
			fields["chat_mode"], changed["chat_mode"] = cm, cm
		}
		if req.PauseRequestsEnabled != nil {
			fields["pause_requests_enabled"], changed["pause_requests_enabled"] = *req.PauseRequestsEnabled, *req.PauseRequestsEnabled
		}
		if req.FlyingReactionsEnabled != nil {
			fields["flying_reactions_enabled"], changed["flying_reactions_enabled"] = *req.FlyingReactionsEnabled, *req.FlyingReactionsEnabled
		}
		if len(fields) > 0 {
			fields["updated_at"] = time.Now()
			if err := m.db.Table("watch_rooms").Where("id = ?", r.id).Updates(fields).Error; err != nil {
				return errServer
			}
			if v, ok := fields["name"].(string); ok {
				r.row.Name = v
			}
			if v, ok := fields["access"].(string); ok {
				r.row.Access = v
			}
			if v, ok := fields["followers_only"].(bool); ok {
				r.row.FollowersOnly = v
			}
			if v, ok := fields["locked"].(bool); ok {
				r.row.Locked = v
			}
			if v, ok := fields["chat_mode"].(string); ok {
				r.row.ChatMode = v
			}
			if v, ok := fields["pause_requests_enabled"].(bool); ok {
				r.row.PauseRequestsEnabled = v
				if !v && r.pause != nil {
					r.pause = nil
					r.broadcast("pause_request", nil, nil)
				}
			}
			if v, ok := fields["flying_reactions_enabled"].(bool); ok {
				r.row.FlyingReactionsEnabled = v
			}
			r.broadcast("settings", changed, nil)
		}
		out = m.detailLocked(r, v.ID)
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, out)
}

func (m *Module) handleRotateCode(c *gin.Context) {
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
	var code string
	err = m.withRoom(id, func(r *liveRoom) error {
		if !r.isHost(v.ID) {
			return errNotHost
		}
		if r.row.Status == "ended" {
			return errEnded
		}
		for attempt := 0; attempt < 6; attempt++ {
			next := newRoomCode()
			err := m.db.Exec(`UPDATE watch_rooms SET code = ?, updated_at = NOW() WHERE id = ?`, next, r.id).Error
			if err == nil {
				r.row.Code, code = next, next
				r.broadcast("settings", map[string]any{"code": next}, func(c *wsClient) bool { return r.isController(c.userID) })
				return nil
			}
			if !isPgUniqueViolation(err) {
				return errServer
			}
		}
		return errServer
	})
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, gin.H{"code": code})
}

// ── Join / leave ──────────────────────────────────────────────────────────

type joinReq struct {
	Code string `json:"code"`
}

func (m *Module) handleJoin(c *gin.Context) {
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
	var req joinReq
	_ = c.ShouldBindJSON(&req)
	if !m.limiter.allow("join:"+itoa(v.ID)+":"+itoa(id), 0.2, 6) {
		fail(c, errRateLimited)
		return
	}
	codeKey := "joincode:" + itoa(v.ID) + ":" + itoa(id)

	var (
		state string
		out   RoomDetail
		fx    after
	)
	err = m.withRoom(id, func(r *liveRoom) error {
		if !m.canSeeLocked(r, v.ID) {
			return errNotFound
		}
		switch r.row.Status {
		case "ended":
			return errEnded
		case "scheduled":
			return errNotStarted
		}
		mb := r.members[v.ID]
		if mb != nil && mb.State == "kicked" {
			return errKicked
		}
		if mb != nil && mb.State == "joined" {
			state = "joined"
			out = m.detailLocked(r, v.ID)
			return nil
		}
		if r.row.Locked {
			return errLocked
		}
		cfg := m.settings.get()
		if r.joinedCount() >= cfg.s.MaxMembers {
			return errFull
		}
		// An invite pre-approves only while its sender could still let the
		// person in: a host / cohost, or a member of a public room that is not
		// followers-only (a viewer's invite never beats the host's settings).
		var inviter int64
		m.db.Raw(`SELECT inviter_id FROM watch_room_invites WHERE room_id = ? AND invitee_id = ?`, r.id, v.ID).Scan(&inviter)
		invited := inviter != 0 && (r.isController(inviter) || (r.row.Access == "public" && !r.row.FollowersOnly))
		wasMember := mb != nil && mb.State == "left"
		preApproved := invited || wasMember
		if !preApproved && r.row.FollowersOnly && !m.follows(r.row.HostID, v.ID) {
			return errFollowersOnly
		}
		if !preApproved && r.row.Access == "code" {
			// Wrong codes are counted: 5 misses, then one try per 2 minutes.
			if !m.limiter.peek(codeKey, 1.0/120, 5) {
				return errRateLimited
			}
			if normalizeCode(req.Code) != r.row.Code {
				m.limiter.allow(codeKey, 1.0/120, 5)
				return errCodeRequired
			}
			preApproved = true
		}
		if !preApproved && r.row.Access == "approval" {
			var status string
			var decidedAt *time.Time
			m.db.Raw(`SELECT status, decided_at FROM watch_room_requests WHERE room_id = ? AND user_id = ?`, r.id, v.ID).Row().Scan(&status, &decidedAt)
			if status == "rejected" && decidedAt != nil && time.Since(*decidedAt) < time.Duration(cfg.s.RequestTTLMinutes)*time.Minute {
				return errRejected
			}
			ttl := time.Duration(cfg.s.RequestTTLMinutes) * time.Minute
			if err := m.db.Exec(`INSERT INTO watch_room_requests (room_id, user_id, status, expires_at, created_at, updated_at)
				VALUES (?, ?, 'pending', ?, NOW(), NOW())
				ON CONFLICT (room_id, user_id) DO UPDATE SET status = 'pending', expires_at = EXCLUDED.expires_at,
					decided_at = NULL, decided_by = NULL, created_at = CASE WHEN watch_room_requests.status = 'pending'
					THEN watch_room_requests.created_at ELSE NOW() END, updated_at = NOW()`,
				r.id, v.ID, dbNow().Add(ttl)).Error; err != nil {
				return errServer
			}
			state = "requested"
			// One announcement (socket + push) per requester per request TTL,
			// however often they cancel and ask again (module-level, so a room
			// evicted from memory in between does not reset it).
			if status != "pending" && m.limiter.allow("announce:"+itoa(r.id)+":"+itoa(v.ID), 1/ttl.Seconds(), 1) {
				m.announceRequestLocked(r, v.ID, &fx)
			}
			out = m.detailLocked(r, v.ID)
			return nil
		}
		if err := m.addMemberLocked(r, v.ID, "viewer"); err != nil {
			return err
		}
		state = "joined"
		out = m.detailLocked(r, v.ID)
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	fx.run()
	respond(c, gin.H{"state": state, "room": out})
}

// announceRequestLocked — a new join request: WS to online controllers,
// push to the offline ones.
func (m *Module) announceRequestLocked(r *liveRoom, uid int64, fx *after) {
	u := m.ensureUserLocked(r, uid)
	ctx := m.requestContexts(r.row.HostID, joinedIDsLocked(r), []int64{uid})[uid]
	r.toControllers("request", map[string]any{"user": u, "context": ctx, "created_at": time.Now().UTC()})
	roomName, roomID := r.row.Name, r.id
	for _, cid := range r.controllerIDs() {
		if _, online := r.clients[cid]; online {
			continue
		}
		target := cid
		fx.add(func() {
			m.push(target, "watch_request", map[string]string{"user": u.Username, "room": roomName},
				map[string]string{"room_id": itoa(roomID), "user_id": itoa(uid)})
		})
	}
}

func (m *Module) handleCancelRequest(c *gin.Context) {
	uid := currentUserID(c)
	id, ok := paramID(c, "id")
	if !ok {
		fail(c, errNotFound)
		return
	}
	// No room load: the request row is all there is to change.
	res := m.db.Exec(`UPDATE watch_room_requests SET status = 'cancelled', updated_at = NOW()
		WHERE room_id = ? AND user_id = ? AND status = 'pending'`, id, uid)
	if res.Error != nil {
		fail(c, errServer)
		return
	}
	if res.RowsAffected > 0 {
		if r := m.hub.loaded(id); r != nil {
			r.locked(func() {
				if !r.gone {
					r.toControllers("request_resolved", map[string]any{"user_id": uid, "status": "cancelled"})
				}
			})
		}
	}
	respond(c, gin.H{"ok": true})
}

func (m *Module) handleLeave(c *gin.Context) {
	uid := currentUserID(c)
	id, ok := paramID(c, "id")
	if !ok {
		fail(c, errNotFound)
		return
	}
	err := m.withRoom(id, func(r *liveRoom) error {
		mb := r.member(uid)
		if mb == nil {
			return nil
		}
		if r.row.Status == "ended" {
			return nil
		}
		if r.isHost(uid) {
			next := m.nextHostLocked(r, false)
			if next == 0 {
				return m.endRoomLocked(r, "host")
			}
			if err := m.transferHostLocked(r, next, "left"); err != nil {
				return err
			}
		}
		return m.removeMemberLocked(r, uid, "left")
	})
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, gin.H{"ok": true})
}

// ── Membership helpers (room lock held) ───────────────────────────────────

func (m *Module) addMemberLocked(r *liveRoom, uid int64, role string) error {
	var row memberRow
	err := m.db.Raw(`INSERT INTO watch_room_members (room_id, user_id, role, state, chat_muted, joined_at, created_at, updated_at)
		VALUES (?, ?, ?, 'joined', false, NOW(), NOW(), NOW())
		ON CONFLICT (room_id, user_id) DO UPDATE SET state = 'joined', role = EXCLUDED.role,
			joined_at = NOW(), left_at = NULL, updated_at = NOW()
		RETURNING *`, r.id, uid, role).Scan(&row).Error
	if err != nil || row.ID == 0 {
		log.Printf("[watch] room %d add member %d: %v", r.id, uid, err)
		return errServer
	}
	r.members[uid] = &row
	r.countDirty = true
	m.ensureUserLocked(r, uid)
	m.db.Exec(`UPDATE watch_room_requests SET status = 'accepted', decided_at = COALESCE(decided_at, NOW()), updated_at = NOW()
		WHERE room_id = ? AND user_id = ? AND status = 'pending'`, r.id, uid)
	r.broadcast("member", map[string]any{"member": m.memberJSONLocked(r, &row, time.Now()), "change": "joined"}, nil)
	m.systemMessageLocked(r, uid, "joined")
	return nil
}

func (m *Module) removeMemberLocked(r *liveRoom, uid int64, state string) error {
	mb := r.members[uid]
	if mb == nil {
		return nil
	}
	pos := r.expected(nowMs())
	if err := m.db.Exec(`UPDATE watch_room_members SET state = ?, role = 'viewer',
			left_at = NOW(), last_position_ms = ?, updated_at = NOW()
		WHERE room_id = ? AND user_id = ?`, state, pos, r.id, uid).Error; err != nil {
		return errServer
	}
	mb.State = state
	mb.Role = "viewer"
	now := dbNow()
	mb.LeftAt = &now
	mb.LastPositionMs = &pos
	delete(r.ready, uid)
	delete(r.sync, uid)
	if r.pause != nil && r.pause.UserID == uid {
		r.pause = nil
		r.broadcast("pause_request", nil, nil)
	}
	if c := r.clients[uid]; c != nil {
		if state == "kicked" {
			if b := encodeFrame(r.id, "kicked", map[string]any{}); b != nil {
				c.enqueue(b)
			}
		}
		delete(r.clients, uid)
		c.close()
	}
	r.countDirty = true
	r.broadcast("member", map[string]any{"member": m.memberJSONLocked(r, mb, time.Now()), "change": state}, nil)
	if state == "left" {
		m.systemMessageLocked(r, uid, "left")
	}
	return nil
}

// nextHostLocked — cohost (online first), then the earliest joined online
// member, then (unless onlineOnly) the earliest joined member.
func (m *Module) nextHostLocked(r *liveRoom, onlineOnly bool) int64 {
	type cand struct {
		uid    int64
		rank   int
		joined time.Time
	}
	var best *cand
	for uid, mb := range r.members {
		if uid == r.row.HostID || mb.State != "joined" {
			continue
		}
		_, online := r.clients[uid]
		if onlineOnly && !online {
			continue
		}
		rank := 3
		switch {
		case mb.Role == "cohost" && online:
			rank = 0
		case mb.Role == "cohost":
			rank = 1
		case online:
			rank = 2
		}
		j := time.Time{}
		if mb.JoinedAt != nil {
			j = *mb.JoinedAt
		}
		c := cand{uid, rank, j}
		if best == nil || c.rank < best.rank || (c.rank == best.rank && c.joined.Before(best.joined)) {
			cc := c
			best = &cc
		}
	}
	if best == nil {
		return 0
	}
	return best.uid
}

func (m *Module) transferHostLocked(r *liveRoom, next int64, reason string) error {
	prev := r.row.HostID
	tx := m.db.Begin()
	if err := tx.Exec(`UPDATE watch_rooms SET host_id = ?, updated_at = NOW() WHERE id = ?`, next, r.id).Error; err != nil {
		tx.Rollback()
		return errServer
	}
	if err := tx.Exec(`UPDATE watch_room_members SET role = 'host', updated_at = NOW() WHERE room_id = ? AND user_id = ?`, r.id, next).Error; err != nil {
		tx.Rollback()
		return errServer
	}
	if err := tx.Exec(`UPDATE watch_room_members SET role = 'cohost', updated_at = NOW() WHERE room_id = ? AND user_id = ? AND state = 'joined'`, r.id, prev).Error; err != nil {
		tx.Rollback()
		return errServer
	}
	if err := tx.Commit().Error; err != nil {
		return errServer
	}
	r.row.HostID = next
	if mb := r.members[next]; mb != nil {
		mb.Role = "host"
	}
	if mb := r.members[prev]; mb != nil && mb.State == "joined" {
		mb.Role = "cohost"
	}
	r.hostAway = time.Time{}
	host := m.ensureUserLocked(r, next)
	r.broadcast("host", map[string]any{"host": host, "previous_host_id": prev, "reason": reason}, nil)
	now := time.Now()
	if mb := r.members[next]; mb != nil {
		r.broadcast("member", map[string]any{"member": m.memberJSONLocked(r, mb, now), "change": "role"}, nil)
	}
	if mb := r.members[prev]; mb != nil && mb.State == "joined" {
		r.broadcast("member", map[string]any{"member": m.memberJSONLocked(r, mb, now), "change": "role"}, nil)
	}
	r.sendTo(next, "settings", map[string]any{"code": r.row.Code})
	return nil
}

// systemMessageLocked — "X qoşuldu / ayrıldı" lines (once per user per event
// per 10 minutes so leave → rejoin loops do not flood the chat; kept in the
// module limiter so it survives the room being evicted from memory).
func (m *Module) systemMessageLocked(r *liveRoom, uid int64, event string) {
	if !m.limiter.allow("sysmsg:"+itoa(r.id)+":"+itoa(uid)+":"+event, 1.0/600, 1) {
		return
	}
	meta := `{"event":"` + event + `","user_id":` + itoa(uid) + `}`
	pos := r.expected(nowMs())
	row := messageRow{
		RoomID:     r.id,
		SessionID:  r.row.CurrentSessionID,
		UserID:     &uid,
		Type:       "system",
		Meta:       &meta,
		PositionMs: &pos,
		CreatedAt:  dbNow(),
		UpdatedAt:  dbNow(),
	}
	if err := m.db.Create(&row).Error; err != nil {
		return
	}
	r.broadcastFrom(uid, "message", messageJSON(row, r.users))
}

// ── Reminders ─────────────────────────────────────────────────────────────

func (m *Module) handleRemindOn(c *gin.Context)  { m.setReminder(c, true) }
func (m *Module) handleRemindOff(c *gin.Context) { m.setReminder(c, false) }

func (m *Module) setReminder(c *gin.Context, on bool) {
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
	err = m.withRoom(id, func(r *liveRoom) error {
		if !m.canSeeLocked(r, v.ID) {
			return errNotFound
		}
		if r.row.Status == "ended" {
			return errEnded
		}
		if on {
			return m.db.Exec(`INSERT INTO watch_room_reminders (room_id, user_id, created_at, updated_at)
				VALUES (?, ?, NOW(), NOW()) ON CONFLICT (room_id, user_id) DO NOTHING`, r.id, v.ID).Error
		}
		return m.db.Exec(`DELETE FROM watch_room_reminders WHERE room_id = ? AND user_id = ?`, r.id, v.ID).Error
	})
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, gin.H{"reminder": on})
}

// ── Host lifecycle ────────────────────────────────────────────────────────

func (m *Module) handleOpen(c *gin.Context) {
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
	var (
		out RoomDetail
		fx  after
	)
	err = m.withRoom(id, func(r *liveRoom) error {
		if !r.isHost(v.ID) {
			return errNotHost
		}
		switch r.row.Status {
		case "ended":
			return errEnded
		case "scheduled":
			if err := m.openRoomLocked(r, &fx); err != nil {
				return err
			}
		}
		out = m.detailLocked(r, v.ID)
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	fx.run()
	respond(c, out)
}

// openRoomLocked — scheduled → lobby + "it started" pushes to reminder
// subscribers and invitees who have not joined.
func (m *Module) openRoomLocked(r *liveRoom, fx *after) error {
	res := m.db.Exec(`UPDATE watch_rooms SET status = 'lobby', opened_at = NOW(), last_active_at = NOW(), updated_at = NOW()
		WHERE id = ? AND status = 'scheduled'`, r.id)
	if res.Error != nil {
		return errServer
	}
	if res.RowsAffected == 0 {
		return m.statusDriftLocked(r)
	}
	now := dbNow()
	r.row.Status = "lobby"
	r.row.OpenedAt = &now
	r.row.LastActiveAt = &now
	r.lastTouch = time.Now()
	r.broadcast("status", map[string]any{"status": "lobby"}, nil)

	var targets []int64
	m.db.Raw(`SELECT user_id FROM watch_room_reminders WHERE room_id = ? AND notified_at IS NULL
		UNION SELECT invitee_id FROM watch_room_invites WHERE room_id = ?`, r.id, r.id).Scan(&targets)
	m.db.Exec(`UPDATE watch_room_reminders SET notified_at = NOW(), updated_at = NOW() WHERE room_id = ? AND notified_at IS NULL`, r.id)
	host := m.ensureUserLocked(r, r.row.HostID)
	roomName, roomID := r.row.Name, r.id
	for _, uid := range uniqueIDs(targets) {
		if r.member(uid) != nil || uid == r.row.HostID {
			continue
		}
		target := uid
		fx.add(func() {
			m.push(target, "watch_room_opened", map[string]string{"room": roomName, "host": host.Username},
				map[string]string{"room_id": itoa(roomID)})
		})
	}
	return nil
}

type startReq struct {
	PositionMs *int64 `json:"position_ms"`
	// DelaySeconds — optional longer countdown (3…600, default 3).
	DelaySeconds *int `json:"delay_seconds"`
}

func (m *Module) handleStart(c *gin.Context) {
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
	var req startReq
	_ = c.ShouldBindJSON(&req)
	var out RoomDetail
	err = m.withRoom(id, func(r *liveRoom) error {
		if !r.isHost(v.ID) {
			return errNotHost
		}
		switch r.row.Status {
		case "ended":
			return errEnded
		case "scheduled":
			return errNotStarted
		case "live":
			out = m.detailLocked(r, v.ID)
			return nil
		}
		res := m.db.Exec(`UPDATE watch_rooms SET status = 'live', started_at = COALESCE(started_at, NOW()),
				last_active_at = NOW(), updated_at = NOW() WHERE id = ? AND status = 'lobby'`, r.id)
		if res.Error != nil {
			return errServer
		}
		if res.RowsAffected == 0 {
			// Changed behind our back (ended from the admin panel).
			return m.statusDriftLocked(r)
		}
		now := dbNow()
		r.row.Status = "live"
		if r.row.StartedAt == nil {
			r.row.StartedAt = &now
		}
		if r.row.CurrentSessionID == nil || r.sessionEnded {
			if err := m.openSessionLocked(r); err != nil {
				return err
			}
		}
		r.broadcast("status", map[string]any{"status": "live"}, nil)
		pos := int64(0)
		if req.PositionMs != nil && *req.PositionMs > 0 {
			pos = *req.PositionMs
		}
		delay := 3
		if req.DelaySeconds != nil {
			delay = *req.DelaySeconds
		}
		if err := m.startCountdownAfterLocked(r, pos, delay, "start", v.ID); err != nil {
			return err
		}
		out = m.detailLocked(r, v.ID)
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, out)
}

func (m *Module) handleEnd(c *gin.Context) {
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
	err = m.withRoom(id, func(r *liveRoom) error {
		if !r.isHost(v.ID) {
			return errNotHost
		}
		return m.endRoomLocked(r, "host")
	})
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, gin.H{"ok": true})
}

type transferReq struct {
	UserID int64 `json:"user_id"`
}

func (m *Module) handleTransfer(c *gin.Context) {
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
	var req transferReq
	if err := c.ShouldBindJSON(&req); err != nil || req.UserID <= 0 {
		fail(c, errInvalid)
		return
	}
	var out RoomDetail
	err = m.withRoom(id, func(r *liveRoom) error {
		if !r.isHost(v.ID) {
			return errNotHost
		}
		if r.row.Status == "ended" {
			return errEnded
		}
		if req.UserID == v.ID || r.member(req.UserID) == nil {
			return errNotMember
		}
		if err := m.transferHostLocked(r, req.UserID, "transfer"); err != nil {
			return err
		}
		out = m.detailLocked(r, v.ID)
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, out)
}

// ── Media change ──────────────────────────────────────────────────────────

func (m *Module) handleChangeMedia(c *gin.Context) {
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
	// Permission first, the (quota-costing) media lookup after.
	err = m.withRoom(id, func(r *liveRoom) error {
		if !r.isController(v.ID) {
			return errNotController
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
	if !m.limiter.allow("media:"+itoa(v.ID), 0.2, 5) {
		fail(c, errRateLimited)
		return
	}
	media, err := m.resolveMedia(m.settings.get(), in)
	if err != nil {
		fail(c, err)
		return
	}
	var out RoomDetail
	err = m.withRoom(id, func(r *liveRoom) error {
		if !r.isController(v.ID) {
			return errNotController
		}
		if r.row.Status == "ended" {
			return errEnded
		}
		if err := m.applyMediaLocked(r, media, v.ID); err != nil {
			return err
		}
		out = m.detailLocked(r, v.ID)
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, out)
}

// applyMediaLocked — new media: closes the session, resets the clock and,
// when the room is live, opens a new session with a 3 s countdown.
func (m *Module) applyMediaLocked(r *liveRoom, media resolvedMedia, actor int64) error {
	// A controller's own pick beats the open "what next" vote (otherwise the
	// sweeper would switch the film again when the vote times out).
	if r.poll != nil && r.poll.Kind == "next" {
		m.closePollLocked(r, nil)
	}
	m.closeSessionLocked(r)
	if err := m.db.Exec(`UPDATE watch_rooms SET provider = ?, title_id = ?, media_ref = ?, media_title = ?,
			media_subtitle = ?, media_kind = ?, media_poster_url = ?, media_backdrop_url = ?, media_duration_ms = ?,
			season = ?, episode = ?, break_until_ms = NULL, updated_at = NOW()
		WHERE id = ?`, media.Provider, media.TitleID, media.Ref, media.Title, media.Subtitle, media.Kind,
		media.Poster, media.Backdrop, media.DurationMs, media.Season, media.Episode, r.id).Error; err != nil {
		return errServer
	}
	r.row.Provider = media.Provider
	r.row.TitleID = media.TitleID
	r.row.MediaRef = media.Ref
	r.row.MediaTitle = media.Title
	r.row.MediaSubtitle = media.Subtitle
	r.row.MediaKind = media.Kind
	r.row.MediaPosterURL = media.Poster
	r.row.MediaBackdropURL = media.Backdrop
	r.row.MediaDurationMs = media.DurationMs
	r.row.Season = media.Season
	r.row.Episode = media.Episode
	r.row.BreakUntilMs = nil
	// New media always starts at normal speed (a companion app can't follow
	// a rate, and nobody could reset it there).
	r.row.PlaybackRate = 1
	m.clearPauseRequestLocked(r)
	r.sync = map[int64]syncState{}
	for uid := range r.clients {
		r.ready[uid] = "loading"
	}

	if r.row.Status == "live" {
		if err := m.openSessionLocked(r); err != nil {
			return err
		}
	}
	cfg := m.settings.get()
	r.broadcast("media", map[string]any{
		"media":      mediaFromRoom(cfg, &r.row, m.loadTitle(r.row.TitleID)),
		"session_id": r.row.CurrentSessionID,
	}, nil)
	if r.row.Status == "live" {
		return m.startCountdownLocked(r, 0, "media", actor)
	}
	return m.setPlaybackLocked(r, 0, false, 1, nowMs(), "media", actor)
}

// statusDriftLocked — a guarded status UPDATE matched nothing: the DB row
// moved on without this process (Filament ended it). Adopt the DB status.
func (m *Module) statusDriftLocked(r *liveRoom) error {
	var row struct {
		Status    string
		EndReason *string
	}
	if m.db.Raw(`SELECT status, end_reason FROM watch_rooms WHERE id = ?`, r.id).Scan(&row).Error != nil || row.Status == "" {
		return errNotFound
	}
	if row.Status == "ended" && r.row.Status != "ended" {
		reason := "admin"
		if row.EndReason != nil && *row.EndReason != "" {
			reason = *row.EndReason
		}
		m.closeSessionLocked(r)
		r.row.Status = "ended"
		r.row.EndReason = &reason
		r.row.IsPlaying = false
		r.broadcast("ended", map[string]any{"end_reason": reason, "session_id": r.row.CurrentSessionID}, nil)
		for _, c := range r.clients {
			c.close()
		}
		return errEnded
	}
	r.row.Status = row.Status
	return errf(http.StatusConflict, "STATUS_CHANGED", "Otağın vəziyyəti dəyişib")
}

// ── Internal (Filament) ───────────────────────────────────────────────────

func (m *Module) handleInternalEnd(c *gin.Context) {
	id, ok := paramID(c, "id")
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	err := m.withRoom(id, func(r *liveRoom) error {
		return m.endRoomLocked(r, "admin")
	})
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (m *Module) handleInternalReloadSettings(c *gin.Context) {
	m.settings.invalidate()
	m.settings.get()
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
