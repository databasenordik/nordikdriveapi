package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"

	"nordik-drive-api/config"
)

func TestRealApplicationRetainsRouteInventory(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("CHAT_USE_NIA", "false")
	t.Setenv("OPERATOR_TOKEN_AUDIENCE", "")
	app, err := newApplication(config.Config{}, &runtimeDependencies{})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	data, err := os.ReadFile("../../tests/fixtures/public_routes.json")
	if err != nil {
		t.Fatal(err)
	}
	var expected []struct{ Method, Path string }
	if err := json.Unmarshal(data, &expected); err != nil {
		t.Fatal(err)
	}
	actual := map[string]bool{}
	for _, route := range app.Router.Routes() {
		actual[route.Method+" "+route.Path] = true
	}
	for _, route := range expected {
		key := route.Method + " " + route.Path
		if !actual[key] {
			t.Errorf("existing functionality lost route %s", key)
		}
	}
	if len(actual) != len(expected)+2 {
		t.Fatalf("unexpected route drift: %d routes; expected public inventory plus health and legacy honour trigger", len(actual))
	}
	request := httptest.NewRequest("GET", "/healthz", nil)
	response := httptest.NewRecorder()
	app.Router.ServeHTTP(response, request)
	if response.Code != 200 || response.Header().Get("X-Request-ID") == "" {
		t.Fatal("health/correlation unavailable")
	}
}

func TestWorkerHasNoPublicDomainRoutes(t *testing.T) {
	t.Setenv("WORKER_SCHEDULER_PREFIX", "projects/test/locations/test/jobs")
	t.Setenv("OPERATOR_TOKEN_AUDIENCE", "")
	router, err := workerRouter(config.Config{}, &runtimeDependencies{})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/user/login", "/api/file", "/api/chat", "/api/internal/jobs/honour/run"} {
		req := httptest.NewRequest("POST", path, nil)
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		if res.Code != 404 {
			t.Fatalf("worker exposed public domain %s", path)
		}
	}
	t.Setenv("WORKER_SCHEDULER_PREFIX", "")
	if _, err := workerRouter(config.Config{}, &runtimeDependencies{}); err == nil {
		t.Fatal("worker missing scheduler identity configuration accepted")
	}
}

func TestServerShutdownHonorsCancellation(t *testing.T) {
	t.Setenv("PORT", "0")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := serve(ctx, gin.New()); err != nil {
		t.Fatal(err)
	}
}
