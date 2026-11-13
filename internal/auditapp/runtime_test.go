package auditapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"nordik-drive-api/config"
	"nordik-drive-api/internal/activity"
	"nordik-drive-api/internal/audit"
	"nordik-drive-api/internal/auditoutbox"
	"nordik-drive-api/internal/identityclient"
)

type directoryStub struct{}

func (directoryStub) Profiles(context.Context, []int64, bool) (map[int64]identityclient.Profile, error) {
	return nil, errors.New("owner unavailable")
}
func (directoryStub) Principals(context.Context, []int64) (map[int64]identityclient.Principal, error) {
	return nil, errors.New("not permitted")
}
func (directoryStub) Search(context.Context, string, string, string) ([]int64, error) {
	return nil, errors.New("owner unavailable")
}

type sourceStub struct {
	record auditoutbox.SourceRecord
	err    error
}

func (s *sourceStub) Read(context.Context, string) (auditoutbox.SourceRecord, error) {
	return s.record, s.err
}
func (s *sourceStub) Ack(context.Context, auditoutbox.Receipt) error { return s.err }
func TestAuditRuntimeBoundaries(t *testing.T) {
	cfg := config.Config{DBAuthMode: "iam", DBMaxOpen: 1, DBSchema: "audit,identity", JWTSecret: "existing-test-jwt"}
	if ValidateRuntime(cfg) != nil {
		t.Fatal("valid runtime rejected")
	}
	cfg.DBSchema = "audit,public,identity"
	if ValidateRuntime(cfg) != nil {
		t.Fatal("compatible search path rejected")
	}
	for _, mutate := range []func(*config.Config){func(c *config.Config) { c.DBAuthMode = "password" }, func(c *config.Config) { c.DBMaxOpen = 2 }, func(c *config.Config) { c.DBSchema = "public" }, func(c *config.Config) { c.JWTSecret = "" }} {
		copy := cfg
		mutate(&copy)
		if ValidateRuntime(copy) == nil {
			t.Fatal("unsafe runtime accepted")
		}
	}
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := db.DB()
	defer pool.Close()
	source := &sourceStub{}
	service := &audit.Service{DB: db, Sources: []audit.OwnerSource{{Producer: "legacy", Source: source}, {Producer: "identity", Source: source}}}
	if _, err := New(db, pool, directoryStub{}, service, "https://audit.example", "task"); err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{"bad", "http://audit.example", "https://user@audit.example", "https://audit.example/path", "https://audit.example?q=1", "https://audit.example#fragment"} {
		if _, err := New(db, pool, directoryStub{}, service, address, "task"); err == nil {
			t.Fatal("invalid audience accepted")
		}
	}
	for _, kind := range []string{"db", "pool", "directory", "service", "sources", "identity"} {
		t.Run(kind, func(t *testing.T) {
			d, p, dir, svc, task := db, pool, identityclient.Directory(directoryStub{}), service, "task"
			switch kind {
			case "db":
				d = nil
			case "pool":
				p = nil
			case "directory":
				dir = nil
			case "service":
				svc = nil
			case "sources":
				svc = &audit.Service{DB: db}
			case "identity":
				task = ""
			}
			if _, err := New(d, p, dir, svc, "https://audit.example", task); err == nil {
				t.Fatal("missing dependency accepted")
			}
		})
	}
}
func TestAuditPrivateIngestionAndPublicCompatibility(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := db.DB()
	defer pool.Close()
	pool.SetMaxOpenConns(1)
	if err := db.AutoMigrate(&activity.Log{}, &audit.Ingestion{}); err != nil {
		t.Fatal(err)
	}
	event := activity.Event{ID: uuid.NewString(), Version: 1, Producer: "identity", Log: activity.Log{Level: "INFO", Service: "auth", Action: "LOGIN", CreatedAt: time.Now().UTC()}}
	owner := &sourceStub{record: auditoutbox.SourceRecord{Event: &event, Receipt: auditoutbox.Receipt{ID: event.ID, Fingerprint: event.Fingerprint()}}}
	service := &audit.Service{DB: db, Sources: []audit.OwnerSource{{Producer: "identity", Source: owner}, {Producer: "legacy", Source: owner}}}
	authorize := func(_ context.Context, token string) error {
		if token != "signed-task" {
			return errors.New("denied")
		}
		return nil
	}
	handler := router(db, pool, directoryStub{}, service, authorize)
	call := func(method, path, body, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if call("GET", "/healthz", "", "").Code != 200 || call("POST", "/api/logs", "{}", "").Code != 401 {
		t.Fatal("public health or existing session protection changed")
	}
	if call("POST", "/internal/v1/audit/ingest", "{}", "browser-token").Code != 403 {
		t.Fatal("browser invoked private ingestion")
	}
	for _, body := range []string{"{", `{"event_id":"x","payload":"not-permitted"}`, `{} {}`} {
		if call("POST", "/internal/v1/audit/ingest", body, "signed-task").Code != 400 {
			t.Fatal("malformed task accepted")
		}
	}
	body := fmt.Sprintf(`{"event_id":%q}`, event.ID)
	if w := call("POST", "/internal/v1/audit/ingest", body, "signed-task"); w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	owner.err = errors.New("owner down")
	if w := call("POST", "/internal/v1/audit/ingest", body, "signed-task"); w.Code != 503 || w.Header().Get("Retry-After") != "30" || strings.Contains(w.Body.String(), "owner down") {
		t.Fatal("unavailable owner not retried safely")
	}
	for _, path := range []string{"/internal/v1/cleanup", "/api/user/login", "/internal/v1/notifications"} {
		if call("POST", path, "{}", "").Code != 404 {
			t.Fatal("foreign ownership route exposed", path)
		}
	}
	raw := ingestHandler(service, authorize)
	r := httptest.NewRequest("GET", "/internal/v1/audit/ingest", nil)
	w := httptest.NewRecorder()
	raw.ServeHTTP(w, r)
	if w.Code != 405 {
		t.Fatal("wrong ingestion method accepted")
	}
}
func TestAuditServeCancellationAndListenErrors(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	if Serve(context.Background(), handler, "invalid-port") == nil {
		t.Fatal("listen error hidden")
	}
	for _, port := range []string{"0", ""} {
		ctx, stop := context.WithCancel(context.Background())
		stop()
		if err := Serve(ctx, handler, port); err != nil {
			t.Fatal(err)
		}
	}
}
