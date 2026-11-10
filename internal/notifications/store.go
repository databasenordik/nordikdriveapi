package notifications

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrConflict = errors.New("notification key conflict")
var ErrBusy = errors.New("notification is already being processed")

type Delivery struct {
	MaxAttempts        int    `gorm:"not null;default:5"`
	ID                 string `gorm:"primaryKey"`
	Producer           string
	EventKey           string
	Command            string
	Fingerprint        string
	State              string
	Kind               string
	Deadline           time.Time
	Supersession       string
	Sequence           int64
	Attempts           int
	DispatchGeneration int
	NextAttempt        time.Time
	LeaseUntil         *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

func (Delivery) TableName() string { return "notifications.deliveries" }
func (d Delivery) attemptLimit() int {
	if d.MaxAttempts < 1 {
		return 5
	}
	return d.MaxAttempts
}

type Attempt struct {
	ID         uint
	DeliveryID string
	Number     int
	Outcome    string
	CreatedAt  time.Time
}

func (Attempt) TableName() string { return "notifications.attempts" }

type Repository interface {
	Replay(context.Context, string, string, string, string, time.Time) error
	Prune(context.Context, time.Time) error
	Accept(context.Context, Delivery) (Delivery, error)
	Claim(context.Context, string, time.Time) (Delivery, bool, error)
	Finish(context.Context, Delivery, string, time.Time) error
	Due(context.Context, time.Time) ([]Delivery, error)
	Queued(context.Context, Delivery, time.Time) error
	Statuses(context.Context, []string) (map[string]string, error)
}
type Store struct{ DB *gorm.DB }

func locked(tx *gorm.DB) *gorm.DB {
	if tx.Dialector.Name() == "postgres" {
		return tx.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	return tx
}
func terminal(state string) bool {
	switch state {
	case "provider_accepted", "failed", "expired", "superseded", "unknown":
		return true
	}
	return false
}
func (s *Store) Accept(ctx context.Context, input Delivery) (Delivery, error) {
	var result Delivery
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		insert := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "producer"}, {Name: "event_key"}}, DoNothing: true}).Create(&input)
		if insert.Error != nil {
			return insert.Error
		}
		if err := tx.Where("producer = ? AND event_key = ?", input.Producer, input.EventKey).First(&result).Error; err != nil {
			return err
		}
		if result.Fingerprint != input.Fingerprint {
			return ErrConflict
		}
		return nil
	})
	return result, err
}
func (s *Store) Claim(ctx context.Context, id string, now time.Time) (Delivery, bool, error) {
	var row Delivery
	send := false
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := locked(tx).First(&row, "id = ?", id).Error; err != nil {
			return err
		}
		if terminal(row.State) {
			return nil
		}
		if row.State == "sending" {
			if row.LeaseUntil != nil && row.LeaseUntil.After(now) {
				return ErrBusy
			}
			return tx.Model(&row).Updates(map[string]any{"state": "unknown", "lease_until": nil, "updated_at": now}).Error
		}
		state := ""
		if !row.Deadline.After(now) {
			state = "expired"
		} else if row.Attempts >= row.attemptLimit() {
			state = "failed"
		}
		if state == "" && row.Supersession != "" {
			var count int64
			if err := tx.Model(&Delivery{}).Where("producer = ? AND supersession = ? AND sequence > ?", row.Producer, row.Supersession, row.Sequence).Count(&count).Error; err != nil {
				return err
			}
			if count > 0 {
				state = "superseded"
			}
		}
		if state != "" {
			return tx.Model(&row).Updates(map[string]any{"state": state, "command": "", "lease_until": nil, "updated_at": now}).Error
		}
		if row.State == "retry" && row.NextAttempt.After(now) {
			return ErrBusy
		}
		lease := now.Add(time.Minute)
		row.Attempts++
		row.State = "sending"
		row.LeaseUntil = &lease
		if err := tx.Model(&row).Updates(map[string]any{"state": "sending", "attempts": row.Attempts, "lease_until": lease, "updated_at": now}).Error; err != nil {
			return err
		}
		send = true
		return nil
	})
	return row, send, err
}
func (s *Store) Finish(ctx context.Context, row Delivery, state string, now time.Time) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		updates := map[string]any{"state": state, "lease_until": nil, "updated_at": now, "next_attempt": now.Add(time.Minute)}
		if row.Kind == "otp" && terminal(state) {
			updates["command"] = ""
		}
		result := tx.Model(&Delivery{}).Where("id = ? AND state = ? AND attempts = ?", row.ID, "sending", row.Attempts).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrConflict
		}
		return tx.Create(&Attempt{DeliveryID: row.ID, Number: row.Attempts, Outcome: state, CreatedAt: now}).Error
	})
}
func (s *Store) Due(ctx context.Context, now time.Time) ([]Delivery, error) {
	var rows []Delivery
	err := s.DB.WithContext(ctx).Where("state IN ? AND next_attempt <= ?", []string{"pending_enqueue", "queued", "retry", "sending"}, now).Order("next_attempt ASC").Limit(10).Find(&rows).Error
	return rows, err
}
func (s *Store) Queued(ctx context.Context, row Delivery, now time.Time) error {
	return s.DB.WithContext(ctx).Model(&Delivery{}).Where("id = ? AND state IN ?", row.ID, []string{"pending_enqueue", "queued", "retry"}).Updates(map[string]any{"state": "queued", "dispatch_generation": gorm.Expr("dispatch_generation + 1"), "next_attempt": now.Add(2 * time.Minute), "updated_at": now}).Error
}
func (s *Store) Statuses(ctx context.Context, ids []string) (map[string]string, error) {
	var rows []Delivery
	result := map[string]string{}
	if err := s.DB.WithContext(ctx).Select("id", "state").Where("id IN ?", ids).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.ID] = row.State
	}
	return result, nil
}

func (s *Store) Prune(ctx context.Context, now time.Time) error {
	ids := s.DB.WithContext(ctx).Model(&Delivery{}).Select("id").Where("command <> '' AND (state = ? OR (kind = ? AND state IN ?) OR (deadline <= ? AND state IN ?))", "provider_accepted", "otp", []string{"failed", "unknown", "expired", "superseded"}, now, []string{"failed", "unknown", "expired", "superseded"}).Limit(100)
	return s.DB.WithContext(ctx).Model(&Delivery{}).Where("id IN (?)", ids).Update("command", "").Error
}
