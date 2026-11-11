package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestSignupCleanupBatchCapAndSanitizedFailures(t *testing.T) {
	for _, failure := range []string{"", "lock", "clock", "batch", "count"} {
		t.Run(failure, func(t *testing.T) {
			pool, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			pool.SetMaxOpenConns(1)
			secretError := errors.New("synthetic-sensitive-detail")
			lock := mock.ExpectQuery("SELECT pg_try_advisory_lock").WithArgs("signup-expiry-cleanup")
			if failure == "lock" {
				lock.WillReturnError(secretError)
			} else {
				lock.WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
				now := time.Now().UTC()
				clock := mock.ExpectQuery("SELECT statement_timestamp")
				if failure == "clock" {
					clock.WillReturnError(secretError)
				} else {
					clock.WillReturnRows(sqlmock.NewRows([]string{"now"}).AddRow(now))
					if failure == "batch" {
						mock.ExpectExec("DELETE FROM signup_otp_limits").WithArgs(now).WillReturnError(secretError)
					} else if failure == "count" {
						mock.ExpectExec("DELETE FROM signup_otp_limits").WithArgs(now).WillReturnResult(sqlmock.NewErrorResult(secretError))
					} else {
						for range 10 {
							mock.ExpectExec("DELETE FROM signup_otp_limits").WithArgs(now).WillReturnResult(sqlmock.NewResult(0, 100))
						}
						mock.ExpectExec("DELETE FROM signup_idempotency").WithArgs(now).WillReturnResult(sqlmock.NewResult(0, 0))
						mock.ExpectExec("DELETE FROM signup_otp_challenges").WithArgs(now.Add(-24 * time.Hour)).WillReturnResult(sqlmock.NewResult(0, 0))
					}
				}
				mock.ExpectQuery("SELECT pg_advisory_unlock").WithArgs("signup-expiry-cleanup").WillReturnRows(sqlmock.NewRows([]string{"released"}).AddRow(true))
			}
			result, err := CleanupSignup(context.Background(), pool)
			if failure == "" {
				if err != nil || result.Limits != 1000 || result.Challenges != 0 {
					t.Fatal("bounded cleanup failed", result, err)
				}
			} else if err == nil || strings.Contains(err.Error(), "sensitive") {
				t.Fatal("failure was lost or sensitive details escaped", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSignupCleanupClosedPool(t *testing.T) {
	pool, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	_ = pool.Close()
	if _, err := CleanupSignup(context.Background(), pool); err == nil {
		t.Fatal("unavailable cleanup pool was acknowledged")
	}
}
