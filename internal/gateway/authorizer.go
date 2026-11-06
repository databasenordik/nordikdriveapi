package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"golang.org/x/time/rate"
	"google.golang.org/genproto/googleapis/rpc/status"

	appconfig "nordik-drive-api/config"
	"nordik-drive-api/internal/session"
)

type burstEntry struct {
	limiter *rate.Limiter
	used    time.Time
}
type Authorizer struct {
	authv3.UnimplementedAuthorizationServer
	Config  Config
	Limiter Limiter
	Tokens  map[string]oauth2.TokenSource
	mu      sync.Mutex
	bursts  map[string]burstEntry
	pending chan struct{}
}

func NewAuthorizer(c Config, l Limiter, t map[string]oauth2.TokenSource) *Authorizer {
	return &Authorizer{Config: c, Limiter: l, Tokens: t, bursts: map[string]burstEntry{}, pending: make(chan struct{}, 32)}
}

func (a *Authorizer) Check(ctx context.Context, req *authv3.CheckRequest) (*authv3.CheckResponse, error) {
	h := req.GetAttributes().GetRequest().GetHttp()
	path, err := url.ParseRequestURI(h.GetPath())
	if err != nil {
		return denyRequest(req, 400, "invalid request path", "INVALID_PATH", 0), nil
	}
	route, ok := Match(h.GetMethod(), path.Path)
	if !ok {
		return denyRequest(req, 404, "not found", "NOT_FOUND", 0), nil
	}
	preflight := h.GetMethod() == http.MethodOptions
	headers := h.GetHeaders()
	policy, subject, limit := "public", "", a.Config.PublicLimit
	if !preflight {
		request := http.Request{Header: http.Header{"Cookie": []string{headers["cookie"]}}}
		cookie, e := request.Cookie("access_token")
		if e != nil && route.Auth == "access" {
			return denyRequest(req, 401, "Missing access token", "UNAUTHORIZED", 0), nil
		}
		encoded := ""
		if cookie != nil {
			encoded = cookie.Value
		}
		claims, e := session.Validate(encoded, a.Config.JWTSecret)
		if e != nil && route.Auth == "access" {
			return denyRequest(req, 401, "Invalid or expired token", "UNAUTHORIZED", 0), nil
		}
		if e == nil {
			policy, subject, limit = "user", strconv.Itoa(claims.UserID), a.Config.UserLimit
		}
		if route.Group == "chat" && e == nil {
			policy, limit = "chat", a.Config.ChatLimit
		}
	}
	if !preflight {
		ip, e := a.clientIP(h.GetHeaders(), req.GetAttributes().GetSource().GetAddress().GetSocketAddress().GetAddress())
		if e != nil {
			return denyRequest(req, 400, "client address unavailable", "INVALID_CLIENT_ADDRESS", 0), nil
		}
		if subject == "" || route.Group == "auth" {
			subject = ip
			if route.Group == "auth" {
				policy, limit = "auth", a.Config.AuthLimit
			}
		}
		if !a.burst(ip) {
			slog.WarnContext(ctx, "rate_limit_burst", "mode", a.Config.Mode)
			if a.Config.Mode == "enforce" {
				return denyRequest(req, 429, "Too many requests", "RATE_LIMITED", 1), nil
			}
		} else {
			select {
			case a.pending <- struct{}{}:
				defer func() { <-a.pending }()
			default:
				return denyRequest(req, 503, "Gateway is busy", "GATEWAY_BUSY", 1), nil
			}
			hash := sha256.Sum256([]byte(subject))
			decision, e := a.Limiter.Check(ctx, policy, hex.EncodeToString(hash[:]), limit)
			if e != nil || !decision.Allowed {
				slog.WarnContext(ctx, "rate_limit_decision", "policy", policy, "mode", a.Config.Mode, "unavailable", e != nil, "retry_after", decision.RetryAfter)
				if a.Config.Mode == "enforce" {
					if e != nil {
						return denyRequest(req, 503, "Rate limit service unavailable", "LIMITER_UNAVAILABLE", 1), nil
					}
					return denyRequest(req, 429, "Too many requests", "RATE_LIMITED", decision.RetryAfter), nil
				}
			}
		}
	}
	response := &authv3.OkHttpResponse{HeadersToRemove: []string{"x-user-id", "x-nordik-user-id", "x-workload-token", "x-operator-authorization", "x-serverless-authorization", "x-honour-job-secret", "x-envoy-internal", "x-envoy-original-path", "x-envoy-retry-on", "x-envoy-retry-grpc-on", "x-envoy-max-retries", "x-envoy-upstream-rq-timeout-ms", "x-envoy-upstream-rq-per-try-timeout-ms", "x-envoy-hedge-on-per-try-timeout", "x-envoy-retriable-status-codes", "x-envoy-upstream-rq-timeout-alt-response", "x-forwarded-client-cert", "x-goog-authenticated-user-email", "x-goog-authenticated-user-id"}}
	if !a.Config.Local {
		if a.Tokens[route.Service] == nil {
			return denyRequest(req, 503, "Backend identity unavailable", "WORKLOAD_UNAVAILABLE", 1), nil
		}
		token, e := a.Tokens[route.Service].Token()
		if e != nil || token == nil || !token.Valid() {
			return denyRequest(req, 503, "Backend identity unavailable", "WORKLOAD_UNAVAILABLE", 1), nil
		}
		// A header must not be listed for removal and replacement simultaneously.
		response.HeadersToRemove = removeHeader(response.HeadersToRemove, "x-serverless-authorization")
		response.Headers = append(response.Headers, header("x-serverless-authorization", "Bearer "+token.AccessToken))
	}
	return &authv3.CheckResponse{Status: &status.Status{Code: 0}, HttpResponse: &authv3.CheckResponse_OkResponse{OkResponse: response}}, nil
}
func removeHeader(values []string, name string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v != name {
			out = append(out, v)
		}
	}
	return out
}
func header(key, value string) *corev3.HeaderValueOption {
	return &corev3.HeaderValueOption{Header: &corev3.HeaderValue{Key: key, Value: value}, AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD}
}
func deny(code int, message, machine string, retry int) *authv3.CheckResponse {
	body, _ := json.Marshal(map[string]any{"error": message, "code": machine})
	headers := []*corev3.HeaderValueOption{header("content-type", "application/json")}
	if retry > 0 {
		headers = append(headers, header("retry-after", strconv.Itoa(max(1, retry))))
	}
	return &authv3.CheckResponse{Status: &status.Status{Code: 7}, HttpResponse: &authv3.CheckResponse_DeniedResponse{DeniedResponse: &authv3.DeniedHttpResponse{Status: &typev3.HttpStatus{Code: typev3.StatusCode(code)}, Headers: headers, Body: string(body)}}}
}

