package auth

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"time"

	"nordik-drive-api/internal/database"
)

// SignupCleanupResult contains counts only, never challenges, addresses or proofs.
type SignupCleanupResult struct {
	Challenges, Idempotency, Limits int64
	AlreadyRunning                  bool
}

// CleanupSignup is a finite maintenance command. Every query uses the same
// connection, including the singleton lock, so a one-connection pool suffices.
func CleanupSignup(ctx context.Context, pool *sql.DB) (SignupCleanupResult, error) {
	var result SignupCleanupResult
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := database.Acquire(ctx, pool, 2*time.Second)
	if err != nil {
		return result, errors.New("signup cleanup database unavailable")
	}
	defer conn.Close()
	const lockName = "signup-expiry-cleanup"
	var locked bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock(hashtextextended($1,0))", lockName).Scan(&locked); err != nil {
		return result, errors.New("signup cleanup lock unavailable")
	}
	if !locked {
		result.AlreadyRunning = true
		return result, nil
	}
	defer func() {
		release, stop := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer stop()
		var released bool
		if err := conn.QueryRowContext(release, "SELECT pg_advisory_unlock(hashtextextended($1,0))", lockName).Scan(&released); err != nil || !released {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	var now time.Time
	if err := conn.QueryRowContext(ctx, "SELECT statement_timestamp()").Scan(&now); err != nil {
		return result, errors.New("signup cleanup clock unavailable")
	}
	// Keep challenge checks through the full 24-hour idempotency window following
	// their last possible use. Cleanup never touches users or delivery metadata.
	for _, item := range []struct {
		query  string
		cutoff time.Time
		count  *int64
	}{
		{`DELETE FROM signup_otp_limits WHERE key IN (SELECT key FROM signup_otp_limits WHERE expires_at <= $1 ORDER BY expires_at,key LIMIT 100 FOR UPDATE SKIP LOCKED)`, now, &result.Limits},
		{`DELETE FROM signup_idempotency WHERE key IN (SELECT key FROM signup_idempotency WHERE expires_at <= $1 ORDER BY expires_at,key LIMIT 100 FOR UPDATE SKIP LOCKED)`, now, &result.Idempotency},
		{`DELETE FROM signup_otp_challenges WHERE id IN (SELECT id FROM signup_otp_challenges WHERE GREATEST(expires_at,COALESCE(proof_expires_at,expires_at)) <= $1 ORDER BY GREATEST(expires_at,COALESCE(proof_expires_at,expires_at)),id LIMIT 100 FOR UPDATE SKIP LOCKED)`, now.Add(-24 * time.Hour), &result.Challenges},
	} {
		for range 10 {
			batch, stop := context.WithTimeout(ctx, time.Second)
			deleted, err := conn.ExecContext(batch, item.query, item.cutoff)
			stop()
			if err != nil {
				return result, errors.New("signup cleanup batch did not complete")
			}
			n, err := deleted.RowsAffected()
			if err != nil {
				return result, errors.New("signup cleanup count unavailable")
			}
			*item.count += n
			if n < 100 {
				break
			}
		}
	}
	return result, nil
}
