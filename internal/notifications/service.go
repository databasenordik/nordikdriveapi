package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
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
type Service struct {
	Store    Repository
	Codec    *notificationcontract.Codec
	Queue    Queue
	Provider Provider
	Clock    func() time.Time
}

func (s *Service) now() time.Time {
	if s.Clock != nil {
		return s.Clock().UTC()
	}
	return time.Now().UTC()
}
func (s *Service) Accept(ctx context.Context, command notificationcontract.Command) (string, error) {
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
	if row.State == "pending_enqueue" {
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
	if err != nil || !send {
		return err
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
	if state == "retry" {
		return errors.New("retryable notification failure")
	}
	return nil
}
func (s *Service) Recover(ctx context.Context) error {
	rows, err := s.Store.Due(ctx, s.now())
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		if row.State == "sending" || !row.Deadline.After(s.now()) || row.Attempts >= row.attemptLimit() {
			// Claim only to classify expired leases/deadlines; never make a provider call.
			_, _, err := s.Store.Claim(ctx, row.ID, s.now())
			if err != nil && !errors.Is(err, ErrBusy) {
				return err
			}
			continue
		}
		if err := s.enqueue(ctx, row); err != nil {
			return err
		}
	}
	return s.Store.Prune(ctx, s.now())
}
