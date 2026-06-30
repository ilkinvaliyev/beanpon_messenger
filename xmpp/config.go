package xmpp

import (
	"os"
	"strconv"
)

// Config holds the XMPP bridge settings. Loaded from the same .env as the rest
// of the service (see xmpp/deploy/.env.xmpp.example). When Enabled is false the
// whole subsystem is a no-op: the service behaves exactly like today.
//
// Counterpart: there is no Flutter counterpart — this is server-only plumbing.
type Config struct {
	Enabled bool

	// Domain is the XMPP virtual host. A user's JID is "<user_id>@Domain".
	Domain string

	// ComponentName is the external-component JID (XEP-0114), e.g.
	// "bridge.beanpon.com". ComponentHost/Port/Secret describe how to reach
	// ejabberd's component listener (port 5347).
	ComponentName   string
	ComponentHost   string
	ComponentPort   int
	ComponentSecret string

	// MUCHost is the Multi-User Chat service host, e.g. "groups.beanpon.com".
	// A group's room JID is "group_<conversation_id>@MUCHost".
	MUCHost string
}

// LoadConfig reads XMPP_* environment variables. Mirrors the style of
// config.LoadConfig (envStr/envInt helpers) but kept local to avoid widening
// the main config package before the subsystem is switched on.
func LoadConfig() Config {
	return Config{
		Enabled:         envBool("XMPP_ENABLED", false),
		Domain:          envStr("XMPP_DOMAIN", "beanpon.com"),
		ComponentName:   envStr("XMPP_COMPONENT_NAME", "bridge.beanpon.com"),
		ComponentHost:   envStr("XMPP_COMPONENT_HOST", "127.0.0.1"),
		ComponentPort:   envInt("XMPP_COMPONENT_PORT", 5347),
		ComponentSecret: envStr("XMPP_COMPONENT_SECRET", ""),
		MUCHost:         envStr("XMPP_MUC_HOST", "groups.beanpon.com"),
	}
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
