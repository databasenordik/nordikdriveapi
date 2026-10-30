//go:build integration
// +build integration

package community

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func setupRouterWithDB(t *testing.T, migrate bool) (*gin.Engine, *gorm.DB) {
	t.Helper()

	gin.SetMode(gin.TestMode)

	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "integration.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open isolated sqlite db: %v", err)
	}

	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	if migrate {
		if err := db.AutoMigrate(&Community{}); err != nil {
			t.Fatalf("failed to automigrate: %v", err)
		}
	}

	svc := &CommunityService{DB: db}
	r := gin.New()
	RegisterRoutes(r, svc)

	return r, db
}

func doRequest(r http.Handler, method, path string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestIntegration_Community_TableName_Coverage(t *testing.T) {
	// ensures TableName() is covered
	if (Community{}).TableName() != "communities" {
		t.Fatalf("unexpected table name: %s", (Community{}).TableName())
	}
}

func TestIntegration_Community_GetAllCommunities_Empty_OK200(t *testing.T) {
	r, _ := setupRouterWithDB(t, true)

	w := doRequest(r, http.MethodGet, "/api/communities", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}

	var resp struct {
		Message     string      `json:"message"`
		Communities []Community `json:"communities"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v body=%s", err, w.Body.String())
	}

	if resp.Message != "Communities fetched successfully" {
		t.Fatalf("unexpected message: %q", resp.Message)
	}
	if len(resp.Communities) != 0 {
		t.Fatalf("expected 0 communities, got %d", len(resp.Communities))
	}
}

func TestIntegration_Community_AddCommunities_Created201_ThenGet_OK200(t *testing.T) {
	r, _ := setupRouterWithDB(t, true)

	payload := map[string]any{
		"communities": []string{"Shingwauk", "Batchewana"},
	}
	b, _ := json.Marshal(payload)

	// POST should create
	w := doRequest(r, http.MethodPost, "/api/communities", b)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", w.Code, w.Body.String())
	}

	// GET should return 2
	w2 := doRequest(r, http.MethodGet, "/api/communities", nil)
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w2.Code, w2.Body.String())
	}

	var resp struct {
		Message     string      `json:"message"`
		Communities []Community `json:"communities"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v body=%s", err, w2.Body.String())
	}

	if len(resp.Communities) != 2 {
		t.Fatalf("expected 2 communities, got %d", len(resp.Communities))
	}

	// verify stored fields/behavior
	for _, c := range resp.Communities {
		if c.ID == 0 {
			t.Fatalf("expected ID to be set, got %+v", c)
		}
		if c.Name == "" {
			t.Fatalf("expected Name to be set, got %+v", c)
		}
		if c.Approved != false {
			t.Fatalf("expected Approved=false by default, got %+v", c)
		}
	}
}

func TestIntegration_Community_AddCommunities_BadRequest400_MissingField(t *testing.T) {
	r, _ := setupRouterWithDB(t, true)

	// missing "communities" entirely -> binding required should fail
	w := doRequest(r, http.MethodPost, "/api/communities", []byte(`{}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestIntegration_Community_AddCommunities_BadRequest400_InvalidJSON(t *testing.T) {
	r, _ := setupRouterWithDB(t, true)

	// malformed json -> bind should fail
	w := doRequest(r, http.MethodPost, "/api/communities", []byte(`{"communities":[`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestIntegration_Community_AddCommunities_BadRequest400_WrongType(t *testing.T) {
	r, _ := setupRouterWithDB(t, true)

	// communities must be []string; giving string should fail bind
	w := doRequest(r, http.MethodPost, "/api/communities", []byte(`{"communities":"Shingwauk"}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestIntegration_Community_GetAllCommunities_DBError500_NoMigration(t *testing.T) {
	// no AutoMigrate -> table doesn't exist -> DB error -> 500 path
	r, _ := setupRouterWithDB(t, false)

	w := doRequest(r, http.MethodGet, "/api/communities", nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestIntegration_Community_AddCommunities_DBError500_NoMigration(t *testing.T) {
	// no AutoMigrate -> insert should fail -> 500 path
	r, _ := setupRouterWithDB(t, false)

	w := doRequest(r, http.MethodPost, "/api/communities", []byte(`{"communities":["Shingwauk"]}`))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d body=%s", w.Code, w.Body.String())
	}
}
