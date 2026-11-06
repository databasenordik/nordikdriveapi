package jobs

import (
	"context"
	"database/sql"
	"errors"
	"github.com/DATA-DOG/go-sqlmock"
	"testing"
	"time"
)

func TestRunnerStorageFailuresDoNotExecuteBusinessWork(t *testing.T) {
	for _, stage := range []string{"acquisition", "lock", "read", "journal"} {
		t.Run(stage, func(t *testing.T) {
			pool, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			ctx := context.Background()
			called := false
			runner := &Runner{Pool: pool, Jobs: map[string]RunFunc{"fixture": func(context.Context) error { called = true; return nil }}}
			if stage == "acquisition" {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			} else if stage == "lock" {
				mock.ExpectQuery("SELECT pg_try_advisory_lock").WillReturnError(errors.New("lock unavailable"))
			} else {
				mock.ExpectQuery("SELECT pg_try_advisory_lock").WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
				if stage == "read" {
					mock.ExpectQuery("SELECT status").WillReturnError(errors.New("journal read failed"))
				} else {
					mock.ExpectQuery("SELECT status").WillReturnError(sql.ErrNoRows)
					mock.ExpectExec("INSERT INTO").WillReturnError(errors.New("journal write failed"))
				}
				mock.ExpectQuery("SELECT pg_advisory_unlock").WillReturnRows(sqlmock.NewRows([]string{"released"}).AddRow(true))
			}
			if _, err := runner.Run(ctx, "fixture", time.Now()); err == nil || called {
				t.Fatal("work executed without durable ownership")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
	if _, err := (&Runner{Jobs: map[string]RunFunc{}}).Run(context.Background(), "unknown", time.Now()); err == nil {
		t.Fatal("unknown job admitted")
	}
}

func TestRunnerDiscardsSessionIfUnlockFails(t *testing.T) {
	pool, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	mock.ExpectQuery("SELECT pg_try_advisory_lock").WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	mock.ExpectQuery("SELECT status").WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("succeeded"))
	mock.ExpectQuery("SELECT pg_advisory_unlock").WillReturnError(errors.New("lost session"))
	mock.ExpectClose()
	runner := &Runner{Pool: pool, Jobs: map[string]RunFunc{"fixture": func(context.Context) error { t.Fatal("successful slot repeated"); return nil }}}
	if duplicate, err := runner.Run(context.Background(), "fixture", time.Now()); err != nil || !duplicate {
		t.Fatal("successful slot not recognized")
	}
	if pool.Stats().Idle != 0 {
		t.Fatal("session with uncertain lock returned to idle pool")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerCancelsWorkWhenOwnershipConnectionFails(t *testing.T) {
	pool, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	mock.ExpectQuery("SELECT pg_try_advisory_lock").WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	mock.ExpectQuery("SELECT status").WillReturnError(sql.ErrNoRows)
	mock.ExpectExec("INSERT INTO").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectPing().WillReturnError(errors.New("ownership connection lost"))
	mock.ExpectExec("UPDATE platform_job_runs").WithArgs("fixture", sqlmock.AnyArg(), "failed").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT pg_advisory_unlock").WillReturnRows(sqlmock.NewRows([]string{"released"}).AddRow(true))
	runner := &Runner{Pool: pool, Jobs: map[string]RunFunc{"fixture": func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }}}
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	if _, err := runner.Run(ctx, "fixture", time.Now()); !errors.Is(err, context.Canceled) {
		t.Fatalf("ownership loss did not cancel the provider: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
