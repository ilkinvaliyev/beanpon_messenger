// Package cache — beanpon_messenger üçün Redis cache layeri.
//
// Dizayn prinsipi: Redis öndə dayanır, problemli olduqda bütün
// çağırışlar səssizcə DB-yə düşür. Bu paketdə yazılan funksiyalar
// **heç vaxt panic atmır**, hər çağırışda timeout və circuit breaker
// var. Redis itsə də app işləməyə davam edir.
//
// piokio_golang_main daxili cache layeri ilə eyni sxemə uyğundur —
// həm key prefiksi (`bp:shared:`) həm də davranış eyni olmalıdır ki,
// Laravel tərəfi ilə key namespace ortaq qalsın.
package cache

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/go-redis/redis/v8"

	"beanpon_messenger/config"
)

// Client — go-redis wrapper. SharedPrefix (Laravel ilə ortaq) və LocalPrefix
// (yalnız messenger daxili) iki prefiksi dəstəkləyir.
type Client struct {
	rdb     *redis.Client // master — yazma əməliyyatları
	readRdb *redis.Client // oxuma — lokal replica (təyin olunmayıbsa rdb-yə bərabər)
	enabled bool

	sharedPrefix string
	localPrefix  string

	breaker *Breaker

	readTimeout  time.Duration
	writeTimeout time.Duration
}

// Singleton — bütün handler/service-lər bu paket-səviyyəli instance-a baxır.
var globalClient *Client

// Initialize — main.go-da bir dəfə çağırılır.
// Redis çatmırsa belə client qayıdır (sadəcə enabled=false), boot dayanmır.
func Initialize(cfg config.CacheConfig) *Client {
	if !cfg.Enabled {
		log.Println("cache: REDIS_ENABLED=false — Redis söndürülüb, DB-only rejim")
		globalClient = &Client{
			enabled:      false,
			sharedPrefix: cfg.SharedPrefix,
			localPrefix:  cfg.LocalPrefix,
			breaker:      NewBreaker(cfg.BreakerThreshold, cfg.BreakerCooldown),
		}
		return globalClient
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:         fmt.Sprintf("%s:%s", cfg.Host, cfg.Port),
		Password:     cfg.Password,
		DB:           0, // prefix yolu — DB ayrılması yox (Laravel ilə paylaşılır)
		PoolSize:     cfg.PoolSize,
		DialTimeout:  cfg.DialTimeout,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		MaxRetries:   cfg.MaxRetries,
	})

	// readRdb — oxuma üçün lokal replica. ReadHost boşdursa master-in özünə
	// düşür (köhnə davranış). Beləcə replica yoxdursa heç nə dəyişmir.
	readRdb := rdb
	if cfg.ReadHost != "" {
		readPort := cfg.ReadPort
		if readPort == "" {
			readPort = cfg.Port
		}
		readRdb = redis.NewClient(&redis.Options{
			Addr:         fmt.Sprintf("%s:%s", cfg.ReadHost, readPort),
			Password:     cfg.Password,
			DB:           0,
			PoolSize:     cfg.PoolSize,
			DialTimeout:  cfg.DialTimeout,
			ReadTimeout:  cfg.ReadTimeout,
			WriteTimeout: cfg.WriteTimeout,
			MaxRetries:   cfg.MaxRetries,
		})
	}

	c := &Client{
		rdb:          rdb,
		readRdb:      readRdb,
		enabled:      true,
		sharedPrefix: cfg.SharedPrefix,
		localPrefix:  cfg.LocalPrefix,
		breaker:      NewBreaker(cfg.BreakerThreshold, cfg.BreakerCooldown),
		readTimeout:  cfg.ReadTimeout,
		writeTimeout: cfg.WriteTimeout,
	}

	// Boot-da ping et. Uğursuz olsa belə client qayıdır — sadəcə circuit
	// breaker ilk error-ları udacaq.
	pingCtx, cancel := context.WithTimeout(context.Background(), cfg.DialTimeout)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		log.Printf("cache: boot ping uğursuz: %v — app yenə də ayağa qalxır, DB fallback aktivdir", err)
	} else {
		readAddr := fmt.Sprintf("%s:%s", cfg.Host, cfg.Port)
		if cfg.ReadHost != "" {
			readAddr = fmt.Sprintf("%s:%s", cfg.ReadHost, cfg.ReadPort)
		}
		log.Printf("cache: Redis bağlantısı quruldu — write=%s:%s read=%s (shared=%q local=%q)",
			cfg.Host, cfg.Port, readAddr, cfg.SharedPrefix, cfg.LocalPrefix)
	}

	globalClient = c
	return c
}

