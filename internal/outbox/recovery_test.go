package outbox

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCorruptIntentDoesNotBlockPeersOrReceiptReconciliation(t *testing.T) {
	db, writer, remote := outboxFixture(t)
	now := time.Now().UTC()
	if err := db.Create(&Record{ID: "corrupt", Producer: "records", EventKey: "bad", Kind: "normal", Command: "invalid", State: "pending", CreatedAt: now.Add(-time.Hour), NextAttempt: now, Deadline: now.Add(time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	if err := writer.Send(db, "records", "valid", []string{"test@example.test"}, "Review", "Body"); err != nil {
		t.Fatal(err)
	}
	remote.RecoverError = errors.New("recovery endpoint unavailable")
	remote.States["receipt"] = "provider_accepted"
	if err := db.Create(&Record{ID: "receipt", Producer: "identity", EventKey: "already-sent", Kind: "otp", Command: "{}", State: "accepted", DeliveryID: "receipt", NextAttempt: now.Add(-time.Minute), Deadline: now.Add(time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	if err := (&Dispatcher{DB: db, Remote: remote}).Run(context.Background()); err == nil {
		t.Fatal("partial recovery failures were not reported")
	}
	var corrupt, received Record
	if err := db.First(&corrupt, "id = ?", "corrupt").Error; err != nil || corrupt.State != "failed" || !corrupt.PayloadErased {
		t.Fatal("corrupt intent was not safely quarantined", err)
	}
	if len(remote.Accepted) != 1 {
		t.Fatal("a corrupt intent blocked a valid peer")
	}
	if err := db.First(&received, "id = ?", "receipt").Error; err != nil || received.State != "provider_accepted" || !received.PayloadErased {
		t.Fatal("recovery failure suppressed receipt reconciliation", err)
	}
}
