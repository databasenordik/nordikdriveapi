package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"nordik-drive-api/config"
	"nordik-drive-api/internal/audit"
	"nordik-drive-api/internal/auditoutbox"
	"nordik-drive-api/internal/identityclient"
)

type directoryStub struct{}

func (directoryStub) Profiles(context.Context, []int64, bool) (map[int64]identityclient.Profile, error) {
	return nil, nil
}
func (directoryStub) Principals(context.Context, []int64) (map[int64]identityclient.Principal, error) {
	return nil, nil
}
func (directoryStub) Search(context.Context, string, string, string) ([]int64, error) {
	return nil, nil
}

type sourceStub struct{}

func (sourceStub) Read(context.Context, string) (auditoutbox.SourceRecord, error) {
	return auditoutbox.SourceRecord{}, errors.New("unused")
}
func (sourceStub) Ack(context.Context, auditoutbox.Receipt) error { return errors.New("unused") }
func TestAuditBootstrapOwnsNoCleanupOrEmailRuntime(t *testing.T) {
	savedOpen, savedTrace, savedDirectory, savedSource, savedServe, args := openDatabase, startTracing, configureDirectory, openSource, serveRuntime, os.Args
	defer func() {
		openDatabase, startTracing, configureDirectory, openSource, serveRuntime, os.Args = savedOpen, savedTrace, savedDirectory, savedSource, savedServe, args
	}()
	for _, scenario := range []string{"success", "unsupported", "policy-error", "observability-error", "database-error", "directory-error", "directory-missing", "source-error", "trace-error", "handler-error", "serve-error"} {
		t.Run(scenario, func(t *testing.T) {
			os.Args = []string{"audit"}
			for name, value := range map[string]string{"DB_AUTH_MODE": "iam", "DB_MAX_OPEN": "1", "DB_SCHEMA": "audit,identity", "JWT_SECRET": "existing-test-jwt", "REQUEST_TIMEOUT_SECONDS": "30", "AUDIT_URL": "https://audit.example", "AUDIT_TASK_ACCOUNT": "audit-tasks@p.iam.gserviceaccount.com", "AUDIT_LEGACY_SOURCE_URL": "https://legacy.example", "AUDIT_IDENTITY_SOURCE_URL": "https://identity.example"} {
				t.Setenv(name, value)
			}
			pool, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			db, err := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
			if err != nil {
				t.Fatal(err)
			}
			opened, closed, directoryClosed, flushed, served := 0, 0, 0, 0, 0
			openDatabase = func(_ context.Context, cfg config.Config) (*gorm.DB, func(), error) {
				opened++
				if cfg.RuntimeRole != "audit" {
					t.Error("wrong owner role")
				}
				if scenario == "database-error" {
					return nil, nil, errors.New("database unavailable")
				}
				return db, func() { closed++ }, nil
			}
			configureDirectory = func(context.Context) (identityclient.Directory, func(), error) {
				if scenario == "directory-error" {
					return nil, nil, errors.New("directory unavailable")
				}
				close := func() { directoryClosed++ }
				if scenario == "directory-missing" {
					return nil, close, nil
				}
				return directoryStub{}, close, nil
			}
			openSource = func(_ context.Context, address string) (audit.Source, error) {
				if scenario == "source-error" {
					return nil, errors.New("source unavailable")
				}
				if address != "https://legacy.example" && address != "https://identity.example" {
					t.Error("unexpected owner")
				}
				return sourceStub{}, nil
			}
			startTracing = func(context.Context) (func(context.Context) error, error) {
				if scenario == "trace-error" {
					return nil, errors.New("tracing unavailable")
				}
				return func(context.Context) error { flushed++; return nil }, nil
			}
			serveRuntime = func(_ context.Context, handler http.Handler, _ string) error {
				served++
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
				if w.Code != 200 {
					t.Error("unhealthy assembled runtime")
				}
				if scenario == "serve-error" {
					return errors.New("listen failed")
				}
				return nil
			}
			switch scenario {
			case "unsupported":
				os.Args = append(os.Args, "signup-cleanup")
			case "policy-error":
				t.Setenv("DB_MAX_OPEN", "2")
			case "observability-error":
				t.Setenv("REQUEST_TIMEOUT_SECONDS", "bad")
			case "handler-error":
				t.Setenv("AUDIT_URL", "bad")
			}
			err = run()
			if (err == nil) != (scenario == "success") {
				t.Fatalf("unexpected bootstrap result: %v", err)
			}
			if opened > 0 && scenario != "database-error" && closed != 1 {
				t.Fatal("database leaked")
			}
			if scenario == "success" && (flushed != 1 || served != 1 || directoryClosed != 1) {
				t.Fatal("runtime lifecycle incomplete")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
