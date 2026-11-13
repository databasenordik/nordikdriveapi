package workerapp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"nordik-drive-api/internal/database"
	"nordik-drive-api/internal/workload"
)

// The fixed query exposes counts only. No SQL text, connection addresses, user
// rows, credentials, or caller-supplied query/filter reach this endpoint.
const inspectionSQL = `SELECT application_name, state, owner, connection_count
FROM public.runtime_connection_counts($1::jsonb)`

func NewInspectionHandler(pool *sql.DB, audience, deployer string) (http.Handler, error) {
	if pool == nil || audience == "" || deployer == "" {
		return nil, errors.New("inspection requires existing pool, audience and deployer identity")
	}
	owners := os.Getenv("ROLLOUT_DATABASE_OWNERS")
	var mapping map[string]string
	if json.Unmarshal([]byte(owners), &mapping) != nil || len(mapping) != 6 || mapping[os.Getenv("DB_USER")] != "worker" {
		return nil, errors.New("inspection requires the reviewed runtime database-owner map")
	}
	seen := map[string]bool{}
	for username, owner := range mapping {
		if username == "" || seen[owner] || (owner != "gateway" && owner != "legacy" && owner != "worker" && owner != "notifications" && owner != "identity" && owner != "audit") {
			return nil, errors.New("invalid inspection database-owner map")
		}
		seen[owner] = true
	}
	return inspectionHandler(pool, func(ctx context.Context, token string) error {
		verify, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		return workload.Verify(verify, token, audience, map[string]bool{deployer: true})
	}, owners), nil
}

func inspectionHandler(pool *sql.DB, authorize func(context.Context, string) error, ownerMaps ...string) http.Handler {
	owners := "{}"
	if len(ownerMaps) > 0 {
		owners = ownerMaps[0]
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet {
			w.WriteHeader(405)
			return
		}
		if authorize(r.Context(), strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")) != nil {
			http.Error(w, "forbidden", 403)
			return
		}
		if r.URL.RawQuery != "" {
			http.Error(w, "query parameters are not supported", 400)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		conn, err := database.Acquire(ctx, pool, 250*time.Millisecond)
		if err != nil {
			http.Error(w, "inspection unavailable", 503)
			return
		}
		defer conn.Close()
		rows, err := conn.QueryContext(ctx, inspectionSQL, owners)
		if err != nil {
			http.Error(w, "inspection unavailable", 503)
			return
		}
		defer rows.Close()
		type aggregate struct {
			Application string `json:"application"`
			State       string `json:"state"`
			Owner       string `json:"owner"`
			Count       int64  `json:"count"`
		}
		result := []aggregate{}
		for rows.Next() {
			var item aggregate
			var state sql.NullString
			if err := rows.Scan(&item.Application, &state, &item.Owner, &item.Count); err != nil {
				http.Error(w, "inspection unavailable", 503)
				return
			}
			item.State = state.String
			result = append(result, item)
		}
		if rows.Err() != nil {
			http.Error(w, "inspection unavailable", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Version    int         `json:"version"`
			Sessions   []aggregate `json:"sessions"`
			ObservedAt time.Time   `json:"observed_at"`
		}{2, result, time.Now().UTC()})
	})
}