// GetClient — singleton-a giriş. Hələ Initialize çağırılmayıbsa nil qaytarır.
// Helper funksiyaları bu nil-ı yoxlayır və no-op olur.
func GetClient() *Client {
	return globalClient
}

// Enabled — Redis layer-i aktiv olduğunu qaytarır.
func (c *Client) Enabled() bool {
	if c == nil {
		return false
	}
	return c.enabled
}

// Close — shutdown zamanı çağırılır.
func (c *Client) Close() error {
	if c == nil || c.rdb == nil {
		return nil
	}
	// readRdb ayrı bir client-dirsə onu da bağla (rdb-yə bərabər deyilsə).
	if c.readRdb != nil && c.readRdb != c.rdb {
		_ = c.readRdb.Close()
	}
	return c.rdb.Close()
}

// Ping — health endpoint üçün.
func (c *Client) Ping(ctx context.Context) error {
	if !c.Enabled() {
		return errors.New("cache: disabled")
	}
	pingCtx, cancel := context.WithTimeout(ctx, c.readTimeout)
	defer cancel()
	return c.rdb.Ping(pingCtx).Err()
}

// BreakerState — health endpoint və monitorinq üçün.
func (c *Client) BreakerState() string {
	if c == nil || c.breaker == nil {
		return "disabled"
	}
	return c.breaker.State()
}

// SharedKey — Laravel ilə paylaşılan key (bp:shared: + suffix).
func (c *Client) SharedKey(suffix string) string {
	if c == nil {
		return suffix
	}
	return c.sharedPrefix + suffix
}

// LocalKey — messenger daxili key (bp:msg: + suffix).
func (c *Client) LocalKey(suffix string) string {
	if c == nil {
		return suffix
	}
	return c.localPrefix + suffix
}

// ───────────────────────────────────────────────────────────────
// Daxili helper-lər — bütün Redis çağırışları bunlardan keçir.
// ───────────────────────────────────────────────────────────────

func (c *Client) guard() bool {
	if !c.Enabled() {
		return false
	}
	return c.breaker.Allow()
}

func (c *Client) observe(op string, err error) {
	if err == nil || errors.Is(err, redis.Nil) {
		c.breaker.Success()
		return
	}
	c.breaker.Failure()
	log.Printf("WARN cache: %s xətası: %v", op, err)
}

// Get — fullKey artıq prefix-li olmalıdır (SharedKey/LocalKey-dən gəlmiş).
// Üçüncü qaytarış found=true/false: redis.Nil halında false qayıdır.
func (c *Client) Get(ctx context.Context, fullKey string) (string, bool, error) {
	if !c.guard() {
		return "", false, nil
	}
	val, err := c.readRdb.Get(ctx, fullKey).Result()
	if errors.Is(err, redis.Nil) {
		c.observe("GET", nil)
		return "", false, nil
	}
	c.observe("GET", err)
	if err != nil {
		return "", false, err
	}
	return val, true, nil
}

func (c *Client) Set(ctx context.Context, fullKey, value string, ttl time.Duration) error {
	if !c.guard() {
		return nil
	}
	err := c.rdb.Set(ctx, fullKey, value, ttl).Err()
	c.observe("SET", err)
	return err
}

func (c *Client) Del(ctx context.Context, fullKeys ...string) error {
	if !c.guard() || len(fullKeys) == 0 {
		return nil
	}
	err := c.rdb.Del(ctx, fullKeys...).Err()
	c.observe("DEL", err)
	return err
}
