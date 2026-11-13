package auditoutbox

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"nordik-drive-api/internal/activity"
	"nordik-drive-api/internal/workload"
)

type Receipt struct {
	ID          string `json:"event_id"`
	Fingerprint string `json:"fingerprint"`
	LogID       uint   `json:"log_id"`
}
type SourceRecord struct {
	Event   *activity.Event `json:"event,omitempty"`
	Receipt Receipt         `json:"receipt"`
}
type Authorize func(context.Context, string) error

func GoogleAuthorize(audience string, accounts ...string) (Authorize, error) {
	if audience == "" || len(accounts) == 0 {
		return nil, errors.New("audit audience and caller identities required")
	}
	allow := map[string]bool{}
	for _, account := range accounts {
		if account == "" {
			return nil, errors.New("audit caller identity required")
		}
		allow[account] = true
	}
	return func(ctx context.Context, token string) error {
		ctx, stop := context.WithTimeout(ctx, 3*time.Second)
		defer stop()
		return workload.Verify(ctx, token, audience, allow)
	}, nil
}
func Decode(w http.ResponseWriter, r *http.Request, destination any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128*1024))
	decoder.DisallowUnknownFields()
	if decoder.Decode(destination) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("invalid audit request")
	}
	return nil
}
func SourceHandler(db *gorm.DB, authorize Authorize) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		if authorize(r.Context(), strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")) != nil {
			http.Error(w, "forbidden", 403)
			return
		}
		var input Receipt
		if Decode(w, r, &input) != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		if _, err := uuid.Parse(input.ID); err != nil {
			http.Error(w, "invalid event identifier", 400)
			return
		}
		var row Record
		if err := db.WithContext(r.Context()).First(&row, "id = ?", input.ID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				http.Error(w, "event not found", 404)
			} else {
				http.Error(w, "audit owner unavailable", 503)
			}
			return
		}
		switch r.URL.Path {
		case "/internal/v1/audit-outbox/read":
			result := SourceRecord{Receipt: Receipt{ID: row.ID, Fingerprint: row.Fingerprint, LogID: row.LogID}}
			if row.State == "pending" {
				var event activity.Event
				if json.Unmarshal([]byte(row.Payload), &event) != nil || event.Validate() != nil || event.Fingerprint() != row.Fingerprint {
					http.Error(w, "stored audit event invalid", 503)
					return
				}
				result.Event = &event
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(result)
		case "/internal/v1/audit-outbox/ack":
			if input.LogID == 0 || subtle.ConstantTimeCompare([]byte(input.Fingerprint), []byte(row.Fingerprint)) != 1 || (row.LogID != 0 && row.LogID != input.LogID) {
				http.Error(w, "invalid audit receipt", 409)
				return
			}
			result := db.WithContext(r.Context()).Model(&Record{}).Where("id = ? AND fingerprint = ? AND state = ?", row.ID, row.Fingerprint, "pending").Updates(map[string]any{"state": "ingested", "log_id": input.LogID, "payload": "", "lease_until": nil})
			if result.Error != nil {
				http.Error(w, "audit acknowledgment unavailable", 503)
				return
			}
			w.WriteHeader(204)
		default:
			w.WriteHeader(404)
		}
	})
}

func DispatchHandler(dispatcher *Dispatcher, authorize Authorize) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		if authorize(r.Context(), strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")) != nil {
			http.Error(w, "forbidden", 403)
			return
		}
		if r.URL.Path != "/internal/v1/audit-outbox/dispatch" {
			w.WriteHeader(404)
			return
		}
		ctx, stop := context.WithTimeout(r.Context(), 20*time.Second)
		defer stop()
		if dispatcher.Run(ctx) != nil {
			http.Error(w, "audit dispatch incomplete", 503)
			return
		}
		w.WriteHeader(204)
	})
}
