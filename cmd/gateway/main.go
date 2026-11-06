package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"golang.org/x/oauth2"
	"google.golang.org/grpc"

	"nordik-drive-api/config"
	"nordik-drive-api/internal/database"
	"nordik-drive-api/internal/gateway"
	"nordik-drive-api/internal/observability"
	"nordik-drive-api/internal/workload"
)

func main() {
	if err := run(); err != nil {
		slog.Error("gateway stopped", "error", err.Error())
		os.Exit(1)
	}
}
func run() error {
	observability.Setup()
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
	dbcfg := config.LoadConfig()
	dbcfg.DBSchema = "gateway"
	if os.Getenv("DB_MAX_OPEN") == "" {
		dbcfg.DBMaxOpen = 4
	}
	db, cleanup, err := database.Open(ctx, dbcfg)
	if err != nil {
		return err
	}
	defer cleanup()
	pool, _ := db.DB()
	if len(os.Args) > 1 && os.Args[1] == "cleanup" {
		jobCtx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		for i := 0; i < 100; i++ {
			n, e := gateway.Cleanup(jobCtx, pool)
			if e != nil {
				return errors.New("counter cleanup failed")
			}
			if n < 1000 {
				return nil
			}
		}
		return nil
	}
	cfg, err := gateway.LoadConfig()
	if err != nil {
		return err
	}
	tokens := map[string]oauth2.TokenSource{}
	if !cfg.Local {
		for name, backend := range cfg.Backends {
			source, e := workload.Source(ctx, backend.Audience)
			if e != nil {
				return e
			}
			tokens[name] = source
		}
	}
	bootstrap, err := gateway.Bootstrap(cfg.Backends)
	if err != nil {
		return err
	}
	if path := os.Getenv("ENVOY_CONFIG_PATH"); path != "" {
		if err := os.WriteFile(path, bootstrap, 0644); err != nil {
			return errors.New("cannot write Envoy bootstrap")
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:9001")
	if err != nil {
		return err
	}
	server := grpc.NewServer(grpc.MaxConcurrentStreams(128), grpc.StatsHandler(otelgrpc.NewServerHandler()))
	authv3.RegisterAuthorizationServer(server, gateway.NewAuthorizer(cfg, gateway.PostgresLimiter{Pool: pool, AcquireBudget: time.Duration(cfg.AcquireMillis) * time.Millisecond, QueryBudget: time.Duration(cfg.QueryMillis) * time.Millisecond}, tokens))
	health := &http.Server{Addr: "0.0.0.0:9002", ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(200)
	})}
	errCh := make(chan error, 2)
	go func() { errCh <- server.Serve(listener) }()
	go func() { errCh <- health.ListenAndServe() }()
	select {
	case <-ctx.Done():
	case err = <-errCh:
	}
	server.Stop()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = health.Shutdown(shutdown)
	return err
}
