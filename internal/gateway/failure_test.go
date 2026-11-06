package gateway

import (
	"context"
	"github.com/DATA-DOG/go-sqlmock"
	"strings"
	"testing"
)

func TestUnsafeDeploymentConfigurationsAreRejected(t *testing.T) {
	for name, change := range map[string]func(*Config){
		"missing backend": func(c *Config) { delete(c.Backends, "records") },
		"plaintext backend": func(c *Config) {
			c.Local = false
			c.Backends["records"] = Backend{URL: "http://records.example", Audience: "https://records.example"}
		},
		"missing audience":          func(c *Config) { c.Local = false; c.Backends["records"] = Backend{URL: "https://records.example"} },
		"missing key":               func(c *Config) { c.JWTSecret = "" },
		"invalid quota":             func(c *Config) { c.UserLimit = 0 },
		"unverified proxy":          func(c *Config) { c.ClientIPMode = "forwarded"; c.TrustedHops = 0 },
		"production peer address":   func(c *Config) { c.Local = false },
		"excessive database budget": func(c *Config) { c.AcquireMillis = 5000 },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig()
			change(&cfg)
			if cfg.Validate() == nil {
				t.Fatal("unsafe deployment configuration accepted")
			}
		})
	}
	cfg := testConfig()
	cfg.Local = false
	cfg.ClientIPMode = "forwarded"
	cfg.TrustedHops = 1
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}
func TestLocalBootstrapAndClosedDatabase(t *testing.T) {
	cfg := testConfig()
	for _, name := range ServiceNames {
		cfg.Backends[name] = Backend{URL: "http://127.0.0.1:8088"}
	}
	data, err := Bootstrap(cfg.Backends)
	if err != nil || !strings.Contains(string(data), "8088") || strings.Contains(string(data), "UpstreamTlsContext") {
		t.Fatal("local fixture bootstrap requires plain HTTP and its explicit port")
	}
	cfg.Backends["records"] = Backend{URL: "%"}
	if _, err := Bootstrap(cfg.Backends); err == nil {
		t.Fatal("malformed upstream URL accepted")
	}
	pool, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	_ = pool.Close()
	if _, err := (PostgresLimiter{Pool: pool}).Check(context.Background(), "fixture", strings.Repeat("a", 64), 1); err == nil {
		t.Fatal("closed authoritative pool silently allowed traffic")
	}
	if _, err := Cleanup(context.Background(), pool); err == nil {
		t.Fatal("failed cleanup silently succeeded")
	}
}
func TestGatewayBackpressureAndCorsOnDenial(t *testing.T) {
	cfg := testConfig()
	limiter := &fakeLimiter{decision: Decision{Allowed: true}}
	a := NewAuthorizer(cfg, limiter, nil)
	for i := 0; i < cap(a.pending); i++ {
		a.pending <- struct{}{}
	}
	request := checkRequest("GET", "/api/lookup/province", "")
	request.Attributes.Request.Http.Headers["origin"] = "http://localhost:3000"
	response, _ := a.Check(context.Background(), request)
	if response.GetDeniedResponse().GetStatus().GetCode() != 503 {
		t.Fatal("unbounded pending database checks")
	}
	var cors bool
	for _, header := range response.GetDeniedResponse().Headers {
		if header.Header.Key == "access-control-allow-origin" && header.Header.Value == "http://localhost:3000" {
			cors = true
		}
	}
	if !cors {
		t.Fatal("browser cannot read gateway failure")
	}
	a = NewAuthorizer(cfg, limiter, nil)
	for i := 0; i < 100; i++ {
		a.burst("192.0.2.10")
	}
	response, _ = a.Check(context.Background(), checkRequest("GET", "/api/lookup/province", ""))
	if response.GetDeniedResponse().GetStatus().GetCode() != 429 {
		t.Fatal("local burst protection did not enforce")
	}
	cfg.Mode = "observe"
	a = NewAuthorizer(cfg, limiter, nil)
	for i := 0; i < 100; i++ {
		a.burst("192.0.2.10")
	}
	response, _ = a.Check(context.Background(), checkRequest("GET", "/api/lookup/province", ""))
	if response.GetOkResponse() == nil {
		t.Fatal("observation mode rejected a would-be local limit")
	}
}
