package watch

import (
	"net/http"
	"regexp"
	"strings"

	"beanpon_messenger/utils"
)

// mediaInput — what create / media change / queue / poll options accept.
type mediaInput struct {
	Provider      string  `json:"provider"`
	TitleID       *int64  `json:"title_id"`
	MediaRef      *string `json:"media_ref"`
	MediaTitle    *string `json:"media_title"`
	MediaSubtitle *string `json:"media_subtitle"`
	DurationMs    *int64  `json:"duration_ms"`
	Season        *int    `json:"season"`
	Episode       *int    `json:"episode"`
	URL           *string `json:"url"`
}

type resolvedMedia struct {
	Provider   string
	TitleID    *int64
	Ref        *string
	Title      string
	Subtitle   *string
	Kind       *string
	Poster     *string
	Backdrop   *string
	DurationMs *int64
	Season     *int
	Episode    *int
}

var (
	errProvider      = errf(http.StatusUnprocessableEntity, "PROVIDER_UNAVAILABLE", "Bu platforma hazırda əlçatan deyil")
	errNotEmbeddable = errf(http.StatusUnprocessableEntity, "NOT_EMBEDDABLE", "Bu video tətbiqdə oynadıla bilmir")
	errMediaRequired = errf(http.StatusUnprocessableEntity, "MEDIA_REQUIRED", "Nə izləyəcəyinizi seçin")
	errLink          = errf(http.StatusUnprocessableEntity, "LINK_UNSUPPORTED", "Bu link tanınmadı")
	errYouTubeDown   = errf(http.StatusServiceUnavailable, "YOUTUBE_UNAVAILABLE", "YouTube hazırda cavab vermir, bir az sonra yenə yoxla")
)

var youtubeIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

func youtubePoster(id string) *string {
	u := "https://i.ytimg.com/vi/" + id + "/hqdefault.jpg"
	return &u
}

func youtubeBackdrop(id string) *string {
	u := "https://i.ytimg.com/vi/" + id + "/maxresdefault.jpg"
	return &u
}

// detectProvider — the active provider whose link pattern matches.
func detectProvider(cfg *config, link string) (*provider, string) {
	link = strings.TrimSpace(link)
	for _, p := range cfg.activeProviders() {
		if ref, ok := p.matchRef(link); ok {
			return p, ref
		}
	}
	return nil, ""
}

// typedText — a title / subtitle the client typed itself (shown to the whole
// room, so it passes the same word filter as chat).
func typedText(p *string, max int) (string, error) {
	s := clampRunes(strings.TrimSpace(ptrStr(p)), max)
	if s != "" && utils.ContainsBadWord(s) {
		return "", errBlockedWord
	}
	return s, nil
}

