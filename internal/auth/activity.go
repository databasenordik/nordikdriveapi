package auth

import (
	"fmt"
	"gorm.io/gorm"
	"nordik-drive-api/internal/activity"
)

func (s *AuthService) auditUser(tx *gorm.DB, user Auth, action string) error {
	if s.Audit == nil {
		return nil
	}
	uid := uint(user.ID)
	event := activity.Log{Level: "INFO", Service: "auth", UserID: &uid, Action: action, Communities: user.Community}
	metadata := map[string]any{"email": user.Email}
	switch action {
	case "SIGNUP":
		event.Message = fmt.Sprintf("Account created with email %s", user.Email)
		metadata["firstname"] = user.FirstName
		metadata["lastname"] = user.LastName
	case "SEND_OTP":
		event.Message = fmt.Sprintf("Sent OTP to email: %s", user.Email)
	case "RESET_PASSWORD":
		event.Message = fmt.Sprintf("Password reset for email: %s", user.Email)
	}
	return s.Audit.Put(tx, event, metadata)
}
