// Package auditoutbox persists activity in the producer's business transaction.
package auditoutbox

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
	"gorm.io/gorm"
	"nordik-drive-api/internal/activity"
	"nordik-drive-api/internal/observability"
)

type Record struct {
	ID          string `gorm:"primaryKey"`
	Fingerprint string
	Payload     string `gorm:"type:text"`
	State       string
	LogID       uint
	Attempts    int
	NextAttempt time.Time
	LeaseUntil  *time.Time
	CreatedAt   time.Time
}

func (Record) TableName() string { return "audit_outbox" }

type Writer struct {
	Producer  string
	Publisher Publisher
}

func (w *Writer) Put(tx *gorm.DB, log activity.Log, metadata any) error {
	if w == nil {
		return nil
	}
	log.ID = 0
	log.Metadata = activity.SafeMetadata(metadata)
	if log.CreatedAt.IsZero() {
		log.CreatedAt = time.Now().UTC()
	}
	ctx := tx.Statement.Context
	event := activity.Event{ID: uuid.NewString(), Version: 1, Producer: w.Producer, Log: log, RequestID: observability.RequestID(ctx)}
	if span := trace.SpanContextFromContext(ctx); span.IsValid() {
		event.TraceID = span.TraceID().String()
	}
	if err := event.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return errors.New("audit event encoding failed")
	}
	if len(payload) > 120*1024 {
		return errors.New("audit event exceeds the bounded private contract")
	}
	row := Record{ID: event.ID, Fingerprint: event.Fingerprint(), Payload: string(payload), State: "pending", CreatedAt: time.Now().UTC(), NextAttempt: time.Now().UTC()}
	if err := tx.Create(&row).Error; err != nil {
		return errors.New("audit intent unavailable")
	}
	if pending, ok := ctx.Value(pendingKey{}).(*Pending); ok {
		pending.mu.Lock()
		pending.ids[row.ID] = true
		pending.mu.Unlock()
	}
	return nil
}

type pendingKey struct{}
type Pending struct {
	mu  sync.Mutex
	ids map[string]bool
}

func Capture(ctx context.Context) (context.Context, *Pending) {
	p := &Pending{ids: map[string]bool{}}
	return context.WithValue(ctx, pendingKey{}, p), p
}
func (p *Pending) Publish(ctx context.Context, db *gorm.DB, publisher Publisher) error {
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
	return (&Dispatcher{DB: db, Publisher: publisher}).dispatch(ctx, ids)
}

type Publisher interface {
	Enqueue(context.Context, string, int) error
}
type Dispatcher struct {
	DB        *gorm.DB
	Publisher Publisher
}

func (d *Dispatcher) Run(ctx context.Context) error {
	ctx, stop := context.WithTimeout(ctx, 20*time.Second)
	defer stop()
	var failures error
	for range 100 {
		count, err := d.dispatchBatch(ctx, nil)
		failures = errors.Join(failures, err)
		if count < 100 {
			return failures
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(failures, err)
		}
	}
	return errors.Join(failures, errors.New("audit reconciliation batch limit reached; pending events retained"))
}
func (d *Dispatcher) dispatch(ctx context.Context, ids []string) error {
	_, err := d.dispatchBatch(ctx, ids)
	return err
}
func (d *Dispatcher) dispatchBatch(ctx context.Context, ids []string) (int, error) {
	if d.Publisher == nil {
		return 0, errors.New("audit task publisher required")
	}
	now := time.Now().UTC()
	query := d.DB.WithContext(ctx).Where("state = ? AND next_attempt <= ? AND (lease_until IS NULL OR lease_until < ?)", "pending", now, now)
	if ids != nil {
		query = query.Where("id IN ?", ids)
	}
	var rows []Record
	if err := query.Order("created_at ASC").Order("id ASC").Limit(100).Find(&rows).Error; err != nil {
		return 0, errors.New("audit reconciliation unavailable")
	}
	var failures error
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return len(rows), errors.Join(failures, err)
		}
		// Conditional reservation makes simultaneous request/reconciliation dispatch
		// safe without holding a database connection during a Cloud Tasks request.
		claim := d.DB.WithContext(ctx).Model(&Record{}).Where("id = ? AND state = ? AND attempts = ? AND (lease_until IS NULL OR lease_until < ?)", row.ID, "pending", row.Attempts, now).Updates(map[string]any{"attempts": row.Attempts + 1, "lease_until": now.Add(time.Minute)})
		if claim.Error != nil {
			failures = errors.Join(failures, errors.New("audit reservation unavailable"))
			continue
		}
		if claim.RowsAffected != 1 {
			continue
		}
		err := d.Publisher.Enqueue(ctx, row.ID, row.Attempts+1)
		release, stop := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		update := d.DB.WithContext(release).Model(&Record{}).Where("id = ? AND state = ? AND attempts = ?", row.ID, "pending", row.Attempts+1).Updates(map[string]any{"lease_until": nil, "next_attempt": time.Now().UTC().Add(2 * time.Minute)}).Error
		stop()
		if err != nil || update != nil {
			failures = errors.Join(failures, errors.New("audit publication incomplete; durable intent retained"))
		}
	}
	return len(rows), failures
}
