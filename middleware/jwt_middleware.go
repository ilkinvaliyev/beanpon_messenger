package middleware

import (
	"net/http"
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
		})

		if err != nil || !token.Valid {
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

// JWTMiddlewareForWebSocket WebSocket için JWT kontrolü (query parameter'dan)
func JWTMiddlewareForWebSocket(jwtSecret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		// URL query parameter'dan token al
		tokenString := c.Query("token")
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
		})

		if err != nil || !token.Valid {
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
