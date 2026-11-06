package observability

import (
	"bytes"
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

func TestCorrelationDeadlineAndPanicRedaction(t *testing.T) {
	gin.SetMode(gin.TestMode)
	Setup()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	router := gin.New()
	router.Use(Middleware())
	router.GET("/api/user/test", func(c *gin.Context) {
		deadline, ok := c.Request.Context().Deadline()
		if !ok || time.Until(deadline) > 31*time.Second {
			t.Error("missing ordinary request budget")
		}
		if RequestID(c.Request.Context()) == "" {
			t.Error("lost request ID")
		}
		c.Status(204)
	})
	router.GET("/panic", func(c *gin.Context) { panic("credential-canary") })
	for _, id := range []string{"valid-request-id-123456", "bad!"} {
		req := httptest.NewRequest("GET", "/api/user/test", nil)
		req.Header.Set("X-Request-ID", id)
		req.Header.Set("Cookie", "secret-cookie")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		if response.Code != 204 || !validID(response.Header().Get("X-Request-ID")) {
			t.Fatal("request ID invalid")
		}
		if validID(id) && response.Header().Get("X-Request-ID") != id {
			t.Fatal("gateway correlation discarded")
		}
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("GET", "/panic", nil))
	if response.Code != 500 || strings.Contains(response.Body.String()+logs.String(), "credential-canary") || strings.Contains(logs.String(), "secret-cookie") {
		t.Fatal("panic or headers leaked credentials")
	}
}
func TestControlsAreAbsentWithoutOperatorsAndRejectUnsignedTokens(t *testing.T) {
	t.Setenv("OPERATOR_TOKEN_AUDIENCE", "")
	router := gin.New()
	RegisterControls(router, nil)
	if len(router.Routes()) != 0 {
		t.Fatal("unconfigured controls exposed")
	}
	t.Setenv("OPERATOR_TOKEN_AUDIENCE", "https://operators.internal")
	t.Setenv("OPERATOR_SERVICE_ACCOUNTS", "operator@example.iam.gserviceaccount.com")
	RegisterControls(router, nil)
	request := httptest.NewRequest("POST", "/internal/log-level", strings.NewReader(`{"level":"DEBUG"}`))
	request.Header.Set("X-Operator-Authorization", "Bearer unsigned")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != 403 {
		t.Fatal("unsigned operator admitted")
	}
}
func TestTracingLocalAndInvalidConfiguration(t *testing.T) {
	t.Setenv("OTEL_EXPORTER", "none")
	shutdown, err := Tracing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OTEL_EXPORTER", "unknown")
	if _, err := Tracing(context.Background()); err == nil {
		t.Fatal("unknown exporter accepted")
	}
}

func TestAuthorizedControlAlwaysRecordsLevelChange(t *testing.T) {
	pool, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var output bytes.Buffer
	old := slog.Default()
	previous := Level.Level()
	defer func() { slog.SetDefault(old); Level.Set(previous) }()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: Level})))
	router := gin.New()
	registerControls(router, pool, "https://operators", map[string]bool{"operator": true}, func(context.Context, string, string, map[string]bool) (string, error) {
		return "verified-operator", nil
	})
	for _, tc := range []struct {
		body   string
		status int
	}{{`{"level":"ERROR"}`, 204}, {`{"level":"DEBUG"}`, 204}, {`{"level":"ERROR+100"}`, 400}, {`{`, 400}} {
		request := httptest.NewRequest("POST", "/internal/log-level", strings.NewReader(tc.body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != tc.status {
			t.Fatalf("status %d want %d", response.Code, tc.status)
		}
	}
	if strings.Count(output.String(), "log_level_changed") != 2 || !strings.Contains(output.String(), "verified-operator") {
		t.Fatal("administrative changes lost their actor or audit record")
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("GET", "/internal/metrics", nil))
	if response.Code != 200 || !strings.Contains(response.Body.String(), "wait_count_total") {
		t.Fatal("pool metrics unavailable")
	}
}
