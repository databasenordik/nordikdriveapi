// Package activity defines the versioned, redacted activity exchanged by owners.
package activity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Log preserves the historical logs table and its public representation.
type Log struct {
	ID          uint           `gorm:"primaryKey;autoIncrement" json:"id"`
	Level       string         `gorm:"size:20;not null" json:"level"`
	Service     string         `gorm:"size:100;not null" json:"service"`
	UserID      *uint          `gorm:"index" json:"user_id,omitempty"`
	Action      string         `gorm:"size:255;not null" json:"action"`
	Message     string         `gorm:"type:text;not null" json:"message"`
	Filename    *string        `gorm:"size:512" json:"filename,omitempty"`
	Communities pq.StringArray `gorm:"type:text[];column:communities" json:"communities"`
	Metadata    *string        `gorm:"type:jsonb" json:"metadata,omitempty"`
	CreatedAt   time.Time      `gorm:"autoCreateTime" json:"created_at"`
}

func (Log) TableName() string { return "logs" }

type Event struct {
	ID        string `json:"event_id"`
	Version   int    `json:"version"`
	Producer  string `json:"producer"`
	Log       Log    `json:"activity"`
	RequestID string `json:"request_id,omitempty"`
	TraceID   string `json:"trace_id,omitempty"`
}

func (e Event) Validate() error {
	id, err := uuid.Parse(e.ID)
	if err != nil || id.Version() != 4 || e.Version != 1 || (e.Producer != "identity" && e.Producer != "legacy") || e.Log.ID != 0 || e.Log.CreatedAt.IsZero() || e.Log.Level == "" || e.Log.Action == "" || len(e.Log.Action) > 255 || len(e.RequestID) > 128 || (e.TraceID != "" && len(e.TraceID) != 32) {
		return errors.New("invalid audit event")
	}
	if (e.Producer == "identity" && e.Log.Service != "auth") || (e.Producer == "legacy" && e.Log.Service != "file") {
		return errors.New("audit event owner mismatch")
	}
	if !validAction(e.Log.Service, e.Log.Action) {
		return errors.New("unsupported audit activity action")
	}
	if e.Log.Metadata != nil {
		var metadata any
		if json.Unmarshal([]byte(*e.Log.Metadata), &metadata) != nil {
			return errors.New("invalid audit metadata")
		}
		safe := SafeMetadata(metadata)
		if safe == nil || *safe != *e.Log.Metadata {
			return errors.New("audit metadata must be redacted")
		}
	}
	return nil
}

func validAction(service, action string) bool {
	if service == "auth" {
		switch action {
		case "SIGNUP", "LOGIN", "PASSWORD_VERIFICATION", "SEND_OTP", "RESET_PASSWORD":
			return true
		}
		return false
	}
	switch action {
	case "UPLOAD_FILE", "ACCESS_FILE", "ACCESS_NORMALIZED_FILE", "SYNC_NORMALIZED_FILE", "DELETE_FILE", "RESTORE_FILE", "GRAND_FILE_ACCESS", "REVOKE_FILE_ACCESS", "REPLACE_FILE", "REVERT_FILE":
		return true
	}
	return false
}
func (e Event) Fingerprint() string {
	b, _ := json.Marshal(e)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type Actor struct {
	ID          uint
	Communities []string
}
type actorKey struct{}

func WithActor(ctx context.Context, id uint, communities []string) context.Context {
	return context.WithValue(ctx, actorKey{}, Actor{id, append([]string(nil), communities...)})
}
func ActorFrom(ctx context.Context) Actor { actor, _ := ctx.Value(actorKey{}).(Actor); return actor }

// Transactional identifies actions whose producer has already recorded an event
// in the mutation transaction. Read-only activity is still recorded by handlers.
func Transactional(action string) bool {
	switch action {
	case "SIGNUP", "SEND_OTP", "RESET_PASSWORD", "UPLOAD_FILE", "SYNC_NORMALIZED_FILE", "DELETE_FILE", "RESTORE_FILE", "GRAND_FILE_ACCESS", "REVOKE_FILE_ACCESS", "REPLACE_FILE", "REVERT_FILE":
		return true
	}
	return false
}
