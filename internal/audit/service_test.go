package audit

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"nordik-drive-api/internal/activity"
	"nordik-drive-api/internal/auditoutbox"
	"nordik-drive-api/internal/audittransport"
)

type ownerStub struct {
	record          auditoutbox.SourceRecord
	readErr, ackErr error
	reads, acks     int
}

func (o *ownerStub) Read(context.Context, string) (auditoutbox.SourceRecord, error) {
	o.reads++
	return o.record, o.readErr
}
func (o *ownerStub) Ack(_ context.Context, r auditoutbox.Receipt) error {
	o.acks++
	if o.ackErr == nil {
		o.record.Event = nil
		o.record.Receipt = r
	}
	return o.ackErr
}
func fixture(t *testing.T) (*gorm.DB, *Service, *ownerStub) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", uuid.NewString())), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := db.DB()
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.AutoMigrate(&Ingestion{}, &activity.Log{}); err != nil {
		t.Fatal(err)
	}
	uid := uint(52)
	event := activity.Event{ID: uuid.NewString(), Version: 1, Producer: "identity", Log: activity.Log{Level: "INFO", Service: "auth", UserID: &uid, Action: "LOGIN", Message: "Existing activity", CreatedAt: time.Now().UTC()}}
	owner := &ownerStub{record: auditoutbox.SourceRecord{Event: &event, Receipt: auditoutbox.Receipt{ID: event.ID, Fingerprint: event.Fingerprint()}}}
	return db, &Service{DB: db, Sources: []OwnerSource{{Producer: "identity", Source: owner}}}, owner
}
func countLogs(t *testing.T, db *gorm.DB, want int64) {
	t.Helper()
	var count int64
	if err := db.Model(&activity.Log{}).Count(&count).Error; err != nil || count != want {
		t.Fatalf("logs=%d want=%d err=%v", count, want, err)
	}
}
func TestIngestAcknowledgesOnlyAfterCommitAndNeverDuplicates(t *testing.T) {
	db, s, owner := fixture(t)
	id := owner.record.Receipt.ID
	// A lost response must retain the immutable producer payload for retry.
	owner.ackErr = errors.New("lost ack")
	if s.Ingest(context.Background(), id) == nil {
		t.Fatal("lost acknowledgment hidden")
	}
	countLogs(t, db, 1)
	if owner.record.Event == nil {
		t.Fatal("unacknowledged payload erased")
	}
	owner.ackErr = nil
	if err := s.Ingest(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	countLogs(t, db, 1)
	if owner.record.Event != nil || owner.record.Receipt.LogID == 0 {
		t.Fatal("durable ingestion not acknowledged")
	}
	if err := s.Ingest(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	countLogs(t, db, 1)
	if owner.acks != 2 {
		t.Fatal("already ingested event acknowledged again")
	}
}
func TestIngestionRollsBackReservationAndActivity(t *testing.T) {
	db, s, owner := fixture(t)
	if err := db.Exec("DROP TABLE logs").Error; err != nil {
		t.Fatal(err)
	}
	if s.Ingest(context.Background(), owner.record.Receipt.ID) == nil {
		t.Fatal("failed activity insert accepted")
	}
	var count int64
	db.Model(&Ingestion{}).Count(&count)
	if count != 0 || owner.acks != 0 {
		t.Fatal("partial ingestion committed or acknowledged")
	}
	if err := db.AutoMigrate(&activity.Log{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Ingest(context.Background(), owner.record.Receipt.ID); err != nil {
		t.Fatal(err)
	}
	countLogs(t, db, 1)
}
func TestOwnerOutageAndMissingRowsAreNotSuccess(t *testing.T) {
	db, s, owner := fixture(t)
	missing := &ownerStub{readErr: audittransport.ErrNotFound}
	s.Sources = []OwnerSource{{Producer: "legacy", Source: missing}, {Producer: "identity", Source: owner}}
	if err := s.Ingest(context.Background(), owner.record.Receipt.ID); err != nil {
		t.Fatal(err)
	}
	countLogs(t, db, 1)
	for _, err := range []error{audittransport.ErrNotFound, errors.New("owner down")} {
		owner.readErr = err
		if s.Ingest(context.Background(), uuid.NewString()) == nil {
			t.Fatal("missing/unavailable source accepted")
		}
	}
	if s.Ingest(context.Background(), "invalid") == nil {
		t.Fatal("invalid event identifier accepted")
	}
}
func TestImmutableEventAndReceiptConflicts(t *testing.T) {
	for _, kind := range []string{"id", "fingerprint", "version", "producer", "uncommitted", "changed-after-ingest", "receipt-log"} {
		t.Run(kind, func(t *testing.T) {
			db, s, owner := fixture(t)
			id := owner.record.Receipt.ID
			switch kind {
			case "id":
				owner.record.Receipt.ID = uuid.NewString()
			case "fingerprint":
				owner.record.Receipt.Fingerprint = "wrong"
			case "version":
				owner.record.Event.Version = 2
			case "producer":
				owner.record.Event.Producer = "legacy"
				owner.record.Receipt.Fingerprint = owner.record.Event.Fingerprint()
			case "uncommitted":
				owner.record.Event = nil
				owner.record.Receipt.LogID = 999
			case "changed-after-ingest":
				owner.ackErr = errors.New("lost")
				_ = s.Ingest(context.Background(), id)
				owner.record.Event.Log.Message = "Changed"
				owner.record.Receipt.Fingerprint = owner.record.Event.Fingerprint()
			case "receipt-log":
				if err := s.Ingest(context.Background(), id); err != nil {
					t.Fatal(err)
				}
				owner.record.Receipt.LogID++
			}
			if s.Ingest(context.Background(), id) == nil {
				t.Fatal("conflicting event accepted")
			}
			want := int64(0)
			if kind == "changed-after-ingest" || kind == "receipt-log" {
				want = 1
			}
			countLogs(t, db, want)
		})
	}
}
func TestDatabaseFailuresNeverAcknowledge(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprint(after), func(t *testing.T) {
			db, s, owner := fixture(t)
			id := owner.record.Receipt.ID
			if after {
				if err := s.Ingest(context.Background(), id); err != nil {
					t.Fatal(err)
				}
			}
			acks := owner.acks
			pool, _ := db.DB()
			_ = pool.Close()
			if s.Ingest(context.Background(), id) == nil || owner.acks != acks {
				t.Fatal("database outage acknowledged")
			}
		})
	}
}
