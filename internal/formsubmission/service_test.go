package formsubmission

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	filehelper "nordik-drive-api/internal/file"
	"nordik-drive-api/internal/mailer"
)

func TestParseRequiredInt64Query(t *testing.T) {
	v, err := parseRequiredInt64Query(" 42 ")
	if err != nil || v != 42 {
		t.Fatalf("expected 42, got %d err=%v", v, err)
	}

	if _, err := parseRequiredInt64Query("x"); err == nil {
		t.Fatalf("expected error")
	}
}

func TestUserEmail(t *testing.T) {
	if got := userEmail(nil); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
	if got := userEmail(&FormSubmissionUserRef{Email: "a@b.com", FirstName: "Athul", LastName: "N"}); got != "a@b.com" {
		t.Fatalf("unexpected email %q", got)
	}
}

func TestNormalizeReviewStatus(t *testing.T) {
	if got := normalizeReviewStatus("  APPROVED "); got != "approved" {
		t.Fatalf("unexpected status %q", got)
	}
}

func TestIsValidReviewStatus(t *testing.T) {
	if !isValidReviewStatus(" pending ") {
		t.Fatalf("expected pending valid")
	}
	if isValidReviewStatus("unknown") {
		t.Fatalf("expected unknown invalid")
	}
}

func TestBuildReviewUpdateMap(t *testing.T) {
	now := fixedTime()

	m := buildReviewUpdateMap("pending", " keep ", 99, now)
	if m["status"] != "pending" || m["reviewer_comment"] != "" || m["reviewed_by"] != nil || m["reviewed_at"] != nil {
		t.Fatalf("unexpected pending map: %#v", m)
	}

	m = buildReviewUpdateMap(" approved ", " ok ", 99, now)
	if m["status"] != "approved" {
		t.Fatalf("unexpected status: %#v", m["status"])
	}
	if m["reviewer_comment"] != "ok" {
		t.Fatalf("unexpected reviewer comment: %#v", m["reviewer_comment"])
	}
	if m["reviewed_by"] != 99 {
		t.Fatalf("unexpected reviewed_by: %#v", m["reviewed_by"])
	}
	if _, ok := m["reviewed_at"].(time.Time); !ok {
		t.Fatalf("expected reviewed_at time.Time")
	}
}

func TestParseFormUploadGSURL(t *testing.T) {
	bucket, objectPath, err := parseFormUploadGSURL("gs://my-bucket/a/b/c.pdf")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if bucket != "my-bucket" || objectPath != "a/b/c.pdf" {
		t.Fatalf("unexpected parse: %q %q", bucket, objectPath)
	}

	cases := []string{"", "http://x", "gs://bucket", "gs:///x", "gs://bucket/"}
	for _, in := range cases {
		if _, _, err := parseFormUploadGSURL(in); err == nil {
			t.Fatalf("expected error for %q", in)
		}
	}
}

func TestTriggerReviewEmailAsync(t *testing.T) {
	svc := newTestService(t)

	oldGo := formSubmissionGoHook
	oldTrigger := triggerFormSubmissionReviewEmailHook
	defer func() {
		formSubmissionGoHook = oldGo
		triggerFormSubmissionReviewEmailHook = oldTrigger
	}()

	formSubmissionGoHook = func(fn func()) { fn() }

	sub, _, _ := seedSubmissionWithDetailAndUpload(t, svc)

	t.Run("success updates flag", func(t *testing.T) {
		triggerFormSubmissionReviewEmailHook = func(sub *FormSubmission, mailer mailer.EmailSender) error { return nil }

		if err := svc.DB.Model(&FormSubmission{}).Where("id = ?", sub.ID).Update("review_email_trigger_success", false).Error; err != nil {
			t.Fatalf("reset flag: %v", err)
		}

		svc.triggerReviewEmailAsync(&sub)

		var got FormSubmission
		if err := svc.DB.First(&got, sub.ID).Error; err != nil {
			t.Fatalf("load submission: %v", err)
		}
		if !got.ReviewEmailTriggerSuccess {
			t.Fatalf("expected flag true")
		}
	})

	t.Run("hook error keeps false", func(t *testing.T) {
		triggerFormSubmissionReviewEmailHook = func(sub *FormSubmission, mailer mailer.EmailSender) error { return errors.New("nope") }

		if err := svc.DB.Model(&FormSubmission{}).Where("id = ?", sub.ID).Update("review_email_trigger_success", false).Error; err != nil {
			t.Fatalf("reset flag: %v", err)
		}

		svc.triggerReviewEmailAsync(&sub)

		var got FormSubmission
		if err := svc.DB.First(&got, sub.ID).Error; err != nil {
			t.Fatalf("load submission: %v", err)
		}
		if got.ReviewEmailTriggerSuccess {
			t.Fatalf("expected flag false")
		}
	})

	t.Run("panic keeps false", func(t *testing.T) {
		triggerFormSubmissionReviewEmailHook = func(sub *FormSubmission, mailer mailer.EmailSender) error {
			panic("boom")
		}

		if err := svc.DB.Model(&FormSubmission{}).Where("id = ?", sub.ID).Update("review_email_trigger_success", false).Error; err != nil {
			t.Fatalf("reset flag: %v", err)
		}

		svc.triggerReviewEmailAsync(&sub)

		var got FormSubmission
		if err := svc.DB.First(&got, sub.ID).Error; err != nil {
			t.Fatalf("load submission: %v", err)
		}
		if got.ReviewEmailTriggerSuccess {
			t.Fatalf("expected flag false")
		}
	})
}

