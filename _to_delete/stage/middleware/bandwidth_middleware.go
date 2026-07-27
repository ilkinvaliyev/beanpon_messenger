package middleware

import (
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// ============================================================
// Per-user bandwidth tracking (messenger).
//
// Əsas backend (piokio_golang_main) ilə EYNİ cədvələ yazır:
// bandwidth_logs + bandwidth_daily. `source = "messenger"` sütunu bu
// servisi digərlərindən ayırır (ikiqat sayma yox). Eyni Postgres DB-dir
// (DB_NAME/DB_HOST env).
//
// Qoşulma (cmd/main/main.go):
//   go middleware.StartBandwidthWriter(database.GetDB())   // DB init-dən sonra
//   router.Use(middleware.BandwidthMiddleware())           // route-lardan əvvəl
//
// QEYD: WebSocket route-ları (/ws*) ölçülmür — uzun-ömürlü bağlantılarda
// c.Writer.Size() mənalı deyil.
// ============================================================
//
// ── Issue 61 — SƏSSİZ UĞURSUZLUQ ────────────────────────────────────────────
//
// Əvvəlki yazıcı hər qeyd üçün iki `sess.Exec(...)` çağırırdı və HƏR İKİSİNİN
// qaytardığı xətanı TAMAMİLƏ ATIRDI (`Exec`-in nəticəsi heç yerə yazılmırdı).
// Praktik nəticələri:
//
//   • `bandwidth_logs` / `bandwidth_daily` cədvəli yoxdursa, sütun adı
//     dəyişibsə, və ya `ON CONFLICT (user_id, day, category, source)` üçün
//     UNİKAL İNDEKS yoxdursa (hədəfli ON CONFLICT indekssiz Postgres-də
//     DƏRHAL xəta verir) — bütün yazılar SONSUZA QƏDƏR uğursuz olur və heç
//     bir log, heç bir metrik, heç bir əlamət qalmır. Trafik hesabatı ölür,
//     amma "işləyir" görünür. Bu problem yalnız kimsə "niyə rəqəmlər 0?"
//     deyə soruşanda — aylar sonra — aşkarlanırdı.
//
//   • Kanal dolduqda (`default:` budağı) qeydlər səssizcə ATILIRDI. Yük
//     altında hesabat nə qədər əskik saydığını heç kim bilmirdi.
//
// İndi: xətalar (throttle ilə, log seli olmadan) loglanır, atılan qeyd sayı
// sayılır və dövri olaraq bildirilir; üstəlik yazılar TOPLU (multi-row) hala
// gətirilib — 200 qeyd üçün 400 gediş əvəzinə 2 gediş.

const mediaSampleMsg = 5 // messenger-də media yoxdur, amma simvolik saxlanır

type BandwidthRecord struct {
	UserID     *int64
	Method     string
	Path       string
	Category   string
	Source     string
	StatusCode int
	BytesSent  int64
	BytesRecv  int64
	IsRange    bool
	CreatedAt  time.Time
}

var bandwidthChan = make(chan BandwidthRecord, 4096)

// bandwidthDropped — kanal dolu olduğu üçün atılan qeydlərin sayı (Issue 61).
var bandwidthDropped atomic.Int64

// bandwidthAnonSkipped — `user_id` NULL olduğu üçün bandwidth_daily-ə
// yazılmayan sorğuların sayı (bandwidth_logs-da tam qalır). Görünsün deyə
// dövri hesabata daxil edilir.
var bandwidthAnonSkipped atomic.Int64

// bandwidthLogThrottle — eyni xətanın log-u basmaması üçün minimal interval.
const bandwidthLogThrottle = 60 * time.Second

var (
	lastBandwidthErrLog atomic.Int64 // unix nano
	bandwidthErrCount   atomic.Int64
)

// logBandwidthErr — xətanı throttle ilə loglayır. Aralıqda baş verən xətalar
// sayılır və növbəti log sətrində "N xəta" kimi göstərilir; beləliklə problem
// GÖRÜNÜR, amma log seli yaranmır.
func logBandwidthErr(op string, err error) {
	if err == nil {
		return
	}
	total := bandwidthErrCount.Add(1)

	now := time.Now().UnixNano()
	last := lastBandwidthErrLog.Load()
	if last != 0 && now-last < int64(bandwidthLogThrottle) {
		return
	}
	if !lastBandwidthErrLog.CompareAndSwap(last, now) {
		return // başqa goroutine loglayır
	}
	log.Printf("bandwidth: %s yazma xətası (indiyə qədər %d xəta): %v", op, total, err)
}

// StartBandwidthWriter — main.go-da bir dəfə go ilə çağrılır.
func StartBandwidthWriter(db *gorm.DB) {
	const batchSize = 200
	const flushEvery = 3 * time.Second

	batch := make([]BandwidthRecord, 0, batchSize)
	ticker := time.NewTicker(flushEvery)
	defer ticker.Stop()

	// Atılan qeydlər üçün ayrı, daha seyrək hesabat.
	dropTicker := time.NewTicker(5 * time.Minute)
	defer dropTicker.Stop()
	var lastReportedDrops int64

	flush := func() {
		if len(batch) == 0 {
			return
		}
		writeBandwidthBatch(db, batch)
		batch = batch[:0]
	}

	for {
		select {
		case rec := <-bandwidthChan:
			batch = append(batch, rec)
			if len(batch) >= batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-dropTicker.C:
			// Issue 61: səssiz itki artıq görünür.
			if d := bandwidthDropped.Load(); d > lastReportedDrops {
				log.Printf("bandwidth: kanal dolu olduğu üçün %d qeyd atıldı (cəmi %d, anonim gündəlik atlanan %d) — yazıcı yükə çatmır",
					d-lastReportedDrops, d, bandwidthAnonSkipped.Load())
				lastReportedDrops = d
			}
		}
	}
}

// dailyKey — bandwidth_daily unikal açarı (yaddaşda toplama üçün).
type dailyKey struct {
	userID   int64
	day      string
	category string
	source   string
}

func writeBandwidthBatch(db *gorm.DB, batch []BandwidthRecord) {
	if len(batch) == 0 {
		return
	}
	sess := db.Session(&gorm.Session{SkipDefaultTransaction: true})

	// ── 1) bandwidth_logs — TƏK multi-row INSERT ────────────────────────────
	// Əvvəl qeyd başına ayrıca `Exec` idi (200 qeyd = 200 gediş, hər biri
	// hovuzdan bağlantı tutur). 200 × 10 = 2000 bind parametri — Postgres-in
	// 65535 limitindən çox aşağıdadır.
	placeholders := make([]string, 0, len(batch))
	args := make([]interface{}, 0, len(batch)*10)
	for _, r := range batch {
		placeholders = append(placeholders, "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
		args = append(args,
			r.UserID, r.Method, r.Path, r.Category, r.Source,
			r.StatusCode, r.BytesSent, r.BytesRecv, r.IsRange, r.CreatedAt,
		)
	}
	const logsCols = `INSERT INTO bandwidth_logs
		(user_id, method, path, category, source, status_code, bytes_sent, bytes_recv, is_range, created_at)
		VALUES `
	if err := sess.Exec(logsCols+strings.Join(placeholders, ","), args...).Error; err != nil {
		logBandwidthErr("bandwidth_logs (toplu)", err)
		// Toplu INSERT bir pozuq sətir ucbatından BÜTÜN partiyanı itirir
		// (köhnə sətir-bə-sətir yazıcı yalnız pozuq sətri itirirdi). Ona görə
		// uğursuzluqda BİR DƏFƏ sətir-bə-sətir təkrar cəhd edirik: yalnız
		// həqiqətən problemli qeyd(lər) düşür.
		var perRowFailures int
		for _, r := range batch {
			if e := sess.Exec(logsCols+"(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
				r.UserID, r.Method, r.Path, r.Category, r.Source,
				r.StatusCode, r.BytesSent, r.BytesRecv, r.IsRange, r.CreatedAt,
			).Error; e != nil {
				perRowFailures++
			}
		}
		if perRowFailures > 0 {
			logBandwidthErr(fmt.Sprintf("bandwidth_logs (sətir-bə-sətir, %d/%d düşdü)", perRowFailures, len(batch)),
				errors.New("toplu yazı uğursuz oldu, təkrar cəhd qismən keçdi"))
		}
	}

	// ── 2) bandwidth_daily — əvvəlcə YADDAŞDA topla, sonra tək upsert ───────
	// Eyni (user, gün, kateqoriya, mənbə) açarı bir partiyada onlarla dəfə
	// təkrarlanır; yaddaşda toplamaq həm gediş sayını, həm də eyni sətir
	// üzərindəki kilid rəqabətini kəskin azaldır.
	type dailyAgg struct {
		bytesSent int64
		bytesRecv int64
		count     int64
	}
	agg := make(map[dailyKey]*dailyAgg, len(batch))
	order := make([]dailyKey, 0, len(batch))

	for _, r := range batch {
		// user_id NULL olan sətirlər üçün upsert etmirik: Postgres-də NULL
		// dəyərlər unikal indeksdə BİR-BİRİNDƏN FƏRQLİ sayılır, ona görə
		// hər anonim sorğu bandwidth_daily-də YENİ sətir yaradardı —
		// cədvəl sonsuz böyüyür və gündəlik hesabat mənasızlaşır.
		// Anonim trafik bandwidth_logs-da tam olaraq qalır.
		if r.UserID == nil {
			bandwidthAnonSkipped.Add(1)
			continue
		}
		k := dailyKey{
			userID:   *r.UserID,
			day:      r.CreatedAt.Format("2006-01-02"),
			category: r.Category,
			source:   r.Source,
		}
		cur, ok := agg[k]
		if !ok {
			cur = &dailyAgg{}
			agg[k] = cur
			order = append(order, k)
		}
		cur.bytesSent += r.BytesSent
		cur.bytesRecv += r.BytesRecv
		cur.count++
	}

	if len(order) == 0 {
		return
	}

	dPlaceholders := make([]string, 0, len(order))
	dArgs := make([]interface{}, 0, len(order)*7)
	for _, k := range order {
		v := agg[k]
		dPlaceholders = append(dPlaceholders, "(?, ?, ?, ?, ?, ?, ?)")
		dArgs = append(dArgs, k.userID, k.day, k.category, k.source, v.bytesSent, v.bytesRecv, v.count)
	}
	dailySQL := `INSERT INTO bandwidth_daily
		(user_id, day, category, source, bytes_sent, bytes_recv, req_count)
		VALUES ` + strings.Join(dPlaceholders, ",") + `
		ON CONFLICT (user_id, day, category, source)
		DO UPDATE SET
			bytes_sent = bandwidth_daily.bytes_sent + EXCLUDED.bytes_sent,
			bytes_recv = bandwidth_daily.bytes_recv + EXCLUDED.bytes_recv,
			req_count  = bandwidth_daily.req_count  + EXCLUDED.req_count`
	if err := sess.Exec(dailySQL, dArgs...).Error; err != nil {
		// DİQQƏT: hədəfli `ON CONFLICT (...)` uyğun UNİKAL İNDEKS olmadan
		// Postgres-də dərhal xəta verir. Əvvəl bu xəta udulurdu və
		// bandwidth_daily HEÇ VAXT dolmurdu.
		logBandwidthErr(fmt.Sprintf("bandwidth_daily (%d sətir)", len(order)), err)
	}
}

func BandwidthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path

		// OPTIONS, health, və WebSocket route-larını ölçmə.
		if c.Request.Method == "OPTIONS" ||
			path == "/health" ||
			strings.HasPrefix(path, "/ws") {
			c.Next()
			return
		}

		start := time.Now()
		c.Next()

		bytesSent := int64(c.Writer.Size())
		if bytesSent < 0 {
			bytesSent = 0
		}
		bytesRecv := c.Request.ContentLength
		if bytesRecv < 0 {
			bytesRecv = 0
		}

		category, normPath := classifyBandwidthPath(path)

		if strings.HasPrefix(category, "media_") {
			if start.UnixNano()%mediaSampleMsg != 0 {
				return
			}
			bytesSent *= mediaSampleMsg
			bytesRecv *= mediaSampleMsg
		}

		var uid *int64
		if v, ok := c.Get("user_id"); ok {
			switch t := v.(type) {
			case uint:
				id := int64(t)
				uid = &id
			case int64:
				id := t
				uid = &id
			case int:
				id := int64(t)
				uid = &id
			}
		}

		rec := BandwidthRecord{
			UserID:     uid,
			Method:     c.Request.Method,
			Path:       normPath,
			Category:   category,
			Source:     "messenger",
			StatusCode: c.Writer.Status(),
			BytesSent:  bytesSent,
			BytesRecv:  bytesRecv,
			IsRange:    c.Writer.Status() == 206,
			CreatedAt:  start,
		}

		select {
		case bandwidthChan <- rec:
		default:
			// Issue 61: səssiz atma yerinə sayğac — StartBandwidthWriter
			// dövri olaraq bunu loglayır.
			bandwidthDropped.Add(1)
		}
	}
}

