package outbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"gorm.io/gorm"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/api/cloudtasks/v2"
	"google.golang.org/api/option"
)

func TestEventPublisherOnlyPublishesCommittedIDsAndRetriesStableTask(t *testing.T) {
	db, writer, _ := outboxFixture(t)
	ctx, pending := Capture(context.Background())
	if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := writer.Send(tx, "records", "rolled-back", []string{"private@example.test"}, "Subject", "Body"); err != nil {
			return err
		}
		return errors.New("business rollback")
	}); err == nil {
		t.Fatal("transaction unexpectedly committed")
	}
	var names []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request cloudtasks.CreateTaskRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		names = append(names, request.Task.Name)
		body, _ := base64.StdEncoding.DecodeString(request.Task.HttpRequest.Body)
		var ids map[string]string
		if json.Unmarshal(body, &ids) != nil || len(ids) != 1 || ids["delivery_id"] == "" || strings.Contains(string(body), "private") {
			t.Error("task exposed more than its opaque identifier")
		}
		var value int
		if err := db.WithContext(r.Context()).Raw("SELECT 1").Scan(&value).Error; err != nil {
			t.Error("SQL connection held during enqueue", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if len(names) == 1 {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error":{"code":503,"message":"synthetic"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(request.Task)
	}))
	defer server.Close()
	publisher, err := NewEventPublisher(ctx, "projects/p/locations/r/queues/otp", "projects/p/locations/r/queues/normal", "https://notifications.example.test", "tasks@example.test", option.WithEndpoint(server.URL+"/"), option.WithHTTPClient(server.Client()), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	if err := pending.Publish(ctx, db, publisher); err != nil || len(names) != 0 {
		t.Fatal("rolled back email produced a task", err)
	}
	if err := writer.Send(db.WithContext(ctx), "records", "committed", []string{"private@example.test"}, "Subject", "Body"); err != nil {
		t.Fatal(err)
	}
	if err := pending.Publish(ctx, db, publisher); err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != names[1] {
		t.Fatal("publication retry created another task generation")
	}
	var row Record
	if err := db.First(&row, "event_key = ?", "committed:private@example.test").Error; err != nil || row.State != "pending" || row.LeaseUntil != nil || row.PayloadErased {
		t.Fatal("task creation incorrectly marked physical delivery", err)
	}
}