func TestUploadFormFiles(t *testing.T) {
	svc := newTestService(t)

	t.Run("no jobs", func(t *testing.T) {
		docs, photos, err := svc.uploadFormFiles(&SaveFormSubmissionRequest{})
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if len(docs) != 0 || len(photos) != 0 {
			t.Fatalf("expected empty slices")
		}
	})

	t.Run("missing both base64 and url", func(t *testing.T) {
		_, _, err := svc.uploadFormFiles(&SaveFormSubmissionRequest{
			Documents: []FormSubmissionUploadInput{{FileName: "a.pdf"}},
		})
		if err == nil || !strings.Contains(err.Error(), "missing both data_base64 and file_url") {
			t.Fatalf("unexpected err: %v", err)
		}
	})

	t.Run("upload hook error", func(t *testing.T) {
		old := uploadBase64ToGCSHook
		defer func() { uploadBase64ToGCSHook = old }()

		uploadBase64ToGCSHook = func(_ context.Context, dataBase64, bucket, objectName string) (string, int64, error) {
			return "", 0, errors.New("upload failed")
		}

		_, _, err := svc.uploadFormFiles(&SaveFormSubmissionRequest{
			FileID:  10,
			RowID:   20,
			FormKey: "boarding",
			Documents: []FormSubmissionUploadInput{{
				DetailKey:  "passport",
				FileName:   "a.pdf",
				MimeType:   "application/pdf",
				DataBase64: "ZmFrZQ==",
			}},
		})
		if err == nil || !strings.Contains(err.Error(), "failed to upload document") {
			t.Fatalf("unexpected err: %v", err)
		}
	})

	t.Run("success and boarding alias normalization", func(t *testing.T) {
		old := uploadBase64ToGCSHook
		defer func() { uploadBase64ToGCSHook = old }()

		var gotObject string

		uploadBase64ToGCSHook = func(_ context.Context, dataBase64, bucket, objectName string) (string, int64, error) {
			gotObject = objectName
			return "gs://" + bucket + "/" + objectName, 123, nil
		}

		docs, photos, err := svc.uploadFormFiles(&SaveFormSubmissionRequest{
			FileID:  10,
			RowID:   20,
			FormKey: "boarding_tab",
			Documents: []FormSubmissionUploadInput{{
				DetailKey:  "passport",
				FileName:   "resume.pdf",
				MimeType:   "application/pdf",
				DataBase64: "ZmFrZQ==",
			}},
			Photos: []FormSubmissionUploadInput{{
				DetailKey: "photo",
				FileName:  "selfie.jpg",
				FileURL:   "gs://existing/selfie.jpg",
			}},
		})
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !strings.Contains(gotObject, "requests/boarding_home/10_20/document_") {
			t.Fatalf("unexpected object: %q", gotObject)
		}
		if docs[0].FileURL == "" || docs[0].FileSizeBytes != 123 {
			t.Fatalf("unexpected doc: %+v", docs[0])
		}
		if photos[0].FileURL != "gs://existing/selfie.jpg" {
			t.Fatalf("unexpected photo: %+v", photos[0])
		}
	})
}

