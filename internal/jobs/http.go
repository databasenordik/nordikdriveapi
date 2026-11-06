package jobs

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// RegisterWorker must be mounted ONLY on the dedicated IAM-protected service.
// Scheduler's job/time headers are identifiers, not authentication credentials.
func RegisterWorker(router *gin.Engine, runner *Runner, expectedPrefix string) {
	pending := make(chan struct{}, 3)
	router.POST("/internal/jobs/:job/run", func(c *gin.Context) {
		name := c.Param("job")
		if _, ok := runner.Jobs[name]; !ok {
			c.JSON(404, gin.H{"error": "Unknown job", "code": "NOT_FOUND"})
			return
		}
		if expectedPrefix == "" || c.GetHeader("X-CloudScheduler-JobName") != strings.TrimSuffix(expectedPrefix, "/")+"/"+name {
			c.JSON(403, gin.H{"error": "Unexpected scheduler job", "code": "FORBIDDEN"})
			return
		}
		scheduled, err := time.Parse(time.RFC3339, c.GetHeader("X-CloudScheduler-ScheduleTime"))
		if err != nil || scheduled.After(time.Now().Add(time.Minute)) {
			c.JSON(400, gin.H{"error": "Valid scheduled time required", "code": "INVALID_SCHEDULE"})
			return
		}
		select {
		case pending <- struct{}{}:
			defer func() { <-pending }()
		default:
			c.Header("Retry-After", "5")
			c.JSON(503, gin.H{"error": "Worker busy", "code": "WORKER_BUSY"})
			return
		}
		duplicate, err := runner.Run(c.Request.Context(), name, scheduled)
		if err != nil {
			c.Header("Retry-After", "5")
			code := "JOB_FAILED"
			if errors.Is(err, ErrBusy) {
				code = "JOB_BUSY"
			}
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Job did not complete", "code": code})
			return
		}
		c.JSON(200, gin.H{"status": "completed", "duplicate": duplicate})
	})
}
