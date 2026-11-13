package notificationcontract

import (
	"context"
	"errors"

	"golang.org/x/oauth2"
)

// SourceRecord exposes only the producer's sealed delivery command and state.
type SourceRecord struct {
	State   string  `json:"state"`
	Command Command `json:"command"`
}

type SourceReceipt struct {
	OutboxID   string `json:"outbox_id"`
	DeliveryID string `json:"delivery_id"`
	State      string `json:"state"`
}

// SourceClient uses the same original signed-token convention as intake.
type SourceClient struct{ client *Client }

func NewSourceClient(address string, tokens oauth2.TokenSource) (*SourceClient, error) {
	client, err := NewClient(address, tokens)
	if err != nil {
		return nil, err
	}
	return &SourceClient{client: client}, nil
}

func (s *SourceClient) Read(ctx context.Context, id string) (SourceRecord, error) {
	var record SourceRecord
	err := s.client.call(ctx, "/internal/v1/notification-outbox/read", map[string]string{"delivery_id": id}, &record)
	return record, err
}

func (s *SourceClient) Receipt(ctx context.Context, receipt SourceReceipt) error {
	return s.client.call(ctx, "/internal/v1/notification-outbox/receipt", receipt, nil)
}

type OwnerSource interface {
	Read(context.Context, string) (SourceRecord, error)
	Receipt(context.Context, SourceReceipt) error
}

// OwnerSources supports the compatibility window and transactional row move.
// Only a successful authenticated owner response permits processing. A missing
// row is distinct from an unavailable owner, and neither becomes a false ack.
type OwnerSources []OwnerSource

func (sources OwnerSources) Read(ctx context.Context, id string) (SourceRecord, error) {
	var unavailable bool
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return SourceRecord{}, err
		}
		if source == nil {
			unavailable = true
			continue
		}
		record, err := source.Read(ctx, id)
		if err == nil {
			return record, nil
		}
		if !errors.Is(err, ErrNotFound) {
			unavailable = true
		}
	}
	if unavailable {
		return SourceRecord{}, errors.New("notification owner source unavailable")
	}
	return SourceRecord{}, ErrNotFound
}

func (sources OwnerSources) Receipt(ctx context.Context, receipt SourceReceipt) error {
	var unavailable bool
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return err
		}
		if source == nil {
			unavailable = true
			continue
		}
		err := source.Receipt(ctx, receipt)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			unavailable = true
		}
	}
	if unavailable {
		return errors.New("notification owner receipt unavailable")
	}
	return ErrNotFound
}
