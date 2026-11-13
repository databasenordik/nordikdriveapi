package supportschedule

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"nordik-drive-api/internal/requestscope"
)

var reservingCallStatuses = []string{
	RequestStatusPending,
	RequestStatusAwaitingApproval,
	RequestStatusApproved,
}

func (s *Service) ListAvailability(date string, duration int, requestedStaffID *uint) (*AvailabilityResponse, error) {
	settings, err := s.settings()
	if err != nil {
		return nil, err
	}
	if duration == 0 {
		duration = settings.DefaultDurationMinutes
	}
	if !containsDuration(durationList(settings), duration) {
		return nil, fmt.Errorf("%w: duration is not enabled", ErrInvalidInput)
	}
	loc, err := s.location(settings)
	if err != nil {
		return nil, err
	}
	dateStart, err := parseDateAndTime(date, "00:00", loc)
	if err != nil {
		return nil, err
	}
	date = scheduleDateFor(dateStart, loc)
	if err := s.validateBookableDate(settings, date, loc); err != nil {
		return nil, err
	}
	if err := s.EnsureRollingSchedule(); err != nil {
		return nil, err
	}

	response := &AvailabilityResponse{Date: date, Duration: duration, Slots: []AvailabilitySlot{}, UnavailableSlots: []AvailabilitySlot{}}
	var staffID uint
	if requestedStaffID != nil {
		if err := s.requireSupportAdmin(*requestedStaffID); err != nil {
			return nil, fmt.Errorf("%w: requested support person is unavailable", ErrInvalidInput)
		}
		staff, err := s.user(*requestedStaffID)
		if err != nil {
			return nil, err
		}
		staffID, response.AssignedStaff = *requestedStaffID, staff
	} else {
		var assignment SupportAssignment
		if err := s.DB.Scopes(s.userPreload("PrimaryAssignee")).Where("assignment_date = ?", date).First(&assignment).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return response, nil
			}
			return nil, err
		}
		if err := s.hydrate(s.DB, primaryAssignmentTargets(&assignment)); err != nil {
			return nil, err
		}
		if assignment.PrimaryAssigneeID == nil {
			return response, nil
		}
		staffID, response.AssignedStaff = *assignment.PrimaryAssigneeID, assignment.PrimaryAssignee
	}

	start, end, err := windowForDate(settings, date)
	if err != nil {
		return nil, err
	}
	for candidate := start; !candidate.Add(time.Duration(duration) * time.Minute).After(end); candidate = candidate.Add(time.Duration(duration) * time.Minute) {
		candidateEnd := candidate.Add(time.Duration(duration) * time.Minute)
		if !candidate.After(s.now().In(loc)) {
			response.UnavailableSlots = append(response.UnavailableSlots, AvailabilitySlot{StartAt: candidate, EndAt: candidateEnd, UnavailableReason: "This time has already passed"})
			continue
		}
		if s.slotUnavailableTx(s.DB, staffID, candidate, candidateEnd, 0) {
			response.UnavailableSlots = append(response.UnavailableSlots, AvailabilitySlot{StartAt: candidate, EndAt: candidateEnd, UnavailableReason: "Unavailable or already booked"})
			continue
		}
		response.Slots = append(response.Slots, AvailabilitySlot{StartAt: candidate, EndAt: candidateEnd})
	}
	return response, nil
}

// ListCalendar returns the current booking horizon in a shape that lets the
// client accurately disable impossible days before a user tries to select one.
// Slot availability is still checked again during CreateCall to prevent races.
func (s *Service) ListCalendar(actorID uint, duration int, requestedStaffID *uint) (*CalendarResponse, error) {
	if _, err := s.user(actorID); err != nil {
		return nil, err
	}
	settings, err := s.settings()
	if err != nil {
		return nil, err
	}
	if duration == 0 {
		duration = settings.DefaultDurationMinutes
	}
	if !containsDuration(durationList(settings), duration) {
		return nil, fmt.Errorf("%w: duration is not enabled", ErrInvalidInput)
	}
	loc, err := s.location(settings)
	if err != nil {
		return nil, err
	}
	if err := s.EnsureRollingSchedule(); err != nil {
		return nil, err
	}

	manager, err := s.isManager(actorID)
	if err != nil {
		return nil, err
	}
	supportAdmin, err := s.isSupportAdmin(actorID)
	if err != nil {
		return nil, err
	}

	var requestedStaff *SupportUser
	if requestedStaffID != nil {
		if err := s.requireSupportAdmin(*requestedStaffID); err != nil {
			return nil, fmt.Errorf("%w: requested support person is unavailable", ErrInvalidInput)
		}
		requestedStaff, err = s.user(*requestedStaffID)
		if err != nil {
			return nil, err
		}
	}

	from := scheduleDateFor(s.now(), loc)
	to := scheduleDateFor(s.now().In(loc).AddDate(0, 0, DefaultHorizonDays-1), loc)
	var assignments []SupportAssignment
	if err := s.DB.Scopes(s.userPreload("PrimaryAssignee")).Where("assignment_date >= ? AND assignment_date <= ?", from, to).Find(&assignments).Error; err != nil {
		return nil, err
	}
	if err := s.hydrate(s.DB, primaryAssignmentsTargets(assignments)); err != nil {
		return nil, err
	}
	assignmentByDate := make(map[string]SupportAssignment, len(assignments))
	for _, assignment := range assignments {
		assignmentByDate[assignment.AssignmentDate] = assignment
	}

	var availability []StaffAvailability
	if err := s.DB.Scopes(s.userPreload("Staff")).Where("availability_date >= ? AND availability_date <= ?", from, to).Find(&availability).Error; err != nil {
		return nil, err
	}
	if err := s.hydrate(s.DB, availabilityTargets(availability)); err != nil {
		return nil, err
	}
	availabilityByStaffDate := make(map[string][]StaffAvailability)
	for _, record := range availability {
		key := fmt.Sprintf("%d:%s", record.StaffID, record.AvailabilityDate)
		availabilityByStaffDate[key] = append(availabilityByStaffDate[key], record)
	}

	start, _, err := windowForDate(settings, from)
	if err != nil {
		return nil, err
	}
	_, lastEnd, err := windowForDate(settings, to)
	if err != nil {
		return nil, err
	}
	var calls []SupportCall
	if err := s.DB.Scopes(s.userPreload("AssignedStaff")).Where("scheduled_start_time >= ? AND scheduled_start_time < ? AND status IN ?", start, lastEnd, reservingCallStatuses).Find(&calls).Error; err != nil {
		return nil, err
	}
	if err := s.hydrate(s.DB, callTargets(calls)); err != nil {
		return nil, err
	}
	callsByStaffDate := make(map[string][]SupportCall)
	for _, call := range calls {
		if call.AssignedStaffID == nil {
			continue
		}
		date := scheduleDateFor(call.ScheduledStartTime, loc)
		key := fmt.Sprintf("%d:%s", *call.AssignedStaffID, date)
		callsByStaffDate[key] = append(callsByStaffDate[key], call)
	}

	response := &CalendarResponse{TimeZone: settings.TimeZone, DurationMinutes: duration, Days: make([]CalendarDay, 0, DefaultHorizonDays)}
	for offset := 0; offset < DefaultHorizonDays; offset++ {
		date := scheduleDateFor(s.now().In(loc).AddDate(0, 0, offset), loc)
		day := CalendarDay{Date: date, Status: "uncovered", StatusMessage: "No support person is scheduled"}
		if !isSupportBusinessDay(date) {
			day.Status, day.StatusMessage = "weekend", "No support on weekends"
			response.Days = append(response.Days, day)
			continue
		}

		assignment := assignmentByDate[date]
		staff := assignment.PrimaryAssignee
		staffID := assignment.PrimaryAssigneeID
		if requestedStaffID != nil {
			staff, staffID = requestedStaff, requestedStaffID
		}
		if staffID == nil || staff == nil {
			response.Days = append(response.Days, day)
			continue
		}
		day.AssignedStaff = staff
		day.IsAssignedToViewer = *staffID == actorID
		key := fmt.Sprintf("%d:%s", *staffID, date)
		periods := availabilityByStaffDate[key]
		day.ScheduledCallCount = len(callsByStaffDate[key])
		if manager || (supportAdmin && *staffID == actorID) {
			day.UnavailablePeriods = periods
			day.ScheduledCalls = callsByStaffDate[key]
		}

		fullDayUnavailable := false
		partialUnavailable := false
		for _, period := range periods {
			if period.FullDayUnavailable {
				fullDayUnavailable = true
			} else {
				partialUnavailable = true
			}
		}
		if fullDayUnavailable {
			day.Status, day.StatusMessage = "fully_unavailable", "The assigned support person is unavailable"
			response.Days = append(response.Days, day)
			continue
		}

		windowStart, windowEnd, err := windowForDate(settings, date)
		if err != nil {
			return nil, err
		}
		for candidate := windowStart; !candidate.Add(time.Duration(duration) * time.Minute).After(windowEnd); candidate = candidate.Add(time.Duration(duration) * time.Minute) {
			candidateEnd := candidate.Add(time.Duration(duration) * time.Minute)
			if candidate.After(s.now().In(loc)) && !s.slotUnavailableTx(s.DB, *staffID, candidate, candidateEnd, 0) {
				day.AvailableSlotCount++
			}
		}
		day.IsBookable = day.AvailableSlotCount > 0
		switch {
		case !day.IsBookable:
			day.Status, day.StatusMessage = "fully_booked", "No times remain"
		case partialUnavailable:
			day.Status, day.StatusMessage = "partial_availability", fmt.Sprintf("%d available time%s; partial availability", day.AvailableSlotCount, plural(day.AvailableSlotCount))
		default:
			day.Status, day.StatusMessage = "available", fmt.Sprintf("%d available time%s", day.AvailableSlotCount, plural(day.AvailableSlotCount))
		}
		response.Days = append(response.Days, day)
	}
	return response, nil
}

