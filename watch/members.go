package watch

import (
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
)

var (
	errNoRequest   = errf(http.StatusNotFound, "REQUEST_NOT_FOUND", "İstək tapılmadı")
	errTooManyHost = errf(http.StatusConflict, "COHOST_LIMIT", "Köməkçi limiti doldu")
)

const (
	maxCohosts     = 5
	acceptAllBatch = 25
)

// ── Members ───────────────────────────────────────────────────────────────

func (m *Module) handleMembers(c *gin.Context) {
	uid := currentUserID(c)
	id, ok := paramID(c, "id")
	if !ok {
		fail(c, errNotFound)
		return
	}
	var (
		list   []MemberJSON
		online int
	)
	err := m.withRoom(id, func(r *liveRoom) error {
		if r.member(uid) == nil {
			return errNotMember
		}
		now := time.Now()
		blocked := m.blockedWith(uid)
		for _, mb := range r.members {
			if mb.State != "joined" || (blocked[mb.UserID] && mb.UserID != r.row.HostID) {
				continue
			}
			list = append(list, m.memberJSONLocked(r, mb, now))
		}
		online = len(r.clients)
		rank := func(mj MemberJSON) int {
			switch {
			case mj.Role == "host":
				return 0
			case mj.Role == "cohost":
				return 1
			case mj.Online:
				return 2
			}
			return 3
		}
		sort.SliceStable(list, func(i, j int) bool {
			ri, rj := rank(list[i]), rank(list[j])
			if ri != rj {
				return ri < rj
			}
			ti, tj := time.Time{}, time.Time{}
			if list[i].JoinedAt != nil {
				ti = *list[i].JoinedAt
			}
			if list[j].JoinedAt != nil {
				tj = *list[j].JoinedAt
			}
			return ti.Before(tj)
		})
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	if list == nil {
		list = []MemberJSON{}
	}
	respond(c, gin.H{"members": list, "online_count": online})
}

type roleReq struct {
	Role string `json:"role"`
}

func (m *Module) handleMemberRole(c *gin.Context) {
	v, err := m.guardWrite(c)
	if err != nil {
		fail(c, err)
		return
	}
	id, ok1 := paramID(c, "id")
	target, ok2 := paramID(c, "uid")
	if !ok1 || !ok2 {
		fail(c, errNotFound)
		return
	}
	var req roleReq
	if err := c.ShouldBindJSON(&req); err != nil || (req.Role != "cohost" && req.Role != "viewer") {
		fail(c, errInvalid)
		return
	}
	var out MemberJSON
	err = m.withRoom(id, func(r *liveRoom) error {
		if !r.isHost(v.ID) {
			return errNotHost
		}
		mb := r.member(target)
		if mb == nil || target == r.row.HostID {
			return errNotMember
		}
		if req.Role == "cohost" && mb.Role != "cohost" {
			n := 0
			for _, x := range r.members {
				if x.State == "joined" && x.Role == "cohost" {
					n++
				}
			}
			if n >= maxCohosts {
				return errTooManyHost
			}
		}
		if err := m.db.Exec(`UPDATE watch_room_members SET role = ?, updated_at = NOW() WHERE room_id = ? AND user_id = ?`,
			req.Role, r.id, target).Error; err != nil {
			return errServer
		}
		mb.Role = req.Role
		out = m.memberJSONLocked(r, mb, time.Now())
		r.broadcast("member", map[string]any{"member": out, "change": "role"}, nil)
		if req.Role == "cohost" {
			r.sendTo(target, "settings", map[string]any{"code": r.row.Code})
		}
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, out)
}

type muteReq struct {
	Muted bool `json:"muted"`
}

func (m *Module) handleMemberChatMute(c *gin.Context) {
	v, err := m.guardWrite(c)
	if err != nil {
		fail(c, err)
		return
	}
	id, ok1 := paramID(c, "id")
	target, ok2 := paramID(c, "uid")
	if !ok1 || !ok2 {
		fail(c, errNotFound)
		return
	}
	var req muteReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, errInvalid)
		return
	}
	var out MemberJSON
	err = m.withRoom(id, func(r *liveRoom) error {
		if !r.isHost(v.ID) {
			return errNotHost
		}
		mb := r.member(target)
		if mb == nil || target == r.row.HostID {
			return errNotMember
		}
		if err := m.db.Exec(`UPDATE watch_room_members SET chat_muted = ?, updated_at = NOW() WHERE room_id = ? AND user_id = ?`,
			req.Muted, r.id, target).Error; err != nil {
			return errServer
		}
		mb.ChatMuted = req.Muted
		out = m.memberJSONLocked(r, mb, time.Now())
		r.broadcast("member", map[string]any{"member": out, "change": "updated"}, nil)
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, out)
}

func (m *Module) handleKick(c *gin.Context) {
	v, err := m.guardWrite(c)
	if err != nil {
		fail(c, err)
		return
	}
	id, ok1 := paramID(c, "id")
	target, ok2 := paramID(c, "uid")
	if !ok1 || !ok2 {
		fail(c, errNotFound)
		return
	}
	err = m.withRoom(id, func(r *liveRoom) error {
		if !r.isHost(v.ID) {
			return errNotHost
		}
		if target == r.row.HostID {
			return errInvalid
		}
		mb := r.members[target]
		if mb == nil {
			// Not a member yet (a requester): record the kick so they can't come back.
			if err := m.db.Exec(`INSERT INTO watch_room_members (room_id, user_id, role, state, chat_muted, created_at, updated_at, left_at)
				VALUES (?, ?, 'viewer', 'kicked', false, NOW(), NOW(), NOW())
				ON CONFLICT (room_id, user_id) DO UPDATE SET state = 'kicked', left_at = NOW(), updated_at = NOW()`, r.id, target).Error; err != nil {
				return errServer
			}
			now := dbNow()
			r.members[target] = &memberRow{RoomID: r.id, UserID: target, Role: "viewer", State: "kicked", LeftAt: &now}
		} else if err := m.removeMemberLocked(r, target, "kicked"); err != nil {
			return err
		}
		m.db.Exec(`UPDATE watch_room_requests SET status = 'rejected', decided_at = NOW(), decided_by = ?, updated_at = NOW()
			WHERE room_id = ? AND user_id = ? AND status = 'pending'`, v.ID, r.id, target)
		m.db.Exec(`DELETE FROM watch_room_invites WHERE room_id = ? AND invitee_id = ?`, r.id, target)
		r.toControllers("request_resolved", map[string]any{"user_id": target, "status": "rejected"})
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, gin.H{"ok": true})
}

// ── Join requests ─────────────────────────────────────────────────────────

type requestContextJSON struct {
	Type  string    `json:"type"` // mutual | friend_in_room | mutual_friends
	User  *UserLite `json:"user,omitempty"`
	Count int       `json:"count,omitempty"`
}

// joinedIDsLocked — joined members except the host (room lock held).
func joinedIDsLocked(r *liveRoom) []int64 {
	ids := make([]int64, 0, len(r.members))
	for id, mb := range r.members {
		if mb.State == "joined" && id != r.row.HostID {
			ids = append(ids, id)
		}
	}
	return ids
}

// requestContexts — why the host may know each requester: a mutual follow,
// a member of the room who follows them, or mutual friends. Three batched
// queries for any number of requesters; needs no room lock.
func (m *Module) requestContexts(host int64, memberIDs, ids []int64) map[int64]*requestContextJSON {
	out := map[int64]*requestContextJSON{}
	ids = uniqueIDs(ids)
	if len(ids) == 0 {
		return out
	}
	hostFollows := m.followingAmong(host, ids)
	followHost := m.followersAmong(host, ids)
	rest := make([]int64, 0, len(ids))
	for _, id := range ids {
		if hostFollows[id] && followHost[id] {
			out[id] = &requestContextJSON{Type: "mutual"}
		} else {
			rest = append(rest, id)
		}
	}
	if len(rest) > 0 && len(memberIDs) > 0 {
		var pairs []struct {
			Requester int64
			Member    int64
		}
		m.db.Raw(`SELECT DISTINCT ON (following_id) following_id AS requester, follower_id AS member
			FROM follows WHERE following_id IN ? AND follower_id IN ? AND status = 'accepted'
			ORDER BY following_id, follower_id`, rest, memberIDs).Scan(&pairs)
		if len(pairs) > 0 {
			friendIDs := make([]int64, 0, len(pairs))
			for _, p := range pairs {
				friendIDs = append(friendIDs, p.Member)
			}
			users := m.loadUsers(friendIDs)
			for _, p := range pairs {
				if u, ok := users[p.Member]; ok {
					uu := u
					out[p.Requester] = &requestContextJSON{Type: "friend_in_room", User: &uu}
				}
			}
		}
	}
	left := make([]int64, 0, len(rest))
	for _, id := range rest {
		if out[id] == nil {
			left = append(left, id)
		}
	}
	if len(left) > 0 {
		var counts []struct {
			UserID int64
			N      int
		}
		m.db.Raw(`SELECT b.following_id AS user_id, COUNT(*) AS n FROM follows a
			JOIN follows b ON b.follower_id = a.following_id AND b.status = 'accepted'
			WHERE a.follower_id = ? AND a.status = 'accepted' AND b.following_id IN ?
			GROUP BY b.following_id`, host, left).Scan(&counts)
		for _, c := range counts {
			if c.N > 0 {
				out[c.UserID] = &requestContextJSON{Type: "mutual_friends", Count: c.N}
			}
		}
	}
	return out
}

type requestJSON struct {
	User      UserLite            `json:"user"`
	CreatedAt time.Time           `json:"created_at"`
	ExpiresAt *time.Time          `json:"expires_at"`
	Context   *requestContextJSON `json:"context"`
}

func (m *Module) handleRequests(c *gin.Context) {
	uid := currentUserID(c)
	id, ok := paramID(c, "id")
	if !ok {
		fail(c, errNotFound)
		return
	}
	var (
		host      int64
		memberIDs []int64
	)
	err := m.withRoom(id, func(r *liveRoom) error {
		if !r.isController(uid) {
			return errNotController
		}
		host = r.row.HostID
		memberIDs = joinedIDsLocked(r)
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	// The list itself is built without the room lock (up to 100 requesters).
	var rows []struct {
		UserID    int64
		CreatedAt time.Time
		ExpiresAt *time.Time
	}
	m.db.Raw(`SELECT user_id, created_at, expires_at FROM watch_room_requests
		WHERE room_id = ? AND status = 'pending' AND (expires_at IS NULL OR expires_at > NOW())
		ORDER BY created_at ASC LIMIT 100`, id).Scan(&rows)
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.UserID)
	}
	users := m.loadUserRows(ids)
	blocked := m.blockedWith(host)
	contexts := m.requestContexts(host, memberIDs, ids)
	out := make([]requestJSON, 0, len(rows))
	for _, row := range rows {
		u, ok := users[row.UserID]
		if !ok || u.Gone || blocked[row.UserID] {
			continue
		}
		out = append(out, requestJSON{
			User: u.lite(), CreatedAt: row.CreatedAt, ExpiresAt: row.ExpiresAt,
			Context: contexts[row.UserID],
		})
	}
	respond(c, gin.H{"requests": out})
}

func (m *Module) handleAcceptRequest(c *gin.Context) { m.decideRequest(c, true) }
func (m *Module) handleRejectRequest(c *gin.Context) { m.decideRequest(c, false) }

func (m *Module) decideRequest(c *gin.Context, accept bool) {
	v, err := m.guardWrite(c)
	if err != nil {
		fail(c, err)
		return
	}
	id, ok1 := paramID(c, "id")
	target, ok2 := paramID(c, "uid")
	if !ok1 || !ok2 {
		fail(c, errNotFound)
		return
	}
	var fx after
	err = m.withRoom(id, func(r *liveRoom) error {
		if !r.isController(v.ID) {
			return errNotController
		}
		if r.row.Status == "ended" {
			return errEnded
		}
		return m.decideLocked(r, v.ID, target, accept, &fx)
	})
	if err != nil {
		fail(c, err)
		return
	}
	fx.run()
	respond(c, gin.H{"ok": true})
}

func (m *Module) decideLocked(r *liveRoom, actor, target int64, accept bool, fx *after) error {
	var status string
	m.db.Raw(`SELECT status FROM watch_room_requests WHERE room_id = ? AND user_id = ?
		AND (expires_at IS NULL OR expires_at > NOW())`, r.id, target).Scan(&status)
	if status != "pending" {
		return errNoRequest
	}
	if accept {
		if mb := r.members[target]; mb != nil && mb.State == "kicked" {
			return errKicked
		}
		// A requester who is gone or now has a block with the host can never
		// get in: the request is dropped instead.
		if m.loadViewer(target).Gone || m.isBlocked(r.row.HostID, target) {
			m.dropRequestsLocked(r, []int64{target})
			return errNoRequest
		}
		if r.joinedCount() >= m.settings.get().s.MaxMembers {
			return errFull
		}
		if err := m.db.Exec(`UPDATE watch_room_requests SET status = 'accepted', decided_at = NOW(), decided_by = ?, updated_at = NOW()
			WHERE room_id = ? AND user_id = ?`, actor, r.id, target).Error; err != nil {
			return errServer
		}
		if err := m.addMemberLocked(r, target, "viewer"); err != nil {
			return err
		}
		host := m.ensureUserLocked(r, actor)
		roomID := r.id
		fx.add(func() {
			m.push(target, "watch_request_accepted", map[string]string{"host": host.Username},
				map[string]string{"room_id": itoa(roomID)})
		})
	} else {
		if err := m.db.Exec(`UPDATE watch_room_requests SET status = 'rejected', decided_at = NOW(), decided_by = ?, updated_at = NOW()
			WHERE room_id = ? AND user_id = ?`, actor, r.id, target).Error; err != nil {
			return errServer
		}
	}
	st := "rejected"
	if accept {
		st = "accepted"
	}
	r.toControllers("request_resolved", map[string]any{"user_id": target, "status": st})
	return nil
}

// dropRequestsLocked — pending requests that can never be accepted are
// cancelled (controllers' lists drop them).
func (m *Module) dropRequestsLocked(r *liveRoom, ids []int64) {
	if len(ids) == 0 {
		return
	}
	if m.db.Exec(`UPDATE watch_room_requests SET status = 'cancelled', updated_at = NOW()
		WHERE room_id = ? AND user_id IN ? AND status = 'pending'`, r.id, ids).Error != nil {
		return
	}
	for _, uid := range ids {
		r.toControllers("request_resolved", map[string]any{"user_id": uid, "status": "cancelled"})
	}
}

func (m *Module) handleAcceptAll(c *gin.Context) {
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
		fx        after
		accepted  int
		remaining int64
	)
	err = m.withRoom(id, func(r *liveRoom) error {
		if !r.isController(v.ID) {
			return errNotController
		}
		if r.row.Status == "ended" {
			return errEnded
		}
		// At most acceptAllBatch per call (each one is a few writes under the
		// room lock); the client calls again while `remaining` > 0. Requests
		// that can never be accepted (gone account, block with the host) are
		// dropped on the way, so `remaining` always reaches 0.
		var ids []int64
		m.db.Raw(`SELECT user_id FROM watch_room_requests WHERE room_id = ? AND status = 'pending'
			AND (expires_at IS NULL OR expires_at > NOW()) ORDER BY created_at ASC LIMIT 200`, r.id).Scan(&ids)
		users := m.loadUserRows(ids)
		blocked := m.blockedWith(r.row.HostID)
		var dead []int64
		tried := 0
		for _, uid := range ids {
			if u, ok := users[uid]; !ok || u.Gone || blocked[uid] {
				dead = append(dead, uid)
				continue
			}
			if tried >= acceptAllBatch || r.joinedCount() >= m.settings.get().s.MaxMembers {
				break
			}
			tried++
			if m.decideLocked(r, v.ID, uid, true, &fx) == nil {
				accepted++
			}
		}
		m.dropRequestsLocked(r, dead)
		if r.joinedCount() < m.settings.get().s.MaxMembers {
			m.db.Raw(`SELECT COUNT(*) FROM watch_room_requests WHERE room_id = ? AND status = 'pending'
				AND (expires_at IS NULL OR expires_at > NOW())`, r.id).Scan(&remaining)
		}
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	fx.run()
	respond(c, gin.H{"accepted": accepted, "remaining": remaining})
}

// ── Invites ───────────────────────────────────────────────────────────────

type candidateJSON struct {
	User    UserLite `json:"user"`
	Invited bool     `json:"invited"`
	Member  bool     `json:"member"`
}

func (m *Module) handleInviteCandidates(c *gin.Context) {
	uid := currentUserID(c)
	id, ok := paramID(c, "id")
	if !ok {
		fail(c, errNotFound)
		return
	}
	q := foldSearch(c.Query("q"))
	var (
		members map[int64]bool
		host    int64
	)
	err := m.withRoom(id, func(r *liveRoom) error {
		if r.member(uid) == nil {
			return errNotMember
		}
		members = map[int64]bool{}
		for mid, mb := range r.members {
			if mb.State == "joined" {
				members[mid] = true
			}
		}
		host = r.row.HostID
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	args := []any{uid}
	where := ""
	if q != "" {
		like := "%" + escapeLike(q) + "%"
		where = ` AND (LOWER(u.username) LIKE ? OR LOWER(COALESCE(u.name, '')) LIKE ?)`
		args = append(args, like, like)
	}
	var ids []int64
	m.db.Raw(`SELECT u.id FROM follows f
		JOIN users u ON u.id = f.following_id
		WHERE f.follower_id = ? AND f.status = 'accepted'
		  AND u.deleted_at IS NULL AND u.deactivated_at IS NULL AND COALESCE(u.is_guest, false) = false`+where+`
		ORDER BY EXISTS (SELECT 1 FROM follows b WHERE b.follower_id = u.id AND b.following_id = f.follower_id AND b.status = 'accepted') DESC,
			f.created_at DESC
		LIMIT 60`, args...).Scan(&ids)
	blocked := m.blockedWith(uid)
	hostBlocked := m.blockedWith(host)
	hidden := m.hiddenFor(uid, ids)
	var invited []int64
	if len(ids) > 0 {
		m.db.Raw(`SELECT invitee_id FROM watch_room_invites WHERE room_id = ? AND invitee_id IN ?`, id, ids).Scan(&invited)
	}
	inv := map[int64]bool{}
	for _, x := range invited {
		inv[x] = true
	}
	users := m.loadUsers(ids)
	out := make([]candidateJSON, 0, len(ids))
	for _, cid := range ids {
		if blocked[cid] || hostBlocked[cid] || hidden[cid] || cid == uid {
			continue
		}
		u, ok := users[cid]
		if !ok {
			continue
		}
		out = append(out, candidateJSON{User: u, Invited: inv[cid], Member: members[cid]})
		if len(out) >= 50 {
			break
		}
	}
	respond(c, gin.H{"users": out})
}

type inviteReq struct {
	UserIDs []int64 `json:"user_ids"`
}

func (m *Module) handleInvite(c *gin.Context) {
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
	var req inviteReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, errInvalid)
		return
	}
	ids := uniqueIDs(req.UserIDs)
	if len(ids) == 0 {
		respond(c, gin.H{"invited": 0})
		return
	}
	if len(ids) > 50 {
		ids = ids[:50]
	}
	if !m.limiter.allow("invite:"+itoa(v.ID), 0.5, 5) {
		fail(c, errRateLimited)
		return
	}
	var (
		fx      after
		invited int
	)
	err = m.withRoom(id, func(r *liveRoom) error {
		if r.member(v.ID) == nil {
			return errNotMember
		}
		if r.row.Status == "ended" {
			return errEnded
		}
		if !r.isController(v.ID) && r.row.Access != "public" {
			return errNotController
		}
		// Only people the inviter follows, nobody blocked with inviter or host.
		follows := m.followingAmong(v.ID, ids)
		blocked := m.blockedWith(v.ID)
		hostBlocked := m.blockedWith(r.row.HostID)
		users := m.loadUserRows(ids)
		inviter := m.ensureUserLocked(r, v.ID)
		roomName, roomID := r.row.Name, r.id
		for _, uid := range ids {
			u, ok := users[uid]
			if !ok || u.Gone || u.IsGuest || !follows[uid] || blocked[uid] || hostBlocked[uid] {
				continue
			}
			if mb := r.members[uid]; mb != nil && (mb.State == "joined" || mb.State == "kicked") {
				continue
			}
			res := m.db.Exec(`INSERT INTO watch_room_invites (room_id, inviter_id, invitee_id, created_at, updated_at)
				VALUES (?, ?, ?, NOW(), NOW()) ON CONFLICT (room_id, invitee_id) DO NOTHING`, r.id, v.ID, uid)
			if res.Error != nil || res.RowsAffected == 0 {
				continue
			}
			invited++
			if r.row.Status == "scheduled" {
				continue // they get "it started" when the host opens the room
			}
			target := uid
			fx.add(func() {
				m.push(target, "watch_invite", map[string]string{"host": inviter.Username, "room": roomName},
					map[string]string{"room_id": itoa(roomID)})
			})
		}
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	fx.run()
	respond(c, gin.H{"invited": invited})
}

// ── Host nudges ───────────────────────────────────────────────────────────

func (m *Module) handleRemindUnready(c *gin.Context) {
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
		fx    after
		count int
	)
	err = m.withRoom(id, func(r *liveRoom) error {
		if !r.isHost(v.ID) {
			return errNotHost
		}
		if r.row.Status == "ended" {
			return errEnded
		}
		if time.Since(r.remindAt) < time.Minute {
			return errRateLimited
		}
		r.remindAt = time.Now()
		var targets []int64
		for uid, mb := range r.members {
			if mb.State != "joined" || uid == r.row.HostID {
				continue
			}
			if _, online := r.clients[uid]; !online || r.ready[uid] != "ready" {
				targets = append(targets, uid)
			}
		}
		var invitees []int64
		m.db.Raw(`SELECT invitee_id FROM watch_room_invites WHERE room_id = ?`, r.id).Scan(&invitees)
		for _, uid := range invitees {
			if r.members[uid] == nil {
				targets = append(targets, uid)
			}
		}
		roomName, roomID := r.row.Name, r.id
		for _, uid := range uniqueIDs(targets) {
			target := uid
			count++
			fx.add(func() {
				m.push(target, "watch_remind", map[string]string{"room": roomName}, map[string]string{"room_id": itoa(roomID)})
			})
		}
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	fx.run()
	respond(c, gin.H{"reminded": count})
}

func (m *Module) handleHelp(c *gin.Context) {
	v, err := m.guardWrite(c)
	if err != nil {
		fail(c, err)
		return
	}
	id, ok1 := paramID(c, "id")
	target, ok2 := paramID(c, "uid")
	if !ok1 || !ok2 {
		fail(c, errNotFound)
		return
	}
	var fx after
	err = m.withRoom(id, func(r *liveRoom) error {
		if !r.isHost(v.ID) {
			return errNotHost
		}
		if r.member(target) == nil {
			return errNotMember
		}
		if t, ok := r.helpAt[target]; ok && time.Since(t) < time.Minute {
			return errRateLimited
		}
		r.helpAt[target] = time.Now()
		from := m.ensureUserLocked(r, v.ID)
		p := m.settings.get().provider(r.row.Provider)
		providerName := r.row.Provider
		if p != nil {
			providerName = p.Name
		}
		r.sendTo(target, "help", map[string]any{"from": from, "provider": r.row.Provider})
		roomID := r.id
		if _, online := r.clients[target]; !online {
			fx.add(func() {
				m.push(target, "watch_help", map[string]string{"host": from.Username, "provider": providerName},
					map[string]string{"room_id": itoa(roomID)})
			})
		}
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	fx.run()
	respond(c, gin.H{"ok": true})
}

// itoa — int64 → decimal string.
func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
