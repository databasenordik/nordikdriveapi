package mailer

type EmailSender interface {
	SendOne(to, subject, body string) error
}
type AttachmentSender interface {
	SendWithAttachments([]string, string, string, []Attachment) error
}

// DeliveryError preserves a redacted stage result without exposing SMTP replies.
// SafeToRetry is false once DATA might have been accepted by the provider.
type DeliveryError struct {
	Message     string
	SafeToRetry bool
	Permanent   bool
}

func (e *DeliveryError) Error() string { return e.Message }

type Attachment struct {
	Filename    string
	ContentType string
	Data        []byte
}
