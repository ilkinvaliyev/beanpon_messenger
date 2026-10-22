// Package watch — "Birlikdə izlə" (watch party): rooms where people watch the
// same video together. REST (/api/v1/watch/*) + WebSocket (/ws/watch) live
// here; Laravel only owns the schema and the Filament admin.
// Contract: docs/WATCH_PARTY.md.
package watch

import (
	"encoding/json"
	"time"
)

// ── DB rows ────────────────────────────────────────────────────────────────

type roomRow struct {
	ID                     int64      `gorm:"column:id;primaryKey"`
	HostID                 int64      `gorm:"column:host_id"`
	CreatedBy              int64      `gorm:"column:created_by"`
	Name                   string     `gorm:"column:name"`
	Provider               string     `gorm:"column:provider"`
	TitleID                *int64     `gorm:"column:title_id"`
	MediaRef               *string    `gorm:"column:media_ref"`
	MediaTitle             string     `gorm:"column:media_title"`
	MediaSubtitle          *string    `gorm:"column:media_subtitle"`
	MediaKind              *string    `gorm:"column:media_kind"`
	MediaPosterURL         *string    `gorm:"column:media_poster_url"`
	MediaBackdropURL       *string    `gorm:"column:media_backdrop_url"`
	MediaDurationMs        *int64     `gorm:"column:media_duration_ms"`
	Season                 *int       `gorm:"column:season"`
	Episode                *int       `gorm:"column:episode"`
	Access                 string     `gorm:"column:access"`
	FollowersOnly          bool       `gorm:"column:followers_only"`
	Locked                 bool       `gorm:"column:locked"`
	ChatMode               string     `gorm:"column:chat_mode"`
	PauseRequestsEnabled   bool       `gorm:"column:pause_requests_enabled"`
	FlyingReactionsEnabled bool       `gorm:"column:flying_reactions_enabled"`
	Code                   string     `gorm:"column:code"`
	Status                 string     `gorm:"column:status"`
	ScheduledAt            *time.Time `gorm:"column:scheduled_at"`
	OpenedAt               *time.Time `gorm:"column:opened_at"`
	StartedAt              *time.Time `gorm:"column:started_at"`
	EndedAt                *time.Time `gorm:"column:ended_at"`
	EndReason              *string    `gorm:"column:end_reason"`
	OpenReminderSentAt     *time.Time `gorm:"column:open_reminder_sent_at"`
	PositionMs             int64      `gorm:"column:position_ms"`
	IsPlaying              bool       `gorm:"column:is_playing"`
	PlaybackRate           float64    `gorm:"column:playback_rate"`
	PositionUpdatedMs      int64      `gorm:"column:position_updated_ms"`
	StateVersion           int64      `gorm:"column:state_version"`
	BreakUntilMs           *int64     `gorm:"column:break_until_ms"`
	CurrentSessionID       *int64     `gorm:"column:current_session_id"`
	PinnedMessageID        *int64     `gorm:"column:pinned_message_id"`
	WatchingCount          int        `gorm:"column:watching_count"`
	MemberCount            int        `gorm:"column:member_count"`
	PeakCount              int        `gorm:"column:peak_count"`
	LastActiveAt           *time.Time `gorm:"column:last_active_at"`
	CreatedAt              time.Time  `gorm:"column:created_at"`
	UpdatedAt              time.Time  `gorm:"column:updated_at"`
}

func (roomRow) TableName() string { return "watch_rooms" }

type memberRow struct {
	ID             int64      `gorm:"column:id;primaryKey"`
	RoomID         int64      `gorm:"column:room_id"`
	UserID         int64      `gorm:"column:user_id"`
	Role           string     `gorm:"column:role"`
	State          string     `gorm:"column:state"`
	ChatMuted      bool       `gorm:"column:chat_muted"`
	JoinedAt       *time.Time `gorm:"column:joined_at"`
	LeftAt         *time.Time `gorm:"column:left_at"`
	LastSeenAt     *time.Time `gorm:"column:last_seen_at"`
	LastPositionMs *int64     `gorm:"column:last_position_ms"`
	CreatedAt      time.Time  `gorm:"column:created_at"`
	UpdatedAt      time.Time  `gorm:"column:updated_at"`
}

func (memberRow) TableName() string { return "watch_room_members" }

