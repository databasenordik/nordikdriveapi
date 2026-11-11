package jobs

import (
	"context"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"nordik-drive-api/internal/workload"
)

// SchedulerGuard keeps the task-only identity out of scheduled business jobs,
// independently of Cloud Run's shared service invocation permission.
func SchedulerGuard(audience, scheduler string) gin.HandlerFunc {
	return schedulerGuard(func(ctx context.Context, token string) error {
		verify, stop := context.WithTimeout(ctx, 3*time.Second)
		defer stop()
		return workload.Verify(verify, token, audience, map[string]bool{scheduler: true})
	})
}

func schedulerGuard(authorize func(context.Context, string) error) gin.HandlerFunc {
	return func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/internal/jobs/") && authorize(c.Request.Context(), strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")) != nil {
			c.AbortWithStatusJSON(403, gin.H{"error": "Scheduler identity required"})
			return
		}
		c.Next()
	}
}
