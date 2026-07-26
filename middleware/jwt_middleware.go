package middleware

import (
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// JWTMiddleware JWT token kontrolü yapar
func JWTMiddleware(jwtSecret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Authorization header'ını al
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "Authorization header gerekli",
			})
			c.Abort()
			return
		}

		// Bearer token formatını kontrol et
		tokenString := strings.TrimPrefix(authHeader, "Bearer ")
		if tokenString == authHeader {
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "Bearer token formatı gerekli",
			})
			c.Abort()
			return
		}

		// Token'ı parse et (MapClaims ile)
		token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
			if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, jwt.ErrSignatureInvalid
			}
			return []byte(jwtSecret), nil
		}, jwtParseOptions()...)

		if err != nil || !token.Valid {
			jwtRejectionHint(err)
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "Geçersiz token",
			})
			c.Abort()
			return
		}

		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "Token claims okunamadı",
			})
			c.Abort()
			return
		}

		// user_id veya sub çek
		var userID int64
		var rawID interface{}

		if val, ok := claims["user_id"]; ok {
			rawID = val
		} else if val, ok := claims["sub"]; ok {
			rawID = val
		} else {
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "Token'da user_id veya sub bulunamadı",
			})
			c.Abort()
			return
		}

		// ID'yi int64'e çevir
		switch v := rawID.(type) {
		case float64:
			userID = int64(v)
		case string:
			id, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				c.JSON(http.StatusUnauthorized, gin.H{
					"error": "User ID string formatı geçersiz",
				})
				c.Abort()
				return
			}
			userID = id
		default:
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "User ID formatı desteklenmiyor",
			})
			c.Abort()
			return
		}

		// Context'e ekle (uint'e çevir)
		c.Set("user_id", uint(userID))
		c.Set("jwt_claims", claims)
		c.Next()
	}
}

// jwtParseOptions — Issue 17: hər iki middleware üçün eyni sərt yoxlamalar.
//
// golang-jwt v5 `exp` YOXDURSA token-i etibarlı sayır. Yəni müddəti olmayan
// (yəni ƏBƏDİ) bir token qəbul edilirdi — belə kimlik məlumatı köhnəldilə
// bilməz. `iss`/`aud` yalnız konfiqurasiya edilibsə tətbiq olunur ki, mövcud
// Laravel token-ləri qırılmasın.
func jwtParseOptions() []jwt.ParserOption {
	opts := []jwt.ParserOption{
		jwt.WithValidMethods([]string{"HS256", "HS384", "HS512"}),
	}
	// GERİ ALMA DÜYMƏSİ: `JWT_REQUIRE_EXP=false` bu yoxlamanı söndürür.
	// Verici (Laravel) hər hansı səbəbdən `exp` qoymursa, bu bayraq olmadan
	// BÜTÜN kimlik doğrulama bir anda qırılardı. Rədd baş verərsə aşağıdakı
	// `jwtRejectionHint` fərqli bir log sətri yazır → diaqnoz dərhal.
	if strings.ToLower(strings.TrimSpace(os.Getenv("JWT_REQUIRE_EXP"))) != "false" {
		opts = append(opts, jwt.WithExpirationRequired())
	}
	if v := strings.TrimSpace(os.Getenv("JWT_ISSUER")); v != "" {
		opts = append(opts, jwt.WithIssuer(v))
	}
	if v := strings.TrimSpace(os.Getenv("JWT_AUDIENCE")); v != "" {
		opts = append(opts, jwt.WithAudience(v))
	}
	return opts
}

// jwtRejectionHint — token `exp` OLMADIĞI üçün rədd olunduqda fərqli bir log
// sətri yazır. Yeni sərtləşdirmə canlıda hər şeyi qırarsa səbəb dərhal görünsün
// (əks halda sadəcə "Geçersiz token" seli olardı).
func jwtRejectionHint(err error) {
	if errors.Is(err, jwt.ErrTokenRequiredClaimMissing) {
		log.Printf("[JWT] token `exp` claim-i OLMADIĞI üçün rədd edildi — verici exp qoymursa "+
			"müvəqqəti həll: JWT_REQUIRE_EXP=false (səbəb: %v)", err)
	}
}

// wsTokenFromHeaders — WS upgrade istəyindən token-i BAŞLIQDAN oxuyur.
//
// Brauzer `WebSocket` API-si ixtiyari başlıq göndərə bilmir; yeganə kanal
// alt-protokol siyahısıdır. Ona görə `Sec-WebSocket-Protocol: bearer, <JWT>`
// formasını da qəbul edirik (native istemçilər `Authorization` işlədə bilər).
func wsTokenFromHeaders(c *gin.Context) string {
	if auth := c.GetHeader("Authorization"); auth != "" {
		if t := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer ")); t != "" && t != auth {
			return t
		}
	}
	proto := c.GetHeader("Sec-WebSocket-Protocol")
	if proto == "" {
		return ""
	}
	parts := strings.Split(proto, ",")
	for i, p := range parts {
		if strings.EqualFold(strings.TrimSpace(p), "bearer") && i+1 < len(parts) {
			return strings.TrimSpace(parts[i+1])
		}
	}
	return ""
}

// JWTMiddlewareForWebSocket WebSocket için JWT kontrolü (header, fallback: query)
func JWTMiddlewareForWebSocket(jwtSecret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Issue 17: token ARTIQ header-dən də qəbul olunur.
		//
		// `?token=<JWT>` sorğu sətri proxy / gateway / LB access log-larına
		// OLDUĞU KİMİ düşür (və çox vaxt xəta izlərinə). Uzun ömürlü sessiya
		// JWT-si log-lardan toplanıb hesab ələ keçirilməsinə çevrilə bilər.
		// Sıra: `Sec-WebSocket-Protocol` (brauzer WS API-si yalnız bunu
		// göndərə bilir) → `Authorization: Bearer` → köhnə `?token=`.
		//
		// Query yolu GERİYƏ UYĞUNLUQ üçün saxlanılır: canlıdakı istemçilər
		// yenilənənə qədər işləməlidir. İstemçilər keçdikdən sonra silinməlidir.
		tokenString := wsTokenFromHeaders(c)
		if tokenString == "" {
			tokenString = c.Query("token")
		}
		if tokenString == "" {
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "Token parametresi gerekli",
			})
			c.Abort()
			return
		}

		// Token'ı parse et (MapClaims ile)
		token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
			if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, jwt.ErrSignatureInvalid
			}
			return []byte(jwtSecret), nil
		}, jwtParseOptions()...)

		if err != nil || !token.Valid {
			jwtRejectionHint(err)
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "Geçersiz token",
			})
			c.Abort()
			return
		}

		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "Token claims okunamadı",
			})
			c.Abort()
			return
		}

		// user_id veya sub çek
		var userID int64
		var rawID interface{}

		if val, ok := claims["user_id"]; ok {
			rawID = val
		} else if val, ok := claims["sub"]; ok {
			rawID = val
		} else {
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "Token'da user_id veya sub bulunamadı",
			})
			c.Abort()
			return
		}

		// ID'yi int64'e çevir
		switch v := rawID.(type) {
		case float64:
			userID = int64(v)
		case string:
			id, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				c.JSON(http.StatusUnauthorized, gin.H{
					"error": "User ID string formatı geçersiz",
				})
				c.Abort()
				return
			}
			userID = id
		default:
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "User ID formatı desteklenmiyor",
			})
			c.Abort()
			return
		}

		// Context'e ekle (uint'e çevir)
		c.Set("user_id", uint(userID))
		c.Set("jwt_claims", claims)
		c.Next()
	}
}