// resolveMedia validates the input and builds the snapshot stored on the
// room / queue item / poll option. YouTube items are checked with the Data
// API when a key is configured (embeddable, not live, not age-restricted);
// with a key but no answer the request fails (503) — an unchecked video
// would only break the room later.
func (m *Module) resolveMedia(cfg *config, in mediaInput) (resolvedMedia, error) {
	var out resolvedMedia
	providerKey := strings.TrimSpace(in.Provider)
	ref := strings.TrimSpace(ptrStr(in.MediaRef))

	if in.URL != nil && strings.TrimSpace(*in.URL) != "" {
		p, r := detectProvider(cfg, *in.URL)
		if p == nil {
			return out, errLink
		}
		if providerKey != "" && providerKey != p.Key {
			return out, errLink
		}
		providerKey, ref = p.Key, r
	}
	p := cfg.activeProvider(providerKey)
	if p == nil {
		return out, errProvider
	}
	out.Provider = p.Key

	if in.TitleID != nil && *in.TitleID > 0 {
		t := m.loadTitle(in.TitleID)
		if t == nil || !t.IsActive || t.Provider != p.Key {
			return out, errMediaRequired
		}
		return m.mediaFromTitle(cfg, *t, in), nil
	}

	if p.isSync() {
		if !youtubeIDRe.MatchString(ref) {
			return out, errMediaRequired
		}
		out.Ref = strPtr(ref)
		out.Kind = strPtr("video")
		out.Poster = youtubePoster(ref)
		out.Backdrop = youtubeBackdrop(ref)
		if m.yt.enabled() {
			meta, ok, err := m.yt.video(ref)
			switch {
			case err != nil:
				return out, errYouTubeDown
			case !ok:
				return out, errNotEmbeddable // no such (public) video
			case !meta.Embeddable || meta.Live || meta.AgeRestricted:
				return out, errNotEmbeddable
			}
			out.Title = clampRunes(meta.Title, 200)
			out.Subtitle = nonEmptyPtr(clampRunes(meta.Channel, 200))
			if meta.DurationMs > 0 {
				d := meta.DurationMs
				out.DurationMs = &d
			}
			if meta.Thumbnail != "" {
				out.Poster = strPtr(meta.Thumbnail)
			}
		}
		if out.Title == "" {
			t, err := typedText(in.MediaTitle, 200)
			if err != nil {
				return out, err
			}
			out.Title = t
		}
		if out.Title == "" {
			out.Title = "YouTube"
		}
		if out.Subtitle == nil {
			s, err := typedText(in.MediaSubtitle, 200)
			if err != nil {
				return out, err
			}
			out.Subtitle = nonEmptyPtr(s)
		}
		if out.DurationMs == nil && in.DurationMs != nil && *in.DurationMs > 0 && *in.DurationMs < 24*3600*1000 {
			d := *in.DurationMs
			out.DurationMs = &d
		}
		return out, nil
	}

	// Companion provider without a catalog id: a pasted link (matched against
	// the catalog first) or a typed title.
	if ref != "" {
		var t titleRow
		if m.db.Where("provider = ? AND provider_ref = ? AND is_active = true", p.Key, ref).Limit(1).Find(&t).RowsAffected > 0 {
			return m.mediaFromTitle(cfg, t, in), nil
		}
		out.Ref = strPtr(clampRunes(ref, 128))
	}
	title, err := typedText(in.MediaTitle, 200)
	if err != nil {
		return out, err
	}
	if title == "" {
		return out, errMediaRequired
	}
	out.Title = title
	sub, err := typedText(in.MediaSubtitle, 200)
	if err != nil {
		return out, err
	}
	out.Subtitle = nonEmptyPtr(sub)
	out.Kind = strPtr("movie")
	if in.DurationMs != nil && *in.DurationMs > 0 && *in.DurationMs < 24*3600*1000 {
		d := *in.DurationMs
		out.DurationMs = &d
	}
	out.Season, out.Episode = clampEpisode(in.Season), clampEpisode(in.Episode)
	if out.Season != nil || out.Episode != nil {
		out.Kind = strPtr("series")
	}
	return out, nil
}

func (m *Module) mediaFromTitle(cfg *config, t titleRow, in mediaInput) resolvedMedia {
	id := t.ID
	kind := t.Kind
	out := resolvedMedia{
		Provider: t.Provider,
		TitleID:  &id,
		Ref:      t.ProviderRef,
		Title:    clampRunes(t.Title, 200),
		Kind:     &kind,
		Poster:   mediaURL(t.PosterPath),
		Backdrop: mediaURL(t.BackdropPath),
	}
	if t.DurationSec != nil && *t.DurationSec > 0 {
		d := *t.DurationSec * 1000
		out.DurationMs = &d
	}
	if t.Kind == "series" || t.Kind == "show" {
		out.Season, out.Episode = clampEpisode(in.Season), clampEpisode(in.Episode)
		// An episode's own length is unknown for a series row: keep the
		// title's (average) duration only when one was given.
		if in.DurationMs != nil && *in.DurationMs > 0 && *in.DurationMs < 24*3600*1000 {
			d := *in.DurationMs
			out.DurationMs = &d
		}
	}
	return out
}

func clampEpisode(v *int) *int {
	if v == nil || *v <= 0 {
		return nil
	}
	n := *v
	if n > 999 {
		n = 999
	}
	return &n
}
