package supportschedule

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type scheduleFixture struct {
	service  *Service
	location *time.Location
	now      time.Time
	manager  uint
	staffA   uint
	staffB   uint
	survivor uint
}

type scheduleMail struct {
	To      []string
	Subject string
	Body    string
}

type scheduleMailer struct{ sent []scheduleMail }

func (m *scheduleMailer) Send(to []string, subject, body string) error {
	m.sent = append(m.sent, scheduleMail{To: append([]string(nil), to...), Subject: subject, Body: body})
	return nil
}

func newScheduleFixture(t *testing.T) scheduleFixture {
	t.Helper()
	location, err := time.LoadLocation(DefaultTimeZone)
	if err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := createSupportScheduleTestSchema(db); err != nil {
		t.Fatal(err)
	}
	users := []SupportUser{
		{ID: 1, FirstName: "Maya", LastName: "Manager", Email: "manager@example.test", Role: RoleManager},
		{ID: 2, FirstName: "Alex", LastName: "Support", Email: "alex@example.test", Role: RoleAdmin},
		{ID: 3, FirstName: "Blair", LastName: "Support", Email: "blair@example.test", Role: RoleAdmin},
		{ID: 4, FirstName: "Sam", LastName: "Survivor", Email: "sam@example.test", Role: "User"},
	}
	if err := db.Create(&users).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 3, 8, 0, 0, 0, location)
	service := &Service{DB: db, Clock: func() time.Time { return now }}
	for _, id := range []uint{2, 3} {
		if _, err := service.SetTeamMember(1, id, true); err != nil {
			t.Fatalf("set team member %d: %v", id, err)
		}
	}
	return scheduleFixture{service: service, location: location, now: now, manager: 1, staffA: 2, staffB: 3, survivor: 4}
}

