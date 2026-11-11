package file

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"google.golang.org/api/cloudtasks/v2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"gorm.io/gorm"
	"nordik-drive-api/internal/workload"
)

type VersionEventPublisher struct {
	API                 *cloudtasks.Service
	DB                  *gorm.DB
	Queue, URL, Account string
}

func NewVersionEventPublisher(ctx context.Context, db *gorm.DB, queue, address, account string, opts ...option.ClientOption) (*VersionEventPublisher, error) {
	if queue == "" {
		return nil, nil
	}
	if address == "" || account == "" {
		return nil, errors.New("file event settings are incomplete")
	}
	api, err := cloudtasks.NewService(ctx, opts...)
	if err != nil {
		return nil, errors.New("file event task client unavailable")
	}
	return &VersionEventPublisher{API: api, DB: db, Queue: queue, URL: address, Account: account}, nil
}

func (p *VersionEventPublisher) Publish(ctx context.Context, ids []uint) error {
	if p == nil || len(ids) == 0 {
		return nil
	}
	var rows []FileVersionReconciliationJob
	if err := p.DB.WithContext(ctx).Where("id IN ? AND status = ?", ids, reconciliationJobStatusPending).Find(&rows).Error; err != nil {
		return errors.New("committed file events unavailable")
	}
	for _, row := range rows {
		body, _ := json.Marshal(map[string]uint{"job_id": row.ID})
		task := &cloudtasks.Task{Name: fmt.Sprintf("%s/tasks/file-version-%d-%d", p.Queue, row.ID, row.Attempts), DispatchDeadline: "930s", HttpRequest: &cloudtasks.HttpRequest{
			HttpMethod: "POST", Url: p.URL + "/internal/v1/work/file-version", Headers: map[string]string{"Content-Type": "application/json"}, Body: base64.StdEncoding.EncodeToString(body), OidcToken: &cloudtasks.OidcToken{Audience: p.URL, ServiceAccountEmail: p.Account},
		}}
		var last error
		for range 2 {
			call, stop := context.WithTimeout(ctx, 2*time.Second)
			_, last = p.API.Projects.Locations.Queues.Tasks.Create(p.Queue, &cloudtasks.CreateTaskRequest{Task: task}).Context(call).Do()
			stop()
			var apiError *googleapi.Error
			if last == nil || (errors.As(last, &apiError) && apiError.Code == 409) {
				last = nil
				break
			}
			if ctx.Err() != nil {
				break
			}
		}
		if last != nil {
			return errors.New("file event publication failed")
		}
	}
	return nil
}

func NewVersionEventHandler(db *gorm.DB, audience, account string) (http.Handler, error) {
	if audience == "" || account == "" {
		return nil, errors.New("file event caller and audience required")
	}
	return versionEventHandler(db, func(ctx context.Context, token string) error {
		verify, stop := context.WithTimeout(ctx, 3*time.Second)
		defer stop()
		return workload.Verify(verify, token, audience, map[string]bool{account: true})
	}), nil
}

func versionEventHandler(db *gorm.DB, authorize func(context.Context, string) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		if authorize(r.Context(), strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")) != nil {
			http.Error(w, "forbidden", 403)
			return
		}
		var input struct {
			JobID uint `json:"job_id"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&input) != nil || decoder.Decode(&struct{}{}) != io.EOF || input.JobID == 0 {
			http.Error(w, "valid file job required", 400)
			return
		}
		var row FileVersionReconciliationJob
		if err := db.WithContext(r.Context()).First(&row, "id = ?", input.JobID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				w.WriteHeader(204)
			} else {
				http.Error(w, "file work unavailable", 503)
			}
			return
		}
		if row.Status == reconciliationJobStatusCompleted || row.Status == reconciliationJobStatusFailed {
			w.WriteHeader(204)
			return
		}
		phase, stop := context.WithTimeout(r.Context(), 900*time.Second)
		defer stop()
		_, err := RunVersionReconciliationJobs(db.WithContext(phase), VersionReconciliationRunOptions{MaxJobs: 1, JobID: &input.JobID})
		if err != nil {
			http.Error(w, "file processing incomplete", 503)
			return
		}
		if err := db.WithContext(phase).First(&row, "id = ?", input.JobID).Error; err != nil {
			http.Error(w, "file status unavailable", 503)
			return
		}
		slog.InfoContext(phase, "file_version_event", "job_id", input.JobID, "state", row.Status)
		if row.Status != reconciliationJobStatusCompleted && row.Status != reconciliationJobStatusFailed {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "file processing pending", 503)
			return
		}
		w.WriteHeader(204)
	})
}
