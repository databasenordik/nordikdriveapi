// Package database owns connection creation; runtime identities never migrate.
package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"regexp"
	"time"

	"cloud.google.com/go/cloudsqlconn"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"nordik-drive-api/config"
)

// Open uses pgx for both modes and returns cleanup for the pool and connector.
func Open(ctx context.Context, cfg config.Config) (*gorm.DB, func(), error) {
	mode := cfg.DBAuthMode
	if mode == "" {
		mode = "password"
	}
	if mode != "password" && mode != "iam" {
		return nil, nil, errors.New("DB_AUTH_MODE must be password or iam")
	}
	u := &url.URL{Scheme: "postgres", Host: net.JoinHostPort(cfg.DBHost, cfg.DBPort), Path: "/" + cfg.DBName}
	u.User = url.UserPassword(cfg.DBUser, cfg.DBPassword)
	q := u.Query()
	q.Set("sslmode", "disable")
	q.Set("connect_timeout", "10")
	u.RawQuery = q.Encode()
	var dialer *cloudsqlconn.Dialer
	var pgConfig *pgx.ConnConfig
	var err error
	if mode == "iam" {
		if cfg.CloudSQLInstance == "" || cfg.DBUser == "" || cfg.DBName == "" || cfg.DBPassword != "" {
			return nil, nil, errors.New("IAM mode requires CLOUD_SQL_INSTANCE, DB_USER, DB_NAME and no DB_PASSWORD")
		}
		if cfg.CloudSQLIPType != "public" && cfg.CloudSQLIPType != "private" {
			return nil, nil, errors.New("CLOUD_SQL_IP_TYPE must be public or private")
		}
		pgConfig, err = pgx.ParseConfig("sslmode=disable connect_timeout=10")
		if err != nil {
			return nil, nil, err
		}
		pgConfig.User, pgConfig.Database = cfg.DBUser, cfg.DBName
		dialer, err = cloudsqlconn.NewDialer(ctx, cloudsqlconn.WithIAMAuthN(), cloudsqlconn.WithLazyRefresh())
		if err != nil {
			return nil, nil, errors.New("Cloud SQL connector initialization failed")
		}
		pgConfig.DialFunc = func(dialCtx context.Context, _, _ string) (net.Conn, error) {
			options := []cloudsqlconn.DialOption{}
			if cfg.CloudSQLIPType == "private" {
				options = append(options, cloudsqlconn.WithPrivateIP())
			}
			return dialer.Dial(dialCtx, cfg.CloudSQLInstance, options...)
		}
	} else {
		pgConfig, err = pgx.ParseConfig(u.String())
		if err != nil {
			return nil, nil, errors.New("invalid database configuration")
		}
	}
	pgConfig.Tracer = queryTracer{}
	schema := cfg.DBSchema
	if schema == "" {
		schema = "public"
	}
	if !regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`).MatchString(schema) {
		if dialer != nil {
			_ = dialer.Close()
		}
		return nil, nil, errors.New("DB_SCHEMA must be a lowercase SQL identifier")
	}
	pgConfig.RuntimeParams["search_path"] = schema
	pool := stdlib.OpenDB(*pgConfig)
	maxOpen := cfg.DBMaxOpen
	if maxOpen <= 0 {
		maxOpen = 25
	}
	pool.SetMaxOpenConns(maxOpen)
	pool.SetMaxIdleConns(min(5, maxOpen))
	pool.SetConnMaxIdleTime(5 * time.Minute)
	pool.SetConnMaxLifetime(30 * time.Minute)
	cleanup := func() {
		_ = pool.Close()
		if dialer != nil {
			_ = dialer.Close()
		}
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.PingContext(pingCtx); err != nil {
		cleanup()
		return nil, nil, errors.New("database connectivity check failed")
	}
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: &boundedPool{pool: pool, budget: 2 * time.Second}}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		cleanup()
		return nil, nil, errors.New("database initialization failed")
	}
	return db, cleanup, nil
}

type queryKey struct{}
type queryTracer struct{}

func (queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	ctx, _ = otel.Tracer("nordik/database").Start(ctx, "postgres.query", trace.WithSpanKind(trace.SpanKindClient))
	return context.WithValue(ctx, queryKey{}, time.Now())
}
func (queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(attribute.Bool("db.failed", data.Err != nil))
	defer span.End()
	start, _ := ctx.Value(queryKey{}).(time.Time)
	// Never export SQL, parameters or driver errors (which may contain values).
	slog.DebugContext(ctx, "database_query", "duration_ms", time.Since(start).Milliseconds(), "failed", data.Err != nil)
}

func (queryTracer) TraceConnectStart(ctx context.Context, _ pgx.TraceConnectStartData) context.Context {
	ctx, _ = otel.Tracer("nordik/database").Start(ctx, "postgres.connect", trace.WithSpanKind(trace.SpanKindClient))
	return ctx
}
func (queryTracer) TraceConnectEnd(ctx context.Context, data pgx.TraceConnectEndData) {
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(attribute.Bool("db.failed", data.Err != nil))
	span.End()
}

func PoolStats(pool *sql.DB) map[string]any {
	s := pool.Stats()
	return map[string]any{"open": s.OpenConnections, "in_use": s.InUse, "idle": s.Idle, "wait_count_total": s.WaitCount, "wait_duration_ms_total": s.WaitDuration.Milliseconds()}
}

// Acquire measures actual acquisition, distinct from driver execution. Callers
// release the connection before performing remote provider work.
func Acquire(ctx context.Context, pool *sql.DB, budget time.Duration) (*sql.Conn, error) {
	ctx, span := otel.Tracer("nordik/database").Start(ctx, "postgres.acquire", trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()
	start := time.Now()
	acquireCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	conn, err := pool.Conn(acquireCtx)
	slog.DebugContext(ctx, "database_acquisition", "duration_ms", time.Since(start).Milliseconds(), "failed", err != nil)
	if err != nil {
		return nil, fmt.Errorf("database connection unavailable: %w", ctxOrError(ctx, err))
	}
	return conn, nil
}
func ctxOrError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return errors.New("database connection unavailable")
}
