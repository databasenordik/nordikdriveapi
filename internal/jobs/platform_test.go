package jobs

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestWorkerRejectsUnknownJobsAndInvalidSchedulerIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterWorker(router, &Runner{Jobs: map[string]RunFunc{"fixture": func(context.Context) error { return nil }}}, "projects/test/locations/test/jobs")
	for _, tc := range []struct {
		path, identity, scheduled string
		status                    int
	}{
		{"/internal/jobs/unknown/run", "", "", 404},
		{"/internal/jobs/fixture/run", "forged", "", 403},
		{"/internal/jobs/fixture/run", "projects/test/locations/test/jobs/fixture", "bad", 400},
		{"/internal/jobs/fixture/run", "projects/test/locations/test/jobs/fixture", time.Now().Add(time.Hour).Format(time.RFC3339), 400},
	} {
		req := httptest.NewRequest("POST", tc.path, nil)
		req.Header.Set("X-CloudScheduler-JobName", tc.identity)
		req.Header.Set("X-CloudScheduler-ScheduleTime", tc.scheduled)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		if response.Code != tc.status {
			t.Fatalf("status %d want %d", response.Code, tc.status)
		}
	}
}

func TestScheduledSlotsUseTorontoCalendarAcrossDST(t *testing.T) {
	location, err := time.LoadLocation("America/Toronto")
	if err != nil {
		t.Fatal(err)
	}
	for _, date := range []string{"2026-03-08T03:30:00-04:00", "2026-11-01T01:30:00-04:00", "2026-11-01T01:30:00-05:00"} {
		now, err := time.Parse(time.RFC3339, date)
		if err != nil {
			t.Fatal(err)
		}
		daily := ScheduledSlot("daily-honour", now, location).In(location)
		if daily.Hour() != 0 || daily.Day() != now.In(location).Day() {
			t.Fatal("wrong calendar slot across DST")
		}
		for _, name := range []string{"file-review-email", "form-review-email", "file-normalization", "file-reconciliation"} {
			slot := ScheduledSlot(name, now, location)
			if slot.After(now) || slot.Second() != 0 {
				t.Fatal("invalid fixed slot")
			}
		}
	}
	scheduler, err := NewGuardedScheduler(&Runner{}, location, nil)
	if err != nil || len(scheduler.cron.Entries()) != 6 {
		t.Fatal("lost original schedules")
	}
	scheduler.Start()
	scheduler.Stop()
	for _, expression := range Schedules {
		if strings.TrimSpace(expression) == "" {
			t.Fatal("empty schedule")
		}
	}
}
