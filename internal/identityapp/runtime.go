// Package identityapp assembles Identity without business-file, provider or cron
// initialization. Public auth routes retain their existing handlers and cookies.
package identityapp

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"gorm.io/gorm"
	"nordik-drive-api/config"
	"nordik-drive-api/internal/auditoutbox"
	"nordik-drive-api/internal/auth"
	"nordik-drive-api/internal/community"
	"nordik-drive-api/internal/identity"
	"nordik-drive-api/internal/logs"
	"nordik-drive-api/internal/observability"
	"nordik-drive-api/internal/outbox"
	"nordik-drive-api/internal/role"
	"nordik-drive-api/internal/workload"
)

type Options struct {
	Audience, NotificationReader, WorkerAccount string
	SignupKeys, SignupVersion                   string
	RequireVerified                             bool
	Policy                                      identity.Policy
}

func accounts(raw string) (map[string]bool, error) {
	result := map[string]bool{}
	for _, account := range strings.Split(raw, ",") {
		account = strings.TrimSpace(account)
		if account == "" {
			continue
		}
		if !strings.HasSuffix(account, ".iam.gserviceaccount.com") || strings.ContainsAny(account, " \r\n/") || strings.Count(account, "@") != 1 {
			return nil, errors.New("Identity callers must be explicit Google service accounts")
		}
		result[account] = true
	}
	return result, nil
}

func LoadOptions() (Options, error) {
	options := Options{Audience: os.Getenv("IDENTITY_URL"), NotificationReader: os.Getenv("NOTIFICATION_OUTBOX_READER_ACCOUNT"), WorkerAccount: os.Getenv("IDENTITY_WORKER_ACCOUNT"), SignupKeys: os.Getenv("SIGNUP_VERIFICATION_KEYS"), SignupVersion: os.Getenv("SIGNUP_VERIFICATION_KEY_VERSION"), RequireVerified: true}
	var err error
	if options.Policy.Profiles, err = accounts(os.Getenv("IDENTITY_PROFILE_ACCOUNTS")); err != nil {
		return options, err
	}
	if options.Policy.Principals, err = accounts(os.Getenv("IDENTITY_PRINCIPAL_ACCOUNTS")); err != nil {
		return options, err
	}
	if options.Policy.Email, err = accounts(os.Getenv("IDENTITY_EMAIL_ACCOUNTS")); err != nil {
		return options, err
	}
	if value := os.Getenv("SIGNUP_REQUIRE_VERIFIED_EMAIL"); value != "" {
		options.RequireVerified, err = strconv.ParseBool(value)
		if err != nil {
			return options, errors.New("invalid signup verification setting")
		}
	}
	return options, nil
}

func ValidateRuntime(cfg config.Config, cleanup bool) error {
	if cfg.DBAuthMode != "iam" || cfg.DBMaxOpen != 1 || (cfg.DBSchema != "identity" && cfg.DBSchema != "identity,public") {
		return errors.New("Identity requires IAM, one database connection and its validated owner search path")
	}
	if !cleanup && strings.TrimSpace(cfg.JWTSecret) == "" {
		return errors.New("existing JWT secret is required")
	}
	return nil
}

func New(db *gorm.DB, pool *sql.DB, cfg config.Config, options Options, writer *outbox.Writer, remote outbox.Remote) (http.Handler, func(), error) {
	u, err := url.Parse(options.Audience)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, nil, errors.New("Identity requires an exact HTTPS audience")
	}
	if db == nil || pool == nil || writer == nil || writer.Codec == nil || remote == nil || options.NotificationReader == "" || options.WorkerAccount == "" {
		return nil, nil, errors.New("Identity repository, transactional notifications and private caller identities are required")
	}
	auditWriter, err := auditoutbox.Configure(db.Statement.Context, "identity")
	if err != nil {
		return nil, nil, err
	}
	user := &auth.AuthService{DB: db, CFG: &cfg, Outbox: writer, RequireVerified: options.RequireVerified, Audit: auditWriter}
	if options.SignupKeys != "" {
		user.Signup, err = auth.NewSignupVerifier(db, writer, options.SignupKeys, options.SignupVersion)
		if err != nil {
			return nil, nil, err
		}
		user.Signup.Remote = remote
	}
	if user.RequireVerified && user.Signup == nil {
		return nil, nil, errors.New("mandatory signup verification keys are required")
	}
	grpcServer, err := identity.NewGRPC(&identity.Service{DB: db, Policy: options.Policy}, options.Audience)
	if err != nil {
		return nil, nil, err
	}
	router := gin.New()
	router.Use(observability.Middleware())
	router.Use(cors.New(cors.Config{AllowOrigins: config.AllowedOrigins(), AllowMethods: []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"}, AllowHeaders: []string{"Origin", "Content-Type", "Authorization", "Idempotency-Key"}, AllowCredentials: true, ExposeHeaders: []string{"X-Request-ID", "Retry-After"}}))
	router.Use(func(c *gin.Context) {
		ctx, pending := outbox.Capture(c.Request.Context())
		c.Request = c.Request.WithContext(ctx)
		c.Next()
		outbox.PublishPending(ctx, db, writer.Publisher, pending)
	})
	router.GET("/healthz", func(c *gin.Context) { c.Status(200) })
	if err := auditoutbox.RegisterOwner(router, db, auditWriter, options.Audience); err != nil {
		grpcServer.Stop()
		return nil, nil, err
	}
	auth.RegisterRoutes(router, user, &logs.LogService{DB: db, Audit: auditWriter})
	role.RegisterRoutes(router, &role.RoleService{DB: db})
	community.RegisterRoutes(router, &community.CommunityService{DB: db})
	source, err := outbox.NewSourceHandler(db, options.Audience, options.NotificationReader)
	if err != nil {
		grpcServer.Stop()
		return nil, nil, err
	}
	router.POST("/internal/v1/notification-outbox/read", gin.WrapH(source))
	router.POST("/internal/v1/notification-outbox/receipt", gin.WrapH(source))
	router.POST("/internal/v1/identity/dispatch-notifications", gin.WrapH(dispatchHandler(options.Audience, options.WorkerAccount, func(ctx context.Context) error { return (&outbox.Dispatcher{DB: db, Remote: remote}).Run(ctx) }, workload.Verify)))
	observability.RegisterControls(router, pool)
	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			grpcServer.ServeHTTP(w, r)
			return
		}
		router.ServeHTTP(w, r)
	})
	return h2c.NewHandler(mux, &http2.Server{MaxConcurrentStreams: 32}), grpcServer.Stop, nil
}

func dispatchHandler(audience, worker string, run func(context.Context) error, verify func(context.Context, string, string, map[string]bool) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		check, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		err := verify(check, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), audience, map[string]bool{worker: true})
		cancel()
		if err != nil {
			http.Error(w, "forbidden", 403)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		if run(ctx) != nil {
			http.Error(w, "Identity notification dispatch incomplete", 503)
			return
		}
		w.WriteHeader(204)
	})
}

func Serve(ctx context.Context, handler http.Handler, port string) error {
	if port == "" {
		port = "8080"
	}
	server := &http.Server{Addr: "0.0.0.0:" + port, Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
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
