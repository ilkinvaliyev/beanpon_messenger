package xmpp

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// token_handler.go — mints a short-lived XMPP login token for a client.
//
// WHY
// ejabberd's built-in JWT auth (auth_method: jwt) authenticates a client by a
// signed token whose payload carries the user's JID (e.g. "42@beanpon.com").
// Beanpon's normal JWT only carries a numeric user_id / sub, with no `jid`.
// Rather than change the auth service (and every token), the client asks this
// endpoint — authenticated with its EXISTING JWT — for a fresh token that adds
// the `jid` claim. The new token is signed with the SAME secret ejabberd
// trusts (jwt_key), so ejabberd accepts it.
//
// This keeps the JWT the single source of truth: the caller must already hold
// a valid Beanpon JWT to obtain an XMPP token for its own JID.
//
// Route (protected by the normal JWT middleware so c.Get("user_id") is set, or
// validate inline — here we re-validate the bearer to stay self-contained):
//
//	api.POST("/xmpp/token", xmppBridge.TokenHandler(cfg.JWTSecret))

// TokenHandler returns a gin handler that issues an XMPP login JWT for the
// authenticated user. Response: { "token": "...", "jid": "42@beanpon.com",
// "expires_in": 3600 }.
func (b *Bridge) TokenHandler(jwtSecret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Resolve the caller's user_id from the bearer JWT.
		uid, ok := bearerUserID(c, jwtSecret)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			return
		}

		jid := b.cfg.UserJID(uid) // "<uid>@<domain>"
		ttl := time.Hour
		now := time.Now()

		claims := jwt.MapClaims{
			"jid": jid,
			"exp": now.Add(ttl).Unix(),
			"iat": now.Unix(),
		}
		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
		signed, err := tok.SignedString([]byte(jwtSecret))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "sign failed"})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"token":      signed,
			"jid":        jid,
			"expires_in": int(ttl.Seconds()),
		})
	}
}

// bearerUserID validates the Authorization: Bearer <jwt> header and returns the
// numeric user id from user_id/sub. Mirrors middleware/jwt_middleware.go.
func bearerUserID(c *gin.Context, secret string) (uint, bool) {
	auth := c.GetHeader("Authorization")
	const p = "Bearer "
	if len(auth) <= len(p) || auth[:len(p)] != p {
		return 0, false
	}
	tokenString := auth[len(p):]

	token, err := jwt.Parse(tokenString, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, jwt.ErrSignatureInvalid
		}
		return []byte(secret), nil
	})
	if err != nil || !token.Valid {
		return 0, false
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return 0, false
	}
	var raw interface{}
	if v, ok := claims["user_id"]; ok {
		raw = v
	} else if v, ok := claims["sub"]; ok {
		raw = v
	} else {
		return 0, false
	}
	switch v := raw.(type) {
	case float64:
		return uint(v), true
	case string:
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return 0, false
		}
		return uint(n), true
	default:
		return 0, false
	}
}
