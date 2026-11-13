package auditoutbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"google.golang.org/api/option"
)

func TestTaskContractContainsOnlyIdentifiersAndUsesDistinctOIDCIdentity(t *testing.T) {
	status := 200
	id := uuid.NewString()
	calls := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var input struct {
			Task struct {
				Name        string `json:"name"`
				HttpRequest struct {
					URL, Body string
					OIDCToken struct{ Audience, ServiceAccountEmail string } `json:"oidcToken"`
				} `json:"httpRequest"`
			} `json:"task"`
		}
		if json.NewDecoder(r.Body).Decode(&input) != nil {
			t.Error("invalid Cloud Tasks payload")
		}
		body, _ := base64.StdEncoding.DecodeString(input.Task.HttpRequest.Body)
		if string(body) != `{"event_id":"`+id+`"}` || !strings.HasSuffix(input.Task.Name, id+"-2") || input.Task.HttpRequest.OIDCToken.Audience != "https://audit.example" || input.Task.HttpRequest.OIDCToken.ServiceAccountEmail != "audit-tasks@project.iam.gserviceaccount.com" || input.Task.HttpRequest.URL != "https://audit.example/internal/v1/audit/ingest" {
			t.Error("audit task exposed payload or wrong identity")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer api.Close()
	p, err := NewTaskPublisher(context.Background(), "projects/p/locations/r/queues/audit", "https://audit.example", "audit-tasks@project.iam.gserviceaccount.com", option.WithEndpoint(api.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Enqueue(context.Background(), id, 2); err != nil {
		t.Fatal(err)
	}
	status = 409
	if err := p.Enqueue(context.Background(), id, 2); err != nil {
		t.Fatal("duplicate task rejected", err)
	}
	status = 403
	if p.Enqueue(context.Background(), id, 2) == nil {
		t.Fatal("publication failure hidden")
	}
	if calls != 3 {
		t.Fatal("unexpected task retry count", calls)
	}
}
func TestAuditConfigurationFailsClosed(t *testing.T) {
	for _, address := range []string{"bad", "http://audit.example", "https://audit.example/path", "https://user@audit.example", "https://audit.example?q=1", "https://audit.example#fragment"} {
		if _, err := NewTaskPublisher(context.Background(), "queue", address, "task", option.WithoutAuthentication()); err == nil {
			t.Fatal("bad audience accepted", address)
		}
	}
	for _, values := range [][3]string{{"", "https://audit.example", "task"}, {"queue", "https://audit.example", ""}} {
		if _, err := NewTaskPublisher(context.Background(), values[0], values[1], values[2], option.WithoutAuthentication()); err == nil {
			t.Fatal("incomplete configuration accepted")
		}
	}
	if _, err := NewTaskPublisher(context.Background(), "queue", "https://audit.example", "task", option.WithCredentialsJSON([]byte("invalid"))); err == nil {
		t.Fatal("invalid task credentials accepted")
	}
	t.Setenv("AUDIT_EVENTS_ENABLED", "")
	if w, err := Configure(context.Background(), "legacy"); err != nil || w != nil {
		t.Fatal("unset configuration changed runtime")
	}
	t.Setenv("AUDIT_EVENTS_ENABLED", "false")
	if w, err := Configure(context.Background(), "legacy"); err != nil || w != nil {
		t.Fatal("disabled configuration changed runtime")
	}
	t.Setenv("AUDIT_EVENTS_ENABLED", "invalid")
	if _, err := Configure(context.Background(), "legacy"); err == nil {
		t.Fatal("invalid boolean accepted")
	}
	t.Setenv("AUDIT_EVENTS_ENABLED", "true")
	if _, err := configure(context.Background(), "invalid", option.WithoutAuthentication()); err == nil {
		t.Fatal("invalid producer accepted")
	}
	t.Setenv("AUDIT_URL", "https://audit.example")
	t.Setenv("AUDIT_QUEUE", "queue")
	t.Setenv("AUDIT_TASK_ACCOUNT", "task")
	if w, err := configure(context.Background(), "identity", option.WithoutAuthentication()); err != nil || w.Producer != "identity" {
		t.Fatal("valid audit configuration rejected", err)
	}
	t.Setenv("AUDIT_URL", "bad")
	if _, err := configure(context.Background(), "legacy", option.WithoutAuthentication()); err == nil {
		t.Fatal("invalid publisher accepted")
	}
	for _, test := range []struct {
		audience string
		accounts []string
	}{{"", []string{"task"}}, {"https://audit.example", nil}, {"https://audit.example", []string{""}}} {
		if _, err := GoogleAuthorize(test.audience, test.accounts...); err == nil {
			t.Fatal("incomplete caller policy accepted")
		}
	}
	auth, err := GoogleAuthorize("https://audit.example", "task")
	if err != nil || auth(context.Background(), "invalid") == nil {
		t.Fatal("unsigned workload accepted")
	}
}
func TestOwnerRuntimeCapturesCommittedIntentsAndRejectsMissingPolicies(t *testing.T) {
	db, w, p := fixture(t)
	r := gin.New()
	if err := RegisterOwner(r, db, nil, ""); err != nil {
		t.Fatal("disabled owner changed runtime")
	}
	t.Setenv("AUDIT_READER_ACCOUNT", "")
	t.Setenv("AUDIT_WORKER_ACCOUNT", "")
	if RegisterOwner(r, db, w, "https://owner.example") == nil {
		t.Fatal("missing reader accepted")
	}
	t.Setenv("AUDIT_READER_ACCOUNT", "audit@project.iam.gserviceaccount.com")
	if RegisterOwner(r, db, w, "https://owner.example") == nil {
		t.Fatal("missing worker accepted")
	}
	t.Setenv("AUDIT_WORKER_ACCOUNT", "worker@project.iam.gserviceaccount.com")
	if err := RegisterOwner(r, db, w, "https://owner.example"); err != nil {
		t.Fatal(err)
	}
	r.POST("/mutation", func(c *gin.Context) {
		if err := w.Put(db.WithContext(c.Request.Context()), event(), nil); err != nil {
			c.Status(500)
			return
		}
		c.Status(200)
	})
	if request(r, "/mutation", `{}`, "", "POST").Code != 200 || len(p.ids) != 1 {
		t.Fatal("committed intent not published")
	}
	p.err = context.Canceled
	if request(r, "/mutation", `{}`, "", "POST").Code != 200 {
		t.Fatal("queue outage changed committed response")
	}
	if request(r, "/internal/v1/audit-outbox/read", `{}`, "invalid", "POST").Code != 403 {
		t.Fatal("private source accepted unsigned token")
	}
}