func plural(count int) string {
	if count == 1 {
		return ""
	}
	return "s"
}

func (s *Service) validateBookableDate(settings *SupportScheduleSettings, date string, loc *time.Location) error {
	today := scheduleDateFor(s.now(), loc)
	last := scheduleDateFor(s.now().In(loc).AddDate(0, 0, DefaultHorizonDays-1), loc)
	if date < today || date > last {
		return fmt.Errorf("%w: date is outside the 14-day booking horizon", ErrInvalidInput)
	}
	if !isSupportBusinessDay(date) {
		return fmt.Errorf("%w: support is available Monday through Friday only", ErrUnavailable)
	}
	return nil
}

func (s *Service) CreateCall(actorID uint, input CreateCallInput) (*SupportCallRequest, error) {
	if _, err := s.user(actorID); err != nil {
		return nil, err
	}
	settings, err := s.settings()
	if err != nil {
		return nil, err
	}
	if input.DurationMinutes == 0 {
		input.DurationMinutes = settings.DefaultDurationMinutes
	}
	if !containsDuration(durationList(settings), input.DurationMinutes) {
		return nil, fmt.Errorf("%w: duration is not enabled", ErrInvalidInput)
	}
	subject, description := strings.TrimSpace(input.Subject), strings.TrimSpace(input.Message)
	if subject == "" || len(subject) > 160 {
		return nil, fmt.Errorf("%w: subject is required and must be at most 160 characters", ErrInvalidInput)
	}
	if len(description) > 4000 {
		return nil, fmt.Errorf("%w: description must be at most 4000 characters", ErrInvalidInput)
	}
	loc, err := s.location(settings)
	if err != nil {
		return nil, err
	}
	start, err := time.Parse(time.RFC3339, strings.TrimSpace(input.ScheduledStart))
	if err != nil {
		return nil, fmt.Errorf("%w: scheduled_start must be RFC3339", ErrInvalidInput)
	}
	start = start.In(loc)
	end := start.Add(time.Duration(input.DurationMinutes) * time.Minute)
	date := scheduleDateFor(start, loc)
	workdayStart, workdayEnd, err := windowForDate(settings, date)
	if err != nil || start.Before(workdayStart) || end.After(workdayEnd) || !start.After(s.now().In(loc)) {
		return nil, fmt.Errorf("%w: call must be in a future support-hours slot", ErrInvalidInput)
	}
	if err := s.validateBookableDate(settings, date, loc); err != nil {
		return nil, err
	}
	if err := s.EnsureRollingSchedule(); err != nil {
		return nil, err
	}

	var requestID uint
	if s.Directory != nil {
		extra := []uint{actorID}
		if input.RequestedStaffID != nil {
			extra = append(extra, *input.RequestedStaffID)
		}
		prepared, err := s.prepareIdentity(userScope{Extra: extra, Dates: []string{date}})
		if err != nil {
			return nil, err
		}
		s = prepared
	}
	err = s.DB.Transaction(func(tx *gorm.DB) error {
		var assignedStaffID uint
		requestType, status := RequestTypeAutomaticDaily, RequestStatusApproved
		var assignment SupportAssignment
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("assignment_date = ?", date).First(&assignment).Error; err != nil {
			return err
		}
		requestedStaffID := input.RequestedStaffID
		if requestedStaffID != nil {
			ok, err := s.isSupportAdmin(*requestedStaffID)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%w: requested support person is not active", ErrInvalidInput)
			}
			assignedStaffID = *requestedStaffID
			requestType, status = RequestTypeSpecificStaff, RequestStatusAwaitingApproval
			if assignment.PrimaryAssigneeID != nil && *assignment.PrimaryAssigneeID == *requestedStaffID {
				status = RequestStatusApproved
			}
		} else {
			if assignment.PrimaryAssigneeID == nil {
				return ErrNoSupportStaff
			}
			assignedStaffID = *assignment.PrimaryAssigneeID
		}
		if s.slotUnavailableTx(tx, assignedStaffID, start, end, 0) {
			return ErrUnavailable
		}
		request := &SupportCallRequest{
			RequestedByUserID: actorID, RequestType: requestType, RequestedDate: date,
			PreferredStartTime: &start, PreferredEndTime: &end, RequestedStaffID: requestedStaffID,
			AssignedStaffID: &assignedStaffID, Status: status, Subject: subject, Description: description,
		}
		if err := tx.Create(request).Error; err != nil {
			return err
		}
		call := &SupportCall{SupportRequestID: request.ID, AssignedStaffID: &assignedStaffID, ScheduledStartTime: start, ScheduledEndTime: end, Status: status}
		if status == RequestStatusApproved {
			s.markZoomSyncPending(call)
		}
		if err := tx.Create(call).Error; err != nil {
			return mapSlotConflict(err)
		}
		requestID = request.ID
		return s.recordAudit(tx, "support_request", &request.ID, "support_request_created", &actorID, map[string]interface{}{
			"request_type": requestType, "assigned_staff_id": assignedStaffID, "scheduled_start": start,
		})
	})
	if err != nil {
		return nil, err
	}
	_ = s.syncZoomMeetingForRequest(requestID)
	request, err := s.request(requestID)
	if err == nil {
		if request.Status == RequestStatusApproved {
			s.notifyRequest(request, "Support call scheduled")
		} else {
			s.notifyRequest(request, "Support call approval requested")
		}
	}
	return request, err
}

