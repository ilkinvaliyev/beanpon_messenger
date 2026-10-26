package watch

// "Kino zalı" — the 3D cinema mode (docs/WATCH_PARTY.md §13).
//
// A cinema room is a hall of 6…60 seats (rows × cols from layoutFor) with
// side couches for when it is full. Every joined member has one place
// (watch_room_seats): a seat, a couch spot, or "none" (nothing free). Places
// are given by the server so apps that don't know the mode still sit
// somewhere and never notice anything: joining auto-seats (best free seat →
// couch → none), leaving frees the place. New apps (socket `cinema=1`) pick /
// change seats, offer swaps, wait in the queue and switch between the hall
// and the plain player ("Tam ekran" keeps the seat).
//
// All state is guarded by the room lock (liveRoom.mu) like the rest of the
// room; DB writes happen under it, pushes never do.

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const (
	cinemaMinSeats     = 6
	cinemaMaxSeats     = 60
	cinemaMaxReserve   = 2
	cinemaOfferTTL     = 30 * time.Second
	cinemaSwapTTL      = 30 * time.Second
	cinemaReserveGrace = 10 * time.Minute // reserved seats freed this long after the film started
	cinemaReclaimAfter = 10 * time.Minute // a seat of someone offline this long goes to the queue
)

var (
	errCinemaOff     = errf(http.StatusConflict, "CINEMA_OFF", "Bu otaq kino zalı deyil")
	errSeatTaken     = errf(http.StatusConflict, "SEAT_TAKEN", "Bu yer artıq doludur")
	errSeatChangeOff = errf(http.StatusForbidden, "SEAT_CHANGE_OFF", "Bu otaqda yer dəyişmək olmur")
	errSwapUnavail   = errf(http.StatusConflict, "SWAP_UNAVAILABLE", "Bu istifadəçiyə indi təklif göndərmək olmur")
	errNoOffer       = errf(http.StatusNotFound, "OFFER_NOT_FOUND", "Təklif artıq keçərli deyil")
)

// ── DB row ────────────────────────────────────────────────────────────────

type seatRow struct {
	ID          int64      `gorm:"column:id;primaryKey"`
	RoomID      int64      `gorm:"column:room_id"`
	UserID      int64      `gorm:"column:user_id"`
	Kind        string     `gorm:"column:kind"` // seat | couch | none
	Idx         int        `gorm:"column:idx"`
	View        string     `gorm:"column:view"` // hall | full
	Reserved    bool       `gorm:"column:reserved"`
	Chosen      bool       `gorm:"column:chosen"`
	QueueNotify bool       `gorm:"column:queue_notify"`
	QueuedAt    *time.Time `gorm:"column:queued_at"`
	CreatedAt   time.Time  `gorm:"column:created_at"`
	UpdatedAt   time.Time  `gorm:"column:updated_at"`
}

func (seatRow) TableName() string { return "watch_room_seats" }

// ── Layout ────────────────────────────────────────────────────────────────

// cinemaLayout — rows × cols of the hall. The back row holds the remainder
// (seats − cols × (rows − 1)), centred by the apps. Seat index = row × cols +
// column, row 0 = "A" (front).
type cinemaLayout struct {
	Seats, Rows, Cols, Couch int
}

func clampSeats(n int) int {
	if n < cinemaMinSeats {
		return cinemaMinSeats
	}
	if n > cinemaMaxSeats {
		return cinemaMaxSeats
	}
	return n
}

// layoutFor — 12 → 3×4, 18 → 3×6, 24 → 4×6, 40 → 5×8, 60 → 6×10 (wider
// than deep, like a real hall; a neighbouring width that fills every row
// wins). Couches: a third of the seats, 2…6 spots.
func layoutFor(n int) cinemaLayout {
	n = clampSeats(n)
	clampCols := func(c int) int {
		if c < 3 {
			return 3
		}
		if c > 10 {
			return 10
		}
		return c
	}
	cols := clampCols(int(math.Round(math.Sqrt(float64(n) * 1.6))))
	if n%cols != 0 {
		for _, c := range []int{clampCols(cols + 1), clampCols(cols - 1)} {
			if n%c == 0 {
				cols = c
				break
			}
		}
	}
	rows := (n + cols - 1) / cols
	couch := n / 3
	if couch < 2 {
		couch = 2
	}
	if couch > 6 {
		couch = 6
	}
	return cinemaLayout{Seats: n, Rows: rows, Cols: cols, Couch: couch}
}

func (l cinemaLayout) rowLen(row int) int {
	if row == l.Rows-1 {
		return l.Seats - l.Cols*(l.Rows-1)
	}
	return l.Cols
}

// code — "C4": row letter + 1-based seat number in the row.
func (l cinemaLayout) code(idx int) string {
	if idx < 0 || idx >= l.Seats {
		return ""
	}
	row, col := idx/l.Cols, idx%l.Cols
	return string(rune('A'+row)) + itoa(int64(col+1))
}

// score — lower is better: the middle row, the centre of the row (the apps
// call the best ones "Ən yaxşı görünüş").
func (l cinemaLayout) score(idx int) float64 {
	row, col := idx/l.Cols, idx%l.Cols
	center := float64(l.rowLen(row)-1) / 2
	return math.Abs(float64(row)-float64(l.Rows-1)*0.5) + 0.7*math.Abs(float64(col)-center)
}

// ── In-memory state ───────────────────────────────────────────────────────

type seatOffer struct {
	UserID int64
	Idx    int
	Until  time.Time
	Tried  map[int64]bool // asked already for this seat (declined / timed out)
}

type swapOffer struct {
	ID       string
	From, To int64
	ToIdx    int // the seat that was asked for (the swap is void once To moves)
	Until    time.Time
}

