package formsubmission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/storage"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	filehelper "nordik-drive-api/internal/file"
	"nordik-drive-api/internal/mailer"
	"nordik-drive-api/internal/requestscope"
	"nordik-drive-api/internal/util"
)

type FormSubmissionService struct {
	DB     *gorm.DB
	Mailer mailer.EmailSender
}

var uploadBase64ToGCSHook = util.UploadPhotoToGCSContext
var newFormSubmissionGCSClientHook = func(ctx context.Context) (*storage.Client, error) {
	return storage.NewClient(ctx)
}

var (
	ErrInvalidReviewRequest        = errors.New("invalid review request")
	ErrFormSubmissionNotFound      = errors.New("form submission not found")
	ErrUploadNotFoundForSubmission = errors.New("one or more uploads do not belong to the submission")
)

var triggerFormSubmissionReviewEmailHook = func(sub *FormSubmission, mailer mailer.EmailSender) error {
	createdUserName := strings.TrimSpace(sub.CreatedByUser.FirstName + " " + sub.CreatedByUser.LastName)

	body := BuildFormSubmissionReviewEmailBody(
		createdUserName,
		sub.FormLabel,
		sub.Status,
		sub.FirstName,
		sub.LastName,
		sub.ReviewerComment,
	)

	err := mailer.SendOne(
		sub.CreatedByUser.Email,
		fmt.Sprintf("Update to your submission for %s", sub.FormLabel),
		body,
	)

	return err
}

func normalizeReviewStatus(v string) string {
	return strings.TrimSpace(strings.ToLower(v))
}

func isValidReviewStatus(v string) bool {
	_, ok := validReviewStatuses[normalizeReviewStatus(v)]
	return ok
}

func buildReviewUpdateMap(status string, reviewerComment string, reviewerID int, reviewedAt time.Time) map[string]interface{} {
	status = normalizeReviewStatus(status)

	if status == "pending" {
		return map[string]interface{}{
			"status":           status,
			"reviewer_comment": "",
			"reviewed_by":      nil,
			"reviewed_at":      nil,
		}
	}

	return map[string]interface{}{
		"status":           status,
		"reviewer_comment": strings.TrimSpace(reviewerComment),
		"reviewed_by":      reviewerID,
		"reviewed_at":      reviewedAt,
	}
}

type formSubmissionReadCloser struct {
	io.ReadCloser
	closeFn func() error
}

