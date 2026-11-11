//go:build integration

package integration

import (
	"context"
	"os"
	"strings"
	"testing"

	"nordik-drive-api/internal/auth"
)

func TestPostgresSignupCleanupRetentionLocksAndSingleConnection(t *testing.T) {
	db := newPostgresFixture(t)
	for _, ddl := range []string{
		`CREATE TABLE signup_otp_limits (key text PRIMARY KEY,count int,expires_at timestamptz)`,
		`CREATE TABLE signup_idempotency (key text PRIMARY KEY,expires_at timestamptz)`,
		`CREATE TABLE signup_otp_challenges (id text PRIMARY KEY,expires_at timestamptz,proof_expires_at timestamptz)`,
		`CREATE TABLE users (id integer PRIMARY KEY)`,
		`INSERT INTO users VALUES (1)`,
		`INSERT INTO signup_otp_limits VALUES ('expired',1,now()-interval '1 minute'),('active',1,now()+interval '1 hour')`,
		`INSERT INTO signup_idempotency VALUES ('expired',now()-interval '1 minute'),('active',now()+interval '1 hour')`,
		`INSERT INTO signup_otp_challenges VALUES ('old',now()-interval '26 hours',NULL),('recent',now()-interval '1 hour',NULL),('proof',now()-interval '26 hours',now()+interval '1 minute'),('locked',now()-interval '26 hours',NULL)`,
	} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatal(err)
		}
	}
	pool, _ := db.DB()
	locked, err := pool.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Rollback()
	if _, err := locked.Exec("SELECT id FROM signup_otp_challenges WHERE id='locked' FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	// One connection is occupied by the simulated signup. Cleanup itself has
	// only one remaining slot and must not reacquire from the pool under lock.
	pool.SetMaxOpenConns(2)
	result, err := auth.CleanupSignup(context.Background(), pool)
	if err != nil || result.Challenges != 1 || result.Idempotency != 1 || result.Limits != 1 {
		t.Fatal("retention/lock cleanup failed", result, err)
	}
	var remaining int64
	if err := db.Table("signup_otp_challenges").Count(&remaining).Error; err != nil || remaining != 3 {
		t.Fatal("active proof, recent challenge or locked record removed", remaining, err)
	}
	if err := locked.Rollback(); err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	result, err = auth.CleanupSignup(context.Background(), pool)
	if err != nil || result.Challenges != 1 || result.Idempotency != 0 || result.Limits != 0 {
		t.Fatal("one-connection cleanup or retry failed", result, err)
	}
	conn, err := pool.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var acquired bool
	if err := conn.QueryRowContext(context.Background(), "SELECT pg_try_advisory_lock(hashtextextended('signup-expiry-cleanup',0))").Scan(&acquired); err != nil || !acquired {
		t.Fatal("cleanup leaked its singleton lock", err)
	}
	pool.SetMaxOpenConns(2)
	result, err = auth.CleanupSignup(context.Background(), pool)
	if err != nil || !result.AlreadyRunning {
		t.Fatal("overlapping cleanup was not skipped", result, err)
	}
	_, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock(hashtextextended('signup-expiry-cleanup',0))")
	_ = conn.Close()
	if err := db.Table("users").Count(&remaining).Error; err != nil || remaining != 1 {
		t.Fatal("cleanup changed accounts", err)
	}
	// Validate the actual additive expiry-index migration against the owned
	// fixture schema; the deployed migration itself remains public-qualified.
	migration, err := os.ReadFile("../../migrations/004_signup_cleanup.up.sql")
	if err != nil || len(migration) == 0 {
		t.Fatal("cleanup migration missing", err)
	}
	var schema string
	if err := db.Raw("SELECT current_schema()").Scan(&schema).Error; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := db.Exec(strings.ReplaceAll(string(migration), "public.", `"`+schema+`".`)).Error; err != nil {
			t.Fatal("additive cleanup migration failed", err)
		}
	}
}
