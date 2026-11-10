package notifications

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"

	"cloud.google.com/go/storage"
	"nordik-drive-api/internal/mailer"
	"nordik-drive-api/internal/notificationcontract"
	"nordik-drive-api/internal/requestscope"
)

type SMTPProvider struct {
	Mail    mailer.AttachmentSender
	Storage *storage.Client
}

func (p *SMTPProvider) Deliver(ctx context.Context, payload notificationcontract.Payload) string {
	if p.Mail == nil {
		return "retry"
	}
	if payload.Validate() != nil {
		return "failed"
	}
	var attachments []mailer.Attachment
	for _, ref := range payload.Attachments {
		if p.Storage == nil {
			return "retry"
		}
		reader, err := p.Storage.Bucket(ref.Bucket).Object(ref.Object).Generation(ref.Generation).NewReader(ctx)
		if errors.Is(err, storage.ErrObjectNotExist) {
			return "failed"
		}
		if err != nil {
			return "retry"
		}
		data, readErr := io.ReadAll(io.LimitReader(reader, ref.Size+1))
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil {
			return "retry"
		}
		hash := sha256.Sum256(data)
		if int64(len(data)) != ref.Size || reader.Attrs.ContentType != ref.ContentType || hex.EncodeToString(hash[:]) != ref.SHA256 {
			return "failed"
		}
		attachments = append(attachments, mailer.Attachment{Filename: ref.Filename, ContentType: ref.ContentType, Data: data})
	}
	mail := requestscope.Service(ctx, p.Mail)
	err := mail.SendWithAttachments([]string{payload.To}, payload.Subject, payload.HTML, attachments)
	if err == nil {
		return "provider_accepted"
	}
	var outcome *mailer.DeliveryError
	if errors.As(err, &outcome) {
		if outcome.Permanent {
			return "failed"
		}
		if outcome.SafeToRetry {
			return "retry"
		}
	}
	return "unknown"
}
