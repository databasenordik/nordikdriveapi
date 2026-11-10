package supportrequest

import (
	"bytes"
	"context"
	"encoding/base64"
	"testing"

	"nordik-drive-api/internal/notificationcontract"
	"nordik-drive-api/internal/outbox"
)

func TestSupportRequestOutboxCommitsWithBusinessAndOnlyAcceptedMailSetsFlags(t *testing.T) {
	s, mail := newTestService(t)
	codec, err := notificationcontract.NewCodec(`{"1":"`+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32))+`"}`, "1")
	if err != nil {
		t.Fatal(err)
	}
	s.Outbox = &outbox.Writer{Codec: codec}
	if err := s.DB.AutoMigrate(&outbox.Record{}); err != nil {
		t.Fatal(err)
	}
	created, err := s.Create(&CreateSupportRequestRequest{RequestType: RequestTypeQuestion, Subject: "Help", Message: "Details"}, 7, nil)
	if err != nil {
		t.Fatal(err)
	}
	var row SupportRequest
	if err := s.DB.First(&row, created.ID).Error; err != nil {
		t.Fatal(err)
	}
	var count int64
	s.DB.Model(&outbox.Record{}).Count(&count)
	if count != 2 || len(mail.sendCalls) != 0 || len(mail.sendWithAttachCalls) != 0 || row.RequesterEmailSent || row.NotificationEmailSent {
		t.Fatal("queued mail used SMTP or set delivery flags")
	}
	if err := ReconcileOutgoing(context.Background(), s.DB, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Model(&outbox.Record{}).Where("state = ?", "pending").Update("state", "provider_accepted").Error; err != nil {
		t.Fatal(err)
	}
	if err := ReconcileOutgoing(context.Background(), s.DB, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.First(&row, created.ID).Error; err != nil || !row.RequesterEmailSent || !row.NotificationEmailSent {
		t.Fatal("accepted recipients did not reconcile flags", err)
	}
	if err := s.DB.Migrator().DropTable(&outbox.Record{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(&CreateSupportRequestRequest{RequestType: RequestTypeQuestion, Subject: "Rollback", Message: "Must not commit"}, 7, nil); err == nil {
		t.Fatal("business write acknowledged without durable email intent")
	}
	s.DB.Model(&SupportRequest{}).Count(&count)
	if count != 1 {
		t.Fatal("failed intent left a partial business record")
	}
}