// The test schema is explicit to ensure the package never masks a missing
// production column through GORM AutoMigrate.
func createSupportScheduleTestSchema(db *gorm.DB) error {
	statements := []string{
		`CREATE TABLE users (id INTEGER PRIMARY KEY, firstname TEXT NOT NULL, lastname TEXT NOT NULL, email TEXT NOT NULL, role TEXT NOT NULL)`,
		`CREATE TABLE support_schedule_settings (id INTEGER PRIMARY KEY, time_zone TEXT NOT NULL, workday_start TEXT NOT NULL, workday_end TEXT NOT NULL, allowed_durations_json JSON NOT NULL, default_duration_minutes INTEGER NOT NULL, booking_horizon_days INTEGER NOT NULL, updated_by_id INTEGER, created_at DATETIME, updated_at DATETIME)`,
		`CREATE TABLE support_team_members (id INTEGER PRIMARY KEY AUTOINCREMENT, user_id INTEGER NOT NULL UNIQUE, is_active BOOLEAN NOT NULL, added_by_id INTEGER, created_at DATETIME, updated_at DATETIME)`,
		`CREATE TABLE support_assignments (id INTEGER PRIMARY KEY AUTOINCREMENT, assignment_date TEXT NOT NULL UNIQUE, primary_assignee_id INTEGER, assignment_source TEXT NOT NULL, previous_assignee_id INTEGER, reassigned_by_id INTEGER, reassignment_reason TEXT NOT NULL DEFAULT '', created_at DATETIME, updated_at DATETIME)`,
		`CREATE TABLE support_staff_availabilities (id INTEGER PRIMARY KEY AUTOINCREMENT, staff_id INTEGER NOT NULL, availability_date TEXT NOT NULL, full_day_unavailable BOOLEAN NOT NULL, unavailable_start_time DATETIME, unavailable_end_time DATETIME, reason TEXT NOT NULL, created_at DATETIME, updated_at DATETIME)`,
		`CREATE TABLE support_call_requests (id INTEGER PRIMARY KEY AUTOINCREMENT, requested_by_user_id INTEGER NOT NULL, request_type TEXT NOT NULL, requested_date TEXT NOT NULL, preferred_start_time DATETIME, preferred_end_time DATETIME, requested_staff_id INTEGER, assigned_staff_id INTEGER, status TEXT NOT NULL, subject TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', rejection_reason TEXT NOT NULL DEFAULT '', alternative_start_time DATETIME, alternative_end_time DATETIME, created_at DATETIME, updated_at DATETIME)`,
		`CREATE TABLE support_calls (id INTEGER PRIMARY KEY AUTOINCREMENT, support_request_id INTEGER NOT NULL UNIQUE, assigned_staff_id INTEGER, scheduled_start_time DATETIME NOT NULL, scheduled_end_time DATETIME NOT NULL, actual_start_time DATETIME, actual_end_time DATETIME, actual_duration_minutes INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL, internal_notes TEXT NOT NULL DEFAULT '', zoom_meeting_id TEXT NOT NULL DEFAULT '', zoom_join_url TEXT NOT NULL DEFAULT '', zoom_passcode TEXT NOT NULL DEFAULT '', zoom_host_email TEXT NOT NULL DEFAULT '', zoom_sync_status TEXT NOT NULL DEFAULT 'not_requested', zoom_sync_error TEXT NOT NULL DEFAULT '', zoom_synced_at DATETIME, completed_by_id INTEGER, completed_at DATETIME, created_at DATETIME, updated_at DATETIME)`,
		`CREATE TABLE support_schedule_audit_logs (id INTEGER PRIMARY KEY AUTOINCREMENT, entity_type TEXT NOT NULL, entity_id INTEGER, action TEXT NOT NULL, actor_id INTEGER, details JSON NOT NULL, created_at DATETIME, notification_state TEXT NOT NULL DEFAULT 'ignored', notification_snapshot TEXT NOT NULL DEFAULT '', notification_next_attempt DATETIME)`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}

func TestPostgresInitSQLDefinesRedesignedSupportScheduleSchema(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "db-init", "init.sql"))
	if err != nil {
		t.Fatal(err)
	}
	sql := string(data)
	for _, required := range []string{
		"CREATE TABLE IF NOT EXISTS support_assignments",
		"CREATE TABLE IF NOT EXISTS support_staff_availabilities",
		"CREATE TABLE IF NOT EXISTS support_call_requests",
		"CREATE TABLE IF NOT EXISTS support_calls",
		"CREATE TABLE IF NOT EXISTS support_schedule_audit_logs",
		"support_calls_no_overlapping_slots",
		"zoom_meeting_id",
		"zoom_sync_status",
		"ADD COLUMN IF NOT EXISTS zoom_meeting_id",
		"ADD COLUMN IF NOT EXISTS zoom_join_url",
		"ADD COLUMN IF NOT EXISTS zoom_passcode",
		"ADD COLUMN IF NOT EXISTS zoom_sync_status",
		"('Manager',      0)",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("db-init/init.sql is missing %q", required)
		}
	}
	if strings.Contains(sql, "CREATE TABLE IF NOT EXISTS support_daily_assignments") || strings.Contains(sql, "CREATE TABLE IF NOT EXISTS support_staff_unavailabilities") {
		t.Fatal("legacy support scheduling tables must not be recreated by init.sql")
	}
}

func TestSettingsRepairsAnInvalidStoredTimeZone(t *testing.T) {
	f := newScheduleFixture(t)
	if _, err := f.service.settings(); err != nil {
		t.Fatal(err)
	}
	if err := f.service.DB.Model(&SupportScheduleSettings{}).Where("id = ?", 1).Update("time_zone", "not/a-real-time-zone").Error; err != nil {
		t.Fatal(err)
	}

	settings, err := f.service.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.TimeZone != DefaultTimeZone {
		t.Fatalf("time zone = %q, want %q", settings.TimeZone, DefaultTimeZone)
	}
}

func TestEmptySupportTeamBootstrapsExistingAdminsForSelection(t *testing.T) {
	f := newScheduleFixture(t)
	if err := f.service.DB.Exec("DELETE FROM support_team_members").Error; err != nil {
		t.Fatal(err)
	}

	staff, err := f.service.ListSelectableStaff()
	if err != nil {
		t.Fatal(err)
	}
	if len(staff) != 2 || staff[0].FirstName != "Alex" || staff[1].FirstName != "Blair" {
		t.Fatalf("selectable staff = %#v, want both existing Admin users", staff)
	}
	var members int64
	if err := f.service.DB.Model(&SupportTeamMember{}).Where("is_active = ?", true).Count(&members).Error; err != nil {
		t.Fatal(err)
	}
	if members != 2 {
		t.Fatalf("active support-team members = %d, want 2", members)
	}
}

