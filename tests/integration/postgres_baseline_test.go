//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"nordik-drive-api/internal/auth"
	"nordik-drive-api/internal/file"
	"nordik-drive-api/internal/logs"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/lib/pq"
	"gorm.io/gorm"
)

func recordsFixture(t testing.TB) (*gorm.DB, *auth.Auth, *file.File) {
	t.Helper()
	db := newPostgresFixture(t)
	if err := db.AutoMigrate(&auth.Auth{}, &file.File{}, &file.FileVersion{}, &file.FileData{}, &file.FileEditRequest{}, &logs.SystemLog{}); err != nil {
		t.Fatalf("migrate synthetic fixture: %v", err)
	}
	// FileAccess's legacy ORM annotation says json; deployed access joins require
	// numeric IDs. Define the fixture's join table explicitly, not a migration.
	for _, statement := range []string{
		`CREATE TABLE file_access (id bigserial PRIMARY KEY, user_id bigint NOT NULL, file_id bigint NOT NULL)`,
		`CREATE TABLE form_submissions (id bigserial PRIMARY KEY, file_id bigint NOT NULL, status text NOT NULL)`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	user := &auth.Auth{FirstName: "Synthetic", LastName: "User", Email: "synthetic@example.com", Password: "unused-test-hash", Role: "User", Community: pq.StringArray{"north"}}
	if err := db.Create(user).Error; err != nil {
		t.Fatal(err)
	}
	record := &file.File{Filename: "baseline.csv", InsertedBy: uint(user.ID), Version: 1}
	if err := db.Create(record).Error; err != nil {
		t.Fatal(err)
	}
	return db, user, record
}

func fixtureCookie(t testing.TB, userID int) *http.Cookie {
	t.Helper()
	t.Setenv("JWT_SECRET", "synthetic-fixture-key")
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id": userID, "exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString([]byte("synthetic-fixture-key"))
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: "access_token", Value: token}
}