func denyRequest(req *authv3.CheckRequest, code int, message, machine string, retry int) *authv3.CheckResponse {
	response := deny(code, message, machine, retry)
	denied := response.GetDeniedResponse()
	headers := req.GetAttributes().GetRequest().GetHttp().GetHeaders()
	id := headers["x-request-id"]
	if _, err := uuid.Parse(id); err != nil {
		id = uuid.NewString()
	}
	body, _ := json.Marshal(map[string]any{"error": message, "code": machine, "request_id": id})
	denied.Body = string(body)
	denied.Headers = append(denied.Headers, header("x-request-id", id))
	for _, origin := range appconfig.AllowedOrigins() {
		if origin == headers["origin"] {
			denied.Headers = append(denied.Headers, header("access-control-allow-origin", origin), header("access-control-allow-credentials", "true"), header("access-control-expose-headers", "X-Request-ID, Retry-After"), header("vary", "Origin"))
			break
		}
	}
	return response
}
func (a *Authorizer) clientIP(headers map[string]string, peer string) (string, error) {
	value := peer
	if a.Config.ClientIPMode == "forwarded" {
		parts := strings.Split(headers["x-forwarded-for"], ",")
		index := len(parts) - a.Config.TrustedHops
		if index < 0 {
			return "", fmt.Errorf("missing trusted proxy chain")
		}
		value = strings.TrimSpace(parts[index])
	}
	if host, _, err := net.SplitHostPort(value); err == nil {
		value = host
	}
	ip := net.ParseIP(value)
	if ip == nil {
		return "", fmt.Errorf("invalid address")
	}
	return ip.String(), nil
}
func (a *Authorizer) burst(key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	entry, ok := a.bursts[key]
	if !ok {
		if len(a.bursts) >= 10000 {
			for k, v := range a.bursts {
				if now.Sub(v.used) > 2*time.Minute {
					delete(a.bursts, k)
				}
			}
		}
		if len(a.bursts) >= 10000 {
			return false
		}
		entry = burstEntry{limiter: rate.NewLimiter(50, 100)}
	}
	entry.used = now
	a.bursts[key] = entry
	return entry.limiter.Allow()
}
