package database

import (
	"beanpon_messenger/config"
	"fmt"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"log"
)

var DB *gorm.DB

// InitializePostgreSQL PostgreSQL veritabanına bağlanır
func InitializePostgreSQL(cfg *config.Config) {
	// PostgreSQL bağlantı string'i oluştur
	dsn := fmt.Sprintf(
		"host=%s user=%s password=%s dbname=%s port=%s sslmode=disable TimeZone=UTC",
		cfg.PostgresHost,
		cfg.PostgresUser,
		cfg.PostgresPass,
		cfg.PostgresDB,
		cfg.PostgresPort,
	)

	// GORM ile bağlan
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatalf("PostgreSQL bağlantı hatası: %v", err)
	}

	// Global DB değişkenine ata
	DB = db

	log.Println("PostgreSQL bağlantısı başarılı!")
}

// GetDB veritabanı bağlantısını döndürür
func GetDB() *gorm.DB {
	return DB
}
