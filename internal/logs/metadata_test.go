package logs

import (
	"database/sql/driver"
	"encoding/json"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestLogService_RedactsCredentialsBeforeInsert(t *testing.T) {
	db, mock, cleanup := newMockGorm(t)
	defer cleanup()
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
	mock.ExpectQuery(`INSERT INTO "logs"`).WithArgs(
		"INFO", "auth", sqlmock.AnyArg(), "LOGIN", "User logged in",
		sqlmock.AnyArg(), sqlmock.AnyArg(), credentialSafeMetadata{}, sqlmock.AnyArg(),
	).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
	if err := (&LogService{DB: db}).Log(SystemLog{
		Level: "INFO", Service: "auth", Action: "LOGIN", Message: "User logged in",
	}, metadata); err != nil {
		t.Fatalf("insert safe audit event: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if metadata["Password"] != "password-canary" {
		t.Fatal("redaction mutated caller metadata")
	}
	if metadata["nested"].([]any)[0].(map[string]any)["OTP"] != "123456-canary" {
		t.Fatal("redaction mutated nested caller metadata")
	}
}

type credentialSafeMetadata struct{}

func (credentialSafeMetadata) Match(value driver.Value) bool {
	encoded, ok := value.(string)
	if !ok || strings.Contains(encoded, "canary") {
		return false
	}
	var decoded map[string]any
	if json.Unmarshal([]byte(encoded), &decoded) != nil {
		return false
	}
	return decoded["email"] == "synthetic@example.com" && decoded["request_id"] == "request-1" &&
		decoded["Password"] == "[REDACTED]" && strings.Contains(encoded, "9007199254740993")
}

func TestSafeMetadata_StructAndUnsafePayloads(t *testing.T) {
	type payload struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	result := safeMetadata(payload{Email: "synthetic@example.com", Password: "canary"})
	if result == nil || strings.Contains(*result, "canary") || !strings.Contains(*result, "synthetic@example.com") {
		t.Fatal("struct metadata was not safely retained")
	}
	cycle := map[string]any{}
	cycle["self"] = cycle
	for _, input := range []any{nil, func() {}, cycle, "unstructured-password-canary", json.RawMessage(`invalid`)} {
		if safeMetadata(input) != nil {
			t.Fatalf("unsafe metadata of type %T must be omitted", input)
		}
	}
}
