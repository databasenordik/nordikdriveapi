//go:build integration
// +build integration

package integration

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"nordik-drive-api/internal/auth"
	"nordik-drive-api/internal/role"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

type mockRoleService struct {
	getRoleByUserFn    func(userid int) ([]auth.UserRole, error)
	getRolesByUserIdFn func(userid int) ([]auth.UserRole, error)
	getAllRolesFn      func(uniqueRoles []string) ([]role.Role, error)
	lastUniqueRolesArg []string
}

func (m *mockRoleService) GetRoleByUser(userid int) ([]auth.UserRole, error) {
	if m.getRoleByUserFn == nil {
		return nil, nil
	}
	return m.getRoleByUserFn(userid)
}

func (m *mockRoleService) GetRolesByUserId(userid int) ([]auth.UserRole, error) {
	if m.getRolesByUserIdFn == nil {
		return nil, nil
	}
	return m.getRolesByUserIdFn(userid)
}

func (m *mockRoleService) GetAllRoles(uniqueRoles []string) ([]role.Role, error) {
	m.lastUniqueRolesArg = append([]string(nil), uniqueRoles...)
	if m.getAllRolesFn == nil {
		return nil, nil
	}
	return m.getAllRolesFn(uniqueRoles)
}

func setupControllerRouter(t *testing.T, svc role.RoleServiceAPI, withUserID bool, userID any) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	r := gin.New()
	rc := &role.RoleController{RoleService: svc}

	// We mount routes WITHOUT the real AuthMiddleware so we can control c.Set("userID", ...)
	r.GET("/api/role", func(c *gin.Context) {
		if withUserID {
			c.Set("userID", userID)
		}
		rc.GetAllRoles(c)
	})

	r.GET("/api/role/user", func(c *gin.Context) {
		if withUserID {
			c.Set("userID", userID)
		}
		rc.GetRolesByUserId(c)
	})

	return r
}

