package outbox

import (
	"context"
	"encoding/json"
	"nordik-drive-api/internal/notificationcontract"
	"time"
)

func deliveryTerminal(state string) bool {
	switch state {
	case "provider_accepted", "processed", "failed", "expired", "superseded", "unknown":
		return true
	}
	return false
}
func (d *Dispatcher) Reconcile(ctx context.Context) error {
	now := time.Now().UTC()
	var rows []Record
	if err := d.DB.WithContext(ctx).Where("next_attempt <= ? AND (state = ? OR (state IN ? AND kind = ? AND deadline > ?))", now, "accepted", []string{"failed", "unknown"}, "normal", now).Order("next_attempt ASC").Limit(50).Find(&rows).Error; err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.DeliveryID)
	}
	states, err := d.Remote.Status(ctx, ids)
	if err != nil {
		return err
	}
	for _, row := range rows {
		state := states[row.DeliveryID]
		if !deliveryTerminal(state) {
			state = "accepted"
		}
		next := now.Add(time.Minute)
		if state == "provider_accepted" {
			next = now
		}
		if state == "failed" || state == "unknown" {
			next = now.Add(5 * time.Minute)
		}
		if err := d.DB.WithContext(ctx).Model(&row).Updates(map[string]any{"state": state, "next_attempt": next}).Error; err != nil {
			return err
		}
	}
	return d.Cleanup(ctx)
}
func (d *Dispatcher) Cleanup(ctx context.Context) error {
	now := time.Now().UTC()
	var rows []Record
	if err := d.DB.WithContext(ctx).Where("payload_erased = ? AND (deadline <= ? OR state IN ? OR (kind = ? AND state IN ?))", false, now, []string{"provider_accepted", "processed"}, "otp", []string{"failed", "unknown", "expired", "superseded"}).Limit(50).Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		var command notificationcontract.Command
		if err := json.Unmarshal([]byte(row.Command), &command); err != nil {
			return err
		}
		command.Ciphertext = ""
		encoded, err := json.Marshal(command)
		if err != nil {
			return err
		}
		if err := d.DB.WithContext(ctx).Model(&row).Updates(map[string]any{"command": string(encoded), "payload_erased": true}).Error; err != nil {
			return err
		}
	}
	return nil
}
