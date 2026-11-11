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
	enabled, err := EventDispatchEnabled()
	if err != nil {
		return nil, nil, err
	}
	if enabled && mode != "outbox" {
		return nil, nil, errors.New("event notification dispatch requires outbox delivery")
	}
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
	writer := &Writer{Codec: codec}
	if enabled {
		writer.Publisher, err = NewEventPublisher(ctx, os.Getenv("NOTIFICATIONS_OTP_QUEUE"), os.Getenv("NOTIFICATIONS_NORMAL_QUEUE"), os.Getenv("NOTIFICATIONS_URL"), os.Getenv("NOTIFICATIONS_TASK_ACCOUNT"))
		if err != nil {
			return nil, nil, err
		}
	}
	return writer, client, nil
}
