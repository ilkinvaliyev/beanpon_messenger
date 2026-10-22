package watch

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"beanpon_messenger/cache"
)

// ytVideo — what the room needs from a YouTube video.
type ytVideo struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	Channel       string `json:"channel"`
	Thumbnail     string `json:"thumbnail"`
	DurationMs    int64  `json:"duration_ms"`
	Embeddable    bool   `json:"embeddable"`
	Live          bool   `json:"live"`
	AgeRestricted bool   `json:"age_restricted"`
}

// youtubeClient — YouTube Data API v3 (videos.list / search.list), cached in
// Redis (falls back to a small in-process cache). Quota matters: search.list
// costs 100 units, videos.list 1 unit — searches are cached 6 h and capped
// per day (searchMax), so searches can never eat the units that room
// creation (videos.list) needs.
type youtubeClient struct {
	key       string
	http      *http.Client
	searchMax int // search.list calls per quota day; 0 = no cap

	mu       sync.Mutex
	local    map[string]localEntry
	day      string
	searches int
	perUser  map[int64]int // today's new (uncached) searches per user
}

type localEntry struct {
	val string
	exp time.Time
}

func newYouTubeClient(key string, searchMax int) *youtubeClient {
	return &youtubeClient{
		key:       strings.TrimSpace(key),
		http:      &http.Client{Timeout: 6 * time.Second},
		searchMax: searchMax,
		local:     map[string]localEntry{},
		perUser:   map[int64]int{},
	}
}

// userSearchMax — one user's share of the daily budget (1/6, at least 3),
// so a single account can't spend everyone's searches.
func (y *youtubeClient) userSearchMax() int {
	if y.searchMax <= 0 {
		return 0
	}
	if n := y.searchMax / 6; n > 3 {
		return n
	}
	return 3
}

func (y *youtubeClient) enabled() bool { return y != nil && y.key != "" }

var (
	errYouTube       = errors.New("youtube api error")
	errYouTubeBudget = errors.New("youtube daily search budget spent")
)

// quotaZone — YouTube quotas reset at midnight Pacific time (a fixed UTC−8
// here: an hour off in summer is fine for a soft cap).
var quotaZone = time.FixedZone("PT", -8*3600)

// takeSearch — one search.list call out of today's budget, overall and for
// this user (per process; messenger runs one replica). Cached queries never
// get here.
func (y *youtubeClient) takeSearch(uid int64) bool {
	y.mu.Lock()
	defer y.mu.Unlock()
	if d := time.Now().In(quotaZone).Format("2006-01-02"); d != y.day {
		y.day, y.searches, y.perUser = d, 0, map[int64]int{}
	}
	if y.searchMax > 0 && (y.searches >= y.searchMax || y.perUser[uid] >= y.userSearchMax()) {
		return false
	}
	y.searches++
	y.perUser[uid]++
	return true
}

// ── cache ─────────────────────────────────────────────────────────────────

func (y *youtubeClient) cacheGet(key string) (string, bool) {
	if cl := cache.GetClient(); cl.Enabled() {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		if v, ok, err := cl.Get(ctx, cl.LocalKey(key)); err == nil && ok {
			return v, true
		}
	}
	y.mu.Lock()
	defer y.mu.Unlock()
	if e, ok := y.local[key]; ok && time.Now().Before(e.exp) {
		return e.val, true
	}
	return "", false
}

func (y *youtubeClient) cacheSet(key, val string, ttl time.Duration) {
	if cl := cache.GetClient(); cl.Enabled() {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		_ = cl.Set(ctx, cl.LocalKey(key), val, ttl)
	}
	y.mu.Lock()
	defer y.mu.Unlock()
	if len(y.local) > 2000 {
		y.local = map[string]localEntry{}
	}
	y.local[key] = localEntry{val: val, exp: time.Now().Add(ttl)}
}

func hashKey(s string) string {
	h := sha1.Sum([]byte(s))
	return hex.EncodeToString(h[:8])
}

// ── API ───────────────────────────────────────────────────────────────────

