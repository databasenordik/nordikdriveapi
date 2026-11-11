package supportschedule

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"nordik-drive-api/internal/notificationcontract"
	"nordik-drive-api/internal/outbox"
)

func TestSupportOutboxWaitsForPersistedZoomLinkAndRecoversOnce(t *testing.T) {
	f := newScheduleFixture(t)
	codec, err := notificationcontract.NewCodec(`{"1":"`+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))+`"}`, "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.DB.AutoMigrate(&outbox.Record{}); err != nil {
		t.Fatal(err)
	}
	f.service.Outbox = &outbox.Writer{Codec: codec}
	mail := &scheduleMailer{}
	f.service.Mailer = mail
	provider := &fakeMeetingProvider{createErr: context.DeadlineExceeded}
	f.service.MeetingProvider = provider
	created, err := f.service.CreateCall(f.survivor, CreateCallInput{ScheduledStart: f.at(1, 11, 0).Format(time.RFC3339), DurationMinutes: 30, Subject: "Help", Message: "Details"})
	if err != nil {
		t.Fatal(err)
	}
	var count int64
	f.service.DB.Model(&outbox.Record{}).Count(&count)
	if count != 0 || len(mail.sent) != 0 {
		t.Fatal("meeting email escaped before the participant URL was persisted")
	}
	var audit SupportScheduleAuditLog
	if err := f.service.DB.First(&audit, "notification_state = ?", "pending").Error; err != nil || audit.NotificationSnapshot == "" {
		t.Fatal("waiting email intent was not durable", err)
	}
	provider.createErr = nil
	if err := f.service.SyncPendingZoomMeetings(); err != nil {
		t.Fatal(err)
	}
	ready, err := f.service.request(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	var intents []outbox.Record
	if err := f.service.DB.Find(&intents).Error; err != nil {
		t.Fatal(err)
	}
	if len(intents) != 2 || len(mail.sent) != 0 {
		t.Fatal("expected exactly two finalized intents and no direct SMTP sends", len(intents))
	}
	for _, intent := range intents {
		var command notificationcontract.Command
		if err := json.Unmarshal([]byte(intent.Command), &command); err != nil {
			t.Fatal(err)
		}
		payload, err := codec.Open(command)
		if err != nil || !strings.Contains(payload.HTML, ready.Call.ZoomJoinURL) {
			t.Fatal("finalized intent missing meeting link", err)
		}
		if !command.Deadline.Equal(audit.CreatedAt.Add(24 * time.Hour)) {
			t.Fatal("retry extended business event deadline")
		}
	}
	if err := f.service.RecoverNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.service.DB.Model(&outbox.Record{}).Count(&count)
	if count != 2 {
		t.Fatal("recovery duplicated recipients")
	}
	if err := f.service.DB.First(&audit, audit.ID).Error; err != nil || audit.NotificationState != "processed" || audit.NotificationSnapshot != "" {
		t.Fatal("processed recipient snapshot was retained")
	}
}

func TestExpiredSupportSnapshotIsErasedWithoutSending(t *testing.T) {
	f := newScheduleFixture(t)
	codec, _ := notificationcontract.NewCodec(`{"1":"`+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))+`"}`, "1")
	f.service.Outbox = &outbox.Writer{Codec: codec}
	row := SupportScheduleAuditLog{EntityType: "support_request", Action: "synthetic", Details: []byte(`{}`), NotificationState: "pending", NotificationSnapshot: "sensitive old snapshot", CreatedAt: time.Now().Add(-25 * time.Hour)}
	if err := f.service.DB.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.service.RecoverNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.service.DB.First(&row, row.ID).Error; err != nil || row.NotificationState != "expired" || row.NotificationSnapshot != "" {
		t.Fatal("expired snapshot retained or processed")
	}
}

func TestNewSupportNotificationDoesNotWaitBehindRepairBacklog(t *testing.T) {
	f := newScheduleFixture(t)
	codec, _ := notificationcontract.NewCodec(`{"1":"`+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))+`"}`, "1")
	if err := f.service.DB.AutoMigrate(&outbox.Record{}); err != nil {
		t.Fatal(err)
	}
	f.service.Outbox = &outbox.Writer{Codec: codec}
	f.service.MeetingProvider = &fakeMeetingProvider{}
	for range 12 {
		row := SupportScheduleAuditLog{EntityType: "support_request", Action: "old", Details: []byte(`{}`), NotificationState: "pending", NotificationSnapshot: "malformed old intent", CreatedAt: time.Now().Add(-time.Hour)}
		if err := f.service.DB.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.service.CreateCall(f.survivor, CreateCallInput{ScheduledStart: f.at(1, 11, 0).Format(time.RFC3339), DurationMinutes: 30, Subject: "Help", Message: "Details"}); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := f.service.DB.Model(&outbox.Record{}).Count(&count).Error; err != nil || count != 2 {
		t.Fatal("fresh finalized notification was blocked by unrelated backlog", count, err)
	}
}
