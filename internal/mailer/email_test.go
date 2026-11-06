package mailer

import (
	"context"
	"errors"
	"net"
	"net/smtp"
	"strings"
	"testing"
	"time"
)

func TestSMTPStopsOnCancellationAndRequiresTLS(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	connected := make(chan struct{})
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		close(connected)
		buffer := make([]byte, 1)
		_, _ = connection.Read(buffer)
	}()
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	service := NewService("from@example.com", "credential-canary", host, port)
	ctx, cancel := context.WithCancel(context.Background())
	scoped := service.WithContext(ctx).(*Service)
	done := make(chan error, 1)
	go func() { done <- scoped.SendOne("to@example.com", "subject", "body") }()
	<-connected
	cancel()
	select {
	case err := <-done:
		if err == nil || strings.Contains(err.Error(), "credential-canary") {
			t.Fatal("cancellation failure or credential leak")
		}
	case <-time.After(time.Second):
		t.Fatal("SMTP ignored request cancellation")
	}
}
func TestSMTPValidationAndMessageContracts(t *testing.T) {
	service := NewService("from@example.com", "fixture", "smtp.example.com", "587")
	var message string
	service.sendMail = func(addr string, auth smtp.Auth, from string, to []string, data []byte) error {
		message = string(data)
		if addr != "smtp.example.com:587" || from != "from@example.com" || len(to) != 1 {
			t.Fatal("SMTP envelope changed")
		}
		return nil
	}
	for _, tc := range []struct {
		to            []string
		subject, body string
	}{{nil, "subject", "body"}, {[]string{"to@example.com"}, "", "body"}, {[]string{"to@example.com"}, "subject", ""}} {
		if service.Send(tc.to, tc.subject, tc.body) == nil {
			t.Fatal("invalid message accepted")
		}
	}
	if err := service.SendOne("to@example.com", "subject", "<b>body</b>"); err != nil || !strings.Contains(message, "Content-Type: text/html") || !strings.Contains(message, "<b>body</b>") {
		t.Fatal("HTML email changed")
	}
	if err := service.SendWithAttachments([]string{"to@example.com"}, "subject", "body", []Attachment{{Filename: "fixture.txt", Data: []byte("attachment")}}); err != nil {
		t.Fatal(err)
	}
	service.sendMail = func(string, smtp.Auth, string, []string, []byte) error { return errors.New("provider failure") }
	if service.SendOne("to@example.com", "subject", "body") == nil {
		t.Fatal("provider failure hidden")
	}
}
