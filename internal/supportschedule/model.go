// Package supportschedule owns the support-call rota, staff availability and
// call-request workflow. The package is deliberately backend authoritative:
// clients cannot bypass availability, approval, assignment or audit rules.
package supportschedule

import (
	"time"

	"gorm.io/datatypes"
)

const (
	RoleAdmin   = "Admin"
	RoleManager = "Manager"

	AssignmentSourceAutomatic = "automatic"
	AssignmentSourceManager   = "manager_reassignment"
	AssignmentSourceFullDay   = "full_day_reassignment"
	AssignmentSourceUncovered = "uncovered"

	RequestTypeAutomaticDaily = "automatic_daily_assignee"
	RequestTypeSpecificStaff  = "specific_support_person"

	RequestStatusPending             = "pending"
	RequestStatusAwaitingApproval    = "awaiting_assignee_approval"
	RequestStatusApproved            = "approved"
	RequestStatusAlternativeProposed = "alternative_time_proposed"
	RequestStatusRejected            = "rejected"
	RequestStatusCancelled           = "cancelled"
	RequestStatusCompleted           = "completed"

	DefaultTimeZone       = "America/Toronto"
	DefaultStartTime      = "08:30"
	DefaultEndTime        = "16:30"
	DefaultHorizonDays    = 14
	DefaultDurationMinute = 30

	ZoomSyncNotRequested = "not_requested"
	ZoomSyncPending      = "pending"
	ZoomSyncSynced       = "synced"
	ZoomSyncFailed       = "failed"
	ZoomSyncDeleted      = "deleted"
)

var DefaultCallReasons = []string{
	"Technical support",
	"Account or access help",
	"Document or data help",
	"Training or guidance",
}

// Compatibility aliases keep existing API clients using the previous status
// labels working while the persisted model uses request/call terminology.
const (
	CallStatusAwaitingApproval = RequestStatusAwaitingApproval
	CallStatusScheduled        = RequestStatusApproved
	CallStatusDeclined         = RequestStatusRejected
	CallStatusCancelled        = RequestStatusCancelled
	CallStatusNeedsReschedule  = RequestStatusAlternativeProposed
	CallStatusCompleted        = RequestStatusCompleted
)

type SupportScheduleSettings struct {
	ID                     uint           `gorm:"primaryKey" json:"id"`
	TimeZone               string         `gorm:"not null;default:'America/Toronto'" json:"time_zone"`
	WorkdayStart           string         `gorm:"not null;default:'08:30'" json:"workday_start"`
	WorkdayEnd             string         `gorm:"not null;default:'16:30'" json:"workday_end"`
	AllowedDurationsJSON   datatypes.JSON `gorm:"type:jsonb;not null" json:"-"`
	DefaultDurationMinutes int            `gorm:"not null;default:30" json:"default_duration_minutes"`
	BookingHorizonDays     int            `gorm:"not null;default:14" json:"booking_horizon_days"`
	UpdatedByID            *uint          `json:"updated_by_id,omitempty"`
	CreatedAt              time.Time      `json:"created_at"`
	UpdatedAt              time.Time      `json:"updated_at"`
}

func (SupportScheduleSettings) TableName() string { return "support_schedule_settings" }

type SupportUser struct {
	ID        uint   `gorm:"primaryKey" json:"id"`
	FirstName string `gorm:"column:firstname" json:"firstname"`
	LastName  string `gorm:"column:lastname" json:"lastname"`
	Email     string `gorm:"column:email" json:"email"`
	Role      string `gorm:"column:role" json:"role"`
}

func (SupportUser) TableName() string { return "users" }

