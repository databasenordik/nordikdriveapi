package supportrequest

import "time"

const (
	RequestTypeQuestion       = "question"
	RequestTypeTechnicalIssue = "technical_issue"

	RequestStatusOpen       = "open"
	RequestStatusInProgress = "in_progress"
	RequestStatusClosed     = "closed"

	MaxSubjectLength         = 140
	MaxMessageLength         = 2000
	MaxRequesterNameLength   = 120
	MaxRequesterEmailLength  = 255
	MaxTeamNameLength        = 120
	MaxAdminNoteLength       = 2000
	MaxScreenshotSizeInBytes = 5 * 1024 * 1024
)

var validSupportRequestTypes = map[string]struct{}{
	RequestTypeQuestion:       {},
	RequestTypeTechnicalIssue: {},
}

var validSupportRequestStatuses = map[string]struct{}{
	RequestStatusOpen:       {},
	RequestStatusInProgress: {},
	RequestStatusClosed:     {},
}

var validScreenshotMimeTypes = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/webp": ".webp",
}

type SupportRequest struct {
	ID                     int64      `json:"id" gorm:"primaryKey;autoIncrement"`
	CreatedByID            int        `json:"created_by" gorm:"column:created_by;not null;index"`
	RequesterName          string     `json:"requester_name" gorm:"column:requester_name;type:varchar(120);not null"`
	RequesterEmail         string     `json:"requester_email" gorm:"column:requester_email;type:varchar(255);not null"`
	RequestType            string     `json:"request_type" gorm:"column:request_type;type:varchar(30);not null;index"`
	Subject                string     `json:"subject" gorm:"column:subject;type:varchar(140);not null"`
	Message                string     `json:"message" gorm:"column:message;type:text;not null"`
	ScreenshotFileName     string     `json:"screenshot_file_name" gorm:"column:screenshot_file_name;type:text;not null;default:''"`
	ScreenshotMimeType     string     `json:"screenshot_mime_type" gorm:"column:screenshot_mime_type;type:text;not null;default:''"`
	ScreenshotSizeBytes    int64      `json:"screenshot_size_bytes" gorm:"column:screenshot_size_bytes;not null;default:0"`
	ScreenshotURL          string     `json:"screenshot_url" gorm:"column:screenshot_url;type:text;not null;default:''"`
	Status                 string     `json:"status" gorm:"column:status;type:varchar(20);not null;default:'open'"`
	AssignedTeam           string     `json:"assigned_team" gorm:"column:assigned_team;type:varchar(120);not null;default:''"`
	AssignedTeamRecipients string     `json:"assigned_team_recipients" gorm:"column:assigned_team_recipients;type:text;not null;default:''"`
	AssignedByID           *int       `json:"assigned_by" gorm:"column:assigned_by"`
	AssignedAt             *time.Time `json:"assigned_at" gorm:"column:assigned_at"`
	AdminNote              string     `json:"admin_note" gorm:"column:admin_note;type:text;not null;default:''"`
	ClosedByID             *int       `json:"closed_by" gorm:"column:closed_by"`
	ClosedAt               *time.Time `json:"closed_at" gorm:"column:closed_at"`
	NotificationEmailSent  bool       `json:"notification_email_sent" gorm:"column:notification_email_sent;not null;default:false"`
	RequesterEmailSent     bool       `json:"requester_email_sent" gorm:"column:requester_email_sent;not null;default:false"`
	StatusEmailSent        bool       `json:"status_email_sent" gorm:"column:status_email_sent;not null;default:false"`
	TeamForwardEmailSent   bool       `json:"team_forward_email_sent" gorm:"column:team_forward_email_sent;not null;default:false"`
	NotificationVersion    int64      `json:"-" gorm:"not null;default:0"`
	CreatedAt              time.Time  `json:"created_at" gorm:"column:created_at;autoCreateTime"`
	UpdatedAt              time.Time  `json:"updated_at" gorm:"column:updated_at;autoUpdateTime"`

	CreatedByUser *SupportRequestUserRef `json:"-" gorm:"foreignKey:CreatedByID;references:ID"`
}

func (SupportRequest) TableName() string { return "support_requests" }

type SupportRequestUserRef struct {
	ID        int    `json:"id" gorm:"column:id;primaryKey"`
	FirstName string `json:"firstname" gorm:"column:firstname"`
	LastName  string `json:"lastname" gorm:"column:lastname"`
	Email     string `json:"email" gorm:"column:email"`
	Role      string `json:"-" gorm:"column:role"`
}

func (SupportRequestUserRef) TableName() string { return "users" }

type CreateSupportRequestRequest struct {
	RequestType    string `form:"request_type" binding:"required"`
	RequesterName  string `form:"requester_name"`
	RequesterEmail string `form:"requester_email"`
	Subject        string `form:"subject" binding:"required"`
	Message        string `form:"message" binding:"required"`
}

type CreateSupportRequestResponse struct {
	ID      int64  `json:"id"`
	Message string `json:"message"`
}

type UpdateSupportRequestRequest struct {
	Status                 string `json:"status" binding:"required"`
	AssignedTeam           string `json:"assigned_team"`
	AssignedTeamRecipients string `json:"assigned_team_recipients"`
	AdminNote              string `json:"admin_note"`
}

type SupportRequestListResponse struct {
	Page       int              `json:"page"`
	PageSize   int              `json:"page_size"`
	TotalItems int64            `json:"total_items"`
	TotalPages int              `json:"total_pages"`
	Items      []SupportRequest `json:"items"`
}
