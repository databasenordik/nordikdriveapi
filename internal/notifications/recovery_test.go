package notifications

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type queueFunc func(context.Context, Delivery) error

func (f queueFunc) Enqueue(ctx context.Context, row Delivery) error { return f(ctx, row) }

func TestRecoveryReservesBothKindsAndPrunesDespiteQueueFailure(t *testing.T) {
	s, store, initial, clock := serviceFixture(t)
	initial.Fail = true
	*clock = clock.Add(-5 * time.Minute)
	for i := range 25 {
		if _, err := s.Accept(context.Background(), sealedCommand(t, s, fmt.Sprintf("normal-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	*clock = clock.Add(5 * time.Minute)
	for i := range 10 {
		command := sealedCommand(t, s, fmt.Sprintf("otp-%d", i))
		payload, _ := s.Codec.Open(command)
		command.Kind = "otp"
		command.Deadline = s.now().Add(time.Duration(i+1) * time.Minute)
		command, _ = s.Codec.Seal(command, payload)
		if _, err := s.Accept(context.Background(), command); err != nil {
			t.Fatal(err)
		}
	}
	// A terminal OTP still needs pruning even when the normal queue is broken.
	old := Delivery{ID: "terminal", Kind: "otp", State: "unknown", Producer: "identity", EventKey: "terminal", Command: "protected-payload", Deadline: s.now().Add(time.Minute), CreatedAt: s.now(), NextAttempt: s.now()}
	if err := store.DB.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	var once sync.Once
	s.Queue = queueFunc(func(ctx context.Context, row Delivery) error {
		if row.Kind == "otp" {
			once.Do(func() { close(ready) })
			return nil
		}
		select {
		case <-ready:
			return errors.New("ordinary queue unavailable")
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if err := s.Recover(context.Background()); err == nil {
		t.Fatal("queue failure was not reported")
	}
	var queued int64
	if err := store.DB.Model(&Delivery{}).Where("kind = ? AND state = ?", "otp", "queued").Count(&queued).Error; err != nil || queued != 5 {
		t.Fatal("ordinary backlog starved the reserved OTP batch", queued, err)
	}
	if err := store.DB.First(&old, "id = ?", old.ID).Error; err != nil || old.Command != "" {
		t.Fatal("queue failure suppressed sensitive-payload pruning", err)
	}
	// Reverse the failure: a broken OTP queue must not stop ordinary recovery.
	s.Queue = queueFunc(func(_ context.Context, row Delivery) error {
		if row.Kind == "otp" {
			return errors.New("OTP queue unavailable")
		}
		return nil
	})
	if err := s.Recover(context.Background()); err == nil {
		t.Fatal("OTP queue failure was not reported")
	}
	if err := store.DB.Model(&Delivery{}).Where("kind = ? AND state = ?", "normal", "queued").Count(&queued).Error; err != nil || queued != 5 {
		t.Fatal("OTP failure suppressed ordinary recovery", queued, err)
	}
}

func TestDueDoesNotSpendBatchOnActiveSendingLease(t *testing.T) {
	s, store, queue, _ := serviceFixture(t)
	id, err := s.Accept(context.Background(), sealedCommand(t, s, "active-send"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DB.Model(&Delivery{}).Where("id = ?", id).Updates(map[string]any{"state": "sending", "next_attempt": s.now().Add(-time.Minute), "lease_until": s.now().Add(time.Minute)}).Error; err != nil {
		t.Fatal(err)
	}
	before := queue.Calls
	if err := s.Recover(context.Background()); err != nil || queue.Calls != before {
		t.Fatal("active delivery was recovered before its lease expired", err)
	}
}