func (r *formSubmissionReadCloser) Close() error {
	var firstErr error

	if r.ReadCloser != nil {
		if err := r.ReadCloser.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if r.closeFn != nil {
		if err := r.closeFn(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

var formSubmissionGoHook = func(fn func()) {
	go fn()
}

var openFormUploadReaderHook = func(ctx context.Context, rec FormSubmissionUpload) (io.ReadCloser, string, string, error) {
	bucket, objectPath, err := parseFormUploadGSURL(rec.FileURL)
	if err != nil {
		return nil, "", "", err
	}

	client, err := newFormSubmissionGCSClientHook(ctx)
	if err != nil {
		return nil, "", "", err
	}

	rc, err := client.Bucket(bucket).Object(objectPath).NewReader(ctx)
	if err != nil {
		_ = client.Close()
		return nil, "", "", err
	}

	return &formSubmissionReadCloser{
		ReadCloser: rc,
		closeFn:    client.Close,
	}, strings.TrimSpace(rc.ContentType()), objectPath, nil
}

func (s *FormSubmissionService) triggerReviewEmailAsync(sub *FormSubmission) {
	formSubmissionGoHook(func() {
		defer func() {
			if recover() != nil {
				_ = s.DB.Model(&FormSubmission{}).
					Where("id = ?", sub.ID).
					Update("review_email_trigger_success", false).Error
			}
		}()

		if err := triggerFormSubmissionReviewEmailHook(sub, s.Mailer); err != nil {
			return
		}

		_ = s.DB.Model(&FormSubmission{}).
			Where("id = ?", sub.ID).
			Update("review_email_trigger_success", true).Error
	})
}

func parseFormUploadGSURL(gsURL string) (bucket string, objectPath string, err error) {
	gsURL = strings.TrimSpace(gsURL)
	if gsURL == "" {
		return "", "", fmt.Errorf("empty gs url")
	}
	if !strings.HasPrefix(gsURL, "gs://") {
		return "", "", fmt.Errorf("invalid gs url (must start with gs://): %s", gsURL)
	}

	rest := strings.TrimPrefix(gsURL, "gs://")
	slash := strings.Index(rest, "/")
	if slash < 0 || slash == len(rest)-1 {
		return "", "", fmt.Errorf("invalid gs url format: %s", gsURL)
	}

	bucket = strings.TrimSpace(rest[:slash])
	objectPath = strings.TrimSpace(rest[slash+1:])
	if bucket == "" || objectPath == "" {
		return "", "", fmt.Errorf("invalid gs url format: %s", gsURL)
	}

	return bucket, objectPath, nil
}

func userEmail(u *FormSubmissionUserRef) string {
	if u == nil {
		return ""
	}
	return u.Email
}

func (s *FormSubmissionService) uploadFormFiles(req *SaveFormSubmissionRequest) ([]FormSubmissionUploadInput, []FormSubmissionUploadInput, error) {
	folder := util.SanitizePart(req.FormKey)
	switch folder {
	case "boarding", "boarding_tab", "boarding_home":
		folder = "boarding_home"
	}

	basePrefix := fmt.Sprintf("requests/%s/%d_%d", folder, req.FileID, req.RowID)
	timestamp := time.Now().UTC().Format("20060102150405")
	bucket := "nordik-drive-photos"

	safeBase := func(name string) string {
		name = strings.TrimSpace(name)
		ext := path.Ext(name)
		base := strings.TrimSpace(strings.TrimSuffix(name, ext))
		base = util.SanitizePart(base)
		if base == "" {
			base = "file"
		}
		return base
	}

	type job struct {
		kind string
		idx  int
		item FormSubmissionUploadInput
	}
	type result struct {
		kind string
		idx  int
		item FormSubmissionUploadInput
		err  error
	}

	docs := make([]FormSubmissionUploadInput, len(req.Documents))
	photos := make([]FormSubmissionUploadInput, len(req.Photos))

	jobs := make([]job, 0, len(req.Documents)+len(req.Photos))
	for i, d := range req.Documents {
		jobs = append(jobs, job{kind: "document", idx: i, item: d})
	}
	for i, p := range req.Photos {
		jobs = append(jobs, job{kind: "photo", idx: i, item: p})
	}

	if len(jobs) == 0 {
		return docs, photos, nil
	}

	sem := make(chan struct{}, 4) // 4 parallel uploads
	outCh := make(chan result, len(jobs))
	var wg sync.WaitGroup

	for _, j := range jobs {
		select {
		case sem <- struct{}{}:
		case <-requestscope.Context(s.DB).Done():
			wg.Wait()
			return nil, nil, requestscope.Context(s.DB).Err()
		}
		wg.Add(1)

		go func(j job) {
			defer wg.Done()

			defer func() { <-sem }()

			out := j.item

			// Backward compatible: allow already-uploaded URLs
			if strings.TrimSpace(out.DataBase64) == "" {
				if strings.TrimSpace(out.FileURL) == "" {
					outCh <- result{
						err: fmt.Errorf("%s upload %d is missing both data_base64 and file_url", j.kind, j.idx+1),
					}
					return
				}

				outCh <- result{
					kind: j.kind,
					idx:  j.idx,
					item: out,
				}
				return
			}

			ext := util.ExtFromFilenameOrMime(out.FileName, out.MimeType)

			objectName := fmt.Sprintf(
				"%s/%s_%s_%d_%s%s",
				basePrefix,
				j.kind,
				timestamp,
				j.idx+1,
				safeBase(out.FileName),
				ext,
			)

			url, sizeBytes, err := uploadBase64ToGCSHook(requestscope.Context(s.DB),
				out.DataBase64,
				bucket,
				objectName,
			)
			if err != nil {
				outCh <- result{
					err: fmt.Errorf("failed to upload %s %q: %w", j.kind, strings.TrimSpace(out.FileName), err),
				}
				return
			}

			out.FileURL = url
			out.FileSizeBytes = sizeBytes

			outCh <- result{
				kind: j.kind,
				idx:  j.idx,
				item: out,
			}
		}(j)
	}

	wg.Wait()
	close(outCh)

	for r := range outCh {
		if r.err != nil {
			return nil, nil, r.err
		}

		if r.kind == "document" {
			docs[r.idx] = r.item
		} else {
			photos[r.idx] = r.item
		}
	}

	return docs, photos, nil
}

func (s *FormSubmissionService) Upsert(req *SaveFormSubmissionRequest, userId int) (*GetFormSubmissionResponse, error) {
	if req == nil {
		return nil, errors.New("request is required")
	}
	if req.FileID <= 0 {
		return nil, errors.New("file_id is required")
	}
	if req.RowID <= 0 {
		return nil, errors.New("row_id is required")
	}
	if strings.TrimSpace(req.FormKey) == "" {
		return nil, errors.New("form_key is required")
	}
	if strings.TrimSpace(req.FormLabel) == "" {
		return nil, errors.New("form_label is required")
	}
	if err := filehelper.EnsureFileVersionTransitionIdleByID(s.DB, uint(req.FileID)); err != nil {
		return nil, err
	}

	formKey := strings.TrimSpace(req.FormKey)
	fileName := strings.TrimSpace(req.FileName)
	formLabel := strings.TrimSpace(req.FormLabel)
	firstName := strings.TrimSpace(req.FirstName)
	lastName := strings.TrimSpace(req.LastName)

	// Validate detail keys before upload
	detailKeys := make(map[string]struct{}, len(req.Details))
	for _, d := range req.Details {
		key := strings.TrimSpace(d.DetailKey)
		if key == "" {
			return nil, errors.New("detail_key is required in details")
		}
		detailKeys[key] = struct{}{}
	}

	for _, d := range req.Documents {
		key := strings.TrimSpace(d.DetailKey)
		if key == "" {
			return nil, errors.New("detail_key is required in documents")
		}
		if _, ok := detailKeys[key]; !ok {
			return nil, fmt.Errorf("document detail_key not found in details: %s", key)
		}
	}

	for _, p := range req.Photos {
		key := strings.TrimSpace(p.DetailKey)
		if key == "" {
			return nil, errors.New("detail_key is required in photos")
		}
		if _, ok := detailKeys[key]; !ok {
			return nil, fmt.Errorf("photo detail_key not found in details: %s", key)
		}
	}

	// Upload first (outside DB transaction)
	uploadedDocs, uploadedPhotos, err := s.uploadFormFiles(req)
	if err != nil {
		return nil, err
	}

	var savedSubmissionID int64
	err = s.DB.Transaction(func(tx *gorm.DB) error {
		var sub FormSubmission
		findErr := tx.
			Where("file_id = ? AND row_id = ? AND form_key = ? AND status <> ?", req.FileID, req.RowID, formKey, ReviewStatusRejected).
			Order("id desc").
			First(&sub).Error

		if findErr != nil && !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}

		if errors.Is(findErr, gorm.ErrRecordNotFound) {
			sub = FormSubmission{
				FileID:       req.FileID,
				RowID:        req.RowID,
				FileName:     fileName,
				FormKey:      formKey,
				FormLabel:    formLabel,
				ConsentText:  req.ConsentText,
				ConsentGiven: req.Consent,
				FirstName:    firstName,
				LastName:     lastName,
				CreatedByID:  &userId,
			}
			if err := tx.Create(&sub).Error; err != nil {
				return err
			}
		} else {
			if err := tx.Model(&sub).Updates(map[string]interface{}{
				"file_name":     fileName,
				"form_label":    formLabel,
				"consent_text":  req.ConsentText,
				"consent_given": req.Consent,
				"firstname":     firstName,
				"lastname":      lastName,
				"edited_by":     userId,
			}).Error; err != nil {
				return err
			}
		}

		savedSubmissionID = sub.ID

		// Load existing details once so we can preserve IDs
		var existingDetails []FormSubmissionDetail
		if err := tx.Where("submission_id = ?", sub.ID).Find(&existingDetails).Error; err != nil {
			return err
		}

		existingByKey := make(map[string]FormSubmissionDetail, len(existingDetails))
		detailIDByKey := make(map[string]int64, len(existingDetails))

		for _, ed := range existingDetails {
			key := strings.TrimSpace(ed.DetailKey)
			if key == "" {
				continue
			}
			existingByKey[key] = ed
			detailIDByKey[key] = ed.ID
		}

		// Upsert details by detail_key (preserve old IDs)
		for _, d := range req.Details {
			key := strings.TrimSpace(d.DetailKey)

			raw, err := json.Marshal(d.Value)
			if err != nil {
				return fmt.Errorf("failed to marshal value for detail_key %s: %w", key, err)
			}
			if len(raw) == 0 {
				raw = []byte("null")
			}

			if existing, ok := existingByKey[key]; ok {
				if err := tx.Model(&FormSubmissionDetail{}).
					Where("id = ?", existing.ID).
					Updates(map[string]interface{}{
						"detail_label":     strings.TrimSpace(d.DetailLabel),
						"field_type":       strings.TrimSpace(d.FieldType),
						"consent_required": d.ConsentRequired,
						"value_json":       datatypes.JSON(raw),
					}).Error; err != nil {
					return err
				}
				detailIDByKey[key] = existing.ID
				continue
			}

			row := FormSubmissionDetail{
				SubmissionID:    sub.ID,
				DetailKey:       key,
				DetailLabel:     strings.TrimSpace(d.DetailLabel),
				FieldType:       strings.TrimSpace(d.FieldType),
				ConsentRequired: d.ConsentRequired,
				ValueJSON:       datatypes.JSON(raw),
			}

			if err := tx.Create(&row).Error; err != nil {
				return err
			}

			detailIDByKey[key] = row.ID
		}

		// Insert ONLY newly uploaded files. Do not delete old uploads.
		createUpload := func(uploadType string, in FormSubmissionUploadInput) error {
			key := strings.TrimSpace(in.DetailKey)
			detailID, ok := detailIDByKey[key]
			if !ok {
				return fmt.Errorf("upload detail_key not found in details: %s", key)
			}

			row := FormSubmissionUpload{
				SubmissionID:    sub.ID,
				DetailID:        detailID,
				UploadType:      uploadType,
				FileName:        strings.TrimSpace(in.FileName),
				MimeType:        strings.TrimSpace(in.MimeType),
				FileSizeBytes:   in.FileSizeBytes,
				FileURL:         strings.TrimSpace(in.FileURL),
				FileCategory:    strings.TrimSpace(in.FileCategory),
				FileComment:     strings.TrimSpace(in.FileComment),
				Status:          "pending",
				ReviewerComment: "",
			}

			return tx.Create(&row).Error
		}

		for _, d := range uploadedDocs {
			if err := createUpload("document", d); err != nil {
				return err
			}
		}

		for _, p := range uploadedPhotos {
			if err := createUpload("photo", p); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return s.getSubmissionResponseByID(savedSubmissionID)
}

func (s *FormSubmissionService) GetByRowAndForm(rowID int64, formKey string, fileID *int64) (*GetFormSubmissionResponse, error) {
	key := strings.TrimSpace(formKey)
	if rowID <= 0 {
		return nil, errors.New("row_id is required")
	}
	if key == "" {
		return nil, errors.New("form_key is required")
	}

	q := s.DB.
		Preload("CreatedByUser", preloadFormSubmissionUser).
		Preload("EditedByUser", preloadFormSubmissionUser).
		Preload("ReviewedByUser", preloadFormSubmissionUser).
		Where("row_id = ? AND form_key = ?", rowID, key)

	if fileID != nil && *fileID > 0 {
		q = q.Where("file_id = ?", *fileID)
	}

	var sub FormSubmission
	err := q.First(&sub).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			resp := &GetFormSubmissionResponse{
				Found:                     false,
				ID:                        0,
				RowID:                     rowID,
				FormKey:                   key,
				Details:                   []FormSubmissionDetailResponse{},
				Documents:                 []FormSubmissionUploadResponse{},
				Photos:                    []FormSubmissionUploadResponse{},
				Status:                    "pending",
				ReviewerComment:           "",
				ReviewEmailTriggerSuccess: false,
			}
			if fileID != nil {
				resp.FileID = *fileID
			}
			return resp, nil
		}
		return nil, err
	}

	return s.buildFormSubmissionResponse(sub)
}

func (s *FormSubmissionService) GetActiveByRowAndForm(rowID int64, formKey string, fileID *int64) (*GetFormSubmissionResponse, error) {
	key := strings.TrimSpace(formKey)
	if rowID <= 0 {
		return nil, errors.New("row_id is required")
	}
	if key == "" {
		return nil, errors.New("form_key is required")
	}

	q := s.DB.
		Preload("CreatedByUser", preloadFormSubmissionUser).
		Preload("EditedByUser", preloadFormSubmissionUser).
		Preload("ReviewedByUser", preloadFormSubmissionUser).
		Where("row_id = ? AND form_key = ? AND status <> ?", rowID, key, ReviewStatusRejected).
		Order("id desc")

	if fileID != nil && *fileID > 0 {
		q = q.Where("file_id = ?", *fileID)
	}

	var sub FormSubmission
	err := q.First(&sub).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}

	return s.buildFormSubmissionResponse(sub)
}

func (s *FormSubmissionService) GetByID(id int64) (*GetFormSubmissionResponse, error) {
	if id <= 0 {
		return nil, errors.New("id is required")
	}

	var sub FormSubmission
	if err := s.DB.
		Preload("CreatedByUser", preloadFormSubmissionUser).
		Preload("EditedByUser", preloadFormSubmissionUser).
		Preload("ReviewedByUser", preloadFormSubmissionUser).
		First(&sub, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return &GetFormSubmissionResponse{
				Found:                     false,
				ID:                        id,
				Details:                   []FormSubmissionDetailResponse{},
				Documents:                 []FormSubmissionUploadResponse{},
				Photos:                    []FormSubmissionUploadResponse{},
				Status:                    "pending",
				ReviewerComment:           "",
				ReviewEmailTriggerSuccess: false,
			}, nil
		}
		return nil, err
	}

	return s.buildFormSubmissionResponse(sub)
}

func preloadFormSubmissionUser(db *gorm.DB) *gorm.DB {
	return db.Select("id", "email")
}

func (s *FormSubmissionService) getSubmissionResponseByID(id int64) (*GetFormSubmissionResponse, error) {
	var sub FormSubmission
	if err := s.DB.
		Preload("CreatedByUser", preloadFormSubmissionUser).
		Preload("EditedByUser", preloadFormSubmissionUser).
		Preload("ReviewedByUser", preloadFormSubmissionUser).
		First(&sub, id).Error; err != nil {
		return nil, err
	}

	return s.buildFormSubmissionResponse(sub)
}

func (s *FormSubmissionService) buildFormSubmissionResponse(sub FormSubmission) (*GetFormSubmissionResponse, error) {
	var details []FormSubmissionDetail
	if err := s.DB.
		Where("submission_id = ?", sub.ID).
		Order("id asc").
		Find(&details).Error; err != nil {
		return nil, err
	}

	var uploads []FormSubmissionUpload
	if err := s.DB.
		Preload("ReviewedByUser", preloadFormSubmissionUser).
		Where("submission_id = ?", sub.ID).
		Order("id asc").
		Find(&uploads).Error; err != nil {
		return nil, err
	}

	detailKeyByID := make(map[int64]string, len(details))
	respDetails := make([]FormSubmissionDetailResponse, 0, len(details))

	for _, d := range details {
		detailKeyByID[d.ID] = d.DetailKey

		var val interface{}
		if len(d.ValueJSON) > 0 {
			if err := json.Unmarshal(d.ValueJSON, &val); err != nil {
				return nil, err
			}
		}

		respDetails = append(respDetails, FormSubmissionDetailResponse{
			DetailKey:       d.DetailKey,
			DetailLabel:     d.DetailLabel,
			FieldType:       d.FieldType,
			ConsentRequired: d.ConsentRequired,
			Value:           val,
		})
	}

	respDocs := make([]FormSubmissionUploadResponse, 0)
	respPhotos := make([]FormSubmissionUploadResponse, 0)

	for _, u := range uploads {
		item := FormSubmissionUploadResponse{
			ID:              u.ID,
			DetailKey:       detailKeyByID[u.DetailID],
			FileName:        u.FileName,
			MimeType:        u.MimeType,
			FileSizeBytes:   u.FileSizeBytes,
			FileURL:         u.FileURL,
			FileCategory:    u.FileCategory,
			FileComment:     u.FileComment,
			Status:          u.Status,
			ReviewerComment: u.ReviewerComment,
			ReviewedBy:      userEmail(u.ReviewedByUser),
			ReviewedAt:      u.ReviewedAt,
		}

		if strings.EqualFold(u.UploadType, "document") {
			respDocs = append(respDocs, item)
		} else {
			respPhotos = append(respPhotos, item)
		}
	}

	return &GetFormSubmissionResponse{
		ID:          sub.ID,
		Found:       true,
		FileID:      sub.FileID,
		RowID:       sub.RowID,
		FileName:    sub.FileName,
		FormKey:     sub.FormKey,
		FormLabel:   sub.FormLabel,
		ConsentText: sub.ConsentText,
		Consent:     sub.ConsentGiven,
		Details:     respDetails,
		Documents:   respDocs,
		Photos:      respPhotos,
		FirstName:   sub.FirstName,
		LastName:    sub.LastName,
		CreatedBy:   userEmail(sub.CreatedByUser),
		EditedBy:    userEmail(sub.EditedByUser),
		ReviewedBy:  userEmail(sub.ReviewedByUser),

		Status:          sub.Status,
		ReviewerComment: sub.ReviewerComment,
		ReviewedAt:      sub.ReviewedAt,

		ReviewEmailTriggerSuccess: sub.ReviewEmailTriggerSuccess,
	}, nil
}

func (s *FormSubmissionService) GetUploadBytes(id uint) ([]byte, string, string, error) {
	var rec FormSubmissionUpload
	if err := s.DB.First(&rec, id).Error; err != nil {
		return nil, "", "", err
	}

	ctx := requestscope.Context(s.DB)
	rc, contentType, objectPath, err := openFormUploadReaderHook(ctx, rec)
	if err != nil {
		return nil, "", "", err
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, "", "", err
	}

	contentType = strings.TrimSpace(contentType)
	if contentType == "" {
		contentType = strings.TrimSpace(rec.MimeType)
	}
	if contentType == "" {
		contentType = http.DetectContentType(data)
	}

	filename := strings.TrimSpace(rec.FileName)
	if filename == "" {
		filename = path.Base(objectPath)
	}
	if filename == "" {
		filename = fmt.Sprintf("upload_%d", rec.ID)
	}

	return data, contentType, filename, nil
}

func (s *FormSubmissionService) SearchSubmissions(
	ctx context.Context,
	req SearchFormSubmissionsRequest,
	page int,
	pageSize int,
) (*PaginatedFormSubmissionsResponse, error) {
	q := s.DB.WithContext(ctx).Model(&FormSubmission{})

	if req.FileID != nil && *req.FileID > 0 {
		q = q.Where("file_id = ?", *req.FileID)
	}

	if req.FormKey != nil {
		key := strings.TrimSpace(*req.FormKey)
		if key != "" {
			q = q.Where("form_key = ?", key)
		}
	}

	if req.FirstName != nil {
		v := strings.TrimSpace(*req.FirstName)
		if v != "" {
			q = q.Where("firstname ILIKE ?", "%"+v+"%")
		}
	}

	if req.LastName != nil {
		v := strings.TrimSpace(*req.LastName)
		if v != "" {
			q = q.Where("lastname ILIKE ?", "%"+v+"%")
		}
	}

	if req.CreatedBy != nil && *req.CreatedBy > 0 {
		q = q.Where("created_by = ?", *req.CreatedBy)
	}

	if req.ConsentGiven != nil {
		q = q.Where("consent_given = ?", *req.ConsentGiven)
	}

	if len(req.Status) > 0 {
		q = q.Where("status IN ?", req.Status)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, err
	}

	offset := (page - 1) * pageSize

	preloadUser := func(db *gorm.DB) *gorm.DB {
		return db.Select("id", "email")
	}

	var items []FormSubmission
	if err := q.
		Preload("CreatedByUser", preloadUser).
		Preload("EditedByUser", preloadUser).
		Preload("ReviewedByUser", preloadUser).
		Order("updated_at desc").
		Order("id desc").
		Limit(pageSize).
		Offset(offset).
		Find(&items).Error; err != nil {
		return nil, err
	}

	respItems := make([]FormSubmissionListItemResponse, 0, len(items))
	for _, item := range items {
		respItems = append(respItems, FormSubmissionListItemResponse{
			ID:                        item.ID,
			FileID:                    item.FileID,
			RowID:                     item.RowID,
			FileName:                  item.FileName,
			FormKey:                   item.FormKey,
			FormLabel:                 item.FormLabel,
			ConsentText:               item.ConsentText,
			ConsentGiven:              item.ConsentGiven,
			CreatedAt:                 item.CreatedAt,
			UpdatedAt:                 item.UpdatedAt,
			FirstName:                 item.FirstName,
			LastName:                  item.LastName,
			CreatedBy:                 userEmail(item.CreatedByUser),
			EditedBy:                  userEmail(item.EditedByUser),
			ReviewedBy:                userEmail(item.ReviewedByUser),
			Status:                    item.Status,
			ReviewerComment:           item.ReviewerComment,
			ReviewedAt:                item.ReviewedAt,
			ReviewEmailTriggerSuccess: item.ReviewEmailTriggerSuccess,
		})
	}

	totalPages := 0
	if total > 0 {
		totalPages = int((total + int64(pageSize) - 1) / int64(pageSize))
	}

	return &PaginatedFormSubmissionsResponse{
		Page:       page,
		PageSize:   pageSize,
		TotalItems: total,
		TotalPages: totalPages,
		Items:      respItems,
	}, nil
}

func (s *FormSubmissionService) GetFormsByFileID(fileID int64) ([]FormFileMappingResponse, error) {
	if fileID <= 0 {
		return nil, errors.New("file_id is required")
	}

	var rows []FormFileMapping
	if err := s.DB.
		Where("file_id = ?", fileID).
		Order("id asc").
		Find(&rows).Error; err != nil {
		return nil, err
	}

	resp := make([]FormFileMappingResponse, 0, len(rows))
	for _, row := range rows {
		resp = append(resp, FormFileMappingResponse{
			ID:       row.ID,
			FileName: row.FileName,
			FileID:   row.FileID,
			FormKey:  row.FormKey,
			FormName: row.FormName,
		})
	}

	return resp, nil
}

func (s *FormSubmissionService) ReviewSubmission(req *ReviewFormSubmissionRequest, reviewerID int) (*GetFormSubmissionResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("%w: request is required", ErrInvalidReviewRequest)
	}
	if req.SubmissionID <= 0 {
		return nil, fmt.Errorf("%w: submission_id is required", ErrInvalidReviewRequest)
	}
	if req.SubmissionReview == nil && len(req.UploadReviews) == 0 {
		return nil, fmt.Errorf("%w: either submission_review or upload_reviews is required", ErrInvalidReviewRequest)
	}

	if req.SubmissionReview != nil {
		req.SubmissionReview.Status = normalizeReviewStatus(req.SubmissionReview.Status)
		if !isValidReviewStatus(req.SubmissionReview.Status) {
			return nil, fmt.Errorf("%w: invalid submission_review.status", ErrInvalidReviewRequest)
		}
	}

	seenUploadIDs := make(map[int64]struct{}, len(req.UploadReviews))
	for i := range req.UploadReviews {
		req.UploadReviews[i].Status = normalizeReviewStatus(req.UploadReviews[i].Status)

		if req.UploadReviews[i].UploadID <= 0 {
			return nil, fmt.Errorf("%w: upload_id is required", ErrInvalidReviewRequest)
		}
		if !isValidReviewStatus(req.UploadReviews[i].Status) {
			return nil, fmt.Errorf("%w: invalid upload_reviews.status for upload_id %d", ErrInvalidReviewRequest, req.UploadReviews[i].UploadID)
		}
		if _, exists := seenUploadIDs[req.UploadReviews[i].UploadID]; exists {
			return nil, fmt.Errorf("%w: duplicate upload_id %d", ErrInvalidReviewRequest, req.UploadReviews[i].UploadID)
		}
		seenUploadIDs[req.UploadReviews[i].UploadID] = struct{}{}
	}

	var sub FormSubmission
	now := time.Now().UTC()
	shouldTriggerEmail := false

	err := s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.
			Preload("CreatedByUser").
			Where("id = ?", req.SubmissionID).
			First(&sub).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrFormSubmissionNotFound
			}
			return err
		}

		if req.SubmissionReview != nil {
			submissionUpdates := buildReviewUpdateMap(
				req.SubmissionReview.Status,
				req.SubmissionReview.ReviewerComment,
				reviewerID,
				now,
			)

			submissionUpdates["review_email_trigger_success"] = false

			if err := tx.Model(&FormSubmission{}).
				Where("id = ?", sub.ID).
				Updates(submissionUpdates).Error; err != nil {
				return err
			}

			shouldTriggerEmail = true
		}

		if len(req.UploadReviews) > 0 {
			uploadIDs := make([]int64, 0, len(req.UploadReviews))
			for _, item := range req.UploadReviews {
				uploadIDs = append(uploadIDs, item.UploadID)
			}

			var uploads []FormSubmissionUpload
			if err := tx.
				Where("submission_id = ? AND id IN ?", sub.ID, uploadIDs).
				Find(&uploads).Error; err != nil {
				return err
			}

			if len(uploads) != len(uploadIDs) {
				return ErrUploadNotFoundForSubmission
			}

			for _, item := range req.UploadReviews {
				uploadUpdates := buildReviewUpdateMap(
					item.Status,
					item.ReviewerComment,
					reviewerID,
					now,
				)

				if err := tx.Model(&FormSubmissionUpload{}).
					Where("id = ? AND submission_id = ?", item.UploadID, sub.ID).
					Updates(uploadUpdates).Error; err != nil {
					return err
				}
			}

			shouldTriggerEmail = true
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	if shouldTriggerEmail {
		sub.Status = req.SubmissionReview.Status
		sub.ReviewerComment = req.SubmissionReview.ReviewerComment
		s.triggerReviewEmailAsync(&sub)
	}

	return s.getSubmissionResponseByID(sub.ID)
}

