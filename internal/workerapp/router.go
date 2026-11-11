// Package workerapp assembles only scheduled business work, without API routers.
package workerapp

import (
	"context"
	"database/sql"
	"errors"
	"github.com/gin-gonic/gin"
	"google.golang.org/genai"
	"gorm.io/gorm"
	"nordik-drive-api/config"
	"nordik-drive-api/internal/chat"
	"nordik-drive-api/internal/file"
	"nordik-drive-api/internal/honour"
	"nordik-drive-api/internal/jobs"
	"nordik-drive-api/internal/mailer"
	"nordik-drive-api/internal/observability"
	"nordik-drive-api/internal/outbox"
	"nordik-drive-api/internal/supportrequest"
	"nordik-drive-api/internal/supportschedule"
	"os"
	"time"
)

func Router(ctx context.Context, cfg config.Config, db *gorm.DB, pool *sql.DB, vertex *genai.Client) (*gin.Engine, error) {
	prefix := os.Getenv("WORKER_SCHEDULER_PREFIX")
	if prefix == "" {
		return nil, errors.New("WORKER_SCHEDULER_PREFIX is required")
	}
	writer, remote, err := outbox.Configure(ctx)
	if err != nil {
		return nil, err
	}
	var mail *mailer.Service
	var email mailer.EmailSender
	var supportMail supportschedule.SupportScheduleMailer
	if writer == nil {
		mail = mailer.NewService(cfg.GmailUser, cfg.GmailPass, "smtp.gmail.com", "587")
		email = mail
		supportMail = mail
	}
	honourService := honour.NewService(db, &chat.ChatService{DB: db, Client: vertex})
	zone := cfg.AppTimeZone
	if zone == "" {
		zone = "America/Toronto"
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		return nil, errors.New("invalid worker timezone")
	}
	honourService.Location = location
	var provider supportschedule.MeetingProvider = supportschedule.DisabledMeetingProvider{}
	if cfg.ZoomEnabled {
		provider, err = supportschedule.NewZoomMeetingProvider(supportschedule.ZoomConfig{AccountID: cfg.ZoomAccountID, ClientID: cfg.ZoomClientID, ClientSecret: cfg.ZoomClientSecret})
		if err != nil {
			return nil, errors.New("invalid Zoom configuration")
		}
	}
	support := &supportschedule.Service{DB: db, Mailer: supportMail, MeetingProvider: provider}
	support.Outbox = writer
	registry := jobs.Registry(db, email, honourService, support, nil)
	if writer != nil {
		registry["file-review-email"] = func(ctx context.Context) error {
			job := jobs.NewFileEditReviewEmailJob(db.WithContext(ctx), nil, nil)
			job.Outbox = writer
			job.Remote = remote
			return job.Run()
		}
		registry["form-review-email"] = func(ctx context.Context) error {
			job := jobs.NewFormSubmissionReviewEmailJob(db.WithContext(ctx), nil, nil)
			job.Outbox = writer
			job.Remote = remote
			return job.Run()
		}
		registry["notification-outbox-dispatch"] = func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			dispatchErr := (&outbox.Dispatcher{DB: db, Remote: remote}).Run(ctx)
			supportErr := outbox.RunPhase(ctx, 3*time.Second, func(phase context.Context) error {
				owner := *support
				owner.DB = db.WithContext(phase)
				return owner.RecoverNotifications(phase)
			})
			statusErr := outbox.RunPhase(ctx, 2*time.Second, func(phase context.Context) error {
				return supportrequest.ReconcileOutgoing(phase, db, remote)
			})
			return errors.Join(dispatchErr, supportErr, statusErr)
		}
	}
	if writer != nil && writer.Publisher != nil {
		for name, run := range registry {
			registry[name] = func(ctx context.Context) error {
				scope, pending := outbox.Capture(ctx)
				err := run(scope)
				outbox.PublishPending(scope, db, writer.Publisher, pending)
				return err
			}
		}
	}
	router := gin.New()
	router.Use(observability.Middleware())
	router.GET("/healthz", func(c *gin.Context) { c.Status(200) })
	if account := os.Getenv("FILE_VERSION_EVENT_ACCOUNT"); account != "" {
		audience, scheduler := os.Getenv("FILE_VERSION_WORKER_URL"), os.Getenv("WORKER_SCHEDULER_ACCOUNT")
		if audience == "" || scheduler == "" || scheduler == account {
			return nil, errors.New("distinct file task and scheduler identities required")
		}
		router.Use(jobs.SchedulerGuard(audience, scheduler))
		handler, err := file.NewVersionEventHandler(db, audience, account)
		if err != nil {
			return nil, err
		}
		router.POST("/internal/v1/work/file-version", gin.WrapH(handler))
	}
	jobs.RegisterWorker(router, &jobs.Runner{Pool: pool, Jobs: registry}, prefix)
	observability.RegisterControls(router, pool)
	return router, nil
}
