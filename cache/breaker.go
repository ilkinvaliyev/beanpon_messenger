package cache

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// ErrCircuitOpen — circuit breaker açıq olanda hər çağırış bu xətanı qaytarır.
// Çağıran kod bunu görəndə Redis-i atlayır və birbaşa DB-yə düşür.
var ErrCircuitOpen = errors.New("cache: circuit breaker open")

type breakerState int32

const (
	stateClosed   breakerState = 0
	stateOpen     breakerState = 1
	stateHalfOpen breakerState = 2
)

// Breaker — sadə sliding-window circuit breaker.
//
// Threshold qədər xəta 1 dəqiqə içində baş verərsə "open" vəziyyətinə keçir.
// Open vəziyyətində bütün çağırışlar dərhal ErrCircuitOpen qaytarır —
// Redis-ə getmir. Cooldown bitdikdən sonra "half-open"-da bir trial buraxır:
// uğurludursa "closed"-a, uğursuzdursa yenidən "open"-a.
type Breaker struct {
	threshold int
	cooldown  time.Duration

	state atomic.Int32

	mu     sync.Mutex
	errors []time.Time

	openedAtNano atomic.Int64
}

// NewBreaker — threshold=0 və ya cooldown=0 olduqda breaker effektiv
// olaraq söndürülür (yalnız Closed olur).
func NewBreaker(threshold int, cooldown time.Duration) *Breaker {
	return &Breaker{
		threshold: threshold,
		cooldown:  cooldown,
		errors:    make([]time.Time, 0, threshold+1),
	}
}

// Allow — Redis çağırışından əvvəl çağırılır. False qaytararsa
// çağıran metod dərhal cache-i bypass etməlidir.
func (b *Breaker) Allow() bool {
	if b.threshold <= 0 {
		return true
	}
	st := breakerState(b.state.Load())
	switch st {
	case stateClosed:
		return true
	case stateOpen:
		openedAt := time.Unix(0, b.openedAtNano.Load())
		if time.Since(openedAt) >= b.cooldown {
			if b.state.CompareAndSwap(int32(stateOpen), int32(stateHalfOpen)) {
				return true
			}
		}
		return false
	case stateHalfOpen:
		return false
	}
	return true
}

// Success — Redis çağırışı uğurlu olduqda.
func (b *Breaker) Success() {
	if b.threshold <= 0 {
		return
	}
	if breakerState(b.state.Load()) == stateHalfOpen {
		b.state.Store(int32(stateClosed))
		b.mu.Lock()
		b.errors = b.errors[:0]
		b.mu.Unlock()
	}
}

// Failure — Redis çağırışı uğursuz olduqda.
func (b *Breaker) Failure() {
	if b.threshold <= 0 {
		return
	}
	if breakerState(b.state.Load()) == stateHalfOpen {
		b.openedAtNano.Store(time.Now().UnixNano())
		b.state.Store(int32(stateOpen))
		return
	}

	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()

	cutoff := now.Add(-time.Minute)
	i := 0
	for i < len(b.errors) && b.errors[i].Before(cutoff) {
		i++
	}
	b.errors = b.errors[i:]
	b.errors = append(b.errors, now)

	if len(b.errors) >= b.threshold {
		b.openedAtNano.Store(now.UnixNano())
		b.state.Store(int32(stateOpen))
	}
}

// State — debug və health endpoint üçün.
func (b *Breaker) State() string {
	switch breakerState(b.state.Load()) {
	case stateClosed:
		return "closed"
	case stateOpen:
		return "open"
	case stateHalfOpen:
		return "half-open"
	}
	return "unknown"
}