func mapSlotConflict(err error) error {
	if err != nil && (strings.Contains(strings.ToLower(err.Error()), "exclusion") || strings.Contains(strings.ToLower(err.Error()), "unique constraint")) {
		return fmt.Errorf("%w: the slot was booked by another request", ErrUnavailable)
	}
	return err
}

func (s *Service) ListRequests(actorID uint, scope string) ([]SupportCallRequest, error) {
	query := s.requestQuery().Order("created_at DESC")
	switch scope {
	case "", "mine":
		query = query.Where("requested_by_user_id = ?", actorID)
	case "staff":
		if err := s.requireSupportAdmin(actorID); err != nil {
			return nil, err
		}
		query = query.Where("assigned_staff_id = ?", actorID)
	case "manage", "all":
		if err := s.requireManager(actorID); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("%w: unsupported request scope", ErrInvalidInput)
	}
	var requests []SupportCallRequest
	if err := query.Find(&requests).Error; err != nil {
		return nil, err
	}
	if err := s.hydrate(s.DB, requestsTargets(requests)); err != nil {
		return nil, err
	}
	return requests, nil
}

func (s *Service) ListCalls(actorID uint, scope string) ([]SupportCall, error) {
	query := s.DB.Scopes(s.userPreload("AssignedStaff")).Order("scheduled_start_time ASC")
	switch scope {
	case "", "mine":
		query = query.Joins("JOIN support_call_requests ON support_call_requests.id = support_calls.support_request_id").
			Where("support_call_requests.requested_by_user_id = ? OR support_calls.assigned_staff_id = ?", actorID, actorID)
	case "staff":
		if err := s.requireSupportAdmin(actorID); err != nil {
			return nil, err
		}
		query = query.Where("assigned_staff_id = ?", actorID)
	case "manage", "all":
		if err := s.requireManager(actorID); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("%w: unsupported call scope", ErrInvalidInput)
	}
	var calls []SupportCall
	if err := query.Find(&calls).Error; err != nil {
		return nil, err
	}
	if err := s.hydrate(s.DB, callTargets(calls)); err != nil {
		return nil, err
	}
	return calls, nil
}

func (s *Service) DecideRequest(actorID, requestID uint, input RequestDecisionInput) (*SupportCallRequest, error) {
	decision := strings.ToLower(strings.TrimSpace(input.Decision))
	if decision != "approve" && decision != "reject" && decision != "propose_alternative" {
		return nil, fmt.Errorf("%w: decision must be approve, reject, or propose_alternative", ErrInvalidInput)
	}
	var resultID uint
	if s.Directory != nil {
		prepared, err := s.prepareIdentity(userScope{Extra: []uint{actorID}, Requests: []uint{requestID}})
		if err != nil {
			return nil, err
		}
		s = prepared
	}
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		request, call, err := s.requestAndCallTx(tx, requestID, true)
		if err != nil {
			return err
		}
		if request.AssignedStaffID == nil || *request.AssignedStaffID != actorID {
			return ErrForbidden
		}
		if err := s.requireSupportAdmin(actorID); err != nil {
			return err
		}
		if request.Status != RequestStatusPending && request.Status != RequestStatusAwaitingApproval && request.Status != RequestStatusAlternativeProposed {
			return ErrInvalidStatusChange
		}
		note := strings.TrimSpace(input.Note)
		switch decision {
		case "approve":
			if call.AssignedStaffID == nil || s.slotUnavailableTx(tx, *call.AssignedStaffID, call.ScheduledStartTime, call.ScheduledEndTime, call.ID) {
				return ErrUnavailable
			}
			request.Status, call.Status, request.RejectionReason = RequestStatusApproved, RequestStatusApproved, ""
			s.markZoomSyncPending(call)
		case "reject":
			if note == "" {
				return fmt.Errorf("%w: a rejection reason is required", ErrInvalidInput)
			}
			request.Status, call.Status, request.RejectionReason = RequestStatusRejected, RequestStatusRejected, note
			s.markZoomSyncPending(call)
		case "propose_alternative":
			alternativeStart, alternativeEnd, err := s.parseAlternativeSlot(input, request.RequestedDate)
			if err != nil {
				return err
			}
			if call.AssignedStaffID == nil || s.slotUnavailableTx(tx, *call.AssignedStaffID, alternativeStart, alternativeEnd, call.ID) {
				return ErrUnavailable
			}
			request.Status, call.Status = RequestStatusAlternativeProposed, RequestStatusAlternativeProposed
			request.AlternativeStartTime, request.AlternativeEndTime, request.RejectionReason = &alternativeStart, &alternativeEnd, note
			s.markZoomSyncPending(call)
		}
		if err := tx.Save(request).Error; err != nil {
			return err
		}
		if err := tx.Save(call).Error; err != nil {
			return err
		}
		resultID = request.ID
		return s.recordAudit(tx, "support_request", &request.ID, "support_request_"+decision, &actorID, map[string]interface{}{"note": note})
	})
	if err != nil {
		return nil, err
	}
	_ = s.syncZoomMeetingForRequest(resultID)
	request, err := s.request(resultID)
	if err == nil {
		event := "Support call updated"
		switch request.Status {
		case RequestStatusApproved:
			event = "Support call confirmed"
		case RequestStatusRejected:
			event = "Support call request declined"
		case RequestStatusAlternativeProposed:
			event = "A new support call time was proposed"
		}
		s.notifyRequest(request, event)
	}
	return request, err
}

func (s *Service) ApproveSpecificCall(actorID, requestID uint, input SpecificApprovalInput) (*SupportCallRequest, error) {
	decision := "reject"
	if input.Approved {
		decision = "approve"
	}
	return s.DecideRequest(actorID, requestID, RequestDecisionInput{Decision: decision, Note: input.Note})
}

