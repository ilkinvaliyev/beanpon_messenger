package middleware

import (
	"strconv"
	"strings"
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

// StartBandwidthWriter — main.go-da bir dəfə go ilə çağrılır.
func StartBandwidthWriter(db *gorm.DB) {
	const batchSize = 200
	const flushEvery = 3 * time.Second

	batch := make([]BandwidthRecord, 0, batchSize)
	ticker := time.NewTicker(flushEvery)
	defer ticker.Stop()

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
		}
	}
}

func writeBandwidthBatch(db *gorm.DB, batch []BandwidthRecord) {
	sess := db.Session(&gorm.Session{SkipDefaultTransaction: true})

	for _, r := range batch {
		sess.Exec(`INSERT INTO bandwidth_logs
			(user_id, method, path, category, source, status_code, bytes_sent, bytes_recv, is_range, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.UserID, r.Method, r.Path, r.Category, r.Source,
			r.StatusCode, r.BytesSent, r.BytesRecv, r.IsRange, r.CreatedAt,
		)

		day := r.CreatedAt.Format("2006-01-02")
		sess.Exec(`INSERT INTO bandwidth_daily
			(user_id, day, category, source, bytes_sent, bytes_recv, req_count)
			VALUES (?, ?, ?, ?, ?, ?, 1)
			ON CONFLICT (user_id, day, category, source)
			DO UPDATE SET
				bytes_sent = bandwidth_daily.bytes_sent + EXCLUDED.bytes_sent,
				bytes_recv = bandwidth_daily.bytes_recv + EXCLUDED.bytes_recv,
				req_count  = bandwidth_daily.req_count  + 1`,
			r.UserID, day, r.Category, r.Source, r.BytesSent, r.BytesRecv,
		)
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
