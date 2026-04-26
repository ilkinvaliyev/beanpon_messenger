package websocket

import (
	"log"
	"time"
)

func (h *Hub) setUserOnline(userID uint) {
	go func() {
		sql := `
            INSERT INTO user_presences (user_id, is_online, updated_at)
            VALUES (?, true, ?)
            ON CONFLICT (user_id) DO UPDATE
            SET is_online = true, updated_at = EXCLUDED.updated_at
        `
		if err := h.db.Exec(sql, userID, time.Now().UTC()).Error; err != nil {
			log.Printf("⚠️ Presence online yazılamadı (user %d): %v", userID, err)
		}
	}()
}

func (h *Hub) setUserOffline(userID uint) {
	go func() {
		now := time.Now().UTC()
		sql := `
            INSERT INTO user_presences (user_id, is_online, last_seen_at, updated_at)
            VALUES (?, false, ?, ?)
            ON CONFLICT (user_id) DO UPDATE
            SET is_online = false, last_seen_at = EXCLUDED.last_seen_at, updated_at = EXCLUDED.updated_at
        `
		if err := h.db.Exec(sql, userID, now, now).Error; err != nil {
			log.Printf("⚠️ Presence offline yazılamadı (user %d): %v", userID, err)
		}
	}()
}
