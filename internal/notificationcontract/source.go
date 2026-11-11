package notificationcontract

import (
	"context"

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
