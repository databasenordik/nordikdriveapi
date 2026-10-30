package logs

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode"
)

// safeMetadata copies structured metadata through JSON before redacting it.
// Callers must still allowlist fields and keep secrets out of free-text messages.
// Unserializable or unstructured payloads are omitted without losing the event.
func safeMetadata(metadata any) *string {
	if metadata == nil {
		return nil
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil
	}
	switch value.(type) {
	case map[string]any, []any:
	default:
		return nil
	}
	redactMetadata(value)
	encoded, err = json.Marshal(value)
	if err != nil {
		return nil
	}
	result := string(encoded)
	return &result
}

func redactMetadata(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			if sensitiveMetadataKey(key) {
				typed[key] = "[REDACTED]"
			} else {
				redactMetadata(item)
			}
		}
	case []any:
		for _, item := range typed {
			redactMetadata(item)
		}
	}
}

func sensitiveMetadataKey(key string) bool {
	key = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, key)
	for _, fragment := range []string{"password", "secret", "token", "apikey", "privatekey", "credential"} {
		if strings.Contains(key, fragment) {
			return true
		}
	}
	switch key {
	case "pass", "pwd", "otp", "otpcode", "onetimecode", "onetimepassword", "authorization", "proxyauthorization", "cookie", "setcookie", "dsn", "connectionstring":
		return true
	}
	return false
}
