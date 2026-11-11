package file

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/cloudtasks/v2"
	"google.golang.org/api/option"
	"gorm.io/gorm"
	"nordik-drive-api/internal/outbox"
)

func TestFileEventsPublishOnlyCommittedIDsAndDeduplicate(t *testing.T) {
	db := newTestDB(t)
	ctx, pending := outbox.Capture(context.Background())
	if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row := FileVersionReconciliationJob{Status: reconciliationJobStatusPending}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		outbox.TrackFileVersion(tx.Statement.Context, row.ID)
		return errors.New("business rollback")
	}); err == nil {
		t.Fatal("expected rollback")
	}
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var request cloudtasks.CreateTaskRequest
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			t.Error("invalid task")
		}
		body, _ := base64.StdEncoding.DecodeString(request.Task.HttpRequest.Body)
		var fields map[string]uint
		if json.Unmarshal(body, &fields) != nil || len(fields) != 1 || fields["job_id"] == 0 {
			t.Error("task must contain only job ID")
		}
		if request.Task.HttpRequest.OidcToken.Audience != "https://worker.example.test" || !strings.HasSuffix(request.Task.HttpRequest.Url, "/internal/v1/work/file-version") {
			t.Error("wrong task target")
		}
		var count int
		if db.Raw("SELECT 1").Scan(&count).Error != nil {
			t.Error("connection held while publishing")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(409)
		_, _ = w.Write([]byte(`{"error":{"code":409,"message":"exists"}}`))
	}))
	defer server.Close()
	publisher, err := NewVersionEventPublisher(ctx, db, "projects/p/locations/r/queues/work", "https://worker.example.test", "tasks@example.test", option.WithEndpoint(server.URL+"/"), option.WithHTTPClient(server.Client()), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.Publish(ctx, pending.FileVersions()); err != nil || calls != 0 {
		t.Fatal("rolled-back job published", err)
	}
	row := FileVersionReconciliationJob{Status: reconciliationJobStatusPending}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	outbox.TrackFileVersion(ctx, row.ID)
	outbox.TrackFileVersion(ctx, row.ID)
	if err := publisher.Publish(ctx, pending.FileVersions()); err != nil || calls != 1 {
		t.Fatal("committed task not published/deduplicated", calls, err)
	}
	if err := publisher.Publish(ctx, nil); err != nil {
		t.Fatal(err)
	}
}

func TestFileEventTargetingAuthorizationAndRedelivery(t *testing.T) {
	db := newTestDB(t)
	rows := []FileVersionReconciliationJob{
		{FileVersionID: 1, Status: reconciliationJobStatusPending, AvailableAt: time.Now().Add(-time.Hour)},
		{FileVersionID: 2, Status: reconciliationJobStatusPending, AvailableAt: time.Now().Add(time.Hour)},
		{FileVersionID: 3, Status: reconciliationJobStatusCompleted},
		{FileVersionID: 4, Status: reconciliationJobStatusFailed},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	// A task for a future job must not process an unrelated due job.
	if _, ok, err := claimNextVersionReconciliationJob(db, &rows[1].ID); err != nil || ok {
		t.Fatal("claimed unrelated job", err)
	}
	handler := versionEventHandler(db, func(_ context.Context, token string) error {
		if token != "task-identity" {
			return errors.New("forbidden")
		}
		return nil
	})
	for _, test := range []struct {
		token, body string
		status      int
	}{
		{"scheduler-identity", `{"job_id":1}`, 403},
		{"task-identity", `{"job_id":0}`, 400},
		{"task-identity", `{"job_id":2,"unexpected":true}`, 400},
		{"task-identity", `{"job_id":2} {}`, 400},
		{"task-identity", `{"job_id":999999}`, 204},
		{"task-identity", `{"job_id":2}`, 503},
		{"task-identity", `{"job_id":3}`, 204},
		{"task-identity", `{"job_id":4}`, 204},
	} {
		r := httptest.NewRequest("POST", "/internal/v1/work/file-version", strings.NewReader(test.body))
		r.Header.Set("Authorization", "Bearer "+test.token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != test.status {
			t.Fatalf("%s: status %d expected %d", test.body, w.Code, test.status)
		}
	}
	var untouched FileVersionReconciliationJob
	if err := db.First(&untouched, rows[0].ID).Error; err != nil || untouched.Attempts != 0 || untouched.Status != reconciliationJobStatusPending {
		t.Fatal("unrelated work was processed", err)
	}
}
