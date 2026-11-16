package chat

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"google.golang.org/api/googleapi"
	"google.golang.org/genai"
)

func TestComparisonGateAndEngineValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cc := &ChatController{ChatService: &ChatService{}}
	for _, until := range []string{"", "invalid", "2020-01-01T00:00:00Z", "2099-01-01T00:00:00Z"} {
		t.Setenv("CHAT_COMPARISON_UNTIL", until)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		if _, allowed := cc.comparisonBackend(c, "api"); allowed {
			t.Fatalf("allowed disabled/expired comparison %q", until)
		}
	}
	// No implicit routing change when no comparison engine is supplied.
	t.Setenv("CHAT_COMPARISON_UNTIL", "")
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	if _, allowed := cc.comparisonBackend(c, ""); !allowed {
		t.Fatal("default route changed")
	}
}

func TestProviderTimingCapturesFallbackWithoutPreparation(t *testing.T) {
	original := genaiGenerateContentHook
	t.Cleanup(func() { genaiGenerateContentHook = original })
	capture := &LLMTiming{}
	ctx := context.WithValue(context.Background(), llmTimingKey{}, capture)
	// Slow retrieval/preparation must not enter the provider timer.
	time.Sleep(40 * time.Millisecond)
	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, model string, _ []*genai.Content, _ *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		if model == "primary" {
			return nil, &googleapi.Error{Code: 429}
		}
		return &genai.GenerateContentResponse{}, nil
	}
	_, model, err := (&ChatService{}).generateWithModelFallback(ctx, "primary", "fallback", nil, nil)
	if err != nil || model != "fallback" {
		t.Fatalf("fallback failed: %s %v", model, err)
	}
	if len(capture.Calls) != 2 || capture.Calls[0].Outcome != "error" || capture.Calls[1].Outcome != "ok" {
		t.Fatalf("missing attempts: %+v", capture.Calls)
	}
	if capture.Calls[0].DurationMS+capture.Calls[1].DurationMS >= 30 {
		t.Fatal("preparation entered provider timing")
	}
	if (&LLMTiming{}).summary()["llm_call_count"] != 0 {
		t.Fatal("no-call path is not represented explicitly")
	}
}

func TestComparisonWindowIsBounded(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-11-17T12:00:00Z")
	if !comparisonAllowed(comparisonDeadline, now) {
		t.Fatal("tomorrow window rejected")
	}
	end, _ := time.Parse(time.RFC3339, comparisonDeadline)
	if comparisonAllowed(comparisonDeadline, end) {
		t.Fatal("deadline is not exclusive")
	}
	if comparisonAllowed("2026-11-19T05:00:00Z", now) {
		t.Fatal("unbounded extension allowed")
	}
}
