package websocket

import (
	"log"
	"time"

	"beanpon_messenger/database"
)

// addDailyLiveTime — bir canlı otaq (live room) sessiyasının müddətini
// istifadəçinin Günün Kartındakı günlük `live_seconds` sütununa əlavə edir.
//
// Host / broadcaster / audience fərq etməz — hamı eyni WS axını ilə otağa
// qoşulur (LiveHub.Register) və ayrılır (Unregister); bu funksiya Unregister-də
// çağırılır. Presence → screen_seconds ilə eyni məntiq: sessiya iki günə
// (Bakı vaxtı) düşürsə hər günə öz payı yazılır ki, istifadəçi öz yaşadığı
// günə görə doğru dəyəri görsün. Yalnız `live_seconds` sütununa toxunur
// (ON CONFLICT DO UPDATE) — server/cihaz sütunları toxunulmur.
//
// Timezone: DB UTC saxlayır; Bakı sabit UTC+4-dür (2016-dan sonra DST yox).
func addDailyLiveTime(userID uint, joinedAt, leftAt time.Time) {
	if userID == 0 {
		return
	}
	// Etibarsız / boş sessiya (JoinedAt heç təyin olunmayıb, ya da tərs sıra).
	if joinedAt.IsZero() || !leftAt.After(joinedAt) {
		return
	}

	go func() {
		baku := time.FixedZone("Asia/Baku", 4*3600)
		start := joinedAt.UTC()
		end := leftAt.UTC()

		// Sessiyanı Bakı gün sərhədlərində parçala (adətən bir gün, gecə yarısını
		// keçərsə iki gün). Hər parça öz statDate-inin sətrinə yazılır.
		cur := start
		for cur.Before(end) {
			// cur-un aid olduğu Bakı gününün sonu (UTC-də).
			curBaku := cur.In(baku)
			dayStartBaku := time.Date(curBaku.Year(), curBaku.Month(), curBaku.Day(), 0, 0, 0, 0, baku)
			nextDayStartUTC := dayStartBaku.Add(24 * time.Hour).UTC()

			segEnd := end
			if nextDayStartUTC.Before(segEnd) {
				segEnd = nextDayStartUTC
			}

			seconds := int(segEnd.Sub(cur).Seconds())
			if seconds > 0 {
				statDate := dayStartBaku.Format("2006-01-02")
				upsertDailyLiveSeconds(userID, statDate, seconds)
			}
			cur = segEnd
		}
	}()
}

// upsertDailyLiveSeconds — daily_card_stats sətrinin `live_seconds`-una `seconds`
// əlavə edir (yalnız bu sütun). Sətir yoxdursa yaradılır. WHERE EXISTS ilə
// silinmiş/guest user üçün INSERT atlanır (foreign key xətasının qarşısı).
func upsertDailyLiveSeconds(userID uint, statDate string, seconds int) {
	sql := `
        INSERT INTO daily_card_stats (user_id, stat_date, live_seconds, created_at, updated_at)
        SELECT ?, ?, ?, NOW(), NOW()
        WHERE EXISTS (SELECT 1 FROM users WHERE id = ?)
        ON CONFLICT (user_id, stat_date) DO UPDATE
        SET live_seconds = daily_card_stats.live_seconds + EXCLUDED.live_seconds,
            updated_at = NOW()
    `
	if err := database.DB.Exec(sql, userID, statDate, seconds, userID).Error; err != nil {
		log.Printf("⚠️ [daily_card] live_seconds yazılamadı (user %d, %s): %v", userID, statDate, err)
	}
}
