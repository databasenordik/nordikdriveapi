package auth

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"nordik-drive-api/internal/logs"
)

func TestAuthAudit_UsesOnlySafeMetadata(t *testing.T) {
	t.Setenv("JWT_SECRET", "synthetic-test-secret")
	const password = "password-canary-123"
	const otp = "987654"
	user := &Auth{ID: 42, Email: "synthetic@example.com", Password: hashPassword(t, password)}
	for _, tc := range []struct {
		path, body, action string
		status             int
	}{
		{"/login", `{"email":"synthetic@example.com","password":"` + password + `","rememberMe":true}`, "LOGIN", http.StatusOK},
		{"/verify-password", `{"password":"` + password + `"}`, "PASSWORD_VERIFICATION", http.StatusOK},
		{"/reset-password", `{"email":"synthetic@example.com","otp":"` + otp + `","password":"` + password + `"}`, "RESET_PASSWORD", http.StatusOK},
		{"/send-otp", `{"email":"synthetic@example.com"}`, "SEND_OTP", http.StatusOK},
		{"/signup", `{"firstname":"Test","lastname":"User","email":"synthetic@example.com","password":"` + password + `"}`, "SIGNUP", http.StatusCreated},
	} {
		t.Run(tc.action, func(t *testing.T) {
			called := false
			ac := &AuthController{
				AuthService: &mockAuthService{
					CreateUserFn:    func(Auth) (*Auth, error) { return user, nil },
					GetUserFn:       func(string) (*Auth, error) { return user, nil },
					GetUserByIDFn:   func(int) (*Auth, error) { return user, nil },
					SendOTPFn:       func(string) (*Auth, string, error) { return user, otp, nil },
					ResetPasswordFn: func(string, string, string) (*Auth, error) { return user, nil },
				},
				LS: &mockLogService{LogFn: func(entry logs.SystemLog, metadata any) error {
					called = true
					if entry.Action != tc.action || entry.Service != "auth" || entry.UserID == nil || *entry.UserID != 42 {
						t.Fatal("audit event identity changed")
					}
					encoded, err := json.Marshal(metadata)
					if err != nil {
						t.Fatal(err)
					}
					for _, secret := range []string{password, otp, user.Password} {
						if strings.Contains(string(encoded), secret) || strings.Contains(entry.Message, secret) {
							t.Fatal("credential reached audit logger")
						}
					}
					var fields map[string]any
					if err := json.Unmarshal(encoded, &fields); err != nil {
						t.Fatal(err)
					}
					if fields["email"] != user.Email {
						t.Fatal("safe account context missing")
					}
					for key := range fields {
						if key != "email" && key != "rememberMe" && key != "firstname" && key != "lastname" {
							t.Fatalf("unexpected auth metadata field %q", key)
						}
					}
					return nil
				}},
			}
			w := postJSONWithHeader(setupAuthRouter(ac), tc.path, []byte(tc.body), "X-UserID", "42")
			if w.Code != tc.status || !called {
				t.Fatalf("status=%d, audit called=%v", w.Code, called)
			}
		})
	}
}
