package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log"
	"time"

	"google.golang.org/genai"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"nordik-drive-api/config"
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
	maxOpen := cfg.DBMaxOpen
	if maxOpen <= 0 {
		maxOpen = 25
	}
	pool.SetMaxOpenConns(maxOpen)
	pool.SetMaxIdleConns(min(10, maxOpen))
	pool.SetConnMaxIdleTime(5 * time.Minute)
	pool.SetConnMaxLifetime(30 * time.Minute)
	client, err := newVertex(ctx, &genai.ClientConfig{
		Backend: genai.BackendVertexAI, Project: cfg.VertexProject, Location: cfg.VertexLocation,
	})
	if err != nil || client == nil {
		_ = pool.Close()
		return nil, errors.New("Vertex AI client initialization failed; check Application Default Credentials, project, and location")
	}
	return &runtimeDependencies{DB: db, Pool: pool, Vertex: client}, nil
}

func databaseLogger(output io.Writer) logger.Interface {
	return logger.New(log.New(output, "", log.LstdFlags), logger.Config{
		SlowThreshold: 200 * time.Millisecond, LogLevel: logger.Warn,
		ParameterizedQueries: true,
	})
}