func TestUpsert_ValidationErrors(t *testing.T) {
	svc := newTestService(t)

	cases := []struct {
		name string
		req  *SaveFormSubmissionRequest
		want string
	}{
		{"nil request", nil, "request is required"},
		{"missing file_id", &SaveFormSubmissionRequest{RowID: 1, FormKey: "k", FormLabel: "l"}, "file_id is required"},
		{"missing row_id", &SaveFormSubmissionRequest{FileID: 1, FormKey: "k", FormLabel: "l"}, "row_id is required"},
		{"missing form_key", &SaveFormSubmissionRequest{FileID: 1, RowID: 1, FormLabel: "l"}, "form_key is required"},
		{"missing form_label", &SaveFormSubmissionRequest{FileID: 1, RowID: 1, FormKey: "k"}, "form_label is required"},
		{"blank detail key", &SaveFormSubmissionRequest{
			FileID: 1, RowID: 1, FormKey: "k", FormLabel: "l",
			Details: []FormSubmissionDetailInput{{DetailKey: ""}},
		}, "detail_key is required in details"},
		{"blank document detail key", &SaveFormSubmissionRequest{
			FileID: 1, RowID: 1, FormKey: "k", FormLabel: "l",
			Details:   []FormSubmissionDetailInput{{DetailKey: "a"}},
			Documents: []FormSubmissionUploadInput{{DetailKey: ""}},
		}, "detail_key is required in documents"},
		{"document detail missing", &SaveFormSubmissionRequest{
			FileID: 1, RowID: 1, FormKey: "k", FormLabel: "l",
			Details:   []FormSubmissionDetailInput{{DetailKey: "a"}},
			Documents: []FormSubmissionUploadInput{{DetailKey: "b"}},
		}, "document detail_key not found in details"},
		{"blank photo detail key", &SaveFormSubmissionRequest{
			FileID: 1, RowID: 1, FormKey: "k", FormLabel: "l",
			Details: []FormSubmissionDetailInput{{DetailKey: "a"}},
			Photos:  []FormSubmissionUploadInput{{DetailKey: ""}},
		}, "detail_key is required in photos"},
		{"photo detail missing", &SaveFormSubmissionRequest{
			FileID: 1, RowID: 1, FormKey: "k", FormLabel: "l",
			Details: []FormSubmissionDetailInput{{DetailKey: "a"}},
			Photos:  []FormSubmissionUploadInput{{DetailKey: "b"}},
		}, "photo detail_key not found in details"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Upsert(tc.req, 1)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestUpsert_BlockedDuringVersionTransition(t *testing.T) {
	svc := newTestService(t)
	if err := svc.DB.AutoMigrate(&filehelper.FileVersion{}); err != nil {
		t.Fatalf("migrate file version: %v", err)
	}
	if err := svc.DB.Create(&filehelper.FileVersion{
		FileID:               10,
		Filename:             "people.csv",
		Version:              2,
		ReconciliationStatus: "processing",
	}).Error; err != nil {
		t.Fatalf("seed pending version: %v", err)
	}

	_, err := svc.Upsert(&SaveFormSubmissionRequest{
		FileID:    10,
		RowID:     20,
		FileName:  "people.csv",
		FormKey:   "boarding",
		FormLabel: "Boarding",
	}, 1)
	if !errors.Is(err, filehelper.ErrFileVersionTransitionInProgress) {
		t.Fatalf("expected transition conflict, got %v", err)
	}
}

func TestUpsert_JsonMarshalError(t *testing.T) {
	svc := newTestService(t)

	_, err := svc.Upsert(&SaveFormSubmissionRequest{
		FileID:    1,
		RowID:     1,
		FormKey:   "k",
		FormLabel: "l",
		Details: []FormSubmissionDetailInput{
			{DetailKey: "bad", DetailLabel: "Bad", FieldType: "custom", Value: make(chan int)},
		},
	}, 1)
	if err == nil || !strings.Contains(err.Error(), "failed to marshal value") {
		t.Fatalf("unexpected err: %v", err)
	}
}

func TestUpsert_CreateAndUpdate(t *testing.T) {
	svc := newTestService(t)

	req1 := &SaveFormSubmissionRequest{
		FileID:      10,
		RowID:       20,
		FileName:    "sheet.xlsx",
		FormKey:     "boarding",
		FormLabel:   "Boarding",
		ConsentText: "consent",
		Consent:     true,
		FirstName:   "Athul",
		LastName:    "Narayanan",
		Details: []FormSubmissionDetailInput{
			{DetailKey: "passport_no", DetailLabel: "Passport", FieldType: "text", Value: "A12345"},
			{DetailKey: "consent_box", DetailLabel: "Consent", FieldType: "checkbox", ConsentRequired: true, Value: true},
		},
		Documents: []FormSubmissionUploadInput{
			{DetailKey: "passport_no", FileName: "p1.pdf", MimeType: "application/pdf", FileURL: "gs://bucket/p1.pdf", FileSizeBytes: 100},
		},
		Photos: []FormSubmissionUploadInput{
			{DetailKey: "consent_box", FileName: "s1.jpg", MimeType: "image/jpeg", FileURL: "gs://bucket/s1.jpg", FileSizeBytes: 200},
		},
	}

	resp1, err := svc.Upsert(req1, 7)
	if err != nil {
		t.Fatalf("upsert create err: %v", err)
	}
	if !resp1.Found || resp1.CreatedBy != "user7@example.com" {
		t.Fatalf("unexpected resp1: %+v", resp1)
	}
	if len(resp1.Details) != 2 || len(resp1.Documents) != 1 || len(resp1.Photos) != 1 {
		t.Fatalf("unexpected counts in resp1")
	}

	var sub FormSubmission
	if err := svc.DB.Where("file_id = ? AND row_id = ? AND form_key = ?", 10, 20, "boarding").First(&sub).Error; err != nil {
		t.Fatalf("find sub: %v", err)
	}

	var detail FormSubmissionDetail
	if err := svc.DB.Where("submission_id = ? AND detail_key = ?", sub.ID, "passport_no").First(&detail).Error; err != nil {
		t.Fatalf("find detail: %v", err)
	}
	originalDetailID := detail.ID

	req2 := &SaveFormSubmissionRequest{
		FileID:    10,
		RowID:     20,
		FileName:  "sheet2.xlsx",
		FormKey:   "boarding",
		FormLabel: "Boarding Updated",
		FirstName: "Athul",
		LastName:  "N",
		Details: []FormSubmissionDetailInput{
			{DetailKey: "passport_no", DetailLabel: "Passport Updated", FieldType: "text", Value: "B99999"},
			{DetailKey: "consent_box", DetailLabel: "Consent", FieldType: "checkbox", ConsentRequired: true, Value: false},
		},
		Documents: []FormSubmissionUploadInput{
			{DetailKey: "passport_no", FileName: "p2.pdf", MimeType: "application/pdf", FileURL: "gs://bucket/p2.pdf", FileSizeBytes: 300},
		},
	}

	resp2, err := svc.Upsert(req2, 8)
	if err != nil {
		t.Fatalf("upsert update err: %v", err)
	}
	if resp2.EditedBy != "user8@example.com" {
		t.Fatalf("unexpected resp2 edited by: %+v", resp2)
	}
	if len(resp2.Documents) != 2 {
		t.Fatalf("expected 2 documents after append, got %d", len(resp2.Documents))
	}

	if err := svc.DB.First(&sub, sub.ID).Error; err != nil {
		t.Fatalf("reload sub: %v", err)
	}
	if sub.CreatedByID == nil || *sub.CreatedByID != 7 {
		t.Fatalf("expected created_by 7, got %#v", sub.CreatedByID)
	}
	if sub.EditedByID == nil || *sub.EditedByID != 8 {
		t.Fatalf("expected edited_by 8, got %#v", sub.EditedByID)
	}

	if err := svc.DB.Where("submission_id = ? AND detail_key = ?", sub.ID, "passport_no").First(&detail).Error; err != nil {
		t.Fatalf("reload detail: %v", err)
	}
	if detail.ID != originalDetailID {
		t.Fatalf("expected detail id preserved")
	}
}

func TestUpsert_CreatesNewSubmissionWhenOnlyRejectedExists(t *testing.T) {
	svc := newTestService(t)

	createdBy := 7
	rejected := FormSubmission{
		FileID:      10,
		RowID:       20,
		FileName:    "sheet.xlsx",
		FormKey:     "boarding",
		FormLabel:   "Boarding",
		CreatedByID: &createdBy,
		Status:      ReviewStatusRejected,
	}
	if err := svc.DB.Create(&rejected).Error; err != nil {
		t.Fatalf("create rejected submission: %v", err)
	}

	oldDetail := FormSubmissionDetail{
		SubmissionID: rejected.ID,
		DetailKey:    "old_passport",
		DetailLabel:  "Old Passport",
		FieldType:    "text",
		ValueJSON:    []byte(`"OLD-123"`),
	}
	if err := svc.DB.Create(&oldDetail).Error; err != nil {
		t.Fatalf("create old detail: %v", err)
	}

	resp, err := svc.Upsert(&SaveFormSubmissionRequest{
		FileID:    10,
		RowID:     20,
		FileName:  "sheet.xlsx",
		FormKey:   "boarding",
		FormLabel: "Boarding",
		FirstName: "New",
		LastName:  "Request",
		Details: []FormSubmissionDetailInput{
			{DetailKey: "passport_no", DetailLabel: "Passport", FieldType: "text", Value: "NEW-999"},
		},
	}, 8)
	if err != nil {
		t.Fatalf("upsert retry after rejection err: %v", err)
	}

	if resp.ID == rejected.ID {
		t.Fatalf("expected a new submission, got rejected submission id %d", resp.ID)
	}
	if resp.Status != ReviewStatusPending {
		t.Fatalf("expected new submission to be pending, got %q", resp.Status)
	}
	if len(resp.Details) != 1 || resp.Details[0].DetailKey != "passport_no" {
		t.Fatalf("unexpected response details: %+v", resp.Details)
	}

	var subs []FormSubmission
	if err := svc.DB.
		Where("file_id = ? AND row_id = ? AND form_key = ?", 10, 20, "boarding").
		Order("id asc").
		Find(&subs).Error; err != nil {
		t.Fatalf("list submissions: %v", err)
	}
	if len(subs) != 2 {
		t.Fatalf("expected 2 submissions, got %d", len(subs))
	}
	if subs[0].ID != rejected.ID || subs[0].Status != ReviewStatusRejected {
		t.Fatalf("expected original submission to remain rejected, got %+v", subs[0])
	}
	if subs[1].ID != resp.ID || subs[1].Status != ReviewStatusPending {
		t.Fatalf("expected second submission to be the new pending one, got %+v", subs[1])
	}

	var oldDetails []FormSubmissionDetail
	if err := svc.DB.Where("submission_id = ?", rejected.ID).Find(&oldDetails).Error; err != nil {
		t.Fatalf("load old details: %v", err)
	}
	if len(oldDetails) != 1 || oldDetails[0].DetailKey != "old_passport" {
		t.Fatalf("expected rejected submission details to stay unchanged, got %+v", oldDetails)
	}

	var newDetails []FormSubmissionDetail
	if err := svc.DB.Where("submission_id = ?", resp.ID).Find(&newDetails).Error; err != nil {
		t.Fatalf("load new details: %v", err)
	}
	if len(newDetails) != 1 || newDetails[0].DetailKey != "passport_no" {
		t.Fatalf("expected new submission details on new row, got %+v", newDetails)
	}
}

func TestFormSubmissionUniqueIndex_AllowsRetryAfterRejectedOnly(t *testing.T) {
	svc := newTestService(t)

	createdBy := 1
	base := FormSubmission{
		FileID:      44,
		RowID:       55,
		FileName:    "sheet.xlsx",
		FormKey:     "boarding",
		FormLabel:   "Boarding",
		CreatedByID: &createdBy,
		Status:      ReviewStatusRejected,
	}

	if err := svc.DB.Create(&base).Error; err != nil {
		t.Fatalf("create first rejected submission: %v", err)
	}

	anotherRejected := FormSubmission{
		FileID:      44,
		RowID:       55,
		FileName:    "sheet.xlsx",
		FormKey:     "boarding",
		FormLabel:   "Boarding",
		CreatedByID: &createdBy,
		Status:      ReviewStatusRejected,
	}

	if err := svc.DB.Create(&anotherRejected).Error; err != nil {
		t.Fatalf("expected another rejected submission to be allowed, got: %v", err)
	}

	retryPending := FormSubmission{
		FileID:      44,
		RowID:       55,
		FileName:    "sheet.xlsx",
		FormKey:     "boarding",
		FormLabel:   "Boarding",
		CreatedByID: &createdBy,
		Status:      ReviewStatusPending,
	}

	if err := svc.DB.Create(&retryPending).Error; err != nil {
		t.Fatalf("expected pending retry after only rejected submissions, got: %v", err)
	}

	blockedPending := FormSubmission{
		FileID:      44,
		RowID:       55,
		FileName:    "sheet.xlsx",
		FormKey:     "boarding",
		FormLabel:   "Boarding",
		CreatedByID: &createdBy,
		Status:      ReviewStatusPending,
	}

	if err := svc.DB.Create(&blockedPending).Error; err == nil {
		t.Fatalf("expected second active submission for same file_id/row_id/form_key to be blocked")
	}
}

func TestGetByRowAndForm(t *testing.T) {
	svc := newTestService(t)

	t.Run("validation", func(t *testing.T) {
		if _, err := svc.GetByRowAndForm(0, "x", nil); err == nil {
			t.Fatalf("expected row validation error")
		}
		if _, err := svc.GetByRowAndForm(1, " ", nil); err == nil {
			t.Fatalf("expected form key validation error")
		}
	})

	t.Run("not found", func(t *testing.T) {
		resp, err := svc.GetByRowAndForm(999, "missing", int64Ptr(55))
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if resp.Found || resp.FileID != 55 || resp.RowID != 999 || resp.FormKey != "missing" {
			t.Fatalf("unexpected resp: %+v", resp)
		}
		if len(resp.Details) != 0 || len(resp.Documents) != 0 || len(resp.Photos) != 0 {
			t.Fatalf("expected empty slices")
		}
	})

	t.Run("bad detail json", func(t *testing.T) {
		createdBy := 1
		sub := FormSubmission{
			FileID:      1,
			RowID:       2,
			FormKey:     "f1",
			FormLabel:   "F1",
			CreatedByID: &createdBy,
		}
		if err := svc.DB.Create(&sub).Error; err != nil {
			t.Fatalf("create sub: %v", err)
		}
		bad := FormSubmissionDetail{
			SubmissionID: sub.ID,
			DetailKey:    "k1",
			DetailLabel:  "K1",
			FieldType:    "text",
			ValueJSON:    []byte("{"),
		}
		if err := svc.DB.Create(&bad).Error; err != nil {
			t.Fatalf("create bad detail: %v", err)
		}

		_, err := svc.GetByRowAndForm(2, "f1", nil)
		if err == nil {
			t.Fatalf("expected json unmarshal error")
		}
	})

	t.Run("success", func(t *testing.T) {
		req := &SaveFormSubmissionRequest{
			FileID:    100,
			RowID:     200,
			FileName:  "sheet.xlsx",
			FormKey:   "boarding",
			FormLabel: "Boarding",
			Details: []FormSubmissionDetailInput{
				{DetailKey: "passport_no", DetailLabel: "Passport", FieldType: "text", Value: "A123"},
				{DetailKey: "consent_box", DetailLabel: "Consent", FieldType: "checkbox", Value: true},
			},
			Documents: []FormSubmissionUploadInput{
				{DetailKey: "passport_no", FileName: "doc.pdf", MimeType: "application/pdf", FileURL: "gs://bucket/doc.pdf", FileSizeBytes: 10},
			},
			Photos: []FormSubmissionUploadInput{
				{DetailKey: "consent_box", FileName: "pic.jpg", MimeType: "image/jpeg", FileURL: "gs://bucket/pic.jpg", FileSizeBytes: 20},
			},
		}
		if _, err := svc.Upsert(req, 7); err != nil {
			t.Fatalf("seed via upsert: %v", err)
		}

		resp, err := svc.GetByRowAndForm(200, "boarding", int64Ptr(100))
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !resp.Found || resp.FileID != 100 || resp.RowID != 200 {
			t.Fatalf("unexpected resp: %+v", resp)
		}
		if len(resp.Details) != 2 || len(resp.Documents) != 1 || len(resp.Photos) != 1 {
			t.Fatalf("unexpected collections")
		}
	})
}

func TestGetByID(t *testing.T) {
	svc := newTestService(t)

	t.Run("validation", func(t *testing.T) {
		if _, err := svc.GetByID(0); err == nil {
			t.Fatalf("expected id validation error")
		}
	})

	t.Run("not found", func(t *testing.T) {
		resp, err := svc.GetByID(999)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if resp.Found || resp.ID != 999 {
			t.Fatalf("unexpected resp: %+v", resp)
		}
		if len(resp.Details) != 0 || len(resp.Documents) != 0 || len(resp.Photos) != 0 {
			t.Fatalf("expected empty slices")
		}
	})

	t.Run("success", func(t *testing.T) {
		sub, _, _ := seedSubmissionWithDetailAndUpload(t, svc)

		resp, err := svc.GetByID(sub.ID)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !resp.Found || resp.ID != sub.ID {
			t.Fatalf("unexpected resp: %+v", resp)
		}
		if resp.FileID != sub.FileID || resp.RowID != sub.RowID || resp.FormKey != sub.FormKey {
			t.Fatalf("unexpected submission fields: %+v", resp)
		}
		if len(resp.Details) != 1 || len(resp.Documents) != 1 || len(resp.Photos) != 0 {
			t.Fatalf("unexpected collections: %+v", resp)
		}
	})
}

func TestGetActiveByRowAndForm(t *testing.T) {
	svc := newTestService(t)

	t.Run("validation", func(t *testing.T) {
		if _, err := svc.GetActiveByRowAndForm(0, "x", nil); err == nil {
			t.Fatalf("expected row validation error")
		}
		if _, err := svc.GetActiveByRowAndForm(1, " ", nil); err == nil {
			t.Fatalf("expected form key validation error")
		}
	})

	t.Run("not found when only rejected exists", func(t *testing.T) {
		createdBy := 1
		sub := FormSubmission{
			FileID:      49,
			RowID:       73992,
			FileName:    "sheet.xlsx",
			FormKey:     "boarding_home",
			FormLabel:   "Boarding",
			CreatedByID: &createdBy,
			Status:      ReviewStatusRejected,
		}
		if err := svc.DB.Create(&sub).Error; err != nil {
			t.Fatalf("create rejected submission: %v", err)
		}

		resp, err := svc.GetActiveByRowAndForm(73992, "boarding_home", int64Ptr(49))
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if resp != nil {
			t.Fatalf("expected nil when only rejected submission exists, got %+v", resp)
		}
	})

	t.Run("not found when nothing exists", func(t *testing.T) {
		resp, err := svc.GetActiveByRowAndForm(12345, "missing_form", int64Ptr(49))
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if resp != nil {
			t.Fatalf("expected nil when nothing exists, got %+v", resp)
		}
	})

	t.Run("success returns non-rejected submission", func(t *testing.T) {
		createdBy := 1
		rejected := FormSubmission{
			FileID:      49,
			RowID:       73992,
			FileName:    "sheet.xlsx",
			FormKey:     "boarding_home",
			FormLabel:   "Boarding",
			CreatedByID: &createdBy,
			Status:      ReviewStatusRejected,
		}
		if err := svc.DB.Create(&rejected).Error; err != nil {
			t.Fatalf("create rejected submission: %v", err)
		}

		active := FormSubmission{
			FileID:      49,
			RowID:       73992,
			FileName:    "sheet.xlsx",
			FormKey:     "boarding_home",
			FormLabel:   "Boarding",
			CreatedByID: &createdBy,
			Status:      ReviewStatusPending,
		}
		if err := svc.DB.Create(&active).Error; err != nil {
			t.Fatalf("create active submission: %v", err)
		}

		detail := FormSubmissionDetail{
			SubmissionID: active.ID,
			DetailKey:    "passport_no",
			DetailLabel:  "Passport",
			FieldType:    "text",
			ValueJSON:    []byte(`"A12345"`),
		}
		if err := svc.DB.Create(&detail).Error; err != nil {
			t.Fatalf("create detail: %v", err)
		}

		resp, err := svc.GetActiveByRowAndForm(73992, "boarding_home", int64Ptr(49))
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !resp.Found || resp.ID != active.ID {
			t.Fatalf("expected active submission, got %+v", resp)
		}
		if resp.Status != ReviewStatusPending {
			t.Fatalf("expected pending status, got %q", resp.Status)
		}
		if len(resp.Details) != 1 || resp.Details[0].DetailKey != "passport_no" {
			t.Fatalf("unexpected details: %+v", resp.Details)
		}
	})
}

func TestGetUploadBytes(t *testing.T) {
	svc := newTestService(t)

	t.Run("record not found", func(t *testing.T) {
		_, _, _, err := svc.GetUploadBytes(999)
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatalf("expected record not found, got %v", err)
		}
	})

	old := openFormUploadReaderHook
	defer func() { openFormUploadReaderHook = old }()

	upload := FormSubmissionUpload{
		SubmissionID: 1,
		DetailID:     1,
		UploadType:   "document",
		FileName:     "",
		MimeType:     "",
		FileURL:      "gs://bucket/path/fallback.bin",
	}
	if err := svc.DB.Create(&upload).Error; err != nil {
		t.Fatalf("create upload: %v", err)
	}

	t.Run("open reader error", func(t *testing.T) {
		openFormUploadReaderHook = func(ctx context.Context, rec FormSubmissionUpload) (io.ReadCloser, string, string, error) {
			return nil, "", "", errors.New("open failed")
		}
		_, _, _, err := svc.GetUploadBytes(uint(upload.ID))
		if err == nil || !strings.Contains(err.Error(), "open failed") {
			t.Fatalf("unexpected err: %v", err)
		}
	})

	t.Run("read error", func(t *testing.T) {
		openFormUploadReaderHook = func(ctx context.Context, rec FormSubmissionUpload) (io.ReadCloser, string, string, error) {
			return &errReadCloser{readErr: errors.New("read failed")}, "", "path/fallback.bin", nil
		}
		_, _, _, err := svc.GetUploadBytes(uint(upload.ID))
		if err == nil || !strings.Contains(err.Error(), "read failed") {
			t.Fatalf("unexpected err: %v", err)
		}
	})

	t.Run("success uses rc content type", func(t *testing.T) {
		openFormUploadReaderHook = func(ctx context.Context, rec FormSubmissionUpload) (io.ReadCloser, string, string, error) {
			return &staticReadCloser{data: []byte("hello")}, "application/pdf", "path/file.pdf", nil
		}
		data, contentType, filename, err := svc.GetUploadBytes(uint(upload.ID))
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if string(data) != "hello" || contentType != "application/pdf" || filename != "file.pdf" {
			t.Fatalf("unexpected values: %q %q %q", string(data), contentType, filename)
		}
	})

	t.Run("success falls back to record mime type", func(t *testing.T) {
		if err := svc.DB.Model(&FormSubmissionUpload{}).Where("id = ?", upload.ID).Updates(map[string]interface{}{
			"file_name": "doc.docx",
			"mime_type": "application/msword",
		}).Error; err != nil {
			t.Fatalf("update upload: %v", err)
		}

		openFormUploadReaderHook = func(ctx context.Context, rec FormSubmissionUpload) (io.ReadCloser, string, string, error) {
			return &staticReadCloser{data: []byte("hello")}, "", "path/file.bin", nil
		}
		_, contentType, filename, err := svc.GetUploadBytes(uint(upload.ID))
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if contentType != "application/msword" || filename != "doc.docx" {
			t.Fatalf("unexpected values: %q %q", contentType, filename)
		}
	})

	t.Run("success falls back to detect content type", func(t *testing.T) {
		if err := svc.DB.Model(&FormSubmissionUpload{}).Where("id = ?", upload.ID).Updates(map[string]interface{}{
			"file_name": "",
			"mime_type": "",
		}).Error; err != nil {
			t.Fatalf("update upload: %v", err)
		}

		openFormUploadReaderHook = func(ctx context.Context, rec FormSubmissionUpload) (io.ReadCloser, string, string, error) {
			return &staticReadCloser{data: []byte("hello world")}, "", "path/detected.txt", nil
		}
		_, contentType, filename, err := svc.GetUploadBytes(uint(upload.ID))
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !strings.HasPrefix(contentType, "text/plain") {
			t.Fatalf("unexpected contentType: %q", contentType)
		}
		if filename != "detected.txt" {
			t.Fatalf("unexpected filename: %q", filename)
		}
	})
}

func TestSearchSubmissions(t *testing.T) {
	svc := newTestService(t)

	user1 := 1
	user2 := 2

	items := []FormSubmission{
		{FileID: 10, RowID: 1, FileName: "a.xlsx", FormKey: "boarding", FormLabel: "Boarding", CreatedByID: &user1, ConsentGiven: true, Status: "approved"},
		{FileID: 10, RowID: 2, FileName: "b.xlsx", FormKey: "visa", FormLabel: "Visa", CreatedByID: &user2, ConsentGiven: false, Status: "rejected"},
		{FileID: 11, RowID: 3, FileName: "c.xlsx", FormKey: "boarding", FormLabel: "Boarding", CreatedByID: &user1, ConsentGiven: true, Status: "pending"},
	}
	if err := svc.DB.Create(&items).Error; err != nil {
		t.Fatalf("seed submissions: %v", err)
	}

	t.Run("filters and paginates", func(t *testing.T) {
		resp, err := svc.SearchSubmissions(context.Background(), SearchFormSubmissionsRequest{
			FileID:       int64Ptr(10),
			FormKey:      strPtr("boarding"),
			CreatedBy:    intPtr(1),
			ConsentGiven: boolPtr(true),
			Status:       []string{"approved"},
		}, 1, 10)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if resp.TotalItems != 1 || len(resp.Items) != 1 {
			t.Fatalf("unexpected resp: %+v", resp)
		}
	})

	t.Run("zero results gives zero total pages", func(t *testing.T) {
		resp, err := svc.SearchSubmissions(context.Background(), SearchFormSubmissionsRequest{
			FileID: int64Ptr(999),
		}, 1, 10)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if resp.TotalItems != 0 || resp.TotalPages != 0 || len(resp.Items) != 0 {
			t.Fatalf("unexpected resp: %+v", resp)
		}
	})

	t.Run("first name branch on sqlite", func(t *testing.T) {
		_, err := svc.SearchSubmissions(context.Background(), SearchFormSubmissionsRequest{
			FirstName: strPtr("Athul"),
		}, 1, 10)
		if err == nil {
			t.Fatalf("expected sqlite ILIKE error")
		}
	})

	t.Run("last name branch on sqlite", func(t *testing.T) {
		_, err := svc.SearchSubmissions(context.Background(), SearchFormSubmissionsRequest{
			LastName: strPtr("Narayanan"),
		}, 1, 10)
		if err == nil {
			t.Fatalf("expected sqlite ILIKE error")
		}
	})
}

func TestGetFormsByFileID_Service(t *testing.T) {
	svc := newTestService(t)

	if _, err := svc.GetFormsByFileID(0); err == nil {
		t.Fatalf("expected validation error")
	}

	rows := []FormFileMapping{
		{FileName: "sheet.xlsx", FileID: 55, FormKey: "boarding", FormName: "Boarding"},
		{FileName: "sheet.xlsx", FileID: 55, FormKey: "visa", FormName: "Visa"},
	}
	if err := svc.DB.Create(&rows).Error; err != nil {
		t.Fatalf("seed mappings: %v", err)
	}

	resp, err := svc.GetFormsByFileID(55)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(resp) != 2 || resp[0].FormKey != "boarding" || resp[1].FormKey != "visa" {
		t.Fatalf("unexpected resp: %+v", resp)
	}
}

func TestReviewSubmission(t *testing.T) {
	svc := newTestService(t)

	oldGo := formSubmissionGoHook
	oldTrigger := triggerFormSubmissionReviewEmailHook
	defer func() {
		formSubmissionGoHook = oldGo
		triggerFormSubmissionReviewEmailHook = oldTrigger
	}()

	formSubmissionGoHook = func(fn func()) { fn() }
	triggerFormSubmissionReviewEmailHook = func(sub *FormSubmission, mailer mailer.EmailSender) error { return errors.New("skip success update") }

	t.Run("validation errors", func(t *testing.T) {
		cases := []struct {
			name string
			req  *ReviewFormSubmissionRequest
			want error
		}{
			{"nil req", nil, ErrInvalidReviewRequest},
			{"missing submission id", &ReviewFormSubmissionRequest{}, ErrInvalidReviewRequest},
			{"no review payload", &ReviewFormSubmissionRequest{SubmissionID: 1}, ErrInvalidReviewRequest},
			{"invalid submission status", &ReviewFormSubmissionRequest{
				SubmissionID: 1,
				SubmissionReview: &SubmissionReviewInput{
					Status: "bad",
				},
			}, ErrInvalidReviewRequest},
			{"invalid upload id", &ReviewFormSubmissionRequest{
				SubmissionID:  1,
				UploadReviews: []UploadReviewInput{{UploadID: 0, Status: "approved"}},
			}, ErrInvalidReviewRequest},
			{"invalid upload status", &ReviewFormSubmissionRequest{
				SubmissionID:  1,
				UploadReviews: []UploadReviewInput{{UploadID: 1, Status: "bad"}},
			}, ErrInvalidReviewRequest},
			{"duplicate upload id", &ReviewFormSubmissionRequest{
				SubmissionID: 1,
				UploadReviews: []UploadReviewInput{
					{UploadID: 1, Status: "approved"},
					{UploadID: 1, Status: "rejected"},
				},
			}, ErrInvalidReviewRequest},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := svc.ReviewSubmission(tc.req, 99)
				if !errors.Is(err, tc.want) {
					t.Fatalf("expected %v got %v", tc.want, err)
				}
			})
		}
	})

	t.Run("submission not found", func(t *testing.T) {
		_, err := svc.ReviewSubmission(&ReviewFormSubmissionRequest{
			SubmissionID:     999,
			SubmissionReview: &SubmissionReviewInput{Status: "approved"},
		}, 99)
		if !errors.Is(err, ErrFormSubmissionNotFound) {
			t.Fatalf("expected not found, got %v", err)
		}
	})

	t.Run("upload not found for submission", func(t *testing.T) {
		sub1, _, _ := seedSubmissionWithDetailAndUpload(t, svc)
		sub2, _, up2 := seedSubmissionWithDetailAndUpload(t, svc)

		_, err := svc.ReviewSubmission(&ReviewFormSubmissionRequest{
			SubmissionID: sub1.ID,
			UploadReviews: []UploadReviewInput{
				{UploadID: up2.ID, Status: "approved"},
			},
		}, 99)
		_ = sub2
		if !errors.Is(err, ErrUploadNotFoundForSubmission) {
			t.Fatalf("expected upload not found, got %v", err)
		}
	})

	t.Run("submission review success", func(t *testing.T) {
		sub, _, _ := seedSubmissionWithDetailAndUpload(t, svc)

		resp, err := svc.ReviewSubmission(&ReviewFormSubmissionRequest{
			SubmissionID: sub.ID,
			SubmissionReview: &SubmissionReviewInput{
				Status:          "approved",
				ReviewerComment: "looks good",
			},
		}, 99)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if resp.Status != "approved" || resp.ReviewerComment != "looks good" || resp.ReviewedBy != "reviewer@example.com" {
			t.Fatalf("unexpected resp: %+v", resp)
		}
	})

	t.Run("upload review success", func(t *testing.T) {
		sub, _, up := seedSubmissionWithDetailAndUpload(t, svc)

		resp, err := svc.ReviewSubmission(&ReviewFormSubmissionRequest{
			SubmissionID:     sub.ID,
			SubmissionReview: &SubmissionReviewInput{Status: "approved", ReviewerComment: "ok"},
			UploadReviews: []UploadReviewInput{
				{UploadID: up.ID, Status: "rejected", ReviewerComment: "blurred"},
			},
		}, 99)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if len(resp.Documents) != 1 || resp.Documents[0].Status != "rejected" || resp.Documents[0].ReviewerComment != "blurred" {
			t.Fatalf("unexpected docs: %+v", resp.Documents)
		}
	})

	t.Run("pending clears reviewer fields", func(t *testing.T) {
		sub, _, up := seedSubmissionWithDetailAndUpload(t, svc)

		if _, err := svc.ReviewSubmission(&ReviewFormSubmissionRequest{
			SubmissionID: sub.ID,
			SubmissionReview: &SubmissionReviewInput{
				Status:          "approved",
				ReviewerComment: "ok",
			},
			UploadReviews: []UploadReviewInput{
				{UploadID: up.ID, Status: "approved", ReviewerComment: "ok"},
			},
		}, 99); err != nil {
			t.Fatalf("seed review: %v", err)
		}

		resp, err := svc.ReviewSubmission(&ReviewFormSubmissionRequest{
			SubmissionID: sub.ID,
			SubmissionReview: &SubmissionReviewInput{
				Status:          "pending",
				ReviewerComment: "should clear",
			},
			UploadReviews: []UploadReviewInput{
				{UploadID: up.ID, Status: "pending", ReviewerComment: "should clear"},
			},
		}, 99)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}

		if resp.Status != "pending" || resp.ReviewerComment != "" || resp.ReviewedBy != "" || resp.ReviewedAt != nil {
			t.Fatalf("submission fields not cleared: %+v", resp)
		}
		if resp.Documents[0].Status != "pending" || resp.Documents[0].ReviewerComment != "" || resp.Documents[0].ReviewedBy != "" || resp.Documents[0].ReviewedAt != nil {
			t.Fatalf("upload fields not cleared: %+v", resp.Documents[0])
		}
	})
}

