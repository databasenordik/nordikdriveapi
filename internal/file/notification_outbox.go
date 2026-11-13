package file

import (
	"fmt"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"nordik-drive-api/internal/auth"
)

func (s *FileService) enqueueReviewTx(tx *gorm.DB, request *FileEditRequest) error {
	if request.ReviewNotificationKey == "" {
		request.ReviewNotificationKey = uuid.NewString()
		if err := tx.Model(request).Updates(map[string]any{"review_notification_key": request.ReviewNotificationKey, "review_email_trigger_success": false}).Error; err != nil {
			return err
		}
	}
	var user auth.Auth
	if s.Directory != nil {
		p, found := s.reviewRecipients[int64(request.UserID)]
		if !found {
			return fmt.Errorf("review recipient must be prepared before the transaction")
		}
		user = auth.Auth{ID: int(p.ID), FirstName: p.Firstname, LastName: p.Lastname, Email: p.Email}
	} else if err := tx.First(&user, "id = ?", request.UserID).Error; err != nil {
		return err
	}
	var details []FileEditRequestDetails
	if err := tx.Where("request_id = ?", request.RequestID).Order("id ASC").Find(&details).Error; err != nil {
		return err
	}
	body := BuildFileEditRequestReviewEmailBody(user.FirstName+" "+user.LastName, request.Status, request.FirstName, request.LastName, request.ReviewComment, details...)
	return s.Outbox.Send(tx, "records", "file-review:"+request.ReviewNotificationKey, []string{user.Email}, "Update to your submission", body)
}

// RecoverReviewIntent covers actionable pre-migration rows without resending true flags.
func (s *FileService) RecoverReviewIntent(id uint) (string, error) {
	if s.Directory != nil {
		prepared, err := s.prepareReviewRecipient(id)
		if err != nil {
			return "", err
		}
		s = prepared
	}
	var key string
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		query := tx
		if tx.Dialector.Name() == "postgres" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		var request FileEditRequest
		if err := query.First(&request, "request_id = ?", id).Error; err != nil {
			return err
		}
		if request.ReviewEmailTriggerSuccess {
			return nil
		}
		if request.ReviewNotificationKey != "" {
			key = request.ReviewNotificationKey
			return nil
		}
		if request.Status == "pending" {
			return nil
		}
		if err := s.enqueueReviewTx(tx, &request); err != nil {
			return err
		}
		key = request.ReviewNotificationKey
		return nil
	})
	return key, err
}
func FileReviewEvent(key, email string) string { return fmt.Sprintf("file-review:%s:%s", key, email) }