type titleRow struct {
	ID           int64     `gorm:"column:id;primaryKey"`
	Provider     string    `gorm:"column:provider"`
	Kind         string    `gorm:"column:kind"`
	Title        string    `gorm:"column:title"`
	Original     *string   `gorm:"column:original_title"`
	Year         *int      `gorm:"column:year"`
	DurationSec  *int64    `gorm:"column:duration_sec"`
	SeasonsCount *int      `gorm:"column:seasons_count"`
	AgeRating    *string   `gorm:"column:age_rating"`
	Genres       *string   `gorm:"column:genres"` // json array text
	PosterPath   *string   `gorm:"column:poster_path"`
	BackdropPath *string   `gorm:"column:backdrop_path"`
	ProviderRef  *string   `gorm:"column:provider_ref"`
	ProviderURL  *string   `gorm:"column:provider_url"`
	Description  *string   `gorm:"column:description"`
	IsActive     bool      `gorm:"column:is_active"`
	IsFeatured   bool      `gorm:"column:is_featured"`
	Sort         int       `gorm:"column:sort"`
	CreatedAt    time.Time `gorm:"column:created_at"`
}

func (titleRow) TableName() string { return "watch_titles" }

type messageRow struct {
	ID         int64      `gorm:"column:id;primaryKey"`
	RoomID     int64      `gorm:"column:room_id"`
	SessionID  *int64     `gorm:"column:session_id"`
	UserID     *int64     `gorm:"column:user_id"`
	Type       string     `gorm:"column:type"`
	Text       *string    `gorm:"column:text"`
	MediaURL   *string    `gorm:"column:media_url"`
	Meta       *string    `gorm:"column:meta"`
	PositionMs *int64     `gorm:"column:position_ms"`
	ClientID   *string    `gorm:"column:client_id"`
	CreatedAt  time.Time  `gorm:"column:created_at"`
	UpdatedAt  time.Time  `gorm:"column:updated_at"`
	DeletedAt  *time.Time `gorm:"column:deleted_at"`
}

func (messageRow) TableName() string { return "watch_room_messages" }

type sessionRow struct {
	ID              int64      `gorm:"column:id;primaryKey"`
	RoomID          int64      `gorm:"column:room_id"`
	Provider        string     `gorm:"column:provider"`
	TitleID         *int64     `gorm:"column:title_id"`
	MediaRef        *string    `gorm:"column:media_ref"`
	MediaTitle      string     `gorm:"column:media_title"`
	MediaPosterURL  *string    `gorm:"column:media_poster_url"`
	MediaDurationMs *int64     `gorm:"column:media_duration_ms"`
	StartedAt       *time.Time `gorm:"column:started_at"`
	EndedAt         *time.Time `gorm:"column:ended_at"`
	WatchedMs       int64      `gorm:"column:watched_ms"`
	PeakViewers     int        `gorm:"column:peak_viewers"`
	ViewersCount    int        `gorm:"column:viewers_count"`
	MessagesCount   int        `gorm:"column:messages_count"`
	ReactionsCount  int        `gorm:"column:reactions_count"`
	BreaksCount     int        `gorm:"column:breaks_count"`
	CreatedAt       time.Time  `gorm:"column:created_at"`
	UpdatedAt       time.Time  `gorm:"column:updated_at"`
}

func (sessionRow) TableName() string { return "watch_room_sessions" }

type pollRow struct {
	ID             int64      `gorm:"column:id;primaryKey"`
	RoomID         int64      `gorm:"column:room_id"`
	SessionID      *int64     `gorm:"column:session_id"`
	CreatedBy      *int64     `gorm:"column:created_by"`
	Kind           string     `gorm:"column:kind"`
	Question       *string    `gorm:"column:question"`
	Options        string     `gorm:"column:options"` // jsonb text
	ClosesAt       *time.Time `gorm:"column:closes_at"`
	ClosedAt       *time.Time `gorm:"column:closed_at"`
	WinnerOptionID *string    `gorm:"column:winner_option_id"`
	MessageID      *int64     `gorm:"column:message_id"`
	CreatedAt      time.Time  `gorm:"column:created_at"`
	UpdatedAt      time.Time  `gorm:"column:updated_at"`
}

func (pollRow) TableName() string { return "watch_room_polls" }

type queueRow struct {
	ID              int64     `gorm:"column:id;primaryKey"`
	RoomID          int64     `gorm:"column:room_id"`
	SuggestedBy     *int64    `gorm:"column:suggested_by"`
	Provider        string    `gorm:"column:provider"`
	TitleID         *int64    `gorm:"column:title_id"`
	MediaRef        *string   `gorm:"column:media_ref"`
	MediaTitle      string    `gorm:"column:media_title"`
	MediaSubtitle   *string   `gorm:"column:media_subtitle"`
	MediaKind       *string   `gorm:"column:media_kind"`
	MediaPosterURL  *string   `gorm:"column:media_poster_url"`
	MediaDurationMs *int64    `gorm:"column:media_duration_ms"`
	Status          string    `gorm:"column:status"`
	VotesCount      int       `gorm:"column:votes_count"`
	CreatedAt       time.Time `gorm:"column:created_at"`
	UpdatedAt       time.Time `gorm:"column:updated_at"`
}

