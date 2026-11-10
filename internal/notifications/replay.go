package notifications

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"strings"
	"time"
)

type ReplayRecord struct {
	Key        string `gorm:"primaryKey"`
	DeliveryID string
	Caller     string
	Reason     string
	CreatedAt  time.Time
}

func (ReplayRecord) TableName() string { return "notifications.replays" }

// Each explicit approval permits one further attempt without resetting history.
// OTPs and expired notifications cannot be replayed.
func (s *Store) Replay(ctx context.Context, id, key, caller, reason string, now time.Time) error {
	reason = strings.TrimSpace(reason)
	if len(key) < 16 || len(key) > 128 || caller == "" || reason == "" || len(reason) > 500 {
		return ErrConflict
	}
	digest := sha256.Sum256([]byte(caller + "\x00" + key))
	cacheKey := hex.EncodeToString(digest[:])
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var previous ReplayRecord
		err := tx.First(&previous, "key = ?", cacheKey).Error
		if err == nil {
			if previous.DeliveryID != id || previous.Caller != caller || previous.Reason != reason {
				return ErrConflict
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var row Delivery
		if err := locked(tx).First(&row, "id = ?", id).Error; err != nil {
			return err
		}
		err = tx.First(&previous, "key = ?", cacheKey).Error
		if err == nil {
			if previous.DeliveryID != id || previous.Caller != caller || previous.Reason != reason {
				return ErrConflict
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if row.Kind != "normal" || !row.Deadline.After(now) || row.Command == "" || (row.State != "failed" && row.State != "unknown") {
			return ErrConflict
		}
		approval := ReplayRecord{Key: cacheKey, DeliveryID: id, Caller: caller, Reason: reason, CreatedAt: now}
		insert := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&approval)
		if insert.Error != nil {
			return insert.Error
		}
		if insert.RowsAffected != 1 {
			return ErrConflict
		}
		return tx.Model(&row).Updates(map[string]any{"state": "pending_enqueue", "max_attempts": row.Attempts + 1, "lease_until": nil, "dispatch_generation": gorm.Expr("dispatch_generation + 1"), "next_attempt": now, "updated_at": now}).Error
	})
}