// SupportTeamMember identifies active support admins. A user must have the
// Admin role as well as an active membership to be eligible for a rota/call.
type SupportTeamMember struct {
	ID        uint        `gorm:"primaryKey" json:"id"`
	UserID    uint        `gorm:"uniqueIndex;not null" json:"user_id"`
	IsActive  bool        `gorm:"not null;default:true;index" json:"is_active"`
	AddedByID *uint       `json:"added_by_id,omitempty"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
	User      SupportUser `gorm:"foreignKey:UserID;references:ID" json:"user"`
}

func (SupportTeamMember) TableName() string { return "support_team_members" }

// SupportAssignment is the one authoritative primary assignee for a calendar
// date. AssignmentDate intentionally uses YYYY-MM-DD so PostgreSQL and SQLite
// test databases share the same representation.
type SupportAssignment struct {
	ID                 uint         `gorm:"primaryKey" json:"id"`
	AssignmentDate     string       `gorm:"column:assignment_date;type:varchar(10);uniqueIndex;not null" json:"assignment_date"`
	PrimaryAssigneeID  *uint        `gorm:"column:primary_assignee_id;index" json:"primary_assignee_id,omitempty"`
	AssignmentSource   string       `gorm:"column:assignment_source;type:varchar(40);not null" json:"assignment_source"`
	PreviousAssigneeID *uint        `gorm:"column:previous_assignee_id" json:"previous_assignee_id,omitempty"`
	ReassignedByID     *uint        `gorm:"column:reassigned_by_id" json:"reassigned_by_id,omitempty"`
	ReassignmentReason string       `gorm:"column:reassignment_reason;type:text;not null;default:''" json:"reassignment_reason"`
	CreatedAt          time.Time    `json:"created_at"`
	UpdatedAt          time.Time    `json:"updated_at"`
	PrimaryAssignee    *SupportUser `gorm:"foreignKey:PrimaryAssigneeID;references:ID" json:"primary_assignee,omitempty"`
	PreviousAssignee   *SupportUser `gorm:"foreignKey:PreviousAssigneeID;references:ID" json:"previous_assignee,omitempty"`
}

func (SupportAssignment) TableName() string { return "support_assignments" }

// StaffAvailability permits one full-day record and any number of partial-day
// records per support admin/date.
type StaffAvailability struct {
	ID                   uint         `gorm:"primaryKey" json:"id"`
	StaffID              uint         `gorm:"column:staff_id;index;not null" json:"staff_id"`
	AvailabilityDate     string       `gorm:"column:availability_date;type:varchar(10);not null;index" json:"availability_date"`
	FullDayUnavailable   bool         `gorm:"column:full_day_unavailable;not null;default:false" json:"full_day_unavailable"`
	UnavailableStartTime *time.Time   `gorm:"column:unavailable_start_time" json:"unavailable_start_time,omitempty"`
	UnavailableEndTime   *time.Time   `gorm:"column:unavailable_end_time" json:"unavailable_end_time,omitempty"`
	Reason               string       `gorm:"type:text;not null" json:"reason"`
	CreatedAt            time.Time    `json:"created_at"`
	UpdatedAt            time.Time    `json:"updated_at"`
	Staff                *SupportUser `gorm:"foreignKey:StaffID;references:ID" json:"staff,omitempty"`
}

func (StaffAvailability) TableName() string { return "support_staff_availabilities" }

type SupportCallRequest struct {
	ID                   uint         `gorm:"primaryKey" json:"id"`
	RequestedByUserID    uint         `gorm:"column:requested_by_user_id;index;not null" json:"requested_by_user_id"`
	RequestType          string       `gorm:"column:request_type;type:varchar(40);not null" json:"request_type"`
	RequestedDate        string       `gorm:"column:requested_date;type:varchar(10);not null;index" json:"requested_date"`
	PreferredStartTime   *time.Time   `gorm:"column:preferred_start_time" json:"preferred_start_time,omitempty"`
	PreferredEndTime     *time.Time   `gorm:"column:preferred_end_time" json:"preferred_end_time,omitempty"`
	RequestedStaffID     *uint        `gorm:"column:requested_staff_id;index" json:"requested_staff_id,omitempty"`
	AssignedStaffID      *uint        `gorm:"column:assigned_staff_id;index" json:"assigned_staff_id,omitempty"`
	Status               string       `gorm:"type:varchar(40);not null;index" json:"status"`
	Subject              string       `gorm:"type:varchar(160);not null" json:"subject"`
	Description          string       `gorm:"type:text;not null;default:''" json:"description"`
	RejectionReason      string       `gorm:"type:text;not null;default:''" json:"rejection_reason"`
	AlternativeStartTime *time.Time   `gorm:"column:alternative_start_time" json:"alternative_start_time,omitempty"`
	AlternativeEndTime   *time.Time   `gorm:"column:alternative_end_time" json:"alternative_end_time,omitempty"`
	CreatedAt            time.Time    `json:"created_at"`
	UpdatedAt            time.Time    `json:"updated_at"`
	RequestedBy          SupportUser  `gorm:"foreignKey:RequestedByUserID;references:ID" json:"requested_by"`
	RequestedStaff       *SupportUser `gorm:"foreignKey:RequestedStaffID;references:ID" json:"requested_staff,omitempty"`
	AssignedStaff        *SupportUser `gorm:"foreignKey:AssignedStaffID;references:ID" json:"assigned_staff,omitempty"`
	Call                 *SupportCall `gorm:"foreignKey:SupportRequestID" json:"call,omitempty"`
}

func (SupportCallRequest) TableName() string { return "support_call_requests" }

type SupportCall struct {
	ID                    uint         `gorm:"primaryKey" json:"id"`
	SupportRequestID      uint         `gorm:"column:support_request_id;uniqueIndex;not null" json:"support_request_id"`
	AssignedStaffID       *uint        `gorm:"column:assigned_staff_id;index" json:"assigned_staff_id,omitempty"`
	ScheduledStartTime    time.Time    `gorm:"column:scheduled_start_time;index;not null" json:"scheduled_start_time"`
	ScheduledEndTime      time.Time    `gorm:"column:scheduled_end_time;not null" json:"scheduled_end_time"`
	ActualStartTime       *time.Time   `gorm:"column:actual_start_time" json:"actual_start_time,omitempty"`
	ActualEndTime         *time.Time   `gorm:"column:actual_end_time" json:"actual_end_time,omitempty"`
	ActualDurationMinutes int          `gorm:"column:actual_duration_minutes;not null;default:0" json:"actual_duration_minutes"`
	Status                string       `gorm:"type:varchar(40);not null;index" json:"status"`
	InternalNotes         string       `gorm:"type:text;not null;default:''" json:"internal_notes"`
	ZoomMeetingID         string       `gorm:"column:zoom_meeting_id;type:varchar(32);not null;default:''" json:"zoom_meeting_id,omitempty"`
	ZoomJoinURL           string       `gorm:"column:zoom_join_url;type:text;not null;default:''" json:"zoom_join_url,omitempty"`
	ZoomPasscode          string       `gorm:"column:zoom_passcode;type:varchar(16);not null;default:''" json:"zoom_passcode,omitempty"`
	ZoomHostEmail         string       `gorm:"column:zoom_host_email;type:varchar(255);not null;default:''" json:"zoom_host_email,omitempty"`
	ZoomSyncStatus        string       `gorm:"column:zoom_sync_status;type:varchar(32);not null;default:'not_requested';index" json:"zoom_sync_status"`
	ZoomSyncError         string       `gorm:"column:zoom_sync_error;type:text;not null;default:''" json:"-"`
	ZoomSyncedAt          *time.Time   `gorm:"column:zoom_synced_at" json:"zoom_synced_at,omitempty"`
	CompletedByID         *uint        `gorm:"column:completed_by_id" json:"completed_by_id,omitempty"`
	CompletedAt           *time.Time   `gorm:"column:completed_at" json:"completed_at,omitempty"`
	CreatedAt             time.Time    `json:"created_at"`
	UpdatedAt             time.Time    `json:"updated_at"`
	AssignedStaff         *SupportUser `gorm:"foreignKey:AssignedStaffID;references:ID" json:"assigned_staff,omitempty"`
}

func (SupportCall) TableName() string { return "support_calls" }

type SupportScheduleAuditLog struct {
	NotificationState       string         `json:"-" gorm:"not null;default:'ignored'"`
	NotificationNextAttempt *time.Time     `json:"-"`
	NotificationSnapshot    string         `json:"-" gorm:"type:text;not null;default:''"`
	ID                      uint           `gorm:"primaryKey" json:"id"`
	EntityType              string         `gorm:"type:varchar(40);not null;index" json:"entity_type"`
	EntityID                *uint          `gorm:"index" json:"entity_id,omitempty"`
	Action                  string         `gorm:"type:varchar(80);not null" json:"action"`
	ActorID                 *uint          `gorm:"index" json:"actor_id,omitempty"`
	Details                 datatypes.JSON `gorm:"type:jsonb;not null" json:"details"`
	CreatedAt               time.Time      `json:"created_at"`
}

func (SupportScheduleAuditLog) TableName() string { return "support_schedule_audit_logs" }

type SettingsResponse struct {
	TimeZone              string   `json:"time_zone"`
	WorkdayStart          string   `json:"workday_start"`
	WorkdayEnd            string   `json:"workday_end"`
	AllowedDurations      []int    `json:"allowed_durations"`
	DefaultDurationMinute int      `json:"default_duration_minutes"`
	BookingHorizonDays    int      `json:"booking_horizon_days"`
	CallReasons           []string `json:"call_reasons"`
}

type UpdateSettingsInput struct {
	TimeZone              string `json:"time_zone"`
	WorkdayStart          string `json:"workday_start"`
	WorkdayEnd            string `json:"workday_end"`
	AllowedDurations      []int  `json:"allowed_durations"`
	DefaultDurationMinute int    `json:"default_duration_minutes"`
}

type AvailabilitySlot struct {
	StartAt           time.Time `json:"start_at"`
	EndAt             time.Time `json:"end_at"`
	UnavailableReason string    `json:"unavailable_reason,omitempty"`
}

type AvailabilityResponse struct {
	TimeZone         string             `json:"time_zone"`
	Date             string             `json:"date"`
	Duration         int                `json:"duration_minutes"`
	AssignedStaff    *SupportUser       `json:"assigned_staff,omitempty"`
	Slots            []AvailabilitySlot `json:"slots"`
	UnavailableSlots []AvailabilitySlot `json:"unavailable_slots"`
}

// CalendarDay is a compact, server-authoritative summary used to render the
// scheduling calendar. It deliberately exposes only counts to regular users;
// availability periods and calls are included only when the viewer is allowed
// to inspect them.
type CalendarDay struct {
	Date               string              `json:"date"`
	AssignedStaff      *SupportUser        `json:"assigned_staff,omitempty"`
	Status             string              `json:"status"`
	StatusMessage      string              `json:"status_message"`
	IsBookable         bool                `json:"is_bookable"`
	AvailableSlotCount int                 `json:"available_slot_count"`
	ScheduledCallCount int                 `json:"scheduled_call_count"`
	IsAssignedToViewer bool                `json:"is_assigned_to_viewer"`
	UnavailablePeriods []StaffAvailability `json:"unavailable_periods,omitempty"`
	ScheduledCalls     []SupportCall       `json:"scheduled_calls,omitempty"`
}

type CalendarResponse struct {
	TimeZone        string        `json:"time_zone"`
	DurationMinutes int           `json:"duration_minutes"`
	Days            []CalendarDay `json:"days"`
}

// CreateCallInput is retained as the API input name. RequestedStaffID selects
// the separate specific-person workflow; omitting it routes to the daily rota.
type CreateCallInput struct {
	ScheduledStart   string `json:"scheduled_start"`
	DurationMinutes  int    `json:"duration_minutes"`
	Reason           string `json:"reason"`
	OtherReason      string `json:"other_reason"`
	Subject          string `json:"subject"`
	Message          string `json:"message"`
	RequestedStaffID *uint  `json:"requested_staff_id"`
}

type CreateAvailabilityInput struct {
	Date               string `json:"date"`
	FullDayUnavailable bool   `json:"full_day_unavailable"`
	StartsAt           string `json:"starts_at"`
	EndsAt             string `json:"ends_at"`
	Reason             string `json:"reason"`
}

// CreateUnavailabilityInput remains accepted by the old endpoint but is
// restricted to the authenticated support admin's own profile.
type CreateUnavailabilityInput = CreateAvailabilityInput

type UpdateAvailabilityInput = CreateAvailabilityInput

type CompleteCallInput struct {
	ActualStart   string `json:"actual_start"`
	ActualEnd     string `json:"actual_end"`
	InternalNotes string `json:"internal_notes"`
}

type ReassignInput struct {
	UserID uint   `json:"user_id"`
	Reason string `json:"reason"`
}

type RequestDecisionInput struct {
	Decision         string `json:"decision"`
	Note             string `json:"note"`
	AlternativeStart string `json:"alternative_start"`
	AlternativeEnd   string `json:"alternative_end"`
}

type SpecificApprovalInput struct {
	Approved bool   `json:"approved"`
	Note     string `json:"note"`
}

type TeamMemberInput struct {
	IsActive bool `json:"is_active"`
}

type SupportStaffOption struct {
	UserID    uint   `json:"user_id"`
	FirstName string `json:"firstname"`
	LastName  string `json:"lastname"`
}

type FairnessStat struct {
	Staff              SupportStaffOption `json:"staff"`
	ActualMinutes      int64              `json:"actual_completed_minutes"`
	ActualHours        float64            `json:"actual_completed_hours"`
	AssignedDays       int64              `json:"assigned_days"`
	LastAssignmentDate string             `json:"last_assignment_date,omitempty"`
}

type SupportProfile struct {
	Assignments    []SupportAssignment  `json:"assignments"`
	UpcomingCalls  []SupportCall        `json:"upcoming_calls"`
	DirectRequests []SupportCallRequest `json:"direct_requests"`
	Availability   []StaffAvailability  `json:"availability"`
}

type ZoomStartURLResponse struct {
	StartURL string `json:"start_url"`
}
