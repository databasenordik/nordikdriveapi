package database

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
)

var runtimeLabel = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// ApplicationName is also implemented by the rollout coordinator. PostgreSQL
// truncates application_name at 63 bytes; hash long names instead of allowing
// distinct revisions to become indistinguishable during drain inspection.
func ApplicationName(role, revision, execution string) (string, error) {
	kind, value := "local:", role
	if execution != "" {
		kind, value = "job:", execution
	} else if revision != "" {
		kind, value = "run:", revision
	}
	if value == "" {
		value = "monolith"
	}
	if !runtimeLabel.MatchString(value) {
		return "", errors.New("invalid database runtime label")
	}
	if len(kind)+len(value) <= 63 {
		return kind + value, nil
	}
	digest := sha256.Sum256([]byte(value))
	return kind + value[:32] + ":" + hex.EncodeToString(digest[:12]), nil
}

// Only these multi-schema paths are permitted during the staged owner move.
// Single schemas remain supported for isolated PostgreSQL integration fixtures.
func validatedSearchPath(schema string) (string, error) {
	if schema == "" {
		return "public", nil
	}
	if regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`).MatchString(schema) {
		return schema, nil
	}
	switch schema {
	case "public,identity", "identity,public", "public,identity,audit", "audit,identity", "audit,public,identity":
		return schema, nil
	}
	return "", errors.New("DB_SCHEMA is not a validated schema or transitional search path")
}
