package jobs

import (
	"bytes"
	"encoding/base64"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"nordik-drive-api/internal/notificationcontract"
	"nordik-drive-api/internal/outbox"
)

func TestReviewOutboxBacklogCannotStarveNewIntentsOrSetFlagsOnQueueing(t *testing.T) {
	for _, kind := range []string{"file", "form"} {
		t.Run(kind, func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			if err != nil {
				t.Fatal(err)
			}
			pool, _ := db.DB()
			pool.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = pool.Close() })
			for _, statement := range []string{
				`CREATE TABLE users (id integer PRIMARY KEY, firstname text, lastname text, email text)`,
				`INSERT INTO users VALUES (1,'Test','User','user@example.test')`,
				`CREATE TABLE file_edit_request (request_id integer PRIMARY KEY, user_id integer, firstname text, lastname text, status text, reviewer_comment text, created_at datetime, review_notification_key text DEFAULT '', review_email_trigger_success boolean DEFAULT false)`,
				`CREATE TABLE file_edit_request_details (id integer PRIMARY KEY,request_id integer)`,
				`CREATE TABLE form_submissions (id integer PRIMARY KEY, created_by integer, form_label text, firstname text, lastname text, status text, reviewer_comment text, created_at datetime, updated_at datetime, review_notification_key text DEFAULT '', review_email_trigger_success boolean DEFAULT false)`,
			} {
				if err := db.Exec(statement).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := db.AutoMigrate(&outbox.Record{}); err != nil {
				t.Fatal(err)
			}
			codec, err := notificationcontract.NewCodec(`{"1":"`+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32))+`"}`, "1")
			if err != nil {
				t.Fatal(err)
			}
			writer := &outbox.Writer{Codec: codec}
			table, idColumn := "file_edit_request", "request_id"
			if kind == "form" {
				table, idColumn = "form_submissions", "id"
			}
			for i := 1; i <= 2; i++ {
				key := ""
				if i == 1 {
					key = uuid.NewString()
				}
				values := map[string]any{idColumn: i, "firstname": "Test", "lastname": "User", "status": "approved", "reviewer_comment": "Reviewed", "created_at": time.Now().Add(time.Duration(i) * time.Minute), "review_notification_key": key, "review_email_trigger_success": false}
				if kind == "file" {
					values["user_id"] = 1
				} else {
					values["created_by"] = 1
					values["form_label"] = "Test form"
				}
				if err := db.Table(table).Create(values).Error; err != nil {
					t.Fatal(err)
				}
			}
			run := func() error {
				if kind == "file" {
					job := NewFileEditReviewEmailJob(db, nil, nil)
					job.Outbox = writer
					job.BatchSize = 1
					return job.Run()
				}
				job := NewFormSubmissionReviewEmailJob(db, nil, nil)
				job.Outbox = writer
				job.BatchSize = 1
				return job.Run()
			}
			for range 2 {
				if err := run(); err != nil {
					t.Fatal(err)
				}
			}
			var count int64
			db.Model(&outbox.Record{}).Count(&count)
			if count != 1 {
				t.Fatal("old unresolved row starved the new review, or recovery duplicated an intent", count)
			}
			var accepted bool
			db.Table(table).Select("review_email_trigger_success").Where(idColumn+" = ?", 2).Scan(&accepted)
			if accepted {
				t.Fatal("queueing marked review successful")
			}
			if err := db.Model(&outbox.Record{}).Where("state = ?", "pending").Update("state", "provider_accepted").Error; err != nil {
				t.Fatal(err)
			}
			if err := run(); err != nil {
				t.Fatal(err)
			}
			db.Table(table).Select("review_email_trigger_success").Where(idColumn+" = ?", 2).Scan(&accepted)
			if !accepted {
				t.Fatal("provider acceptance did not reconcile review flag")
			}
			// A pending-status update emitted by an actual review is reconciled,
			// but historical pending rows never acquire a new email intent.
			key := uuid.NewString()
			pending := map[string]any{idColumn: 3, "firstname": "Test", "lastname": "User", "status": "pending", "reviewer_comment": "Still reviewing", "created_at": time.Now(), "review_notification_key": key, "review_email_trigger_success": false}
			if kind == "file" {
				pending["user_id"] = 1
			} else {
				pending["created_by"] = 1
				pending["form_label"] = "Test form"
			}
			if err := db.Table(table).Create(pending).Error; err != nil {
				t.Fatal(err)
			}
			if err := writer.Send(db, "records", kind+"-review:"+key, []string{"user@example.test"}, "Review update", "Body"); err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&outbox.Record{}).Where("state = ?", "pending").Update("state", "provider_accepted").Error; err != nil {
				t.Fatal(err)
			}
			if err := run(); err != nil {
				t.Fatal(err)
			}
			accepted = false
			db.Table(table).Select("review_email_trigger_success").Where(idColumn+" = ?", 3).Scan(&accepted)
			if !accepted {
				t.Fatal("existing pending-status review email was not reconciled")
			}
		})
	}
}