type cinemaState struct {
	layout       cinemaLayout
	allowChange  bool
	places       map[int64]*seatRow // user → place (reserved friends included)
	offlineSince map[int64]time.Time
	offers       map[int]*seatOffer // seat → open offer (the seat is held)
	swaps        map[string]*swapOffer
	lastSweep    time.Time
	// rev grows on every change sent to the clients; it starts at the load
	// time (ms) so it keeps growing across a restart. Clients drop an answer
	// older than the state they already have.
	rev int64
}

func (cs *cinemaState) holderOf(kind string, idx int) int64 {
	for uid, p := range cs.places {
		if p.Kind == kind && p.Idx == idx {
			return uid
		}
	}
	return 0
}

func (m *Module) loadCinema(r *liveRoom) {
	if !r.row.CinemaEnabled {
		return
	}
	seats := 0
	if r.row.CinemaSeats != nil {
		seats = *r.row.CinemaSeats
	}
	cs := &cinemaState{
		layout:       layoutFor(seats),
		allowChange:  r.row.CinemaAllowSeatChange,
		places:       map[int64]*seatRow{},
		offlineSince: map[int64]time.Time{},
		offers:       map[int]*seatOffer{},
		swaps:        map[string]*swapOffer{},
		lastSweep:    time.Now(),
		rev:          time.Now().UnixMilli(),
	}
	var rows []seatRow
	if err := m.db.Where("room_id = ?", r.id).Find(&rows).Error; err != nil {
		log.Printf("[watch] room %d load seats: %v", r.id, err)
	}
	var missing []int64
	now := time.Now()
	for i := range rows {
		p := rows[i]
		cs.places[p.UserID] = &p
		cs.offlineSince[p.UserID] = now
		if _, ok := r.users[p.UserID]; !ok {
			missing = append(missing, p.UserID)
		}
	}
	for id, u := range m.loadUsers(missing) {
		r.users[id] = u
	}
	r.cinema = cs
}

// ── JSON ──────────────────────────────────────────────────────────────────

type CinemaInfoJSON struct {
	Enabled         bool `json:"enabled"`
	Seats           int  `json:"seats"`
	Rows            int  `json:"rows"`
	Cols            int  `json:"cols"`
	Couch           int  `json:"couch"`
	AllowSeatChange bool `json:"allow_seat_change"`
}

type CinemaPlaceJSON struct {
	User     *UserLite `json:"user"`
	UserID   int64     `json:"user_id"` // 0 = hidden (a block with the viewer)
	Kind     string    `json:"kind"`
	Index    int       `json:"index"`
	Code     string    `json:"code"`
	View     string    `json:"view"`
	Reserved bool      `json:"reserved"`
	Chosen   bool      `json:"chosen"`
	Notify   bool      `json:"notify"`
}

type CinemaOfferJSON struct {
	Index       int    `json:"index"`
	Code        string `json:"code"`
	ExpiresAtMs int64  `json:"expires_at_ms"`
}

type CinemaStateJSON struct {
	CinemaInfoJSON
	Rev    int64             `json:"rev"`
	Places []CinemaPlaceJSON `json:"places"`
	Queue  []int64           `json:"queue"` // waiting for a seat, first = next offer
	Held   []int             `json:"held"`  // seats held by an open offer
	// The viewer's own open queue offer (per-viewer answers only, never in
	// the shared `cinema_places` broadcast).
	MyOffer *CinemaOfferJSON `json:"my_offer,omitempty"`
}

func (cs *cinemaState) info() *CinemaInfoJSON {
	return &CinemaInfoJSON{
		Enabled: true, Seats: cs.layout.Seats, Rows: cs.layout.Rows, Cols: cs.layout.Cols,
		Couch: cs.layout.Couch, AllowSeatChange: cs.allowChange,
	}
}

// queueLocked — couch / no-place members asking for a seat, oldest first.
func (r *liveRoom) cinemaQueue() []int64 {
	cs := r.cinema
	type q struct {
		uid int64
		at  time.Time
	}
	var list []q
	for uid, p := range cs.places {
		if p.Kind == "seat" || !p.QueueNotify || r.member(uid) == nil {
			continue
		}
		at := time.Time{}
		if p.QueuedAt != nil {
			at = *p.QueuedAt
		}
		list = append(list, q{uid, at})
	}
	sort.Slice(list, func(i, j int) bool {
		if !list[i].at.Equal(list[j].at) {
			return list[i].at.Before(list[j].at)
		}
		return list[i].uid < list[j].uid
	})
	out := make([]int64, 0, len(list))
	for _, x := range list {
		out = append(out, x.uid)
	}
	return out
}

