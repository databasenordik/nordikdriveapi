package outbox

import (
	"context"
	"errors"
	"os"

	"nordik-drive-api/internal/notificationcontract"
	"nordik-drive-api/internal/workload"
)

func Configure(ctx context.Context) (*Writer, *notificationcontract.Client, error) {
	mode := os.Getenv("NOTIFICATION_DELIVERY_MODE")
	if mode == "" || mode == "direct" {
		return nil, nil, nil
	}
	if mode != "outbox" {
		return nil, nil, errors.New("NOTIFICATION_DELIVERY_MODE must be direct or outbox")
	}
	codec, err := notificationcontract.NewCodec(os.Getenv("NOTIFICATION_PAYLOAD_KEYS"), os.Getenv("NOTIFICATION_PAYLOAD_KEY_VERSION"))
	if err != nil {
		return nil, nil, errors.New("notification payload keys are required")
	}
	tokens, err := workload.Source(ctx, os.Getenv("NOTIFICATIONS_URL"))
	if err != nil {
		return nil, nil, err
	}
	client, err := notificationcontract.NewClient(os.Getenv("NOTIFICATIONS_URL"), tokens)
	if err != nil {
		return nil, nil, err
	}
	return &Writer{Codec: codec}, client, nil
}
