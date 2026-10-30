package session

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestValidate_RejectsUnsafeTokens(t *testing.T) {
	for _, tc := range []struct {
		name   string
		id     any
		method jwt.SigningMethod
		exp    any
		secret string
	}{
		{"missing user", nil, jwt.SigningMethodHS256, time.Now().Add(time.Hour).Unix(), "test-key"},
		{"zero", 0, jwt.SigningMethodHS256, time.Now().Add(time.Hour).Unix(), "test-key"},
		{"negative", -1, jwt.SigningMethodHS256, time.Now().Add(time.Hour).Unix(), "test-key"},
		{"fraction", 1.5, jwt.SigningMethodHS256, time.Now().Add(time.Hour).Unix(), "test-key"},
		{"unsafe precision", json.Number("9007199254740993"), jwt.SigningMethodHS256, time.Now().Add(time.Hour).Unix(), "test-key"},
		{"overflow", "18446744073709551616", jwt.SigningMethodHS256, time.Now().Add(time.Hour).Unix(), "test-key"},
		{"NaN", "NaN", jwt.SigningMethodHS256, time.Now().Add(time.Hour).Unix(), "test-key"},
		{"infinity", "Inf", jwt.SigningMethodHS256, time.Now().Add(time.Hour).Unix(), "test-key"},
		{"decimal string", "1.5", jwt.SigningMethodHS256, time.Now().Add(time.Hour).Unix(), "test-key"},
		{"object", map[string]any{"id": 1}, jwt.SigningMethodHS256, time.Now().Add(time.Hour).Unix(), "test-key"},
		{"missing expiration", 1, jwt.SigningMethodHS256, nil, "test-key"},
		{"expired", 1, jwt.SigningMethodHS256, time.Now().Add(-time.Minute).Unix(), "test-key"},
		{"wrong algorithm", 1, jwt.SigningMethodHS384, time.Now().Add(time.Hour).Unix(), "test-key"},
		{"empty key", 1, jwt.SigningMethodHS256, time.Now().Add(time.Hour).Unix(), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := jwt.MapClaims{"user_id": tc.id}
			if tc.exp != nil {
				claims["exp"] = tc.exp
			}
			encoded, err := jwt.NewWithClaims(tc.method, claims).SignedString([]byte(tc.secret))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Validate(encoded, tc.secret); err == nil {
				t.Fatal("unsafe session accepted")
			}
		})
	}
}

func TestValidate_PreservesLegacyClaims(t *testing.T) {
	for _, id := range []any{42, "42"} {
		encoded, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"user_id": id, "exp": time.Now().Add(time.Hour).Unix(),
			"communities": []any{"north", "", 123, "south"},
		}).SignedString([]byte("test-key"))
		if err != nil {
			t.Fatal(err)
		}
		claims, err := Validate(encoded, "test-key")
		if err != nil || claims.UserID != 42 || strings.Join(claims.Communities, ",") != "north,south" {
			t.Fatalf("legacy claims rejected: %+v, %v", claims, err)
		}
	}
}
