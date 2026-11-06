// Package circuit implements a bounded, per-upstream availability breaker.
package circuit

import (
	"sync"
	"time"
)

type Ticket struct {
	generation uint64
	probe      bool
}
type Breaker struct {
	mu         sync.Mutex
	failures   []time.Time
	openUntil  time.Time
	probing    bool
	generation uint64
	Now        func() time.Time
}

func (b *Breaker) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}
func (b *Breaker) Allow() (Ticket, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if !b.openUntil.IsZero() {
		if now.Before(b.openUntil) {
			return Ticket{}, b.openUntil.Sub(now)
		}
		if b.probing {
			return Ticket{}, time.Second
		}
		b.probing = true
		return Ticket{b.generation, true}, 0
	}
	return Ticket{b.generation, false}, 0
}

// Record treats excluded outcomes as successful availability observations.
// Outcomes from calls admitted before a state transition cannot close a probe.
func (b *Breaker) Record(t Ticket, availabilityFailure bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if t.generation != b.generation {
		return
	}
	now := b.now()
	if t.probe {
		b.probing = false
		b.generation++
		if availabilityFailure {
			b.openUntil = now.Add(5 * time.Minute)
		} else {
			b.openUntil = time.Time{}
			b.failures = nil
		}
		return
	}
	if !b.openUntil.IsZero() {
		return
	}
	if !availabilityFailure {
		return
	}
	kept := b.failures[:0]
	for _, when := range b.failures {
		if now.Sub(when) < time.Minute {
			kept = append(kept, when)
		}
	}
	b.failures = append(kept, now)
	if len(b.failures) >= 6 {
		b.openUntil = now.Add(5 * time.Minute)
		b.generation++
		b.failures = nil
	}
}

// Exclude releases a cancelled probe without claiming upstream recovery.
func (b *Breaker) Exclude(ticket Ticket) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ticket.generation == b.generation && ticket.probe {
		b.probing = false
		b.generation++
	}
}
