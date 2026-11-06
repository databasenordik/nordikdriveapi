package jobs

import (
	"context"
	"log"

	"gorm.io/gorm"

	"nordik-drive-api/internal/mailer"
	"nordik-drive-api/internal/requestscope"
)

func Registry(db *gorm.DB, mail mailer.EmailSender, honour HonourRunner, support SupportScheduleRunner, logger *log.Logger) map[string]RunFunc {
	return map[string]RunFunc{
		"file-review-email": func(ctx context.Context) error {
			return NewFileEditReviewEmailJob(db.WithContext(ctx), requestscope.Service(ctx, mail), logger).Run()
		},
		"form-review-email": func(ctx context.Context) error {
			return NewFormSubmissionReviewEmailJob(db.WithContext(ctx), requestscope.Service(ctx, mail), logger).Run()
		},
		"file-normalization": func(ctx context.Context) error { return NewFileDataNormalizationJob(db.WithContext(ctx), logger).Run() },
		"file-reconciliation": func(ctx context.Context) error {
			return NewFileVersionReconciliationJob(db.WithContext(ctx), logger).Run()
		},
		"daily-honour": func(ctx context.Context) error {
			if honour == nil {
				return nil
			}
			return requestscope.Service(ctx, honour).RunDailyHonours()
		},
		"support-maintenance": func(ctx context.Context) error {
			if support == nil {
				return nil
			}
			return requestscope.Service(ctx, support).RunScheduledMaintenance()
		},
	}
}

var Schedules = map[string]string{"file-review-email": "*/5 * * * *", "form-review-email": "*/5 * * * *", "file-normalization": "*/10 * * * *", "file-reconciliation": "* * * * *", "daily-honour": "0 0 * * *", "support-maintenance": "0 0 * * *"}
