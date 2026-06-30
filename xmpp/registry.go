package xmpp

import (
	"sync"
	"time"
)

// registry.go — the capability registry.
//
// This is the single source of truth for backward compatibility: for any
// user_id, is that user currently reachable over XMPP (NEW client) or only
// over the legacy WebSocket (OLD client)?
//
// How a user becomes "NEW":
//   - When an XMPP-capable client authenticates against ejabberd, ejabberd
//     calls back to POST /internal/xmpp/auth (handled in Go). At that point
//     the Go service marks the user XMPP-capable via MarkXMPP.
//   - Equivalently, the component receives <presence> for that user's JID and
//     marks them online-on-XMPP.
//
// How a user becomes "OLD":
//   - They connect to the legacy /ws endpoint. websocket/hub.registerClient
//     calls MarkLegacy.
//
// A user can flip between the two across reconnects (e.g. after an app
// update). The most recent observation wins. Presence is also TTL-guarded so a
// silently dropped XMPP session eventually falls back to legacy/offline.

// Capability describes how a user is currently reachable.
type Capability int

const (
	CapUnknown Capability = iota // not seen recently → treat as offline/legacy
	CapLegacy                    // connected via legacy WebSocket (/ws)
	CapXMPP                      // connected via an XMPP client
)

type entry struct {
	cap      Capability
	resource string // last seen device resource (for Carbons / direct routing)
	seenAt   time.Time
}

// Registry is safe for concurrent use.
type Registry struct {
	mu  sync.RWMutex
	m   map[uint]entry
	ttl time.Duration
}

// NewRegistry creates a registry. ttl bounds how long an XMPP presence is
// trusted without refresh; 0 disables expiry.
func NewRegistry(ttl time.Duration) *Registry {
	return &Registry{m: make(map[uint]entry), ttl: ttl}
}

// MarkXMPP records that userID is reachable over XMPP (optionally with the
// device resource that connected).
func (r *Registry) MarkXMPP(userID uint, resource string) {
	r.mu.Lock()
	r.m[userID] = entry{cap: CapXMPP, resource: resource, seenAt: time.Now()}
	r.mu.Unlock()
}

// MarkLegacy records that userID is on a legacy WebSocket client.
func (r *Registry) MarkLegacy(userID uint) {
	r.mu.Lock()
	r.m[userID] = entry{cap: CapLegacy, seenAt: time.Now()}
	r.mu.Unlock()
}

// Forget removes a user (called on disconnect).
func (r *Registry) Forget(userID uint) {
	r.mu.Lock()
	delete(r.m, userID)
	r.mu.Unlock()
}

// Of returns the current capability of a user, applying TTL expiry.
func (r *Registry) Of(userID uint) Capability {
	r.mu.RLock()
	e, ok := r.m[userID]
	ttl := r.ttl
	r.mu.RUnlock()
	if !ok {
		return CapUnknown
	}
	if ttl > 0 && time.Since(e.seenAt) > ttl {
		return CapUnknown
	}
	return e.cap
}

// IsXMPP is a convenience predicate used by the Router.
func (r *Registry) IsXMPP(userID uint) bool { return r.Of(userID) == CapXMPP }

// Resource returns the last seen device resource for a user, if any.
func (r *Registry) Resource(userID uint) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.m[userID].resource
}
