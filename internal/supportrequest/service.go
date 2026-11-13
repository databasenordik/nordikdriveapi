package supportrequest

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/mail"
	"path/filepath"
	"strings"
	"time"

	"gorm.io/gorm"

	"nordik-drive-api/internal/identityclient"
	"nordik-drive-api/internal/mailer"
	"nordik-drive-api/internal/notificationcontract"
	"nordik-drive-api/internal/outbox"
	"nordik-drive-api/internal/requestscope"
	"nordik-drive-api/internal/util"
)

type SupportRequestMailer interface {
	Send(to []string, subject, body string) error
	SendWithAttachments(to []string, subject, body string, attachments []mailer.Attachment) error
}

type SupportRequestService struct {
	DB                     *gorm.DB
	Mailer                 SupportRequestMailer
	NotificationRecipients []string
	Outbox                 *outbox.Writer
	Directory              identityclient.Directory
}

type preparedScreenshot struct {
	FileName      string
	MimeType      string
	FileSizeBytes int64
	FileURL       string
	Attachment    mailer.Attachment
	Reference     *notificationcontract.Attachment
}

var ErrInvalidSupportRequest = errors.New("invalid support request")
var ErrSupportRequestForbidden = errors.New("support request access is forbidden")
var ErrSupportRequestNotFound = errors.New("support request not found")

const (
	defaultSupportRequestPageSize = 20
	maxSupportRequestPageSize     = 100
)

var uploadSupportRequestScreenshotHook = util.UploadPhotoToGCSContext
var uploadVersionedSupportRequestScreenshotHook = util.UploadPhotoToGCSVersionedContext
var supportRequestGoHook = func(fn func()) {
	fn()
}

var triggerSupportRequestNotificationHook = func(
	req *SupportRequest,
	attachment *mailer.Attachment,
	recipients []string,
	sender SupportRequestMailer,
) error {
	if sender == nil || len(recipients) == 0 {
		return nil
	}

	subject := fmt.Sprintf("New %s support request: %s", supportRequestTypeLabel(req.RequestType), req.Subject)
	body := BuildSupportRequestNotificationEmailBody(req)

	if attachment != nil {
		return sender.SendWithAttachments(recipients, subject, body, []mailer.Attachment{*attachment})
	}

	return sender.Send(recipients, subject, body)
}

var triggerSupportRequestReceiptEmailHook = func(
	req *SupportRequest,
	sender SupportRequestMailer,
) error {
	if sender == nil || strings.TrimSpace(req.RequesterEmail) == "" {
		return nil
	}

	return sender.Send(
		[]string{req.RequesterEmail},
		fmt.Sprintf("We received your support request #%d", req.ID),
		BuildSupportRequestReceiptEmailBody(req),
	)
}

var triggerSupportRequestStatusEmailHook = func(
	req *SupportRequest,
	sender SupportRequestMailer,
) error {
	if sender == nil || strings.TrimSpace(req.RequesterEmail) == "" {
		return nil
	}

	return sender.Send(
		[]string{req.RequesterEmail},
		fmt.Sprintf("Support request #%d is now %s", req.ID, supportRequestStatusLabel(req.Status)),
		BuildSupportRequestStatusEmailBody(req),
	)
}

var triggerSupportRequestForwardEmailHook = func(
	req *SupportRequest,
	recipients []string,
	sender SupportRequestMailer,
) error {
	if sender == nil || len(recipients) == 0 {
		return nil
	}

	return sender.Send(
		recipients,
		fmt.Sprintf("Support request #%d forwarded: %s", req.ID, req.Subject),
		BuildSupportRequestForwardEmailBody(req),
	)
}

func normalizeSupportRequestType(value string) string {
	return strings.TrimSpace(strings.ToLower(value))
}

