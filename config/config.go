package config

import (
	"github.com/joho/godotenv"
	"log"
	"os"
)

// Config yapılandırma verilerini tutar
type Config struct {
	Port         string
	JWTSecret    string
	AESKey       string
	PostgresUser string
	PostgresPass string
	PostgresHost string
	PostgresPort string
	PostgresDB   string
}

// LoadConfig konfigürasyon dosyasını okur ve Config yapısına doldurur
func LoadConfig() *Config {
	// .env dosyasını yükleyin
	err := godotenv.Load()
	if err != nil {
		log.Println("Error loading .env file, continuing with environment variables")
	}

	cfg := &Config{
		Port:         os.Getenv("APP_PORT"),
		JWTSecret:    os.Getenv("JWT_SECRET"),
		PostgresUser: os.Getenv("DB_USER"),
		PostgresPass: os.Getenv("DB_PASSWORD"),
		PostgresHost: os.Getenv("DB_HOST"),
		PostgresPort: os.Getenv("DB_PORT"),
		PostgresDB:   os.Getenv("DB_NAME"),
		AESKey:       os.Getenv("AES_KEY"),
	}

	// Default values
	if cfg.Port == "" {
		cfg.Port = "8080"
	}
	if cfg.PostgresPort == "" {
		cfg.PostgresPort = "5432"
	}

	// Zorunlu alanların kontrolü (PostgreSQL için)
	if cfg.JWTSecret == "" || cfg.AESKey == "" || cfg.PostgresUser == "" || // ← AESKey ekle
		cfg.PostgresPass == "" || cfg.PostgresHost == "" ||
		cfg.PostgresDB == "" {
		log.Fatal("Some required PostgreSQL environment variables are missing!")
	}

	println(len(cfg.AESKey))
	// AES key uzunluk kontrolü ekle
	if len(cfg.AESKey) != 32 {
		log.Fatal("AES_KEY must be exactly 32 characters long!")
	}

	return cfg
}
