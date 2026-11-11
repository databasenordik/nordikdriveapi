package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"nordik-drive-api/internal/notificationcontract"
	"nordik-drive-api/internal/workload"
)

func NewSourceHandler(db *gorm.DB, audience, reader string) (http.Handler, error) {
	if audience == "" || reader == "" {
		return nil, errors.New("notification source audience and reader identity are required")
	}
	return sourceHandler(db, func(ctx context.Context, token string) error {
		ctx, stop := context.WithTimeout(ctx, 3*time.Second)
		defer stop()
		return workload.Verify(ctx, token, audience, map[string]bool{reader: true})
	}), nil
}

func sourceHandler(db *gorm.DB, authorize func(context.Context, string) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if err := authorize(r.Context(), strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")); err != nil {
			http.Error(w, "forbidden", 403)
			return
		}
		var input struct {
			OutboxID   string `json:"outbox_id"`
			DeliveryID string `json:"delivery_id"`
			State      string `json:"state"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&input) != nil || decoder.Decode(&struct{}{}) != io.EOF {
			http.Error(w, "invalid request", 400)
			return
		}
		id := input.OutboxID
		if r.URL.Path == "/internal/v1/notification-outbox/read" {
			id = input.DeliveryID
		}
		if _, err := uuid.Parse(id); err != nil {
			http.Error(w, "valid notification identifier required", 400)
			return
		}
		var row Record
		if err := db.WithContext(r.Context()).First(&row, "id = ?", id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				http.Error(w, "notification not found", 404)
			} else {
				http.Error(w, "notification source unavailable", 503)
			}
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		switch r.URL.Path {
		case "/internal/v1/notification-outbox/read":
			result := notificationcontract.SourceRecord{State: row.State}
			if row.State == "pending" && !row.Deadline.After(time.Now().UTC()) {
				result.State = "expired"
			} else if row.State == "pending" && !row.PayloadErased {
				if json.Unmarshal([]byte(row.Command), &result.Command) != nil {
					http.Error(w, "stored notification unavailable", 503)
					return
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(result)
		case "/internal/v1/notification-outbox/receipt":
			if _, err := uuid.Parse(input.DeliveryID); err != nil || (row.DeliveryID != "" && row.DeliveryID != input.DeliveryID) {
				http.Error(w, "invalid delivery receipt", 409)
				return
			}
			state := input.State
			if state == "retry" || state == "queued" || state == "pending_enqueue" {
				state = "accepted"
			}
			if state != "accepted" && !deliveryTerminal(state) {
				http.Error(w, "invalid delivery state", 400)
				return
			}
			updates := map[string]any{"state": state, "delivery_id": input.DeliveryID, "lease_until": nil, "next_attempt": time.Now().UTC().Add(time.Minute)}
			if state == "provider_accepted" || (row.Kind == "otp" && deliveryTerminal(state)) {
				updates["command"], updates["payload_erased"] = erasePayload(row.Command), true
			}
			if err := db.WithContext(r.Context()).Model(&Record{}).Where("id = ? AND state NOT IN ?", id, []string{"provider_accepted", "processed", "expired", "superseded", "failed", "unknown"}).Updates(updates).Error; err != nil {
				http.Error(w, "notification receipt unavailable", 503)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func erasePayload(encoded string) string {
	var command notificationcontract.Command
	if json.Unmarshal([]byte(encoded), &command) != nil {
		return ""
	}
	command.Ciphertext = ""
	metadata, _ := json.Marshal(command)
	return string(metadata)
}
