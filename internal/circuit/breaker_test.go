package circuit

import (
	"sync"
	"testing"
	"time"
)

func TestBreakerTransitionsAndStaleOutcomes(t *testing.T) {
	now := time.Now()
	b := &Breaker{Now: func() time.Time { return now }}
	stale, _ := b.Allow()
	for i := 0; i < 6; i++ {
		ticket, retry := b.Allow()
		if retry != 0 {
			t.Fatal("opened early")
		}
		b.Record(ticket, true)
	}
	if _, retry := b.Allow(); retry != 5*time.Minute {
		t.Fatal("did not open")
	}
	b.Record(stale, false)
	now = now.Add(5 * time.Minute)
	probe, retry := b.Allow()
	if retry != 0 || !probe.probe {
		t.Fatal("missing probe")
	}
	if _, retry := b.Allow(); retry == 0 {
		t.Fatal("concurrent probe admitted")
	}
	b.Record(probe, true)
	now = now.Add(5 * time.Minute)
	probe, _ = b.Allow()
	b.Record(probe, false)
	if _, retry := b.Allow(); retry != 0 {
		t.Fatal("did not close")
	}
}
func TestOldFailuresExpireAndExcludedOutcomesDoNotCount(t *testing.T) {
	now := time.Now()
	b := &Breaker{Now: func() time.Time { return now }}
	for i := 0; i < 5; i++ {
		ticket, _ := b.Allow()
		b.Record(ticket, true)
	}
	now = now.Add(time.Minute)
	for i := 0; i < 20; i++ {
		ticket, _ := b.Allow()
		b.Record(ticket, false)
	}
	ticket, _ := b.Allow()
	b.Record(ticket, true)
	if _, retry := b.Allow(); retry != 0 {
		t.Fatal("expired/excluded outcomes opened breaker")
	}
}
func TestConcurrentBreakerAdmission(t *testing.T) {
	b := &Breaker{}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticket, retry := b.Allow()
			if retry == 0 {
				b.Record(ticket, true)
			}
		}()
	}
	wg.Wait()
	if _, retry := b.Allow(); retry <= 0 {
		t.Fatal("breaker did not open under concurrent failures")
	}
}

func TestCancelledProbeDoesNotClaimRecovery(t *testing.T) {
	now := time.Now()
	b := &Breaker{Now: func() time.Time { return now }}
	for i := 0; i < 6; i++ {
		ticket, _ := b.Allow()
		b.Record(ticket, true)
	}
	now = now.Add(5 * time.Minute)
	probe, _ := b.Allow()
	b.Exclude(probe)
	next, retry := b.Allow()
	if retry != 0 || !next.probe {
		t.Fatal("cancelled probe changed availability state")
	}
	b.Record(probe, false)
	b.Record(next, true)
	if _, retry := b.Allow(); retry == 0 {
		t.Fatal("failed fresh probe did not reopen")
	}
}
