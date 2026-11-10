package outbox

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"nordik-drive-api/internal/notificationcontract"
	"testing"
	"time"
)

type remoteFixture struct {
	AcceptError, RecoverError error
	Accepted                  []string
	States                    map[string]string
	Batches                   []int
	BeforeAccept              func()
}

func (f *remoteFixture) Accept(_ context.Context, c notificationcontract.Command) (string, error) {
	if f.BeforeAccept != nil {
		f.BeforeAccept()
	}
	f.Accepted = append(f.Accepted, c.ID)
	return c.ID, f.AcceptError
}
func (f *remoteFixture) Status(_ context.Context, ids []string) (map[string]string, error) {
	f.Batches = append(f.Batches, len(ids))
	result := map[string]string{}
	for _, id := range ids {
		result[id] = f.States[id]
	}
	return result, nil
}
func (f *remoteFixture) Recover(context.Context) error { return f.RecoverError }
func outboxFixture(t *testing.T) (*gorm.DB, *Writer, *remoteFixture) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := db.DB()
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.AutoMigrate(&Record{}); err != nil {
		t.Fatal(err)
	}
	codec, err := notificationcontract.NewCodec(`{"1":"`+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32))+`"}`, "1")
	if err != nil {
		t.Fatal(err)
	}
	return db, &Writer{Codec: codec}, &remoteFixture{States: map[string]string{}}
}
func TestBusinessWriteAndIntentCommitOrRollbackTogether(t *testing.T) {
	db, w, _ := outboxFixture(t)
	if err := db.Exec("CREATE TABLE business (id integer PRIMARY KEY)").Error; err != nil {
		t.Fatal(err)
	}
	write := func(tx *gorm.DB) error {
		if err := tx.Exec("INSERT INTO business(id) VALUES (1)").Error; err != nil {
			return err
		}
		return w.Send(tx, "records", "resource:version", []string{"person@example.test"}, "Review", "Body")
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := write(tx); err != nil {
			return err
		}
		return errors.New("business rejected")
	}); err == nil {
		t.Fatal("expected rejected transaction")
	}
	var count int64
	_ = db.Model(&Record{}).Count(&count).Error
	if count != 0 {
		t.Fatal("orphaned intent")
	}
	_ = db.Table("business").Count(&count).Error
	if count != 0 {
		t.Fatal("business write did not roll back")
	}
	if err := db.Transaction(write); err != nil {
		t.Fatal(err)
	}
	if err := w.Send(db, "records", "resource:version", []string{"person@example.test"}, "Review", "Body"); err != nil {
		t.Fatal("idempotent producer retry failed", err)
	}
	if err := w.Send(db, "records", "resource:version", []string{"person@example.test"}, "Changed", "Body"); err == nil {
		t.Fatal("event key reused with different content")
	}
	_ = db.Model(&Record{}).Count(&count).Error
	if count != 1 {
		t.Fatal("duplicate producer intent")
	}
}
func TestDispatcherRecoversEnqueueFailureAndErasesCompletedOTPPayload(t *testing.T) {
	db, w, remote := outboxFixture(t)
	now := time.Now().UTC()
	id, err := w.Put(db, notificationcontract.Command{Producer: "identity", Key: "signup-otp:challenge", Kind: "otp", CreatedAt: now, Deadline: now.Add(10 * time.Minute)}, notificationcontract.Payload{To: "person@example.test", Subject: "Code", HTML: "<strong>012345</strong>"})
	if err != nil {
		t.Fatal(err)
	}
	d := &Dispatcher{DB: db, Remote: remote}
	remote.AcceptError = errors.New("intake unavailable")
	remote.BeforeAccept = func() {
		var value int
		if err := db.Raw("SELECT 1").Scan(&value).Error; err != nil {
			t.Fatal("producer connection held during remote intake")
		}
	}
	if err := d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	var record Record
	_ = db.First(&record, "id = ?", id).Error
	if record.State != "pending" || record.LeaseUntil != nil {
		t.Fatal("failed intake lost its pending intent")
	}
	remote.AcceptError = nil
	_ = db.Model(&record).Update("next_attempt", now.Add(-time.Second)).Error
	if err := d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = db.First(&record, "id = ?", id).Error
	if record.State != "accepted" {
		t.Fatal("intake acknowledgement missing")
	}
	remote.States[record.DeliveryID] = "provider_accepted"
	_ = db.Model(&record).Update("next_attempt", now.Add(-time.Second)).Error
	if err := d.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = db.First(&record, "id = ?", id).Error
	var command notificationcontract.Command
	_ = json.Unmarshal([]byte(record.Command), &command)
	if record.State != "provider_accepted" || !record.PayloadErased || command.Ciphertext != "" {
		t.Fatal("completed code payload retained")
	}
	if _, err := w.Put(db, notificationcontract.Command{Producer: "identity", Key: "signup-otp:challenge", Kind: "otp", CreatedAt: now, Deadline: now.Add(10 * time.Minute)}, notificationcontract.Payload{To: "person@example.test", Subject: "Code", HTML: "<strong>012345</strong>"}); err != nil {
		t.Fatal("payload cleanup broke persistent deduplication", err)
	}
}
func TestExpiredIntentsAndActiveLeasesAreNotDispatched(t *testing.T) {
	db, w, remote := outboxFixture(t)
	now := time.Now().UTC()
	id, err := w.Put(db, notificationcontract.Command{Producer: "identity", Key: "expired", Kind: "otp", CreatedAt: now.Add(-20 * time.Minute), Deadline: now.Add(-10 * time.Minute)}, notificationcontract.Payload{To: "person@example.test", Subject: "Code", HTML: "Expired"})
	if err != nil {
		t.Fatal(err)
	}
	d := &Dispatcher{DB: db, Remote: remote}
	if err := d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	var record Record
	_ = db.First(&record, "id = ?", id).Error
	if record.State != "expired" || len(remote.Accepted) != 0 || !record.PayloadErased {
		t.Fatal("expired OTP dispatched or retained")
	}
	if err := w.Send(db, "records", "leased", []string{"person@example.test"}, "Review", "Body"); err != nil {
		t.Fatal(err)
	}
	_ = db.Model(&Record{}).Where("state = ?", "pending").Update("lease_until", now.Add(time.Minute)).Error
	if err := d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(remote.Accepted) != 0 {
		t.Fatal("active producer lease stolen")
	}
}
func TestGroupStatusIncludesEveryRecipientAndEscapesEventPatterns(t *testing.T) {
	db, _, remote := outboxFixture(t)
	now := time.Now().UTC()
	var records []Record
	for i := range 101 {
		id := uuid.NewString()
		remote.States[id] = "provider_accepted"
		if i == 100 {
			remote.States[id] = "failed"
		}
		records = append(records, Record{ID: id, Producer: "support", EventKey: fmt.Sprintf("group_email:recipient-%d", i), State: "accepted", DeliveryID: id, Kind: "normal", Deadline: now.Add(time.Hour), NextAttempt: now, CreatedAt: now})
	}
	if err := db.Create(&records).Error; err != nil {
		t.Fatal(err)
	}
	state, err := GroupState(context.Background(), db, remote, "support", "group_email")
	if err != nil || state != "failed" {
		t.Fatal("failed recipient outside first page ignored", err)
	}
	if len(remote.Batches) != 2 || remote.Batches[0] != 100 || remote.Batches[1] != 1 {
		t.Fatal("status API batch size incorrect")
	}
	if err := db.Create(&Record{ID: uuid.NewString(), Producer: "support", EventKey: "other_group:one", State: "provider_accepted"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&Record{ID: uuid.NewString(), Producer: "support", EventKey: "otherXgroup:two", State: "failed"}).Error; err != nil {
		t.Fatal(err)
	}
	accepted, err := ProviderAcceptedFor(context.Background(), db, remote, "support", "other_group")
	if err != nil || !accepted {
		t.Fatal("event separator treated as SQL wildcard")
	}
}
