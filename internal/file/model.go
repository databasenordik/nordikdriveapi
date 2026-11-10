package file

import (
	"time"

	"cloud.google.com/go/storage"
	"github.com/lib/pq"
	"gorm.io/datatypes"
)

type File struct {
	ID              uint           `gorm:"primaryKey" json:"id"`
	Filename        string         `gorm:"unique;not null" json:"filename"`
	Description     string         `gorm:"type:text;not null;default:''" json:"description"`
	InsertedBy      uint           `gorm:"not null" json:"inserted_by"`
	CreatedAt       time.Time      `json:"created_at"`
	Private         bool           `json:"private"`
	IsDelete        bool           `json:"is_delete"`
	Size            float64        `json:"size"`
	Version         int            `json:"version"`
	Rows            int            `json:"rows"`
	ColumnsOrder    datatypes.JSON `gorm:"type:jsonb"`
	CommunityFilter bool           `json:"community_filter"`
}

type FileVersion struct {
	ID                   uint           `gorm:"primaryKey;autoIncrement" jsoxn:"id"`
	FileID               uint           `gorm:"not null;index;index:idx_file_version_file_version,priority:1;index:idx_file_version_transition_state,priority:1" json:"file_id"`
	Filename             string         `gorm:"size:255;not null" json:"filename"`
	Description          string         `gorm:"type:text;not null;default:''" json:"description"`
	InsertedBy           uint           `gorm:"not null;index" json:"inserted_by"`
	CreatedAt            time.Time      `gorm:"autoCreateTime" json:"created_at"`
	Private              bool           `gorm:"default:false" json:"private"`
	IsDelete             bool           `gorm:"default:false" json:"is_delete"`
	Size                 float64        `gorm:"not null" json:"size"`
	Version              int            `gorm:"not null;default:1;index:idx_file_version_file_version,priority:2" json:"version"`
	Rows                 int            `gorm:"not null" json:"rows"`
	ColumnsOrder         datatypes.JSON `gorm:"type:jsonb" json:"columns_order"`
	ReconciliationStatus string         `gorm:"type:varchar(20);not null;default:'ready';index:idx_file_version_transition_state,priority:2" json:"reconciliation_status"`
	ReconciliationError  string         `gorm:"type:text;not null;default:''" json:"reconciliation_error"`
	ReconciledAt         *time.Time     `json:"reconciled_at"`
	TransitionOperation  string         `gorm:"type:varchar(20);not null;default:''" json:"transition_operation"`
}

type FileVersionWithUser struct {
	ID                   uint           `json:"id"`
	FileID               uint           `json:"file_id"`
	FileName             string         `json:"filename" gorm:"column:filename"`
	Description          string         `json:"description" gorm:"column:description"`
	Firstname            string         `json:"firstname" gorm:"column:firstname"`
	Lastname             string         `json:"lastname" gorm:"column:lastname"`
	CreatedAt            time.Time      `json:"created_at"`
	Private              bool           `json:"private"`
	IsDelete             bool           `json:"is_delete"`
	Size                 float64        `json:"size"`
	Version              int            `json:"version"`
	Rows                 int            `json:"rows"`
	ColumnsOrder         datatypes.JSON `json:"columns_order" gorm:"column:columns_order"`
	ReconciliationStatus string         `json:"reconciliation_status" gorm:"column:reconciliation_status"`
	ReconciliationError  string         `json:"reconciliation_error" gorm:"column:reconciliation_error"`
	ReconciledAt         *time.Time     `json:"reconciled_at" gorm:"column:reconciled_at"`
	TransitionOperation  string         `json:"transition_operation" gorm:"column:transition_operation"`
}

type FileData struct {
	ID         uint           `gorm:"primaryKey" json:"id"`
	FileID     uint           `gorm:"not null;index" json:"file_id"`
	RowData    datatypes.JSON `gorm:"type:jsonb" json:"row_data"`
	InsertedBy uint           `gorm:"not null" json:"inserted_by"`
	CreatedAt  time.Time      `gorm:"not null;autoCreateTime" json:"created_at"`
	UpdatedAt  time.Time      `gorm:"not null;autoUpdateTime" json:"updated_at"`
	Version    int            `gorm:"not null;default:1" json:"version"`
}

type FileAccess struct {
	ID     uint `gorm:"primaryKey" json:"id"`
	UserID uint `gorm:"type:json" json:"user_id"`
	FileID uint `gorm:"not null;index" json:"file_id"`
}

type RevertFileInput struct {
	Filename string `json:"filename" binding:"required"`
	Version  int    `json:"version" binding:"required"`
}