func (y *youtubeClient) get(path string, q url.Values, out any) error {
	q.Set("key", y.key)
	req, err := http.NewRequest(http.MethodGet, "https://www.googleapis.com/youtube/v3/"+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := y.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode != http.StatusOK {
		log.Printf("[watch] youtube %s: HTTP %d", path, resp.StatusCode)
		return fmt.Errorf("%w: %d", errYouTube, resp.StatusCode)
	}
	return json.Unmarshal(body, out)
}

type ytThumb struct {
	URL string `json:"url"`
}

type ytItem struct {
	ID      json.RawMessage `json:"id"`
	Snippet struct {
		Title                string             `json:"title"`
		ChannelTitle         string             `json:"channelTitle"`
		LiveBroadcastContent string             `json:"liveBroadcastContent"`
		Thumbnails           map[string]ytThumb `json:"thumbnails"`
	} `json:"snippet"`
	ContentDetails struct {
		Duration      string `json:"duration"`
		ContentRating struct {
			YtRating string `json:"ytRating"`
		} `json:"contentRating"`
	} `json:"contentDetails"`
	Status struct {
		Embeddable    bool   `json:"embeddable"`
		PrivacyStatus string `json:"privacyStatus"`
	} `json:"status"`
}

func (it ytItem) videoID() string {
	var s string
	if json.Unmarshal(it.ID, &s) == nil {
		return s
	}
	var obj struct {
		VideoID string `json:"videoId"`
	}
	_ = json.Unmarshal(it.ID, &obj)
	return obj.VideoID
}

func bestThumb(m map[string]ytThumb) string {
	for _, k := range []string{"maxres", "standard", "high", "medium", "default"} {
		if t, ok := m[k]; ok && t.URL != "" {
			return t.URL
		}
	}
	return ""
}

func (it ytItem) toVideo() ytVideo {
	return ytVideo{
		ID:            it.videoID(),
		Title:         it.Snippet.Title,
		Channel:       it.Snippet.ChannelTitle,
		Thumbnail:     bestThumb(it.Snippet.Thumbnails),
		DurationMs:    parseISODuration(it.ContentDetails.Duration),
		Embeddable:    it.Status.Embeddable && it.Status.PrivacyStatus != "private",
		Live:          it.Snippet.LiveBroadcastContent == "live" || it.Snippet.LiveBroadcastContent == "upcoming",
		AgeRestricted: it.ContentDetails.ContentRating.YtRating == "ytAgeRestricted",
	}
}

const videoParts = "snippet,contentDetails,status"

// videos — details for up to 50 ids (cache per id, 24 h).
func (y *youtubeClient) videos(ids []string) (map[string]ytVideo, error) {
	out := map[string]ytVideo{}
	if !y.enabled() || len(ids) == 0 {
		return out, nil
	}
	var missing []string
	for _, id := range ids {
		if raw, ok := y.cacheGet("watch:yt:v:" + id); ok {
			var v ytVideo
			if raw == "-" {
				continue // known missing
			}
			if json.Unmarshal([]byte(raw), &v) == nil {
				out[id] = v
				continue
			}
		}
		missing = append(missing, id)
	}
	for start := 0; start < len(missing); start += 50 {
		end := start + 50
		if end > len(missing) {
			end = len(missing)
		}
		chunk := missing[start:end]
		var res struct {
			Items []ytItem `json:"items"`
		}
		q := url.Values{}
		q.Set("part", videoParts)
		q.Set("id", strings.Join(chunk, ","))
		q.Set("maxResults", "50")
		if err := y.get("videos", q, &res); err != nil {
			return out, err
		}
		found := map[string]bool{}
		for _, it := range res.Items {
			v := it.toVideo()
			if v.ID == "" {
				continue
			}
			found[v.ID] = true
			out[v.ID] = v
			if b, err := json.Marshal(v); err == nil {
				y.cacheSet("watch:yt:v:"+v.ID, string(b), 24*time.Hour)
			}
		}
		for _, id := range chunk {
			if !found[id] {
				y.cacheSet("watch:yt:v:"+id, "-", time.Hour)
			}
		}
	}
	return out, nil
}

// video — one video; ok = false when YouTube does not know it (or it is private).
func (y *youtubeClient) video(id string) (ytVideo, bool, error) {
	if !y.enabled() {
		return ytVideo{}, false, errYouTube
	}
	vids, err := y.videos([]string{id})
	if err != nil {
		return ytVideo{}, false, err
	}
	v, ok := vids[id]
	return v, ok, nil
}

func (y *youtubeClient) listFromIDs(cacheKey string, ttl time.Duration, ids []string) ([]ytVideo, error) {
	vids, err := y.videos(ids)
	if err != nil {
		return nil, err
	}
	out := make([]ytVideo, 0, len(ids))
	for _, id := range ids {
		if v, ok := vids[id]; ok {
			out = append(out, v)
		}
	}
	if b, err := json.Marshal(ids); err == nil {
		y.cacheSet(cacheKey, string(b), ttl)
	}
	return out, nil
}

func (y *youtubeClient) cachedList(cacheKey string) ([]ytVideo, bool) {
	raw, ok := y.cacheGet(cacheKey)
	if !ok {
		return nil, false
	}
	var ids []string
	if json.Unmarshal([]byte(raw), &ids) != nil {
		return nil, false
	}
	vids, err := y.videos(ids)
	if err != nil {
		return nil, false
	}
	out := make([]ytVideo, 0, len(ids))
	for _, id := range ids {
		if v, ok := vids[id]; ok {
			out = append(out, v)
		}
	}
	return out, true
}

// search — embeddable videos for q (6 h cache per query + region); a new
// query is charged to uid's and everyone's daily budget.
func (y *youtubeClient) search(uid int64, q, region string) ([]ytVideo, error) {
	if !y.enabled() {
		return nil, errYouTube
	}
	key := "watch:yt:s:" + region + ":" + hashKey(strings.ToLower(strings.TrimSpace(q)))
	if list, ok := y.cachedList(key); ok {
		return list, nil
	}
	if !y.takeSearch(uid) {
		return nil, errYouTubeBudget
	}
	var res struct {
		Items []ytItem `json:"items"`
	}
	p := url.Values{}
	p.Set("part", "id")
	p.Set("type", "video")
	p.Set("videoEmbeddable", "true")
	p.Set("safeSearch", "moderate")
	p.Set("maxResults", "20")
	p.Set("q", q)
	p.Set("regionCode", region)
	if err := y.get("search", p, &res); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(res.Items))
	for _, it := range res.Items {
		if id := it.videoID(); id != "" {
			ids = append(ids, id)
		}
	}
	return y.listFromIDs(key, 6*time.Hour, ids)
}

