package audittransport

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/oauth2"
	"nordik-drive-api/internal/auditoutbox"
)

type tokenFailure struct{}

func (tokenFailure) Token() (*oauth2.Token, error) { return nil, errors.New("SECRET") }
func TestPrivateOwnerContractAndRedactedErrors(t *testing.T) {
	status := 200
	payload := `{"receipt":{"event_id":"event","fingerprint":"digest","log_id":1}}`
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer signed-google-token" || r.Header.Get("X-Serverless-Authorization") != "Bearer signed-google-token" {
			t.Error("original signed token not preserved")
		}
		if r.Header.Get("Content-Type") != "application/json" || r.Method != "POST" {
			t.Error("invalid private method")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(payload))
	}))
	defer server.Close()
	c := &Client{URL: server.URL, HTTP: server.Client(), Tokens: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "signed-google-token"})}
	record, err := c.Read(context.Background(), "event")
	if err != nil || record.Receipt.LogID != 1 {
		t.Fatal(record, err)
	}
	if err := c.Ack(context.Background(), auditoutbox.Receipt{}); err != nil {
		t.Fatal(err)
	}
	if err := c.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	status = 404
	if _, err := c.Read(context.Background(), "event"); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing source not distinguished", err)
	}
	status = 403
	payload = "SECRET"
	if err := c.Dispatch(context.Background()); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatal("remote error exposed or hidden", err)
	}
	status = 200
	for _, bad := range []string{`{`, `{"unknown":1}`, `{} {}`} {
		payload = bad
		if _, err := c.Read(context.Background(), "event"); err == nil {
			t.Fatal("invalid source response accepted")
		}
	}
	before := calls
	c.Tokens = tokenFailure{}
	if c.Dispatch(context.Background()) == nil || calls != before {
		t.Fatal("invalid credentials sent")
	}
	c.Tokens = nil
	if c.Dispatch(context.Background()) == nil {
		t.Fatal("absent token accepted")
	}
	c.Tokens = oauth2.StaticTokenSource(&oauth2.Token{})
	if c.Dispatch(context.Background()) == nil {
		t.Fatal("empty token accepted")
	}
	c.Tokens = oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "signed-google-token"})
	ctx, stop := context.WithCancel(context.Background())
	stop()
	if c.Dispatch(ctx) == nil {
		t.Fatal("cancelled owner call succeeded")
	}
	if c.call(context.Background(), "/path", make(chan bool), nil) == nil {
		t.Fatal("unencodable request accepted")
	}
	c.URL = ":invalid"
	if c.Dispatch(context.Background()) == nil {
		t.Fatal("invalid owner URL accepted")
	}
}
func TestOwnerConfigurationRequiresExactAudienceAndADC(t *testing.T) {
	for _, address := range []string{"bad", "http://owner.example", "https://owner.example/path", "https://user@owner.example", "https://owner.example?q=1", "https://owner.example#fragment"} {
		if _, err := New(context.Background(), address); err == nil {
			t.Fatal("invalid owner audience accepted", address)
		}
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "nonexistent-m4-test-credentials.json")
	if _, err := New(context.Background(), "https://owner.example"); err == nil {
		t.Fatal("missing credentials accepted")
	}
}
