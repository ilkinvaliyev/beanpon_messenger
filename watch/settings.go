package watch

import (
	"encoding/json"
	"log"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
)

// watchSettings — the single watch_settings row (Filament "Ayarlar").
type watchSettings struct {
	Enabled               bool `gorm:"column:enabled"`
	HomePreviewEnabled    bool `gorm:"column:home_preview_enabled"`
	MaxMembers            int  `gorm:"column:max_members"`
	MaxActiveRoomsPerUser int  `gorm:"column:max_active_rooms_per_user"`
	MaxRoomsPerDay        int  `gorm:"column:max_rooms_per_day"`
	RequestTTLMinutes     int  `gorm:"column:request_ttl_minutes"`
	EmptyRoomMinutes      int  `gorm:"column:empty_room_minutes"`
	ScheduledGraceMinutes int  `gorm:"column:scheduled_grace_minutes"`
	MaxScheduleDays       int  `gorm:"column:max_schedule_days"`
	YouTubeSearchEnabled  bool `gorm:"column:youtube_search_enabled"`
	ChatMaxLength         int  `gorm:"column:chat_max_length"`
	BreakMinutes          int  `gorm:"column:break_minutes"`
	NextPollSeconds       int  `gorm:"column:next_poll_seconds"`
}

// normalized fills zero / broken values with the migration defaults so a
// half-edited row can never produce a 0-member room or a 0 s request TTL.
func (s watchSettings) normalized() watchSettings {
	def := func(v *int, d, min, max int) {
		if *v <= 0 {
			*v = d
		}
		if *v < min {
			*v = min
		}
		if *v > max {
			*v = max
		}
	}
	def(&s.MaxMembers, 50, 2, 500)
	def(&s.MaxActiveRoomsPerUser, 1, 1, 20)
	def(&s.MaxRoomsPerDay, 20, 1, 500)
	def(&s.RequestTTLMinutes, 10, 1, 1440)
	def(&s.EmptyRoomMinutes, 2, 2, 1440)
	def(&s.ScheduledGraceMinutes, 120, 10, 1440)
	def(&s.MaxScheduleDays, 7, 1, 60)
	def(&s.ChatMaxLength, 500, 20, 4000)
	def(&s.BreakMinutes, 5, 1, 15)
	def(&s.NextPollSeconds, 100, 15, 600)
	return s
}

type providerRow struct {
	Key                  string  `gorm:"column:key;primaryKey"`
	Name                 string  `gorm:"column:name"`
	Mode                 string  `gorm:"column:mode"`
	IsActive             bool    `gorm:"column:is_active"`
	Sort                 int     `gorm:"column:sort"`
	RequiresSubscription bool    `gorm:"column:requires_subscription"`
	BrandColor           *string `gorm:"column:brand_color"`
	OpenURLTemplate      *string `gorm:"column:open_url_template"`
	SearchURLTemplate    *string `gorm:"column:search_url_template"`
	LinkPatterns         *string `gorm:"column:link_patterns"`
	IOSAppStoreURL       *string `gorm:"column:ios_app_store_url"`
	AndroidPackage       *string `gorm:"column:android_package"`
	AndroidStoreURL      *string `gorm:"column:android_store_url"`
}

func (providerRow) TableName() string { return "watch_providers" }

type provider struct {
	providerRow
	patternSources []string
	patterns       []*regexp.Regexp
}

func (p *provider) isSync() bool { return p != nil && p.Mode == "sync" }

// openURL — the provider's own link for a ref (nil when the provider has no
// template or the ref is empty).
func (p *provider) openURL(ref string) *string {
	if p == nil || p.OpenURLTemplate == nil || strings.TrimSpace(*p.OpenURLTemplate) == "" {
		return nil
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil
	}
	u := strings.ReplaceAll(*p.OpenURLTemplate, "{ref}", url.PathEscape(ref))
	return &u
}

