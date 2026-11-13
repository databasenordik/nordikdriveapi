package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"nordik-drive-api/internal/notificationcontract"
	"nordik-drive-api/internal/workload"
)

type Authorizer func(context.Context, string, string) (string, error)
type callerContextKey struct{}

// Roles: producer:<owner>, delivery, recovery. No browser/login token is accepted.
func WorkloadAuthorizer(audience string, grants map[string]map[string]bool) Authorizer {
	return func(ctx context.Context, token, role string) (string, error) {
		if len(grants[role]) == 0 {
			return "", errors.New("caller role is not allowed")
		}
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		return workload.VerifiedAccount(ctx, token, audience, grants[role])
	}
}
func Handler(service *Service, authorize Authorizer) http.Handler {
	mux := http.NewServeMux()
	health := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }
	mux.HandleFunc("GET /healthz", health)
	mux.HandleFunc("GET /health", health)
	guard := func(role string, next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			account, err := authorize(r.Context(), token, role)
			if err != nil || account == "" {
				http.Error(w, "forbidden", 403)
				return
			}
			next(w, r.WithContext(context.WithValue(r.Context(), callerContextKey{}, account)))
		}
	}
	mux.HandleFunc("POST /internal/v1/notifications", func(w http.ResponseWriter, r *http.Request) {
		var command notificationcontract.Command
		if !decode(w, r, &command) {
			return
		}
		guard("producer:"+command.Producer, func(w http.ResponseWriter, r *http.Request) {
			id, err := service.Accept(r.Context(), command)
			if errors.Is(err, notificationcontract.ErrInvalid) {
				http.Error(w, "invalid command", 400)
				return
			}
			if errors.Is(err, ErrConflict) {
				http.Error(w, "conflicting event", 409)
				return
			}
			if err != nil {
				http.Error(w, "intake unavailable", 503)
				return
			}
			respond(w, 202, map[string]string{"delivery_id": id})
		})(w, r)
	})
	mux.HandleFunc("POST /internal/v1/deliver", guard("delivery", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ID string `json:"delivery_id"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.ID == "" {
			http.Error(w, "missing delivery ID", 400)
			return
		}
		if err := service.Deliver(r.Context(), body.ID); err != nil {
			w.Header().Set("Retry-After", "30")
			http.Error(w, "delivery incomplete", 503)
			return
		}
		w.WriteHeader(204)
	}))
	mux.HandleFunc("POST /internal/v1/recover", guard("recovery", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		if err := service.Recover(ctx); err != nil {
			http.Error(w, "recovery incomplete", 503)
			return
		}
		w.WriteHeader(204)
	}))
	mux.HandleFunc("POST /internal/v1/status", guard("recovery", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			IDs []string `json:"ids"`
		}
		if !decode(w, r, &body) {
			return
		}
		if len(body.IDs) == 0 || len(body.IDs) > 100 {
			http.Error(w, "invalid batch", 400)
			return
		}
		states, err := service.Store.Statuses(r.Context(), body.IDs)
		if err != nil {
			http.Error(w, "status unavailable", 503)
			return
		}
		respond(w, 200, states)
	}))
	mux.HandleFunc("POST /internal/v1/replay", guard("operator", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ID     string `json:"delivery_id"`
			Reason string `json:"reason"`
		}
		if !decode(w, r, &body) {
			return
		}
		caller, _ := r.Context().Value(callerContextKey{}).(string)
		err := service.Store.Replay(r.Context(), body.ID, r.Header.Get("Idempotency-Key"), caller, body.Reason, service.now())
		if errors.Is(err, ErrConflict) {
			http.Error(w, "delivery cannot be replayed with this approval", 409)
			return
		}
		if err != nil {
			http.Error(w, "replay unavailable", 503)
			return
		}
		_ = service.Recover(r.Context())
		respond(w, 202, map[string]string{"delivery_id": body.ID})
	}))
	return mux
}
func decode(w http.ResponseWriter, r *http.Request, value any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 512*1024))
	d.DisallowUnknownFields()
	if d.Decode(value) != nil || d.Decode(&struct{}{}) != io.EOF {
		http.Error(w, "invalid JSON", 400)
		return false
	}
	return true
}
func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
