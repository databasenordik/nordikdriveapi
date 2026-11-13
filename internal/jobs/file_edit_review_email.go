package jobs

import (
	"fmt"
	"log"
	"strings"
	"time"

	"gorm.io/gorm"

	filehelper "nordik-drive-api/internal/file"
	"nordik-drive-api/internal/identityclient"
	"nordik-drive-api/internal/mailer"
	"nordik-drive-api/internal/outbox"
	"nordik-drive-api/internal/requestscope"
)

type FileEditReviewEmailJob struct {
	DB        *gorm.DB
	Mailer    mailer.EmailSender
	Logger    *log.Logger
	BatchSize int
	Outbox    *outbox.Writer
	Remote    outbox.Remote
	Directory identityclient.Directory
}

type fileEditReviewEmailRow struct {
	CreatedAt       *time.Time `gorm:"column:created_at"`
	RequestID       uint       `gorm:"column:request_id"`
	UserID          uint       `gorm:"column:user_id"`
	Status          string     `gorm:"column:status"`
	FirstName       string     `gorm:"column:firstname"`
	LastName        string     `gorm:"column:lastname"`
	ReviewComment   string     `gorm:"column:reviewer_comment"`
	Email           string     `gorm:"column:email"`
	CreatedUserName string     `gorm:"column:created_user_name"`
}

func NewFileEditReviewEmailJob(
	db *gorm.DB,
	mailerSvc mailer.EmailSender,
	logger *log.Logger,
) *FileEditReviewEmailJob {
	return &FileEditReviewEmailJob{
		DB:        db,
		Mailer:    mailerSvc,
		Logger:    logger,
		BatchSize: 100,
	}
}

func (j *FileEditReviewEmailJob) Run() error {
	var rows []fileEditReviewEmailRow

	query := j.DB.Table("file_edit_request fer")
	if j.Outbox != nil {
		// Do not let old queued/failed deliveries occupy every scan slot.
		// The dispatcher reconciles replay outcomes into the producer outbox.
		query = query.Where(`((COALESCE(fer.review_notification_key, '') = '' AND fer.status <> 'pending') OR EXISTS (
		 SELECT 1 FROM notification_outbox n WHERE n.producer = 'records'
		 AND n.event_key LIKE 'file-review:' || fer.review_notification_key || ':%'
		 AND n.state = 'provider_accepted')) AND fer.review_email_trigger_success = ?`, false)
	} else {
		query = query.Where("fer.status <> ? AND fer.review_email_trigger_success = ?", "pending", false)
	}
	columns := `
			fer.request_id,
			fer.user_id,
			fer.status,
			fer.firstname,
			fer.lastname,
			fer.reviewer_comment,
			u.email,
			TRIM(COALESCE(u.firstname, '') || ' ' || COALESCE(u.lastname, '')) AS created_user_name
		`
	if j.Directory != nil {
		columns = "fer.request_id,fer.user_id,fer.status,fer.firstname,fer.lastname,fer.reviewer_comment,fer.created_at"
	} else {
		query = query.Joins("JOIN users u ON u.id = fer.user_id")
	}

	directory := j.Directory
	var err error
	if directory == nil {
		err = query.Select(columns).Order("fer.created_at ASC").Limit(j.BatchSize).Scan(&rows).Error
		if err != nil {
			return fmt.Errorf("failed to fetch file edit requests pending review emails: %w", err)
		}
	} else {
		var profiles map[int64]identityclient.Profile
		rows, directory, profiles, err = ownedReviewRows(query, columns, "fer.created_at", "fer.request_id", j.BatchSize, directory, func(row fileEditReviewEmailRow) int64 { return int64(row.UserID) }, func(row fileEditReviewEmailRow) (*time.Time, int64) { return row.CreatedAt, int64(row.RequestID) })
		if err != nil {
			return err
		}
		for i := range rows {
			p := profiles[int64(rows[i].UserID)]
			rows[i].Email = p.Email
			rows[i].CreatedUserName = strings.Trim(p.Firstname+" "+p.Lastname, " ")
		}
	}

	for _, row := range rows {
		if j.Outbox != nil {
			owner := &filehelper.FileService{DB: j.DB, Outbox: j.Outbox, Directory: directory}
			key, err := owner.RecoverReviewIntent(row.RequestID)
			if err != nil {
				return err
			}
			if key == "" {
				continue
			}
			accepted, err := outbox.ProviderAccepted(requestscope.Context(j.DB), j.DB, j.Remote, "file-review:"+key)
			if err != nil {
				return err
			}
			if accepted {
				if err := j.DB.Table("file_edit_request").Where("request_id = ? AND review_notification_key = ?", row.RequestID, key).Update("review_email_trigger_success", true).Error; err != nil {
					return err
				}
			}
			continue
		}
		if err := requestscope.Context(j.DB).Err(); err != nil {
			return err
		}
		email := strings.TrimSpace(row.Email)
		if email == "" {
			if j.Logger != nil {
				j.Logger.Printf("file edit review email job: empty email for request_id=%d user_id=%d", row.RequestID, row.UserID)
			}
			continue
		}

		details, err := j.fileEditRequestDetails(row.RequestID)
		if err != nil {
			if j.Logger != nil {
				j.Logger.Printf("file edit review email job: failed to fetch details for request_id=%d err=%v", row.RequestID, err)
			}
			continue
		}

		body := filehelper.BuildFileEditRequestReviewEmailBody(
			row.CreatedUserName,
			row.Status,
			row.FirstName,
			row.LastName,
			row.ReviewComment,
			details...,
		)

		if err := j.Mailer.SendOne(email, "Update to your submission", body); err != nil {
			if j.Logger != nil {
				j.Logger.Printf("file edit review email job: send failed for request_id=%d email=%s err=%v", row.RequestID, email, err)
			}
			continue
		}

		if err := j.DB.
			Table("file_edit_request").
			Where("request_id = ?", row.RequestID).
			Update("review_email_trigger_success", true).Error; err != nil {
			if j.Logger != nil {
				j.Logger.Printf("file edit review email job: sent email but failed to update flag for request_id=%d err=%v", row.RequestID, err)
			}
			continue
		}

		if j.Logger != nil {
			j.Logger.Printf("file edit review email job: success for request_id=%d email=%s", row.RequestID, email)
		}
	}

	return nil
}

func (j *FileEditReviewEmailJob) fileEditRequestDetails(requestID uint) ([]filehelper.FileEditRequestDetails, error) {
	var details []filehelper.FileEditRequestDetails

	err := j.DB.
		Table("file_edit_request_details").
		Select(`
			id,
			request_id,
			file_id,
			filename,
			row_id,
			field_name,
			COALESCE(old_value, '') AS old_value,
			COALESCE(new_value, '') AS new_value,
			COALESCE(status, 'pending') AS status,
			COALESCE(reviewer_comment, '') AS reviewer_comment,
			created_at
		`).
		Where("request_id = ?", requestID).
		Order("id ASC").
		Scan(&details).Error
	if err != nil {
		return nil, err
	}

	return details, nil
}
