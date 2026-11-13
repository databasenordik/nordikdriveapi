// Package auditapp serves activity queries and private idempotent ingestion.
package auditapp

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"nordik-drive-api/config"
	"nordik-drive-api/internal/audit"
	"nordik-drive-api/internal/auditoutbox"
	"nordik-drive-api/internal/identityclient"
	"nordik-drive-api/internal/logs"
	"nordik-drive-api/internal/observability"
)

func ValidateRuntime(cfg config.Config) error {
	if cfg.DBAuthMode != "iam" || cfg.DBMaxOpen != 1 || (cfg.DBSchema != "audit,identity" && cfg.DBSchema != "audit,public,identity") || cfg.JWTSecret == "" {
		return errors.New("Audit requires IAM, one connection, its validated search path and the existing JWT secret")
	}
	return nil
}
func New(db *gorm.DB, pool *sql.DB, directory identityclient.Directory, service *audit.Service, audience, taskAccount string) (http.Handler, error) {
	u, err := url.Parse(audience)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("Audit requires an exact HTTPS audience")
	}
	if db == nil || pool == nil || directory == nil || service == nil || len(service.Sources) != 2 {
		return nil, errors.New("Audit repository, Identity directory and both producer sources are required")
	}
	owners := map[string]bool{}
	for _, owner := range service.Sources {
		if owner.Source == nil || owners[owner.Producer] || (owner.Producer != "identity" && owner.Producer != "legacy") {
			return nil, errors.New("Audit requires distinct Legacy and Identity owner sources")
		}
		owners[owner.Producer] = true
	}
	authorize, err := auditoutbox.GoogleAuthorize(audience, taskAccount)
	if err != nil {
		return nil, err
	}
	return router(db, pool, directory, service, authorize), nil
}
func router(db *gorm.DB, pool *sql.DB, directory identityclient.Directory, service *audit.Service, authorize auditoutbox.Authorize) http.Handler {
	r := gin.New()
	r.Use(observability.Middleware())
	r.Use(cors.New(cors.Config{AllowOrigins: config.AllowedOrigins(), AllowMethods: []string{"POST", "OPTIONS"}, AllowHeaders: []string{"Origin", "Content-Type", "Authorization"}, AllowCredentials: true, ExposeHeaders: []string{"X-Request-ID", "Retry-After"}}))
	r.GET("/healthz", func(c *gin.Context) { c.Status(200) })
	logs.RegisterRoutes(r, &logs.LogService{DB: db, Directory: directory})
	r.POST("/internal/v1/audit/ingest", gin.WrapH(ingestHandler(service, authorize)))
	observability.RegisterControls(r, pool)
	return r
}
func ingestHandler(service *audit.Service, authorize auditoutbox.Authorize) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		if authorize(r.Context(), strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")) != nil {
			http.Error(w, "forbidden", 403)
			return
		}
		var input struct {
			ID string `json:"event_id"`
		}
		if auditoutbox.Decode(w, r, &input) != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		ctx, stop := context.WithTimeout(r.Context(), 30*time.Second)
		defer stop()
		if service.Ingest(ctx, input.ID) != nil {
			w.Header().Set("Retry-After", "30")
			http.Error(w, "audit ingestion incomplete", 503)
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
	end, stop := context.WithTimeout(context.Background(), 8*time.Second)
	defer stop()
	return server.Shutdown(end)
}
