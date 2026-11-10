package auth

import (
	"time"

	"github.com/lib/pq"
)

type Auth struct {
	ID              int            `gorm:"primaryKey;autoIncrement" json:"id"`
	FirstName       string         `gorm:"size:100;not null;column:firstname" json:"firstname"`
	LastName        string         `gorm:"size:100;not null;column:lastname" json:"lastname"`
	Email           string         `gorm:"size:100;uniqueIndex;not null" json:"email"`
	Community       pq.StringArray `gorm:"type:text[];column:community" json:"community"`
	Password        string         `gorm:"not null" json:"-"`
	Role            string         `json:"role"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
	EmailVerifiedAt *time.Time     `json:"email_verified_at,omitempty"`
}

type Access struct {
	CommunityName string    `gorm:"primaryKey;size:255;not null" json:"community_name"`
	Filename      *string   `gorm:"primaryKey;size:255" json:"filename,omitempty"`
	UserID        int       `gorm:"primaryKey;not null" json:"user_id"`
	CreatedAt     time.Time `gorm:"autoCreateTime" json:"created_at"`
	Status        string    `gorm:"not null" json:"status"`
}

type Role struct {
	ID            uint   `gorm:"primaryKey;autoIncrement" json:"id"`
	Role          string `gorm:"size:100;not null;unique" json:"role"`
	Priority      int    `gorm:"not null" json:"priority"`
	CanUpload     bool   `gorm:"not null" json:"can_upload"`
	CanView       bool   `gorm:"not null" json:"can_view"`
	CanApprove    bool   `gorm:"not null" json:"can_approve"`
	CanApproveAll bool   `gorm:"not null" json:"can_approve_all"`
}

type RequestAction struct {
	Filename      string `json:"filename"`
	CommunityName string `json:"community_name"`
	UserID        int    `json:"user_id"`
	Status        string `json:"status"`
	Role          string `json:"role"`
}

type UserRole struct {
	ID            uint    `gorm:"primaryKey;autoIncrement" json:"id"`
	Role          string  `gorm:"size:100;not null" json:"role"`
	UserID        int     `gorm:"not null" json:"user_id"`
	CommunityName *string `gorm:"size:255" json:"community_name,omitempty"`
}

type FileAccessResponse struct {
	Filename    string   `json:"filename"`
	Communities []string `json:"community"`
}

type LoginResponse struct {
	Token       string         `json:"token"`
	FirstName   string         `json:"firstname"`
	LastName    string         `json:"lastname"`
	ID          int            `json:"id"`
	Email       string         `json:"email"`
	PhoneNumber string         `json:"phonenumber"`
	Role        string         `json:"role"`
	Community   pq.StringArray `gorm:"type:text[];column:community" json:"community"`
}

type SendOTPRequest struct {
	Email string `json:"email" binding:"required,email"`
}

type ResetPasswordRequest struct {
	Email    string `json:"email" binding:"required,email"`
	OTP      string `json:"otp" binding:"required"`
	Password string `json:"password" binding:"required,min=6"`
}

type VerifyPasswordRequest struct {
	Password string `json:"password"`
}

type OTP struct {
	ID        uint      `gorm:"primaryKey"`
	Email     string    `gorm:"index;not null"`
	Code      string    `gorm:"size:6;not null"`
	CreatedAt time.Time `gorm:"autoCreateTime"`
}

type mediaZipRow struct {
	ID               uint
	RequestID        uint
	RowID            int
	PhotoURL         string
	FileName         string
	DocumentType     string // "photos" or "document"
	DocumentCategory string

	UserID    uint
	UserFirst string
	UserLast  string
}

type VerifyPasswordResponse struct {
	Match bool `json:"match"`
}

func (Role) TableName() string {
	return "roles"
}

func (OTP) TableName() string {
	return "otps"
}

func (UserRole) TableName() string {
	return "user_roles"
}

func (Auth) TableName() string {
	return "users"
}

func (Access) TableName() string {
	return "access"
}