func (s *SupportRequestService) Create(
	req *CreateSupportRequestRequest,
	userID int,
	screenshot *multipart.FileHeader,
) (*CreateSupportRequestResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("%w: request is required", ErrInvalidSupportRequest)
	}
	if userID <= 0 {
		return nil, fmt.Errorf("%w: valid user ID is required", ErrInvalidSupportRequest)
	}

	user, err := s.lookupUser(userID)
	if err != nil {
		return nil, err
	}

	requestType := normalizeSupportRequestType(req.RequestType)
	if _, ok := validSupportRequestTypes[requestType]; !ok {
		return nil, fmt.Errorf("%w: request_type must be question or technical_issue", ErrInvalidSupportRequest)
	}

	requesterName := strings.TrimSpace(req.RequesterName)
	if requesterName == "" {
		requesterName = strings.TrimSpace(user.FirstName + " " + user.LastName)
	}
	if requesterName == "" {
		return nil, fmt.Errorf("%w: requester_name is required", ErrInvalidSupportRequest)
	}
	if len(requesterName) > MaxRequesterNameLength {
		return nil, fmt.Errorf("%w: requester_name must be %d characters or fewer", ErrInvalidSupportRequest, MaxRequesterNameLength)
	}

	requesterEmail := strings.TrimSpace(req.RequesterEmail)
	if requesterEmail == "" {
		requesterEmail = strings.TrimSpace(user.Email)
	}
	if requesterEmail == "" {
		return nil, fmt.Errorf("%w: requester_email is required", ErrInvalidSupportRequest)
	}
	if len(requesterEmail) > MaxRequesterEmailLength {
		return nil, fmt.Errorf("%w: requester_email must be %d characters or fewer", ErrInvalidSupportRequest, MaxRequesterEmailLength)
	}
	if _, err := mail.ParseAddress(requesterEmail); err != nil {
		return nil, fmt.Errorf("%w: requester_email must be a valid email address", ErrInvalidSupportRequest)
	}

	subject := strings.TrimSpace(req.Subject)
	if subject == "" {
		return nil, fmt.Errorf("%w: subject is required", ErrInvalidSupportRequest)
	}
	if len(subject) > MaxSubjectLength {
		return nil, fmt.Errorf("%w: subject must be %d characters or fewer", ErrInvalidSupportRequest, MaxSubjectLength)
	}

	message := strings.TrimSpace(req.Message)
	if message == "" {
		return nil, fmt.Errorf("%w: message is required", ErrInvalidSupportRequest)
	}
	if len(message) > MaxMessageLength {
		return nil, fmt.Errorf("%w: message must be %d characters or fewer", ErrInvalidSupportRequest, MaxMessageLength)
	}

	now := time.Now().UTC()
	prepared, err := s.prepareScreenshotUpload(screenshot, userID, now)
	if err != nil {
		return nil, err
	}

	record := SupportRequest{
		CreatedByID:    userID,
		RequesterName:  requesterName,
		RequesterEmail: requesterEmail,
		RequestType:    requestType,
		Subject:        subject,
		Message:        message,
		Status:         RequestStatusOpen,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	if prepared != nil {
		record.ScreenshotFileName = prepared.FileName
		record.ScreenshotMimeType = prepared.MimeType
		record.ScreenshotSizeBytes = prepared.FileSizeBytes
		record.ScreenshotURL = prepared.FileURL
	}

	if s.Outbox != nil {
		if err := s.DB.Transaction(func(tx *gorm.DB) error {
			if err := tx.Create(&record).Error; err != nil {
				return err
			}
			return s.enqueueInitialTx(tx, &record, prepared)
		}); err != nil {
			return nil, err
		}
		return &CreateSupportRequestResponse{ID: record.ID, Message: "Support request received successfully."}, nil
	}
	if err := s.DB.Create(&record).Error; err != nil {
		return nil, err
	}

	if s.Mailer != nil {
		recordCopy := record
		var attachmentCopy *mailer.Attachment
		if prepared != nil {
			attachment := prepared.Attachment
			attachmentCopy = &attachment
		}
		s.triggerInitialNotificationsAsync(&recordCopy, attachmentCopy)
	}

	return &CreateSupportRequestResponse{
		ID:      record.ID,
		Message: "Support request received successfully.",
	}, nil
}

func (s *SupportRequestService) lookupUser(userID int) (*SupportRequestUserRef, error) {
	if s.Directory != nil {
		profiles, err := s.Directory.Profiles(requestscope.Context(s.DB), []int64{int64(userID)}, true)
		if err != nil {
			return nil, err
		}
		p, found := profiles[int64(userID)]
		if !found {
			return nil, gorm.ErrRecordNotFound
		}
		return &SupportRequestUserRef{ID: int(p.ID), FirstName: p.Firstname, LastName: p.Lastname, Email: p.Email}, nil
	}
	var user SupportRequestUserRef
	if err := s.DB.First(&user, userID).Error; err != nil {
		return nil, err
	}

	return &user, nil
}

