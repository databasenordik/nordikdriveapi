package dataconfig

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type mockDataConfigService struct {
	getByFileNameIfModifiedFn func(fileName string, clientLastModified *time.Time) (*GetConfigResult, error)
}

func (m *mockDataConfigService) GetByFileNameIfModified(fileName string, clientLastModified *time.Time) (*GetConfigResult, error) {
	if m.getByFileNameIfModifiedFn == nil {
		return nil, nil
	}
	return m.getByFileNameIfModifiedFn(fileName, clientLastModified)
}

func setupControllerRouter(svc DataConfigServiceAPI) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	cc := &DataConfigController{DataConfigService: svc}
	r.GET("/api/config", cc.GetConfig)
	return r
}

func TestParseOptionalTime(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		got, err := parseOptionalTime("   ")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got != nil {
			t.Fatalf("expected nil, got %v", got)
		}
	})

	t.Run("rfc3339nano", func(t *testing.T) {
		in := "2026-02-25T10:20:30.123456789Z"
		got, err := parseOptionalTime(in)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got == nil {
			t.Fatal("expected time, got nil")
		}
		if got.UTC().Format(time.RFC3339Nano) != in {
			t.Fatalf("got %s want %s", got.UTC().Format(time.RFC3339Nano), in)
		}
	})

	t.Run("unix ms", func(t *testing.T) {
		in := "1708451234567"
		got, err := parseOptionalTime(in)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got == nil {
			t.Fatal("expected time, got nil")
		}
		want := time.Unix(0, 1708451234567*int64(time.Millisecond))
		if !got.Equal(want) {
			t.Fatalf("got %v want %v", got, want)
		}
	})

	t.Run("invalid", func(t *testing.T) {
		got, err := parseOptionalTime("bad-time")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if got != nil {
			t.Fatalf("expected nil, got %v", got)
		}
	})
}

