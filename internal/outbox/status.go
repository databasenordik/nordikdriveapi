package outbox

import (
	"context"
	"errors"
	"gorm.io/gorm"
	"strings"
)

// EventPattern escapes literal event identifiers before using a prefix query.
func EventPattern(prefix string) string {
	return strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(prefix) + ":%"
}
func ProviderAccepted(ctx context.Context, db *gorm.DB, remote Remote, eventPrefix string) (bool, error) {
	return ProviderAcceptedFor(ctx, db, remote, "records", eventPrefix)
}
func ProviderAcceptedFor(ctx context.Context, db *gorm.DB, remote Remote, producer, eventPrefix string) (bool, error) {
	state, err := GroupState(ctx, db, remote, producer, eventPrefix)
	return state == "provider_accepted", err
}

// GroupState checks every recipient, paging status requests to the owner API.
func GroupState(ctx context.Context, db *gorm.DB, remote Remote, producer, prefix string) (string, error) {
	outcome := "provider_accepted"
	pending, found := false, false
	for offset := 0; ; offset += 100 {
		var rows []Record
		if err := db.WithContext(ctx).Select("id", "state", "delivery_id").Where("producer = ? AND event_key LIKE ? ESCAPE '!'", producer, EventPattern(prefix)).Order("id ASC").Offset(offset).Limit(100).Find(&rows).Error; err != nil {
			return "", err
		}
		if len(rows) == 0 {
			break
		}
		found = true
		var ids []string
		for _, row := range rows {
			if row.State == "accepted" {
				ids = append(ids, row.DeliveryID)
			}
		}
		states := map[string]string{}
		if len(ids) > 0 {
			if remote == nil {
				return "", errors.New("notification status client unavailable")
			}
			var err error
			states, err = remote.Status(ctx, ids)
			if err != nil {
				return "", err
			}
		}
		for _, row := range rows {
			state := row.State
			if state == "accepted" {
				state = states[row.DeliveryID]
			}
			if !deliveryTerminal(state) {
				pending = true
			} else if state != "provider_accepted" && state != "processed" {
				if outcome == "provider_accepted" || state == "unknown" {
					outcome = state
				}
			}
		}
		if len(rows) < 100 {
			break
		}
	}
	if !found || pending {
		return "pending", nil
	}
	return outcome, nil
}