func (s *SupportRequestService) prepareScreenshotUpload(
	fileHeader *multipart.FileHeader,
	userID int,
	now time.Time,
) (*preparedScreenshot, error) {
	if fileHeader == nil {
		return nil, nil
	}

	file, err := fileHeader.Open()
	if err != nil {
		return nil, err
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, MaxScreenshotSizeInBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: screenshot is empty", ErrInvalidSupportRequest)
	}
	if int64(len(data)) > MaxScreenshotSizeInBytes {
		return nil, fmt.Errorf("%w: screenshot must be 5 MB or smaller", ErrInvalidSupportRequest)
	}

	detectedContentType := strings.TrimSpace(http.DetectContentType(data))
	declaredContentType := strings.TrimSpace(fileHeader.Header.Get("Content-Type"))
	contentType := allowedScreenshotContentType(detectedContentType, declaredContentType)
	if contentType == "" {
		return nil, fmt.Errorf("%w: screenshot must be a PNG, JPG, or WEBP image", ErrInvalidSupportRequest)
	}

	displayFilename, objectPath := buildScreenshotFileNames(fileHeader.Filename, contentType, userID, now)
	base64Body := base64.StdEncoding.EncodeToString(data)
	var fileURL string
	var sizeBytes, generation int64
	if s.Outbox != nil {
		fileURL, sizeBytes, generation, err = uploadVersionedSupportRequestScreenshotHook(requestscope.Context(s.DB), fmt.Sprintf("data:%s;base64,%s", contentType, base64Body), "nordik-drive-photos", objectPath)
	} else {
		fileURL, sizeBytes, err = uploadSupportRequestScreenshotHook(requestscope.Context(s.DB),
			fmt.Sprintf("data:%s;base64,%s", contentType, base64Body),
			"nordik-drive-photos",
			objectPath,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to upload screenshot: %w", err)
	}

	if sizeBytes <= 0 {
		sizeBytes = int64(len(data))
	}

	prepared := &preparedScreenshot{
		FileName:      displayFilename,
		MimeType:      contentType,
		FileSizeBytes: sizeBytes,
		FileURL:       fileURL,
		Attachment: mailer.Attachment{
			Filename:    displayFilename,
			ContentType: contentType,
			Data:        data,
		},
	}
	if s.Outbox != nil {
		reference, err := screenshotReference(objectPath, generation, prepared)
		if err != nil {
			return nil, err
		}
		prepared.Reference = &reference
	}
	return prepared, nil
}

func (s *SupportRequestService) triggerInitialNotificationsAsync(
	request *SupportRequest,
	attachment *mailer.Attachment,
) {
	supportRequestGoHook(func() {
		defer func() {
			if recover() != nil {
				_ = s.DB.Model(&SupportRequest{}).
					Where("id = ?", request.ID).
					Updates(map[string]interface{}{
						"notification_email_sent": false,
						"requester_email_sent":    false,
					}).Error
			}
		}()

		if len(s.NotificationRecipients) > 0 {
			if err := triggerSupportRequestNotificationHook(
				request,
				attachment,
				s.NotificationRecipients,
				s.Mailer,
			); err != nil {
				log.Printf("support request admin notification failed for request_id=%d: %v", request.ID, err)
			} else {
				_ = s.DB.Model(&SupportRequest{}).
					Where("id = ?", request.ID).
					Update("notification_email_sent", true).Error
			}
		}

		if err := triggerSupportRequestReceiptEmailHook(request, s.Mailer); err != nil {
			log.Printf("support request receipt email failed for request_id=%d: %v", request.ID, err)
			return
		}

		_ = s.DB.Model(&SupportRequest{}).
			Where("id = ?", request.ID).
			Update("requester_email_sent", true).Error
	})
}

func (s *SupportRequestService) ListMine(userID, page, pageSize int) (*SupportRequestListResponse, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("%w: valid user ID is required", ErrInvalidSupportRequest)
	}

	return s.list(page, pageSize, s.DB.Where("created_by = ?", userID), true)
}