func (f scheduleFixture) at(dayOffset, hour, minute int) time.Time {
	date := f.now.AddDate(0, 0, dayOffset)
	return time.Date(date.Year(), date.Month(), date.Day(), hour, minute, 0, 0, f.location)
}

func (f scheduleFixture) date(dayOffset int) string {
	return scheduleDateFor(f.now.AddDate(0, 0, dayOffset), f.location)
}

func (f scheduleFixture) assignedStaff(t *testing.T, dayOffset int) uint {
	t.Helper()
	if err := f.service.EnsureRollingSchedule(); err != nil {
		t.Fatal(err)
	}
	var assignment SupportAssignment
	if err := f.service.DB.Where("assignment_date = ?", f.date(dayOffset)).First(&assignment).Error; err != nil {
		t.Fatal(err)
	}
	if assignment.PrimaryAssigneeID == nil {
		t.Fatalf("date %s does not have an assigned support person", f.date(dayOffset))
	}
	return *assignment.PrimaryAssigneeID
}

func (f scheduleFixture) otherStaff(assigned uint) uint {
	if assigned == f.staffA {
		return f.staffB
	}
	return f.staffA
}

func TestInitialScheduleCreatesExactlyFourteenCalendarAssignmentsAndIsIdempotent(t *testing.T) {
	f := newScheduleFixture(t)
	if err := f.service.EnsureRollingSchedule(); err != nil {
		t.Fatal(err)
	}
	if err := f.service.EnsureRollingSchedule(); err != nil {
		t.Fatal(err)
	}
	assignments, err := f.service.ListSchedule("", "")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(assignments), 14; got != want {
		t.Fatalf("assignments = %d, want %d", got, want)
	}
	for index, assignment := range assignments {
		if assignment.AssignmentDate != f.date(index) {
			t.Fatalf("unexpected assignment at %d: %#v", index, assignment)
		}
		weekend := f.at(index, 12, 0).Weekday() == time.Saturday || f.at(index, 12, 0).Weekday() == time.Sunday
		if weekend {
			if assignment.PrimaryAssigneeID != nil || assignment.AssignmentSource != AssignmentSourceUncovered {
				t.Fatalf("weekend assignment at %d = %#v, want uncovered with no assignee", index, assignment)
			}
			continue
		}
		if assignment.PrimaryAssigneeID == nil || assignment.AssignmentSource != AssignmentSourceAutomatic {
			t.Fatalf("weekday assignment at %d = %#v, want automatic assignee", index, assignment)
		}
	}
	var count int64
	if err := f.service.DB.Model(&SupportAssignment{}).Count(&count).Error; err != nil || count != 14 {
		t.Fatalf("assignment count = %d, err = %v", count, err)
	}
}

