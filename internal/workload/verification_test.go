package workload

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/api/idtoken"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSignedWorkloadTokenValidation(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks, _ := json.Marshal(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "fixture", "alg": "RS256", "use": "sig", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB"}}})
	validator, err := idtoken.NewValidator(context.Background(), idtoken.WithHTTPClient(&http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Cache-Control": []string{"max-age=3600"}}, Body: io.NopCloser(strings.NewReader(string(jwks)))}, nil
	})}))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, audience, email, issuer string
		verified, expired, allowed    bool
	}{
		{"valid", "https://service", "operator@example.iam.gserviceaccount.com", "https://accounts.google.com", true, false, true},
		{"expired", "https://service", "operator@example.iam.gserviceaccount.com", "https://accounts.google.com", true, true, false},
		{"wrong audience", "https://other", "operator@example.iam.gserviceaccount.com", "https://accounts.google.com", true, false, false},
		{"wrong caller", "https://service", "intruder@example.iam.gserviceaccount.com", "https://accounts.google.com", true, false, false},
		{"unverified email", "https://service", "operator@example.iam.gserviceaccount.com", "https://accounts.google.com", false, false, false},
		{"wrong issuer", "https://service", "operator@example.iam.gserviceaccount.com", "https://invalid.example", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expiry := time.Now().Add(time.Hour)
			if tc.expired {
				expiry = time.Now().Add(-time.Hour)
			}
			token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": tc.issuer, "aud": tc.audience, "sub": "fixture-service-account", "iat": time.Now().Unix(), "exp": expiry.Unix(), "email": tc.email, "email_verified": tc.verified})
			token.Header["kid"] = "fixture"
			encoded, err := token.SignedString(key)
			if err != nil {
				t.Fatal(err)
			}
			account, err := verifiedAccount(context.Background(), encoded, "https://service", map[string]bool{"operator@example.iam.gserviceaccount.com": true}, validator.Validate)
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v, error=%v", err == nil, err)
			}
			if tc.allowed && account != tc.email {
				t.Fatal("lost verified actor")
			}
		})
	}
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	forged := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"aud": "https://service", "exp": time.Now().Add(time.Hour).Unix()})
	forged.Header["kid"] = "fixture"
	encoded, _ := forged.SignedString(other)
	if _, err := verifiedAccount(context.Background(), encoded, "https://service", nil, validator.Validate); err == nil {
		t.Fatal("forged signature admitted")
	}
}
