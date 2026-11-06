package mailer

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"
)

type Service struct {
	From     string
	Password string
	SMTPHost string
	SMTPPort string
	ctx      context.Context
	slots    chan struct{}

	// keep this overridable for tests
	sendMail func(addr string, a smtp.Auth, from string, to []string, msg []byte) error
}

func NewService(from, password, smtpHost, smtpPort string) *Service {
	return &Service{
		From:     from,
		Password: password,
		SMTPHost: smtpHost,
		SMTPPort: smtpPort,
		slots:    make(chan struct{}, 4),
	}
}

func (s *Service) Send(to []string, subject, body string) error {
	if len(to) == 0 {
		return fmt.Errorf("recipient list is empty")
	}
	if strings.TrimSpace(subject) == "" {
		return fmt.Errorf("subject is required")
	}
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("body is required")
	}

	auth := smtp.PlainAuth("", s.From, s.Password, s.SMTPHost)

	message := []byte(fmt.Sprintf(
		"From: %s\r\n"+
			"To: %s\r\n"+
			"Subject: %s\r\n"+
			"MIME-Version: 1.0\r\n"+
			"Content-Type: text/html; charset=\"UTF-8\"\r\n"+
			"\r\n"+
			"%s",
		s.From,
		strings.Join(to, ", "),
		subject,
		body,
	))

	return s.deliver(
		s.SMTPHost+":"+s.SMTPPort,
		auth,
		s.From,
		to,
		message,
	)
}

func (s *Service) SendOne(to, subject, body string) error {
	return s.Send([]string{to}, subject, body)
}

func (s *Service) SendWithAttachments(to []string, subject, body string, attachments []Attachment) error {
	if len(attachments) == 0 {
		return s.Send(to, subject, body)
	}
	if len(to) == 0 {
		return fmt.Errorf("recipient list is empty")
	}
	if strings.TrimSpace(subject) == "" {
		return fmt.Errorf("subject is required")
	}
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("body is required")
	}

	var message bytes.Buffer
	writer := multipart.NewWriter(&message)

	fmt.Fprintf(
		&message,
		"From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=%q\r\n\r\n",
		s.From,
		strings.Join(to, ", "),
		subject,
		writer.Boundary(),
	)

	htmlHeader := textproto.MIMEHeader{}
	htmlHeader.Set("Content-Type", `text/html; charset="UTF-8"`)
	htmlHeader.Set("Content-Transfer-Encoding", "quoted-printable")

	htmlPart, err := writer.CreatePart(htmlHeader)
	if err != nil {
		return err
	}

	qpWriter := quotedprintable.NewWriter(htmlPart)
	if _, err := qpWriter.Write([]byte(body)); err != nil {
		return err
	}
	if err := qpWriter.Close(); err != nil {
		return err
	}

	for _, attachment := range attachments {
		filename := sanitizeAttachmentFilename(attachment.Filename)
		if filename == "" {
			return fmt.Errorf("attachment filename is required")
		}

		contentType := strings.TrimSpace(attachment.ContentType)
		if contentType == "" {
			contentType = "application/octet-stream"
		}

		partHeader := textproto.MIMEHeader{}
		partHeader.Set("Content-Type", fmt.Sprintf(`%s; name="%s"`, contentType, filename))
		partHeader.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
		partHeader.Set("Content-Transfer-Encoding", "base64")

		part, err := writer.CreatePart(partHeader)
		if err != nil {
			return err
		}

		encoder := base64.NewEncoder(base64.StdEncoding, &base64LineWriter{Writer: part})
		if _, err := encoder.Write(attachment.Data); err != nil {
			return err
		}
		if err := encoder.Close(); err != nil {
			return err
		}
		if _, err := part.Write([]byte("\r\n")); err != nil {
			return err
		}
	}

	if err := writer.Close(); err != nil {
		return err
	}

	auth := smtp.PlainAuth("", s.From, s.Password, s.SMTPHost)

	return s.deliver(
		s.SMTPHost+":"+s.SMTPPort,
		auth,
		s.From,
		to,
		message.Bytes(),
	)
}

func (s *Service) WithContext(ctx context.Context) any { copy := *s; copy.ctx = ctx; return &copy }

// deliver keeps SMTP work within the request/job lifetime. Cancellation closes
// the socket, including while net/smtp is waiting for a server response.
func (s *Service) deliver(addr string, auth smtp.Auth, from string, to []string, message []byte) error {
	if s.sendMail != nil {
		return s.sendMail(addr, auth, from, to, message)
	}
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if s.slots != nil {
		select {
		case s.slots <- struct{}{}:
			defer func() { <-s.slots }()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return errors.New("SMTP connection failed")
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	client, err := smtp.NewClient(conn, s.SMTPHost)
	if err != nil {
		return errors.New("SMTP handshake failed")
	}
	defer client.Close()
	if ok, _ := client.Extension("STARTTLS"); !ok {
		return errors.New("SMTP server must support STARTTLS")
	}
	if err := client.StartTLS(&tls.Config{ServerName: s.SMTPHost, MinVersion: tls.VersionTLS12}); err != nil {
		return errors.New("SMTP TLS failed")
	}
	if err := client.Auth(auth); err != nil {
		return errors.New("SMTP authentication failed")
	}
	if err := client.Mail(from); err != nil {
		return errors.New("SMTP sender rejected")
	}
	for _, recipient := range to {
		if err := client.Rcpt(recipient); err != nil {
			return errors.New("SMTP recipient rejected")
		}
	}
	writer, err := client.Data()
	if err != nil {
		return errors.New("SMTP data rejected")
	}
	if _, err := writer.Write(message); err != nil {
		return errors.New("SMTP write failed")
	}
	if err := writer.Close(); err != nil {
		return errors.New("SMTP delivery outcome uncertain")
	}
	_ = client.Quit()
	return nil
}

type base64LineWriter struct {
	io.Writer
	lineLength int
}

func (w *base64LineWriter) Write(p []byte) (int, error) {
	written := 0

	for len(p) > 0 {
		remaining := 76 - w.lineLength
		if remaining == 0 {
			if _, err := w.Writer.Write([]byte("\r\n")); err != nil {
				return written, err
			}
			w.lineLength = 0
			remaining = 76
		}

		if remaining > len(p) {
			remaining = len(p)
		}

		n, err := w.Writer.Write(p[:remaining])
		written += n
		w.lineLength += n
		if err != nil {
			return written, err
		}

		p = p[remaining:]
	}

	return written, nil
}

func sanitizeAttachmentFilename(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, `"`, "")
	return name
}
