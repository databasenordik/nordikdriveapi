package activity

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMetadataRedactsCredentialsWithoutMutatingInput(t *testing.T) {
	metadata := map[string]any{
		"email": "synthetic@example.com", "request_id": "request-1",
		"Password": "password-canary", "new_password": "new-password-canary",
		"nested": []any{map[string]any{
			"OTP": "123456-canary", "access-token": "token-canary",
			"X_API_KEY": "key-canary", "clientSecret": "secret-canary",
			"Authorization": "Bearer authorization-canary", "Cookie": "cookie-canary",
			"connection_string": "connection-canary", "private_key": "private-canary",
		}},
		"record_id": json.Number("9007199254740993"),
	}
	result := SafeMetadata(metadata)
	if result == nil || strings.Contains(*result, "canary") {
		t.Fatal("secret material leaked")
	}
	if !strings.Contains(*result, "9007199254740993") || !strings.Contains(*result, "synthetic@example.com") {
		t.Fatal("non-secret metadata changed")
	}
	if metadata["Password"] != "password-canary" {
		t.Fatal("redaction mutated caller metadata")
	}
	if metadata["nested"].([]any)[0].(map[string]any)["OTP"] != "123456-canary" {
		t.Fatal("redaction mutated nested caller metadata")
	}
}

func TestSafeMetadata_StructAndUnsafePayloads(t *testing.T) {
	type payload struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	result := SafeMetadata(payload{Email: "synthetic@example.com", Password: "canary"})
	if result == nil || strings.Contains(*result, "canary") || !strings.Contains(*result, "synthetic@example.com") {
		t.Fatal("struct metadata was not safely retained")
	}
	cycle := map[string]any{}
	cycle["self"] = cycle
	for _, input := range []any{nil, func() {}, cycle, "unstructured-password-canary", json.RawMessage(`invalid`)} {
		if SafeMetadata(input) != nil {
			t.Fatalf("unsafe metadata of type %T must be omitted", input)
		}
	}
}