func (s *Service) AcceptAlternative(actorID, requestID uint) (*SupportCallRequest, error) {
	if s.Directory != nil {
		prepared, err := s.prepareIdentity(userScope{Extra: []uint{actorID}, Requests: []uint{requestID}})
		if err != nil {
			return nil, err
		}
		s = prepared
	}
	var resultID uint
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		request, call, err := s.requestAndCallTx(tx, requestID, true)
		if err != nil {
			return err
		}
		if request.RequestedByUserID != actorID {
			return ErrForbidden
		}
		if request.Status != RequestStatusAlternativeProposed || request.AlternativeStartTime == nil || request.AlternativeEndTime == nil || call.AssignedStaffID == nil {
			return ErrInvalidStatusChange
		}
		if s.slotUnavailableTx(tx, *call.AssignedStaffID, *request.AlternativeStartTime, *request.AlternativeEndTime, call.ID) {
			return ErrUnavailable
		}
		call.ScheduledStartTime, call.ScheduledEndTime, call.Status = *request.AlternativeStartTime, *request.AlternativeEndTime, RequestStatusApproved
		s.markZoomSyncPending(call)
		request.PreferredStartTime, request.PreferredEndTime, request.Status = request.AlternativeStartTime, request.AlternativeEndTime, RequestStatusApproved
		if err := tx.Save(call).Error; err != nil {
			return mapSlotConflict(err)
		}
		if err := tx.Save(request).Error; err != nil {
			return err
		}
		resultID = request.ID
		return s.recordAudit(tx, "support_request", &request.ID, "alternative_time_accepted", &actorID, map[string]interface{}{"scheduled_start": call.ScheduledStartTime})
	})
	if err != nil {
		return nil, err
	}
	_ = s.syncZoomMeetingForRequest(resultID)
	request, err := s.request(resultID)
	if err == nil {
		s.notifyRequest(request, "Support call scheduled")
	}
	return request, err
}

func (s *Service) CancelRequest(actorID, requestID uint) (*SupportCallRequest, error) {
	if s.Directory != nil {
		prepared, err := s.prepareIdentity(userScope{Extra: []uint{actorID}, Requests: []uint{requestID}})
		if err != nil {
			return nil, err
		}
		s = prepared
	}
	var resultID uint
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		request, call, err := s.requestAndCallTx(tx, requestID, true)
		if err != nil {
			return err
		}
		manager, err := s.isManager(actorID)
		if err != nil {
			return err
		}
		if request.RequestedByUserID != actorID && !manager {
			return ErrForbidden
		}
		if request.Status == RequestStatusCompleted || request.Status == RequestStatusCancelled || request.Status == RequestStatusRejected {
			return ErrInvalidStatusChange
		}
		request.Status, call.Status = RequestStatusCancelled, RequestStatusCancelled
		s.markZoomSyncPending(call)
		if err := tx.Save(request).Error; err != nil {
			return err
		}
		if err := tx.Save(call).Error; err != nil {
			return err
		}
		resultID = request.ID
		return s.recordAudit(tx, "support_request", &request.ID, "support_request_cancelled", &actorID, nil)
	})
	if err != nil {
		return nil, err
	}
	_ = s.syncZoomMeetingForRequest(resultID)
	request, err := s.request(resultID)
	if err == nil {
		s.notifyRequest(request, "Support call request cancelled")
	}
	return request, err
}

func (s *Service) CompleteCall(actorID, callID uint, input CompleteCallInput) (*SupportCall, error) {
	start, err := time.Parse(time.RFC3339, strings.TrimSpace(input.ActualStart))
	if err != nil {
		return nil, fmt.Errorf("%w: actual_start must be RFC3339", ErrInvalidInput)
	}
	end, err := time.Parse(time.RFC3339, strings.TrimSpace(input.ActualEnd))
	if err != nil || !end.After(start) {
		return nil, fmt.Errorf("%w: actual_end must be after actual_start", ErrInvalidInput)
	}
	minutes := int(end.Sub(start).Minutes())
	if minutes < 1 || minutes > 12*60 || len(strings.TrimSpace(input.InternalNotes)) > 4000 {
		return nil, fmt.Errorf("%w: invalid actual duration or internal notes", ErrInvalidInput)
	}
	var resultID uint
	if s.Directory != nil {
		prepared, err := s.prepareIdentity(userScope{Extra: []uint{actorID}, Calls: []uint{callID}})
		if err != nil {
			return nil, err
		}
		s = prepared
	}
	err = s.DB.Transaction(func(tx *gorm.DB) error {
		var call SupportCall
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&call, callID).Error; err != nil {
			return mapNotFound(err)
		}
		manager, err := s.isManager(actorID)
		if err != nil {
			return err
		}
		if call.AssignedStaffID == nil || (*call.AssignedStaffID != actorID && !manager) {
			return ErrForbidden
		}
		if !manager {
			if err := s.requireSupportAdmin(actorID); err != nil {
				return err
			}
		}
		if call.Status != RequestStatusApproved {
			return ErrInvalidStatusChange
		}
		var request SupportCallRequest
		if err := tx.First(&request, call.SupportRequestID).Error; err != nil {
			return mapNotFound(err)
		}
		now := s.now()
		call.ActualStartTime, call.ActualEndTime, call.ActualDurationMinutes = &start, &end, minutes
		call.CompletedByID, call.CompletedAt, call.Status, call.InternalNotes = &actorID, &now, RequestStatusCompleted, strings.TrimSpace(input.InternalNotes)
		request.Status = RequestStatusCompleted
		if err := tx.Save(&call).Error; err != nil {
			return err
		}
		if err := tx.Save(&request).Error; err != nil {
			return err
		}
		resultID = call.ID
		return s.recordAudit(tx, "support_call", &call.ID, "actual_duration_recorded", &actorID, map[string]interface{}{
			"actual_start": start, "actual_end": end, "actual_duration_minutes": minutes,
		})
	})
	if err != nil {
		return nil, err
	}
	var call SupportCall
	if err := s.DB.Scopes(s.userPreload("AssignedStaff")).First(&call, resultID).Error; err != nil {
		return nil, err
	}
	if err := s.hydrate(s.DB, callRefTargets(&call)); err != nil {
		return nil, err
	}
	if request, requestErr := s.request(call.SupportRequestID); requestErr == nil {
		s.notifyRequest(request, "Support call completed")
	}
	return &call, nil
}

