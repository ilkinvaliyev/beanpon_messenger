package watch

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

// apiError — handler error with an HTTP status + UPPER_SNAKE code.
type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string { return e.Code }

func errf(status int, code, msg string) *apiError {
	return &apiError{Status: status, Code: code, Message: msg}
}

var (
	errNotFound      = errf(http.StatusNotFound, "ROOM_NOT_FOUND", "Otaq tapılmadı")
	errNotMember     = errf(http.StatusForbidden, "NOT_MEMBER", "Bu otağın üzvü deyilsən")
	errNotHost       = errf(http.StatusForbidden, "NOT_HOST", "Bunu yalnız aparıcı edə bilər")
	errNotController = errf(http.StatusForbidden, "NOT_CONTROLLER", "Bunu yalnız aparıcı və köməkçilər edə bilər")
	errEnded         = errf(http.StatusGone, "ROOM_ENDED", "Otaq bitib")
	errDisabled      = errf(http.StatusForbidden, "WATCH_DISABLED", "Birlikdə izlə müvəqqəti bağlıdır")
	errGuest         = errf(http.StatusForbidden, "GUEST_FORBIDDEN", "Davam etmək üçün daxil ol")
	errInvalid       = errf(http.StatusUnprocessableEntity, "INVALID_INPUT", "Məlumat düzgün deyil")
	errRateLimited   = errf(http.StatusTooManyRequests, "RATE_LIMITED", "Çox tez-tez")
	errServer        = errf(http.StatusInternalServerError, "SERVER_ERROR", "Xəta baş verdi")
)

func respond(c *gin.Context, data any) {
	c.JSON(http.StatusOK, gin.H{"data": data})
}

func fail(c *gin.Context, err error) {
	var ae *apiError
	if errors.As(err, &ae) {
		c.JSON(ae.Status, gin.H{"error": ae.Message, "code": ae.Code})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Xəta baş verdi", "code": "SERVER_ERROR"})
}

// currentUserID — the JWT middleware stores user_id as uint.
func currentUserID(c *gin.Context) int64 {
	v, ok := c.Get("user_id")
	if !ok {
		return 0
	}
	switch t := v.(type) {
	case uint:
		return int64(t)
	case int64:
		return t
	case int:
		return int64(t)
	case float64:
		return int64(t)
	}
	return 0
}

func paramID(c *gin.Context, name string) (int64, bool) {
	v, err := strconv.ParseInt(strings.TrimSpace(c.Param(name)), 10, 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

func queryInt(c *gin.Context, name string, def, min, max int) int {
	v, err := strconv.Atoi(strings.TrimSpace(c.Query(name)))
	if err != nil {
		return def
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func nowMs() int64 { return time.Now().UnixMilli() }

func strPtr(s string) *string { return &s }

func int64Ptr(v int64) *int64 { return &v }

func ptrStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func nonEmptyPtr(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return &s
}

// clampRunes trims s to at most n runes.
func clampRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

// codeAlphabet — no 0/O, 1/I/L (read aloud / typed from a screen).
const codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

func newRoomCode() string {
	b := make([]byte, 6)
	max := big.NewInt(int64(len(codeAlphabet)))
	for i := range b {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			b[i] = codeAlphabet[time.Now().UnixNano()%int64(len(codeAlphabet))]
			continue
		}
		b[i] = codeAlphabet[n.Int64()]
	}
	return string(b)
}

// normalizeCode — accepts "m8k 4tq", "M8K·4TQ", "m8k-4tq".
func normalizeCode(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(raw) {
		if strings.ContainsRune(codeAlphabet, r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// foldSearch — the same folding as Laravel WatchTitle::normalizeSearch:
// every i variant → "i", then lower case.
func foldSearch(s string) string {
	s = strings.NewReplacer("İ", "i", "I", "i", "ı", "i").Replace(s)
	return strings.ToLower(strings.TrimSpace(s))
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func parseJSONStrings(raw *string) []string {
	out := []string{}
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return out
	}
	_ = json.Unmarshal([]byte(*raw), &out)
	return out
}

func isPgUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	type sqlState interface{ SQLState() string }
	var s sqlState
	if errors.As(err, &s) {
		return s.SQLState() == "23505"
	}
	return strings.Contains(err.Error(), "23505") || strings.Contains(strings.ToLower(err.Error()), "duplicate key")
}

// rateLimiter — keyed token buckets for module-level limits (code lookups,
// searches, invites). In memory, per instance.
type rateLimiter struct {
	mu sync.Mutex
	m  map[string]*tokenBucket
}

func newRateLimiter() *rateLimiter { return &rateLimiter{m: map[string]*tokenBucket{}} }

func (l *rateLimiter) allow(key string, ratePerSec, burst float64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.m) > 50000 {
		l.m = map[string]*tokenBucket{}
	}
	b := l.m[key]
	if b == nil {
		b = &tokenBucket{}
		l.m[key] = b
	}
	return b.allow(time.Now(), ratePerSec, burst)
}

// peek — would allow() pass now? Refills the bucket but takes no token
// (used to refuse early and charge only failures, e.g. wrong room codes).
func (l *rateLimiter) peek(key string, ratePerSec, burst float64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.m[key]
	if b == nil {
		return true
	}
	now := time.Now()
	b.tokens += now.Sub(b.last).Seconds() * ratePerSec
	if b.tokens > burst {
		b.tokens = burst
	}
	b.last = now
	return b.tokens >= 1
}

// tokenBucket — tiny per-user rate limiter (in memory, guarded by the room lock).
type tokenBucket struct {
	tokens float64
	last   time.Time
}

func (b *tokenBucket) allow(now time.Time, ratePerSec, burst float64) bool {
	if b.last.IsZero() {
		b.tokens = burst
		b.last = now
	}
	elapsed := now.Sub(b.last).Seconds()
	b.last = now
	b.tokens += elapsed * ratePerSec
	if b.tokens > burst {
		b.tokens = burst
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
