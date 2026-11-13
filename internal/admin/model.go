package admin

import (
	"time"

	"github.com/lib/pq"
	"gorm.io/datatypes"
)

type Mode string
type Operation string

const (
	ModeChanges Mode = "CHANGES"
	ModePhotos  Mode = "PHOTOS"

	OpEQ        Operation = "EQ"
	OpNEQ       Operation = "NEQ"
	OpCONTAINS  Operation = "CONTAINS"
	OpIN        Operation = "IN"
	OpBETWEEN   Operation = "BETWEEN"
	OpLAST7     Operation = "LAST_7"
	OpLAST30    Operation = "LAST_30"
	OpTHISMONTH Operation = "THIS_MONTH"
	OpLASTMONTH Operation = "LAST_MONTH"
	OpALLTIME   Operation = "ALL_TIME"
)

type Clause struct {
	ID     string    `json:"id"`
	Joiner string    `json:"joiner"` // AND/OR (we treat all as AND for now)
	Field  string    `json:"field"`
	Op     Operation `json:"op"`
	Value  *string   `json:"value"`
	Values []string  `json:"values"`
	Start  *string   `json:"start"` // YYYY-MM-DD
	End    *string   `json:"end"`   // YYYY-MM-DD
}

type AdminFileEditSearchRequest struct {
	Mode     Mode     `json:"mode"`
	Clauses  []Clause `json:"clauses"`
	Page     int      `json:"page"`
	PageSize int      `json:"page_size"`
}

type AdminDetailsRequest struct {
	RequestID int `json:"request_id"`
}

type AggKV struct {
	Key   string `json:"key"`
	Count int64  `json:"count"`
}

type Aggregations struct {
	ByField []AggKV `json:"by_field,omitempty"`
	ByFile  []AggKV `json:"by_file,omitempty"`
}

type AdminSearchResponse struct {
	Message      string       `json:"message"`
	Page         int          `json:"page"`
	PageSize     int          `json:"page_size"`
	TotalPages   int          `json:"total_pages"`
	TotalRows    int64        `json:"total_rows"`              // ✅ requests count
	TotalChanges int64        `json:"total_changes,omitempty"` // ✅ changes count (details, non-empty)
	Aggregations Aggregations `json:"aggregations,omitempty"`
	Data         interface{}  `json:"data"`
}

// ---- Row shapes for AG-Grid ----

// ✅ CHANGES now returns request rows (not details rows)
type AdminChangeRow struct {
	RequestedByID *uint  `json:"-" gorm:"column:requested_by_id"`
	ReviewedByID  *uint  `json:"-" gorm:"column:reviewed_by_id"`
	RequestID     uint   `json:"request_id"`
	Status        string `json:"status"`

	FileID   uint   `json:"file_id"`
	FileName string `json:"file_name"`

	Firstname string `json:"firstname"`
	Lastname  string `json:"lastname"`

	Community         pq.StringArray `json:"community" gorm:"type:text[]"`
	UploaderCommunity pq.StringArray `json:"uploader_community" gorm:"type:text[]"`

	RequestedBy string `json:"requested_by"`
	ReviewedBy  string `json:"reviewed_by"`

	Consent     bool      `json:"consent"`
	ChangeCount int64     `json:"change_count"`
	CreatedAt   time.Time `json:"created_at"`
}

type AdminChangeDetailRow struct {
	FieldKey      string `json:"field_key" gorm:"column:field_key"`
	OldValue      string `json:"old_value" gorm:"column:old_value"`
	NewValue      string `json:"new_value" gorm:"column:new_value"`
	Status        string `json:"status" gorm:"column:status"`
	ReviewComment string `json:"reviewer_comment" gorm:"column:reviewer_comment"`
}

type AdminPhotoRow struct {
	ID             uint   `json:"id"`
	RequestID      uint   `json:"request_id"`
	FileID         uint   `json:"file_id"`
	FileName       string `json:"file_name"`
	RowID          int    `json:"row_id"`
	PhotoURL       string `json:"photo_url"`
	IsGalleryPhoto bool   `json:"is_gallery_photo"`
	IsApproved     bool   `json:"is_approved"`

	Firstname string `json:"firstname"`
	Lastname  string `json:"lastname"`

	Community         string `json:"community"`
	UploaderCommunity string `json:"uploader_community"`

	ReviewedBy string    `json:"reviewed_by"`
	CreatedAt  time.Time `json:"created_at"`
}

type AdminDownloadRequest struct {
	Mode    Mode     `json:"mode"`   // "CHANGES"
	Format  string   `json:"format"` // "excel" | "csv"
	Clauses []Clause `json:"clauses"`
}

type fileMeta struct {
	ID           uint           `gorm:"column:id"`
	Filename     string         `gorm:"column:filename"`
	ColumnsOrder datatypes.JSON `gorm:"column:columns_order"`
}

type fileDataRow struct {
	ID      uint           `gorm:"column:id"`
	FileID  uint           `gorm:"column:file_id"`
	Version int            `gorm:"column:version"`
	RowData datatypes.JSON `gorm:"column:row_data"`
}

// Matches YOUR struct fields/columns
type fileEditRequestDetailsRow struct {
	ID            uint      `gorm:"column:id"`
	RequestID     uint      `gorm:"column:request_id"`
	FileID        uint      `gorm:"column:file_id"`
	Filename      string    `gorm:"column:filename"`
	RowID         int       `gorm:"column:row_id"`
	FieldName     string    `gorm:"column:field_name"`
	OldValue      string    `gorm:"column:old_value"`
	NewValue      string    `gorm:"column:new_value"`
	Status        string    `gorm:"column:status"`
	ReviewComment string    `gorm:"column:reviewer_comment"`
	CreatedAt     time.Time `gorm:"column:created_at"`
}

type exportRow struct {
	FileID         uint
	FileName       string
	Version        int
	RowID          int
	RequestIDs     []uint
	ChangedColumns []string
	ChangedSet     map[string]struct{}
	ColumnsOrder   []string
	ValuesByCol    map[string]any
}

type adminSearchRow struct {
	RequestID uint `json:"request_id"`
}

type AdminDownloadMediaRequest struct {
	RequestIDs       []uint   `json:"request_ids,omitempty"`
	Clauses          []Clause `json:"clauses"`
	DocumentType     string   `json:"document_type"`      // "all" | "photos" | "document" | "stories"
	CategorizeByUser bool     `json:"categorize_by_user"` // /User_X/
	CategorizeByType bool     `json:"categorize_by_type"` // /photos/, /documents/, or /stories/
	OnlyApproved     *bool    `json:"only_approved"`      // optional
}

type mediaZipRow struct {
	ID               uint
	RequestID        uint
	RowID            int
	ObjectURL        string `gorm:"column:photo_url"`
	FileName         string
	DocumentType     string // "photos", "document", or "stories"
	DocumentCategory string

	UserID    uint
	UserFirst string
	UserLast  string
}