func (s *Service) ReassignCall(actorID, callID uint, input ReassignInput) (*SupportCall, error) {
	if err := s.requireManager(actorID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(input.Reason) == "" {
		return nil, fmt.Errorf("%w: reassignment reason is required", ErrInvalidInput)
	}
	if err := s.requireSupportAdmin(input.UserID); err != nil {
		return nil, fmt.Errorf("%w: replacement must be an active support admin", ErrInvalidInput)
	}
	var resultID uint
	if s.Directory != nil {
		prepared, err := s.prepareIdentity(userScope{Extra: []uint{actorID, input.UserID}, Calls: []uint{callID}})
		if err != nil {
			return nil, err
		}
		s = prepared
	}
	var previousAssigneeID *uint
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		var call SupportCall
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&call, callID).Error; err != nil {
			return mapNotFound(err)
		}
		if call.Status == RequestStatusCompleted || call.Status == RequestStatusCancelled || call.Status == RequestStatusRejected {
			return ErrInvalidStatusChange
		}
		if s.slotUnavailableTx(tx, input.UserID, call.ScheduledStartTime, call.ScheduledEndTime, call.ID) {
			return ErrUnavailable
		}
		var request SupportCallRequest
		if err := tx.First(&request, call.SupportRequestID).Error; err != nil {
			return mapNotFound(err)
		}
		previous := call.AssignedStaffID
		if previous != nil {
			value := *previous
			previousAssigneeID = &value
		}
		call.AssignedStaffID, request.AssignedStaffID, call.Status, request.Status = &input.UserID, &input.UserID, RequestStatusApproved, RequestStatusApproved
		s.markZoomSyncPending(&call)
		request.RejectionReason = strings.TrimSpace(input.Reason)
		if err := tx.Save(&call).Error; err != nil {
			return mapSlotConflict(err)
		}
		if err := tx.Save(&request).Error; err != nil {
			return err
		}
		resultID = call.ID
		return s.recordAudit(tx, "support_call", &call.ID, "manager_call_reassignment", &actorID, map[string]interface{}{
			"previous_assignee_id": previous, "new_assignee_id": input.UserID, "reason": input.Reason,
		})
	})
	if err != nil {
		return nil, err
	}
	_ = s.syncZoomMeeting(resultID)
	var call SupportCall
	if err := s.DB.Scopes(s.userPreload("AssignedStaff")).First(&call, resultID).Error; err != nil {
		return nil, err
	}
	if err := s.hydrate(s.DB, callRefTargets(&call)); err != nil {
		return nil, err
	}
	if request, requestErr := s.request(call.SupportRequestID); requestErr == nil {
		var previousAssignee *SupportUser
		if previousAssigneeID != nil && *previousAssigneeID != input.UserID {
			previousAssignee, _ = s.user(*previousAssigneeID)
		}
		s.notifyRequest(request, "Support call details changed", previousAssignee)
	}
	return &call, nil
}

func (s *Service) ReassignDay(actorID uint, date string, input ReassignInput) (*SupportAssignment, error) {
	if err := s.requireManager(actorID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(input.Reason) == "" {
		return nil, fmt.Errorf("%w: reassignment reason is required", ErrInvalidInput)
	}
	if err := s.requireSupportAdmin(input.UserID); err != nil {
		return nil, fmt.Errorf("%w: replacement must be an active support admin", ErrInvalidInput)
	}
	settings, err := s.settings()
	if err != nil {
		return nil, err
	}
	loc, err := s.location(settings)
	if err != nil {
		return nil, err
	}
	dateTime, err := parseDateAndTime(date, "00:00", loc)
	if err != nil {
		return nil, err
	}
	date = scheduleDateFor(dateTime, loc)
	if !isSupportBusinessDay(date) {
		return nil, fmt.Errorf("%w: support is available Monday through Friday only", ErrUnavailable)
	}
	if unavailable, err := s.fullDayUnavailableTx(s.DB, input.UserID, date); err != nil {
		return nil, err
	} else if unavailable {
		return nil, ErrUnavailable
	}
	if err := s.EnsureRollingSchedule(); err != nil {
		return nil, err
	}
	var resultID uint
	if s.Directory != nil {
		prepared, err := s.prepareIdentity(userScope{Extra: []uint{actorID, input.UserID}, Dates: []string{date}})
		if err != nil {
			return nil, err
		}
		s = prepared
	}
	err = s.DB.Transaction(func(tx *gorm.DB) error {
		var assignment SupportAssignment
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("assignment_date = ?", date).First(&assignment).Error; err != nil {
			return mapNotFound(err)
		}
		previous := assignment.PrimaryAssigneeID
		assignment.PrimaryAssigneeID, assignment.PreviousAssigneeID = &input.UserID, previous
		assignment.ReassignedByID, assignment.AssignmentSource, assignment.ReassignmentReason = &actorID, AssignmentSourceManager, strings.TrimSpace(input.Reason)
		if err := tx.Save(&assignment).Error; err != nil {
			return err
		}
		resultID = assignment.ID
		return s.recordAudit(tx, "assignment", &assignment.ID, "manager_assignment_reassignment", &actorID, map[string]interface{}{
			"assignment_date": date, "previous_assignee_id": previous, "new_assignee_id": input.UserID, "reason": input.Reason,
		})
	})
	if err != nil {
		return nil, err
	}
	var assignment SupportAssignment
	if err := s.DB.Scopes(s.userPreload("PrimaryAssignee")).Scopes(s.userPreload("PreviousAssignee")).First(&assignment, resultID).Error; err != nil {
		return nil, err
	}
	if err := s.hydrate(s.DB, assignmentTargets(&assignment)); err != nil {
		return nil, err
	}
	s.notifyAssignmentChange(date, input.Reason)
	return &assignment, nil
}

func (s *Service) CreateAvailability(actorID uint, input CreateAvailabilityInput) (*StaffAvailability, error) {
	if err := s.requireSupportAdmin(actorID); err != nil {
		return nil, err
	}
	record, err := s.validateAvailabilityInput(actorID, input)
	if err != nil {
		return nil, err
	}
	if err := s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(record).Error; err != nil {
			return err
		}
		return s.recordAudit(tx, "availability", &record.ID, "availability_created", &actorID, map[string]interface{}{
			"availability_date": record.AvailabilityDate, "full_day_unavailable": record.FullDayUnavailable,
		})
	}); err != nil {
		return nil, err
	}
	if record.FullDayUnavailable {
		if err := s.reassignUnavailableDay(record.AvailabilityDate, "primary assignee marked fully unavailable"); err != nil {
			return nil, err
		}
	} else {
		if err := s.markCallsForAvailability(record, "support admin became partially unavailable"); err != nil {
			return nil, err
		}
	}
	return record, nil
}

func (s *Service) CreateUnavailability(actorID uint, input CreateUnavailabilityInput) (*StaffAvailability, error) {
	return s.CreateAvailability(actorID, CreateAvailabilityInput(input))
}

