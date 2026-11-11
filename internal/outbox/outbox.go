// Package outbox records producer-owned notification intent in a business transaction.
package outbox

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"nordik-drive-api/internal/notificationcontract"
)

type Record struct {
	Kind          string
	Deadline      time.Time
	PayloadErased bool
	ID            string `gorm:"primaryKey"`
	Producer      string `gorm:"uniqueIndex:notification_outbox_event"`
	EventKey      string `gorm:"uniqueIndex:notification_outbox_event"`
	Command       string
	State         string
	DeliveryID    string
	NextAttempt   time.Time
	LeaseUntil    *time.Time
	Attempts      int
	CreatedAt     time.Time
}

func (Record) TableName() string { return "notification_outbox" }

type Writer struct {
	Codec     *notificationcontract.Codec
	Publisher *EventPublisher
}

func (w *Writer) Put(tx *gorm.DB, command notificationcontract.Command, payload notificationcontract.Payload) (string, error) {
	command.Version = 1
	command.ID = uuid.NewString()
	if command.CreatedAt.IsZero() {
		command.CreatedAt = time.Now().UTC()
	}
	if command.Deadline.IsZero() {
		command.Deadline = command.CreatedAt.Add(24 * time.Hour)
	}
	sealed, err := w.Codec.Seal(command, payload)
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(sealed)
	record := Record{ID: sealed.ID, Producer: sealed.Producer, EventKey: sealed.Key, Command: string(b), State: "pending", NextAttempt: sealed.CreatedAt, CreatedAt: sealed.CreatedAt, Kind: sealed.Kind, Deadline: sealed.Deadline}
	result := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "producer"}, {Name: "event_key"}}, DoNothing: true}).Create(&record)
	if result.Error != nil {
		return "", result.Error
	}
	if result.RowsAffected == 1 {
		track(tx.Statement.Context, record.ID)
		return record.ID, nil
	}
	var existing Record
	if err := tx.Where("producer = ? AND event_key = ?", sealed.Producer, sealed.Key).First(&existing).Error; err != nil {
		return "", err
	}
	var previous notificationcontract.Command
	if json.Unmarshal([]byte(existing.Command), &previous) != nil || !hmac.Equal([]byte(previous.Fingerprint), []byte(w.Codec.Fingerprint(previous, payload))) {
		return "", errors.New("notification event key conflicts with its original payload")
	}
	track(tx.Statement.Context, existing.ID)
	return existing.ID, nil
}

// Send records one logical event per recipient, using the caller's transaction.
func (w *Writer) Send(tx *gorm.DB, producer, event string, to []string, subject, html string, refs ...notificationcontract.Attachment) error {
	return w.SendAt(tx, producer, event, time.Now().UTC(), to, subject, html, refs...)
}

// SendAt keeps a deferred producer event's deadline tied to its business time.
func (w *Writer) SendAt(tx *gorm.DB, producer, event string, createdAt time.Time, to []string, subject, html string, refs ...notificationcontract.Attachment) error {
	for _, recipient := range to {
		if _, err := w.Put(tx, notificationcontract.Command{Producer: producer, Key: fmt.Sprintf("%s:%s", event, recipient), Kind: "normal", CreatedAt: createdAt, Deadline: createdAt.Add(24 * time.Hour)}, notificationcontract.Payload{To: recipient, Subject: subject, HTML: html, Attachments: refs}); err != nil {
			return err
		}
	}
	return nil
}

type Remote interface {
	Accept(context.Context, notificationcontract.Command) (string, error)
	Status(context.Context, []string) (map[string]string, error)
	Recover(context.Context) error
}
type Dispatcher struct {
	DB     *gorm.DB
	Remote Remote
}

func (d *Dispatcher) Run(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	// Separate deadlines reserve time for repair and reconciliation even when
	// intake is unavailable. Failures do not suppress later phases.
	return errors.Join(
		RunPhase(ctx, 5*time.Second, d.Dispatch),
		RunPhase(ctx, 5*time.Second, d.Remote.Recover),
		RunPhase(ctx, 2*time.Second, d.Cleanup),
		RunPhase(ctx, 3*time.Second, d.Reconcile),
	)
}

