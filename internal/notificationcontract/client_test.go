package notificationcontract

import (
	"context"
	"encoding/json"
	"errors"
	"golang.org/x/oauth2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type tokenFailure struct{}

func (tokenFailure) Token() (*oauth2.Token, error) {
	return nil, errors.New("synthetic credential failure")
}
func TestPrivateClientAuthenticationAndResponseContracts(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic" || r.Header.Get("X-Serverless-Authorization") != "Bearer synthetic" || r.Header.Get("Cookie") != "" {
			t.Error("workload identity headers incorrect")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/internal/v1/notifications":
			_ = json.NewEncoder(w).Encode(map[string]string{"delivery_id": "receipt"})
		case "/internal/v1/status":
			_, _ = w.Write([]byte(`{"receipt":"provider_accepted"}`))
		case "/internal/v1/recover":
			w.WriteHeader(204)
		default:
			http.Error(w, "private provider detail", 500)
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "synthetic"}))
	if err != nil {
		t.Fatal(err)
	}
	client.HTTP = server.Client()
	ctx := context.Background()
	if id, err := client.Accept(ctx, testCommand()); err != nil || id != "receipt" {
		t.Fatal("intake response contract", err)
	}
	if states, err := client.Status(ctx, []string{"receipt"}); err != nil || states["receipt"] != "provider_accepted" {
		t.Fatal("status response contract", err)
	}
	if err := client.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.call(ctx, "/failure", struct{}{}, nil); err == nil || strings.Contains(err.Error(), "private provider detail") {
		t.Fatal("provider response leaked")
	}
	client.Tokens = tokenFailure{}
	if err := client.Recover(ctx); err == nil {
		t.Fatal("credential failure ignored")
	}
	for _, url := range []string{"http://example.test", "https://user@example.test", "https://example.test/path", "https://example.test?query=1", "https://example.test#fragment", "bad"} {
		if _, err := NewClient(url, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "synthetic"})); err == nil {
			t.Fatal("invalid endpoint accepted")
		}
	}
	if _, err := NewClient(server.URL, nil); err == nil {
		t.Fatal("nil credential source accepted")
	}
}
func TestPrivateClientRejectsRedirectsMalformedResponsesAndCancellation(t *testing.T) {
	mode := "redirect"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "redirect":
			http.Redirect(w, r, "https://untrusted.example.test", 302)
		case "bad-json":
			_, _ = w.Write([]byte("not-json"))
		case "missing-id":
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "synthetic", Expiry: time.Now().Add(time.Hour)}))
	if err != nil {
		t.Fatal(err)
	}
	client.HTTP.Transport = server.Client().Transport
	if err := client.Recover(context.Background()); err == nil {
		t.Fatal("redirect followed")
	}
	mode = "bad-json"
	if _, err := client.Accept(context.Background(), testCommand()); err == nil {
		t.Fatal("invalid response accepted")
	}
	mode = "missing-id"
	if _, err := client.Accept(context.Background(), testCommand()); err == nil {
		t.Fatal("empty receipt accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.Recover(ctx); err == nil {
		t.Fatal("cancelled request performed")
	}
	if err := client.call(context.Background(), "/recover", make(chan int), nil); err == nil {
		t.Fatal("unserializable payload accepted")
	}
}
