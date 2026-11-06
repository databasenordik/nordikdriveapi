package supportrequest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestBuildSupportRequestNotificationEmailBody(t *testing.T) {
	body := BuildSupportRequestNotificationEmailBody(&SupportRequest{
		RequestType:         RequestTypeTechnicalIssue,
		RequesterName:       "Athul Narayanan",
		RequesterEmail:      "athul@example.com",
		Subject:             "Search page error",
		Message:             "The page is blank.\nPlease investigate.",
		ScreenshotFileName:  "preview.png",
		ScreenshotSizeBytes: 2048,
		CreatedAt:           time.Date(2026, 7, 20, 10, 30, 0, 0, time.UTC),
	})

	if !strings.Contains(body, "Technical Issue") || !strings.Contains(body, "preview.png") {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestCreateSupportRequestValidation(t *testing.T) {
	svc, _ := newTestService(t)

	cases := []struct {
		name string
		req  *CreateSupportRequestRequest
		user int
		want string
	}{
		{"nil request", nil, 1, "request is required"},
		{"invalid user", &CreateSupportRequestRequest{RequestType: RequestTypeQuestion, Subject: "A", Message: "B"}, 0, "valid user ID is required"},
		{"invalid type", &CreateSupportRequestRequest{RequestType: "other", Subject: "A", Message: "B"}, 1, "request_type"},
		{"missing subject", &CreateSupportRequestRequest{RequestType: RequestTypeQuestion, Message: "B"}, 1, "subject is required"},
		{"missing message", &CreateSupportRequestRequest{RequestType: RequestTypeQuestion, Subject: "A"}, 1, "message is required"},
		{"invalid email", &CreateSupportRequestRequest{RequestType: RequestTypeQuestion, Subject: "A", Message: "B", RequesterEmail: "bad"}, 1, "valid email address"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Create(tc.req, tc.user, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestCreateSupportRequestCreatesRecordAndNotification(t *testing.T) {
	svc, fakeMailer := newTestService(t)

	oldGo := supportRequestGoHook
	oldUpload := uploadSupportRequestScreenshotHook
	defer func() {
		supportRequestGoHook = oldGo
		uploadSupportRequestScreenshotHook = oldUpload
	}()

	supportRequestGoHook = func(fn func()) { fn() }
	uploadSupportRequestScreenshotHook = func(_ context.Context, base64Data, bucketName, objectName string) (string, int64, error) {
		if !strings.HasPrefix(objectName, "support_requests/7/") {
			t.Fatalf("unexpected object path: %q", objectName)
		}
		return "gs://bucket/" + objectName, 2048, nil
	}

	screenshot := multipartFileHeaderFromBytes(
		t,
		"screenshot",
		"preview.png",
		"image/png",
		[]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0x00},
	)

	res, err := svc.Create(&CreateSupportRequestRequest{
		RequestType: RequestTypeTechnicalIssue,
		Subject:     "Search page error",
		Message:     "The page loads blank after I search.",
	}, 7, screenshot)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.ID == 0 {
		t.Fatalf("expected saved record id")
	}

	var record SupportRequest
	if err := svc.DB.First(&record, res.ID).Error; err != nil {
		t.Fatalf("load record: %v", err)
	}
	if record.RequesterName != "User Seven" || record.RequesterEmail != "user7@example.com" {
		t.Fatalf("unexpected requester details: %+v", record)
	}
	if record.ScreenshotFileName != "preview.png" || record.ScreenshotURL == "" {
		t.Fatalf("expected screenshot metadata, got %+v", record)
	}
	if !record.NotificationEmailSent {
		t.Fatalf("expected notification flag true")
	}
	if len(fakeMailer.sendWithAttachCalls) != 1 {
		t.Fatalf("expected one email with attachment, got %d", len(fakeMailer.sendWithAttachCalls))
	}
	if len(fakeMailer.sendWithAttachCalls[0].Attachments) != 1 {
		t.Fatalf("expected one attachment")
	}
}

func TestCreateSupportRequestNotificationFailureKeepsFlagFalse(t *testing.T) {
	svc, fakeMailer := newTestService(t)

	oldGo := supportRequestGoHook
	defer func() { supportRequestGoHook = oldGo }()
	supportRequestGoHook = func(fn func()) { fn() }
	fakeMailer.sendErr = errors.New("smtp failed")

	res, err := svc.Create(&CreateSupportRequestRequest{
		RequestType: RequestTypeQuestion,
		Subject:     "Need help",
		Message:     "Please help with filters.",
	}, 1, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	var record SupportRequest
	if err := svc.DB.First(&record, res.ID).Error; err != nil {
		t.Fatalf("load record: %v", err)
	}
	if record.NotificationEmailSent {
		t.Fatalf("expected notification flag false")
	}
}

func TestSupportRequestManagementLifecycle(t *testing.T) {
	svc, fakeMailer := newTestService(t)

	oldGo := supportRequestGoHook
	defer func() { supportRequestGoHook = oldGo }()
	supportRequestGoHook = func(fn func()) { fn() }

	created, err := svc.Create(&CreateSupportRequestRequest{
		RequestType: RequestTypeQuestion,
		Subject:     "Need help with filters",
		Message:     "How do I search for a student?",
	}, 1, nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}

	updated, err := svc.Update(created.ID, &UpdateSupportRequestRequest{
		Status:                 RequestStatusInProgress,
		AssignedTeam:           "Platform Support",
		AssignedTeamRecipients: "platform@example.com, second@example.com",
		AdminNote:              "The support team is reviewing the search filters.",
	}, 99)
	if err != nil {
		t.Fatalf("forward request: %v", err)
	}
	if updated.Status != RequestStatusInProgress || updated.AssignedTeam != "Platform Support" {
		t.Fatalf("unexpected update: %+v", updated)
	}
	if updated.AssignedByID == nil || *updated.AssignedByID != 99 || updated.AssignedAt == nil {
		t.Fatalf("expected assignment audit data: %+v", updated)
	}

	var forwarded SupportRequest
	if err := svc.DB.First(&forwarded, created.ID).Error; err != nil {
		t.Fatalf("load forwarded request: %v", err)
	}
	if !forwarded.StatusEmailSent || !forwarded.TeamForwardEmailSent {
		t.Fatalf("expected lifecycle email flags true: %+v", forwarded)
	}
	if len(fakeMailer.sendCalls) != 4 {
		t.Fatalf("expected receipt, admin, forward, and status emails; got %d", len(fakeMailer.sendCalls))
	}
	if got := fakeMailer.sendCalls[2].To; len(got) != 2 || got[0] != "platform@example.com" {
		t.Fatalf("unexpected team recipients: %+v", got)
	}

	closed, err := svc.Update(created.ID, &UpdateSupportRequestRequest{
		Status:    RequestStatusClosed,
		AdminNote: "This has been resolved.",
	}, 99)
	if err != nil {
		t.Fatalf("close request: %v", err)
	}
	if closed.Status != RequestStatusClosed || closed.ClosedByID == nil || *closed.ClosedByID != 99 || closed.ClosedAt == nil {
		t.Fatalf("expected closed audit data: %+v", closed)
	}

	adminList, err := svc.ListForAdmin(99, 1, 20)
	if err != nil || adminList.TotalItems != 1 || len(adminList.Items) != 1 {
		t.Fatalf("unexpected admin list: %+v err=%v", adminList, err)
	}
	myList, err := svc.ListMine(1, 1, 20)
	if err != nil || len(myList.Items) != 1 || myList.Items[0].AssignedTeamRecipients != "" {
		t.Fatalf("unexpected requester list: %+v err=%v", myList, err)
	}
	if _, err := svc.ListForAdmin(1, 1, 20); !errors.Is(err, ErrSupportRequestForbidden) {
		t.Fatalf("expected forbidden non-admin list, got %v", err)
	}
}

func TestSupportRequestInProgressRequiresForwardingDetails(t *testing.T) {
	svc, _ := newTestService(t)
	created, err := svc.Create(&CreateSupportRequestRequest{
		RequestType: RequestTypeQuestion,
		Subject:     "Need help",
		Message:     "Please help with filters.",
	}, 1, nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}

	_, err = svc.Update(created.ID, &UpdateSupportRequestRequest{
		Status: RequestStatusInProgress,
	}, 99)
	if err == nil || !strings.Contains(err.Error(), "forwarding to a team") {
		t.Fatalf("expected forwarding validation error, got %v", err)
	}
}

func TestPrepareScreenshotUploadRejectsInvalidFiles(t *testing.T) {
	svc, _ := newTestService(t)

	t.Run("unsupported content type", func(t *testing.T) {
		fileHeader := multipartFileHeaderFromBytes(t, "screenshot", "notes.txt", "text/plain", []byte("hello"))
		_, err := svc.prepareScreenshotUpload(fileHeader, 1, time.Now().UTC())
		if err == nil || !strings.Contains(err.Error(), "PNG, JPG, or WEBP") {
			t.Fatalf("unexpected err: %v", err)
		}
	})

	t.Run("too large", func(t *testing.T) {
		content := make([]byte, MaxScreenshotSizeInBytes+1)
		content[0] = 0x89
		fileHeader := multipartFileHeaderFromBytes(t, "screenshot", "big.png", "image/png", content)
		_, err := svc.prepareScreenshotUpload(fileHeader, 1, time.Now().UTC())
		if err == nil || !strings.Contains(err.Error(), "5 MB or smaller") {
			t.Fatalf("unexpected err: %v", err)
		}
	})
}
