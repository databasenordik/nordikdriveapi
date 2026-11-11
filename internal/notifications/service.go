package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"nordik-drive-api/internal/notificationcontract"
)

type Queue interface {
	Enqueue(context.Context, Delivery) error
}

// Provider reports retry only when the message is known not to have been accepted.
type Provider interface {
	Deliver(context.Context, notificationcontract.Payload) string
}

type Source interface {
	Read(context.Context, string) (notificationcontract.SourceRecord, error)
	Receipt(context.Context, notificationcontract.SourceReceipt) error
}
type Service struct {
	Store    Repository
	Codec    *notificationcontract.Codec
	Queue    Queue
	Provider Provider
	Source   Source
	Clock    func() time.Time
}

func (s *Service) now() time.Time {
	if s.Clock != nil {
		return s.Clock().UTC()
	}
	return time.Now().UTC()
}
func (s *Service) Accept(ctx context.Context, command notificationcontract.Command) (string, error) {
	return s.accept(ctx, command, true)
}

func (s *Service) accept(ctx context.Context, command notificationcontract.Command, enqueue bool) (string, error) {
	ctx, span := otel.Tracer("nordik/notifications").Start(ctx, "notifications.accept")
	defer span.End()
	now := s.now()
	if command.Validate(now) != nil {
		return "", notificationcontract.ErrInvalid
	}
	if _, err := s.Codec.Open(command); err != nil {
		return "", err
	}
	b, _ := json.Marshal(command)
	row, err := s.Store.Accept(ctx, Delivery{ID: command.ID, Producer: command.Producer, EventKey: command.Key, Command: string(b), Fingerprint: command.Fingerprint, State: "pending_enqueue", Kind: command.Kind, Deadline: command.Deadline, Supersession: command.Supersession, Sequence: command.Sequence, NextAttempt: now, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		return "", err
	}
	// Intake is durable already. Recovery repairs a failed enqueue, including crashes.
	if enqueue && row.State == "pending_enqueue" {
		_ = s.enqueue(ctx, row)
	}
	slog.InfoContext(ctx, "notification_intake", "delivery_id", row.ID, "producer", row.Producer, "kind", row.Kind, "state", row.State, "intent_age_ms", now.Sub(command.CreatedAt).Milliseconds())
	return row.ID, nil
}
func (s *Service) enqueue(ctx context.Context, row Delivery) error {
	if err := s.Queue.Enqueue(ctx, row); err != nil {
		slog.WarnContext(ctx, "notification_enqueue_failed", "delivery_id", row.ID, "kind", row.Kind)
		return err
	}
	return s.Store.Queued(ctx, row, s.now())
}
func (s *Service) Deliver(ctx context.Context, id string) error {
	ctx, span := otel.Tracer("nordik/notifications").Start(ctx, "notifications.deliver")
	defer span.End()
	row, send, err := s.Store.Claim(ctx, id, s.now())
	sourceID := id
	if errors.Is(err, ErrNotFound) && s.Source != nil {
		record, readErr := s.Source.Read(ctx, id)
		if readErr != nil {
			return errors.New("notification intent source unavailable")
		}
		if terminal(record.State) {
			return s.Source.Receipt(ctx, notificationcontract.SourceReceipt{OutboxID: sourceID, DeliveryID: id, State: record.State})
		}
		if record.State != "pending" || record.Command.ID != id {
			return errors.New("notification source requires reconciliation")
		}
		// This durable task is already executing. Do not create a second task
		// merely to record its producer's committed command.
		id, err = s.accept(ctx, record.Command, false)
		if err != nil {
			return err
		}
		row, send, err = s.Store.Claim(ctx, id, s.now())
	}
	if err != nil {
		return err
	}
	if !send {
		return s.receipt(ctx, sourceID, id)
	}
	var command notificationcontract.Command
	started := s.now()
	state := "failed"
	if json.Unmarshal([]byte(row.Command), &command) == nil {
		if payload, err := s.Codec.Open(command); err == nil {
			providerCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
			providerCtx, expire := context.WithDeadline(providerCtx, row.Deadline)
			state = s.Provider.Deliver(providerCtx, payload)
			cancel()
			expire()
		}
	}
	switch state {
	case "provider_accepted", "retry", "failed", "unknown":
	default:
		state = "unknown"
	}
	if state == "retry" && row.Attempts >= row.attemptLimit() {
		state = "failed"
	}
	// Persist the provider result even if the task disconnected. Otherwise recovery
	// would have to classify a known acceptance as unknown.
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.Store.Finish(finishCtx, row, state, s.now()); err != nil {
		slog.ErrorContext(finishCtx, "notification_outcome_write_failed", "delivery_id", id, "attempt", row.Attempts)
		return err
	}
	span.SetAttributes(attribute.String("notification.outcome", state), attribute.String("notification.kind", row.Kind))
	slog.InfoContext(finishCtx, "notification_delivery", "delivery_id", id, "producer", row.Producer, "kind", row.Kind, "outcome", state, "attempt", row.Attempts, "duration_ms", s.now().Sub(started).Milliseconds(), "intent_age_ms", s.now().Sub(command.CreatedAt).Milliseconds())
	if err := s.receipt(finishCtx, sourceID, id); err != nil {
		return err
	}
	if state == "retry" {
		return errors.New("retryable notification failure")
	}
	return nil
}

func (s *Service) receipt(ctx context.Context, sourceID, deliveryID string) error {
	if s.Source == nil {
		return nil
	}
	states, err := s.Store.Statuses(ctx, []string{deliveryID})
	if err != nil {
		return err
	}
	if err := s.Source.Receipt(ctx, notificationcontract.SourceReceipt{OutboxID: sourceID, DeliveryID: deliveryID, State: states[deliveryID]}); err != nil {
		return errors.New("notification producer receipt unavailable")
	}
	return nil
}
func (s *Service) Recover(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	lookup, stop := context.WithTimeout(ctx, time.Second)
	rows, lookupErr := s.Store.Due(lookup, s.now())
	stop()
	var joined sync.WaitGroup
	var failures [2]error
	if lookupErr == nil {
		for group, kind := range []string{"otp", "normal"} {
			joined.Add(1)
			go func() {
				defer joined.Done()
				phase, stop := context.WithTimeout(ctx, 3*time.Second)
				defer stop()
				for _, row := range rows {
					if row.Kind != kind {
						continue
					}
					if phase.Err() != nil {
						failures[group] = errors.Join(failures[group], phase.Err())
						break
					}
					var err error
					if row.State == "sending" || !row.Deadline.After(s.now()) || row.Attempts >= row.attemptLimit() {
						// Classification never calls the provider or resets its attempt cap.
						_, _, err = s.Store.Claim(phase, row.ID, s.now())
						if errors.Is(err, ErrBusy) {
							err = nil
						}
					} else {
						err = s.enqueue(phase, row)
					}
					failures[group] = errors.Join(failures[group], err)
				}
			}()
		}
	}
	joined.Wait()
	prune, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	return errors.Join(lookupErr, failures[0], failures[1], s.Store.Prune(prune, s.now()))
}