func (s *FormSubmissionService) SearchMySubmissions(
	ctx context.Context,
	userID int,
	req SearchFormSubmissionsRequest,
	page int,
	pageSize int,
) (*PaginatedFormSubmissionsResponse, error) {
	if userID <= 0 {
		return nil, errors.New("valid user_id is required")
	}

	q := s.DB.WithContext(ctx).
		Model(&FormSubmission{}).
		Where("(created_by = ? OR edited_by = ?)", userID, userID)

	if req.FileID != nil && *req.FileID > 0 {
		q = q.Where("file_id = ?", *req.FileID)
	}

	if req.FormKey != nil {
		key := strings.TrimSpace(*req.FormKey)
		if key != "" {
			q = q.Where("form_key = ?", key)
		}
	}

	if req.FirstName != nil {
		v := strings.TrimSpace(*req.FirstName)
		if v != "" {
			q = q.Where("firstname ILIKE ?", "%"+v+"%")
		}
	}

	if req.LastName != nil {
		v := strings.TrimSpace(*req.LastName)
		if v != "" {
			q = q.Where("lastname ILIKE ?", "%"+v+"%")
		}
	}

	if req.ConsentGiven != nil {
		q = q.Where("consent_given = ?", *req.ConsentGiven)
	}

	if len(req.Status) > 0 {
		q = q.Where("status IN ?", req.Status)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, err
	}

	offset := (page - 1) * pageSize

	preloadUser := func(db *gorm.DB) *gorm.DB {
		return db.Select("id", "email")
	}

	var items []FormSubmission
	if err := q.
		Preload("CreatedByUser", preloadUser).
		Preload("EditedByUser", preloadUser).
		Preload("ReviewedByUser", preloadUser).
		Order("updated_at desc").
		Order("id desc").
		Limit(pageSize).
		Offset(offset).
		Find(&items).Error; err != nil {
		return nil, err
	}

	respItems := make([]FormSubmissionListItemResponse, 0, len(items))
	for _, item := range items {
		respItems = append(respItems, FormSubmissionListItemResponse{
			ID:           item.ID,
			FileID:       item.FileID,
			RowID:        item.RowID,
			FileName:     item.FileName,
			FormKey:      item.FormKey,
			FormLabel:    item.FormLabel,
			ConsentText:  item.ConsentText,
			ConsentGiven: item.ConsentGiven,
			CreatedAt:    item.CreatedAt,
			UpdatedAt:    item.UpdatedAt,
			FirstName:    item.FirstName,
			LastName:     item.LastName,
			CreatedBy:    userEmail(item.CreatedByUser),
			EditedBy:     userEmail(item.EditedByUser),
			ReviewedBy:   userEmail(item.ReviewedByUser),

			Status:          item.Status,
			ReviewerComment: item.ReviewerComment,
			ReviewedAt:      item.ReviewedAt,

			ReviewEmailTriggerSuccess: item.ReviewEmailTriggerSuccess,
		})
	}

	totalPages := 0
	if total > 0 {
		totalPages = int((total + int64(pageSize) - 1) / int64(pageSize))
	}

	return &PaginatedFormSubmissionsResponse{
		Page:       page,
		PageSize:   pageSize,
		TotalItems: total,
		TotalPages: totalPages,
		Items:      respItems,
	}, nil
}
