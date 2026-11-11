package jobs

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestTaskIdentityCannotForgeScheduledJobHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(schedulerGuard(func(_ context.Context, token string) error {
		if token != "scheduler" {
			return errors.New("not scheduler")
		}
		return nil
	}))
	router.POST("/internal/jobs/file-reconciliation/run", func(c *gin.Context) { c.Status(204) })
	router.POST("/internal/v1/work/file-version", func(c *gin.Context) { c.Status(202) })
	for _, test := range []struct {
		path, caller string
		status       int
	}{
		{"/internal/jobs/file-reconciliation/run", "task", 403},
		{"/internal/jobs/file-reconciliation/run", "scheduler", 204},
		{"/internal/v1/work/file-version", "task", 202},
	} {
		req := httptest.NewRequest(http.MethodPost, test.path, nil)
		req.Header.Set("Authorization", "Bearer "+test.caller)
		req.Header.Set("X-CloudScheduler-JobName", "file-reconciliation")
		req.Header.Set("X-CloudScheduler-ScheduleTime", "2026-11-11T00:00:00Z")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != test.status {
			t.Fatal("scheduled caller isolation failed", test.caller, w.Code)
		}
	}
}
