package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"nordik-drive-api/config"

	"github.com/DATA-DOG/go-sqlmock"
	"google.golang.org/genai"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestInitializeRuntime_RejectsMissingSecretBeforeDependencies(t *testing.T) {
	_, err := initializeRuntime(context.Background(), config.Config{},
		func(config.Config) (*gorm.DB, error) { t.Fatal("database opened before validation"); return nil, nil },
		func(context.Context, *genai.ClientConfig) (*genai.Client, error) {
			t.Fatal("Vertex opened before validation")
			return nil, nil
		},
	)
	if err == nil || !strings.Contains(err.Error(), "JWT_SECRET") {
		t.Fatalf("expected missing configuration error, got %v", err)
	}
}

func TestInitializeRuntime_DatabaseFailureIsSanitized(t *testing.T) {
	_, err := initializeRuntime(context.Background(), config.Config{JWTSecret: "test-key"},
		func(config.Config) (*gorm.DB, error) { return nil, errors.New("password=credential-canary") },
		func(context.Context, *genai.ClientConfig) (*genai.Client, error) {
			t.Fatal("Vertex called after DB failure")
			return nil, nil
		},
	)
	if err == nil || strings.Contains(err.Error(), "credential-canary") {
		t.Fatal("database startup diagnostic is missing or leaks credentials")
	}
}

func TestInitializeRuntime_VertexFailureClosesDatabase(t *testing.T) {
	pool, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectClose()
	_, err = initializeRuntime(context.Background(), config.Config{JWTSecret: "test-key"},
		func(config.Config) (*gorm.DB, error) { return db, nil },
		func(context.Context, *genai.ClientConfig) (*genai.Client, error) {
			return nil, errors.New("token=credential-canary")
		},
	)
	if err == nil || !strings.Contains(err.Error(), "Vertex AI") || strings.Contains(err.Error(), "credential-canary") {
		t.Fatal("Vertex failure was overwritten or credential was exposed")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestInitializeRuntime_Success(t *testing.T) {
	pool, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	vertex := &genai.Client{}
	dependencies, err := initializeRuntime(context.Background(), config.Config{JWTSecret: "test-key"},
		func(config.Config) (*gorm.DB, error) { return db, nil },
		func(_ context.Context, cfg *genai.ClientConfig) (*genai.Client, error) {
			if cfg.Backend != genai.BackendVertexAI {
				t.Fatal("AI backend changed")
			}
			return vertex, nil
		},
	)
	if err != nil || dependencies.DB != db || dependencies.Vertex != vertex || dependencies.Pool.Stats().MaxOpenConnections != 25 {
		t.Fatalf("runtime initialization failed: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDatabaseLogger_DoesNotPrintCredentialParameters(t *testing.T) {
	pool, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var output bytes.Buffer
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{Logger: databaseLogger(&output)})
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec("UPDATE users").WithArgs("password-canary").WillReturnError(errors.New("synthetic database failure"))
	if err := db.Exec("UPDATE users SET password = ?", "password-canary").Error; err == nil {
		t.Fatal("expected failed statement")
	}
	if strings.Contains(output.String(), "password-canary") || !strings.Contains(output.String(), "synthetic database failure") {
		t.Fatal("database log must retain the error without credential parameters")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
