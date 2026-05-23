package websocket

import (
	"log"
	"time"
)

func (h *Hub) setUserOnline(userID uint) {
	go func() {
		// SELECT ... WHERE EXISTS: yalnız users cədvəlində real qeydi olan
		// user üçün INSERT edilir. Silinmiş ya da guest user (users-də qeyd
		// yoxdur) halında INSERT sakitcə atlanır — əvvəl bu hal
		// "user_presences_user_id_foreign violates" xətası verirdi.
		sql := `
            INSERT INTO user_presences (user_id, is_online, online_at, updated_at)
            SELECT ?, true, ?, ?
            WHERE EXISTS (SELECT 1 FROM users WHERE id = ?)
            ON CONFLICT (user_id) DO UPDATE
            SET is_online = true, online_at = EXCLUDED.online_at, updated_at = EXCLUDED.updated_at
        `
		now := time.Now().UTC()
		if err := h.db.Exec(sql, userID, now, now, userID).Error; err != nil {
			log.Printf("⚠️ Presence online yazılamadı (user %d): %v", userID, err)
		}
	}()
}

func (h *Hub) setUserOffline(userID uint) {
	go func() {
		now := time.Now().UTC()
		// SELECT ... WHERE EXISTS: silinmiş/guest user üçün INSERT atlanır
		// (foreign key xətasının qarşısını alır).
		sql := `
            INSERT INTO user_presences (user_id, is_online, last_seen_at, updated_at, total_online_seconds)
            SELECT ?, false, ?, ?, 0
            WHERE EXISTS (SELECT 1 FROM users WHERE id = ?)
            ON CONFLICT (user_id) DO UPDATE
            SET
                is_online = false,
                last_seen_at = EXCLUDED.last_seen_at,
                updated_at = EXCLUDED.updated_at,
                total_online_seconds = user_presences.total_online_seconds +
                    GREATEST(0, EXTRACT(EPOCH FROM (EXCLUDED.last_seen_at - user_presences.online_at))::bigint)
        `
		if err := h.db.Exec(sql, userID, now, now, userID).Error; err != nil {
			log.Printf("⚠️ Presence offline yazılamadı (user %d): %v", userID, err)
		}
	}()
}
