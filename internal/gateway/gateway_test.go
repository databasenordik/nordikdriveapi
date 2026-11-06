package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	bootstrapv3 "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	_ "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_authz/v3"
	_ "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	_ "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	_ "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	_ "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/oauth2"
	"google.golang.org/protobuf/encoding/protojson"
)

type fakeLimiter struct {
	decision        Decision
	err             error
	calls           int
	policy, subject string
	limit           int
}

func (l *fakeLimiter) Check(_ context.Context, policy, subject string, limit int) (Decision, error) {
	l.calls++
	l.policy, l.subject, l.limit = policy, subject, limit
	return l.decision, l.err
}
func testConfig() Config {
	backends := map[string]Backend{}
	for _, name := range ServiceNames {
		backends[name] = Backend{URL: "https://" + name + ".example", Audience: "https://" + name + ".example"}
	}
	return Config{Backends: backends, JWTSecret: "synthetic", Mode: "enforce", ClientIPMode: "peer", AuthLimit: 20, PublicLimit: 120, UserLimit: 120, ChatLimit: 10, Local: true}
}
func cookie(t *testing.T) string {
	t.Helper()
	encoded, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"user_id": 7, "exp": time.Now().Add(time.Hour).Unix()}).SignedString([]byte("synthetic"))
	if err != nil {
		t.Fatal(err)
	}
	return "access_token=" + encoded
}
func checkRequest(method, path, cookie string) *authv3.CheckRequest {
	return &authv3.CheckRequest{Attributes: &authv3.AttributeContext{Source: &authv3.AttributeContext_Peer{Address: &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{Address: "192.0.2.10"}}}}, Request: &authv3.AttributeContext_Request{Http: &authv3.AttributeContext_HttpRequest{Method: method, Path: path, Headers: map[string]string{"cookie": cookie, "x-request-id": "synthetic-request-id"}}}}}
}

func TestServiceBoundariesAndCompatibility(t *testing.T) {
	data, err := os.ReadFile("../../tests/fixtures/public_routes.json")
	if err != nil {
		t.Fatal(err)
	}
	var routes []struct{ Method, Path string }
	if json.Unmarshal(data, &routes) != nil || len(routes) != 95 {
		t.Fatal("public route fixture changed unexpectedly")
	}
	for _, route := range routes {
		if _, ok := Match(route.Method, route.Path); !ok {
			t.Errorf("existing route lost: %s %s", route.Method, route.Path)
		}
	}
	for _, boundary := range Boundaries {
		route, ok := Match("PATCH", boundary.Prefix+"/future/endpoint/123")
		if !ok || route.Service != boundary.Service {
			t.Fatal("new endpoints must be delegated to their service router")
		}
	}
	for _, path := range []string{"/internal/jobs/x/run", "/api/internal/jobs/honour/run", "/api/fileevil", "/api/file/../internal", "/api/file//x", "/api/file/\\x", "/api/file/\x00"} {
		if _, ok := Match("GET", path); ok {
			t.Errorf("admitted unowned path %q", path)
		}
	}
}