type FileWithUser struct {
	File
	Firstname string `json:"firstname"`
	Lastname  string `json:"lastname"`
}

type PhotoReviewInput struct {
	PhotoID       uint   `json:"photo_id"`
	Status        string `json:"status"`
	ReviewComment string `json:"reviewer_comment"`
}

type FileEditRequest struct {
	RequestID                 uint           `gorm:"primaryKey;autoIncrement" json:"request_id"`
	RowID                     int            `gorm:"not null" json:"row_id"`
	UserID                    uint           `gorm:"not null" json:"user_id"`
	Status                    string         `gorm:"type:varchar(50);default:'pending'" json:"status"`
	FirstName                 string         `gorm:"type:varchar(100);column:firstname" json:"firstname"`
	LastName                  string         `gorm:"type:varchar(100);column:lastname" json:"lastname"`
	Consent                   bool           `gorm:"default:false" json:"consent"`
	ArchiveConsent            bool           `gorm:"column:archive_consent;default:false" json:"archive_consent"`
	CreatedAt                 time.Time      `gorm:"autoCreateTime" json:"created_at"`
	IsEdited                  bool           `gorm:"default:false" json:"is_edited"`
	RequestType               string         `gorm:"type:varchar(30);not null;default:'edit';column:request_type" json:"request_type"`
	FileID                    uint           `gorm:"column:file_id;not null;" json:"file_id"`
	ReviewedBy                *int           `gorm:"column:reviewed_by" json:"reviewed_by"`
	ReviewComment             string         `gorm:"type:text;column:reviewer_comment" json:"reviewer_comment"`
	Community                 pq.StringArray `gorm:"type:text[];column:community;default:'{}'" json:"community"`
	UploaderCommunity         pq.StringArray `gorm:"type:text[];column:uploader_community;default:'{}'" json:"uploader_community"`
	ReviewEmailTriggerSuccess bool           `gorm:"column:review_email_trigger_success;not null;default:false" json:"review_email_trigger_success"`
	ReviewNotificationKey     string         `json:"-" gorm:"not null;default:''"`
}

type FileEditRequestDetails struct {
	ID            uint      `gorm:"primaryKey;autoIncrement" json:"id"`
	RequestID     uint      `gorm:"not null" json:"request_id"`
	FileID        uint      `gorm:"not null" json:"file_id"`
	Filename      string    `gorm:"type:varchar(255);not null" json:"filename"`
	RowID         int       `gorm:"not null" json:"row_id"`
	FieldName     string    `gorm:"type:varchar(255);not null" json:"field_name"`
	OldValue      string    `gorm:"type:text" json:"old_value"`
	NewValue      string    `gorm:"type:text" json:"new_value"`
	Status        string    `gorm:"type:varchar(20);not null;default:'pending'" json:"status"`
	ReviewComment string    `gorm:"type:text;column:reviewer_comment;not null;default:''" json:"reviewer_comment"`
	CreatedAt     time.Time `gorm:"autoCreateTime" json:"created_at"`
}

type FileEditRequestWithUser struct {
	RequestID      uint                     `json:"request_id"`
	RowID          int                      `json:"row_id"`
	UserID         uint                     `json:"user_id"`
	Firstname      string                   `json:"firstname"`
	Lastname       string                   `json:"lastname"`
	Status         string                   `json:"status"`
	RequestType    string                   `json:"request_type"`
	ReviewComment  string                   `json:"reviewer_comment"`
	CreatedAt      time.Time                `json:"created_at"`
	Details        []FileEditRequestDetails `json:"details"`
	EFirstName     string                   `json:"efirstname"`
	ELastName      string                   `json:"elastname"`
	IsEdited       bool                     `gorm:"default:true" json:"is_edited"`
	Consent        bool                     `json:"consent"`
	ArchiveConsent bool                     `json:"archive_consent"`
	FileID         uint                     `json:"file_id"`
	FileName       string                   `json:"file_name"`
	Stories        []AchieverStory          `json:"stories"`
}

type EditRequestInput struct {
	FileID    uint                         `json:"file_id"`
	Filename  string                       `json:"filename"`
	Changes   map[string][]EditChangeInput `json:"changes"`
	FirstName string                       `json:"firstname"`
	LastName  string                       `json:"lastname"`
	RowID     int                          `json:"row_id"`

	PhotosInApp      []PhotoInput `json:"photos_in_app"`
	PhotosForGallery []PhotoInput `json:"photos_for_gallery_review"`

	Consent        bool `json:"consent"`
	ArchiveConsent bool `json:"archive_consent"`

	IsEdited bool `json:"is_edited"`

	Documents         []DocumentInput `json:"documents"`
	Community         pq.StringArray  `gorm:"type:text[];column:community;default:'{}'" json:"community"`
	UploaderCommunity pq.StringArray  `gorm:"type:text[];column:uploader_community;default:'{}'" json:"uploader_community"`
}

