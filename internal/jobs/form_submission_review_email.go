package jobs

import (
	"fmt"
	"log"
	"strings"

	"gorm.io/gorm"

	"nordik-drive-api/internal/formsubmission"
	"nordik-drive-api/internal/mailer"
	"nordik-drive-api/internal/outbox"
	"nordik-drive-api/internal/requestscope"
)

type FormSubmissionReviewEmailJob struct {
	DB        *gorm.DB
	Mailer    mailer.EmailSender
	Logger    *log.Logger
	BatchSize int
	Outbox    *outbox.Writer
	Remote    outbox.Remote
}

type formSubmissionReviewEmailRow struct {
	ID               int64  `gorm:"column:id"`
	FormLabel        string `gorm:"column:form_label"`
	Status           string `gorm:"column:status"`
	FirstName        string `gorm:"column:firstname"`
	LastName         string `gorm:"column:lastname"`
	ReviewerComment  string `gorm:"column:reviewer_comment"`
	CreatedUserEmail string `gorm:"column:created_user_email"`
	CreatedUserName  string `gorm:"column:created_user_name"`
}

func NewFormSubmissionReviewEmailJob(
	db *gorm.DB,
	mailerSvc mailer.EmailSender,
	logger *log.Logger,
) *FormSubmissionReviewEmailJob {
	return &FormSubmissionReviewEmailJob{
		DB:        db,
		Mailer:    mailerSvc,
		Logger:    logger,
		BatchSize: 100,
	}
}

func (j *FormSubmissionReviewEmailJob) Run() error {
	var rows []formSubmissionReviewEmailRow

	query := j.DB.Model(&formsubmission.FormSubmission{})
	if j.Outbox != nil {
		query = query.Where(`((COALESCE(form_submissions.review_notification_key, '') = '' AND form_submissions.status <> 'pending') OR EXISTS (
		 SELECT 1 FROM notification_outbox n WHERE n.producer = 'records'
		 AND n.event_key LIKE 'form-review:' || form_submissions.review_notification_key || ':%'
		 AND n.state = 'provider_accepted')) AND form_submissions.review_email_trigger_success = ?`, false)
	} else {
		query = query.Where("form_submissions.status <> ? AND form_submissions.review_email_trigger_success = ?", "pending", false)
	}
	err := query.
		Select(`
			form_submissions.id,
			form_submissions.form_label,
			form_submissions.status,
			form_submissions.firstname,
			form_submissions.lastname,
			form_submissions.reviewer_comment,
			u.email AS created_user_email,
			TRIM(COALESCE(u.firstname, '') || ' ' || COALESCE(u.lastname, '')) AS created_user_name
		`).
		Joins("JOIN users u ON u.id = form_submissions.created_by").
		Order("form_submissions.created_at ASC").
		Limit(j.BatchSize).
		Scan(&rows).Error
	if err != nil {
		return fmt.Errorf("failed to fetch form submissions pending review emails: %w", err)
	}

	for _, row := range rows {
		if j.Outbox != nil {
			owner := &formsubmission.FormSubmissionService{DB: j.DB, Outbox: j.Outbox}
			key, err := owner.RecoverReviewIntent(row.ID)
			if err != nil {
				return err
			}
			if key == "" {
				continue
			}
			accepted, err := outbox.ProviderAccepted(requestscope.Context(j.DB), j.DB, j.Remote, "form-review:"+key)
			if err != nil {
				return err
			}
			if accepted {
				if err := j.DB.Table("form_submissions").Where("id = ? AND review_notification_key = ?", row.ID, key).Update("review_email_trigger_success", true).Error; err != nil {
					return err
				}
			}
			continue
		}
		if err := requestscope.Context(j.DB).Err(); err != nil {
			return err
		}
		email := strings.TrimSpace(row.CreatedUserEmail)
		if email == "" {
			if j.Logger != nil {
				j.Logger.Printf("form submission review email job: empty email for submission_id=%d", row.ID)
			}
			continue
		}

		body := formsubmission.BuildFormSubmissionReviewEmailBody(
			row.CreatedUserName,
			row.FormLabel,
			row.Status,
			row.FirstName,
			row.LastName,
			row.ReviewerComment,
		)

		subject := fmt.Sprintf("Update to your submission for %s", row.FormLabel)

		if err := j.Mailer.SendOne(email, subject, body); err != nil {
			if j.Logger != nil {
				j.Logger.Printf(
					"form submission review email job: send failed for submission_id=%d email=%s err=%v",
					row.ID,
					email,
					err,
				)
			}
			continue
		}

		if err := j.DB.
			Model(&formsubmission.FormSubmission{}).
			Where("id = ?", row.ID).
			Update("review_email_trigger_success", true).Error; err != nil {
			if j.Logger != nil {
				j.Logger.Printf(
					"form submission review email job: sent email but failed to update flag for submission_id=%d err=%v",
					row.ID,
					err,
				)
			}
			continue
		}

		if j.Logger != nil {
			j.Logger.Printf(
				"form submission review email job: success for submission_id=%d email=%s",
				row.ID,
				email,
			)
		}
	}

	return nil
}
