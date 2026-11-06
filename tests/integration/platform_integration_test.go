//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"nordik-drive-api/internal/gateway"
	"nordik-drive-api/internal/jobs"
)

func platformFixture(t testing.TB) *gorm.DB {
	t.Helper()
	db := newPostgresFixture(t)
	data, err := os.ReadFile("../../migrations/002_platform.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	// Preserve DDL/constraints while mapping only namespaces into the fixture's
	// randomly owned schema. Nothing is created in public or gateway.
	sql := strings.ReplaceAll(string(data), "CREATE SCHEMA IF NOT EXISTS gateway;", "")
	sql = strings.ReplaceAll(sql, "REVOKE ALL ON SCHEMA gateway FROM PUBLIC;", "")
	sql = strings.ReplaceAll(strings.ReplaceAll(sql, "gateway.", ""), "public.", "")
	if err := db.Exec(sql).Error; err != nil {
		t.Fatal(err)
	}
	return db
}
func limiterSubject(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}

func TestPostgresLimiterConcurrentReplicasAndWindows(t *testing.T) {
	db := platformFixture(t)
	pool, _ := db.DB()
	pool.SetMaxOpenConns(12)
	pool.SetMaxIdleConns(12)
	var warm []*sql.Conn
	for i := 0; i < 12; i++ {
		connection, err := pool.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		warm = append(warm, connection)
	}
	for _, connection := range warm {
		_ = connection.Close()
	}
	var seconds float64
	if err := db.Raw(`SELECT extract(second FROM statement_timestamp())`).Scan(&seconds).Error; err != nil {
		t.Fatal(err)
	}
	if seconds > 57 {
		time.Sleep(time.Duration((61 - seconds) * float64(time.Second)))
	}
	limiters := []gateway.PostgresLimiter{{Pool: pool}, {Pool: pool}, {Pool: pool}}
	var allowed atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 48; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			decision, err := limiters[i%3].Check(context.Background(), "shared", limiterSubject("one-user"), 7)
			if err != nil {
				t.Errorf("limiter failed: %v", err)
				return
			}
			if decision.RetryAfter < 1 || decision.RetryAfter > 60 {
				t.Error("invalid retry interval")
			}
			if decision.Allowed {
				allowed.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if allowed.Load() != 7 {
		t.Fatalf("admitted %d requests; limit is 7 across all replicas", allowed.Load())
	}
	var count int
	db.Raw(`SELECT count FROM rate_limits WHERE policy='shared'`).Scan(&count)
	if count != 7 {
		t.Fatal("exhausted counter continued incrementing")
	}
	// A previous full fixed window must not block a new window.
	if err := db.Exec(`INSERT INTO rate_limits VALUES('previous',?,date_trunc('minute',statement_timestamp())-interval '1 minute',7,statement_timestamp()+interval '1 minute')`, limiterSubject("one-user")).Error; err != nil {
		t.Fatal(err)
	}
	decision, err := limiters[0].Check(context.Background(), "previous", limiterSubject("one-user"), 7)
	if err != nil || !decision.Allowed {
		t.Fatal("new window did not reset quota")
	}
	// A missing table is unavailability, never quota exhaustion.
	if err := db.Exec(`DROP TABLE rate_limits`).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := limiters[0].Check(context.Background(), "shared", limiterSubject("one-user"), 7); err == nil {
		t.Fatal("missing authoritative state was silently allowed")
	}
}

func TestPostgresLimiterCleanupIsBounded(t *testing.T) {
	db := platformFixture(t)
	pool, _ := db.DB()
	if err := db.Exec(`INSERT INTO rate_limits SELECT 'expired',repeat('a',64),statement_timestamp()-i*interval '1 minute',1,statement_timestamp()-interval '1 minute' FROM generate_series(1,1005) AS i`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO rate_limits VALUES('live',repeat('b',64),statement_timestamp(),1,statement_timestamp()+interval '1 minute')`).Error; err != nil {
		t.Fatal(err)
	}
	for _, want := range []int64{1000, 5, 0} {
		count, err := gateway.Cleanup(context.Background(), pool)
		if err != nil || count != want {
			t.Fatalf("cleanup count=%d want=%d: %v", count, want, err)
		}
	}
	var live int64
	db.Table("rate_limits").Count(&live)
	if live != 1 {
		t.Fatal("cleanup deleted active counter")
	}
}

func TestPostgresJobOwnershipDeduplicationAndRetry(t *testing.T) {
	db := platformFixture(t)
	pool, _ := db.DB()
	pool.SetMaxOpenConns(8)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	runner := &jobs.Runner{Pool: pool, Jobs: map[string]jobs.RunFunc{"fixture": func(ctx context.Context) error {
		calls.Add(1)
		close(started)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}}
	slot := time.Now().UTC().Truncate(time.Minute)
	done := make(chan error, 1)
	go func() { _, err := runner.Run(context.Background(), "fixture", slot); done <- err }()
	<-started
	if _, err := runner.Run(context.Background(), "fixture", slot.Add(time.Minute)); !errors.Is(err, jobs.ErrBusy) {
		t.Fatalf("overlapping slot admitted: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	duplicate, err := runner.Run(context.Background(), "fixture", slot)
	if err != nil || !duplicate || calls.Load() != 1 {
		t.Fatal("successful slot executed twice")
	}
	// Provider errors remain retryable and do not poison the pooled lock session.
	runner.Jobs["fixture"] = func(context.Context) error { return errors.New("provider unavailable") }
	if _, err := runner.Run(context.Background(), "fixture", slot.Add(time.Minute)); err == nil {
		t.Fatal("failed provider marked successful")
	}
	runner.Jobs["fixture"] = func(context.Context) error { return nil }
	if _, err := runner.Run(context.Background(), "fixture", slot.Add(time.Minute)); err != nil {
		t.Fatal("failed slot could not recover")
	}
	var state struct {
		Status   string
		Attempts int
	}
	db.Raw(`SELECT status,attempts FROM platform_job_runs WHERE job_name='fixture' AND scheduled_at=?`, slot.Add(time.Minute)).Scan(&state)
	if state.Status != "succeeded" || state.Attempts != 2 {
		t.Fatalf("incorrect persistent retry state: %+v", state)
	}
	// Simulate a process crash that left a running journal but no live lock.
	if err := db.Exec(`UPDATE platform_job_runs SET status='running' WHERE job_name='fixture' AND scheduled_at=?`, slot).Error; err != nil {
		t.Fatal(err)
	}
	if duplicate, err := runner.Run(context.Background(), "fixture", slot); err != nil || duplicate {
		t.Fatal("crashed slot did not recover")
	}
}

func TestPostgresJobCancellationReleasesOwnership(t *testing.T) {
	db := platformFixture(t)
	pool, _ := db.DB()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	runner := &jobs.Runner{Pool: pool, Jobs: map[string]jobs.RunFunc{"cancel": func(ctx context.Context) error { close(started); <-ctx.Done(); return ctx.Err() }}}
	slot := time.Now().UTC()
	done := make(chan error, 1)
	go func() { _, err := runner.Run(ctx, "cancel", slot); done <- err }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	runner.Jobs["cancel"] = func(context.Context) error { return nil }
	if _, err := runner.Run(context.Background(), "cancel", slot); err != nil {
		t.Fatalf("cancelled owner retained lock: %v", err)
	}
}

func BenchmarkPostgresPlatformLimiter(b *testing.B) {
	db := platformFixture(b)
	pool, _ := db.DB()
	limiter := gateway.PostgresLimiter{Pool: pool}
	subject := limiterSubject("benchmark")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := limiter.Check(context.Background(), "benchmark", subject, 1000000000); err != nil {
			b.Fatal(err)
		}
	}
}

func TestPostgresRuntimeTransactionCancellationReleasesSingleConnection(t *testing.T) {
	db := platformFixture(t)
	pool, _ := db.DB()
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	ctx, cancel := context.WithCancel(context.Background())
	tx := db.WithContext(ctx).Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	if err := tx.Exec("SELECT 1").Error; err != nil {
		t.Fatal(err)
	}
	cancel()
	check, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	var value int
	if err := db.WithContext(check).Raw("SELECT 1").Scan(&value).Error; err != nil || value != 1 {
		t.Fatalf("cancelled transaction did not release the only connection: %v", err)
	}
}

func TestPostgresLegacyDescriptionPreparationPreservesExistingRows(t *testing.T) {
	db := newPostgresFixture(t)
	if err := db.Exec(`CREATE TABLE file(id integer PRIMARY KEY); CREATE TABLE file_version(id integer PRIMARY KEY); INSERT INTO file VALUES(1); INSERT INTO file_version VALUES(1)`).Error; err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../../migrations/002_legacy_descriptions.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := db.Exec(strings.ReplaceAll(string(data), "public.", "")).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"file", "file_version"} {
		var count int64
		if err := db.Table(table).Count(&count).Error; err != nil || count != 1 {
			t.Fatal("legacy migration changed existing rows")
		}
		if !db.Migrator().HasColumn(table, "description") {
			t.Fatal("runtime would still need ALTER permission")
		}
	}
}
