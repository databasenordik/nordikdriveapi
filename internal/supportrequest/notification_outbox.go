package supportrequest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"gorm.io/gorm"
	"nordik-drive-api/internal/notificationcontract"
	"nordik-drive-api/internal/outbox"
	"strconv"
	"strings"
	"time"
)

func screenshotReference(object string, generation int64, p *preparedScreenshot) (notificationcontract.Attachment, error) {
	hash := sha256.Sum256(p.Attachment.Data)
	ref := notificationcontract.Attachment{Bucket: "nordik-drive-photos", Object: object, Generation: generation, Size: p.FileSizeBytes, SHA256: hex.EncodeToString(hash[:]), Filename: p.FileName, ContentType: p.MimeType}
	if !strings.HasPrefix(object, "support_requests/") || generation <= 0 {
		return ref, errors.New("immutable screenshot reference unavailable")
	}
	return ref, nil
}
func supportEvent(id int64, flag string, version int64) string {
	return fmt.Sprintf("support-request:%d:%s:%d", id, flag, version)
}
func (s *SupportRequestService) enqueueInitialTx(tx *gorm.DB, r *SupportRequest, p *preparedScreenshot) error {
	var refs []notificationcontract.Attachment
	if p != nil {
		if p.Reference == nil {
			return errors.New("screenshot reference required")
		}
		refs = append(refs, *p.Reference)
	}
	if len(s.NotificationRecipients) > 0 {
		if err := s.Outbox.Send(tx, "support", supportEvent(r.ID, "notification_email_sent", 0), s.NotificationRecipients, fmt.Sprintf("New %s support request: %s", supportRequestTypeLabel(r.RequestType), r.Subject), BuildSupportRequestNotificationEmailBody(r), refs...); err != nil {
			return err
		}
	}
	return s.Outbox.Send(tx, "support", supportEvent(r.ID, "requester_email_sent", 0), []string{r.RequesterEmail}, fmt.Sprintf("We received your support request #%d", r.ID), BuildSupportRequestReceiptEmailBody(r))
}
func (s *SupportRequestService) enqueueManagementTx(tx *gorm.DB, r *SupportRequest, recipients []string, forward bool) error {
	if forward && len(recipients) > 0 {
		if err := s.Outbox.Send(tx, "support", supportEvent(r.ID, "team_forward_email_sent", r.NotificationVersion), recipients, fmt.Sprintf("Support request #%d forwarded: %s", r.ID, r.Subject), BuildSupportRequestForwardEmailBody(r)); err != nil {
			return err
		}
	}
	return s.Outbox.Send(tx, "support", supportEvent(r.ID, "status_email_sent", r.NotificationVersion), []string{r.RequesterEmail}, fmt.Sprintf("Support request #%d is now %s", r.ID, supportRequestStatusLabel(r.Status)), BuildSupportRequestStatusEmailBody(r))
}

// ReconcileOutgoing changes business flags only after every recipient was accepted.
func ReconcileOutgoing(ctx context.Context, db *gorm.DB, remote outbox.Remote) error {
	var records []outbox.Record
	if err := db.WithContext(ctx).Where("producer = ? AND state = ? AND event_key LIKE ? AND next_attempt <= ?", "support", "provider_accepted", "support-request:%", time.Now().UTC()).Order("next_attempt ASC").Limit(20).Find(&records).Error; err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, row := range records {
		parts := strings.SplitN(row.EventKey, ":", 5)
		if len(parts) != 5 {
			return errors.New("invalid support notification reference")
		}
		prefix := strings.Join(parts[:4], ":")
		if seen[prefix] {
			continue
		}
		seen[prefix] = true
		id, err := strconv.ParseUint(parts[1], 10, 64)
		if err != nil {
			return err
		}
		version, err := strconv.ParseInt(parts[3], 10, 64)
		if err != nil {
			return err
		}
		flag := parts[2]
		if flag != "notification_email_sent" && flag != "requester_email_sent" && flag != "status_email_sent" && flag != "team_forward_email_sent" {
			return errors.New("invalid support notification flag")
		}
		state, err := outbox.GroupState(ctx, db, remote, "support", prefix)
		if err != nil {
			return err
		}
		if state == "pending" {
			if err := db.WithContext(ctx).Model(&outbox.Record{}).Where("producer = ? AND event_key LIKE ? ESCAPE '!' AND state = ?", "support", outbox.EventPattern(prefix), "provider_accepted").Update("next_attempt", time.Now().UTC().Add(5*time.Minute)).Error; err != nil {
				return err
			}
			continue
		}
		if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			query := tx.Model(&SupportRequest{}).Where("id = ?", id)
			if version != 0 {
				query = query.Where("notification_version = ?", version)
			}
			if state == "provider_accepted" {
				if err := query.Update(flag, true).Error; err != nil {
					return err
				}
			}
			return tx.Model(&outbox.Record{}).Where("producer = ? AND event_key LIKE ? ESCAPE '!' AND state = ?", "support", outbox.EventPattern(prefix), "provider_accepted").Update("state", "processed").Error
		}); err != nil {
			return err
		}
	}
	return nil
}
