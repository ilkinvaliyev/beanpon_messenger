package websocket

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// chat_streak.go — two-person chat streaks ("seriya"). Every rule lives in
// piokio_community (docs/CHAT_STREAKS.md there); this file only:
//
//  1. signals "X wrote a DM to Y" → community POST /internal/chat-streak/activity
//     (X-Api-Key = COMMUNITY_INTERNAL_API_KEY), async on a small worker pool, at most once
//     per direction per Baku day per instance (community is idempotent anyway), never
//     for `call` rows or self chats, never blocking the send path;
//  2. relays the `chat_streak` WS events community answers (and the ones it pushes to
//     POST /internal/chat-streak/notify, e.g. after a restore) to the users.
//
// COMMUNITY_INTERNAL_URL empty → nothing is signalled (streaks simply do not count).

const (
	chatStreakWorkers = 2
	chatStreakQueue   = 2048
)

type chatStreakPair [2]uint // sender, receiver

type chatStreakSignaler struct {
	hub    *Hub
	url    string
	key    string
	client *http.Client
	queue  chan chatStreakPair

	mu   sync.Mutex
	day  string
	sent map[chatStreakPair]struct{}
}

type chatStreakNotify struct {
	UserID  uint            `json:"user_id"`
	Payload json.RawMessage `json:"payload"`
}

// AttachChatStreak — wires the streak signal (main.go). Empty URL → disabled.
func (h *Hub) AttachChatStreak(communityURL, apiKey string) {
	communityURL = strings.TrimRight(strings.TrimSpace(communityURL), "/")
	if communityURL == "" {
		log.Println("ℹ️ chat streaks: COMMUNITY_INTERNAL_URL boşdur — seriya siqnalı söndürülüb")
		return
	}
	s := &chatStreakSignaler{
		hub:    h,
		url:    communityURL + "/internal/chat-streak/activity",
		key:    apiKey,
		client: &http.Client{Timeout: 5 * time.Second},
		queue:  make(chan chatStreakPair, chatStreakQueue),
		sent:   make(map[chatStreakPair]struct{}),
	}
	for i := 0; i < chatStreakWorkers; i++ {
		go s.run()
	}
	h.chatStreak = s
}

// signalChatStreak — called for every persisted DM (HandleNewMessage). Non-blocking.
func (h *Hub) signalChatStreak(senderID, receiverID uint, msgType string) {
	s := h.chatStreak
	if s == nil || senderID == 0 || receiverID == 0 || senderID == receiverID || msgType == "call" {
		return
	}
	pair := chatStreakPair{senderID, receiverID}
	if !s.mark(pair) {
		return
	}
	select {
	case s.queue <- pair:
	default:
		s.unmark(pair) // queue full — the next message retries
	}
}

// mark — first signal of this direction today on this instance?
func (s *chatStreakSignaler) mark(p chatStreakPair) bool {
	day := time.Now().In(bakuLocation()).Format("2006-01-02")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.day != day {
		s.day, s.sent = day, make(map[chatStreakPair]struct{})
	}
	if _, ok := s.sent[p]; ok {
		return false
	}
	s.sent[p] = struct{}{}
	return true
}

func (s *chatStreakSignaler) unmark(p chatStreakPair) {
	s.mu.Lock()
	delete(s.sent, p)
	s.mu.Unlock()
}

func (s *chatStreakSignaler) run() {
	for p := range s.queue {
		s.send(p)
	}
}

func (s *chatStreakSignaler) send(p chatStreakPair) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("⚠️ chat streak signal panic: %v", r)
		}
	}()
	body, _ := json.Marshal(map[string]uint{"sender_id": p[0], "receiver_id": p[1]})
	req, err := http.NewRequest(http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		s.unmark(p)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", s.key)
	resp, err := s.client.Do(req)
	if err != nil {
		s.unmark(p)
		log.Printf("⚠️ chat streak signal %d→%d: %v", p[0], p[1], err)
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode >= 300 {
		s.unmark(p)
		log.Printf("⚠️ chat streak signal %d→%d: HTTP %d", p[0], p[1], resp.StatusCode)
		return
	}
	var out struct {
		Notify []chatStreakNotify `json:"notify"`
	}
	if json.Unmarshal(raw, &out) == nil {
		s.hub.deliverChatStreak(out.Notify)
	}
}

// deliverChatStreak — each payload as WS `chat_streak` to its user (cluster-aware).
func (h *Hub) deliverChatStreak(items []chatStreakNotify) {
	for _, it := range items {
		if it.UserID == 0 || len(it.Payload) == 0 {
			continue
		}
		h.SendToUser(it.UserID, "chat_streak", it.Payload)
	}
}

// RelayChatStreak — internal POST /internal/chat-streak/notify {notify: [{user_id,
// payload}]} (X-Internal-Secret): events that start in piokio_community (restore).
func (h *Hub) RelayChatStreak(c *gin.Context) {
	var body struct {
		Notify []chatStreakNotify `json:"notify"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false})
		return
	}
	if len(body.Notify) > 50 {
		body.Notify = body.Notify[:50]
	}
	h.deliverChatStreak(body.Notify)
	c.JSON(http.StatusOK, gin.H{"ok": true, "delivered": len(body.Notify)})
}