func TestPostgres_FileListingPreservesAccessAndUserJoin(t *testing.T) {
	db, user, record := recordsFixture(t)
	private := file.File{Filename: "private.csv", Private: true, InsertedBy: uint(user.ID), Version: 1}
	if err := db.Create(&private).Error; err != nil {
		t.Fatal(err)
	}
	service := &file.FileService{DB: db}
	for _, granted := range []bool{false, true} {
		if granted {
			if err := db.Exec(`INSERT INTO file_access (user_id, file_id) VALUES (?, ?)`, user.ID, private.ID).Error; err != nil {
				t.Fatal(err)
			}
		}
		gin.SetMode(gin.TestMode)
		router := gin.New()
		file.RegisterRoutes(router, service, &logs.LogService{DB: db})
		request := httptest.NewRequest(http.MethodGet, "/api/file", nil)
		request.AddCookie(fixtureCookie(t, user.ID))
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		var body struct {
			Message string              `json:"message"`
			Files   []file.FileWithUser `json:"files"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		want := 1
		if granted {
			want = 2
		}
		if response.Code != http.StatusOK || body.Message != "Files fetched successfully" || len(body.Files) != want {
			t.Fatalf("file contract changed: status=%d, count=%d", response.Code, len(body.Files))
		}
		found := false
		for _, got := range body.Files {
			if got.ID == record.ID && got.Firstname == user.FirstName && got.Lastname == user.LastName {
				found = true
			}
		}
		if !found {
			t.Fatal("uploader join or public file missing")
		}
	}
}

func TestPostgres_FileHistoryPreservesVersionAndUploader(t *testing.T) {
	db, user, record := recordsFixture(t)
	if err := db.Create(&file.FileVersion{FileID: record.ID, Filename: record.Filename, Version: 1, InsertedBy: uint(user.ID), Rows: 2}).Error; err != nil {
		t.Fatal(err)
	}
	history, err := (&file.FileService{DB: db}).GetFileHistory(strconv.FormatUint(uint64(record.ID), 10))
	if err != nil || len(history) != 1 {
		t.Fatalf("history failed: %v, count=%d", err, len(history))
	}
	if history[0].Version != 1 || history[0].Firstname != user.FirstName || history[0].Rows != 2 {
		t.Fatal("history contract changed")
	}
}

func TestPostgres_FileReplacementBlockedByOpenRequests(t *testing.T) {
	for _, tc := range []struct{ name, statement string }{
		{"edit request", `INSERT INTO file_edit_request (file_id, user_id, row_id, status) VALUES (?, 1, 1, 'pending')`},
		{"form request", `INSERT INTO form_submissions (file_id, status) VALUES (?, 'needs more information')`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, user, record := recordsFixture(t)
			// Use the model's table name instead of assuming its pluralization.
			statement := strings.ReplaceAll(tc.statement, "file_edit_request ", (file.FileEditRequest{}).TableName()+" ")
			if err := db.Exec(statement, record.ID).Error; err != nil {
				t.Fatal(err)
			}
			err := (&file.FileService{DB: db}).ReplaceFiles(nil, record.ID, uint(user.ID))
			if !errors.Is(err, file.ErrFileOperationBlocked) {
				t.Fatalf("open request failed to block replacement: %v", err)
			}
			var unchanged file.File
			if err := db.First(&unchanged, record.ID).Error; err != nil {
				t.Fatal(err)
			}
			if unchanged.Version != 1 {
				t.Fatal("blocked operation modified the file")
			}
		})
	}
}

func TestPostgres_AuditRedactionSearchArraysAndAggregates(t *testing.T) {
	db, user, record := recordsFixture(t)
	service := &logs.LogService{DB: db}
	userID := uint(user.ID)
	if err := service.Log(logs.SystemLog{
		Level: "INFO", Service: "auth", Action: "LOGIN", Message: "Synthetic login",
		UserID: &userID, Filename: &record.Filename, Communities: pq.StringArray{"north", "south"},
	}, map[string]any{"password": "password-canary", "nested": map[string]any{"api_key": "api-key-canary"}, "request_id": "test-1"}); err != nil {
		t.Fatal(err)
	}
	var stored logs.SystemLog
	if err := db.First(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Metadata == nil || strings.Contains(*stored.Metadata, "canary") || !strings.Contains(*stored.Metadata, "test-1") {
		t.Fatal("audit metadata was not safely persisted")
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	logs.RegisterRoutes(router, service)
	request := httptest.NewRequest(http.MethodPost, "/api/logs", bytes.NewBufferString(`{"search":"Synthetic","communities":["north"],"page":1,"page_size":20}`))
	request.Header.Set("Content-Type", "application/json")
	request.AddCookie(fixtureCookie(t, user.ID))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	var body struct {
		Data       []logs.LogRow      `json:"data"`
		Total      int64              `json:"total"`
		Aggregates logs.LogAggregates `json:"aggregates"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || body.Total != 1 || len(body.Data) != 1 || len(body.Aggregates.ByCommunity) != 2 || len(body.Aggregates.ByPerson) != 1 || len(body.Aggregates.ByFilename) != 1 {
		t.Fatalf("audit filtering/aggregation contract changed: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestPostgres_FixturesUseSeparateSchemas(t *testing.T) {
	first, second := newPostgresFixture(t), newPostgresFixture(t)
	if err := first.Exec(`CREATE TABLE isolation_marker (id integer)`).Error; err != nil {
		t.Fatal(err)
	}
	var visible bool
	if err := second.Raw(`SELECT to_regclass('isolation_marker') IS NOT NULL`).Scan(&visible).Error; err != nil {
		t.Fatal(err)
	}
	if visible {
		t.Fatal("test fixtures share a schema")
	}
}

func BenchmarkPostgres_FileListing(b *testing.B) {
	db, user, _ := recordsFixture(b)
	service := &file.FileService{DB: db}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := service.GetAllFiles(uint(user.ID), "User"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPostgres_AuditSearch(b *testing.B) {
	db, user, _ := recordsFixture(b)
	userID := uint(user.ID)
	service := &logs.LogService{DB: db}
	if err := service.Log(logs.SystemLog{Level: "INFO", Service: "test", Action: "BASELINE", Message: "Synthetic", UserID: &userID, Communities: pq.StringArray{"north"}}, nil); err != nil {
		b.Fatal(err)
	}
	input := logs.LogFilterInput{Communities: []string{"north"}, Page: 1, PageSize: 20}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, _, _, err := service.GetLogs(input); err != nil {
			b.Fatal(err)
		}
	}
}
