package xmpp

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// auth_handler.go — HTTP endpoint that ejabberd calls to authenticate XMPP
// clients (ejabberd_auth http backend). This keeps the JWT as the single
// source of truth: there is no separate XMPP password.
//
// Wire it in main.go INSIDE the existing /internal group, e.g.:
//
//	internal.POST("/xmpp/auth", xmppBridge.AuthHandler(cfg.JWTSecret))
//
// ejabberd sends one of these operations (method field):
//   - "auth"    : check credentials. user=<user_id>, pass=<JWT>.
//   - "isuser"  : does this user exist? user=<user_id>.
//   - "setpass" : not supported (JWT only) → false.
//
// We accept "auth" when the JWT is valid AND its user_id/sub equals the
// provided user local-part. On success we ALSO mark the user XMPP-capable in
// the registry so routing flips immediately, without waiting for presence.

type authRequest struct {
	Method string `json:"method"`
	User   string `json:"user"`   // local part = user_id
	Server string `json:"server"` // XMPP domain
	Pass   string `json:"password"`
}

// AuthHandler returns a gin handler validating ejabberd auth callbacks.
func (b *Bridge) AuthHandler(jwtSecret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req authRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusOK, gin.H{"result": false})
			return
		}

		switch req.Method {
		case "isuser":
			// Treat any well-formed positive integer local-part as an existing
			// user. (The real user table is authoritative elsewhere; XMPP only
			// needs the JID to be routable.)
			_, err := strconv.ParseUint(req.User, 10, 64)
			c.JSON(http.StatusOK, gin.H{"result": err == nil})
			return

		case "auth":
			uid, ok := b.verifyJWT(req.Pass, jwtSecret, req.User)
			if ok {
				// Flip the user to XMPP-capable right away.
				b.reg.MarkXMPP(uid, "")
			}
			c.JSON(http.StatusOK, gin.H{"result": ok})
			return

		default:
			c.JSON(http.StatusOK, gin.H{"result": false})
		}
	}
}

// verifyJWT validates the token and checks that its user_id/sub matches the
// claimed local-part. Mirrors middleware/jwt_middleware.go semantics.
func (b *Bridge) verifyJWT(tokenString, secret, claimedUser string) (uint, bool) {
	if tokenString == "" {
		return 0, false
	}
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

	var uid uint64
	switch v := raw.(type) {
	case float64:
		uid = uint64(v)
	case string:
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return 0, false
		}
		uid = n
	default:
		return 0, false
	}

	// The JWT's user must equal the JID local-part it is claiming.
	if strconv.FormatUint(uid, 10) != claimedUser {
		return 0, false
	}
	return uint(uid), true
}
