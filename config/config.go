package config

import (
	"github.com/joho/godotenv"
	"log"
	"os"
	"strconv"
	"time"
)

// Config yapılandırma verilerini tutar
type Config struct {
	Port           string
	JWTSecret      string
	AESKey         string
	PostgresUser   string
	PostgresPass   string
	PostgresHost   string
	PostgresPort   string
	PostgresDB     string
	CloudToken     string
	BackendUrl     string
	InternalSecret string

	// OpenAIAPIKey — arxa-plan mesaj moderasiyası (gpt-4o-mini) üçün açar.
	// Boş olduqda moderasiya sakitcə deaktiv olur — tətbiq normal işləyir.
	OpenAIAPIKey string

	// Telegram — moderasiya off_platform aşkarlanmaları üçün xəbərdarlıq botu.
	// TelegramBotToken boş olduqda Telegram bildirişi sakitcə deaktiv olur.
	// Hər iki dəyər .env-dən oxunur; verilmədikdə default-lara düşür.
	TelegramBotToken string
	TelegramChatID   string

	// PgBouncer — true olduqda DSN-ə "default_query_exec_mode=simple_protocol"
	// əlavə olunur ki, pgbouncer transaction/statement mode-da pgx-in
	// prepared statement cache-i ilə bağlı xətalar olmasın.
	PgBouncerEnabled bool

	// Cache — Redis layeri (Laravel ilə paylaşılan spam_ban, və s.).
	// Disable olunsa belə tətbiq işləyir — bütün cache çağırışları no-op olur,
	// DB-yə düşür.
	Cache CacheConfig
}

// CacheConfig — Redis cache layeri üçün konfiqurasiya. piokio_golang_main
// dakı CacheConfig ilə eyni sxemə uyğundur — eyni Redis instance-ı və eyni
// `bp:shared:` prefiksi istifadə olunur ki, Laravel ilə key namespace ortaq
// qalsın.
type CacheConfig struct {
	Enabled bool

	Host     string
	Port     string
	Password string

	// ReadHost/ReadPort — oxuma sorğuları üçün ayrı Redis (lokal replica).
	// Boş qoyularsa Host/Port-a düşür, yəni köhnə davranış (master-dən oxu).
	// Yazma həmişə Host/Port (master) üzərindən gedir.
	ReadHost string
	ReadPort string

	// SharedPrefix — Laravel ilə paylaşılan key-lər (spam_ban, user və s.).
	// Default: "bp:shared:".
	SharedPrefix string

	// LocalPrefix — yalnız messenger daxili key-lər. Hələlik istifadə yoxdur
	// amma gələcəkdə (məs. message draft cache) lazım ola bilər.
	LocalPrefix string

	PoolSize     int
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	MaxRetries   int

	// Circuit breaker — Redis sıxıntıda olduqda fail fast.
	BreakerThreshold int
	BreakerCooldown  time.Duration
}

// LoadConfig konfigürasyon dosyasını okur ve Config yapısına doldurur
func LoadConfig() *Config {
	// .env dosyasını yükleyin
	err := godotenv.Load()
	if err != nil {
		log.Println("Error loading .env file, continuing with environment variables")
	}

	cfg := &Config{
		Port:             os.Getenv("APP_PORT"),
		JWTSecret:        os.Getenv("JWT_SECRET"),
		PostgresUser:     os.Getenv("DB_USER"),
		PostgresPass:     os.Getenv("DB_PASSWORD"),
		PostgresHost:     os.Getenv("DB_HOST"),
		PostgresPort:     os.Getenv("DB_PORT"),
		PostgresDB:       os.Getenv("DB_NAME"),
		AESKey:           os.Getenv("AES_KEY"),
		CloudToken:       os.Getenv("CLOUD_TOKEN"),
		BackendUrl:       os.Getenv("BACKEND_URL"),
		InternalSecret:   os.Getenv("INTERNAL_SECRET"),
		OpenAIAPIKey:     os.Getenv("OPENAI_API_KEY"),
		TelegramBotToken: envStr("TELEGRAM_BOT_TOKEN", "8862168493:AAH2WPoYyUgbIEmolXkCJ3JAaKh3aP53fBE"),
		TelegramChatID:   envStr("TELEGRAM_CHAT_ID", "739452673"),
		PgBouncerEnabled: envBool("PGBOUNCER_ENABLED", false),
		Cache: CacheConfig{
			Enabled:          envBool("REDIS_ENABLED", true),
			Host:             envStr("REDIS_HOST", "127.0.0.1"),
			Port:             envStr("REDIS_PORT", "6379"),
			ReadHost:         envStr("REDIS_READ_HOST", ""),
			ReadPort:         envStr("REDIS_READ_PORT", ""),
			Password:         envRedisPassword("REDIS_PASSWORD"),
			SharedPrefix:     envStr("REDIS_SHARED_PREFIX", "bp:shared:"),
			LocalPrefix:      envStr("REDIS_LOCAL_PREFIX", "bp:msg:"),
			PoolSize:         envInt("REDIS_POOL_SIZE", 20),
			DialTimeout:      envDuration("REDIS_DIAL_TIMEOUT", 2*time.Second),
			ReadTimeout:      envDuration("REDIS_READ_TIMEOUT", 500*time.Millisecond),
			WriteTimeout:     envDuration("REDIS_WRITE_TIMEOUT", 500*time.Millisecond),
			MaxRetries:       envInt("REDIS_MAX_RETRIES", 2),
			BreakerThreshold: envInt("REDIS_BREAKER_THRESHOLD", 10),
			BreakerCooldown:  envDuration("REDIS_BREAKER_COOLDOWN", 30*time.Second),
		},
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
		cfg.PostgresPass == "" || cfg.PostgresHost == "" || cfg.CloudToken == "" || cfg.BackendUrl == "" ||
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

func envBool(key string, defaultVal bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return defaultVal
}

func envStr(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

// envRedisPassword — Laravel-style "null" sentinel-ini boş string-ə çevirir.
// .env-də REDIS_PASSWORD=null olduqda Redis-ə "null" string-i göndərilməsin
// (piokio_golang_main ilə eyni davranış).
func envRedisPassword(key string) string {
	v := os.Getenv(key)
	if v == "" || v == "null" {
		return ""
	}
	return v
}

func envInt(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return defaultVal
}

func envDuration(key string, defaultVal time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return defaultVal
}
