package handlers

import (
	"context"
	"strconv"
	"time"

	"beanpon_messenger/cache"
	"beanpon_messenger/database"
)

// Soft-throttle: users.bad_traffic = true olan istifadəçilərin sorğularına
// qəsdən gecikmə əlavə edir. Flag Laravel admin panelindən idarə olunur; status
// paylaşılan Redis-də (bp:shared:bad_traffic:{id}) keşlənir — Filament dəyişəndə
// dərhal yazır, burada cache miss-də DB-dən doldurulur (1 saat TTL).

var (
	badTrafficDelay    time.Duration
	badTrafficCacheTTL = time.Hour
)

// InitBadTraffic — main-dən config-i bağlayır (delaySeconds=0 → söndürülü).
func InitBadTraffic(delaySeconds, cacheSeconds int) {
	if delaySeconds > 0 {
		badTrafficDelay = time.Duration(delaySeconds) * time.Second
	}
	if cacheSeconds > 0 {
		badTrafficCacheTTL = time.Duration(cacheSeconds) * time.Second
	}
}

// throttleBadTraffic — user bad_traffic flag-ındadırsa gecikmə tətbiq edir.
func throttleBadTraffic(userID int64) {
	if badTrafficDelay <= 0 {
		return
	}
	if isBadTraffic(userID) {
		time.Sleep(badTrafficDelay)
	}
}

// isBadTraffic — əvvəlcə paylaşılan Redis cache, miss-də DB → cache doldur.
func isBadTraffic(userID int64) bool {
	key := "bp:shared:bad_traffic:" + strconv.FormatInt(userID, 10)
	ctx := context.Background()
	rc := cache.GetClient()

	if rc != nil && rc.Enabled() {
		if v, ok, err := rc.Get(ctx, key); err == nil && ok {
			return v == "1"
		}
	}

	var bad bool
	if err := database.GetDB().
		Raw("SELECT bad_traffic FROM users WHERE id = ?", userID).
		Scan(&bad).Error; err != nil {
		return false // fail-open
	}

	if rc != nil && rc.Enabled() {
		val := "0"
		if bad {
			val = "1"
		}
		_ = rc.Set(ctx, key, val, badTrafficCacheTTL)
	}
	return bad
}