func (queueRow) TableName() string { return "watch_room_queue" }

// ── JSON shapes (docs/WATCH_PARTY.md §5) ───────────────────────────────────

type UserLite struct {
	ID         int64   `json:"id"`
	Username   string  `json:"username"`
	Name       string  `json:"name"`
	Avatar     *string `json:"avatar"`
	IsVerified bool    `json:"is_verified"`
}

type MediaJSON struct {
	Provider    string   `json:"provider"`
	TitleID     *int64   `json:"title_id"`
	MediaRef    *string  `json:"media_ref"`
	Title       string   `json:"title"`
	Subtitle    *string  `json:"subtitle"`
	Kind        *string  `json:"kind"`
	PosterURL   *string  `json:"poster_url"`
	BackdropURL *string  `json:"backdrop_url"`
	DurationMs  *int64   `json:"duration_ms"`
	Season      *int     `json:"season"`
	Episode     *int     `json:"episode"`
	Year        *int     `json:"year"`
	AgeRating   *string  `json:"age_rating"`
	Genres      []string `json:"genres"`
	OpenURL     *string  `json:"open_url"`
}

type PlaybackJSON struct {
	PositionMs   int64   `json:"position_ms"`
	IsPlaying    bool    `json:"is_playing"`
	Rate         float64 `json:"rate"`
	UpdatedAtMs  int64   `json:"updated_at_ms"`
	ServerMs     int64   `json:"server_ms"`
	BreakUntilMs *int64  `json:"break_until_ms"`
	Version      int64   `json:"version"`
}

type FriendsJSON struct {
	Users []UserLite `json:"users"`
	Count int        `json:"count"`
}

type RoomCard struct {
	ID            int64        `json:"id"`
	Name          string       `json:"name"`
	Status        string       `json:"status"`
	Access        string       `json:"access"`
	FollowersOnly bool         `json:"followers_only"`
	Locked        bool         `json:"locked"`
	Media         MediaJSON    `json:"media"`
	Playback      PlaybackJSON `json:"playback"`
	WatchingCount int          `json:"watching_count"`
	MemberCount   int          `json:"member_count"`
	Host          UserLite     `json:"host"`
	Friends       FriendsJSON  `json:"friends"`
	ScheduledAt   *time.Time   `json:"scheduled_at"`
	StartedAt     *time.Time   `json:"started_at"`
	CreatedAt     time.Time    `json:"created_at"`
}

type MeJSON struct {
	Role             *string    `json:"role"`
	State            *string    `json:"state"`
	RequestStatus    *string    `json:"request_status"`
	RequestExpiresAt *time.Time `json:"request_expires_at"`
	Invited          bool       `json:"invited"`
	Reminder         bool       `json:"reminder"`
	ChatMuted        bool       `json:"chat_muted"`
	CanControl       bool       `json:"can_control"`
	IsHost           bool       `json:"is_host"`
	HasAccount       *bool      `json:"has_account"`
	HostFollowsMe    bool       `json:"host_follows_me"`
}

type RoomDetail struct {
	RoomCard
	ChatMode               string        `json:"chat_mode"`
	PauseRequestsEnabled   bool          `json:"pause_requests_enabled"`
	FlyingReactionsEnabled bool          `json:"flying_reactions_enabled"`
	Code                   *string       `json:"code"`
	PinnedMessage          *MessageJSON  `json:"pinned_message"`
	CurrentSessionID       *int64        `json:"current_session_id"`
	MaxMembers             int           `json:"max_members"`
	EndReason              *string       `json:"end_reason"`
	Me                     MeJSON        `json:"me"`
	ProviderInfo           *ProviderJSON `json:"provider_info"`
}

type ProviderJSON struct {
	Key                  string   `json:"key"`
	Name                 string   `json:"name"`
	Mode                 string   `json:"mode"`
	RequiresSubscription bool     `json:"requires_subscription"`
	BrandColor           *string  `json:"brand_color"`
	OpenURLTemplate      *string  `json:"open_url_template"`
	SearchURLTemplate    *string  `json:"search_url_template"`
	LinkPatterns         []string `json:"link_patterns"`
	IOSAppStoreURL       *string  `json:"ios_app_store_url"`
	AndroidPackage       *string  `json:"android_package"`
	AndroidStoreURL      *string  `json:"android_store_url"`
}

type SocialJSON struct {
	Type        string     `json:"type"` // friends_watched | trending | resume
	Users       []UserLite `json:"users"`
	RemainingMs *int64     `json:"remaining_ms"`
}

