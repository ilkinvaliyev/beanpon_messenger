package watch

import (
	"regexp"
	"testing"
)

func TestExpectedClock(t *testing.T) {
	dur := int64(60_000)
	r := &liveRoom{row: roomRow{PositionMs: 10_000, IsPlaying: true, PlaybackRate: 1, PositionUpdatedMs: 1_000, MediaDurationMs: &dur}}
	if got := r.expected(3_000); got != 12_000 {
		t.Fatalf("playing: got %d want 12000", got)
	}
	// Countdown: updated_at in the future → the clock holds.
	r.row.PositionUpdatedMs = 5_000
	if got := r.expected(3_000); got != 10_000 {
		t.Fatalf("countdown: got %d want 10000", got)
	}
	// Rate 2x.
	r.row.PositionUpdatedMs = 0
	r.row.PlaybackRate = 2
	if got := r.expected(1_000); got != 12_000 {
		t.Fatalf("rate: got %d want 12000", got)
	}
	// Clamped at the duration.
	if got := r.expected(1_000_000); got != dur {
		t.Fatalf("clamp: got %d want %d", got, dur)
	}
	// Paused never moves.
	r.row.IsPlaying = false
	if got := r.expected(1_000_000); got != 10_000 {
		t.Fatalf("paused: got %d want 10000", got)
	}
}

func TestParseISODuration(t *testing.T) {
	cases := map[string]int64{
		"PT1H2M10S": 3_730_000,
		"PT45S":     45_000,
		"PT0S":      0,
		"P1DT1H":    90_000_000,
		"bogus":     0,
		"":          0,
	}
	for in, want := range cases {
		if got := parseISODuration(in); got != want {
			t.Errorf("%q: got %d want %d", in, got, want)
		}
	}
}

