package notificationcontract

import (
	"context"
	"errors"
	"testing"
)

type ownerStub struct {
	readErr, receiptErr error
	reads, receipts     int
}

func (s *ownerStub) Read(context.Context, string) (SourceRecord, error) {
	s.reads++
	return SourceRecord{State: "pending"}, s.readErr
}
func (s *ownerStub) Receipt(context.Context, SourceReceipt) error { s.receipts++; return s.receiptErr }

func TestOwnerHandoffNeverTurnsMissingOrUnavailableIntoAcknowledgment(t *testing.T) {
	for _, firstError := range []error{ErrNotFound, errors.New("owner temporarily unavailable")} {
		first := &ownerStub{readErr: firstError, receiptErr: firstError}
		second := &ownerStub{}
		sources := OwnerSources{first, second}
		if row, err := sources.Read(context.Background(), "event-id"); err != nil || row.State != "pending" {
			t.Fatal("other owner could not serve its committed event")
		}
		if err := sources.Receipt(context.Background(), SourceReceipt{OutboxID: "event-id"}); err != nil {
			t.Fatal("receipt did not follow transferred owner")
		}
		if second.reads != 1 || second.receipts != 1 {
			t.Fatal("owner handoff was skipped")
		}
	}
	first := &ownerStub{}
	second := &ownerStub{}
	if _, err := (OwnerSources{first, second}).Read(context.Background(), "id"); err != nil {
		t.Fatal(err)
	}
	if err := (OwnerSources{first, second}).Receipt(context.Background(), SourceReceipt{}); err != nil {
		t.Fatal(err)
	}
	if second.reads != 0 || second.receipts != 0 {
		t.Fatal("a valid owner response was ignored")
	}
	for _, sources := range []OwnerSources{nil, {&ownerStub{readErr: ErrNotFound, receiptErr: ErrNotFound}}} {
		if _, err := sources.Read(context.Background(), "id"); !errors.Is(err, ErrNotFound) {
			t.Fatal("all-missing owners acknowledged an event")
		}
		if err := sources.Receipt(context.Background(), SourceReceipt{}); !errors.Is(err, ErrNotFound) {
			t.Fatal("all-missing owners acknowledged a receipt")
		}
	}
	for _, sources := range []OwnerSources{{nil}, {&ownerStub{readErr: errors.New("down"), receiptErr: errors.New("down")}}} {
		if _, err := sources.Read(context.Background(), "id"); err == nil || errors.Is(err, ErrNotFound) {
			t.Fatal("owner outage misclassified as absent")
		}
		if err := sources.Receipt(context.Background(), SourceReceipt{}); err == nil || errors.Is(err, ErrNotFound) {
			t.Fatal("owner receipt outage misclassified as absent")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (OwnerSources{first}).Read(ctx, "id"); !errors.Is(err, context.Canceled) {
		t.Fatal("source ignored cancellation")
	}
	if err := (OwnerSources{first}).Receipt(ctx, SourceReceipt{}); !errors.Is(err, context.Canceled) {
		t.Fatal("receipt ignored cancellation")
	}
}
