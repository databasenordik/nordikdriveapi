package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"nordik-drive-api/config"
	"nordik-drive-api/internal/audit"
	"nordik-drive-api/internal/auditapp"
	"nordik-drive-api/internal/audittransport"
	"nordik-drive-api/internal/database"
	"nordik-drive-api/internal/identityclient"
	"nordik-drive-api/internal/observability"
)

var (
	openDatabase       = database.Open
	startTracing       = observability.Tracing
	configureDirectory = identityclient.Configured
	openSource         = func(ctx context.Context, address string) (audit.Source, error) {
		return audittransport.New(ctx, address)
	}
	serveRuntime = auditapp.Serve
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
func run() error {
	if len(os.Args) != 1 {
		return errors.New("Audit has no cleanup or migration commands")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	observability.Setup()
	cfg := config.LoadConfig()
	cfg.RuntimeRole = "audit"
	if err := auditapp.ValidateRuntime(cfg); err != nil {
		return err
	}
	if err := observability.ValidateConfig(); err != nil {
		return err
	}
	db, closeDB, err := openDatabase(ctx, cfg)
	if err != nil {
		return err
	}
	defer closeDB()
	pool, err := db.DB()
	if err != nil {
		return errors.New("Audit pool unavailable")
	}
	directory, closeDirectory, err := configureDirectory(ctx)
	if err != nil {
		return err
	}
	defer closeDirectory()
	if directory == nil {
		return errors.New("Audit requires the Identity directory")
	}
	service := &audit.Service{DB: db}
	for _, owner := range []struct{ name, producer string }{{"AUDIT_LEGACY_SOURCE_URL", "legacy"}, {"AUDIT_IDENTITY_SOURCE_URL", "identity"}} {
		client, err := openSource(ctx, os.Getenv(owner.name))
		if err != nil {
			return err
		}
		service.Sources = append(service.Sources, audit.OwnerSource{Producer: owner.producer, Source: client})
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
	handler, err := auditapp.New(db, pool, directory, service, os.Getenv("AUDIT_URL"), os.Getenv("AUDIT_TASK_ACCOUNT"))
	if err != nil {
		return err
	}
	return serveRuntime(ctx, handler, os.Getenv("PORT"))
}