func TestWeekendSupportIsUnavailable(t *testing.T) {
	f := newScheduleFixture(t)
	if err := f.service.EnsureRollingSchedule(); err != nil {
		t.Fatal(err)
	}
	weekend := f.date(5) // The fixture starts on Monday.
	if _, err := f.service.ListAvailability(weekend, 30, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("weekend availability error = %v, want ErrUnavailable", err)
	}
	if _, err := f.service.CreateCall(f.survivor, CreateCallInput{
		Subject: "Weekend call", ScheduledStart: f.at(5, 10, 0).Format(time.RFC3339), DurationMinutes: 30,
	}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("weekend call error = %v, want ErrUnavailable", err)
	}
	if _, err := f.service.ReassignDay(f.manager, weekend, ReassignInput{UserID: f.staffA, Reason: "coverage"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("weekend reassignment error = %v, want ErrUnavailable", err)
	}
	if _, err := f.service.CreateAvailability(f.staffA, CreateAvailabilityInput{Date: weekend, FullDayUnavailable: true, Reason: "away"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("weekend availability update error = %v, want ErrUnavailable", err)
	}
	weekendStart := f.at(5, 10, 0)
	if _, err := f.service.CreateAvailability(f.staffA, CreateAvailabilityInput{
		Date: weekend, StartsAt: weekendStart.Format(time.RFC3339), EndsAt: weekendStart.Add(30 * time.Minute).Format(time.RFC3339), Reason: "appointment",
	}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("weekend partial availability update error = %v, want ErrUnavailable", err)
	}
}

func TestExistingWeekendAssignmentIsUncoveredDuringMaintenance(t *testing.T) {
	f := newScheduleFixture(t)
	if err := f.service.EnsureRollingSchedule(); err != nil {
		t.Fatal(err)
	}
	weekend := f.date(5) // The fixture starts on Monday.
	var assignment SupportAssignment
	if err := f.service.DB.Where("assignment_date = ?", weekend).First(&assignment).Error; err != nil {
		t.Fatal(err)
	}
	assignment.PrimaryAssigneeID = &f.staffA
	assignment.AssignmentSource = AssignmentSourceManager
	if err := f.service.DB.Save(&assignment).Error; err != nil {
		t.Fatal(err)
	}

	if err := f.service.EnsureRollingSchedule(); err != nil {
		t.Fatal(err)
	}
	if err := f.service.DB.Where("assignment_date = ?", weekend).First(&assignment).Error; err != nil {
		t.Fatal(err)
	}
	if assignment.PrimaryAssigneeID != nil || assignment.AssignmentSource != AssignmentSourceUncovered {
		t.Fatalf("normalized weekend assignment = %#v, want uncovered with no assignee", assignment)
	}
}

func TestFullDayUnavailabilityReassignsUsingFairRulesAndAudits(t *testing.T) {
	f := newScheduleFixture(t)
	if err := f.service.EnsureRollingSchedule(); err != nil {
		t.Fatal(err)
	}
	date := f.date(1)
	if _, err := f.service.ReassignDay(f.manager, date, ReassignInput{UserID: f.staffA, Reason: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.CreateAvailability(f.staffA, CreateAvailabilityInput{Date: date, FullDayUnavailable: true, Reason: "away"}); err != nil {
		t.Fatal(err)
	}
	assignments, err := f.service.ListSchedule(date, date)
	if err != nil {
		t.Fatal(err)
	}
	if len(assignments) != 1 || assignments[0].PrimaryAssigneeID == nil || *assignments[0].PrimaryAssigneeID != f.staffB || assignments[0].AssignmentSource != AssignmentSourceFullDay {
		t.Fatalf("expected reassignment to staff B, got %#v", assignments)
	}
	var audits int64
	if err := f.service.DB.Model(&SupportScheduleAuditLog{}).Where("action = ?", "full_day_assignment_reassignment").Count(&audits).Error; err != nil || audits != 1 {
		t.Fatalf("full-day reassignment audit count = %d, err = %v", audits, err)
	}
}

func TestPartialAvailabilityAndPendingRequestReserveSlots(t *testing.T) {
	f := newScheduleFixture(t)
	if err := f.service.EnsureRollingSchedule(); err != nil {
		t.Fatal(err)
	}
	date := f.date(1)
	if _, err := f.service.ReassignDay(f.manager, date, ReassignInput{UserID: f.staffA, Reason: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.CreateAvailability(f.staffA, CreateAvailabilityInput{
		StartsAt: f.at(1, 10, 0).Format(time.RFC3339), EndsAt: f.at(1, 11, 0).Format(time.RFC3339), Reason: "appointment",
	}); err != nil {
		t.Fatal(err)
	}
	availability, err := f.service.ListAvailability(date, 30, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, slot := range availability.Slots {
		if slot.StartAt.Equal(f.at(1, 10, 0)) || slot.StartAt.Equal(f.at(1, 10, 30)) {
			t.Fatalf("partial-unavailability slot was returned: %#v", slot)
		}
	}
	input := CreateCallInput{ScheduledStart: f.at(1, 11, 0).Format(time.RFC3339), DurationMinutes: 30, Subject: "Need help"}
	request, err := f.service.CreateCall(f.survivor, input)
	if err != nil {
		t.Fatal(err)
	}
	if request.RequestType != RequestTypeAutomaticDaily || request.Status != RequestStatusApproved || request.AssignedStaffID == nil || *request.AssignedStaffID != f.staffA {
		t.Fatalf("normal request was not correctly routed: %#v", request)
	}
	if _, err := f.service.CreateCall(f.survivor, input); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("overlapping scheduled request error = %v, want ErrUnavailable", err)
	}
}

func TestCalendarSummarizesDayAvailabilityAndCalls(t *testing.T) {
	f := newScheduleFixture(t)
	if err := f.service.EnsureRollingSchedule(); err != nil {
		t.Fatal(err)
	}
	date := f.date(1)
	if _, err := f.service.ReassignDay(f.manager, date, ReassignInput{UserID: f.staffA, Reason: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.CreateAvailability(f.staffA, CreateAvailabilityInput{
		StartsAt: f.at(1, 9, 0).Format(time.RFC3339), EndsAt: f.at(1, 10, 0).Format(time.RFC3339), Reason: "appointment",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.CreateCall(f.survivor, CreateCallInput{ScheduledStart: f.at(1, 10, 0).Format(time.RFC3339), DurationMinutes: 30, Subject: "Calendar test"}); err != nil {
		t.Fatal(err)
	}

	calendar, err := f.service.ListCalendar(f.manager, 30, nil)
	if err != nil {
		t.Fatal(err)
	}
	var selected, weekend *CalendarDay
	for index := range calendar.Days {
		if calendar.Days[index].Date == date {
			selected = &calendar.Days[index]
		}
		if calendar.Days[index].Date == f.date(5) {
			weekend = &calendar.Days[index]
		}
	}
	if selected == nil || selected.AssignedStaff == nil || selected.AssignedStaff.ID != f.staffA || selected.Status != "partial_availability" || !selected.IsBookable || selected.ScheduledCallCount != 1 || len(selected.UnavailablePeriods) != 1 || len(selected.ScheduledCalls) != 1 {
		t.Fatalf("calendar day = %#v, want assigned partial-availability day with one call", selected)
	}
	if weekend == nil || weekend.Status != "weekend" || weekend.IsBookable {
		t.Fatalf("weekend calendar day = %#v, want unbookable weekend", weekend)
	}
}

func TestSpecificPersonApprovalCompletionAndFairnessUseActualDuration(t *testing.T) {
	f := newScheduleFixture(t)
	if err := f.service.EnsureRollingSchedule(); err != nil {
		t.Fatal(err)
	}
	requestedStaff := f.otherStaff(f.assignedStaff(t, 1))
	request, err := f.service.CreateCall(f.survivor, CreateCallInput{
		ScheduledStart: f.at(1, 10, 0).Format(time.RFC3339), DurationMinutes: 30,
		Subject: "Help with my request", Message: "Please review", RequestedStaffID: &requestedStaff,
	})
	if err != nil {
		t.Fatal(err)
	}
	if request.Status != RequestStatusAwaitingApproval || request.RequestType != RequestTypeSpecificStaff {
		t.Fatalf("specific-person request status = %q", request.Status)
	}
	request, err = f.service.DecideRequest(requestedStaff, request.ID, RequestDecisionInput{Decision: "approve"})
	if err != nil {
		t.Fatal(err)
	}
	if request.Status != RequestStatusApproved || request.Call == nil {
		t.Fatalf("approved request = %#v", request)
	}
	actualStart := f.at(1, 10, 5)
	call, err := f.service.CompleteCall(requestedStaff, request.Call.ID, CompleteCallInput{
		ActualStart: actualStart.Format(time.RFC3339), ActualEnd: actualStart.Add(45 * time.Minute).Format(time.RFC3339), InternalNotes: "resolved",
	})
	if err != nil {
		t.Fatal(err)
	}
	if call.Status != RequestStatusCompleted || call.ActualDurationMinutes != 45 {
		t.Fatalf("completed call = %#v", call)
	}
	picked, err := f.service.pickFairStaff(f.date(13), nil)
	if err != nil {
		t.Fatal(err)
	}
	if picked == requestedStaff {
		t.Fatalf("fair assignment chose %d, want the other staff member after the requested person worked 45 minutes", picked)
	}
	var audits int64
	if err := f.service.DB.Model(&SupportScheduleAuditLog{}).Where("action = ?", "actual_duration_recorded").Count(&audits).Error; err != nil || audits != 1 {
		t.Fatalf("duration audit count = %d, err = %v", audits, err)
	}
}

func TestSupportCallCreationSendsDetailedRoleSpecificEmails(t *testing.T) {
	f := newScheduleFixture(t)
	if err := f.service.EnsureRollingSchedule(); err != nil {
		t.Fatal(err)
	}
	mailer := &scheduleMailer{}
	f.service.Mailer = mailer
	f.service.MeetingProvider = &fakeMeetingProvider{}
	requestedStaff := f.otherStaff(f.assignedStaff(t, 1))

	request, err := f.service.CreateCall(f.survivor, CreateCallInput{
		ScheduledStart:   f.at(1, 10, 0).Format(time.RFC3339),
		DurationMinutes:  30,
		Subject:          "Review a community record",
		Message:          "Please help me confirm the submitted information.",
		RequestedStaffID: &requestedStaff,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(mailer.sent) != 2 {
		t.Fatalf("sent emails = %d, want requester and selected support person", len(mailer.sent))
	}

	bodies := make(map[string]string, len(mailer.sent))
	for _, email := range mailer.sent {
		if len(email.To) != 1 {
			t.Fatalf("email recipients = %#v, want one tailored recipient", email.To)
		}
		bodies[email.To[0]] = email.Body
		if strings.Contains(email.Subject, "#") || strings.Contains(email.Body, "request <strong>#") || !strings.Contains(email.Body, "Scheduled time") || !strings.Contains(email.Body, "What happens next") || !strings.Contains(email.Body, "Review a community record") {
			t.Fatalf("email was not detailed enough: subject=%q body=%s", email.Subject, email.Body)
		}
	}
	if !strings.Contains(bodies["sam@example.test"], "selected support person has been asked") {
		t.Fatalf("requester email missing approval explanation: %s", bodies["sam@example.test"])
	}
	requestedUser, err := f.service.user(requestedStaff)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bodies[requestedUser.Email], "Approve it, decline it with a reason") {
		t.Fatalf("support-admin email missing action: %s", bodies[requestedUser.Email])
	}
	if request.Status != RequestStatusAwaitingApproval {
		t.Fatalf("request status = %q, want awaiting approval", request.Status)
	}

	mailer.sent = nil
	approved, err := f.service.DecideRequest(requestedStaff, request.ID, RequestDecisionInput{Decision: "approve"})
	if err != nil {
		t.Fatal(err)
	}
	if approved.Status != RequestStatusApproved || approved.Call == nil || approved.Call.ZoomJoinURL == "" {
		t.Fatalf("approved request is missing its Zoom link: %#v", approved)
	}
	if len(mailer.sent) != 2 {
		t.Fatalf("approval emails = %d, want requester and assigned support person", len(mailer.sent))
	}
	for _, email := range mailer.sent {
		if !strings.Contains(email.Body, approved.Call.ZoomJoinURL) {
			t.Fatalf("approval email to %v is missing the Zoom link", email.To)
		}
	}
}

func TestSpecificPersonAssignedForTheDayIsScheduledWithoutApproval(t *testing.T) {
	f := newScheduleFixture(t)
	assignedStaff := f.assignedStaff(t, 1)
	request, err := f.service.CreateCall(f.survivor, CreateCallInput{
		ScheduledStart: f.at(1, 13, 0).Format(time.RFC3339), DurationMinutes: 30,
		Subject: "Help from today's support person", RequestedStaffID: &assignedStaff,
	})
	if err != nil {
		t.Fatal(err)
	}
	if request.Status != RequestStatusApproved || request.RequestType != RequestTypeSpecificStaff {
		t.Fatalf("same-day assigned-person request = %#v, want immediately approved", request)
	}
	if _, err := f.service.DecideRequest(assignedStaff, request.ID, RequestDecisionInput{Decision: "approve"}); !errors.Is(err, ErrInvalidStatusChange) {
		t.Fatalf("already scheduled request approval error = %v, want ErrInvalidStatusChange", err)
	}
}

func TestRolePermissionsSeparateUserAdminAndManagerWorkflows(t *testing.T) {
	f := newScheduleFixture(t)
	if err := f.service.EnsureRollingSchedule(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.ReassignDay(f.survivor, f.date(1), ReassignInput{UserID: f.staffA, Reason: "nope"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("regular user reassignment error = %v", err)
	}
	if _, err := f.service.ReassignDay(f.staffA, f.date(1), ReassignInput{UserID: f.staffB, Reason: "nope"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("support admin reassignment error = %v", err)
	}
	profile, err := f.service.GetProfile(f.staffA)
	if err != nil {
		t.Fatal(err)
	}
	if profile == nil {
		t.Fatal("support-admin profile is nil")
	}
}
