package observability

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"nordik-drive-api/internal/database"
	"nordik-drive-api/internal/workload"
)

var Level = new(slog.LevelVar)

type requestIDKey struct{}

func Setup() {
	slog.SetDefault(slog.New(contextHandler{slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: Level})}))
	otel.SetTextMapPropagator(propagation.TraceContext{})
}

type contextHandler struct{ slog.Handler }

func (h contextHandler) Handle(ctx context.Context, record slog.Record) error {
	if id := RequestID(ctx); id != "" {
		record.Add("request_id", id)
	}
	span := trace.SpanContextFromContext(ctx)
	if span.IsValid() {
		record.Add("trace_id", span.TraceID().String(), "span_id", span.SpanID().String())
		if project := os.Getenv("GOOGLE_CLOUD_PROJECT"); project != "" {
			record.Add("logging.googleapis.com/trace", "projects/"+project+"/traces/"+span.TraceID().String(), "logging.googleapis.com/spanId", span.SpanID().String())
		}
	}
	return h.Handler.Handle(ctx, record)
}
func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{h.Handler.WithAttrs(attrs)}
}
func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{h.Handler.WithGroup(name)}
}
func RequestID(ctx context.Context) string {
	value, _ := ctx.Value(requestIDKey{}).(string)
	return value
}
func Middleware() gin.HandlerFunc {
	ordinary, chat, transfer, job := duration("REQUEST_TIMEOUT_SECONDS", 30), duration("CHAT_TIMEOUT_SECONDS", 300), duration("TRANSFER_TIMEOUT_SECONDS", 300), duration("JOB_TIMEOUT_SECONDS", 900)
	return func(c *gin.Context) {
		id := c.GetHeader("X-Request-ID")
		if !validID(id) {
			var bytes [16]byte
			_, _ = rand.Read(bytes[:])
			id = hex.EncodeToString(bytes[:])
		}
		c.Header("X-Request-ID", id)
		ctx := otel.GetTextMapPropagator().Extract(c.Request.Context(), propagation.HeaderCarrier(c.Request.Header))
		ctx = context.WithValue(ctx, requestIDKey{}, id)
		budget := ordinary
		if strings.HasPrefix(c.Request.URL.Path, "/api/chat") {
			budget = chat
		} else if strings.HasPrefix(c.Request.URL.Path, "/api/file") || strings.HasPrefix(c.Request.URL.Path, "/api/form") || strings.HasPrefix(c.Request.URL.Path, "/api/admin") {
			budget = transfer
		}
		if strings.HasPrefix(c.Request.URL.Path, "/internal/jobs/") {
			budget = job
		}
		ctx, cancel := context.WithTimeout(ctx, budget)
		defer cancel()
		ctx, span := otel.Tracer("nordik/http").Start(ctx, c.Request.Method+" "+c.FullPath(), trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()
		c.Request = c.Request.WithContext(ctx)
		start := time.Now()
		defer func() {
			if recover() != nil {
				c.Abort()
				if !c.Writer.Written() {
					c.AbortWithStatusJSON(500, gin.H{"error": "Internal server error", "code": "INTERNAL", "request_id": id})
				}
				slog.ErrorContext(ctx, "request_panic", "request_id", id)
			}
			span.SetAttributes(attribute.Int("http.response.status_code", c.Writer.Status()), attribute.String("http.route", c.FullPath()))
			sc := span.SpanContext()
			slog.InfoContext(ctx, "http_request", "request_id", id, "trace_id", sc.TraceID().String(), "service", os.Getenv("K_SERVICE"), "revision", os.Getenv("K_REVISION"), "method", c.Request.Method, "route", c.FullPath(), "status", c.Writer.Status(), "duration_ms", time.Since(start).Milliseconds())
		}()
		c.Next()
	}
}

func duration(name string, fallback int) time.Duration {
	n, err := strconv.Atoi(os.Getenv(name))
	if err != nil || n < 1 || n > 900 {
		n = fallback
	}
	return time.Duration(n) * time.Second
}
func ValidateConfig() error {
	for _, name := range []string{"REQUEST_TIMEOUT_SECONDS", "CHAT_TIMEOUT_SECONDS", "TRANSFER_TIMEOUT_SECONDS", "JOB_TIMEOUT_SECONDS"} {
		if raw := os.Getenv(name); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 || n > 900 {
				return errors.New(name + " must be between 1 and 900")
			}
		}
	}
	return nil
}
func validID(id string) bool {
	if len(id) < 16 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// Internal endpoints require the ORIGINAL signed operator token, independent
// of Cloud Run's IAM check and its signature-stripped platform header.
func RegisterControls(router *gin.Engine, pool *sql.DB) {
	audience := os.Getenv("OPERATOR_TOKEN_AUDIENCE")
	accounts := map[string]bool{}
	for _, email := range strings.Split(os.Getenv("OPERATOR_SERVICE_ACCOUNTS"), ",") {
		if email = strings.TrimSpace(email); email != "" {
			accounts[email] = true
		}
	}
	if audience == "" || len(accounts) == 0 {
		return
	}
	registerControls(router, pool, audience, accounts, workload.VerifiedAccount)
}
func registerControls(router *gin.Engine, pool *sql.DB, audience string, accounts map[string]bool, verify func(context.Context, string, string, map[string]bool) (string, error)) {
	group := router.Group("/internal")
	group.Use(func(c *gin.Context) {
		encoded := strings.TrimPrefix(c.GetHeader("X-Operator-Authorization"), "Bearer ")
		account, err := verify(c.Request.Context(), encoded, audience, accounts)
		if err != nil {
			c.AbortWithStatusJSON(403, gin.H{"error": "Operator authorization required"})
			return
		}
		c.Set("operator_account", account)
		c.Next()
	})
	group.POST("/log-level", func(c *gin.Context) {
		var body struct {
			Level string `json:"level"`
		}
		var level slog.Level
		if c.ShouldBindJSON(&body) != nil || !strings.Contains("|DEBUG|INFO|WARN|ERROR|", "|"+body.Level+"|") || level.UnmarshalText([]byte(body.Level)) != nil {
			c.JSON(400, gin.H{"error": "level must be DEBUG, INFO, WARN or ERROR"})
			return
		}
		record := slog.NewRecord(time.Now(), slog.LevelInfo, "log_level_changed", 0)
		record.Add("level", level.String(), "request_id", RequestID(c.Request.Context()), "operator_account", c.GetString("operator_account"))
		// Administrative changes remain observable even when raising the log level.
		_ = slog.Default().Handler().Handle(c.Request.Context(), record)
		Level.Set(level)
		c.Status(204)
	})
	group.GET("/metrics", func(c *gin.Context) { c.JSON(http.StatusOK, database.PoolStats(pool)) })
}
