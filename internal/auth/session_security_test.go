package auth

import (
	"net/http"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestSessionEndpoints_RejectMalformedClaimsWithoutPanic(t *testing.T) {
	t.Setenv("JWT_SECRET", "test-key")
	for _, tc := range []struct {
		name   string
		claims jwt.MapClaims
	}{
		{"missing ID", jwt.MapClaims{"exp": time.Now().Add(time.Hour).Unix()}},
		{"fractional ID", jwt.MapClaims{"user_id": 1.5, "exp": time.Now().Add(time.Hour).Unix()}},
		{"missing expiration", jwt.MapClaims{"user_id": 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, tc.claims).SignedString([]byte("test-key"))
			if err != nil {
				t.Fatal(err)
			}
			router := setupAuthRouter(&AuthController{AuthService: &mockAuthService{}})
			for _, route := range []struct{ method, path, cookie string }{
				{http.MethodGet, "/me", "access_token"},
				{http.MethodPost, "/refresh", "refresh_token"},
			} {
				response := doReq(router, route.method, route.path, &http.Cookie{Name: route.cookie, Value: token})
				if response.Code != http.StatusUnauthorized {
					t.Fatalf("%s returned %d", route.path, response.Code)
				}
			}
		})
	}
}