func TestNormalizeCode(t *testing.T) {
	for in, want := range map[string]string{
		"m8k 4tq":   "M8K4TQ",
		"M8K · 4TQ": "M8K4TQ",
		"m8k-4tq":   "M8K4TQ",
		"O0I1L":     "", // ambiguous characters are not part of the alphabet
	} {
		if got := normalizeCode(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
	for i := 0; i < 200; i++ {
		c := newRoomCode()
		if len(c) != 6 || normalizeCode(c) != c {
			t.Fatalf("bad code %q", c)
		}
	}
}

func TestValidEmoji(t *testing.T) {
	for _, ok := range []string{"🔥", "😂", "👍🏽", "❤️", "👨‍👩‍👧", "🇦🇿", "#️⃣", "1️⃣", "⭐", "👩🏻‍❤️‍💋‍👨🏼", "🏴󠁧󠁢󠁳󠁣󠁴󠁿"} {
		if !validEmoji(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "a", "hi", "1", "#", "🔥 x", "ая", "<script>", "→x", "‍", "️", "一", "∑"} {
		if validEmoji(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestValidMediaURL(t *testing.T) {
	for _, ok := range []string{
		"https://api.beanpon.com/api/s3-storage/watch/voice/a.m4a",
		"https://media2.giphy.com/media/abc/giphy.gif",
		"https://media.tenor.com/x/y.gif",
		"https://cdn.piokio.app/a.png",
	} {
		if !validMediaURL(ok) {
			t.Errorf("%q should be allowed", ok)
		}
	}
	for _, bad := range []string{
		"http://api.beanpon.com/a.png",                // not https
		"https://evil.com/a.png",                      // foreign host
		"https://beanpon.com.evil.com/a.png",          // suffix trick
		"https://api.beanpon.com@evil.com/a.png",      // userinfo trick
		"https://notgiphy.com/a.gif",                  // look-alike
		"javascript:alert(1)",                         // scheme
		"https://" + string(make([]byte, 1001)) + "x", // too long
	} {
		if validMediaURL(bad) {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestRateLimiterPeek(t *testing.T) {
	l := newRateLimiter()
	if !l.peek("k", 0.0001, 2) {
		t.Fatal("unknown key must pass")
	}
	l.allow("k", 0.0001, 2)
	l.allow("k", 0.0001, 2)
	if l.peek("k", 0.0001, 2) {
		t.Fatal("spent bucket must refuse")
	}
	// peek never takes a token.
	l2 := newRateLimiter()
	l2.allow("k", 0.0001, 3)
	for i := 0; i < 10; i++ {
		l2.peek("k", 0.0001, 3)
	}
	if !l2.allow("k", 0.0001, 3) || !l2.allow("k", 0.0001, 3) || l2.allow("k", 0.0001, 3) {
		t.Fatal("peek consumed tokens")
	}
}

func TestSearchBudget(t *testing.T) {
	// Global cap 6 → 3 per user (the minimum share).
	y := newYouTubeClient("k", 6)
	for i := 0; i < 3; i++ {
		if !y.takeSearch(1) {
			t.Fatalf("user 1 search %d", i)
		}
	}
	if y.takeSearch(1) {
		t.Fatal("user share of 3")
	}
	if !y.takeSearch(2) || !y.takeSearch(2) || !y.takeSearch(2) {
		t.Fatal("user 2 has its own share")
	}
	if y.takeSearch(3) {
		t.Fatal("global cap of 6")
	}
	y.day = "1999-01-01" // a new quota day resets both
	if !y.takeSearch(1) {
		t.Fatal("new day")
	}
	// Bigger cap → 1/6 per user.
	if b := newYouTubeClient("k", 90); b.userSearchMax() != 15 {
		t.Fatalf("share: %d", b.userSearchMax())
	}
	u := newYouTubeClient("k", 0)
	for i := 0; i < 50; i++ {
		if !u.takeSearch(1) {
			t.Fatal("0 = no cap")
		}
	}
}

func TestFoldSearch(t *testing.T) {
	if got := foldSearch("  İnception ILIQ ınce "); got != "inception iliq ince" {
		t.Fatalf("got %q", got)
	}
}

func TestRenderPush(t *testing.T) {
	title, body, ok := renderPush("watch_invite", "az", map[string]string{"host": "leyla", "room": "Kino gecəsi"})
	if !ok || title != "Birlikdə izlə" || body != "leyla səni “Kino gecəsi” otağına dəvət etdi" {
		t.Fatalf("got %q / %q", title, body)
	}
	if _, body, _ := renderPush("watch_remind", "xx", map[string]string{"room": "R"}); body != "“R” is about to start — get ready" {
		t.Fatalf("fallback: %q", body)
	}
	if normalizeLang("tr-TR") != "tr" || normalizeLang("pt") != "en" || normalizeLang("") != "en" {
		t.Fatal("normalizeLang")
	}
	for typ, langs := range pushBodies {
		for lang := range supportedLangs {
			if langs[lang] == "" {
				t.Errorf("%s missing %s", typ, lang)
			}
		}
	}
}

func TestTopOption(t *testing.T) {
	opts := []pollOption{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	if w := topOption(opts, map[int64]string{1: "b", 2: "c", 3: "b"}, false); w == nil || *w != "b" {
		t.Fatal("most votes")
	}
	if w := topOption(opts, map[int64]string{1: "c", 2: "b"}, false); w == nil || *w != "b" {
		t.Fatal("tie → earlier option")
	}
	if w := topOption(opts, map[int64]string{}, false); w != nil {
		t.Fatal("no votes, no fallback")
	}
	if w := topOption(opts, map[int64]string{}, true); w == nil || *w != "a" {
		t.Fatal("next poll fallback")
	}
}

func TestProviderMatch(t *testing.T) {
	cfg := &config{byKey: map[string]*provider{}}
	add := func(key, mode string, patterns ...string) {
		p := &provider{providerRow: providerRow{Key: key, Mode: mode, IsActive: true}}
		for _, src := range patterns {
			p.patterns = append(p.patterns, regexp.MustCompile("(?i)"+src))
		}
		cfg.providers = append(cfg.providers, p)
		cfg.byKey[key] = p
	}
	add("youtube", "sync", `(?:youtube\.com/(?:watch\?(?:.*&)?v=|embed/|shorts/|live/)|youtu\.be/)([A-Za-z0-9_-]{11})`)
	add("netflix", "companion", `netflix\.com/(?:[a-z]{2}(?:-[a-zA-Z]{2})?/)?(?:title|watch)/(\d+)`)
	for link, want := range map[string]string{
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ&t=10": "youtube:dQw4w9WgXcQ",
		"https://youtu.be/dQw4w9WgXcQ":                     "youtube:dQw4w9WgXcQ",
		"https://m.youtube.com/shorts/abcdefghijk":         "youtube:abcdefghijk",
		"https://www.netflix.com/az-en/title/81234567":     "netflix:81234567",
		"https://www.netflix.com/watch/80100172?trackId=1": "netflix:80100172",
		"https://example.com/watch?v=dQw4w9WgXcQ":          "",
	} {
		p, ref := detectProvider(cfg, link)
		got := ""
		if p != nil {
			got = p.Key + ":" + ref
		}
		if got != want {
			t.Errorf("%s: got %q want %q", link, got, want)
		}
	}
}

func TestCinemaLayout(t *testing.T) {
	cases := []struct{ n, rows, cols, couch int }{
		{6, 2, 3, 2}, {12, 3, 4, 4}, {18, 3, 6, 6}, {24, 4, 6, 6}, {40, 5, 8, 6}, {60, 6, 10, 6},
		{1, 2, 3, 2}, {99, 6, 10, 6}, // clamped to 6 / 60
	}
	for _, c := range cases {
		l := layoutFor(c.n)
		if l.Rows != c.rows || l.Cols != c.cols || l.Couch != c.couch {
			t.Fatalf("layoutFor(%d) = %+v, want rows %d cols %d couch %d", c.n, l, c.rows, c.cols, c.couch)
		}
		total := 0
		for row := 0; row < l.Rows; row++ {
			total += l.rowLen(row)
		}
		if total != l.Seats {
			t.Fatalf("layoutFor(%d): rows hold %d seats, want %d", c.n, total, l.Seats)
		}
	}
	l := layoutFor(40)
	if got := l.code(19); got != "C4" {
		t.Fatalf("code(19) = %q, want C4", got)
	}
	if got := l.code(0); got != "A1" {
		t.Fatalf("code(0) = %q, want A1", got)
	}
	// The best seat of a 5×8 hall is in row C, next to the centre aisle.
	best, bestScore := -1, 1e9
	for idx := 0; idx < l.Seats; idx++ {
		if s := l.score(idx); s < bestScore {
			best, bestScore = idx, s
		}
	}
	if code := l.code(best); code != "C4" {
		t.Fatalf("best seat = %s, want C4", code)
	}
	// A partial back row: 13 seats → 3×4 + 1, the last row holds one seat.
	p := layoutFor(13)
	if p.rowLen(p.Rows-1) != 13-p.Cols*(p.Rows-1) {
		t.Fatalf("partial row length wrong: %+v", p)
	}
}
