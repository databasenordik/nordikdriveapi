package chat

// Permanent provider measurements and chat logs. Only engine selection is
// restricted to the temporary development comparison window.
import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"google.golang.org/genai"
	"nordik-drive-api/internal/observability"
)

const comparisonDeadline = "2026-11-18T05:00:00Z"

func comparisonEnabled() bool {
	return comparisonAllowed(os.Getenv("CHAT_COMPARISON_UNTIL"), time.Now())
}

func comparisonAllowed(value string, now time.Time) bool {
	until, err := time.Parse(time.RFC3339, value)
	ceiling, _ := time.Parse(time.RFC3339, comparisonDeadline)
	return err == nil && !until.After(ceiling) && now.Before(until)
}

type ProviderCall struct {
	Model      string  `json:"model"`
	DurationMS float64 `json:"duration_ms"`
	Outcome    string  `json:"outcome"`
}
type LLMTiming struct {
	mu          sync.Mutex
	Calls       []ProviderCall `json:"calls"`
	Unavailable bool           `json:"-"`
}
type llmTimingKey struct{}

func (t *LLMTiming) add(call ProviderCall) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Calls = append(t.Calls, call)
}
func (t *LLMTiming) summary() gin.H {
	t.mu.Lock()
	defer t.mu.Unlock()
	calls := append([]ProviderCall{}, t.Calls...)
	total := 0.0
	for _, call := range calls {
		total += call.DurationMS
	}
	if t.Unavailable {
		return gin.H{"provider_calls": calls, "llm_ms": nil, "llm_call_count": nil, "measurement": "provider_round_trip", "available": false}
	}
	return gin.H{"available": true, "provider_calls": calls, "llm_ms": total, "llm_call_count": len(calls), "measurement": "provider_round_trip"}
}

// Only the SDK call is timed: excludes retrieval, prompt preparation, tools and retry backoff.
func (cs *ChatService) measuredGenerate(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
	started := time.Now()
	response, err := genaiGenerateContentHook(cs.Client, ctx, model, contents, config)
	elapsed := time.Since(started)
	if capture, ok := ctx.Value(llmTimingKey{}).(*LLMTiming); ok && capture != nil {
		outcome := "ok"
		if err != nil {
			outcome = "error"
		}
		capture.add(ProviderCall{Model: model, DurationMS: float64(elapsed) / float64(time.Millisecond), Outcome: outcome})
	}
	return response, err
}

func (cc *ChatController) comparisonBackend(c *gin.Context, engine string) (ChatBackend, bool) {
	if engine == "" {
		return cc.Backend, true
	}
	if !comparisonEnabled() {
		c.JSON(http.StatusForbidden, gin.H{"error": "Temporary NIA comparison is disabled or expired"})
		return nil, false
	}
	switch engine {
	case "api":
		return legacyChatBackend{service: cc.ChatService}, true
	case "nia":
		if _, ok := cc.Backend.(*niaChatBackend); ok {
			return cc.Backend, true
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "NIA API is not configured"})
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "engine must be api or nia"})
	}
	return nil, false
}

func (cc *ChatController) Comparison(c *gin.Context) {
	_, nia := cc.Backend.(*niaChatBackend)
	c.JSON(200, gin.H{"enabled": comparisonEnabled() && nia, "expires_at": os.Getenv("CHAT_COMPARISON_UNTIL")})
}

func logComparison(ctx context.Context, engine, question, answer string, metrics gin.H, failed bool) {
	slog.InfoContext(ctx, "NIA comparison", "event", "nia_comparison", "engine", engine,
		"request_id", observability.RequestID(ctx), "question", question, "answer", answer, "failed", failed, "timing", metrics)
}
