package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"cloud.google.com/go/storage"
	"nordik-drive-api/config"
	"nordik-drive-api/internal/database"
	"nordik-drive-api/internal/mailer"
	"nordik-drive-api/internal/notificationcontract"
	"nordik-drive-api/internal/notifications"
	"nordik-drive-api/internal/observability"
	"nordik-drive-api/internal/workload"
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
	if cfg.DBSchema != "notifications" || cfg.DBMaxOpen != 1 || cfg.DBAuthMode != "iam" {
		return errors.New("Notifications requires IAM, DB_SCHEMA=notifications, DB_MAX_OPEN=1")
	}
	if cfg.GmailUser == "" || cfg.GmailPass == "" {
		return errors.New("notification SMTP configuration is required")
	}
	codec, err := notificationcontract.NewCodec(os.Getenv("NOTIFICATION_PAYLOAD_KEYS"), os.Getenv("NOTIFICATION_PAYLOAD_KEY_VERSION"))
	if err != nil {
		return errors.New("invalid notification payload key configuration")
	}
	address, legacy, worker, task := os.Getenv("NOTIFICATIONS_URL"), os.Getenv("NOTIFICATIONS_LEGACY_ACCOUNT"), os.Getenv("NOTIFICATIONS_WORKER_ACCOUNT"), os.Getenv("NOTIFICATIONS_TASK_ACCOUNT")
	if legacy == "" || worker == "" || task == "" || task == legacy || task == worker {
		return errors.New("distinct notification caller identities are required")
	}
	queue, err := notifications.NewCloudQueue(ctx, os.Getenv("NOTIFICATIONS_OTP_QUEUE"), os.Getenv("NOTIFICATIONS_NORMAL_QUEUE"), address, task)
	if err != nil {
		return err
	}
	db, closeDB, err := database.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer closeDB()
	objects, err := storage.NewClient(ctx)
	if err != nil {
		return errors.New("notification object client unavailable")
	}
	defer objects.Close()
	service := &notifications.Service{Store: &notifications.Store{DB: db}, Codec: codec, Queue: queue, Provider: &notifications.SMTPProvider{Mail: mailer.NewService(cfg.GmailUser, cfg.GmailPass, "smtp.gmail.com", "587"), Storage: objects}}
	sources := notificationcontract.OwnerSources{}
	for _, address := range []string{os.Getenv("NOTIFICATION_OUTBOX_IDENTITY_URL"), os.Getenv("NOTIFICATION_OUTBOX_SOURCE_URL")} {
		if address == "" {
			continue
		}
		tokens, err := workload.Source(ctx, address)
		if err != nil {
			return err
		}
		source, sourceErr := notificationcontract.NewSourceClient(address, tokens)
		err = sourceErr
		if err != nil {
			return err
		}
		sources = append(sources, source)
	}
	if len(sources) > 0 {
		service.Source = sources
	}
	grants := map[string]map[string]bool{"producer:identity": {legacy: true, worker: true}, "producer:records": {legacy: true, worker: true}, "producer:support": {legacy: true, worker: true}, "delivery": {task: true}, "recovery": {worker: true}}
	if err := configureIdentityPolicy(grants, os.Getenv("NOTIFICATIONS_IDENTITY_ACCOUNT"), os.Getenv("NOTIFICATIONS_IDENTITY_EXCLUSIVE")); err != nil {
		return err
	}

	grants["operator"] = map[string]bool{}
	for _, operator := range strings.Split(os.Getenv("NOTIFICATIONS_OPERATOR_ACCOUNTS"), ",") {
		operator = strings.TrimSpace(operator)
		if operator != "" {
			grants["operator"][operator] = true
			grants["recovery"][operator] = true
		}
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	server := &http.Server{Addr: net.JoinHostPort("0.0.0.0", port), Handler: notifications.Handler(service, notifications.WorkloadAuthorizer(address, grants)), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 40 * time.Second, IdleTimeout: 60 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
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
