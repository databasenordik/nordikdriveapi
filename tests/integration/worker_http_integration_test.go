//go:build integration

package integration

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"nordik-drive-api/internal/jobs"
)

func TestPostgresWorkerHTTPDispatchAndPersistentDeduplication(t *testing.T) {
	db := platformFixture(t)
	pool, _ := db.DB()
	var calls atomic.Int32
	fail := false
	runner := &jobs.Runner{Pool: pool, Jobs: map[string]jobs.RunFunc{"fixture": func(context.Context) error {
		calls.Add(1)
		if fail {
			return errors.New("provider-credential-canary")
		}
		return nil
	}}}
	router := gin.New()
	jobs.RegisterWorker(router, runner, "projects/fixture/locations/local/jobs")
	invoke := func(slot time.Time, identity string) *httptest.ResponseRecorder {
		request := httptest.NewRequest("POST", "/internal/jobs/fixture/run", nil)
		request.Header.Set("X-CloudScheduler-JobName", identity)
		request.Header.Set("X-CloudScheduler-ScheduleTime", slot.Format(time.RFC3339))
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response
	}
	slot := time.Now().UTC().Truncate(time.Minute)
	first := invoke(slot, "projects/fixture/locations/local/jobs/fixture")
	second := invoke(slot, "fixture")
	if first.Code != 200 || second.Code != 200 || calls.Load() != 1 || !strings.Contains(second.Body.String(), `"duplicate":true`) {
		t.Fatal("HTTP retry executed successful work twice")
	}
	fail = true
	failed := invoke(slot.Add(-time.Minute), "fixture")
	if failed.Code != 503 || failed.Header().Get("Retry-After") == "" || strings.Contains(failed.Body.String(), "credential-canary") {
		t.Fatal("worker failure is not safely retryable")
	}
	fail = false
	if recovered := invoke(slot.Add(-time.Minute), "fixture"); recovered.Code != 200 {
		t.Fatal("failed delivery slot could not recover over HTTP")
	}
}

func TestPostgresWorkerBoundsConcurrentRunsAndRejectsBusyOwner(t *testing.T) {
	db := platformFixture(t)
	pool, _ := db.DB()
	pool.SetMaxOpenConns(8)
	started := make(chan string, 3)
	release := make(chan struct{})
	defer close(release)
	runner := &jobs.Runner{Pool: pool, Jobs: map[string]jobs.RunFunc{}}
	for _, name := range []string{"one", "two", "three", "four"} {
		name := name
		runner.Jobs[name] = func(ctx context.Context) error {
			started <- name
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	router := gin.New()
	jobs.RegisterWorker(router, runner, "projects/fixture/locations/local/jobs")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	invoke := func(name string) *httptest.ResponseRecorder {
		request := httptest.NewRequest("POST", "/internal/jobs/"+name+"/run", nil).WithContext(ctx)
		request.Header.Set("X-CloudScheduler-JobName", "projects/fixture/locations/local/jobs/"+name)
		request.Header.Set("X-CloudScheduler-ScheduleTime", time.Now().UTC().Truncate(time.Minute).Format(time.RFC3339))
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response
	}
	done := make(chan *httptest.ResponseRecorder, 3)
	go func() { done <- invoke("one") }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("first worker never acquired ownership")
	}
	if busy := invoke("one"); busy.Code != 503 || !strings.Contains(busy.Body.String(), "JOB_BUSY") {
		t.Fatal("overlapping owner was admitted")
	}
	for _, name := range []string{"two", "three"} {
		name := name
		go func() { done <- invoke(name) }()
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("worker never started")
		}
	}
	if full := invoke("four"); full.Code != 503 || !strings.Contains(full.Body.String(), "WORKER_BUSY") {
		t.Fatal("worker concurrency was not bounded")
	}
	cancel()
	for i := 0; i < 3; i++ {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("cancelled worker did not drain")
		}
	}
}
