package supportschedule

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"nordik-drive-api/internal/requestscope"
)

var ErrMeetingUnavailable = errors.New("Zoom meeting is not ready")

func (s *Service) meetingIntegrationEnabled() bool {
	return s != nil && s.MeetingProvider != nil && s.MeetingProvider.Enabled()
}

func (s *Service) markZoomSyncPending(call *SupportCall) {
	if call == nil || (!s.meetingIntegrationEnabled() && strings.TrimSpace(call.ZoomMeetingID) == "") {
		return
	}
	call.ZoomSyncStatus = ZoomSyncPending
	call.ZoomSyncError = ""
}

func (s *Service) meetingInput(tx *gorm.DB, request *SupportCallRequest, call *SupportCall, settings *SupportScheduleSettings) (MeetingInput, error) {
	if request == nil || call == nil || call.AssignedStaffID == nil {
		return MeetingInput{}, errors.New("support call does not have an assigned Zoom host")
	}
	var host SupportUser
	if s.Directory != nil {
		users, err := s.identityUsers(tx, []uint{*call.AssignedStaffID})
		if err != nil {
			return MeetingInput{}, err
		}
		var found bool
		host, found = users[*call.AssignedStaffID]
		if !found {
			return MeetingInput{}, ErrNotFound
		}
	} else if err := tx.First(&host, *call.AssignedStaffID).Error; err != nil {
		return MeetingInput{}, mapNotFound(err)
	}
	hostEmail := strings.TrimSpace(host.Email)
	if hostEmail == "" {
		return MeetingInput{}, errors.New("assigned support person does not have an email address")
	}
	timeZone := DefaultTimeZone
	if settings != nil && strings.TrimSpace(settings.TimeZone) != "" {
		timeZone = strings.TrimSpace(settings.TimeZone)
	}
	return MeetingInput{
		RequestID: request.ID,
		HostEmail: hostEmail,
		Topic:     fmt.Sprintf("NORDIK support call: %s", strings.TrimSpace(request.Subject)),
		Agenda:    "Scheduled NORDIK Drive support call. Support details remain securely available in NORDIK Drive.",
		StartTime: call.ScheduledStartTime,
		EndTime:   call.ScheduledEndTime,
		TimeZone:  timeZone,
	}, nil
}

