package watch

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Config — what the module needs from the messenger config.
type Config struct {
	CloudToken    string // Laravel cloud API key (push)
	BackendURL    string // Laravel cloud base, e.g. https://api.beanpon.com/api/cloud
	YouTubeAPIKey string
	// YouTubeSearchDailyMax — search.list calls per day (100 quota units
	// each); 0 = no cap. Cached repeats are free.
	YouTubeSearchDailyMax int
}

// Module — the whole watch-party feature (REST + WS hub + background jobs).
type Module struct {
	db       *gorm.DB
	cfg      Config
	hub      *Hub
	settings *settingsCache
	yt       *youtubeClient
	http     *http.Client
	pushSem  chan struct{}
	limiter  *rateLimiter
}

func New(db *gorm.DB, cfg Config) *Module {
	m := &Module{
		db:      db,
		cfg:     cfg,
		http:    &http.Client{Timeout: 10 * time.Second},
		pushSem: make(chan struct{}, 8),
		limiter: newRateLimiter(),
	}
	m.settings = newSettingsCache(db)
	m.yt = newYouTubeClient(cfg.YouTubeAPIKey, cfg.YouTubeSearchDailyMax)
	m.hub = newHub(m)
	return m
}

// RegisterRoutes mounts the REST endpoints on the JWT-protected /api/v1 group
// and the admin hooks on the /internal group (X-Internal-Secret).
func (m *Module) RegisterRoutes(api *gin.RouterGroup, internal *gin.RouterGroup) {
	w := api.Group("/watch")

	w.GET("/config", m.handleConfig)
	w.PUT("/accounts/:provider", m.handleSetAccount)
	w.GET("/home", m.handleHome)
	w.GET("/hub", m.handleHub)
	w.GET("/rooms", m.handleRoomsList)
	w.GET("/titles/featured", m.handleTitlesFeatured)
	w.GET("/titles/search", m.handleTitlesSearch)
	w.POST("/titles/resolve", m.handleTitlesResolve)

	w.POST("/rooms", m.handleCreateRoom)
	w.GET("/code/:code", m.handleCodeLookup)
	w.GET("/rooms/:id", m.handleGetRoom)
	w.PATCH("/rooms/:id", m.handlePatchRoom)
	w.POST("/rooms/:id/join", m.handleJoin)
	w.DELETE("/rooms/:id/request", m.handleCancelRequest)
	w.POST("/rooms/:id/leave", m.handleLeave)
	w.POST("/rooms/:id/remind", m.handleRemindOn)
	w.DELETE("/rooms/:id/remind", m.handleRemindOff)
	w.POST("/rooms/:id/open", m.handleOpen)
	w.POST("/rooms/:id/start", m.handleStart)
	w.POST("/rooms/:id/end", m.handleEnd)
	w.POST("/rooms/:id/code", m.handleRotateCode)
	w.POST("/rooms/:id/media", m.handleChangeMedia)
	w.POST("/rooms/:id/transfer", m.handleTransfer)

	w.GET("/rooms/:id/members", m.handleMembers)
	w.POST("/rooms/:id/members/:uid/role", m.handleMemberRole)
	w.POST("/rooms/:id/members/:uid/chat-mute", m.handleMemberChatMute)
	w.DELETE("/rooms/:id/members/:uid", m.handleKick)
	w.GET("/rooms/:id/requests", m.handleRequests)
	w.POST("/rooms/:id/requests/accept-all", m.handleAcceptAll)
	w.POST("/rooms/:id/requests/:uid/accept", m.handleAcceptRequest)
	w.POST("/rooms/:id/requests/:uid/reject", m.handleRejectRequest)
	w.GET("/rooms/:id/invite-candidates", m.handleInviteCandidates)
	w.POST("/rooms/:id/invites", m.handleInvite)
	w.POST("/rooms/:id/remind-unready", m.handleRemindUnready)
	w.POST("/rooms/:id/help/:uid", m.handleHelp)

	w.GET("/rooms/:id/messages", m.handleMessages)
	w.POST("/rooms/:id/messages", m.handleSendMessage)
	w.DELETE("/rooms/:id/messages/:mid", m.handleDeleteMessage)
	w.POST("/rooms/:id/pin", m.handlePin)
	w.POST("/rooms/:id/moments", m.handleMoment)

	w.POST("/rooms/:id/polls", m.handleCreatePoll)
	w.POST("/polls/:pid/vote", m.handlePollVote)
	w.POST("/polls/:pid/pick", m.handlePollPick)
	w.POST("/polls/:pid/close", m.handlePollClose)

	w.GET("/rooms/:id/queue", m.handleQueue)
	w.POST("/rooms/:id/queue", m.handleQueueAdd)
	w.POST("/queue/:qid/vote", m.handleQueueVote)
	w.DELETE("/queue/:qid", m.handleQueueRemove)
	w.POST("/queue/:qid/play", m.handleQueuePlay)

	w.GET("/rooms/:id/summary", m.handleSummary)

	if internal != nil {
		internal.POST("/watch/rooms/:id/end", m.handleInternalEnd)
		internal.POST("/watch/settings/reload", m.handleInternalReloadSettings)
	}
}

// Start launches the hub ticker and the sweeper. Both stop with ctx.
func (m *Module) Start(ctx context.Context) {
	// Nobody is connected to a freshly started process: counters left by the
	// previous one (crash / deploy) would show phantom viewers in the lists.
	go func() {
		defer func() { _ = recover() }()
		if err := m.db.Exec(`UPDATE watch_rooms SET watching_count = 0
			WHERE status <> 'ended' AND watching_count <> 0`).Error; err != nil {
			log.Printf("[watch] reset watching counts: %v", err)
		}
	}()
	go m.safeLoop(ctx, "hub-tick", time.Second, m.hub.tick)
	go m.safeLoop(ctx, "sweeper", 30*time.Second, m.sweep)
	log.Println("🎬 watch party module started")
}

// safeLoop runs fn every d until ctx is done; a panic in one run is logged
// and the loop keeps going (a bug here must never take messenger down).
func (m *Module) safeLoop(ctx context.Context, name string, d time.Duration, fn func(now time.Time)) {
	ticker := time.NewTicker(d)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("[watch] %s panic: %v", name, r)
					}
				}()
				fn(now)
			}()
		}
	}
}
