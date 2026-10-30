package middlewares

import (
	"errors"
	"net/http"
	"nordik-drive-api/config"
	"nordik-drive-api/internal/session"

	"github.com/gin-gonic/gin"
)

func AuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		cfg := config.LoadConfig()
		accessToken, err := c.Cookie("access_token")
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Missing access token"})
			c.Abort()
			return
		}

		claims, err := session.Validate(accessToken, cfg.JWTSecret)
		if err != nil {
			message := "Invalid or expired token"
			if errors.Is(err, session.ErrInvalidUserID) {
				message = "invalid user ID"
			}
			c.JSON(http.StatusUnauthorized, gin.H{"error": message})
			c.Abort()
			return
		}

		communities := []string{}
		communities = append(communities, claims.Communities...)
		c.Set("userID", float64(claims.UserID))
		c.Set("communities", communities)
		c.Next()
	}
}
