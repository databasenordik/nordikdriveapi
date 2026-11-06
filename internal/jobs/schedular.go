package jobs

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/robfig/cron/v3"
	"gorm.io/gorm"

	"nordik-drive-api/internal/mailer"
)

type Scheduler struct {
	cron   *cron.Cron
	cancel context.CancelFunc
}
type HonourRunner interface{ RunDailyHonours() error }
type SupportScheduleRunner interface{ RunScheduledMaintenance() error }

func NewScheduler(db *gorm.DB, mail mailer.EmailSender, honour HonourRunner, support SupportScheduleRunner, location *time.Location, logger *log.Logger) (*Scheduler, error) {
	pool, err := db.DB()
	if err != nil {
		return nil, err
	}
	return NewGuardedScheduler(&Runner{Pool: pool, Jobs: Registry(db, mail, honour, support, logger)}, location, logger)
}
func NewGuardedScheduler(runner *Runner, location *time.Location, logger *log.Logger) (*Scheduler, error) {
	if location == nil {
		location = time.UTC
	}
	root, cancel := context.WithCancel(context.Background())
	scheduler := cron.New(cron.WithLocation(location), cron.WithChain(cron.SkipIfStillRunning(cron.DefaultLogger)))
	for name, expression := range Schedules {
		if _, err := scheduler.AddFunc(expression, func() {
			ctx, cancel := context.WithTimeout(root, 900*time.Second)
			defer cancel()
			_, err := runner.Run(ctx, name, ScheduledSlot(name, time.Now(), location))
			if err != nil && logger != nil {
				logger.Printf("scheduled job failed: job=%s", name)
			}
		}); err != nil {
			cancel()
			return nil, fmt.Errorf("invalid job schedule: %s", name)
		}
	}
	return &Scheduler{cron: scheduler, cancel: cancel}, nil
}
func ScheduledSlot(name string, now time.Time, location *time.Location) time.Time {
	switch name {
	case "file-review-email", "form-review-email":
		return now.UTC().Truncate(5 * time.Minute)
	case "file-normalization":
		return now.UTC().Truncate(10 * time.Minute)
	case "daily-honour", "support-maintenance":
		local := now.In(location)
		return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location).UTC()
	default:
		return now.UTC().Truncate(time.Minute)
	}
}
func (s *Scheduler) Start() { s.cron.Start() }
func (s *Scheduler) Stop()  { s.cancel(); <-s.cron.Stop().Done() }
