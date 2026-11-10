package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"nordik-drive-api/internal/mailer"
)

func TestSignupHTTPVerificationRequiredAndNoEarlyAccountOrCookies(t *testing.T) {
	verifier, _ := signupFixture(t)
	// Match Notifications-mode wiring: an absent SMTP pointer is held in the
	// mailer interface while all email is produced through the outbox.
	service := &AuthService{DB: verifier.DB, Outbox: verifier.Outbox, Signup: verifier, RequireVerified: true, Mailer: (*mailer.Service)(nil)}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterSignupOTP(r, service)
	controller := &AuthController{AuthService: service, LS: &mockLogService{}}
	r.POST("/api/user/signup", controller.SignUp)
	post := func(path, body, key string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", key)
		request.Header.Set("X-Nordik-Client-IP", "192.0.2.1")
		response := httptest.NewRecorder()
		r.ServeHTTP(response, request)
		return response
	}
	user := `{"firstname":"New","lastname":"User","email":"new@example.test","password":"test-pass-123"}`
	if w := post("/api/user/signup", user, uuid.NewString()); w.Code != 400 {
		t.Fatal("direct signup bypassed verification", w.Code)
	}
	w := post("/api/user/signup/otp/request", `{"email":"new@example.test"}`, uuid.NewString())
	if w.Code != 202 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Code, w.Body.String())
	}
	var sent SignupResult
	_ = json.Unmarshal(w.Body.Bytes(), &sent)
	var count int64
	verifier.DB.Model(&Auth{}).Count(&count)
	if count != 0 || len(w.Result().Cookies()) != 0 {
		t.Fatal("request created an account or login session")
	}
	verifyBody := `{"email":"new@example.test","challenge_id":"` + sent.ChallengeID + `","otp":"` + signupCode(t, verifier, sent.ChallengeID) + `"}`
	w = post("/api/user/signup/otp/verify", verifyBody, uuid.NewString())
	if w.Code != 200 || len(w.Result().Cookies()) != 0 {
		t.Fatal("verify response issued login session or failed", w.Code, w.Body.String())
	}
	var proof SignupResult
	_ = json.Unmarshal(w.Body.Bytes(), &proof)
	verifier.DB.Model(&Auth{}).Count(&count)
	if count != 0 {
		t.Fatal("verify created an account")
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(user), &body)
	body["signup_verification_token"] = proof.Token
	encoded, _ := json.Marshal(body)
	key := uuid.NewString()
	for range 2 {
		if w := post("/api/user/signup", string(encoded), key); w.Code != 201 || len(w.Result().Cookies()) != 0 {
			t.Fatal("idempotent final signup failed", w.Code, w.Body.String())
		}
	}
	verifier.DB.Model(&Auth{}).Count(&count)
	if count != 1 {
		t.Fatal("duplicate signup account")
	}
	if w := post("/api/user/signup/otp/request", `{"email":"new@example.test"}`, uuid.NewString()); w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatal("cooldown HTTP contract failed", w.Code)
	}
	if w := post("/api/user/signup/otp/verify", `{"email":"new@example.test"}`, uuid.NewString()); w.Code != 400 {
		t.Fatal("malformed verify body accepted")
	}
}
