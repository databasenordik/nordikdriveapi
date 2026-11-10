package main

import (
	"context"
	"errors"
	"google.golang.org/genai"
	"log"
	"net"
	"net/http"
	"nordik-drive-api/config"
	"nordik-drive-api/internal/database"
	"nordik-drive-api/internal/observability"
	"nordik-drive-api/internal/workerapp"
	"os"
	"os/signal"
	"syscall"
	"time"
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
	if err := observability.ValidateConfig(); err != nil {
		return err
	}
	flush, err := observability.Tracing(ctx)
	if err != nil {
		return err
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = flush(c)
	}()
	cfg := config.LoadConfig()
	cfg.RuntimeRole = "worker"
	if os.Getenv("SCHEDULER_MODE") != "external" {
		return errors.New("dedicated worker requires SCHEDULER_MODE=external")
	}
	db, closeDB, err := database.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer closeDB()
	pool, err := db.DB()
	if err != nil {
		return err
	}
	vertex, err := genai.NewClient(ctx, &genai.ClientConfig{Backend: genai.BackendVertexAI, Project: cfg.VertexProject, Location: cfg.VertexLocation})
	if err != nil {
		return errors.New("worker Vertex client unavailable")
	}
	router, err := workerapp.Router(ctx, cfg, db, pool, vertex)
	if err != nil {
		return err
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	server := &http.Server{Addr: net.JoinHostPort("0.0.0.0", port), Handler: router, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	finished := make(chan error, 1)
	go func() { finished <- server.ListenAndServe() }()
	select {
	case err := <-finished:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	return server.Shutdown(shutdown)
}