func (s *Service) UpdateAvailability(actorID, availabilityID uint, input UpdateAvailabilityInput) (*StaffAvailability, error) {
	if err := s.requireSupportAdmin(actorID); err != nil {
		return nil, err
	}
	replacement, err := s.validateAvailabilityInput(actorID, CreateAvailabilityInput(input))
	if err != nil {
		return nil, err
	}
	if err := s.DB.Transaction(func(tx *gorm.DB) error {
		var record StaffAvailability
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&record, availabilityID).Error; err != nil {
			return mapNotFound(err)
		}
		if record.StaffID != actorID {
			return ErrForbidden
		}
		record.AvailabilityDate, record.FullDayUnavailable, record.UnavailableStartTime, record.UnavailableEndTime, record.Reason = replacement.AvailabilityDate, replacement.FullDayUnavailable, replacement.UnavailableStartTime, replacement.UnavailableEndTime, replacement.Reason
		*replacement = record
		if err := tx.Save(&record).Error; err != nil {
			return err
		}
		return s.recordAudit(tx, "availability", &record.ID, "availability_updated", &actorID, map[string]interface{}{"availability_date": record.AvailabilityDate})
	}); err != nil {
		return nil, err
	}
	if replacement.FullDayUnavailable {
		if err := s.reassignUnavailableDay(replacement.AvailabilityDate, "primary assignee marked fully unavailable"); err != nil {
			return nil, err
		}
	} else if err := s.markCallsForAvailability(replacement, "support admin availability changed"); err != nil {
		return nil, err
	}
	return replacement, nil
}

func (s *Service) DeleteAvailability(actorID, availabilityID uint) error {
	if err := s.requireSupportAdmin(actorID); err != nil {
		return err
	}
	return s.DB.Transaction(func(tx *gorm.DB) error {
		var record StaffAvailability
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&record, availabilityID).Error; err != nil {
			return mapNotFound(err)
		}
		if record.StaffID != actorID {
			return ErrForbidden
		}
		if err := tx.Delete(&record).Error; err != nil {
			return err
		}
		return s.recordAudit(tx, "availability", &availabilityID, "availability_deleted", &actorID, map[string]interface{}{"availability_date": record.AvailabilityDate})
	})
}

func (s *Service) validateAvailabilityInput(actorID uint, input CreateAvailabilityInput) (*StaffAvailability, error) {
	settings, err := s.settings()
	if err != nil {
		return nil, err
	}
	loc, err := s.location(settings)
	if err != nil {
		return nil, err
	}
	record := &StaffAvailability{StaffID: actorID, FullDayUnavailable: input.FullDayUnavailable, Reason: strings.TrimSpace(input.Reason)}
	if len(record.Reason) > 1000 {
		return nil, fmt.Errorf("%w: reason must be at most 1000 characters", ErrInvalidInput)
	}
	if input.FullDayUnavailable {
		date, err := parseDateAndTime(input.Date, "00:00", loc)
		if err != nil {
			return nil, fmt.Errorf("%w: date is required for a full-day unavailability", ErrInvalidInput)
		}
		record.AvailabilityDate = scheduleDateFor(date, loc)
		if !isSupportBusinessDay(record.AvailabilityDate) {
			return nil, fmt.Errorf("%w: support availability can be updated Monday through Friday only", ErrUnavailable)
		}
		return record, nil
	}
	start, err := time.Parse(time.RFC3339, strings.TrimSpace(input.StartsAt))
	if err != nil {
		return nil, fmt.Errorf("%w: starts_at must be RFC3339", ErrInvalidInput)
	}
	end, err := time.Parse(time.RFC3339, strings.TrimSpace(input.EndsAt))
	if err != nil || !end.After(start) {
		return nil, fmt.Errorf("%w: ends_at must be after starts_at", ErrInvalidInput)
	}
	start, end = start.In(loc), end.In(loc)
	date := scheduleDateFor(start, loc)
	if scheduleDateFor(end.Add(-time.Nanosecond), loc) != date {
		return nil, fmt.Errorf("%w: a partial unavailability must stay within one calendar day", ErrInvalidInput)
	}
	if !isSupportBusinessDay(date) {
		return nil, fmt.Errorf("%w: support availability can be updated Monday through Friday only", ErrUnavailable)
	}
	if strings.TrimSpace(input.Date) != "" && strings.TrimSpace(input.Date) != date {
		return nil, fmt.Errorf("%w: date must match starts_at", ErrInvalidInput)
	}
	record.AvailabilityDate, record.UnavailableStartTime, record.UnavailableEndTime = date, &start, &end
	return record, nil
}

func (s *Service) ListUnavailability(actorID uint) ([]StaffAvailability, error) {
	manager, err := s.isManager(actorID)
	if err != nil {
		return nil, err
	}
	if !manager {
		if err := s.requireSupportAdmin(actorID); err != nil {
			return nil, err
		}
	}
	query := s.DB.Scopes(s.userPreload("Staff")).Order("availability_date ASC, unavailable_start_time ASC")
	if !manager {
		query = query.Where("staff_id = ?", actorID)
	}
	var records []StaffAvailability
	if err := query.Find(&records).Error; err != nil {
		return nil, err
	}
	if err := s.hydrate(s.DB, availabilityTargets(records)); err != nil {
		return nil, err
	}
	return records, nil
}

func (s *Service) GetProfile(actorID uint) (*SupportProfile, error) {
	if err := s.requireSupportAdmin(actorID); err != nil {
		return nil, err
	}
	if err := s.EnsureRollingSchedule(); err != nil {
		return nil, err
	}
	profile := &SupportProfile{}
	if err := s.DB.Scopes(s.userPreload("PrimaryAssignee")).Where("primary_assignee_id = ? AND assignment_date >= ?", actorID, s.today()).Order("assignment_date ASC").Find(&profile.Assignments).Error; err != nil {
		return nil, err
	}
	if err := s.DB.Scopes(s.userPreload("AssignedStaff")).Where("assigned_staff_id = ? AND scheduled_start_time >= ? AND status = ?", actorID, s.now(), RequestStatusApproved).Order("scheduled_start_time ASC").Find(&profile.UpcomingCalls).Error; err != nil {
		return nil, err
	}
	if err := s.requestQuery().Where("requested_staff_id = ? AND status IN ?", actorID, []string{RequestStatusAwaitingApproval, RequestStatusAlternativeProposed}).Order("created_at ASC").Find(&profile.DirectRequests).Error; err != nil {
		return nil, err
	}
	if err := s.DB.Scopes(s.userPreload("Staff")).Where("staff_id = ?", actorID).Order("availability_date ASC, unavailable_start_time ASC").Find(&profile.Availability).Error; err != nil {
		return nil, err
	}
	targets := append(primaryAssignmentsTargets(profile.Assignments), callTargets(profile.UpcomingCalls)...)
	targets = append(targets, availabilityTargets(profile.Availability)...)
	if err := s.hydrate(s.DB, targets); err != nil {
		return nil, err
	}
	return profile, nil
}

