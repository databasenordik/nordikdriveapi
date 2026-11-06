package gateway

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	AcquireMillis, QueryMillis                                 int
	Backends                                                   map[string]Backend
	BackendURL, BackendAudience, JWTSecret, Mode, ClientIPMode string
	TrustedHops, AuthLimit, PublicLimit, UserLimit, ChatLimit  int
	Local                                                      bool
}

type Backend struct {
	URL      string `json:"url"`
	Audience string `json:"audience"`
}

func LoadConfig() (Config, error) {
	c := Config{BackendURL: os.Getenv("LEGACY_BACKEND_URL"), BackendAudience: os.Getenv("LEGACY_BACKEND_AUDIENCE"), JWTSecret: os.Getenv("JWT_SECRET"), Mode: os.Getenv("RATE_LIMIT_MODE"), ClientIPMode: os.Getenv("CLIENT_IP_MODE"), AuthLimit: 20, PublicLimit: 120, UserLimit: 120, ChatLimit: 10}
	if c.Mode == "" {
		c.Mode = "observe"
	}
	if c.ClientIPMode == "" {
		c.ClientIPMode = "peer"
	}
	c.AcquireMillis, c.QueryMillis = 2000, 250
	var err error
	if value := os.Getenv("GATEWAY_LOCAL"); value != "" {
		c.Local, err = strconv.ParseBool(value)
		if err != nil {
			return c, errors.New("invalid GATEWAY_LOCAL")
		}
	}
	c.Backends = map[string]Backend{}
	if raw := os.Getenv("SERVICE_BACKENDS_JSON"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &c.Backends); err != nil {
			return c, errors.New("invalid SERVICE_BACKENDS_JSON")
		}
	}
	for _, name := range ServiceNames {
		if _, ok := c.Backends[name]; !ok {
			c.Backends[name] = Backend{URL: c.BackendURL, Audience: c.BackendAudience}
		}
	}
	for name, target := range map[string]*int{"RATE_DB_ACQUIRE_MS": &c.AcquireMillis, "RATE_DB_QUERY_MS": &c.QueryMillis, "TRUSTED_PROXY_HOPS": &c.TrustedHops, "RATE_AUTH_PER_MINUTE": &c.AuthLimit, "RATE_PUBLIC_PER_MINUTE": &c.PublicLimit, "RATE_USER_PER_MINUTE": &c.UserLimit, "RATE_CHAT_PER_MINUTE": &c.ChatLimit} {
		if raw := os.Getenv(name); raw != "" {
			n, e := strconv.Atoi(raw)
			if e != nil || n <= 0 || n > 1000000 {
				return c, errors.New("invalid " + name)
			}
			*target = n
		}
	}
	return c, c.Validate()
}
func (c Config) Validate() error {
	if c.AcquireMillis < 0 || c.QueryMillis < 0 || c.AcquireMillis+c.QueryMillis > 4500 {
		return errors.New("limiter acquisition/query budgets must fit within 4500ms")
	}
	for _, name := range ServiceNames {
		b, ok := c.Backends[name]
		if !ok {
			return errors.New("missing backend for " + name)
		}
		u, err := url.Parse(b.URL)
		if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return errors.New("backend must be an origin URL: " + name)
		}
		if u.Scheme != "https" && !(c.Local && u.Scheme == "http") {
			return errors.New("backend requires HTTPS outside local mode")
		}
		if !c.Local && !strings.HasPrefix(b.Audience, "https://") {
			return errors.New("backend audience is required: " + name)
		}
	}
	if strings.TrimSpace(c.JWTSecret) == "" {
		return errors.New("JWT_SECRET is required")
	}
	if c.Mode != "observe" && c.Mode != "enforce" {
		return errors.New("RATE_LIMIT_MODE must be observe or enforce")
	}
	if c.ClientIPMode != "peer" && c.ClientIPMode != "forwarded" {
		return errors.New("CLIENT_IP_MODE must be peer or forwarded")
	}
	if c.ClientIPMode == "forwarded" && (c.TrustedHops < 1 || c.TrustedHops > 10) {
		return errors.New("forwarded mode requires 1-10 TRUSTED_PROXY_HOPS")
	}
	if !c.Local && c.ClientIPMode != "forwarded" {
		return errors.New("production requires a verified forwarded client IP configuration")
	}
	if c.AuthLimit < 1 || c.PublicLimit < 1 || c.UserLimit < 1 || c.ChatLimit < 1 {
		return errors.New("rate thresholds must be positive")
	}
	return nil
}
