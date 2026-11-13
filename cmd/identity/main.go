package main

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"nordik-drive-api/config"
	"nordik-drive-api/internal/auth"
	"nordik-drive-api/internal/database"
	"nordik-drive-api/internal/identityapp"
	"nordik-drive-api/internal/observability"
	"nordik-drive-api/internal/outbox"
)

var (
	openDatabase           = database.Open
	startTracing           = observability.Tracing
	configureNotifications = outbox.Configure
	serveRuntime           = identityapp.Serve
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	observability.Setup()
	cleanup := len(os.Args) == 2 && os.Args[1] == "signup-cleanup"
	if len(os.Args) > 1 && !cleanup {
		return errors.New("unsupported Identity command")
	}
	cfg := config.LoadConfig()
	cfg.RuntimeRole = "identity"
	if err := identityapp.ValidateRuntime(cfg, cleanup); err != nil {
		return err
	}
	db, closeDB, err := openDatabase(ctx, cfg)
	if err != nil {
		return err
	}
	defer closeDB()
	pool, err := db.DB()
	if err != nil {
		return errors.New("Identity database pool unavailable")
	}
	if cleanup {
		result, err := auth.CleanupSignup(ctx, pool)
		slog.InfoContext(ctx, "signup_cleanup", "challenges_deleted", result.Challenges, "idempotency_deleted", result.Idempotency, "limits_deleted", result.Limits, "already_running", result.AlreadyRunning, "succeeded", err == nil)
		return err
	}
	if err := observability.ValidateConfig(); err != nil {
		return err
	}
	flush, err := startTracing(ctx)
	if err != nil {
		return err
	}
	defer func() {
		end, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = flush(end)
	}()
	options, err := identityapp.LoadOptions()
	if err != nil {
		return err
	}
	writer, remote, err := configureNotifications(ctx)
	if err != nil {
		return err
	}
	handler, closeServer, err := identityapp.New(db, pool, cfg, options, writer, remote)
	if err != nil {
		return err
	}
	defer closeServer()
	return serveRuntime(ctx, handler, os.Getenv("PORT"))
}