func classifyBandwidthPath(path string) (category, norm string) {
	if strings.HasPrefix(path, "/api/s3-storage/") {
		ext := ""
		if i := strings.LastIndex(path, "."); i != -1 {
			ext = strings.ToLower(path[i+1:])
		}
		switch ext {
		case "jpg", "jpeg", "png", "webp", "gif",
			"heic", "heif", "jfif", "bmp", "tiff", "tif", "svg", "avif":
			return "media_image", "/api/s3-storage/*"
		case "mp4", "webm", "mov",
			"mkv", "avi", "m4v", "3gp", "flv", "ts", "mpeg", "mpg":
			return "media_video", "/api/s3-storage/*"
		case "mp3", "m4a", "aac", "wav",
			"ogg", "opus", "amr", "flac", "weba", "caf":
			return "media_audio", "/api/s3-storage/*"
		default:
			return "media_other", "/api/s3-storage/*"
		}
	}

	parts := strings.Split(path, "/")
	for i, p := range parts {
		if p == "" {
			continue
		}
		if _, err := strconv.Atoi(p); err == nil {
			parts[i] = ":id"
		}
	}
	norm = strings.Join(parts, "/")

	if strings.HasPrefix(path, "/api/") {
		return "api", norm
	}
	return "other", norm
}
