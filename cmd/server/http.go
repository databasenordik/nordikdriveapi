package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"

	"nordik-drive-api/config"
	"nordik-drive-api/internal/workerapp"
)

func serve(ctx context.Context, handler http.Handler) error {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	server := &http.Server{Addr: "0.0.0.0:" + port, Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	select {
	case err := <-errCh:
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

func workerRouter(cfg config.Config, dependencies *runtimeDependencies) (*gin.Engine, error) {
	return workerapp.Router(context.Background(), cfg, dependencies.DB, dependencies.Pool, dependencies.Vertex)
}
