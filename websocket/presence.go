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

		// Bu sessiya-nın başlanğıcı (online_at) — həm ümumi total, həm də
		// "Günün Kartı" gündəlik ekran vaxtı üçün lazımdır. Offline yazmadan
		// ÖNCƏ oxuyuruq (yazıdan sonra online_at dəyişmir, amma dəqiqlik üçün).
		var onlineAt *time.Time
		h.db.Raw(`SELECT online_at FROM user_presences WHERE user_id = ?`, userID).Scan(&onlineAt)

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

		// "Günün Kartı" gündəlik ekran vaxtı: bu sessiyanın müddətini (online_at
		// → now) Bakı günlərinə bölüb daily_card_stats.screen_seconds-ə əlavə et.
		if onlineAt != nil {
			h.addDailyScreenTime(userID, onlineAt.UTC(), now)
		}
	}()
}

// bakuLocation — Azərbaycan sabit UTC+4 (2016-dan sonra DST yoxdur).
func bakuLocation() *time.Location {
	return time.FixedZone("Asia/Baku", 4*3600)
}

// addDailyScreenTime bir online sessiyanın müddətini Bakı təqvim günlərinə
// bölüb hər gün üçün daily_card_stats.screen_seconds-ə əlavə edir (gecə yarısını
// keçən sessiya iki günə düzgün paylanır). Yalnız screen_seconds sütununa toxunur.
func (h *Hub) addDailyScreenTime(userID uint, start, end time.Time) {
	if !end.After(start) {
		return
	}
	baku := bakuLocation()
	cursor := start.In(baku)
	endBaku := end.In(baku)

	for cursor.Before(endBaku) {
		// Bu günün Bakı sonu (növbəti gün 00:00).
		dayStart := time.Date(cursor.Year(), cursor.Month(), cursor.Day(), 0, 0, 0, 0, baku)
		nextMidnight := dayStart.Add(24 * time.Hour)

		segmentEnd := endBaku
		if nextMidnight.Before(segmentEnd) {
			segmentEnd = nextMidnight
		}
		seconds := int64(segmentEnd.Sub(cursor).Seconds())
		if seconds > 0 {
			statDate := cursor.Format("2006-01-02")
			// Yalnız screen_seconds-i artır; server/cihaz digər sütunlara toxunmur.
			upsert := `
                INSERT INTO daily_card_stats (user_id, stat_date, screen_seconds, created_at, updated_at)
                SELECT ?, ?, ?, NOW(), NOW()
                WHERE EXISTS (SELECT 1 FROM users WHERE id = ?)
                ON CONFLICT (user_id, stat_date) DO UPDATE
                SET screen_seconds = daily_card_stats.screen_seconds + EXCLUDED.screen_seconds,
                    updated_at = NOW()
            `
			if err := h.db.Exec(upsert, userID, statDate, seconds, userID).Error; err != nil {
				log.Printf("⚠️ Günün Kartı ekran vaxtı yazılamadı (user %d, %s): %v", userID, statDate, err)
			}
		}
		cursor = nextMidnight
	}
}
