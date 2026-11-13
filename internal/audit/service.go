// Package audit durably ingests immutable activity before acknowledging its owner.
package audit

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"nordik-drive-api/internal/auditoutbox"
	"nordik-drive-api/internal/audittransport"
)

type Ingestion struct {
	EventID     string `gorm:"primaryKey"`
	Fingerprint string
	Producer    string
	Version     int
	RequestID   string
	TraceID     string
	LogID       uint
	CreatedAt   time.Time
}

func (Ingestion) TableName() string { return "audit_ingestions" }

type Source interface {
	Read(context.Context, string) (auditoutbox.SourceRecord, error)
	Ack(context.Context, auditoutbox.Receipt) error
}
type OwnerSource struct {
	Producer string
	Source   Source
}
type Service struct {
	DB      *gorm.DB
	Sources []OwnerSource
}

func (s *Service) Ingest(ctx context.Context, id string) error {
	if _, err := uuid.Parse(id); err != nil {
		return errors.New("invalid audit event identifier")
	}
	var source Source
	var record auditoutbox.SourceRecord
	var unavailable bool
	var producer string
	for _, candidate := range s.Sources {
		if candidate.Source == nil || (candidate.Producer != "identity" && candidate.Producer != "legacy") {
			return errors.New("invalid audit owner configuration")
		}
		result, err := candidate.Source.Read(ctx, id)
		if err == nil {
			source = candidate.Source
			producer = candidate.Producer
			record = result
			break
		}
		if !errors.Is(err, audittransport.ErrNotFound) {
			unavailable = true
		}
	}
	if source == nil {
		if unavailable {
			return errors.New("audit owner unavailable")
		}
		return errors.New("audit event not found at its owners")
	}
	receipt := record.Receipt
	if receipt.ID != id || len(receipt.Fingerprint) != 64 {
		return errors.New("invalid owner audit receipt")
	}
	if record.Event == nil {
		var prior Ingestion
		if err := s.DB.WithContext(ctx).First(&prior, "event_id = ?", id).Error; err != nil {
			return errors.New("owner acknowledged an uncommitted audit event")
		}
		if prior.Producer != producer || prior.Fingerprint != receipt.Fingerprint || prior.LogID != receipt.LogID || prior.LogID == 0 {
			return errors.New("audit receipt conflict")
		}
		return nil
	}
	event := *record.Event
	if event.Validate() != nil || event.Producer != producer || event.ID != id || event.Fingerprint() != receipt.Fingerprint {
		return errors.New("invalid owner audit event")
	}
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		ingestion := Ingestion{EventID: id, Fingerprint: receipt.Fingerprint, Producer: event.Producer, Version: event.Version, RequestID: event.RequestID, TraceID: event.TraceID, CreatedAt: time.Now().UTC()}
		insert := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "event_id"}}, DoNothing: true}).Create(&ingestion)
		if insert.Error != nil {
			return errors.New("audit ingestion reservation unavailable")
		}
		if insert.RowsAffected == 0 {
			if err := tx.First(&ingestion, "event_id = ?", id).Error; err != nil {
				return errors.New("audit ingestion unavailable")
			}
			if ingestion.Fingerprint != receipt.Fingerprint || ingestion.LogID == 0 {
				return errors.New("immutable audit event conflict")
			}
			receipt.LogID = ingestion.LogID
			return nil
		}
		log := event.Log
		if err := tx.Create(&log).Error; err != nil {
			return errors.New("audit activity write failed")
		}
		receipt.LogID = log.ID
		return tx.Model(&ingestion).Update("log_id", log.ID).Error
	})
	if err != nil {
		return err
	}
	// The database transaction has committed before the network acknowledgment.
	// A lost acknowledgment retries the same immutable ID without another log.
	if err := source.Ack(ctx, receipt); err != nil {
		return errors.New("audit committed; owner acknowledgment pending")
	}
	return nil
}
