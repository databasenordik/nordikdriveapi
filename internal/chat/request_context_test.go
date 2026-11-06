package chat

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/sync/singleflight"
)

func TestCoalescedWaiterCanCancelWithoutAbandoningLeader(t *testing.T) {
	var group singleflight.Group
	started, release := make(chan struct{}), make(chan struct{})
	leader := make(chan error, 1)
	go func() {
		_, err := coalesced(context.Background(), &group, "shared", func() (any, error) { close(started); <-release; return "value", nil })
		leader <- err
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	waiter := make(chan error, 1)
	go func() {
		_, err := coalesced(ctx, &group, "shared", func() (any, error) { t.Error("duplicated shared operation"); return nil, nil })
		waiter <- err
	}()
	cancel()
	select {
	case err := <-waiter:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("caller cancellation lost")
		}
	case <-time.After(time.Second):
		t.Fatal("waiter remained blocked on another request")
	}
	close(release)
	if err := <-leader; err != nil {
		t.Fatal(err)
	}
}
func TestScopedChatSharesCachesWithoutCopyingMutexes(t *testing.T) {
	original := &ChatService{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scoped := original.WithContext(ctx).(*ChatService)
	if scoped == original || scoped.sharedState() != original.sharedState() {
		t.Fatal("request scope lost shared cache state")
	}
}
