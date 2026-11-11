package notifications

import (
	"context"
	"errors"
	"testing"
	"time"

	"nordik-drive-api/internal/notificationcontract"
)

type sourceFixture struct {
	record     notificationcontract.SourceRecord
	readErr    error
	receiptErr error
	receipts   []notificationcontract.SourceReceipt
}

func (f *sourceFixture) Read(context.Context, string) (notificationcontract.SourceRecord, error) {
	return f.record, f.readErr
}
func (f *sourceFixture) Receipt(_ context.Context, r notificationcontract.SourceReceipt) error {
	f.receipts = append(f.receipts, r)
	return f.receiptErr
}

func TestEventDeliveryPullsCommittedIntentWithoutCreatingAnotherTask(t *testing.T) {
	s, store, queue, clock := serviceFixture(t)
	command := sealedCommand(t, s, "event-driven")
	source := &sourceFixture{record: notificationcontract.SourceRecord{State: "pending", Command: command}, receiptErr: errors.New("receipt unavailable")}
	s.Source = source
	calls := 0
	s.Provider = providerFunc(func(context.Context, notificationcontract.Payload) string { calls++; return "provider_accepted" })
	if err := s.Deliver(context.Background(), command.ID); err == nil {
		t.Fatal("missing producer acknowledgment was not retried")
	}
	source.receiptErr = nil
	*clock = clock.Add(time.Minute)
	if err := s.Deliver(context.Background(), command.ID); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || queue.Calls != 0 || len(source.receipts) != 2 {
		t.Fatal("receipt recovery resent SMTP or created a duplicate task", calls, queue.Calls)
	}
	var row Delivery
	if err := store.DB.First(&row, "id = ?", command.ID).Error; err != nil || row.State != "provider_accepted" || row.Command != "" {
		t.Fatal("durable outcome/payload cleanup missing", err)
	}
	if source.receipts[1].State != "provider_accepted" || source.receipts[1].OutboxID != command.ID {
		t.Fatal("producer received an incorrect receipt")
	}
}

func TestEventDeliveryNeverResendsTerminalOrUncertainSource(t *testing.T) {
	for _, state := range []string{"unknown", "failed", "expired", "superseded", "provider_accepted"} {
		t.Run(state, func(t *testing.T) {
			s, _, queue, _ := serviceFixture(t)
			command := sealedCommand(t, s, "terminal-source")
			source := &sourceFixture{record: notificationcontract.SourceRecord{State: state}}
			s.Source = source
			calls := 0
			s.Provider = providerFunc(func(context.Context, notificationcontract.Payload) string { calls++; return "provider_accepted" })
			if err := s.Deliver(context.Background(), command.ID); err != nil {
				t.Fatal(err)
			}
			if calls != 0 || queue.Calls != 0 || len(source.receipts) != 1 {
				t.Fatal("terminal source was sent again")
			}
		})
	}
}

func TestEventDeliveryRejectsMissingOrMismatchedSource(t *testing.T) {
	for _, failure := range []string{"read", "mismatch", "accepted", "invalid"} {
		t.Run(failure, func(t *testing.T) {
			s, _, queue, _ := serviceFixture(t)
			command := sealedCommand(t, s, "bad-source")
			source := &sourceFixture{record: notificationcontract.SourceRecord{State: "pending", Command: command}}
			switch failure {
			case "read":
				source.readErr = errors.New("unavailable")
			case "mismatch":
				source.record.Command.ID = "different"
			case "accepted":
				source.record.State = "accepted"
			case "invalid":
				source.record.Command.Ciphertext = "invalid"
			}
			s.Source = source
			if err := s.Deliver(context.Background(), command.ID); err == nil || queue.Calls != 0 {
				t.Fatal("untrusted/incomplete source was accepted")
			}
		})
	}
}

func TestAutomaticRetriesRespectBackoffAndProviderAttemptCap(t *testing.T) {
	s, store, _, clock := serviceFixture(t)
	command := sealedCommand(t, s, "automatic-transient-retry")
	s.Source = &sourceFixture{record: notificationcontract.SourceRecord{State: "pending", Command: command}}
	calls := 0
	s.Provider = providerFunc(func(context.Context, notificationcontract.Payload) string { calls++; return "retry" })
	for attempt, delay := range []time.Duration{10, 20, 40, 60, 60} {
		_ = s.Deliver(context.Background(), command.ID)
		var row Delivery
		if err := store.DB.First(&row, "id = ?", command.ID).Error; err != nil || row.Attempts != attempt+1 {
			t.Fatal("retry attempt not durable", err)
		}
		if attempt < 4 && !row.NextAttempt.Equal(clock.Add(delay*time.Second)) {
			t.Fatal("application delay does not match task backoff")
		}
		*clock = clock.Add(delay * time.Second)
	}
	if err := s.Deliver(context.Background(), command.ID); err != nil || calls != 5 {
		t.Fatal("provider retry budget was reset", calls, err)
	}
	var row Delivery
	_ = store.DB.First(&row, "id = ?", command.ID).Error
	if row.State != "failed" {
		t.Fatal("retry exhaustion was not visible")
	}
}
