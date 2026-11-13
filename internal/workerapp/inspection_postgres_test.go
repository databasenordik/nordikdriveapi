//go:build integration

package workerapp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestPostgresInspectionCountsOldAndLabelledOwnersWithoutUserData(t *testing.T) {
	connection, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil || connection == nil || connection.User == nil || connection.User.Username() != "nordik_test" || connection.Path != "/nordik_baseline_test" || (connection.Hostname() != "127.0.0.1" && connection.Hostname() != "localhost" && connection.Hostname() != "::1") || (connection.Scheme != "postgres" && connection.Scheme != "postgresql") {
		t.Fatal("inspection integration requires the disposable local nordik_baseline_test fixture")
	}
	values := url.Values{"sslmode": {"disable"}, "connect_timeout": {"5"}, "application_name": {"m4-inspection-fixture"}}
	connection.RawQuery = values.Encode()
	admin, err := sql.Open("pgx", connection.String())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	role := "m4_inspect_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec("CREATE ROLE " + pgx.Identifier{role}.Sanitize() + " NOLOGIN"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = admin.Exec("DROP FUNCTION IF EXISTS public.runtime_connection_counts(jsonb)")
		_, _ = admin.Exec("DROP ROLE " + pgx.Identifier{role}.Sanitize())
	}()
	migration, err := os.ReadFile("../../migrations/005_runtime_inspection.sql")
	if err != nil {
		t.Fatal(err)
	}
	source := strings.ReplaceAll(string(migration), `\set ON_ERROR_STOP on`, "")
	source = strings.ReplaceAll(source, `:"worker_user"`, pgx.Identifier{role}.Sanitize())
	if _, err := admin.Exec(source); err != nil {
		t.Fatal("install restricted aggregate function", err)
	}
	values.Set("options", "-c role="+role)
	connection.RawQuery = values.Encode()
	pool, err := sql.Open("pgx", connection.String())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	pool.SetMaxOpenConns(2)
	pool.SetMaxIdleConns(1)
	var broadStats bool
	if err := pool.QueryRow("SELECT pg_has_role(current_user,'pg_read_all_stats','USAGE')").Scan(&broadStats); err != nil || broadStats {
		t.Fatal("Worker must not receive broad statistics privileges", err)
	}
	// A caller-owned temporary relation must never shadow the real catalogue
	// inside the SECURITY DEFINER function and manufacture false zero counts.
	if _, err := pool.Exec("CREATE TEMP TABLE pg_stat_activity(application_name text,state text,usename text,datname text,backend_type text)"); err != nil {
		t.Fatal(err)
	}
	foreign, err := admin.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	if _, err := foreign.ExecContext(context.Background(), "SET application_name='run:m4-inspection-foreign'"); err != nil {
		t.Fatal(err)
	}
	handler := inspectionHandler(pool, func(_ context.Context, token string) error {
		if token != "fixture-deployer" {
			return errors.New("denied")
		}
		return nil
	}, `{"nordik_test":"worker"}`)
	request := httptest.NewRequest("GET", "/internal/v1/rollout/connections", nil)
	request.Header.Set("Authorization", "Bearer fixture-deployer")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatalf("fixed PostgreSQL aggregate query failed: status %d", response.Code)
	}
	var result struct {
		Version    int       `json:"version"`
		ObservedAt time.Time `json:"observed_at"`
		Sessions   []struct {
			Application, Owner, State string
			Count                     int64
		} `json:"sessions"`
	}
	if json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Version != 2 || time.Since(result.ObservedAt) > time.Minute {
		t.Fatal("missing versioned current inspection evidence")
	}
	found := false
	foreignFound := false
	for _, item := range result.Sessions {
		if item.Application == "m4-inspection-fixture" && item.Owner == "worker" && item.Count >= 1 {
			found = true
		}
		if item.Application == "run:m4-inspection-foreign" && item.Count >= 1 {
			foreignFound = true
		}
	}
	if !found || !foreignFound {
		t.Fatal("unlabelled or another role's revision sessions were not counted")
	}
	conn, err := pool.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), "SET application_name='run:m4-inspection-labelled'"); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	result.Sessions = nil
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil {
		t.Fatal("labelled connection inspection failed")
	}
	found = false
	for _, item := range result.Sessions {
		if item.Application == "run:m4-inspection-labelled" && item.Owner == "worker" && item.Count >= 1 {
			found = true
		}
	}
	if !found {
		t.Fatal("revision-labelled sessions were not counted")
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(response.Body.Bytes(), &envelope) != nil || len(envelope) != 3 {
		t.Fatal("inspection exposed fields beyond version/timestamp/aggregate counts")
	}
	if pool.Stats().OpenConnections > 2 {
		t.Fatal("inspection exceeded its existing two-connection pool")
	}
}