func (s *SupportRequestService) ListForAdmin(userID, page, pageSize int) (*SupportRequestListResponse, error) {
	if err := s.ensureAdmin(userID); err != nil {
		return nil, err
	}

	return s.list(page, pageSize, s.DB, false)
}

func (s *SupportRequestService) list(
	page, pageSize int,
	query *gorm.DB,
	redactManagementFields bool,
) (*SupportRequestListResponse, error) {
	page, pageSize = normalizeSupportRequestPagination(page, pageSize)

	var totalItems int64
	if err := query.Model(&SupportRequest{}).Count(&totalItems).Error; err != nil {
		return nil, err
	}

	items := make([]SupportRequest, 0)
	if err := query.
		Order("created_at DESC").
		Limit(pageSize).
		Offset((page - 1) * pageSize).
		Find(&items).Error; err != nil {
		return nil, err
	}

	if redactManagementFields {
		for index := range items {
			items[index].AssignedTeamRecipients = ""
			items[index].AssignedByID = nil
			items[index].ClosedByID = nil
		}
	}

	totalPages := int((totalItems + int64(pageSize) - 1) / int64(pageSize))
	if totalPages == 0 {
		totalPages = 1
	}

	return &SupportRequestListResponse{
		Page:       page,
		PageSize:   pageSize,
		TotalItems: totalItems,
		TotalPages: totalPages,
		Items:      items,
	}, nil
}

func (s *SupportRequestService) Update(
	id int64,
	request *UpdateSupportRequestRequest,
	adminUserID int,
) (*SupportRequest, error) {
	if id <= 0 {
		return nil, fmt.Errorf("%w: valid request ID is required", ErrInvalidSupportRequest)
	}
	if request == nil {
		return nil, fmt.Errorf("%w: request is required", ErrInvalidSupportRequest)
	}
	if err := s.ensureAdmin(adminUserID); err != nil {
		return nil, err
	}

	status := strings.TrimSpace(strings.ToLower(request.Status))
	if _, ok := validSupportRequestStatuses[status]; !ok {
		return nil, fmt.Errorf("%w: status must be open, in_progress, or closed", ErrInvalidSupportRequest)
	}

	teamName := strings.TrimSpace(request.AssignedTeam)
	if len(teamName) > MaxTeamNameLength {
		return nil, fmt.Errorf("%w: assigned_team must be %d characters or fewer", ErrInvalidSupportRequest, MaxTeamNameLength)
	}

	adminNote := strings.TrimSpace(request.AdminNote)
	if len(adminNote) > MaxAdminNoteLength {
		return nil, fmt.Errorf("%w: admin_note must be %d characters or fewer", ErrInvalidSupportRequest, MaxAdminNoteLength)
	}

	teamRecipients, err := parseSupportRequestEmailRecipients(request.AssignedTeamRecipients)
	if err != nil {
		return nil, err
	}

	var record SupportRequest
	if err := s.DB.First(&record, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrSupportRequestNotFound
		}
		return nil, err
	}

	if status == RequestStatusInProgress {
		if teamName == "" {
			teamName = record.AssignedTeam
		}
		if len(teamRecipients) == 0 {
			teamRecipients, err = parseSupportRequestEmailRecipients(record.AssignedTeamRecipients)
			if err != nil {
				return nil, err
			}
		}
		if teamName == "" || len(teamRecipients) == 0 {
			return nil, fmt.Errorf("%w: forwarding to a team and at least one team email are required for in_progress", ErrInvalidSupportRequest)
		}
	}

	now := time.Now().UTC()
	record.Status = status
	record.AdminNote = adminNote
	if status == RequestStatusInProgress {
		record.AssignedTeam = teamName
		record.AssignedTeamRecipients = strings.Join(teamRecipients, ", ")
		record.AssignedByID = &adminUserID
		record.AssignedAt = &now
	}
	if status == RequestStatusClosed {
		record.ClosedByID = &adminUserID
		record.ClosedAt = &now
	} else {
		record.ClosedByID = nil
		record.ClosedAt = nil
	}

	if s.Outbox != nil {
		record.NotificationVersion = now.UnixNano()
		record.StatusEmailSent = false
		if status == RequestStatusInProgress {
			record.TeamForwardEmailSent = false
		}
		err := s.DB.Transaction(func(tx *gorm.DB) error {
			if err := tx.Save(&record).Error; err != nil {
				return err
			}
			return s.enqueueManagementTx(tx, &record, teamRecipients, status == RequestStatusInProgress)
		})
		return &record, err
	}
	if err := s.DB.Save(&record).Error; err != nil {
		return nil, err
	}

	if s.Mailer != nil {
		recordCopy := record
		s.triggerManagementNotificationsAsync(&recordCopy, teamRecipients, status == RequestStatusInProgress)
	}

	return &record, nil
}

