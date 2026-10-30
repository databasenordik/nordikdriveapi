package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log"
	"os"
	"time"

	"nordik-drive-api/config"

	"google.golang.org/genai"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type runtimeDependencies struct {
	DB     *gorm.DB
	Pool   *sql.DB
	Vertex *genai.Client
}

// initializeRuntime uses small constructor seams so failure paths can be tested
// without database or Google credentials. Diagnostics deliberately omit causes
// that may contain connection strings, API keys, or credential file contents.
func initializeRuntime(ctx context.Context, cfg config.Config,
	openDB func(config.Config) (*gorm.DB, error),
	newVertex func(context.Context, *genai.ClientConfig) (*genai.Client, error),
) (*runtimeDependencies, error) {
	if err := cfg.ValidateServer(); err != nil {
		return nil, err
	}
	db, err := openDB(cfg)
	if err != nil {
		return nil, errors.New("database initialization failed; check database configuration and connectivity")
	}
	pool, err := db.DB()
	if err != nil {
		return nil, errors.New("database connection pool initialization failed")
	}
	pool.SetMaxOpenConns(25)
	pool.SetMaxIdleConns(10)
	pool.SetConnMaxIdleTime(5 * time.Minute)
	pool.SetConnMaxLifetime(30 * time.Minute)
	client, err := newVertex(ctx, &genai.ClientConfig{
		Backend: genai.BackendVertexAI, Project: "planar-ray-472112-e8", Location: "global",
	})
	if err != nil || client == nil {
		_ = pool.Close()
		return nil, errors.New("Vertex AI client initialization failed; check Application Default Credentials, project, and location")
	}
	return &runtimeDependencies{DB: db, Pool: pool, Vertex: client}, nil
}

func openDatabase(cfg config.Config) (*gorm.DB, error) {
	dsn := "host=" + cfg.DBHost + " user=" + cfg.DBUser + " password=" + cfg.DBPassword +
		" dbname=" + cfg.DBName + " port=" + cfg.DBPort + " sslmode=disable"
	// GORM's default initialization logger can print the driver's raw error.
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		PrepareStmt: true, Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		if db != nil {
			if pool, poolErr := db.DB(); poolErr == nil {
				_ = pool.Close()
			}
		}
		return nil, err
	}
	db.Logger = databaseLogger(os.Stderr)
	return db, nil
}

func databaseLogger(output io.Writer) logger.Interface {
	return logger.New(log.New(output, "", log.LstdFlags), logger.Config{
		SlowThreshold: 200 * time.Millisecond, LogLevel: logger.Warn,
		ParameterizedQueries: true,
	})
}