func TestAuthorizationPolicies(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, token, policy string
		code                              int
	}{
		{"protected missing", "GET", "/api/file", "", "", 401}, {"protected invalid", "GET", "/api/file", "access_token=forged", "", 401},
		{"file", "GET", "/api/file/new/123", cookie(t), "user", 0}, {"chat", "POST", "/api/chat", cookie(t), "chat", 0},
		{"public lookup", "GET", "/api/lookup/new", "", "public", 0}, {"public identity", "POST", "/api/user/login", "access_token=stale", "auth", 0},
		{"refresh self validates", "POST", "/api/user/refresh", "refresh_token=fixture", "auth", 0}, {"me self validates", "GET", "/api/user/me", "", "auth", 0},
		{"preflight", "OPTIONS", "/api/file/new", "", "", 0}, {"private", "POST", "/api/internal/jobs/honour/run", "", "", 404},
		{"bad path", "GET", "%", "", "", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := &fakeLimiter{decision: Decision{Allowed: true}}
			a := NewAuthorizer(testConfig(), l, nil)
			response, err := a.Check(context.Background(), checkRequest(tc.method, tc.path, tc.token))
			if err != nil {
				t.Fatal(err)
			}
			actual := int(response.GetDeniedResponse().GetStatus().GetCode())
			if actual != tc.code {
				t.Fatalf("status %d want %d: %s", actual, tc.code, response.GetDeniedResponse().GetBody())
			}
			if tc.policy != "" && (l.policy != tc.policy || len(l.subject) != 64) {
				t.Fatal("incorrect rate-limit identity")
			}
			if tc.method == "OPTIONS" && l.calls != 0 {
				t.Fatal("preflight must not query limiter")
			}
		})
	}
}

func TestLimiterModesAndFailures(t *testing.T) {
	for _, mode := range []string{"observe", "enforce"} {
		for _, unavailable := range []bool{false, true} {
			t.Run(mode+string(rune('0'+boolInt(unavailable))), func(t *testing.T) {
				l := &fakeLimiter{decision: Decision{Allowed: false, RetryAfter: 17}}
				if unavailable {
					l.err = errors.New("sensitive-driver-error")
				}
				cfg := testConfig()
				cfg.Mode = mode
				a := NewAuthorizer(cfg, l, nil)
				r, _ := a.Check(context.Background(), checkRequest("GET", "/api/lookup/province", ""))
				want := 0
				if mode == "enforce" {
					want = 429
					if unavailable {
						want = 503
					}
				}
				if int(r.GetDeniedResponse().GetStatus().GetCode()) != want {
					t.Fatal("wrong failure behavior")
				}
				if strings.Contains(r.GetDeniedResponse().GetBody(), "sensitive") {
					t.Fatal("driver diagnostic leaked")
				}
				if want == 429 && r.GetDeniedResponse().Headers[1].Header.Value != "17" {
					t.Fatal("missing retry metadata")
				}
			})
		}
	}
}
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

type failingSource struct{}

func (failingSource) Token() (*oauth2.Token, error) { return nil, errors.New("credential-canary") }
func TestBackendIdentityIsSelectedByService(t *testing.T) {
	cfg := testConfig()
	cfg.Local = false
	tokens := map[string]oauth2.TokenSource{}
	for _, name := range ServiceNames {
		tokens[name] = oauth2.StaticTokenSource(&oauth2.Token{AccessToken: name + "-identity", Expiry: time.Now().Add(time.Hour)})
	}
	a := NewAuthorizer(cfg, &fakeLimiter{decision: Decision{Allowed: true}}, tokens)
	for _, boundary := range Boundaries {
		req := checkRequest("GET", boundary.Prefix, cookie(t))
		req.Attributes.Request.Http.Headers["x-serverless-authorization"] = "Bearer forged"
		r, _ := a.Check(context.Background(), req)
		if r.GetOkResponse() == nil || r.GetOkResponse().Headers[0].Header.Value != "Bearer "+boundary.Service+"-identity" {
			t.Fatalf("wrong backend token for %s", boundary.Prefix)
		}
		if !contains(r.GetOkResponse().HeadersToRemove, "x-user-id") {
			t.Fatal("caller identity headers not stripped")
		}
	}
	for _, source := range []oauth2.TokenSource{nil, failingSource{}, oauth2.StaticTokenSource(&oauth2.Token{})} {
		a.Tokens["records"] = source
		r, _ := a.Check(context.Background(), checkRequest("GET", "/api/file", cookie(t)))
		if r.GetDeniedResponse().GetStatus().GetCode() != 503 || strings.Contains(r.GetDeniedResponse().Body, "canary") {
			t.Fatal("workload failure must fail closed")
		}
	}
}
func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

