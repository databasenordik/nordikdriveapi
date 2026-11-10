// Package outbox records producer-owned notification intent in a business transaction.
package outbox

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
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

type Writer struct{ Codec *notificationcontract.Codec }

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
	for range 10 {
		var record Record
		now := time.Now().UTC()
		lease := now.Add(45 * time.Second)
		err := d.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			query := tx.Where("state = ? AND next_attempt <= ? AND (lease_until IS NULL OR lease_until < ?)", "pending", now, now).Order("created_at ASC")
			if tx.Dialector.Name() == "postgres" {
				query = query.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"})
			}
			if err := query.First(&record).Error; err != nil {
				return err
			}
			return tx.Model(&record).Updates(map[string]any{"lease_until": lease, "attempts": gorm.Expr("attempts + 1")}).Error
		})
		if errors.Is(err, gorm.ErrRecordNotFound) {
			break
		}
		if err != nil {
			return err
		}
		var command notificationcontract.Command
		if json.Unmarshal([]byte(record.Command), &command) != nil {
			return errors.New("invalid stored outbox command")
		}
		state, delivery := "pending", ""
		if !command.Deadline.After(now) {
			state = "expired"
		} else {
			id, acceptErr := d.Remote.Accept(ctx, command)
			if acceptErr == nil {
				state, delivery = "accepted", id
			}
		}
		if err := d.DB.WithContext(ctx).Model(&record).Where("state = ?", "pending").Updates(map[string]any{"state": state, "delivery_id": delivery, "lease_until": nil, "next_attempt": now.Add(time.Minute)}).Error; err != nil {
			return err
		}
	}
	if err := d.Cleanup(ctx); err != nil {
		return err
	}
	if err := d.Remote.Recover(ctx); err != nil {
		return err
	}
	return d.Reconcile(ctx)
}
