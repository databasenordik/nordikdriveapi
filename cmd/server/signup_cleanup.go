package main

import (
	"context"
	"errors"
	"log/slog"

	"nordik-drive-api/config"
	"nordik-drive-api/internal/auth"
	"nordik-drive-api/internal/database"
)

func runSignupCleanup(ctx context.Context, cfg config.Config) error {
	if cfg.DBAuthMode != "iam" || cfg.DBMaxOpen != 1 || cfg.DBSchema != "public" {
		return errors.New("signup cleanup requires IAM, DB_SCHEMA=public and DB_MAX_OPEN=1")
	}
	db, closeDB, err := database.Open(ctx, cfg)
	if err != nil {
		return errors.New("signup cleanup database initialization failed")
	}
	defer closeDB()
	pool, err := db.DB()
	if err != nil {
		return errors.New("signup cleanup pool unavailable")
	}
	result, err := auth.CleanupSignup(ctx, pool)
	slog.InfoContext(ctx, "signup_cleanup", "challenges_deleted", result.Challenges, "idempotency_deleted", result.Idempotency, "limits_deleted", result.Limits, "already_running", result.AlreadyRunning, "succeeded", err == nil)
	return err
}
