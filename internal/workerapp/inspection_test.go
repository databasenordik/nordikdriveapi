package workerapp

import (
	"context"
	"errors"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestInspectionIsPrivateAndFixed(t *testing.T) {
	for _, tc := range []struct {
		name, method, path              string
		denied                          bool
		queryError, scanError, rowError bool
		status                          int
	}{
		{"success", "GET", "/internal/v1/rollout/connections", false, false, false, false, 200},
		{"wrong caller", "GET", "/internal/v1/rollout/connections", true, false, false, false, 403},
		{"wrong method", "POST", "/internal/v1/rollout/connections", false, false, false, false, 405},
		{"arbitrary query", "GET", "/internal/v1/rollout/connections?sql=secret", false, false, false, false, 400},
		{"database error", "GET", "/internal/v1/rollout/connections", false, true, false, false, 503},
		{"invalid result", "GET", "/internal/v1/rollout/connections", false, false, true, false, 503},
		{"interrupted rows", "GET", "/internal/v1/rollout/connections", false, false, false, true, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			if !tc.denied && tc.method == "GET" && !strings.Contains(tc.path, "?") {
				query := mock.ExpectQuery(regexp.QuoteMeta(inspectionSQL))
				if tc.queryError {
					query.WillReturnError(errors.New("SECRET_DATABASE_ERROR"))
				} else {
					rows := sqlmock.NewRows([]string{"application_name", "state", "owner", "count"})
					if tc.scanError {
						rows.AddRow("run:worker", "idle", "worker", "invalid")
					} else {
						rows.AddRow("run:worker", "idle", "worker", 2).AddRow("external", nil, "external", 1)
					}
					if tc.rowError {
						rows.RowError(1, errors.New("SECRET_ROW_ERROR"))
					}
					query.WillReturnRows(rows)
				}
			}
			handler := inspectionHandler(pool, func(context.Context, string) error {
				if tc.denied {
					return errors.New("unauthorized")
				}
				return nil
			})
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(tc.method, tc.path, nil))
			if response.Code != tc.status {
				t.Fatalf("HTTP %d, want %d", response.Code, tc.status)
			}
			if response.Header().Get("Cache-Control") != "no-store" || strings.Contains(response.Body.String(), "SECRET") {
				t.Fatal("inspection leaked diagnostics or cacheable data")
			}
			if tc.status == 200 && (!strings.Contains(response.Body.String(), `"count":2`) || !strings.Contains(response.Body.String(), `"observed_at"`)) {
				t.Fatal("missing aggregate evidence")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInspectionCannotGrowOrWaitBeyondExistingPool(t *testing.T) {
	pool, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	pool.SetMaxOpenConns(1)
	conn, err := pool.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	response := httptest.NewRecorder()
	inspectionHandler(pool, func(context.Context, string) error { return nil }).ServeHTTP(response, httptest.NewRequest("GET", "/internal/v1/rollout/connections", nil).WithContext(ctx))
	if response.Code != 503 || pool.Stats().OpenConnections != 1 {
		t.Fatal("inspection exceeded existing pool or ignored cancellation")
	}
	if _, err := NewInspectionHandler(nil, "audience", "deployer"); err == nil {
		t.Fatal("nil pool accepted")
	}
	if _, err := NewInspectionHandler(pool, "", "deployer"); err == nil {
		t.Fatal("missing audience accepted")
	}
	if _, err := NewInspectionHandler(pool, "audience", ""); err == nil {
		t.Fatal("missing deployer accepted")
	}
	t.Setenv("ROLLOUT_DATABASE_OWNERS", `{"worker-user":"worker","gateway-user":"gateway","legacy-user":"legacy","notify-user":"notifications","identity-user":"identity","audit-user":"audit"}`)
	t.Setenv("DB_USER", "worker-user")
	handler, err := NewInspectionHandler(pool, "https://worker.example", "deployer@example.iam.gserviceaccount.com")
	if err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/internal/v1/rollout/connections", nil))
	if response.Code != 403 {
		t.Fatal("missing signed Google token accepted")
	}
}