type TitleResult struct {
	Provider     string      `json:"provider"`
	TitleID      *int64      `json:"title_id"`
	MediaRef     *string     `json:"media_ref"`
	Title        string      `json:"title"`
	Kind         string      `json:"kind"`
	Year         *int        `json:"year"`
	DurationMs   *int64      `json:"duration_ms"`
	SeasonsCount *int        `json:"seasons_count"`
	AgeRating    *string     `json:"age_rating"`
	Genres       []string    `json:"genres"`
	PosterURL    *string     `json:"poster_url"`
	BackdropURL  *string     `json:"backdrop_url"`
	Channel      *string     `json:"channel"`
	OpenURL      *string     `json:"open_url"`
	Embeddable   bool        `json:"embeddable"`
	Social       *SocialJSON `json:"social"`
}

type MessageJSON struct {
	ID         int64           `json:"id"`
	ClientID   *string         `json:"client_id"`
	Type       string          `json:"type"`
	User       *UserLite       `json:"user"`
	Text       *string         `json:"text"`
	MediaURL   *string         `json:"media_url"`
	Meta       json.RawMessage `json:"meta"`
	PositionMs *int64          `json:"position_ms"`
	CreatedAt  time.Time       `json:"created_at"`
	Deleted    bool            `json:"deleted"`
}

type SyncJSON struct {
	State   string `json:"state"` // in_sync | behind | ahead | unknown | detached
	DriftMs int64  `json:"drift_ms"`
}

type MemberJSON struct {
	User      UserLite   `json:"user"`
	Role      string     `json:"role"`
	ChatMuted bool       `json:"chat_muted"`
	Online    bool       `json:"online"`
	Ready     string     `json:"ready"`
	Sync      SyncJSON   `json:"sync"`
	JoinedAt  *time.Time `json:"joined_at"`
}

type PollOptionJSON struct {
	ID         string  `json:"id"`
	Label      string  `json:"label"`
	Provider   string  `json:"provider"`
	TitleID    *int64  `json:"title_id"`
	MediaRef   *string `json:"media_ref"`
	PosterURL  *string `json:"poster_url"`
	Subtitle   *string `json:"subtitle"`
	Kind       *string `json:"kind"`
	DurationMs *int64  `json:"duration_ms"`
	Votes      int     `json:"votes"`
}

type PollJSON struct {
	ID             int64            `json:"id"`
	Kind           string           `json:"kind"`
	Question       *string          `json:"question"`
	Options        []PollOptionJSON `json:"options"`
	TotalVotes     int              `json:"total_votes"`
	MyOption       *string          `json:"my_option"`
	ClosesAtMs     *int64           `json:"closes_at_ms"`
	Closed         bool             `json:"closed"`
	WinnerOptionID *string          `json:"winner_option_id"`
	CreatedBy      *UserLite        `json:"created_by"`
}

type QueueItemJSON struct {
	ID          int64     `json:"id"`
	Media       MediaJSON `json:"media"`
	SuggestedBy *UserLite `json:"suggested_by"`
	Votes       int       `json:"votes"`
	Voted       bool      `json:"voted"`
	CreatedAt   time.Time `json:"created_at"`
}

// pollOption is the stored shape inside watch_room_polls.options.
type pollOption struct {
	ID         string  `json:"id"`
	Label      string  `json:"label"`
	Provider   string  `json:"provider"`
	TitleID    *int64  `json:"title_id,omitempty"`
	MediaRef   *string `json:"media_ref,omitempty"`
	PosterURL  *string `json:"poster_url,omitempty"`
	Subtitle   *string `json:"subtitle,omitempty"`
	Kind       *string `json:"kind,omitempty"`
	DurationMs *int64  `json:"duration_ms,omitempty"`
	Backdrop   *string `json:"backdrop_url,omitempty"`
	QueueID    *int64  `json:"queue_id,omitempty"`
	Title      string  `json:"title,omitempty"` // the media title (label may be custom text)
}

func (o pollOption) hasMedia() bool {
	return o.Provider != "" && (o.TitleID != nil || (o.MediaRef != nil && *o.MediaRef != ""))
}

func (o pollOption) media() resolvedMedia {
	title := o.Title
	if title == "" {
		title = o.Label
	}
	return resolvedMedia{
		Provider:   o.Provider,
		TitleID:    o.TitleID,
		Ref:        o.MediaRef,
		Title:      title,
		Subtitle:   o.Subtitle,
		Kind:       o.Kind,
		Poster:     o.PosterURL,
		Backdrop:   o.Backdrop,
		DurationMs: o.DurationMs,
	}
}
