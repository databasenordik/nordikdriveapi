package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"nordik-drive-api/internal/notificationcontract"
	"strings"
	"testing"
	"time"
)

func TestAuditedReplayGrantsOneAttemptAndCannotResendCodes(t *testing.T) {
	s, store, _, now := serviceFixture(t)
	ctx := context.Background()
	calls := 0
	s.Provider = providerFunc(func(context.Context, notificationcontract.Payload) string { calls++; return "unknown" })
	id, err := s.Accept(ctx, sealedCommand(t, s, "manual-replay"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Deliver(ctx, id); err != nil {
		t.Fatal(err)
	}
	key := uuid.NewString()
	if err := store.Replay(ctx, id, key, "operator@example.test", "Provider acceptance investigated", *now); err != nil {
		t.Fatal(err)
	}
	if err := store.Replay(ctx, id, key, "operator@example.test", "Provider acceptance investigated", *now); err != nil {
		t.Fatal("approval retry was not idempotent", err)
	}
	if err := store.Replay(ctx, id, key, "operator@example.test", "Changed reason", *now); !errors.Is(err, ErrConflict) {
		t.Fatal("approval key reused")
	}
	if err := store.Replay(ctx, id, uuid.NewString(), "operator@example.test", "Concurrent new approval", *now); !errors.Is(err, ErrConflict) {
		t.Fatal("active replay was approved again")
	}
	s.Provider = providerFunc(func(context.Context, notificationcontract.Payload) string { calls++; return "retry" })
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	_ = s.Deliver(ctx, id)
	*now = now.Add(61 * time.Second)
	_ = s.Deliver(ctx, id)
	var row Delivery
	if err := store.DB.First(&row, "id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	if calls != 2 || row.Attempts != 2 || row.State != "failed" {
		t.Fatal("one approval reset automatic retry history")
	}
	var approvals []ReplayRecord
	if err := store.DB.Find(&approvals).Error; err != nil || len(approvals) != 1 || approvals[0].Caller != "operator@example.test" {
		t.Fatal("approval audit missing")
	}
	for _, kind := range []string{"otp", "expired", "accepted"} {
		t.Run(kind, func(t *testing.T) {
			service, repository, _, clock := serviceFixture(t)
			command := sealedCommand(t, service, "forbidden-replay")
			if kind == "otp" {
				p, _ := service.Codec.Open(command)
				command.Kind = "otp"
				command.Deadline = command.CreatedAt.Add(10 * time.Minute)
				command, _ = service.Codec.Seal(command, p)
			}
			id, err := service.Accept(ctx, command)
			if err != nil {
				t.Fatal(err)
			}
			if kind != "accepted" {
				service.Provider = providerFunc(func(context.Context, notificationcontract.Payload) string { return "failed" })
			}
			_ = service.Deliver(ctx, id)
			if kind == "expired" {
				*clock = clock.Add(2 * time.Hour)
			}
			if err := repository.Replay(ctx, id, uuid.NewString(), "operator@example.test", "Not permitted", *clock); !errors.Is(err, ErrConflict) {
				t.Fatal("unsafe replay approved")
			}
		})
	}
}
func TestPrivateHTTPFailureResponsesAndOperatorIdentity(t *testing.T) {
	s, store, _, _ := serviceFixture(t)
	authorize := func(_ context.Context, token, role string) (string, error) {
		if token == role {
			return "verified-operator@example.test", nil
		}
		return "", errors.New("forbidden")
	}
	handler := Handler(s, authorize)
	request := func(path, role, body, key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+role)
		r.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := request("/internal/v1/notifications", "producer:identity", `{"producer":"identity"}`, ""); w.Code != 400 {
		t.Fatal("invalid command not rejected")
	}
	command := sealedCommand(t, s, "http-replay")
	b, _ := json.Marshal(command)
	w := request("/internal/v1/notifications", "producer:identity", string(b), "")
	if w.Code != 202 {
		t.Fatal(w.Code)
	}
	var receipt map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &receipt)
	id := receipt["delivery_id"]
	s.Provider = providerFunc(func(context.Context, notificationcontract.Payload) string { return "unknown" })
	_ = s.Deliver(context.Background(), id)
	body := `{"delivery_id":"` + id + `","reason":"Acceptance checked before resend"}`
	if w := request("/internal/v1/replay", "delivery", body, uuid.NewString()); w.Code != 403 {
		t.Fatal("task identity approved replay")
	}
	if w := request("/internal/v1/replay", "operator", body, ""); w.Code != 409 {
		t.Fatal("missing approval key accepted")
	}
	if w := request("/internal/v1/replay", "operator", body, uuid.NewString()); w.Code != 202 {
		t.Fatal("valid approval rejected", w.Code, w.Body.String())
	}
	var approval ReplayRecord
	if err := store.DB.First(&approval).Error; err != nil || approval.Caller != "verified-operator@example.test" {
		t.Fatal("audit caller was not taken from verified identity")
	}
	if w := request("/internal/v1/status", "recovery", `{"ids":[]}`, ""); w.Code != 400 {
		t.Fatal("empty status batch accepted")
	}
	if w := request("/internal/v1/deliver", "delivery", `{}`, ""); w.Code != 400 {
		t.Fatal("missing delivery identifier accepted")
	}
	if w := request("/internal/v1/deliver", "delivery", `{"delivery_id":"missing"}`, ""); w.Code != 503 {
		t.Fatal("missing delivery acknowledged")
	}
	pool, _ := store.DB.DB()
	_ = pool.Close()
	for _, test := range []struct{ Path, Role, Body string }{{"/internal/v1/status", "recovery", `{"ids":["id"]}`}, {"/internal/v1/recover", "recovery", ""}, {"/internal/v1/notifications", "producer:identity", string(b)}, {"/internal/v1/replay", "operator", body}} {
		if w := request(test.Path, test.Role, test.Body, uuid.NewString()); w.Code != 503 {
			t.Fatal("database failure exposed or acknowledged", test.Path, w.Code)
		}
	}
}
func TestCorruptStoredMessageNeverCallsProvider(t *testing.T) {
	for _, corrupt := range []string{"json", "ciphertext"} {
		t.Run(corrupt, func(t *testing.T) {
			s, store, _, _ := serviceFixture(t)
			command := sealedCommand(t, s, "corrupt")
			id, err := s.Accept(context.Background(), command)
			if err != nil {
				t.Fatal(err)
			}
			stored := "invalid JSON"
			if corrupt == "ciphertext" {
				command.Ciphertext = "bad"
				b, _ := json.Marshal(command)
				stored = string(b)
			}
			if err := store.DB.Model(&Delivery{}).Where("id = ?", id).Update("command", stored).Error; err != nil {
				t.Fatal(err)
			}
			calls := 0
			s.Provider = providerFunc(func(context.Context, notificationcontract.Payload) string { calls++; return "provider_accepted" })
			if err := s.Deliver(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			states, _ := store.Statuses(context.Background(), []string{id})
			if calls != 0 || states[id] != "failed" {
				t.Fatal("corrupt payload sent")
			}
		})
	}
	if _, err := WorkloadAuthorizer("https://example.test", nil)(context.Background(), "invalid", "operator"); err == nil {
		t.Fatal("unconfigured caller role accepted")
	}
	if _, err := WorkloadAuthorizer("https://example.test", map[string]map[string]bool{"recovery": {"worker@example.test": true}})(context.Background(), "not-a-token", "recovery"); err == nil {
		t.Fatal("unsigned caller accepted")
	}
}