func (s *Service) reassignUnavailableDay(date, reason string) error {
	if !isSupportBusinessDay(date) {
		return nil
	}
	if s.Directory != nil {
		prepared, err := s.prepareIdentity(userScope{Dates: []string{date}})
		if err != nil {
			return err
		}
		s = prepared
	}
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		var assignment SupportAssignment
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("assignment_date = ?", date).First(&assignment).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		oldAssignee := assignment.PrimaryAssigneeID
		if oldAssignee != nil {
			unavailable, err := s.fullDayUnavailableTx(tx, *oldAssignee, date)
			if err != nil {
				return err
			}
			if !unavailable {
				return nil
			}
		}
		excluded := map[uint]struct{}{}
		if oldAssignee != nil {
			excluded[*oldAssignee] = struct{}{}
		}
		replacement, pickErr := s.pickFairStaffTx(tx, date, excluded)
		if pickErr != nil && !errors.Is(pickErr, ErrNoSupportStaff) {
			return pickErr
		}
		assignment.PreviousAssigneeID, assignment.ReassignedByID, assignment.ReassignmentReason = oldAssignee, nil, reason
		if errors.Is(pickErr, ErrNoSupportStaff) {
			assignment.PrimaryAssigneeID, assignment.AssignmentSource = nil, AssignmentSourceUncovered
		} else {
			assignment.PrimaryAssigneeID, assignment.AssignmentSource = &replacement, AssignmentSourceFullDay
		}
		if err := tx.Save(&assignment).Error; err != nil {
			return err
		}
		if err := s.recordAudit(tx, "assignment", &assignment.ID, "full_day_assignment_reassignment", nil, map[string]interface{}{
			"assignment_date": date, "previous_assignee_id": oldAssignee, "new_assignee_id": assignment.PrimaryAssigneeID, "reason": reason,
		}); err != nil {
			return err
		}
		if oldAssignee == nil {
			return nil
		}
		settings, err := s.settings()
		if err != nil {
			return err
		}
		dayStart, dayEnd, err := windowForDate(settings, date)
		if err != nil {
			return err
		}
		var calls []SupportCall
		if err := tx.Where("assigned_staff_id = ? AND scheduled_start_time >= ? AND scheduled_start_time < ? AND status IN ?", *oldAssignee, dayStart, dayEnd, reservingCallStatuses).Find(&calls).Error; err != nil {
			return err
		}
		for _, call := range calls {
			var request SupportCallRequest
			if err := tx.First(&request, call.SupportRequestID).Error; err != nil {
				return err
			}
			if request.RequestType == RequestTypeAutomaticDaily && assignment.PrimaryAssigneeID != nil && !s.slotUnavailableTx(tx, *assignment.PrimaryAssigneeID, call.ScheduledStartTime, call.ScheduledEndTime, call.ID) {
				call.AssignedStaffID, request.AssignedStaffID = assignment.PrimaryAssigneeID, assignment.PrimaryAssigneeID
				s.markZoomSyncPending(&call)
				if err := tx.Save(&call).Error; err != nil {
					return mapSlotConflict(err)
				}
				if err := tx.Save(&request).Error; err != nil {
					return err
				}
				if err := s.recordAudit(tx, "support_call", &call.ID, "full_day_call_reassignment", nil, map[string]interface{}{"new_assignee_id": assignment.PrimaryAssigneeID}); err != nil {
					return err
				}
				continue
			}
			call.Status, request.Status, request.RejectionReason = RequestStatusAlternativeProposed, RequestStatusAlternativeProposed, "Assigned support admin became unavailable: "+reason
			s.markZoomSyncPending(&call)
			if err := tx.Save(&call).Error; err != nil {
				return err
			}
			if err := tx.Save(&request).Error; err != nil {
				return err
			}
			if err := s.recordAudit(tx, "support_call", &call.ID, "call_needs_alternative_after_full_day_unavailability", nil, nil); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		_ = s.syncZoomMeetingsForDate(date)
		s.notifyAssignmentChange(date, reason)
	}
	return err
}