func doReq(r http.Handler, method, path string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

/* -----------------------------
   Controller tests (mock service)
------------------------------ */

func TestRoleController_GetAllRoles_401_UserIDMissing(t *testing.T) {
	svc := &mockRoleService{}
	r := setupControllerRouter(t, svc, false, nil)

	w := doReq(r, http.MethodGet, "/api/role", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestRoleController_GetAllRoles_401_UserIDWrongType(t *testing.T) {
	svc := &mockRoleService{}
	r := setupControllerRouter(t, svc, true, "123") // should be float64 per your controller

	w := doReq(r, http.MethodGet, "/api/role", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestRoleController_GetAllRoles_500_WhenGetRoleByUserFails(t *testing.T) {
	svc := &mockRoleService{
		getRoleByUserFn: func(userid int) ([]auth.UserRole, error) {
			return nil, errors.New("db down")
		},
	}
	r := setupControllerRouter(t, svc, true, float64(7))

	w := doReq(r, http.MethodGet, "/api/role", nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestRoleController_GetAllRoles_500_WhenGetAllRolesFails(t *testing.T) {
	svc := &mockRoleService{
		getRoleByUserFn: func(userid int) ([]auth.UserRole, error) {
			// include duplicates; controller must de-dupe
			return []auth.UserRole{
				{Role: "admin"},
				{Role: "admin"},
				{Role: "viewer"},
			}, nil
		},
		getAllRolesFn: func(uniqueRoles []string) ([]role.Role, error) {
			return nil, errors.New("query failed")
		},
	}
	r := setupControllerRouter(t, svc, true, float64(10))

	w := doReq(r, http.MethodGet, "/api/role", nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestRoleController_GetAllRoles_200_Success_AndPassesUniqueRoles(t *testing.T) {
	mock := &mockRoleService{
		getRoleByUserFn: func(userid int) ([]auth.UserRole, error) {
			// duplicates to test de-dupe
			return []auth.UserRole{
				{Role: "admin"},
				{Role: "admin"},
				{Role: "editor"},
			}, nil
		},
		getAllRolesFn: func(uniqueRoles []string) ([]role.Role, error) {
			// Return some roles to prove success path
			return []role.Role{
				{ID: 1, Role: "viewer", Priority: 3, CanView: true},
			}, nil
		},
	}
	r := setupControllerRouter(t, mock, true, float64(99))

	w := doReq(r, http.MethodGet, "/api/role", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}

	// Verify response schema
	var resp struct {
		Message string      `json:"message"`
		Roles   []role.Role `json:"roles"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal failed: %v body=%s", err, w.Body.String())
	}
	if resp.Message != "Roles fetched successfully" {
		t.Fatalf("unexpected message: %q", resp.Message)
	}
	if len(resp.Roles) != 1 || resp.Roles[0].Role != "viewer" {
		t.Fatalf("unexpected roles payload: %+v", resp.Roles)
	}

	// Ensure controller passed de-duped uniqueRoles into GetAllRoles
	// Order is not guaranteed (map iteration), so compare as a set.
	set := map[string]bool{}
	for _, r := range mock.lastUniqueRolesArg {
		set[r] = true
	}
	if !(set["admin"] && set["editor"]) || len(set) != 2 {
		t.Fatalf("expected unique roles {admin, editor}, got=%v", mock.lastUniqueRolesArg)
	}
}

func TestRoleController_GetRolesByUserId_401_UserIDMissing(t *testing.T) {
	svc := &mockRoleService{}
	r := setupControllerRouter(t, svc, false, nil)

	w := doReq(r, http.MethodGet, "/api/role/user", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestRoleController_GetRolesByUserId_401_UserIDWrongType(t *testing.T) {
	svc := &mockRoleService{}
	r := setupControllerRouter(t, svc, true, int(5))

	w := doReq(r, http.MethodGet, "/api/role/user", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestRoleController_GetRolesByUserId_500_WhenServiceFails(t *testing.T) {
	svc := &mockRoleService{
		getRolesByUserIdFn: func(userid int) ([]auth.UserRole, error) {
			return nil, errors.New("boom")
		},
	}
	r := setupControllerRouter(t, svc, true, float64(2))

	w := doReq(r, http.MethodGet, "/api/role/user", nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestRoleController_GetRolesByUserId_200_Success(t *testing.T) {
	svc := &mockRoleService{
		getRolesByUserIdFn: func(userid int) ([]auth.UserRole, error) {
			return []auth.UserRole{
				{UserID: userid, Role: "admin"},
				{UserID: userid, Role: "viewer"},
			}, nil
		},
	}
	r := setupControllerRouter(t, svc, true, float64(12))

	w := doReq(r, http.MethodGet, "/api/role/user", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}

	var resp struct {
		Message string          `json:"message"`
		Roles   []auth.UserRole `json:"roles"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal failed: %v body=%s", err, w.Body.String())
	}
	if resp.Message != "Roles fetched successfully" {
		t.Fatalf("unexpected message: %q", resp.Message)
	}
	if len(resp.Roles) != 2 {
		t.Fatalf("expected 2 roles, got %d", len(resp.Roles))
	}
}

/* -----------------------------
   Service tests (sqlite + gorm)
------------------------------ */

func openTestDB(t *testing.T, migrate bool) *gorm.DB {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "integration.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}

	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	if migrate {
		// auth.UserRole must be a real model struct in your project.
		if err := db.AutoMigrate(&role.Role{}, &auth.UserRole{}); err != nil {
			t.Fatalf("failed to migrate: %v", err)
		}
	}
	return db
}

func TestRoleService_GetAllRoles_EmptyUniqueRoles_ReturnsEmpty(t *testing.T) {
	db := openTestDB(t, true)
	rs := &role.RoleService{DB: db}

	roles, err := rs.GetAllRoles(nil)
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if len(roles) != 0 {
		t.Fatalf("expected 0 roles, got %d", len(roles))
	}
}

func TestRoleService_GetAllRoles_ExcludesUniqueRoles(t *testing.T) {
	db := openTestDB(t, true)
	rs := &role.RoleService{DB: db}

	seed := []role.Role{
		{Role: "admin", Priority: 1, CanUpload: true, CanView: true, CanApprove: true, CanApproveAll: true},
		{Role: "viewer", Priority: 3, CanUpload: false, CanView: true, CanApprove: false, CanApproveAll: false},
		{Role: "editor", Priority: 2, CanUpload: true, CanView: true, CanApprove: false, CanApproveAll: false},
	}
	if err := db.Create(&seed).Error; err != nil {
		t.Fatalf("seed failed: %v", err)
	}

	roles, err := rs.GetAllRoles([]string{"admin", "editor"})
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}

	// Should return only "viewer"
	if len(roles) != 1 || roles[0].Role != "viewer" {
		t.Fatalf("expected [viewer], got %+v", roles)
	}
}

func TestRoleService_GetRolesByUserId_ReturnsRows(t *testing.T) {
	db := openTestDB(t, true)
	rs := &role.RoleService{DB: db}

	seed := []auth.UserRole{
		{UserID: 55, Role: "admin"},
		{UserID: 55, Role: "viewer"},
		{UserID: 99, Role: "editor"},
	}
	if err := db.Create(&seed).Error; err != nil {
		t.Fatalf("seed user_roles failed: %v", err)
	}

	roles, err := rs.GetRolesByUserId(55)
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if len(roles) != 2 {
		t.Fatalf("expected 2 roles, got %d", len(roles))
	}
}

func TestRoleService_GetRolesByUserId_DBError_NoMigration(t *testing.T) {
	db := openTestDB(t, false) // no tables
	rs := &role.RoleService{DB: db}

	_, err := rs.GetRolesByUserId(1)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
}

func TestRoleService_GetAllRoles_DBError_NoMigration(t *testing.T) {
	db := openTestDB(t, false) // no tables
	rs := &role.RoleService{DB: db}

	_, err := rs.GetAllRoles([]string{"admin"})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
}

/* -----------------------------
   role.RegisterRoutes coverage
------------------------------ */

func TestRole_RegisterRoutes_CoverageOnly(t *testing.T) {
	// We only want to execute role.RegisterRoutes lines for coverage.
	// We are NOT testing AuthMiddleware here (that's in middlewares package).
	gin.SetMode(gin.TestMode)
	r := gin.New()

	// DB can be nil because we won't actually serve requests through these routes.
	role.RegisterRoutes(r, &role.RoleService{DB: nil})

	// If we reached here without panic, routes registration is covered.
}
