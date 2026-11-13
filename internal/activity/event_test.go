package activity

import (
	"context"
	"github.com/google/uuid"
	"strings"
	"testing"
	"time"
)

func TestEventValidationAndActorIsolation(t *testing.T) {
	e := Event{ID: uuid.NewString(), Version: 1, Producer: "legacy", Log: Log{Level: "INFO", Service: "file", Action: "ACCESS_FILE", CreatedAt: time.Now().UTC()}}
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(e.Fingerprint()) != 64 {
		t.Fatal("fingerprint missing")
	}
	values := []string{"one"}
	ctx := WithActor(context.Background(), 52, values)
	values[0] = "changed"
	if ActorFrom(ctx).Communities[0] != "one" || ActorFrom(context.Background()).ID != 0 {
		t.Fatal("actor context mutated")
	}
	for _, mutate := range []func(*Event){func(e *Event) { e.ID = "invalid" }, func(e *Event) { e.Version = 2 }, func(e *Event) { e.Producer = "unknown" }, func(e *Event) { e.Log.ID = 1 }, func(e *Event) { e.Log.CreatedAt = time.Time{} }, func(e *Event) { e.Log.Level = "" }, func(e *Event) { e.Log.Action = "" }, func(e *Event) { e.Log.Action = strings.Repeat("a", 256) }, func(e *Event) { e.RequestID = strings.Repeat("a", 129) }, func(e *Event) { e.TraceID = "bad" }, func(e *Event) { e.Producer = "identity" }, func(e *Event) { e.Log.Service = "auth" }, func(e *Event) { s := `{"otp":"secret"}`; e.Log.Metadata = &s }, func(e *Event) { s := "invalid"; e.Log.Metadata = &s }, func(e *Event) { s := `"string"`; e.Log.Metadata = &s }} {
		copy := e
		mutate(&copy)
		if copy.Validate() == nil {
			t.Fatal("invalid event accepted", copy)
		}
	}
	e.Log.Metadata = SafeMetadata(map[string]any{"email": "synthetic@example.com", "password": "secret"})
	if e.Validate() != nil || strings.Contains(*e.Log.Metadata, "secret") {
		t.Fatal("redacted metadata rejected/leaked")
	}
	for _, action := range []string{"SIGNUP", "SEND_OTP", "RESET_PASSWORD", "UPLOAD_FILE", "SYNC_NORMALIZED_FILE", "DELETE_FILE", "RESTORE_FILE", "GRAND_FILE_ACCESS", "REVOKE_FILE_ACCESS", "REPLACE_FILE", "REVERT_FILE"} {
		if !Transactional(action) {
			t.Fatal("mutation missing", action)
		}
	}
	if Transactional("LOGIN") || Transactional("ACCESS_FILE") {
		t.Fatal("read-only activity suppressed")
	}
}