func (s *SupportRequestService) triggerManagementNotificationsAsync(
	request *SupportRequest,
	teamRecipients []string,
	forwarded bool,
) {
	supportRequestGoHook(func() {
		defer func() {
			if recover() != nil {
				_ = s.DB.Model(&SupportRequest{}).
					Where("id = ?", request.ID).
					Updates(map[string]interface{}{
						"status_email_sent":       false,
						"team_forward_email_sent": false,
					}).Error
			}
		}()

		if forwarded {
			if err := triggerSupportRequestForwardEmailHook(request, teamRecipients, s.Mailer); err != nil {
				log.Printf("support request forward email failed for request_id=%d: %v", request.ID, err)
			} else {
				_ = s.DB.Model(&SupportRequest{}).
					Where("id = ?", request.ID).
					Update("team_forward_email_sent", true).Error
			}
		}

		if err := triggerSupportRequestStatusEmailHook(request, s.Mailer); err != nil {
			log.Printf("support request status email failed for request_id=%d: %v", request.ID, err)
			return
		}

		_ = s.DB.Model(&SupportRequest{}).
			Where("id = ?", request.ID).
			Update("status_email_sent", true).Error
	})
}

func (s *SupportRequestService) ensureAdmin(userID int) error {
	if s.Directory != nil {
		if userID <= 0 {
			return ErrSupportRequestForbidden
		}
		principals, err := s.Directory.Principals(requestscope.Context(s.DB), []int64{int64(userID)})
		if err != nil {
			return err
		}
		principal, found := principals[int64(userID)]
		if !found || !strings.EqualFold(principal.Role, "admin") {
			return ErrSupportRequestForbidden
		}
		return nil
	}
	if userID <= 0 {
		return ErrSupportRequestForbidden
	}

	var count int64
	if err := s.DB.Model(&SupportRequestUserRef{}).
		Where("id = ? AND LOWER(role) = ?", userID, "admin").
		Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		return ErrSupportRequestForbidden
	}

	return nil
}

func normalizeSupportRequestPagination(page, pageSize int) (int, int) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = defaultSupportRequestPageSize
	}
	if pageSize > maxSupportRequestPageSize {
		pageSize = maxSupportRequestPageSize
	}

	return page, pageSize
}

func parseSupportRequestEmailRecipients(value string) ([]string, error) {
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n'
	})
	recipients := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		email := strings.TrimSpace(part)
		if email == "" {
			continue
		}
		parsed, err := mail.ParseAddress(email)
		if err != nil || parsed.Address != email {
			return nil, fmt.Errorf("%w: assigned_team_recipients must contain valid email addresses", ErrInvalidSupportRequest)
		}
		key := strings.ToLower(email)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		recipients = append(recipients, email)
	}

	return recipients, nil
}

func allowedScreenshotContentType(values ...string) string {
	for _, value := range values {
		normalized := strings.TrimSpace(strings.ToLower(value))
		if _, ok := validScreenshotMimeTypes[normalized]; ok {
			return normalized
		}
	}

	return ""
}

func buildScreenshotFileNames(originalName string, contentType string, userID int, now time.Time) (string, string) {
	extension := validScreenshotMimeTypes[contentType]

	displayName := strings.TrimSpace(filepath.Base(originalName))
	if displayName == "" {
		displayName = "screenshot" + extension
	}
	if filepath.Ext(displayName) == "" {
		displayName += extension
	}

	baseName := strings.TrimSuffix(displayName, filepath.Ext(displayName))
	safeBaseName := util.SanitizePart(baseName)
	if safeBaseName == "" || safeBaseName == "unknown" {
		safeBaseName = "screenshot"
	}

	objectPath := fmt.Sprintf(
		"support_requests/%d/%s_%s%s",
		userID,
		now.Format("20060102150405"),
		safeBaseName,
		extension,
	)

	return displayName, objectPath
}