// matchRef — first capture group of the first matching link pattern.
func (p *provider) matchRef(link string) (string, bool) {
	for _, re := range p.patterns {
		m := re.FindStringSubmatch(link)
		if len(m) >= 2 && m[1] != "" {
			return m[1], true
		}
	}
	return "", false
}

func (p *provider) toJSON() ProviderJSON {
	return ProviderJSON{
		Key:                  p.Key,
		Name:                 p.Name,
		Mode:                 p.Mode,
		RequiresSubscription: p.RequiresSubscription,
		BrandColor:           p.BrandColor,
		OpenURLTemplate:      p.OpenURLTemplate,
		SearchURLTemplate:    p.SearchURLTemplate,
		LinkPatterns:         p.patternSources,
		IOSAppStoreURL:       p.IOSAppStoreURL,
		AndroidPackage:       p.AndroidPackage,
		AndroidStoreURL:      p.AndroidStoreURL,
	}
}

// config — one immutable snapshot of settings + providers.
type config struct {
	s         watchSettings
	providers []*provider // all, by sort
	byKey     map[string]*provider
	loaded    bool
}

func (c *config) provider(key string) *provider {
	if c == nil {
		return nil
	}
	return c.byKey[key]
}

func (c *config) activeProvider(key string) *provider {
	p := c.provider(key)
	if p == nil || !p.IsActive {
		return nil
	}
	return p
}

func (c *config) activeProviders() []*provider {
	out := make([]*provider, 0, len(c.providers))
	for _, p := range c.providers {
		if p.IsActive {
			out = append(out, p)
		}
	}
	return out
}

// settingsCache — watch_settings + watch_providers, reloaded every 60 s (or
// at once through POST /internal/watch/settings/reload). A failed reload keeps
// the last good snapshot; before the first good load the feature is off (the
// migration has not run yet).
type settingsCache struct {
	db  *gorm.DB
	mu  sync.Mutex
	cur *config
	at  time.Time
}

func newSettingsCache(db *gorm.DB) *settingsCache {
	return &settingsCache{db: db, cur: &config{byKey: map[string]*provider{}}}
}

const settingsTTL = 60 * time.Second

func (c *settingsCache) get() *config {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.at) < settingsTTL && c.cur != nil {
		return c.cur
	}
	c.at = time.Now()
	if next, err := c.load(); err == nil {
		c.cur = next
	} else {
		log.Printf("[watch] settings reload failed: %v", err)
	}
	return c.cur
}

func (c *settingsCache) invalidate() {
	c.mu.Lock()
	c.at = time.Time{}
	c.mu.Unlock()
}

func (c *settingsCache) load() (*config, error) {
	var s watchSettings
	res := c.db.Table("watch_settings").Where("id = ?", 1).Limit(1).Find(&s)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		// No row yet (seed missing) — defaults, feature on.
		s = watchSettings{Enabled: true, HomePreviewEnabled: true, YouTubeSearchEnabled: true}
	}
	s = s.normalized()

	var rows []providerRow
	if err := c.db.Order("sort ASC, key ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	cfg := &config{s: s, byKey: make(map[string]*provider, len(rows)), loaded: true}
	for _, r := range rows {
		p := &provider{providerRow: r}
		if r.LinkPatterns != nil && strings.TrimSpace(*r.LinkPatterns) != "" {
			var list []string
			if err := json.Unmarshal([]byte(*r.LinkPatterns), &list); err == nil {
				for _, src := range list {
					src = strings.TrimSpace(src)
					if src == "" {
						continue
					}
					re, err := regexp.Compile("(?i)" + src)
					if err != nil {
						log.Printf("[watch] provider %s: bad link pattern %q: %v", r.Key, src, err)
						continue
					}
					p.patterns = append(p.patterns, re)
					p.patternSources = append(p.patternSources, src)
				}
			}
		}
		if p.patternSources == nil {
			p.patternSources = []string{}
		}
		cfg.providers = append(cfg.providers, p)
		cfg.byKey[r.Key] = p
	}
	return cfg, nil
}
