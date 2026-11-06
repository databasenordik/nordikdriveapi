package jobs

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"log/slog"
	"time"

	"nordik-drive-api/internal/database"
)

var ErrBusy = errors.New("job already running")

type RunFunc func(context.Context) error
type Runner struct {
	Pool *sql.DB
	Jobs map[string]RunFunc
}

// Run serializes all execution slots of a job with a session advisory lock and
// persists successful slots. A crashed running slot is retryable after its
// connection releases the lock. The lock is never held by a pooled idle session.
func (r *Runner) Run(ctx context.Context, name string, scheduled time.Time) (bool, error) {
	job, ok := r.Jobs[name]
	if !ok {
		return false, errors.New("unknown job")
	}
	conn, err := database.Acquire(ctx, r.Pool, 2*time.Second)
	if err != nil {
		return false, err
	}
	locked := false
	defer func() {
		if locked {
			release, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var released bool
			if e := conn.QueryRowContext(release, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, name).Scan(&released); e != nil || !released {
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			}
		}
		_ = conn.Close()
	}()
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,0))`, name).Scan(&locked); err != nil {
		return false, err
	}
	if !locked {
		return false, ErrBusy
	}
	var state string
	err = conn.QueryRowContext(ctx, `SELECT status FROM platform_job_runs WHERE job_name=$1 AND scheduled_at=$2`, name, scheduled.UTC()).Scan(&state)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if state == "succeeded" {
		return true, nil
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO platform_job_runs(job_name,scheduled_at,status,attempts,started_at) VALUES($1,$2,'running',1,statement_timestamp()) ON CONFLICT(job_name,scheduled_at) DO UPDATE SET status='running',attempts=platform_job_runs.attempts+1,started_at=statement_timestamp(),finished_at=NULL`, name, scheduled.UTC())
	if err != nil {
		return false, err
	}
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-jobCtx.Done():
				return
			case <-ticker.C:
				pingCtx, stop := context.WithTimeout(jobCtx, 2*time.Second)
				e := conn.PingContext(pingCtx)
				stop()
				if e != nil {
					cancel()
					return
				}
			}
		}
	}()
	// Always stop the monitor, including on a panic handled by HTTP middleware.
	defer func() { close(done); <-monitorDone }()
	err = job(jobCtx)
	if err == nil {
		err = jobCtx.Err()
	}
	state = "succeeded"
	if err != nil {
		state = "failed"
	}
	finishCtx, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	_, finishErr := conn.ExecContext(finishCtx, `UPDATE platform_job_runs SET status=$3,finished_at=statement_timestamp() WHERE job_name=$1 AND scheduled_at=$2`, name, scheduled.UTC(), state)
	slog.InfoContext(ctx, "job_completed", "job", name, "scheduled_at", scheduled.UTC(), "status", state)
	if err != nil {
		return false, err
	}
	return false, finishErr
}
