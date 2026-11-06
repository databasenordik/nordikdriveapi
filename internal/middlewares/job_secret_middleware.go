package middlewares

import (
	"crypto/subtle"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"

	"nordik-drive-api/config"
)

const HonourJobSecretHeader = "X-Honour-Job-Secret"

func JobSecretMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if os.Getenv("SCHEDULER_MODE") == "external" || os.Getenv("SCHEDULER_MODE") == "disabled" {
			c.AbortWithStatusJSON(404, gin.H{"error": "not found"})
			return
		}
		cfg := config.LoadConfig()
		expected := strings.TrimSpace(cfg.HonourJobSecret)
		if expected == "" {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "honour job secret is not configured"})
			c.Abort()
			return
		}

		provided := strings.TrimSpace(c.GetHeader(HonourJobSecretHeader))
		if provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid honour job secret"})
			c.Abort()
			return
		}

		c.Next()
	}
}