func TestSearchMySubmissions(t *testing.T) {
	svc := newTestService(t)

	if _, err := svc.SearchMySubmissions(context.Background(), 0, SearchFormSubmissionsRequest{}, 1, 10); err == nil {
		t.Fatalf("expected user validation error")
	}

	user5 := 5
	user8 := 8

	rows := []FormSubmission{
		{FileID: 10, RowID: 1, FileName: "a.xlsx", FormKey: "boarding", FormLabel: "Boarding", CreatedByID: &user5, ConsentGiven: true, Status: "pending"},
		{FileID: 10, RowID: 2, FileName: "b.xlsx", FormKey: "visa", FormLabel: "Visa", CreatedByID: &user8, EditedByID: &user5, ConsentGiven: false, Status: "approved"},
		{FileID: 11, RowID: 3, FileName: "c.xlsx", FormKey: "boarding", FormLabel: "Boarding", CreatedByID: &user8, ConsentGiven: true, Status: "rejected"},
	}
	if err := svc.DB.Create(&rows).Error; err != nil {
		t.Fatalf("seed rows: %v", err)
	}

	t.Run("filters and only own or edited", func(t *testing.T) {
		resp, err := svc.SearchMySubmissions(context.Background(), 5, SearchFormSubmissionsRequest{
			Status: []string{"pending", "approved"},
		}, 1, 10)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if resp.TotalItems != 2 || len(resp.Items) != 2 {
			t.Fatalf("unexpected resp: %+v", resp)
		}
		for _, item := range resp.Items {
			if item.RowID == 3 {
				t.Fatalf("unexpected unrelated row")
			}
		}
	})

	t.Run("first name branch on sqlite", func(t *testing.T) {
		_, err := svc.SearchMySubmissions(context.Background(), 5, SearchFormSubmissionsRequest{
			FirstName: strPtr("Athul"),
		}, 1, 10)
		if err == nil {
			t.Fatalf("expected sqlite ILIKE error")
		}
	})

	t.Run("last name branch on sqlite", func(t *testing.T) {
		_, err := svc.SearchMySubmissions(context.Background(), 5, SearchFormSubmissionsRequest{
			LastName: strPtr("Narayanan"),
		}, 1, 10)
		if err == nil {
			t.Fatalf("expected sqlite ILIKE error")
		}
	})
}
