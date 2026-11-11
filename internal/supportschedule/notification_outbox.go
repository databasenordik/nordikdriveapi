package supportschedule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"sort"
	"strings"
	"time"
)

// The owning workflow keeps a durable snapshot with its transaction/audit row.
// It can wait for persisted Zoom details before producing finalized email intents.
// Existing historical audit rows default to ignored, so rollout cannot replay them.
type supportNotificationSnapshot struct {
	Event                string
	Request              *SupportCallRequest
	Former               *SupportUser
	Assignment           *SupportAssignment
	AssignmentRecipients []supportCallEmailRecipient
	Affected             []*SupportCallRequest
	Reason               string
}

func (s *Service) notificationSnapshot(tx *gorm.DB, entity string, id *uint, action string, details []byte) (*supportNotificationSnapshot, error) {
	if id == nil {
		return nil, nil
	}
	labels := map[string]string{"support_request_created": "Support call approval requested", "support_request_approve": "Support call confirmed", "support_request_reject": "Support call request declined", "support_request_propose_alternative": "A new support call time was proposed", "alternative_time_accepted": "Support call scheduled", "support_request_cancelled": "Support call request cancelled", "actual_duration_recorded": "Support call completed", "manager_call_reassignment": "Support call details changed", "call_needs_alternative_after_availability_change": "Support call needs a new time"}
	owner := *s
	owner.DB = tx
	if event, ok := labels[action]; ok {
		requestID := *id
		if entity == "support_call" {
			var call SupportCall
			if err := tx.First(&call, "id = ?", *id).Error; err != nil {
				return nil, err
			}
			requestID = call.SupportRequestID
		}
		request, err := owner.request(requestID)
		if err != nil {
			return nil, err
		}
		if action == "support_request_created" && request.Status == RequestStatusApproved {
			event = "Support call scheduled"
		}
		snapshot := &supportNotificationSnapshot{Event: event, Request: request}
		var meta struct {
			Previous *uint `json:"previous_assignee_id"`
		}
		if err := json.Unmarshal(details, &meta); err != nil {
			return nil, err
		}
		if meta.Previous != nil {
			var former SupportUser
			if err := tx.First(&former, "id = ?", *meta.Previous).Error; err != nil {
				return nil, err
			}
			snapshot.Former = &former
		}
		return snapshot, nil
	}
	if entity != "assignment" || (action != "manager_assignment_reassignment" && action != "full_day_assignment_reassignment") {
		return nil, nil
	}
	var assignment SupportAssignment
	if err := tx.Preload("PrimaryAssignee").Preload("PreviousAssignee").First(&assignment, "id = ?", *id).Error; err != nil {
		return nil, err
	}
	var meta struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(details, &meta); err != nil {
		return nil, err
	}
	snapshot := &supportNotificationSnapshot{Assignment: &assignment, Reason: meta.Reason}
	users := []*SupportUser{assignment.PrimaryAssignee, assignment.PreviousAssignee}
	var managers []SupportUser
	if err := tx.Where("LOWER(role) = ?", strings.ToLower(RoleManager)).Find(&managers).Error; err != nil {
		return nil, err
	}
	for i := range managers {
		users = append(users, &managers[i])
	}
	seen := map[string]bool{}
	for _, user := range users {
		if user == nil || strings.TrimSpace(user.Email) == "" || seen[user.Email] {
			continue
		}
		seen[user.Email] = true
		audience := supportCallStaffAudience
		if strings.EqualFold(user.Role, RoleManager) {
			audience = supportCallManagerAudience
		}
		snapshot.AssignmentRecipients = append(snapshot.AssignmentRecipients, supportCallEmailRecipient{Email: user.Email, Name: supportUserName(user), Audience: audience})
	}
	sort.Slice(snapshot.AssignmentRecipients, func(i, j int) bool {
		return snapshot.AssignmentRecipients[i].Email < snapshot.AssignmentRecipients[j].Email
	})
	var requests []SupportCallRequest
	if err := owner.requestQuery().Where("requested_date = ?", assignment.AssignmentDate).Find(&requests).Error; err != nil {
		return nil, err
	}
	for i := range requests {
		snapshot.Affected = append(snapshot.Affected, &requests[i])
	}
	return snapshot, nil
}
func requestEmailRecipients(r *SupportCallRequest, former *SupportUser) []supportCallEmailRecipient {
	var result []supportCallEmailRecipient
	seen := map[string]bool{}
	for _, entry := range []struct {
		User     *SupportUser
		Audience supportCallNotificationAudience
	}{{&r.RequestedBy, supportCallRequesterAudience}, {r.AssignedStaff, supportCallStaffAudience}, {former, supportCallFormerStaffAudience}} {
		if entry.User == nil || strings.TrimSpace(entry.User.Email) == "" || seen[entry.User.Email] {
			continue
		}
		seen[entry.User.Email] = true
		result = append(result, supportCallEmailRecipient{Email: entry.User.Email, Name: supportUserName(entry.User), Audience: entry.Audience})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Email < result[j].Email })
	return result
}
func (s *Service) emitRequestSnapshot(tx *gorm.DB, base, event string, createdAt time.Time, r *SupportCallRequest, former *SupportUser) (bool, error) {
	waiting := false
	for _, recipient := range requestEmailRecipients(r, former) {
		source := *r
		if r.Status == RequestStatusApproved && s.meetingIntegrationEnabled() && recipient.Audience != supportCallFormerStaffAudience {
			if r.Call == nil {
				return false, errors.New("support call snapshot missing")
			}
			var current SupportCall
			if err := tx.First(&current, "id = ?", r.Call.ID).Error; err != nil {
				return false, err
			}
			if current.Status != r.Call.Status || !current.ScheduledStartTime.Equal(r.Call.ScheduledStartTime) || !current.ScheduledEndTime.Equal(r.Call.ScheduledEndTime) || !sameStaff(current.AssignedStaffID, r.Call.AssignedStaffID) {
				continue
			}
			if strings.TrimSpace(current.ZoomJoinURL) == "" {
				waiting = true
				continue
			}
			source.Call = &current
		}
		if err := s.Outbox.SendAt(tx, "support", base, createdAt, []string{recipient.Email}, supportCallNotificationSubject(&source, event), supportCallNotificationBody(&source, recipient, event)); err != nil {
			return false, err
		}
	}
	return waiting, nil
}
func sameStaff(a, b *uint) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
func (s *Service) RecoverNotifications(ctx context.Context) error {
	return s.recoverNotifications(ctx, nil)
}

