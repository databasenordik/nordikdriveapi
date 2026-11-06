package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/genai"
	"gorm.io/gorm"

	"nordik-drive-api/config"
	"nordik-drive-api/internal/database"
	"nordik-drive-api/internal/jobs"
	"nordik-drive-api/internal/observability"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	observability.Setup()
	if err := observability.ValidateConfig(); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shutdownTracing, err := observability.Tracing(ctx)
	if err != nil {
		return err
	}
	defer func() {
		flush, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = shutdownTracing(flush)
	}()
	cfg := config.LoadConfig()
	mode := strings.TrimSpace(os.Getenv("SCHEDULER_MODE"))
	if mode == "" {
		mode = "legacy"
	}
	if mode != "legacy" && mode != "external" && mode != "disabled" {
		return errors.New("SCHEDULER_MODE must be legacy, external or disabled")
	}
	if cfg.RuntimeRole != "monolith" && cfg.RuntimeRole != "worker" {
		return errors.New("APP_ROLE must be monolith or worker")
	}
	var closeDB func()
	dependencies, err := initializeRuntime(ctx, cfg, func(c config.Config) (*gorm.DB, error) {
		db, cleanup, err := database.Open(ctx, c)
		closeDB = cleanup
		return db, err
	}, genai.NewClient)
	if closeDB != nil {
		defer closeDB()
	}
	if err != nil {
		return err
	}
	defer dependencies.Pool.Close()
	db := dependencies.DB
	if cfg.RuntimeRole == "worker" {
		router, err := workerRouter(cfg, dependencies)
		if err != nil {
			return err
		}
		return serve(ctx, router)
	}
	app, err := newApplication(cfg, dependencies)
	if err != nil {
		return err
	}
	defer app.Close()
	if mode == "legacy" {
		initialCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		runner := &jobs.Runner{Pool: dependencies.Pool, Jobs: jobs.Registry(db, app.Mailer, app.Honour, app.Support, log.Default())}
		if _, err := runner.Run(initialCtx, "support-maintenance", jobs.ScheduledSlot("support-maintenance", time.Now(), loadAppLocation(cfg.AppTimeZone))); err != nil {
			log.Print("initial support maintenance did not complete")
		}
		cancel()
	}

	if mode == "legacy" {
		scheduler, err := jobs.NewScheduler(db, app.Mailer, app.Honour, app.Support, loadAppLocation(cfg.AppTimeZone), log.Default())
		if err != nil {
			return errors.New("failed to create scheduler")
		}
		scheduler.Start()
		defer scheduler.Stop()
	}
	return serve(ctx, app.Router)
}

func loadAppLocation(name string) *time.Location {
	if name == "" {
		name = "America/Toronto"
	}

	loc, err := time.LoadLocation(name)
	if err != nil {
		log.Printf("invalid APP_TIMEZONE=%q, defaulting to America/Toronto: %v", name, err)
		fallback, fallbackErr := time.LoadLocation("America/Toronto")
		if fallbackErr != nil {
			log.Printf("failed to load fallback timezone America/Toronto: %v", fallbackErr)
			return time.Local
		}
		return fallback
	}

	return loc
}
