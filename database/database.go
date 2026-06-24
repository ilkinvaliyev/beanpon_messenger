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
	if sqlDB, sErr := db.DB(); sErr == nil {
		sqlDB.SetMaxOpenConns(25)                  // eyni anda maksimum açıq bağlantı
		sqlDB.SetMaxIdleConns(25)                  // boşda saxlanan bağlantı (yenidən açma yox)
		sqlDB.SetConnMaxIdleTime(5 * time.Minute)  // boş bağlantı nə qədər yaşasın
		sqlDB.SetConnMaxLifetime(30 * time.Minute) // bağlantı ömrü (DB tərəf timeout-larından qısa)
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
