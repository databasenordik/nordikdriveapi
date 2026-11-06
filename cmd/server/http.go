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
	"nordik-drive-api/internal/chat"
	"nordik-drive-api/internal/honour"
	"nordik-drive-api/internal/jobs"
	"nordik-drive-api/internal/mailer"
	"nordik-drive-api/internal/observability"
	"nordik-drive-api/internal/supportschedule"
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
	prefix := os.Getenv("WORKER_SCHEDULER_PREFIX")
	if prefix == "" {
		return nil, errors.New("WORKER_SCHEDULER_PREFIX is required")
	}
	mail := mailer.NewService(cfg.GmailUser, cfg.GmailPass, "smtp.gmail.com", "587")
	chatService := &chat.ChatService{DB: dependencies.DB, Client: dependencies.Vertex}
	honourService := honour.NewService(dependencies.DB, chatService)
	honourService.Location = loadAppLocation(cfg.AppTimeZone)
	var provider supportschedule.MeetingProvider = supportschedule.DisabledMeetingProvider{}
	if cfg.ZoomEnabled {
		zoom, err := supportschedule.NewZoomMeetingProvider(supportschedule.ZoomConfig{AccountID: cfg.ZoomAccountID, ClientID: cfg.ZoomClientID, ClientSecret: cfg.ZoomClientSecret})
		if err != nil {
			return nil, errors.New("invalid Zoom configuration")
		}
		provider = zoom
	}
	supportService := &supportschedule.Service{DB: dependencies.DB, Mailer: mail, MeetingProvider: provider}
	router := gin.New()
	router.Use(observability.Middleware())
	router.GET("/healthz", func(c *gin.Context) { c.Status(200) })
	jobs.RegisterWorker(router, &jobs.Runner{Pool: dependencies.Pool, Jobs: jobs.Registry(dependencies.DB, mail, honourService, supportService, nil)}, prefix)
	observability.RegisterControls(router, dependencies.Pool)
	return router, nil
}
