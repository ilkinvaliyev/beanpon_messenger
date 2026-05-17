package database

import (
	"beanpon_messenger/config"
	"fmt"
	"log"

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