type DocumentInput struct {
	DocumentType     string `json:"document_type"`
	DocumentCategory string `json:"document_category"`
	Filename         string `json:"filename"`
	MimeType         string `json:"mime_type"`
	Size             int64  `json:"size"`
	DataBase64       string `json:"data_base64"`
}

// AchieverStoryTemplateInput contains the fields used to generate the common
// achiever-story PDF. Date values intentionally remain strings: a source may
// provide either an exact date or descriptive text such as "Not recorded".
type AchieverStoryTemplateInput struct {
	DateOfBirth              string   `json:"date_of_birth"`
	DateOfDeath              string   `json:"date_of_death"`
	Community                string   `json:"community"`
	Parents                  string   `json:"parents"`
	Siblings                 string   `json:"siblings"`
	Spouse                   string   `json:"spouse"`
	Education                string   `json:"education"`
	ResidentialSchoolHistory string   `json:"residential_school_history"`
	Note                     string   `json:"note"`
	AchieversStory           string   `json:"achievers_story"`
	Sources                  []string `json:"sources"`
}

// AchieverStoryRequestInput is a single proposed story for an existing master
// list row. A user can submit more stories later; each is reviewed separately.
type AchieverStoryRequestInput struct {
	FileID    uint   `json:"file_id"`
	RowID     uint   `json:"row_id"`
	FirstName string `json:"firstname"`
	LastName  string `json:"lastname"`

	StoryType string                      `json:"story_type"`
	StoryText string                      `json:"story_text"`
	VideoURL  string                      `json:"video_url"`
	Video     *DocumentInput              `json:"video"`
	Document  *DocumentInput              `json:"document"`
	Template  *AchieverStoryTemplateInput `json:"template"`
}

type AchieverStoryReviewInput struct {
	StoryID       uint   `json:"story_id"`
	Status        string `json:"status"`
	ReviewComment string `json:"reviewer_comment"`
}

type EditChangeInput struct {
	RowID     int    `json:"row_id"`
	FieldName string `json:"field_name"`
	OldValue  string `json:"old_value"`
	NewValue  string `json:"new_value"`
}

type FileEditRequestPhoto struct {
	ID               uint       `gorm:"primaryKey" json:"id"`
	RequestID        uint       `json:"request_id"`
	RowID            int        `json:"row_id"`
	PhotoURL         string     `json:"photo_url"`
	FileName         string     `json:"file_name"`
	SizeBytes        int64      `json:"size_bytes"`
	IsGalleryPhoto   bool       `json:"is_gallery_photo"`
	Status           string     `gorm:"type:varchar(20);default:'pending'" json:"status"`
	ReviewBy         *int       `gorm:"column:reviewed_by" json:"reviewed_by"`
	ReviewComment    string     `gorm:"type:text;column:reviewer_comment" json:"reviewer_comment"`
	ReviewedAt       *time.Time `gorm:"column:reviewed_at" json:"reviewed_at"`
	CreatedAt        time.Time  `json:"created_at"`
	SourceFile       string     `json:"source_file"`
	FileID           uint       `json:"file_id"`
	DocumentType     string     `gorm:"type:varchar(20);default:'photos'" json:"document_type"`
	DocumentCategory string     `gorm:"type:varchar(50)" json:"document_category"`
	PhotoComment     string     `gorm:"type:text;column:photo_comment" json:"photo_comment"`
}

type PhotoInput struct {
	Filename   string `json:"filename"`
	MimeType   string `json:"mime_type"`
	Size       int64  `json:"size"`
	DataBase64 string `json:"data_base64"`
	Comment    string `json:"comment"`
}

type gcsReadHandle struct {
	Client *storage.Client
	Reader *storage.Reader
}

func (FileEditRequest) TableName() string {
	return "file_edit_request"
}

func (FileEditRequestDetails) TableName() string {
	return "file_edit_request_details"
}

func (File) TableName() string {
	return "file"
}

func (FileData) TableName() string {
	return "file_data"
}

func (FileAccess) TableName() string {
	return "file_access"
}

func (FileVersion) TableName() string {
	return "file_version"
}

func (FileEditRequestPhoto) TableName() string {
	return "file_edit_request_photos"
}
