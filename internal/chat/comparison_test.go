package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"google.golang.org/api/googleapi"
	"google.golang.org/genai"
	niapb "nordik-drive-api/internal/nia/pb"
)

type permanentTimingBackend struct {
	t     *testing.T
	calls []ProviderCall
	err   error
}

func (b permanentTimingBackend) Chat(ctx context.Context, _ string, _ *multipart.FileHeader, _ string, _ []string, _ string) (*ChatResult, error) {
	capture, ok := ctx.Value(llmTimingKey{}).(*LLMTiming)
	if !ok || capture == nil {
		b.t.Fatal("permanent provider capture missing")
	}
	for _, call := range b.calls {
		capture.add(call)
	}
	if b.err != nil {
		return nil, b.err
	}
	return &ChatResult{Answer: "Permanent answer"}, nil
}

func TestChatLoggingSurvivesMissingOrExpiredComparisonWindow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, until := range []string{"", "2026-11-18T05:00:00Z"} {
		for _, scenario := range []struct {
			name   string
			calls  []ProviderCall
			err    error
			status int
		}{
			{name: "cached", status: http.StatusOK},
			{name: "provider", calls: []ProviderCall{{Model: "test-model", DurationMS: 12.5, Outcome: "ok"}}, status: http.StatusOK},
			{name: "provider-failure", calls: []ProviderCall{{Model: "test-model", DurationMS: 8, Outcome: "error"}}, err: &ChatHTTPError{Status: http.StatusBadGateway, Message: "provider unavailable"}, status: http.StatusBadGateway},
		} {
			t.Run(until+"/"+scenario.name, func(t *testing.T) {
				t.Setenv("CHAT_COMPARISON_UNTIL", until)
				var output bytes.Buffer
				previous := slog.Default()
				slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
				t.Cleanup(func() { slog.SetDefault(previous) })
				backend := permanentTimingBackend{t: t, calls: scenario.calls, err: scenario.err}
				router := gin.New()
				router.POST("/api/chat", NewChatController(&ChatService{}, backend).Chat)
				form := url.Values{"filename": {"sheet.xlsx"}, "question": {"Permanent question"}}
				request := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(form.Encode()))
				request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				if response.Code != scenario.status {
					t.Fatalf("unexpected status %d: %s", response.Code, response.Body)
				}
				var event struct {
					Event    string `json:"event"`
					Engine   string `json:"engine"`
					Question string `json:"question"`
					Answer   string `json:"answer"`
					Failed   bool   `json:"failed"`
					Timing   struct {
						Calls []ProviderCall `json:"provider_calls"`
						Total float64        `json:"llm_ms"`
						Count int            `json:"llm_call_count"`
					} `json:"timing"`
				}
				if err := json.Unmarshal(output.Bytes(), &event); err != nil {
					t.Fatalf("missing structured log: %s (%v)", output.String(), err)
				}
				if event.Event != "nia_comparison" || event.Engine != "api" || event.Question != "Permanent question" || event.Failed != (scenario.err != nil) || event.Timing.Count != len(scenario.calls) {
					t.Fatalf("unexpected permanent log: %+v", event)
				}
				if scenario.err == nil && event.Answer != "Permanent answer" {
					t.Fatal("answer was not logged")
				}
				var total float64
				for _, call := range scenario.calls {
					total += call.DurationMS
				}
				if event.Timing.Total != total || len(event.Timing.Calls) != len(scenario.calls) {
					t.Fatalf("provider measurements differ: %+v", event.Timing)
				}
				if strings.Contains(response.Body.String(), `"timing"`) || strings.Contains(response.Body.String(), `"engine"`) {
					t.Fatal("permanent logging changed the expired comparison response contract")
				}
			})
		}
	}
}

func TestPermanentNIALogIncludesRemoteTimingOrExplicitlyUnavailable(t *testing.T) {
	for _, available := range []bool{true, false} {
		t.Run(map[bool]string{true: "measured", false: "unavailable"}[available], func(t *testing.T) {
			t.Setenv("CHAT_COMPARISON_UNTIL", "2026-11-18T05:00:00Z")
			var output bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })
			target := startTestNIA(t, func(_ context.Context, _ *niapb.QueryRequest) (*niapb.QueryResponse, error) {
				result := []byte(`{}`)
				if available {
					result = []byte(`{"timing":{"provider_calls":[{"model":"remote-model","duration_ms":16.5,"outcome":"ok"}]}}`)
				}
				return &niapb.QueryResponse{Status: "answered", Answer: "Remote answer", ResultJson: result}, nil
			})
			router, mock := setupNIAController(t, target)
			response := requestChat(router, "")
			if response.Code != http.StatusOK {
				t.Fatalf("unexpected response %d: %s", response.Code, response.Body)
			}
			var event struct {
				Event  string         `json:"event"`
				Engine string         `json:"engine"`
				Answer string         `json:"answer"`
				Timing map[string]any `json:"timing"`
			}
			if err := json.Unmarshal(output.Bytes(), &event); err != nil {
				t.Fatalf("missing remote comparison log: %s (%v)", output.String(), err)
			}
			if event.Event != "nia_comparison" || event.Engine != "nia" || event.Answer != "Remote answer" || event.Timing["available"] != available {
				t.Fatalf("unexpected remote log: %+v", event)
			}
			if available && (event.Timing["llm_ms"] != 16.5 || event.Timing["llm_call_count"] != float64(1)) {
				t.Fatalf("remote provider measurements lost: %+v", event.Timing)
			}
			if !available && (event.Timing["llm_ms"] != nil || event.Timing["llm_call_count"] != nil) {
				t.Fatalf("missing remote timing must not become zero: %+v", event.Timing)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

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
