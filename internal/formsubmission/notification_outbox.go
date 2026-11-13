package formsubmission

import (
	"fmt"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"strings"
)

func (s *FormSubmissionService) enqueueReviewTx(tx *gorm.DB, id int64, newDecision bool) error {
	var sub FormSubmission
	if err := tx.Scopes(s.userPreload("CreatedByUser")).First(&sub, "id = ?", id).Error; err != nil {
		return err
	}
	if s.Directory != nil {
		sub.CreatedByUser = s.preparedCreator(sub.CreatedByID)
	}
	if sub.Status == "pending" && !newDecision {
		return nil
	}
	if sub.CreatedByUser == nil {
		return fmt.Errorf("submission creator unavailable")
	}
	if newDecision || sub.ReviewNotificationKey == "" {
		sub.ReviewNotificationKey = uuid.NewString()
		if err := tx.Omit(clause.Associations).Model(&sub).Updates(map[string]any{"review_notification_key": sub.ReviewNotificationKey, "review_email_trigger_success": false}).Error; err != nil {
			return err
		}
	}
	body := BuildFormSubmissionReviewEmailBody(strings.TrimSpace(sub.CreatedByUser.FirstName+" "+sub.CreatedByUser.LastName), sub.FormLabel, sub.Status, sub.FirstName, sub.LastName, sub.ReviewerComment)
	return s.Outbox.Send(tx, "records", "form-review:"+sub.ReviewNotificationKey, []string{sub.CreatedByUser.Email}, fmt.Sprintf("Update to your submission for %s", sub.FormLabel), body)
}
func (s *FormSubmissionService) RecoverReviewIntent(id int64) (string, error) {
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
		var sub FormSubmission
		if err := query.First(&sub, "id = ?", id).Error; err != nil {
			return err
		}
		if sub.ReviewEmailTriggerSuccess {
			return nil
		}
		if sub.ReviewNotificationKey != "" {
			key = sub.ReviewNotificationKey
			return nil
		}
		if sub.Status == "pending" {
			return nil
		}
		if err := s.enqueueReviewTx(tx, id, false); err != nil {
			return err
		}
		return tx.Model(&sub).Select("review_notification_key").Where("id = ?", id).Scan(&key).Error
	})
	return key, err
}