// syncZoomMeeting reconciles one local call with Zoom. The local workflow is
// authoritative: a Zoom failure is persisted for retry but never rolls back a
// valid approval, cancellation, or reassignment.
func (s *Service) syncZoomMeeting(callID uint) error {
	if !s.meetingIntegrationEnabled() {
		return nil
	}
	settings, err := s.settings()
	if err != nil {
		return err
	}
	if s.Directory != nil {
		prepared, err := s.prepareIdentity(userScope{Calls: []uint{callID}})
		if err != nil {
			return err
		}
		s = prepared
	}
	ctx, cancel := context.WithTimeout(requestscope.Context(s.DB), 25*time.Second)
	defer cancel()

	var syncErr error
	err = s.DB.Transaction(func(tx *gorm.DB) error {
		var call SupportCall
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&call, callID).Error; err != nil {
			return mapNotFound(err)
		}
		var request SupportCallRequest
		if err := tx.First(&request, call.SupportRequestID).Error; err != nil {
			return mapNotFound(err)
		}

		shouldExist := call.Status == RequestStatusApproved
		shouldDelete := call.Status == RequestStatusCancelled || call.Status == RequestStatusRejected || call.Status == RequestStatusAlternativeProposed
		if !shouldExist && !shouldDelete {
			return nil
		}

		call.ZoomSyncStatus, call.ZoomSyncError = ZoomSyncPending, ""
		if shouldDelete {
			if strings.TrimSpace(call.ZoomMeetingID) != "" {
				if err := s.MeetingProvider.DeleteMeeting(ctx, call.ZoomMeetingID); err != nil {
					syncErr = err
					return s.saveZoomSyncFailure(tx, &call, err)
				}
			}
			call.ZoomMeetingID, call.ZoomJoinURL, call.ZoomPasscode, call.ZoomHostEmail = "", "", "", ""
			call.ZoomSyncStatus, call.ZoomSyncError = ZoomSyncDeleted, ""
			now := s.now()
			call.ZoomSyncedAt = &now
			if err := tx.Save(&call).Error; err != nil {
				return err
			}
			return s.recordAudit(tx, "support_call", &call.ID, "zoom_meeting_deleted", nil, nil)
		}

		input, err := s.meetingInput(tx, &request, &call, settings)
		if err != nil {
			syncErr = err
			return s.saveZoomSyncFailure(tx, &call, err)
		}

		if call.ZoomMeetingID != "" && !strings.EqualFold(strings.TrimSpace(call.ZoomHostEmail), input.HostEmail) {
			if err := s.MeetingProvider.DeleteMeeting(ctx, call.ZoomMeetingID); err != nil {
				syncErr = err
				return s.saveZoomSyncFailure(tx, &call, err)
			}
			call.ZoomMeetingID, call.ZoomJoinURL, call.ZoomPasscode, call.ZoomHostEmail = "", "", "", ""
		}

		if call.ZoomMeetingID != "" {
			err = s.MeetingProvider.UpdateMeeting(ctx, call.ZoomMeetingID, input)
			if errors.Is(err, ErrMeetingNotFound) {
				call.ZoomMeetingID, call.ZoomJoinURL, call.ZoomPasscode, call.ZoomHostEmail = "", "", "", ""
			} else if err != nil {
				syncErr = err
				return s.saveZoomSyncFailure(tx, &call, err)
			}
		}

		action := "zoom_meeting_updated"
		if call.ZoomMeetingID == "" {
			details, err := s.MeetingProvider.CreateMeeting(ctx, input)
			if err != nil {
				syncErr = err
				return s.saveZoomSyncFailure(tx, &call, err)
			}
			call.ZoomMeetingID, call.ZoomJoinURL, call.ZoomPasscode, call.ZoomHostEmail = details.ExternalID, details.JoinURL, details.Passcode, details.HostEmail
			action = "zoom_meeting_created"
		}
		call.ZoomSyncStatus, call.ZoomSyncError = ZoomSyncSynced, ""
		now := s.now()
		call.ZoomSyncedAt = &now
		if err := tx.Save(&call).Error; err != nil {
			return err
		}
		return s.recordAudit(tx, "support_call", &call.ID, action, nil, map[string]interface{}{
			"zoom_meeting_id": call.ZoomMeetingID, "zoom_host_email": call.ZoomHostEmail,
		})
	})
	if err != nil {
		return err
	}
	return syncErr
}

func (s *Service) syncZoomMeetingForRequest(requestID uint) error {
	if !s.meetingIntegrationEnabled() {
		return nil
	}
	var call SupportCall
	if err := s.DB.Where("support_request_id = ?", requestID).First(&call).Error; err != nil {
		return mapNotFound(err)
	}
	return s.syncZoomMeeting(call.ID)
}

func (s *Service) syncZoomMeetingsForDate(date string) error {
	if !s.meetingIntegrationEnabled() {
		return nil
	}
	var calls []SupportCall
	if err := s.DB.Joins("JOIN support_call_requests ON support_call_requests.id = support_calls.support_request_id").
		Where("support_call_requests.requested_date = ?", date).Find(&calls).Error; err != nil {
		return err
	}
	var syncErrors []error
	for _, call := range calls {
		if err := s.syncZoomMeeting(call.ID); err != nil {
			syncErrors = append(syncErrors, fmt.Errorf("support call %d: %w", call.ID, err))
		}
	}
	return errors.Join(syncErrors...)
}