func TestTrustedProxyAndBurstBounds(t *testing.T) {
	cfg := testConfig()
	cfg.ClientIPMode = "forwarded"
	cfg.TrustedHops = 2
	a := NewAuthorizer(cfg, &fakeLimiter{decision: Decision{Allowed: true}}, nil)
	ip, err := a.clientIP(map[string]string{"x-forwarded-for": "forged, 192.0.2.4, 192.0.2.5"}, "")
	if err != nil || ip != "192.0.2.4" {
		t.Fatal("did not select trusted suffix")
	}
	for _, headers := range []map[string]string{{}, {"x-forwarded-for": "127.0.0.1"}, {"x-forwarded-for": "bad, 127.0.0.1"}} {
		if _, err := a.clientIP(headers, ""); err == nil {
			t.Fatal("invalid proxy chain accepted")
		}
	}
	for i := 0; i < 100; i++ {
		if !a.burst("one") {
			t.Fatal("initial burst unexpectedly rejected")
		}
	}
	if a.burst("one") {
		t.Fatal("unbounded burst")
	}
	a.bursts = map[string]burstEntry{}
	for i := 0; i < 10000; i++ {
		a.bursts[string(rune(i))] = burstEntry{used: time.Now()}
	}
	if a.burst("overflow") {
		t.Fatal("unbounded cardinality")
	}
	for key, entry := range a.bursts {
		entry.used = time.Now().Add(-3 * time.Minute)
		a.bursts[key] = entry
	}
	if !a.burst("fresh") {
		t.Fatal("expired guard entries not reclaimed")
	}
	request := checkRequest("GET", "/api/lookup/province", "")
	r, _ := a.Check(context.Background(), request)
	if r.GetDeniedResponse().GetStatus().GetCode() != 400 {
		t.Fatal("missing platform address accepted")
	}
}

func TestBootstrapRoutesServicesAndValidates(t *testing.T) {
	data, err := Bootstrap(testConfig().Backends)
	if err != nil {
		t.Fatal(err)
	}
	var config bootstrapv3.Bootstrap
	if err := protojson.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if err := config.ValidateAll(); err != nil {
		t.Fatal(err)
	}
	if len(config.StaticResources.Clusters) != 6 {
		t.Fatal("missing independent upstreams")
	}
	if strings.Contains(string(data), "with_request_body") || strings.Contains(string(data), "retry_policy") {
		t.Fatal("must stream uploads and avoid replaying writes")
	}
	if !strings.Contains(string(data), "path_separated_prefix") || !strings.Contains(string(data), "match_typed_subject_alt_names") {
		t.Fatal("missing service boundary or TLS hostname verification")
	}
}

func TestLoadConfigValidation(t *testing.T) {
	t.Setenv("GATEWAY_LOCAL", "true")
	t.Setenv("LEGACY_BACKEND_URL", "http://localhost:8080")
	t.Setenv("JWT_SECRET", "synthetic")
	if cfg, err := LoadConfig(); err != nil || cfg.Mode != "observe" || len(cfg.Backends) != 5 {
		t.Fatalf("defaults: %v", err)
	}
	for name, value := range map[string]string{"SERVICE_BACKENDS_JSON": "{", "RATE_LIMIT_MODE": "allow", "GATEWAY_LOCAL": "nope", "RATE_CHAT_PER_MINUTE": "0", "CLIENT_IP_MODE": "anything", "LEGACY_BACKEND_URL": "http://user:password@localhost"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, value)
			if _, err := LoadConfig(); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	t.Setenv("SERVICE_BACKENDS_JSON", `{"records":{"url":"http://localhost:9090","audience":""}}`)
	cfg, err := LoadConfig()
	if err != nil || cfg.Backends["records"].URL != "http://localhost:9090" || cfg.Backends["identity"].URL != "http://localhost:8080" {
		t.Fatal("backend overrides lost")
	}
}
