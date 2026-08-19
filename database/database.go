package database

import (
	"beanpon_messenger/config"
	"fmt"
	"log"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

var DB *gorm.DB

// InitializePostgreSQL PostgreSQL veritabanına bağlanır.
// PGBOUNCER_ENABLED=true olduqda DSN-ə "default_query_exec_mode=simple_protocol"
// əlavə olunur — pgbouncer-in transaction/statement pool mode-larında
// pgx-in prepared statement cache-i ilə bağlı xətaları aradan qaldırır.
func InitializePostgreSQL(cfg *config.Config) {
	dsn := fmt.Sprintf(
		"host=%s user=%s password=%s dbname=%s port=%s sslmode=disable TimeZone=UTC",
		cfg.PostgresHost,
		cfg.PostgresUser,
		cfg.PostgresPass,
		cfg.PostgresDB,
		cfg.PostgresPort,
	)

	if cfg.PgBouncerEnabled {
		dsn += " default_query_exec_mode=simple_protocol"
	}

	// ── statement_timeout ───────────────────────────────────────────────────
	// Əvvəl HEÇ TƏYİN OLUNMAMIŞDI. İlişən bir sorğu hovuzdakı bağlantını
	// SONSUZA qədər tuturdu; `handlers/conversation_handler.go:136-140`-dakı
	// şərh məhz bu səbəbdən yaranan kilidlənməni ("kilid ƏBƏDİDİR") təsvir
	// edir. İndi server tərəfdə kəsilir → bağlantı hovuza qayıdır.
	//
	// Dəyər ehtiyatlı seçilib (default 15 s): `GetConversations` hazırda
	// 1–4 s sürə bilir. Həmin sorğu yenidən yazıldıqdan sonra 5 s-ə çəkiləcək.
	// `DB_STATEMENT_TIMEOUT_MS=0` ilə tamamilə söndürülə bilər.
	if cfg.DB.StatementTimeoutMS > 0 {
		dsn += fmt.Sprintf(" statement_timeout=%d", cfg.DB.StatementTimeoutMS)
	}

	gormCfg := &gorm.Config{
		PrepareStmt: false,
	}

	db, err := gorm.Open(postgres.Open(dsn), gormCfg)
	if err != nil {
		log.Fatalf("PostgreSQL bağlantı hatası: %v", err)
	}

	// 🔧 Bağlantı havuzu (connection pool) ayarları.
	// DB uzaq makinədədir (DB_HOST fərqli host) — hər sorğu üçün yeni TCP
	// bağlantısı açmaq bahalıdır (~100ms RTT ölçüldü). Pool sayəsində açıq
	// bağlantılar təkrar istifadə olunur, latency dəfələrlə azalır.
	//
	// ── HOVUZ ARTIQ SABİT KODDA DEYİL ───────────────────────────────────────
	// `MaxOpenConns(25)` bütün prosesin eyni andakı sorğu TAVANI idi. Bir DM
	// göndərmək ~12 gediş-dönüş tələb etdiyinə görə 25 bağlantı yük altında
	// birbaşa növbəyə çevrilirdi. Env ilə tənzimlənir; default 50.
	//
	// DİQQƏT: bu dəyər PostgreSQL `max_connections`-dan (bütün replica-lar +
	// Laravel + pgbouncer daxil) KİÇİK olmalıdır. Artırmadan əvvəl
	// `SHOW max_connections;` yoxlayın. Geri qayıtmaq üçün: DB_MAX_OPEN_CONNS=25.
	if sqlDB, sErr := db.DB(); sErr == nil {
		maxOpen := cfg.DB.MaxOpenConns
		if maxOpen <= 0 {
			maxOpen = 25 // köhnə davranış
		}
		maxIdle := cfg.DB.MaxIdleConns
		if maxIdle <= 0 || maxIdle > maxOpen {
			maxIdle = maxOpen
		}
		sqlDB.SetMaxOpenConns(maxOpen)             // eyni anda maksimum açıq bağlantı
		sqlDB.SetMaxIdleConns(maxIdle)             // boşda saxlanan bağlantı (yenidən açma yox)
		sqlDB.SetConnMaxIdleTime(5 * time.Minute)  // boş bağlantı nə qədər yaşasın
		sqlDB.SetConnMaxLifetime(30 * time.Minute) // bağlantı ömrü (DB tərəf timeout-larından qısa)
		log.Printf("PostgreSQL hovuzu: maxOpen=%d maxIdle=%d statementTimeoutMs=%d",
			maxOpen, maxIdle, cfg.DB.StatementTimeoutMS)
	} else {
		log.Printf("⚠️ connection pool ayarlanamadı: %v", sErr)
	}

	DB = db

	if cfg.PgBouncerEnabled {
		log.Println("PostgreSQL bağlantısı başarılı (pgbouncer mode)!")
	} else {
		log.Println("PostgreSQL bağlantısı başarılı!")
	}
}

// GetDB veritabanı bağlantısını döndürür
func GetDB() *gorm.DB {
	return DB
}