func (s *Service) saveZoomSyncFailure(tx *gorm.DB, call *SupportCall, syncErr error) error {
	message := strings.TrimSpace(syncErr.Error())
	if len(message) > 1000 {
		message = message[:1000]
	}
	call.ZoomSyncStatus, call.ZoomSyncError = ZoomSyncFailed, message
	if err := tx.Save(call).Error; err != nil {
		return err
	}
	return s.recordAudit(tx, "support_call", &call.ID, "zoom_meeting_sync_failed", nil, map[string]interface{}{"error": message})
}

func (s *Service) SyncPendingZoomMeetings() error {
	if !s.meetingIntegrationEnabled() {
		return nil
	}
	var calls []SupportCall
	err := s.DB.Where(
		"zoom_sync_status IN ? OR (status = ? AND zoom_meeting_id = '') OR (status IN ? AND zoom_meeting_id <> '')",
		[]string{ZoomSyncPending, ZoomSyncFailed}, RequestStatusApproved,
		[]string{RequestStatusCancelled, RequestStatusRejected, RequestStatusAlternativeProposed},
	).Order("scheduled_start_time ASC").Find(&calls).Error
	if err != nil {
		return err
	}
	var syncErrors []error
	for _, call := range calls {
		if err := s.syncZoomMeeting(call.ID); err != nil {
			syncErrors = append(syncErrors, fmt.Errorf("support call %d: %w", call.ID, err))
			continue
		}
		s.notifyZoomMeetingReady(call.SupportRequestID)
	}
	return errors.Join(syncErrors...)
}

func (s *Service) notifyZoomMeetingReady(requestID uint) {
	request, err := s.request(requestID)
	if err != nil || request.Call == nil || request.Status != RequestStatusApproved || request.Call.ZoomSyncStatus != ZoomSyncSynced {
		return
	}
	s.notifyRequest(request, "Zoom link ready")
}

func (s *Service) StartZoomMeeting(actorID, callID uint) (*ZoomStartURLResponse, error) {
	if !s.meetingIntegrationEnabled() {
		return nil, ErrIntegrationNotConfigured
	}
	if err := s.requireSupportAdmin(actorID); err != nil {
		return nil, err
	}
	var call SupportCall
	if err := s.DB.First(&call, callID).Error; err != nil {
		return nil, mapNotFound(err)
	}
	if call.AssignedStaffID == nil || *call.AssignedStaffID != actorID {
		return nil, ErrForbidden
	}
	if call.Status != RequestStatusApproved {
		return nil, ErrInvalidStatusChange
	}
	if call.ZoomSyncStatus != ZoomSyncSynced || strings.TrimSpace(call.ZoomMeetingID) == "" {
		if err := s.syncZoomMeeting(call.ID); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrMeetingUnavailable, err)
		}
		if err := s.DB.First(&call, call.ID).Error; err != nil {
			return nil, err
		}
		s.notifyZoomMeetingReady(call.SupportRequestID)
	}
	ctx, cancel := context.WithTimeout(requestscope.Context(s.DB), 25*time.Second)
	defer cancel()
	startURL, err := s.MeetingProvider.StartURL(ctx, call.ZoomMeetingID)
	if errors.Is(err, ErrMeetingNotFound) {
		if updateErr := s.DB.Model(&call).Updates(map[string]interface{}{
			"zoom_meeting_id": "", "zoom_join_url": "", "zoom_passcode": "", "zoom_host_email": "", "zoom_sync_status": ZoomSyncPending,
		}).Error; updateErr != nil {
			return nil, updateErr
		}
		if syncErr := s.syncZoomMeeting(call.ID); syncErr != nil {
			return nil, fmt.Errorf("%w: %v", ErrMeetingUnavailable, syncErr)
		}
		if err := s.DB.First(&call, call.ID).Error; err != nil {
			return nil, err
		}
		s.notifyZoomMeetingReady(call.SupportRequestID)
		startURL, err = s.MeetingProvider.StartURL(ctx, call.ZoomMeetingID)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMeetingUnavailable, err)
	}
	return &ZoomStartURLResponse{StartURL: startURL}, nil
}