func TestDataConfigController_GetConfig_MissingFileName(t *testing.T) {
	r := setupControllerRouter(&mockDataConfigService{})

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestDataConfigController_GetConfig_InvalidLastModified(t *testing.T) {
	r := setupControllerRouter(&mockDataConfigService{})

	req := httptest.NewRequest(http.MethodGet, "/api/config?file_name=test.json&last_modified=bad", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestDataConfigController_GetConfig_NotFound(t *testing.T) {
	r := setupControllerRouter(&mockDataConfigService{
		getByFileNameIfModifiedFn: func(fileName string, clientLastModified *time.Time) (*GetConfigResult, error) {
			return nil, gorm.ErrRecordNotFound
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/config?file_name=test.json", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestDataConfigController_GetConfig_InternalError(t *testing.T) {
	r := setupControllerRouter(&mockDataConfigService{
		getByFileNameIfModifiedFn: func(fileName string, clientLastModified *time.Time) (*GetConfigResult, error) {
			return nil, errors.New("db failed")
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/config?file_name=test.json", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
}

func TestDataConfigController_GetConfig_Modified(t *testing.T) {
	updatedAt := time.Date(2026, 2, 25, 12, 0, 0, 0, time.UTC)
	cfg := &DataConfig{
		FileID:    10,
		FileName:  "test.json",
		Version:   2,
		Checksum:  "abc123",
		Config:    datatypes.JSON([]byte(`{"name":"athul","enabled":true}`)),
		UpdatedAt: updatedAt,
	}

	r := setupControllerRouter(&mockDataConfigService{
		getByFileNameIfModifiedFn: func(fileName string, clientLastModified *time.Time) (*GetConfigResult, error) {
			if fileName != "test.json" {
				t.Fatalf("unexpected fileName: %s", fileName)
			}
			return &GetConfigResult{
				NotModified: false,
				Config:      cfg,
			}, nil
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/config?file_name=test.json", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	if got := w.Header().Get("ETag"); got != fmt.Sprintf(`"%x"`, sha256.Sum256(cfg.Config)) {
		t.Fatalf("etag = %q, want content checksum", got)
	}
	if got := w.Header().Get("Last-Modified"); got != updatedAt.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("last-modified = %q", got)
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if body["not_modified"] != false {
		t.Fatalf("not_modified = %v", body["not_modified"])
	}

	cfgBody, ok := body["config"].(map[string]any)
	if !ok {
		t.Fatalf("config missing or invalid: %#v", body["config"])
	}
	if cfgBody["name"] != "athul" {
		t.Fatalf("config.name = %v", cfgBody["name"])
	}
	if cfgBody["enabled"] != true {
		t.Fatalf("config.enabled = %v", cfgBody["enabled"])
	}
}

func TestDataConfigController_GetConfig_ModifiedUsesSourceFileDataConfig(t *testing.T) {
	updatedAt := time.Date(2026, 2, 25, 12, 0, 0, 0, time.UTC)
	cfg := &DataConfig{
		FileID:   12,
		FileName: "source.json",
		Version:  4,
		Checksum: "cfg-4",
		Config: datatypes.JSON([]byte(`{
			"fields": [
				{"key":"id","additional_field":true}
			],
			"source_file": {
				"data_config": {
					"fields": [
						{"key":"student_name","label":"Student Name","additional_field":false},
						{"key":"nia_comments","label":"NIA comments","additional_field":true}
					]
				}
			}
		}`)),
		UpdatedAt: updatedAt,
	}

	r := setupControllerRouter(&mockDataConfigService{
		getByFileNameIfModifiedFn: func(fileName string, clientLastModified *time.Time) (*GetConfigResult, error) {
			return &GetConfigResult{
				NotModified: false,
				Config:      cfg,
			}, nil
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/config?file_name=source.json", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	cfgBody, ok := body["config"].(map[string]any)
	if !ok {
		t.Fatalf("config missing or invalid: %#v", body["config"])
	}

	fields, ok := cfgBody["fields"].([]any)
	if !ok || len(fields) != 1 {
		t.Fatalf("root config fields missing or invalid: %#v", cfgBody["fields"])
	}
	rootField, ok := fields[0].(map[string]any)
	if !ok || rootField["key"] != "id" || rootField["additional_field"] != true {
		t.Fatalf("root additional field should be returned, got %#v", fields[0])
	}

	sourceFile, ok := cfgBody["source_file"].(map[string]any)
	if !ok {
		t.Fatalf("source_file missing or invalid: %#v", cfgBody["source_file"])
	}
	sourceConfig, ok := sourceFile["data_config"].(map[string]any)
	if !ok {
		t.Fatalf("source_file.data_config missing or invalid: %#v", sourceFile["data_config"])
	}
	sourceFields, ok := sourceConfig["fields"].([]any)
	if !ok || len(sourceFields) != 2 {
		t.Fatalf("source-file fields missing or invalid: %#v", sourceConfig["fields"])
	}
	additionalField, ok := sourceFields[1].(map[string]any)
	if !ok || additionalField["key"] != "nia_comments" || additionalField["additional_field"] != true {
		t.Fatalf("source-file additional field should be returned, got %#v", sourceFields[1])
	}
}

func TestDataConfigController_GetConfig_NotModified(t *testing.T) {
	updatedAt := time.Date(2026, 2, 25, 12, 0, 0, 0, time.UTC)
	cfg := &DataConfig{
		FileID:    11,
		FileName:  "cache.json",
		Version:   3,
		Checksum:  "etag-3",
		Config:    datatypes.JSON([]byte(`{"x":1}`)),
		UpdatedAt: updatedAt,
	}

	r := setupControllerRouter(&mockDataConfigService{
		getByFileNameIfModifiedFn: func(fileName string, clientLastModified *time.Time) (*GetConfigResult, error) {
			return &GetConfigResult{
				NotModified: true,
				Config:      cfg,
			}, nil
		},
	})

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/config?file_name=cache.json&checksum=%x", sha256.Sum256(cfg.Config)), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if body["not_modified"] != true {
		t.Fatalf("not_modified = %v", body["not_modified"])
	}
	if _, exists := body["config"]; exists {
		t.Fatalf("config should not exist, got %#v", body["config"])
	}
}

func TestDataConfigController_ContentEditInvalidatesCacheWithoutTimestampChange(t *testing.T) {
	updatedAt := time.Date(2026, 2, 25, 12, 0, 0, 0, time.UTC)
	oldContent := datatypes.JSON([]byte(`{"enabled":true}`))
	cfg := &DataConfig{FileName: "cache.json", Config: oldContent, Checksum: "unchanged-stored-checksum", UpdatedAt: updatedAt}
	r := setupControllerRouter(&mockDataConfigService{
		getByFileNameIfModifiedFn: func(string, *time.Time) (*GetConfigResult, error) {
			return &GetConfigResult{Config: cfg}, nil
		},
	})
	cfg.Config = datatypes.JSON([]byte(`{"enabled":false}`))
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/config?file_name=cache.json&checksum=%x&last_modified=%s", sha256.Sum256(oldContent), updatedAt.Format(time.RFC3339Nano)), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["not_modified"] != false {
		t.Fatalf("content change was suppressed: %s", w.Body.String())
	}
	if body["config"].(map[string]any)["enabled"] != false {
		t.Fatalf("stale config: %s", w.Body.String())
	}
}

func TestRegisterRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()

	RegisterRoutes(r, &mockDataConfigService{})

	found := false
	for _, route := range r.Routes() {
		if route.Method == http.MethodGet && route.Path == "/api/config" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("GET /api/config not registered")
	}
}
