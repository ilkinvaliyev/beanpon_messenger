package watch

import (
	"sync"
	"time"

	"beanpon_messenger/models"
	"beanpon_messenger/utils"
)

// viewer — the calling user's account flags (cached 60 s per user).
type viewer struct {
	ID      int64
	IsGuest bool
	Gone    bool // deleted or deactivated
}

type viewerCacheEntry struct {
	v  viewer
	at time.Time
}

type viewerCache struct {
	mu sync.Mutex
	m  map[int64]viewerCacheEntry
}

var viewers = &viewerCache{m: map[int64]viewerCacheEntry{}}

func (m *Module) loadViewer(id int64) viewer {
	viewers.mu.Lock()
	if e, ok := viewers.m[id]; ok && time.Since(e.at) < time.Minute {
		viewers.mu.Unlock()
		return e.v
	}
	viewers.mu.Unlock()

	var row struct {
		IsGuest bool
		Gone    bool
	}
	err := m.db.Raw(`SELECT COALESCE(is_guest, false) AS is_guest,
		(deleted_at IS NOT NULL OR deactivated_at IS NOT NULL) AS gone
		FROM users WHERE id = ?`, id).Scan(&row).Error
	v := viewer{ID: id, IsGuest: row.IsGuest, Gone: row.Gone}
	if err != nil {
		// Fail open on a DB hiccup: the request itself will hit the DB again.
		return viewer{ID: id}
	}
	viewers.mu.Lock()
	if len(viewers.m) > 20000 {
		viewers.m = map[int64]viewerCacheEntry{}
	}
	viewers.m[id] = viewerCacheEntry{v: v, at: time.Now()}
	viewers.mu.Unlock()
	return v
}

// userRow — the columns UserLite is built from.
type userRow struct {
	ID           int64
	Username     string
	Name         string
	IsVerified   bool
	ProfileImage *string
	IsGuest      bool
	Gone         bool
}

func (u userRow) lite() UserLite {
	return UserLite{
		ID:         u.ID,
		Username:   u.Username,
		Name:       u.Name,
		Avatar:     utils.PrependBaseURL(u.ProfileImage),
		IsVerified: u.IsVerified,
	}
}

// loadUserRows — batch load (one query) of the given ids.
func (m *Module) loadUserRows(ids []int64) map[int64]userRow {
	out := make(map[int64]userRow, len(ids))
	ids = uniqueIDs(ids)
	if len(ids) == 0 {
		return out
	}
	var rows []userRow
	m.db.Raw(`SELECT u.id, u.username, COALESCE(u.name, '') AS name,
			COALESCE(u.is_verified, false) AS is_verified, p.profile_image,
			COALESCE(u.is_guest, false) AS is_guest,
			(u.deleted_at IS NOT NULL OR u.deactivated_at IS NOT NULL) AS gone
		FROM users u
		LEFT JOIN profiles p ON p.user_id = u.id
		WHERE u.id IN ?`, ids).Scan(&rows)
	for _, r := range rows {
		out[r.ID] = r
	}
	return out
}

func (m *Module) loadUsers(ids []int64) map[int64]UserLite {
	rows := m.loadUserRows(ids)
	out := make(map[int64]UserLite, len(rows))
	for id, r := range rows {
		out[id] = r.lite()
	}
	return out
}

func (m *Module) loadUser(id int64) (UserLite, bool) {
	u, ok := m.loadUsers([]int64{id})[id]
	return u, ok
}

// blockedWith — every user that has a block with uid, either direction.
func (m *Module) blockedWith(uid int64) map[int64]bool {
	raw := models.GetBlockedUserIDs(m.db, uint(uid))
	out := make(map[int64]bool, len(raw))
	for id := range raw {
		out[int64(id)] = true
	}
	return out
}

func (m *Module) isBlocked(a, b int64) bool {
	if a == b {
		return false
	}
	return models.IsBlocked(m.db, uint(a), uint(b))
}

// hiddenFor — candidates the viewer can't see because of Hidden Mode (the
// platform switch; returns an empty map while the mode is off).
func (m *Module) hiddenFor(viewerID int64, candidates []int64) map[int64]bool {
	out := map[int64]bool{}
	if len(candidates) == 0 {
		return out
	}
	peers := make([]uint, 0, len(candidates))
	for _, id := range uniqueIDs(candidates) {
		if id != viewerID {
			peers = append(peers, uint(id))
		}
	}
	for id := range models.HiddenBlockedPeerIDs(m.db, uint(viewerID), peers) {
		out[int64(id)] = true
	}
	return out
}

// followingAmong — which of `ids` the follower follows (accepted).
func (m *Module) followingAmong(follower int64, ids []int64) map[int64]bool {
	out := map[int64]bool{}
	ids = uniqueIDs(ids)
	if len(ids) == 0 {
		return out
	}
	var got []int64
	m.db.Raw(`SELECT following_id FROM follows
		WHERE follower_id = ? AND following_id IN ? AND status = 'accepted'`, follower, ids).Scan(&got)
	for _, id := range got {
		out[id] = true
	}
	return out
}

// followersAmong — which of `ids` follow the target (accepted).
func (m *Module) followersAmong(target int64, ids []int64) map[int64]bool {
	out := map[int64]bool{}
	ids = uniqueIDs(ids)
	if len(ids) == 0 {
		return out
	}
	var got []int64
	m.db.Raw(`SELECT follower_id FROM follows
		WHERE following_id = ? AND follower_id IN ? AND status = 'accepted'`, target, ids).Scan(&got)
	for _, id := range got {
		out[id] = true
	}
	return out
}

func (m *Module) follows(follower, target int64) bool {
	if follower == target {
		return true
	}
	return m.followingAmong(follower, []int64{target})[target]
}

func uniqueIDs(ids []int64) []int64 {
	if len(ids) < 2 {
		out := make([]int64, 0, len(ids))
		for _, id := range ids {
			if id > 0 {
				out = append(out, id)
			}
		}
		return out
	}
	seen := make(map[int64]bool, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id > 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// dbNow — timestamps written from Go (the process runs in UTC).
func dbNow() time.Time { return time.Now().UTC() }