// trending — chart=mostPopular for the region (1 h cache). Regions YouTube
// has no chart for fall back to TR, then US.
func (y *youtubeClient) trending(region string) ([]ytVideo, error) {
	if !y.enabled() {
		return nil, errYouTube
	}
	key := "watch:yt:t:" + region
	if list, ok := y.cachedList(key); ok {
		return list, nil
	}
	var lastErr error
	for _, rc := range []string{region, "TR", "US"} {
		var res struct {
			Items []ytItem `json:"items"`
		}
		p := url.Values{}
		p.Set("part", videoParts)
		p.Set("chart", "mostPopular")
		p.Set("regionCode", rc)
		p.Set("maxResults", "24")
		if err := y.get("videos", p, &res); err != nil {
			lastErr = err
			continue
		}
		if len(res.Items) == 0 {
			continue
		}
		ids := make([]string, 0, len(res.Items))
		for _, it := range res.Items {
			v := it.toVideo()
			if v.ID == "" {
				continue
			}
			ids = append(ids, v.ID)
			if b, err := json.Marshal(v); err == nil {
				y.cacheSet("watch:yt:v:"+v.ID, string(b), 24*time.Hour)
			}
		}
		return y.listFromIDs(key, time.Hour, ids)
	}
	if lastErr == nil {
		lastErr = errYouTube
	}
	return nil, lastErr
}

var isoDurRe = regexp.MustCompile(`^P(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)S)?)?$`)

// parseISODuration — "PT1H2M10S" → ms (0 when unknown / live).
func parseISODuration(s string) int64 {
	m := isoDurRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0
	}
	n := func(v string) int64 {
		if v == "" {
			return 0
		}
		x, _ := strconv.ParseInt(v, 10, 64)
		return x
	}
	secs := n(m[1])*86400 + n(m[2])*3600 + n(m[3])*60 + n(m[4])
	return secs * 1000
}
