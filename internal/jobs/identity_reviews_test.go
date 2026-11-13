package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"nordik-drive-api/internal/file"
	"nordik-drive-api/internal/formsubmission"
	"nordik-drive-api/internal/identityclient"
)

type reviewDirectory struct {
	pool  *sql.DB
	calls int
	fail  bool
}

func (d *reviewDirectory) Profiles(_ context.Context, ids []int64, email bool) (map[int64]identityclient.Profile, error) {
	d.calls++
	if d.pool.Stats().InUse != 0 {
		return nil, errors.New("profile RPC inside business transaction")
	}
	if d.fail {
		return nil, errors.New("Identity unavailable")
	}
	result := map[int64]identityclient.Profile{}
	for _, id := range ids {
		if id == 1 || id == 2 {
			p := identityclient.Profile{ID: id, Firstname: fmt.Sprint(id), Lastname: "User"}
			if email {
				p.Email = fmt.Sprintf("user%d@example.test", id)
			}
			result[id] = p
		}
	}
	return result, nil
}
func (d *reviewDirectory) Principals(context.Context, []int64) (map[int64]identityclient.Principal, error) {
	return nil, errors.New("unexpected principals")
}
func (d *reviewDirectory) Search(context.Context, string, string, string) ([]int64, error) {
	return nil, errors.New("unexpected search")
}
func TestReviewJobsSkipMissingProfilesBeforeApplyingBatchLimit(t *testing.T) {
	for _, kind := range []string{"file", "form"} {
		t.Run(kind, func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			if err != nil {
				t.Fatal(err)
			}
			pool, _ := db.DB()
			defer pool.Close()
			pool.SetMaxOpenConns(1)
			if err := db.AutoMigrate(&file.FileEditRequest{}, &file.FileEditRequestDetails{}, &formsubmission.FormSubmission{}); err != nil {
				t.Fatal(err)
			}
			if db.Migrator().HasTable("users") {
				if err := db.Migrator().RenameTable("users", "identity_owned_users"); err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now().UTC().Add(-time.Hour)
			actors := []int{900, 901, 902, 1, 2}
			for index, actor := range actors {
				if kind == "file" {
					if err := db.Create(&file.FileEditRequest{RequestID: uint(index + 1), UserID: uint(actor), FileID: 1, Status: "completed", CreatedAt: now.Add(time.Duration(index) * time.Second)}).Error; err != nil {
						t.Fatal(err)
					}
				} else {
					if err := db.Create(&formsubmission.FormSubmission{ID: int64(index + 1), FileID: 1, RowID: int64(index + 1), FormKey: "owner-review", FormLabel: "Owner review", CreatedByID: &actor, Status: "approved", CreatedAt: now.Add(time.Duration(index) * time.Second)}).Error; err != nil {
						t.Fatal(err)
					}
				}
			}
			directory := &reviewDirectory{pool: pool}
			sender := &fileJobMockEmailSender{}
			if kind == "file" {
				job := NewFileEditReviewEmailJob(db, sender, nil)
				job.Directory = directory
				job.BatchSize = 2
				err = job.Run()
			} else {
				job := NewFormSubmissionReviewEmailJob(db, sender, nil)
				job.Directory = directory
				job.BatchSize = 2
				err = job.Run()
			}
			if err != nil {
				t.Fatal(err)
			}
			if directory.calls != 3 || len(sender.calls) != 2 || sender.calls[0].To != "user1@example.test" || sender.calls[1].To != "user2@example.test" {
				t.Fatal("missing recipients consumed the valid batch", directory.calls, len(sender.calls))
			}
			table, id := "file_edit_request", "request_id"
			if kind == "form" {
				table, id = "form_submissions", "id"
			}
			var count int64
			if err := db.Table(table).Where(id+" IN (4,5) AND review_email_trigger_success = ?", true).Count(&count).Error; err != nil || count != 2 {
				t.Fatal("valid recipients not marked processed", err)
			}
			if err := db.Table(table).Where(id+" IN (1,2,3) AND review_email_trigger_success = ?", true).Count(&count).Error; err != nil || count != 0 {
				t.Fatal("missing recipients falsely accepted", err)
			}
			db.Table(table).Where(id+"=4").Update("review_email_trigger_success", false)
			directory.fail = true
			before := len(sender.calls)
			if kind == "file" {
				job := NewFileEditReviewEmailJob(db, sender, nil)
				job.Directory = directory
				err = job.Run()
			} else {
				job := NewFormSubmissionReviewEmailJob(db, sender, nil)
				job.Directory = directory
				err = job.Run()
			}
			if err == nil || len(sender.calls) != before {
				t.Fatal("owner outage did not stop delivery before business writes")
			}
		})
	}
}