// RunPhase bounds synchronous work and always joins it before returning.
func RunPhase(ctx context.Context, budget time.Duration, run func(context.Context) error) error {
	phase, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	return run(phase)
}

// Dispatch reserves five of ten slots for each kind; an empty group yields its
// unused slots. Two joined branches prevent a stalled kind blocking its peer.
func (d *Dispatcher) Dispatch(ctx context.Context) error {
	ctx, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	var failures error
	for range 100 {
		count, err := d.dispatchBatch(ctx)
		failures = errors.Join(failures, err)
		if count == 0 || ctx.Err() != nil {
			return failures
		}
	}
	return errors.Join(failures, errors.New("notification dispatch batch limit reached"))
}

func (d *Dispatcher) dispatchBatch(ctx context.Context) (int, error) {
	now := time.Now().UTC()
	load := func(kind string, limit int) ([]Record, error) {
		var rows []Record
		query := d.DB.WithContext(ctx).Where("kind = ? AND state = ? AND next_attempt <= ? AND (lease_until IS NULL OR lease_until < ?)", kind, "pending", now, now)
		if kind == "otp" {
			query = query.Order("deadline ASC")
		}
		err := query.Order("created_at ASC").Order("id ASC").Limit(limit).Find(&rows).Error
		return rows, err
	}
	otp, err := load("otp", 5)
	if err != nil {
		return 0, err
	}
	normal, err := load("normal", 10-len(otp))
	if err != nil {
		return 0, err
	}
	if len(normal) < 5 {
		otp, err = load("otp", 10-len(normal))
		if err != nil {
			return 0, err
		}
	}
	var joined sync.WaitGroup
	var failures [2]error
	for group, rows := range [][]Record{otp, normal} {
		joined.Add(1)
		go func() {
			defer joined.Done()
			for _, row := range rows {
				if ctx.Err() != nil {
					failures[group] = errors.Join(failures[group], ctx.Err())
					break
				}
				failures[group] = errors.Join(failures[group], d.dispatchRecord(ctx, row.ID))
			}
		}()
	}
	joined.Wait()
	return len(otp) + len(normal), errors.Join(failures[:]...)
}

func (d *Dispatcher) dispatchRecord(ctx context.Context, id string) error {
	var record Record
	now := time.Now().UTC()
	lease := now.Add(45 * time.Second)
	err := d.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.Where("id = ? AND state = ? AND next_attempt <= ? AND (lease_until IS NULL OR lease_until < ?)", id, "pending", now, now)
		if tx.Dialector.Name() == "postgres" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"})
		}
		if err := query.First(&record).Error; err != nil {
			return err
		}
		return tx.Model(&record).Updates(map[string]any{"lease_until": lease, "attempts": gorm.Expr("attempts + 1")}).Error
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var command notificationcontract.Command
	if json.Unmarshal([]byte(record.Command), &command) != nil {
		// Quarantine a corrupt intent without retaining unreadable payload or
		// repeatedly blocking other records. Keep its persistent event key.
		err := d.DB.WithContext(ctx).Model(&record).Updates(map[string]any{"state": "failed", "command": "", "payload_erased": true, "lease_until": nil}).Error
		return errors.Join(errors.New("invalid stored outbox command"), err)
	}
	state, delivery := "pending", ""
	var intakeErr error
	if !command.Deadline.After(now) {
		state = "expired"
	} else {
		id, acceptErr := d.Remote.Accept(ctx, command)
		if acceptErr == nil {
			state, delivery = "accepted", id
		} else {
			intakeErr = errors.New("notification intake unavailable")
		}
	}
	if err := d.DB.WithContext(ctx).Model(&record).Where("state = ?", "pending").Updates(map[string]any{"state": state, "delivery_id": delivery, "lease_until": nil, "next_attempt": now.Add(time.Minute)}).Error; err != nil {
		return err
	}
	return intakeErr
}