// cinemaJSONLocked — the hall for one viewer (people with a block either way
// are shown as an anonymous occupied place).
func (m *Module) cinemaJSONLocked(r *liveRoom, blocked map[int64]bool) *CinemaStateJSON {
	cs := r.cinema
	if cs == nil {
		return nil
	}
	out := &CinemaStateJSON{CinemaInfoJSON: *cs.info(), Rev: cs.rev, Places: []CinemaPlaceJSON{}, Queue: []int64{}, Held: []int{}}
	uids := make([]int64, 0, len(cs.places))
	for uid := range cs.places {
		uids = append(uids, uid)
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
	for _, uid := range uids {
		p := cs.places[uid]
		// Only joined members and reserved friends sit in the hall.
		if !p.Reserved && r.member(uid) == nil {
			continue
		}
		pj := CinemaPlaceJSON{Kind: p.Kind, Index: p.Idx, View: p.View, Reserved: p.Reserved,
			Chosen: p.Chosen, Notify: p.QueueNotify}
		if p.Kind == "seat" {
			pj.Code = cs.layout.code(p.Idx)
		}
		if !blocked[uid] {
			u := m.ensureUserLocked(r, uid)
			pj.User = &u
			pj.UserID = uid
		}
		out.Places = append(out.Places, pj)
	}
	for _, uid := range r.cinemaQueue() {
		if !blocked[uid] {
			out.Queue = append(out.Queue, uid)
		}
	}
	for idx := range cs.offers {
		out.Held = append(out.Held, idx)
	}
	sort.Ints(out.Held)
	return out
}

// cinemaForLocked — the hall for one viewer, with their own open offer.
func (m *Module) cinemaForLocked(r *liveRoom, uid int64, blocked map[int64]bool) *CinemaStateJSON {
	st := m.cinemaJSONLocked(r, blocked)
	if st == nil {
		return nil
	}
	if idx := m.offerOf(r.cinema, uid); idx >= 0 {
		o := r.cinema.offers[idx]
		st.MyOffer = &CinemaOfferJSON{Index: idx, Code: r.cinema.layout.code(idx), ExpiresAtMs: o.Until.UnixMilli()}
	}
	return st
}

// broadcastCinemaLocked — `cinema_places` to every socket (old apps ignore
// the type). One encoding for everyone without a block in the hall.
func (m *Module) broadcastCinemaLocked(r *liveRoom) {
	if r.cinema == nil {
		return
	}
	r.cinema.rev++
	if len(r.clients) == 0 {
		return
	}
	var shared []byte
	for _, c := range r.clients {
		needsOwn := false
		for uid := range r.cinema.places {
			if c.blocked[uid] {
				needsOwn = true
				break
			}
		}
		if needsOwn {
			if b := encodeFrame(r.id, "cinema_places", m.cinemaJSONLocked(r, c.blocked)); b != nil {
				c.enqueue(b)
			}
			continue
		}
		if shared == nil {
			shared = encodeFrame(r.id, "cinema_places", m.cinemaJSONLocked(r, nil))
			if shared == nil {
				return
			}
		}
		c.enqueue(shared)
	}
}

// ── Seating ───────────────────────────────────────────────────────────────

func (m *Module) bestFreeSeatLocked(r *liveRoom) int {
	cs := r.cinema
	taken := map[int]bool{}
	for _, p := range cs.places {
		if p.Kind == "seat" {
			taken[p.Idx] = true
		}
	}
	best, bestScore := -1, math.MaxFloat64
	for idx := 0; idx < cs.layout.Seats; idx++ {
		if taken[idx] || cs.offers[idx] != nil {
			continue
		}
		if s := cs.layout.score(idx); s < bestScore {
			best, bestScore = idx, s
		}
	}
	return best
}

func (m *Module) freeCouchLocked(r *liveRoom) int {
	cs := r.cinema
	taken := map[int]bool{}
	for _, p := range cs.places {
		if p.Kind == "couch" {
			taken[p.Idx] = true
		}
	}
	for idx := 0; idx < cs.layout.Couch; idx++ {
		if !taken[idx] {
			return idx
		}
	}
	return -1
}

// autoSeatLocked — a member joined: their reserved seat, else the best free
// seat, else a couch spot, else "none". An existing place is kept.
func (m *Module) autoSeatLocked(r *liveRoom, uid int64) {
	cs := r.cinema
	if cs == nil {
		return
	}
	if p := cs.places[uid]; p != nil {
		if p.Reserved {
			p.Reserved = false
			m.db.Exec(`UPDATE watch_room_seats SET reserved = false, updated_at = NOW() WHERE id = ?`, p.ID)
		}
		return
	}
	kind, idx := "seat", m.bestFreeSeatLocked(r)
	if idx < 0 {
		kind, idx = "couch", m.freeCouchLocked(r)
	}
	if idx < 0 {
		kind, idx = "none", 0
	}
	now := dbNow()
	row := seatRow{RoomID: r.id, UserID: uid, Kind: kind, Idx: idx, View: "hall", CreatedAt: now, UpdatedAt: now}
	if err := m.db.Create(&row).Error; err != nil {
		log.Printf("[watch] room %d seat %d: %v", r.id, uid, err)
		return
	}
	cs.places[uid] = &row
}

// releaseLocked — the member left / was kicked: the place goes back (a seat
// is offered to the queue first).
func (m *Module) releaseLocked(r *liveRoom, uid int64) {
	cs := r.cinema
	if cs == nil {
		return
	}
	m.dropSwapsLocked(r, uid)
	for idx, o := range cs.offers {
		if o.UserID == uid {
			delete(cs.offers, idx)
			m.offerSeatLocked(r, idx, o.Tried)
		}
	}
	p := cs.places[uid]
	if p == nil {
		return
	}
	if err := m.db.Exec(`DELETE FROM watch_room_seats WHERE id = ?`, p.ID).Error; err != nil {
		log.Printf("[watch] room %d free seat: %v", r.id, err)
		return
	}
	delete(cs.places, uid)
	delete(cs.offlineSince, uid)
	if p.Kind == "seat" {
		m.offerSeatLocked(r, p.Idx, nil)
	}
}

func (m *Module) cinemaJoinedLocked(r *liveRoom, uid int64) {
	if r.cinema == nil {
		return
	}
	m.autoSeatLocked(r, uid)
	m.broadcastCinemaLocked(r)
}

func (m *Module) cinemaLeftLocked(r *liveRoom, uid int64) {
	if r.cinema == nil {
		return
	}
	m.releaseLocked(r, uid)
	m.broadcastCinemaLocked(r)
}

// offerSeatLocked — a seat became free: hold it 30 s for the first person in
// the queue that is connected with a cinema app (old apps can't answer).
func (m *Module) offerSeatLocked(r *liveRoom, idx int, tried map[int64]bool) {
	cs := r.cinema
	if cs == nil || cs.holderOf("seat", idx) != 0 {
		return
	}
	if tried == nil {
		tried = map[int64]bool{}
	}
	for _, uid := range r.cinemaQueue() {
		if tried[uid] {
			continue
		}
		c := r.clients[uid]
		if c == nil || !c.cinema {
			continue
		}
		if waiting := m.offerOf(cs, uid); waiting >= 0 {
			continue // already holding another offer
		}
		o := &seatOffer{UserID: uid, Idx: idx, Until: time.Now().Add(cinemaOfferTTL), Tried: tried}
		o.Tried[uid] = true
		cs.offers[idx] = o
		r.sendTo(uid, "cinema_seat_offer", map[string]any{
			"index": idx, "code": cs.layout.code(idx), "expires_at_ms": o.Until.UnixMilli(),
		})
		return
	}
	// Nobody in the queue can take it: someone without a place whose app
	// doesn't know the hall (an old version) just gets it — they never see
	// the hall, the others see them sitting.
	for uid, p := range cs.places {
		if p.Kind != "none" || tried[uid] || r.member(uid) == nil {
			continue
		}
		if c := r.clients[uid]; c != nil && c.cinema {
			continue
		}
		_ = m.moveLocked(r, uid, "seat", idx, false)
		return
	}
}

// cinemaConnectedLocked — a socket opened: a member without a place gets one
// (old apps also when they have "none" and something is free now).
func (m *Module) cinemaConnectedLocked(r *liveRoom, uid int64, capable bool) {
	cs := r.cinema
	if cs == nil || r.member(uid) == nil {
		return
	}
	p := cs.places[uid]
	if p == nil {
		m.autoSeatLocked(r, uid)
		m.broadcastCinemaLocked(r)
		return
	}
	if p.Kind != "none" || capable {
		return
	}
	kind, idx := "seat", m.bestFreeSeatLocked(r)
	if idx < 0 {
		kind, idx = "couch", m.freeCouchLocked(r)
	}
	if idx < 0 {
		return
	}
	if m.moveLocked(r, uid, kind, idx, false) == nil {
		m.broadcastCinemaLocked(r)
	}
}

// dropSwapsLocked — uid's place changed or uid left: every open swap with them
// is void; both sides hear "expired" so no card stays on screen.
func (m *Module) dropSwapsLocked(r *liveRoom, uid int64) {
	cs := r.cinema
	for sid, s := range cs.swaps {
		if s.From != uid && s.To != uid {
			continue
		}
		delete(cs.swaps, sid)
		res := map[string]any{"id": s.ID, "status": "expired", "user_id": s.To}
		r.sendTo(s.From, "cinema_swap_result", res)
		r.sendTo(s.To, "cinema_swap_result", res)
	}
}

// releaseOfferOfLocked — uid got a seat another way: their open queue offer
// is given back and goes to the next person.
func (m *Module) releaseOfferOfLocked(r *liveRoom, uid int64) {
	cs := r.cinema
	idx := m.offerOf(cs, uid)
	if idx < 0 {
		return
	}
	o := cs.offers[idx]
	delete(cs.offers, idx)
	r.sendTo(uid, "cinema_seat_offer_expired", map[string]any{"index": idx})
	m.offerSeatLocked(r, idx, o.Tried)
}

func (m *Module) offerOf(cs *cinemaState, uid int64) int {
	for idx, o := range cs.offers {
		if o.UserID == uid {
			return idx
		}
	}
	return -1
}

// offerFreeSeatsLocked — free seats while people wait (a notify switched on,
// a reserved seat expired …).
func (m *Module) offerFreeSeatsLocked(r *liveRoom) {
	cs := r.cinema
	if cs == nil {
		return
	}
	taken := map[int]bool{}
	for _, p := range cs.places {
		if p.Kind == "seat" {
			taken[p.Idx] = true
		}
	}
	for idx := 0; idx < cs.layout.Seats; idx++ {
		if !taken[idx] && cs.offers[idx] == nil {
			m.offerSeatLocked(r, idx, nil)
		}
	}
}

// moveLocked — put uid at kind/idx (the target is free or held for uid).
func (m *Module) moveLocked(r *liveRoom, uid int64, kind string, idx int, chosen bool) error {
	cs := r.cinema
	p := cs.places[uid]
	now := dbNow()
	if p == nil {
		row := seatRow{RoomID: r.id, UserID: uid, Kind: kind, Idx: idx, View: "hall", Chosen: chosen,
			CreatedAt: now, UpdatedAt: now}
		if err := m.db.Create(&row).Error; err != nil {
			if isPgUniqueViolation(err) {
				return errSeatTaken
			}
			return errServer
		}
		cs.places[uid] = &row
		m.dropSwapsLocked(r, uid)
		if kind == "seat" {
			m.releaseOfferOfLocked(r, uid)
		}
		return nil
	}
	oldKind, oldIdx := p.Kind, p.Idx
	notify, queuedAt := p.QueueNotify, p.QueuedAt
	if kind == "seat" {
		notify, queuedAt = false, nil
	}
	if err := m.db.Exec(`UPDATE watch_room_seats SET kind = ?, idx = ?, chosen = ?, reserved = false,
			queue_notify = ?, queued_at = ?, updated_at = NOW() WHERE id = ?`,
		kind, idx, chosen, notify, queuedAt, p.ID).Error; err != nil {
		if isPgUniqueViolation(err) {
			return errSeatTaken
		}
		return errServer
	}
	p.Kind, p.Idx, p.Chosen, p.Reserved = kind, idx, chosen, false
	p.QueueNotify, p.QueuedAt = notify, queuedAt
	if oldKind != kind || oldIdx != idx {
		m.dropSwapsLocked(r, uid)
	}
	if kind == "seat" {
		m.releaseOfferOfLocked(r, uid)
	}
	if oldKind == "seat" && !(kind == "seat" && idx == oldIdx) {
		m.offerSeatLocked(r, oldIdx, nil)
	}
	return nil
}

// ── Create (from handleCreateRoom) ────────────────────────────────────────

type cinemaReserveReq struct {
	UserID int64 `json:"user_id"`
	Seat   int   `json:"seat"`
}

type cinemaCreateReq struct {
	Enabled         bool               `json:"enabled"`
	Seats           int                `json:"seats"`
	AllowSeatChange *bool              `json:"allow_seat_change"`
	Seat            *int               `json:"seat"`
	Reserve         []cinemaReserveReq `json:"reserve"`
}

// cinemaPlan — the validated create request (host seat + friends' seats).
type cinemaPlan struct {
	layout   cinemaLayout
	allow    bool
	hostSeat int
	reserve  []cinemaReserveReq
}

func (m *Module) planCinema(hostID int64, req *cinemaCreateReq) (*cinemaPlan, error) {
	if req == nil || !req.Enabled {
		return nil, nil
	}
	l := layoutFor(req.Seats)
	plan := &cinemaPlan{layout: l, allow: true}
	if req.AllowSeatChange != nil {
		plan.allow = *req.AllowSeatChange
	}
	best, bestScore := 0, math.MaxFloat64
	for idx := 0; idx < l.Seats; idx++ {
		if s := l.score(idx); s < bestScore {
			best, bestScore = idx, s
		}
	}
	plan.hostSeat = best
	if req.Seat != nil {
		if *req.Seat < 0 || *req.Seat >= l.Seats {
			return nil, errInvalid
		}
		plan.hostSeat = *req.Seat
	}
	if len(req.Reserve) == 0 {
		return plan, nil
	}
	ids := make([]int64, 0, len(req.Reserve))
	for _, x := range req.Reserve {
		ids = append(ids, x.UserID)
	}
	follows := m.followingAmong(hostID, ids)
	blocked := m.blockedWith(hostID)
	users := m.loadUserRows(ids)
	usedSeat := map[int]bool{plan.hostSeat: true}
	usedUser := map[int64]bool{hostID: true}
	for _, x := range req.Reserve {
		if len(plan.reserve) >= cinemaMaxReserve {
			break
		}
		if x.Seat < 0 || x.Seat >= l.Seats || usedSeat[x.Seat] || usedUser[x.UserID] {
			continue
		}
		u, ok := users[x.UserID]
		if !ok || u.Gone || u.IsGuest || !follows[x.UserID] || blocked[x.UserID] {
			continue
		}
		usedSeat[x.Seat] = true
		usedUser[x.UserID] = true
		plan.reserve = append(plan.reserve, x)
	}
	return plan, nil
}

// applyCinemaPlanTx — inside the create transaction: the host's seat and the
// friends' reserved seats.
func applyCinemaPlanTx(tx *gorm.DB, roomID, hostID int64, plan *cinemaPlan) error {
	if plan == nil {
		return nil
	}
	if err := tx.Exec(`INSERT INTO watch_room_seats (room_id, user_id, kind, idx, view, reserved, chosen, queue_notify, created_at, updated_at)
		VALUES (?, ?, 'seat', ?, 'hall', false, true, false, NOW(), NOW())`, roomID, hostID, plan.hostSeat).Error; err != nil {
		return err
	}
	for _, x := range plan.reserve {
		if err := tx.Exec(`INSERT INTO watch_room_seats (room_id, user_id, kind, idx, view, reserved, chosen, queue_notify, created_at, updated_at)
			VALUES (?, ?, 'seat', ?, 'hall', true, false, false, NOW(), NOW())`, roomID, x.UserID, x.Seat).Error; err != nil {
			return err
		}
	}
	return nil
}

// inviteReservedLocked — after the create: the friends with a kept seat are
// invited (pre-approved) and pushed, like `POST /invites` (scheduled rooms
// push them when the host opens the room).
func (m *Module) inviteReservedLocked(r *liveRoom, plan *cinemaPlan, fx *after) {
	if plan == nil || len(plan.reserve) == 0 {
		return
	}
	inviter := m.ensureUserLocked(r, r.row.HostID)
	roomName, roomID := r.row.Name, r.id
	for _, x := range plan.reserve {
		res := m.db.Exec(`INSERT INTO watch_room_invites (room_id, inviter_id, invitee_id, created_at, updated_at)
			VALUES (?, ?, ?, NOW(), NOW()) ON CONFLICT (room_id, invitee_id) DO NOTHING`, r.id, r.row.HostID, x.UserID)
		if res.Error != nil || res.RowsAffected == 0 || r.row.Status == "scheduled" {
			continue
		}
		target := x.UserID
		fx.add(func() {
			m.push(target, "watch_invite", map[string]string{"host": inviter.Username, "room": roomName},
				map[string]string{"room_id": itoa(roomID)})
		})
	}
}

// ── REST ──────────────────────────────────────────────────────────────────

// cinemaRoom — common checks: member of a non-ended cinema room.
func (m *Module) cinemaRoom(r *liveRoom, uid int64) error {
	if r.member(uid) == nil {
		return errNotMember
	}
	if r.row.Status == "ended" {
		return errEnded
	}
	if r.cinema == nil {
		return errCinemaOff
	}
	return nil
}

func (m *Module) handleCinema(c *gin.Context) {
	uid := currentUserID(c)
	id, ok := paramID(c, "id")
	if !ok {
		fail(c, errNotFound)
		return
	}
	blocked := m.blockedWith(uid)
	var out *CinemaStateJSON
	err := m.withRoom(id, func(r *liveRoom) error {
		if r.member(uid) == nil {
			return errNotMember
		}
		if r.cinema == nil {
			return errCinemaOff
		}
		out = m.cinemaForLocked(r, uid, blocked)
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, out)
}

type cinemaSeatReq struct {
	Kind  string `json:"kind"`
	Index int    `json:"index"`
}

// handleCinemaSeat — POST /rooms/:id/cinema/seat {kind: seat|couch, index}:
// sit there (free, or held for me by a queue offer).
func (m *Module) handleCinemaSeat(c *gin.Context) {
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
	var req cinemaSeatReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, errInvalid)
		return
	}
	kind := strings.TrimSpace(req.Kind)
	if kind == "" {
		kind = "seat"
	}
	if !m.limiter.allow("seat:"+itoa(v.ID), 1, 4) {
		fail(c, errRateLimited)
		return
	}
	blocked := m.blockedWith(v.ID)
	var out *CinemaStateJSON
	err = m.withRoom(id, func(r *liveRoom) error {
		if err := m.cinemaRoom(r, v.ID); err != nil {
			return err
		}
		cs := r.cinema
		switch kind {
		case "seat":
			if req.Index < 0 || req.Index >= cs.layout.Seats {
				return errInvalid
			}
		case "couch":
			if req.Index < 0 || req.Index >= cs.layout.Couch {
				return errInvalid
			}
		default:
			return errInvalid
		}
		p := cs.places[v.ID]
		if p != nil && p.Kind == kind && p.Idx == req.Index {
			if !p.Chosen {
				p.Chosen = true
				m.db.Exec(`UPDATE watch_room_seats SET chosen = true, updated_at = NOW() WHERE id = ?`, p.ID)
			}
			out = m.cinemaForLocked(r, v.ID, blocked)
			return nil
		}
		// "Yerini dəyişmək olar" off: a picked seat stays (the host may always
		// move; leaving a couch / no place for a seat is always fine).
		if p != nil && p.Kind == "seat" && p.Chosen && !cs.allowChange && !r.isHost(v.ID) {
			return errSeatChangeOff
		}
		if holder := cs.holderOf(kind, req.Index); holder != 0 && holder != v.ID {
			return errSeatTaken
		}
		if kind == "seat" {
			if o := cs.offers[req.Index]; o != nil {
				if o.UserID != v.ID {
					return errSeatTaken
				}
				delete(cs.offers, req.Index)
			}
		}
		if err := m.moveLocked(r, v.ID, kind, req.Index, true); err != nil {
			return err
		}
		m.broadcastCinemaLocked(r)
		out = m.cinemaForLocked(r, v.ID, blocked)
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, out)
}

type cinemaViewReq struct {
	View string `json:"view"`
}

// handleCinemaView — POST /rooms/:id/cinema/view {view: hall|full}: the plain
// player instead of the hall; the place is kept and shown to everyone.
func (m *Module) handleCinemaView(c *gin.Context) {
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
	var req cinemaViewReq
	if err := c.ShouldBindJSON(&req); err != nil || (req.View != "hall" && req.View != "full") {
		fail(c, errInvalid)
		return
	}
	if !m.limiter.allow("seatview:"+itoa(v.ID), 1, 5) {
		fail(c, errRateLimited)
		return
	}
	err = m.withRoom(id, func(r *liveRoom) error {
		if err := m.cinemaRoom(r, v.ID); err != nil {
			return err
		}
		p := r.cinema.places[v.ID]
		if p == nil {
			m.autoSeatLocked(r, v.ID)
			p = r.cinema.places[v.ID]
			if p == nil {
				return errServer
			}
		}
		if p.View == req.View {
			return nil
		}
		if err := m.db.Exec(`UPDATE watch_room_seats SET view = ?, updated_at = NOW() WHERE id = ?`, req.View, p.ID).Error; err != nil {
			return errServer
		}
		p.View = req.View
		m.broadcastCinemaLocked(r)
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, gin.H{"ok": true})
}

type cinemaNotifyReq struct {
	On bool `json:"on"`
}

// handleCinemaNotify — POST /rooms/:id/cinema/notify {on}: "Yer boşalanda
// xəbər ver" (couch / no place only). On = into the queue (a free seat is
// offered at once).
func (m *Module) handleCinemaNotify(c *gin.Context) {
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
	var req cinemaNotifyReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, errInvalid)
		return
	}
	blocked := m.blockedWith(v.ID)
	var out *CinemaStateJSON
	err = m.withRoom(id, func(r *liveRoom) error {
		if err := m.cinemaRoom(r, v.ID); err != nil {
			return err
		}
		p := r.cinema.places[v.ID]
		if p == nil {
			m.autoSeatLocked(r, v.ID)
			p = r.cinema.places[v.ID]
			if p == nil {
				return errServer
			}
		}
		if p.Kind == "seat" {
			out = m.cinemaForLocked(r, v.ID, blocked)
			return nil
		}
		if p.QueueNotify != req.On {
			var at *time.Time
			if req.On {
				t := dbNow()
				at = &t
			}
			if err := m.db.Exec(`UPDATE watch_room_seats SET queue_notify = ?, queued_at = ?, updated_at = NOW() WHERE id = ?`,
				req.On, at, p.ID).Error; err != nil {
				return errServer
			}
			p.QueueNotify, p.QueuedAt = req.On, at
			if !req.On {
				if idx := m.offerOf(r.cinema, v.ID); idx >= 0 {
					o := r.cinema.offers[idx]
					delete(r.cinema.offers, idx)
					m.offerSeatLocked(r, idx, o.Tried)
				}
			} else {
				m.offerFreeSeatsLocked(r)
			}
			m.broadcastCinemaLocked(r)
		}
		out = m.cinemaForLocked(r, v.ID, blocked)
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, out)
}

type cinemaDeclineReq struct {
	Index int `json:"index"`
}

// handleCinemaOfferDecline — POST /rooms/:id/cinema/offer/decline {index}:
// "no thanks" to a queue offer → the next person in the queue.
func (m *Module) handleCinemaOfferDecline(c *gin.Context) {
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
	var req cinemaDeclineReq
	_ = c.ShouldBindJSON(&req)
	err = m.withRoom(id, func(r *liveRoom) error {
		if err := m.cinemaRoom(r, v.ID); err != nil {
			return err
		}
		o := r.cinema.offers[req.Index]
		if o == nil || o.UserID != v.ID {
			return errNoOffer
		}
		delete(r.cinema.offers, req.Index)
		m.offerSeatLocked(r, req.Index, o.Tried)
		m.broadcastCinemaLocked(r)
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, gin.H{"ok": true})
}

type cinemaSwapReq struct {
	Index int `json:"index"`
}

func newOfferID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return itoa(time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// handleCinemaSwap — POST /rooms/:id/cinema/swap {index}: ask the person on
// that seat to change places (they get `cinema_swap_offer`, 30 s).
func (m *Module) handleCinemaSwap(c *gin.Context) {
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
	var req cinemaSwapReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, errInvalid)
		return
	}
	if !m.limiter.allow("seatswap:"+itoa(v.ID), 0.2, 3) {
		fail(c, errRateLimited)
		return
	}
	var offer *swapOffer
	err = m.withRoom(id, func(r *liveRoom) error {
		if err := m.cinemaRoom(r, v.ID); err != nil {
			return err
		}
		cs := r.cinema
		if req.Index < 0 || req.Index >= cs.layout.Seats {
			return errInvalid
		}
		me := cs.places[v.ID]
		if me != nil && me.Kind == "seat" && me.Chosen && !cs.allowChange && !r.isHost(v.ID) {
			return errSeatChangeOff
		}
		target := cs.holderOf("seat", req.Index)
		if target == v.ID {
			return errInvalid
		}
		if target == 0 {
			return errSwapUnavail // the seat just became free
		}
		tp := cs.places[target]
		if tp == nil || tp.Reserved || r.member(target) == nil {
			return errSwapUnavail
		}
		if cl := r.clients[target]; cl == nil || !cl.cinema || cl.blocked[v.ID] {
			return errSwapUnavail
		}
		for sid, s := range cs.swaps {
			if s.From == v.ID {
				// A new ask replaces my old one: its card goes away on the other side.
				delete(cs.swaps, sid)
				r.sendTo(s.To, "cinema_swap_result", map[string]any{"id": s.ID, "status": "expired", "user_id": s.To})
			}
		}
		offer = &swapOffer{ID: newOfferID(), From: v.ID, To: target, ToIdx: req.Index, Until: time.Now().Add(cinemaSwapTTL)}
		cs.swaps[offer.ID] = offer
		fromKind, fromIdx, fromCode := "none", 0, ""
		if me != nil {
			fromKind, fromIdx = me.Kind, me.Idx
			if me.Kind == "seat" {
				fromCode = cs.layout.code(me.Idx)
			}
		}
		r.sendTo(target, "cinema_swap_offer", map[string]any{
			"id": offer.ID, "from": m.ensureUserLocked(r, v.ID),
			"from_kind": fromKind, "from_index": fromIdx, "from_code": fromCode,
			"to_index": req.Index, "to_code": cs.layout.code(req.Index),
			"expires_at_ms": offer.Until.UnixMilli(),
		})
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, gin.H{"id": offer.ID, "expires_at_ms": offer.Until.UnixMilli()})
}

func (m *Module) handleCinemaSwapAccept(c *gin.Context)  { m.decideSwap(c, true) }
func (m *Module) handleCinemaSwapDecline(c *gin.Context) { m.decideSwap(c, false) }

// decideSwap — the asked person answers. Accept = the two places change hands
// (both still where they were when asked).
func (m *Module) decideSwap(c *gin.Context, accept bool) {
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
	sid := c.Param("sid")
	blocked := m.blockedWith(v.ID)
	var out *CinemaStateJSON
	err = m.withRoom(id, func(r *liveRoom) error {
		if err := m.cinemaRoom(r, v.ID); err != nil {
			return err
		}
		cs := r.cinema
		s := cs.swaps[sid]
		if s == nil || s.To != v.ID || time.Now().After(s.Until) {
			return errNoOffer
		}
		delete(cs.swaps, sid)
		if !accept {
			r.sendTo(s.From, "cinema_swap_result", map[string]any{"id": s.ID, "status": "declined", "user_id": v.ID})
			out = m.cinemaForLocked(r, v.ID, blocked)
			return nil
		}
		if r.member(s.From) == nil {
			return errNoOffer
		}
		mine := cs.places[v.ID]
		if mine == nil || mine.Kind != "seat" || mine.Idx != s.ToIdx {
			return errNoOffer
		}
		if err := m.swapPlacesLocked(r, s.From, v.ID); err != nil {
			return err
		}
		r.sendTo(s.From, "cinema_swap_result", map[string]any{"id": s.ID, "status": "accepted", "user_id": v.ID})
		m.broadcastCinemaLocked(r)
		out = m.cinemaForLocked(r, v.ID, blocked)
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, out)
}

// swapPlacesLocked — a (asker, may have no place) ↔ b (on a seat), in one
// transaction (the partial unique index needs a parked step).
func (m *Module) swapPlacesLocked(r *liveRoom, a, b int64) error {
	cs := r.cinema
	pb := cs.places[b]
	pa := cs.places[a]
	if pa == nil {
		m.autoSeatLocked(r, a)
		pa = cs.places[a]
		if pa == nil {
			return errServer
		}
	}
	aKind, aIdx := pa.Kind, pa.Idx
	bKind, bIdx := pb.Kind, pb.Idx
	tx := m.db.Begin()
	if err := tx.Exec(`UPDATE watch_room_seats SET kind = 'none', idx = 0 WHERE id = ?`, pa.ID).Error; err != nil {
		tx.Rollback()
		return errServer
	}
	if err := tx.Exec(`UPDATE watch_room_seats SET kind = ?, idx = ?, chosen = true, updated_at = NOW() WHERE id = ?`,
		aKind, aIdx, pb.ID).Error; err != nil {
		tx.Rollback()
		return errServer
	}
	if err := tx.Exec(`UPDATE watch_room_seats SET kind = ?, idx = ?, chosen = true, queue_notify = false,
			queued_at = NULL, updated_at = NOW() WHERE id = ?`, bKind, bIdx, pa.ID).Error; err != nil {
		tx.Rollback()
		return errServer
	}
	if err := tx.Commit().Error; err != nil {
		return errServer
	}
	pa.Kind, pa.Idx, pa.Chosen, pa.QueueNotify, pa.QueuedAt = bKind, bIdx, true, false, nil
	pb.Kind, pb.Idx, pb.Chosen = aKind, aIdx, true
	// Both moved: other open swaps with them are void, and a's queue offer
	// (a now has a seat) goes to the next person.
	m.dropSwapsLocked(r, a)
	m.dropSwapsLocked(r, b)
	m.releaseOfferOfLocked(r, a)
	return nil
}

// ── Socket presence + tick ────────────────────────────────────────────────

func (r *liveRoom) cinemaOnline(uid int64, online bool) {
	if r.cinema == nil {
		return
	}
	if online {
		delete(r.cinema.offlineSince, uid)
	} else {
		r.cinema.offlineSince[uid] = time.Now()
	}
}

// cinemaTickLocked — every second for loaded cinema rooms: expired offers move
// on, swaps time out, reserved seats are freed 10 minutes into the film, and
// (every 30 s, only while someone waits for a seat) a seat of a member
// offline for 10 minutes goes to the queue.
func (m *Module) cinemaTickLocked(r *liveRoom, now time.Time) {
	cs := r.cinema
	changed := false
	for idx, o := range cs.offers {
		if now.After(o.Until) {
			delete(cs.offers, idx)
			r.sendTo(o.UserID, "cinema_seat_offer_expired", map[string]any{"index": idx})
			m.offerSeatLocked(r, idx, o.Tried)
			changed = true
		}
	}
	for sid, s := range cs.swaps {
		if now.After(s.Until) {
			delete(cs.swaps, sid)
			r.sendTo(s.From, "cinema_swap_result", map[string]any{"id": s.ID, "status": "expired", "user_id": s.To})
			r.sendTo(s.To, "cinema_swap_result", map[string]any{"id": s.ID, "status": "expired", "user_id": s.To})
		}
	}
	if now.Sub(cs.lastSweep) >= 30*time.Second {
		cs.lastSweep = now
		if r.row.Status == "live" && r.row.StartedAt != nil && now.Sub(*r.row.StartedAt) > cinemaReserveGrace {
			for uid, p := range cs.places {
				if !p.Reserved {
					continue
				}
				if m.db.Exec(`DELETE FROM watch_room_seats WHERE id = ?`, p.ID).Error == nil {
					delete(cs.places, uid)
					changed = true
				}
			}
			if changed {
				m.offerFreeSeatsLocked(r)
			}
		}
		if m.reclaimOfflineSeatsLocked(r, now) {
			changed = true
		}
	}
	if changed {
		m.broadcastCinemaLocked(r)
	}
}

// reclaimOfflineSeatsLocked — the hall is full and connected people wait: the
// seat of a member offline for 10 minutes is freed (they keep the membership
// and get a place again when they come back).
func (m *Module) reclaimOfflineSeatsLocked(r *liveRoom, now time.Time) bool {
	cs := r.cinema
	waiting := 0
	for _, uid := range r.cinemaQueue() {
		if c := r.clients[uid]; c != nil && c.cinema {
			waiting++
		}
	}
	if waiting == 0 || m.bestFreeSeatLocked(r) >= 0 {
		return false
	}
	changed := false
	for uid, p := range cs.places {
		if waiting == 0 {
			break
		}
		if p.Kind != "seat" || p.Reserved || r.clients[uid] != nil {
			continue
		}
		since, ok := cs.offlineSince[uid]
		if !ok || now.Sub(since) < cinemaReclaimAfter {
			continue
		}
		idx := p.Idx
		if m.db.Exec(`UPDATE watch_room_seats SET kind = 'none', idx = 0, chosen = false, updated_at = NOW() WHERE id = ?`, p.ID).Error != nil {
			continue
		}
		p.Kind, p.Idx, p.Chosen = "none", 0, false
		m.offerSeatLocked(r, idx, nil)
		waiting--
		changed = true
	}
	return changed
}

// cinemaSnapshotJSON — raw JSON for the `state` frame (nil = not a hall).
func (m *Module) cinemaSnapshotLocked(r *liveRoom, uid int64) any {
	if r.cinema == nil {
		return nil
	}
	var blocked map[int64]bool
	if c := r.clients[uid]; c != nil {
		blocked = c.blocked
	}
	// The viewer's own open offer is included (a reconnect must not lose it).
	return m.cinemaForLocked(r, uid, blocked)
}

// handleCinemaFriends — GET /cinema/friends?q=: who the host can keep a seat
// for while creating a hall (no room yet). Same people as invite-candidates
// (accepted follows, mutual first, newest follow first), without the room.
func (m *Module) handleCinemaFriends(c *gin.Context) {
	uid := currentUserID(c)
	q := foldSearch(c.Query("q"))
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
	hidden := m.hiddenFor(uid, ids)
	users := m.loadUsers(ids)
	out := make([]UserLite, 0, len(ids))
	for _, id := range ids {
		if blocked[id] || hidden[id] || id == uid {
			continue
		}
		if u, ok := users[id]; ok {
			out = append(out, u)
		}
		if len(out) >= 50 {
			break
		}
	}
	respond(c, gin.H{"users": out})
}
