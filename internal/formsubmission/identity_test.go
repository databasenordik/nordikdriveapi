package formsubmission

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	"nordik-drive-api/internal/identityclient"
	"nordik-drive-api/internal/notificationcontract"
	"nordik-drive-api/internal/outbox"
)

type formDirectory struct {
	pool  *sql.DB
	users map[int64]identityclient.Profile
	calls int
	fail  bool
}

func (d *formDirectory) Profiles(_ context.Context, ids []int64, email bool) (map[int64]identityclient.Profile, error) {
	d.calls++
	if d.pool.Stats().InUse != 0 {
		return nil, errors.New("remote profile call inside business transaction")
	}
	if d.fail {
		return nil, errors.New("Identity unavailable")
	}
	result := map[int64]identityclient.Profile{}
	for _, id := range ids {
		if p, ok := d.users[id]; ok {
			if !email {
				p.Email = ""
			}
			result[id] = p
		}
	}
	return result, nil
}
func (d *formDirectory) Principals(context.Context, []int64) (map[int64]identityclient.Principal, error) {
	return nil, errors.New("unexpected principals")
}
func (d *formDirectory) Search(context.Context, string, string, string) ([]int64, error) {
	return nil, errors.New("unexpected search")
}
func ownerFormFixture(t *testing.T) (*FormSubmissionService, *formDirectory) {
	t.Helper()
	service := newTestService(t)
	pool, _ := service.DB.DB()
	directory := &formDirectory{pool: pool, users: map[int64]identityclient.Profile{}}
	var users []FormSubmissionUserRef
	if err := service.DB.Find(&users).Error; err != nil {
		t.Fatal(err)
	}
	for _, user := range users {
		directory.users[int64(user.ID)] = identityclient.Profile{ID: int64(user.ID), Firstname: user.FirstName, Lastname: user.LastName, Email: user.Email}
	}
	if err := service.DB.Migrator().RenameTable("users", "identity_owned_users"); err != nil {
		t.Fatal(err)
	}
	if err := service.DB.AutoMigrate(&outbox.Record{}); err != nil {
		t.Fatal(err)
	}
	codec, err := notificationcontract.NewCodec(`{"1":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`, "1")
	if err != nil {
		t.Fatal(err)
	}
	service.Directory = directory
	service.Outbox = &outbox.Writer{Codec: codec}
	service.Mailer = nil
	return service, directory
}
func TestFormsOwnerProfilesPreserveMutationResponsesWithoutUserTable(t *testing.T) {
	service, directory := ownerFormFixture(t)
	req := &SaveFormSubmissionRequest{FileID: 1, RowID: 10, FormKey: "owner-form", FormLabel: "Owner form", Details: []FormSubmissionDetailInput{{DetailKey: "name", DetailLabel: "Name", FieldType: "text", Value: "Synthetic"}}}
	created, err := service.Upsert(req, 1)
	if err != nil {
		t.Fatal(err)
	}
	if directory.calls != 1 || created.CreatedBy != "user1@example.com" {
		t.Fatal("creation did not use one pre-transaction profile batch")
	}
	req.FormKey = " owner-form "
	before := directory.calls
	updated, err := service.Upsert(req, 2)
	if err != nil {
		t.Fatal(err)
	}
	if directory.calls-before != 1 || updated.ID != created.ID || updated.CreatedBy != "user1@example.com" || updated.EditedBy != "user2@example.com" {
		t.Fatal("trimmed-key update or owner enrichment changed")
	}
	var detail FormSubmissionDetail
	if err := service.DB.Where("submission_id=?", created.ID).First(&detail).Error; err != nil {
		t.Fatal(err)
	}
	reviewer := 7
	upload := FormSubmissionUpload{SubmissionID: created.ID, DetailID: detail.ID, UploadType: "document", FileName: "synthetic.pdf", FileURL: "gs://nordik-drive-photos/form_submissions/synthetic.pdf", ReviewedByID: &reviewer}
	if err := service.DB.Create(&upload).Error; err != nil {
		t.Fatal(err)
	}
	before = directory.calls
	reviewed, err := service.ReviewSubmission(&ReviewFormSubmissionRequest{SubmissionID: created.ID, SubmissionReview: &SubmissionReviewInput{Status: "approved"}, UploadReviews: []UploadReviewInput{{UploadID: upload.ID, Status: "approved"}}}, 99)
	if err != nil {
		t.Fatal(err)
	}
	if directory.calls-before != 1 || reviewed.CreatedBy != "user1@example.com" || reviewed.EditedBy != "user2@example.com" || reviewed.ReviewedBy != "reviewer@example.com" || len(reviewed.Documents) != 1 || reviewed.Documents[0].ReviewedBy != "reviewer@example.com" {
		t.Fatal("review response identities changed or an RPC happened after mutation")
	}
	var intent outbox.Record
	if err := service.DB.First(&intent).Error; err != nil {
		t.Fatal(err)
	}
	var command notificationcontract.Command
	if json.Unmarshal([]byte(intent.Command), &command) != nil {
		t.Fatal("notification command missing")
	}
	payload, err := service.Outbox.Codec.Open(command)
	if err != nil || payload.To != "user1@example.com" {
		t.Fatal("review email recipient changed", err)
	}
	read, err := service.GetByID(created.ID)
	if err != nil || read.ReviewedBy != reviewed.ReviewedBy {
		t.Fatal("read enrichment failed", err)
	}
	search, err := service.SearchSubmissions(context.Background(), SearchFormSubmissionsRequest{}, 1, 10)
	if err != nil || len(search.Items) != 1 || search.Items[0].CreatedBy != "user1@example.com" {
		t.Fatal("paged owner enrichment failed", err)
	}
}
func TestFormsOwnerFailureCannotCommitAndMissingSubmissionKeepsItsError(t *testing.T) {
	service, directory := ownerFormFixture(t)
	directory.fail = true
	req := &SaveFormSubmissionRequest{FileID: 1, RowID: 11, FormKey: "owner-form", FormLabel: "Owner form"}
	if _, err := service.Upsert(req, 1); err == nil {
		t.Fatal("owner outage allowed mutation")
	}
	var count int64
	if err := service.DB.Model(&FormSubmission{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatal("failed owner lookup committed a form")
	}
	directory.fail = false
	if _, err := service.ReviewSubmission(&ReviewFormSubmissionRequest{SubmissionID: 999, SubmissionReview: &SubmissionReviewInput{Status: "approved"}}, 99); !errors.Is(err, ErrFormSubmissionNotFound) {
		t.Fatal("missing-submission error changed", err)
	}
}
