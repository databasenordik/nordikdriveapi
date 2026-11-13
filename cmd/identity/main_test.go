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
	"nordik-drive-api/internal/notificationcontract"
	"nordik-drive-api/internal/outbox"
)

type remoteStub struct{}

func (remoteStub) Accept(context.Context, notificationcontract.Command) (string, error) {
	return "", nil
}
func (remoteStub) Status(context.Context, []string) (map[string]string, error) { return nil, nil }
func (remoteStub) Recover(context.Context) error                               { return nil }
func TestIdentityBootstrapClosesResourcesAndKeepsCleanupFinite(t *testing.T) {
	savedOpen, savedTrace, savedConfigure, savedServe, args := openDatabase, startTracing, configureNotifications, serveRuntime, os.Args
	defer func() {
		openDatabase, startTracing, configureNotifications, serveRuntime, os.Args = savedOpen, savedTrace, savedConfigure, savedServe, args
	}()
	for _, scenario := range []string{"success", "unsupported", "invalid-db-policy", "database-error", "invalid-observability", "trace-error", "options-error", "outbox-error", "handler-error", "serve-error", "cleanup"} {
		t.Run(scenario, func(t *testing.T) {
			os.Args = []string{"identity"}
			env := map[string]string{"DB_AUTH_MODE": "iam", "DB_MAX_OPEN": "1", "DB_SCHEMA": "identity,public", "JWT_SECRET": "existing-test-jwt", "REQUEST_TIMEOUT_SECONDS": "30", "IDENTITY_URL": "https://identity.example", "IDENTITY_PROFILE_ACCOUNTS": "legacy@p.iam.gserviceaccount.com", "IDENTITY_PRINCIPAL_ACCOUNTS": "legacy@p.iam.gserviceaccount.com", "IDENTITY_EMAIL_ACCOUNTS": "legacy@p.iam.gserviceaccount.com", "IDENTITY_WORKER_ACCOUNT": "worker@p.iam.gserviceaccount.com", "NOTIFICATION_OUTBOX_READER_ACCOUNT": "notifications@p.iam.gserviceaccount.com", "SIGNUP_REQUIRE_VERIFIED_EMAIL": "true", "SIGNUP_VERIFICATION_KEYS": `{"1":"AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="}`, "SIGNUP_VERIFICATION_KEY_VERSION": "1", "AUDIT_EVENTS_ENABLED": "false"}
			for name, value := range env {
				t.Setenv(name, value)
			}
			pool, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			pool.SetMaxOpenConns(1)
			db, err := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
			if err != nil {
				t.Fatal(err)
			}
			opened, closed, flushed, served := 0, 0, 0, 0
			openDatabase = func(_ context.Context, cfg config.Config) (*gorm.DB, func(), error) {
				opened++
				if cfg.RuntimeRole != "identity" {
					t.Error("wrong database owner")
				}
				if scenario == "database-error" {
					return nil, nil, errors.New("database unavailable")
				}
				return db, func() { closed++ }, nil
			}
			startTracing = func(context.Context) (func(context.Context) error, error) {
				if scenario == "trace-error" {
					return nil, errors.New("tracing unavailable")
				}
				return func(context.Context) error { flushed++; return nil }, nil
			}
			configureNotifications = func(context.Context) (*outbox.Writer, *notificationcontract.Client, error) {
				if scenario == "outbox-error" {
					return nil, nil, errors.New("outbox unavailable")
				}
				codec, err := notificationcontract.NewCodec(`{"1":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`, "1")
				return &outbox.Writer{Codec: codec}, &notificationcontract.Client{}, err
			}
			serveRuntime = func(_ context.Context, handler http.Handler, _ string) error {
				served++
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest("GET", "/healthz", nil))
				if response.Code != 200 {
					t.Error("assembled runtime is not healthy")
				}
				if scenario == "serve-error" {
					return errors.New("listen failed")
				}
				return nil
			}
			switch scenario {
			case "unsupported":
				os.Args = append(os.Args, "cleanup-api")
			case "invalid-db-policy":
				t.Setenv("DB_MAX_OPEN", "2")
			case "invalid-observability":
				t.Setenv("REQUEST_TIMEOUT_SECONDS", "bad")
			case "options-error":
				t.Setenv("IDENTITY_PROFILE_ACCOUNTS", "user@example.com")
			case "handler-error":
				t.Setenv("SIGNUP_VERIFICATION_KEYS", "invalid")
			case "cleanup":
				os.Args = append(os.Args, "signup-cleanup")
				t.Setenv("JWT_SECRET", "")
				mock.ExpectQuery("SELECT pg_try_advisory_lock").WithArgs("signup-expiry-cleanup").WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(false))
			}
			err = run()
			success := scenario == "success" || scenario == "cleanup"
			if (err == nil) != success {
				t.Fatalf("unexpected bootstrap result: %v", err)
			}
			if opened > 0 && scenario != "database-error" && closed != 1 {
				t.Fatal("database lifecycle leaked")
			}
			if scenario == "cleanup" && (served != 0 || flushed != 0) {
				t.Fatal("cleanup started a server or tracing runtime")
			}
			if scenario == "success" && (served != 1 || flushed != 1) {
				t.Fatal("successful runtime lifecycle incomplete")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
