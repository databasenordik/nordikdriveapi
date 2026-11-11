package outbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"google.golang.org/api/cloudtasks/v2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type pendingKey struct{}

// Pending captures IDs only. Publishing re-reads committed rows, so transactions
// that rolled back can never produce an email task.
type Pending struct {
	mu    sync.Mutex
	ids   map[string]bool
	files map[uint]bool
}

func Capture(ctx context.Context) (context.Context, *Pending) {
	pending := &Pending{ids: make(map[string]bool), files: make(map[uint]bool)}
	return context.WithValue(ctx, pendingKey{}, pending), pending
}

func track(ctx context.Context, id string) {
	if pending, ok := ctx.Value(pendingKey{}).(*Pending); ok {
		pending.mu.Lock()
		pending.ids[id] = true
		pending.mu.Unlock()
	}
}

// Track re-registers a durable intent when an idempotent operation replays a
// cached response instead of executing Put again.
func Track(ctx context.Context, id string) { track(ctx, id) }

func TrackFileVersion(ctx context.Context, id uint) {
	if pending, ok := ctx.Value(pendingKey{}).(*Pending); ok {
		pending.mu.Lock()
		pending.files[id] = true
		pending.mu.Unlock()
	}
}

func (p *Pending) FileVersions() []uint {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := make([]uint, 0, len(p.files))
	for id := range p.files {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

type EventPublisher struct {
	API                       *cloudtasks.Service
	OTP, Normal, URL, Account string
}

func EventDispatchEnabled() (bool, error) {
	value := os.Getenv("NOTIFICATION_EVENT_DISPATCH")
	if value == "" {
		return false, nil
	}
	enabled, err := strconv.ParseBool(value)
	if err != nil {
		return false, errors.New("NOTIFICATION_EVENT_DISPATCH must be a boolean")
	}
	return enabled, nil
}

func NewEventPublisher(ctx context.Context, otp, normal, address, account string, options ...option.ClientOption) (*EventPublisher, error) {
	if otp == "" || normal == "" || address == "" || account == "" {
		return nil, errors.New("event notification dispatch settings are required")
	}
	api, err := cloudtasks.NewService(ctx, options...)
	if err != nil {
		return nil, errors.New("notification task publisher unavailable")
	}
	return &EventPublisher{API: api, OTP: otp, Normal: normal, URL: address, Account: account}, nil
}

func (p *EventPublisher) enqueue(ctx context.Context, row Record) error {
	parent := p.Normal
	if row.Kind == "otp" {
		parent = p.OTP
	}
	body, _ := json.Marshal(map[string]string{"delivery_id": row.ID})
	task := &cloudtasks.Task{Name: fmt.Sprintf("%s/tasks/%s-source-%d", parent, row.ID, row.Attempts), DispatchDeadline: "45s", HttpRequest: &cloudtasks.HttpRequest{
		HttpMethod: "POST", Url: p.URL + "/internal/v1/deliver", Headers: map[string]string{"Content-Type": "application/json"},
		Body: base64.StdEncoding.EncodeToString(body), OidcToken: &cloudtasks.OidcToken{Audience: p.URL, ServiceAccountEmail: p.Account},
	}}
	var last error
	for attempt := 0; attempt < 2; attempt++ {
		call, stop := context.WithTimeout(ctx, 2*time.Second)
		_, last = p.API.Projects.Locations.Queues.Tasks.Create(parent, &cloudtasks.CreateTaskRequest{Task: task}).Context(call).Do()
		stop()
		var apiError *googleapi.Error
		if last == nil || (errors.As(last, &apiError) && apiError.Code == 409) {
			return nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	return errors.New("notification task publication failed")
}

func (p *Pending) Publish(ctx context.Context, db *gorm.DB, publisher *EventPublisher) error {
	if publisher == nil {
		return nil
	}
	p.mu.Lock()
	ids := make([]string, 0, len(p.ids))
	for id := range p.ids {
		ids = append(ids, id)
	}
	p.mu.Unlock()
	if len(ids) == 0 {
		return nil
	}
	sort.Strings(ids)
	var rows []Record
	if err := db.WithContext(ctx).Where("id IN ? AND state = ?", ids, "pending").Find(&rows).Error; err != nil {
		return errors.New("committed notification intents unavailable")
	}
	for _, row := range rows {
		var claimed Record
		now := time.Now().UTC()
		err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			query := tx.Where("id = ? AND state = ? AND (lease_until IS NULL OR lease_until < ?)", row.ID, "pending", now)
			if tx.Dialector.Name() == "postgres" {
				query = query.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"})
			}
			if err := query.First(&claimed).Error; err != nil {
				return err
			}
			claimed.Attempts++
			return tx.Model(&claimed).Updates(map[string]any{"attempts": claimed.Attempts, "lease_until": now.Add(45 * time.Second)}).Error
		})
		if errors.Is(err, gorm.ErrRecordNotFound) {
			continue
		}
		if err != nil {
			return errors.New("notification event claim unavailable")
		}
		publishErr := publisher.enqueue(ctx, claimed)
		release, stop := context.WithTimeout(context.WithoutCancel(ctx), 500*time.Millisecond)
		releaseErr := db.WithContext(release).Model(&Record{}).Where("id = ? AND state = ? AND attempts = ?", claimed.ID, "pending", claimed.Attempts).Updates(map[string]any{"lease_until": nil, "next_attempt": now.Add(time.Minute)}).Error
		stop()
		if err := errors.Join(publishErr, releaseErr); err != nil {
			return errors.New("notification event publication incomplete")
		}
	}
	return nil
}

// PublishPending runs before its enclosing HTTP/job handler returns, never in
// an untracked goroutine. An infrastructure failure leaves the durable outbox
// intact for the twelve-hour safety sweep and does not undo business success.
func PublishPending(ctx context.Context, db *gorm.DB, publisher *EventPublisher, pending *Pending) {
	phase, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stop()
	if err := pending.Publish(phase, db, publisher); err != nil {
		slog.WarnContext(phase, "notification_task_publication_failed")
	}
}