func (s *Service) markCallsForAvailability(availability *StaffAvailability, reason string) error {
	if availability == nil || availability.FullDayUnavailable || availability.UnavailableStartTime == nil || availability.UnavailableEndTime == nil {
		return nil
	}
	requestIDs := make([]uint, 0)
	if s.Directory != nil {
		prepared, err := s.prepareIdentity(userScope{Extra: []uint{availability.StaffID}, Dates: []string{availability.AvailabilityDate}})
		if err != nil {
			return err
		}
		s = prepared
	}
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		var calls []SupportCall
		if err := tx.Where("assigned_staff_id = ? AND scheduled_start_time < ? AND scheduled_end_time > ? AND status IN ?", availability.StaffID, *availability.UnavailableEndTime, *availability.UnavailableStartTime, reservingCallStatuses).Find(&calls).Error; err != nil {
			return err
		}
		for _, call := range calls {
			var request SupportCallRequest
			if err := tx.First(&request, call.SupportRequestID).Error; err != nil {
				return err
			}
			call.Status, request.Status, request.RejectionReason = RequestStatusAlternativeProposed, RequestStatusAlternativeProposed, reason
			s.markZoomSyncPending(&call)
			if err := tx.Save(&call).Error; err != nil {
				return err
			}
			if err := tx.Save(&request).Error; err != nil {
				return err
			}
			requestIDs = append(requestIDs, request.ID)
			if err := s.recordAudit(tx, "support_call", &call.ID, "call_needs_alternative_after_availability_change", &availability.StaffID, map[string]interface{}{"reason": reason}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, requestID := range requestIDs {
		_ = s.syncZoomMeetingForRequest(requestID)
		if request, requestErr := s.request(requestID); requestErr == nil {
			s.notifyRequest(request, "Support call needs a new time")
		}
	}
	return nil
}

func (s *Service) slotUnavailableTx(tx *gorm.DB, staffID uint, start, end time.Time, exceptCallID uint) bool {
	date := start.Format("2006-01-02")
	fullDay, err := s.fullDayUnavailableTx(tx, staffID, date)
	if err != nil || fullDay {
		return true
	}
	var unavailableCount int64
	if err := tx.Model(&StaffAvailability{}).
		Where("staff_id = ? AND availability_date = ? AND full_day_unavailable = ? AND unavailable_start_time < ? AND unavailable_end_time > ?", staffID, date, false, end, start).
		Count(&unavailableCount).Error; err != nil || unavailableCount > 0 {
		return true
	}
	query := tx.Model(&SupportCall{}).Where("assigned_staff_id = ? AND scheduled_start_time < ? AND scheduled_end_time > ? AND status IN ?", staffID, end, start, reservingCallStatuses)
	if exceptCallID > 0 {
		query = query.Where("id <> ?", exceptCallID)
	}
	var conflicts int64
	return query.Count(&conflicts).Error != nil || conflicts > 0
}

func (s *Service) parseAlternativeSlot(input RequestDecisionInput, requestedDate string) (time.Time, time.Time, error) {
	settings, err := s.settings()
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	loc, err := s.location(settings)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	start, err := time.Parse(time.RFC3339, strings.TrimSpace(input.AlternativeStart))
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: alternative_start must be RFC3339", ErrInvalidInput)
	}
	end, err := time.Parse(time.RFC3339, strings.TrimSpace(input.AlternativeEnd))
	if err != nil || !end.After(start) {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: alternative_end must be after alternative_start", ErrInvalidInput)
	}
	start, end = start.In(loc), end.In(loc)
	date := scheduleDateFor(start, loc)
	if date != requestedDate || scheduleDateFor(end.Add(-time.Nanosecond), loc) != date {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: alternative time must be on the requested date", ErrInvalidInput)
	}
	workdayStart, workdayEnd, err := windowForDate(settings, date)
	if err != nil || start.Before(workdayStart) || end.After(workdayEnd) {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: alternative time must be within support hours", ErrInvalidInput)
	}
	return start, end, nil
}

func (s *Service) requestQuery() *gorm.DB {
	return s.DB.Scopes(s.userPreload("RequestedBy")).Scopes(s.userPreload("RequestedStaff")).Scopes(s.userPreload("AssignedStaff")).Preload("Call")
}

func (s *Service) request(requestID uint) (*SupportCallRequest, error) {
	var request SupportCallRequest
	if err := s.requestQuery().First(&request, requestID).Error; err != nil {
		return nil, mapNotFound(err)
	}
	if err := s.hydrate(s.DB, requestTargets(&request)); err != nil {
		return nil, err
	}
	return &request, nil
}

func (s *Service) requestAndCallTx(tx *gorm.DB, requestID uint, lock bool) (*SupportCallRequest, *SupportCall, error) {
	requestQuery := tx
	callQuery := tx
	if lock {
		requestQuery = requestQuery.Clauses(clause.Locking{Strength: "UPDATE"})
		callQuery = callQuery.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var request SupportCallRequest
	if err := requestQuery.First(&request, requestID).Error; err != nil {
		return nil, nil, mapNotFound(err)
	}
	var call SupportCall
	if err := callQuery.Where("support_request_id = ?", request.ID).First(&call).Error; err != nil {
		return nil, nil, mapNotFound(err)
	}
	return &request, &call, nil
}

func mapNotFound(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	return err
}

func (s *Service) today() string {
	settings, err := s.settings()
	if err != nil {
		return s.now().Format("2006-01-02")
	}
	loc, err := s.location(settings)
	if err != nil {
		return s.now().Format("2006-01-02")
	}
	return scheduleDateFor(s.now(), loc)
}

func (s *Service) notifyRequest(request *SupportCallRequest, event string, formerAssignees ...*SupportUser) {
	if s.Outbox != nil {
		if request == nil {
			return
		}
		if request != nil {
			callID := uint(0)
			if request.Call != nil {
				callID = request.Call.ID
			}
			_ = s.DB.Model(&SupportScheduleAuditLog{}).Where("notification_state = ? AND ((entity_type = ? AND entity_id = ?) OR (entity_type = ? AND entity_id = ?))", "pending", "support_request", request.ID, "support_call", callID).Update("notification_next_attempt", time.Now().UTC()).Error
		}
		callID := uint(0)
		if request.Call != nil {
			callID = request.Call.ID
		}
		_ = s.recoverNotifications(requestscope.Context(s.DB), func(query *gorm.DB) *gorm.DB {
			return query.Where("((entity_type = ? AND entity_id = ?) OR (entity_type = ? AND entity_id = ?))", "support_request", request.ID, "support_call", callID)
		})
		return
	}
	if s.Mailer == nil || request == nil {
		return
	}
	recipients := make(map[string]supportCallEmailRecipient)
	addRecipient := func(user *SupportUser, audience supportCallNotificationAudience) {
		if user == nil {
			return
		}
		email := strings.TrimSpace(user.Email)
		if email == "" {
			return
		}
		if _, alreadyAdded := recipients[email]; !alreadyAdded {
			recipients[email] = supportCallEmailRecipient{Email: email, Name: supportUserName(user), Audience: audience}
		}
	}
	addRecipient(&request.RequestedBy, supportCallRequesterAudience)
	addRecipient(request.AssignedStaff, supportCallStaffAudience)
	for _, formerAssignee := range formerAssignees {
		addRecipient(formerAssignee, supportCallFormerStaffAudience)
	}
	addresses := make([]string, 0, len(recipients))
	for email := range recipients {
		addresses = append(addresses, email)
	}
	sort.Strings(addresses)
	for _, email := range addresses {
		recipient := recipients[email]
		// A confirmed call notification is the participant's meeting invite.
		// When Zoom is enabled, never send that invite before its join URL is
		// persisted. A successful maintenance retry sends the requester and the
		// assigned host a fresh "Zoom link ready" notification. A former host is
		// still told immediately that the call is no longer assigned to them.
		if request.Status == RequestStatusApproved && s.meetingIntegrationEnabled() &&
			(request.Call == nil || strings.TrimSpace(request.Call.ZoomJoinURL) == "") &&
			recipient.Audience != supportCallFormerStaffAudience {
			continue
		}
		if err := s.Mailer.Send([]string{recipient.Email}, supportCallNotificationSubject(request, event), supportCallNotificationBody(request, recipient, event)); err != nil {
			log.Printf("support-call email delivery failed for %s: %v", recipient.Email, err)
		}
	}
}

func (s *Service) notifyAssignmentChange(date, reason string) {
	if s.Outbox != nil {
		var assignment SupportAssignment
		if s.DB.Where("assignment_date = ?", date).First(&assignment).Error != nil {
			return
		}
		_ = s.recoverNotifications(requestscope.Context(s.DB), func(query *gorm.DB) *gorm.DB {
			return query.Where("entity_type = ? AND entity_id = ?", "assignment", assignment.ID)
		})
		return
	}
	if s.Mailer == nil {
		return
	}
	var assignment SupportAssignment
	if err := s.DB.Scopes(s.userPreload("PrimaryAssignee")).Scopes(s.userPreload("PreviousAssignee")).Where("assignment_date = ?", date).First(&assignment).Error; err != nil {
		return
	}
	if err := s.hydrate(s.DB, assignmentTargets(&assignment)); err != nil {
		return
	}
	recipients := make(map[string]supportCallEmailRecipient)
	addRecipient := func(user *SupportUser, audience supportCallNotificationAudience) {
		if user == nil {
			return
		}
		email := strings.TrimSpace(user.Email)
		if email == "" {
			return
		}
		if _, alreadyAdded := recipients[email]; !alreadyAdded {
			recipients[email] = supportCallEmailRecipient{Email: email, Name: supportUserName(user), Audience: audience}
		}
	}
	addRecipient(assignment.PrimaryAssignee, supportCallStaffAudience)
	addRecipient(assignment.PreviousAssignee, supportCallStaffAudience)
	if managers, err := s.roleUsers(s.DB, RoleManager); err == nil {
		for _, manager := range managers {
			addRecipient(&manager, supportCallManagerAudience)
		}
	}
	addresses := make([]string, 0, len(recipients))
	for address := range recipients {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	for _, email := range addresses {
		recipient := recipients[email]
		_ = s.Mailer.Send([]string{recipient.Email}, "[Nordik Drive] Support schedule updated", supportScheduleAssignmentEmailBody(&assignment, recipient, reason))
	}

	// A full-day reassignment can move normal requests or require a direct
	// request to choose another time. Notify each requester/assignee as well
	// as the old/new assignee and managers included by notifyRequest.
	var requests []SupportCallRequest
	if err := s.requestQuery().Where("requested_date = ?", date).Find(&requests).Error; err == nil {
		for index := range requests {
			s.notifyRequest(&requests[index], "A support schedule change affected your request")
		}
	}
}
