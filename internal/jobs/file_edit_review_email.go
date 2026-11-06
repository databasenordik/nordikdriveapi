package jobs

import (
	"fmt"
	"log"
	"strings"

	"gorm.io/gorm"

	filehelper "nordik-drive-api/internal/file"
	"nordik-drive-api/internal/mailer"
	"nordik-drive-api/internal/requestscope"
)

type FileEditReviewEmailJob struct {
	DB        *gorm.DB
	Mailer    mailer.EmailSender
	Logger    *log.Logger
	BatchSize int
}

type fileEditReviewEmailRow struct {
	RequestID       uint   `gorm:"column:request_id"`
	UserID          uint   `gorm:"column:user_id"`
	Status          string `gorm:"column:status"`
	FirstName       string `gorm:"column:firstname"`
	LastName        string `gorm:"column:lastname"`
	ReviewComment   string `gorm:"column:reviewer_comment"`
	Email           string `gorm:"column:email"`
	CreatedUserName string `gorm:"column:created_user_name"`
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

	err := j.DB.
		Table("file_edit_request fer").
		Select(`
			fer.request_id,
			fer.user_id,
			fer.status,
			fer.firstname,
			fer.lastname,
			fer.reviewer_comment,
			u.email,
			TRIM(COALESCE(u.firstname, '') || ' ' || COALESCE(u.lastname, '')) AS created_user_name
		`).
		Joins("JOIN users u ON u.id = fer.user_id").
		Where("fer.status <> ? AND fer.review_email_trigger_success = ?", "pending", false).
		Order("fer.created_at ASC").
		Limit(j.BatchSize).
		Scan(&rows).Error
	if err != nil {
		return fmt.Errorf("failed to fetch file edit requests pending review emails: %w", err)
	}

	for _, row := range rows {
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