func (s *Service) recoverNotifications(ctx context.Context, filter func(*gorm.DB) *gorm.DB) error {
	if s.Outbox == nil {
		return nil
	}
	var pending []SupportScheduleAuditLog
	query := s.DB.WithContext(ctx).Where("notification_state = ? AND (notification_next_attempt IS NULL OR notification_next_attempt <= ?)", "pending", time.Now().UTC())
	if filter != nil {
		query = filter(query)
	}
	if err := query.Order("notification_next_attempt ASC, id ASC").Limit(10).Find(&pending).Error; err != nil {
		return err
	}
	for _, event := range pending {
		if err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			query := tx
			if tx.Dialector.Name() == "postgres" {
				query = query.Clauses(clause.Locking{Strength: "UPDATE"})
			}
			var record SupportScheduleAuditLog
			if err := query.First(&record, "id = ?", event.ID).Error; err != nil {
				return err
			}
			if record.NotificationState != "pending" {
				return nil
			}
			if time.Since(record.CreatedAt) > 24*time.Hour {
				return tx.Model(&record).Updates(map[string]any{"notification_state": "expired", "notification_snapshot": ""}).Error
			}
			var snapshot supportNotificationSnapshot
			if err := json.Unmarshal([]byte(record.NotificationSnapshot), &snapshot); err != nil {
				return err
			}
			base := fmt.Sprintf("support-audit:%d", record.ID)
			waiting := false
			if snapshot.Request != nil {
				var err error
				waiting, err = s.emitRequestSnapshot(tx, base, snapshot.Event, record.CreatedAt, snapshot.Request, snapshot.Former)
				if err != nil {
					return err
				}
			}
			if snapshot.Assignment != nil {
				for _, recipient := range snapshot.AssignmentRecipients {
					if err := s.Outbox.SendAt(tx, "support", base+":assignment", record.CreatedAt, []string{recipient.Email}, "[Nordik Drive] Support schedule updated", supportScheduleAssignmentEmailBody(snapshot.Assignment, recipient, snapshot.Reason)); err != nil {
						return err
					}
				}
				for _, request := range snapshot.Affected {
					wait, err := s.emitRequestSnapshot(tx, fmt.Sprintf("%s:request:%d", base, request.ID), "A support schedule change affected your request", record.CreatedAt, request, nil)
					if err != nil {
						return err
					}
					waiting = waiting || wait
				}
			}
			if waiting {
				return tx.Model(&record).Update("notification_next_attempt", time.Now().UTC().Add(5*time.Minute)).Error
			}
			return tx.Model(&record).Updates(map[string]any{"notification_state": "processed", "notification_snapshot": ""}).Error
		}); err != nil {
			return err
		}
	}
	return nil
}
