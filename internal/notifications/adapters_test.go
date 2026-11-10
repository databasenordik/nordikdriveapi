package notifications

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/fsouza/fake-gcs-server/fakestorage"
	"github.com/google/uuid"
	"google.golang.org/api/cloudtasks/v2"
	"google.golang.org/api/option"
	"io"
	"net/http"
	"net/http/httptest"
	"nordik-drive-api/internal/mailer"
	"nordik-drive-api/internal/notificationcontract"
	"sync/atomic"
	"testing"
)

type attachmentMailer struct {
	Err         error
	Calls       int
	Attachments []mailer.Attachment
}

func (m *attachmentMailer) SendWithAttachments(_ []string, _ string, _ string, attachments []mailer.Attachment) error {
	m.Calls++
	m.Attachments = attachments
	return m.Err
}
func TestSMTPOutcomeClassification(t *testing.T) {
	payload := notificationcontract.Payload{To: "person@example.test", Subject: "Review", HTML: "Body"}
	for _, test := range []struct {
		Err  error
		Want string
	}{{nil, "provider_accepted"}, {&mailer.DeliveryError{SafeToRetry: true}, "retry"}, {&mailer.DeliveryError{Permanent: true}, "failed"}, {errors.New("uncertain SMTP outcome"), "unknown"}} {
		sender := &attachmentMailer{Err: test.Err}
		p := &SMTPProvider{Mail: sender}
		if got := p.Deliver(context.Background(), payload); got != test.Want {
			t.Fatalf("outcome %s, expected %s", got, test.Want)
		}
	}
	if got := (&SMTPProvider{}).Deliver(context.Background(), payload); got != "retry" {
		t.Fatal("unavailable sender was not retryable")
	}
	if got := (&SMTPProvider{Mail: &attachmentMailer{}}).Deliver(context.Background(), notificationcontract.Payload{}); got != "failed" {
		t.Fatal("invalid message accepted")
	}
}
func TestImmutableScreenshotRetrievalAndValidation(t *testing.T) {
	data := []byte("synthetic screenshot bytes")
	server := fakestorage.NewServer([]fakestorage.Object{{ObjectAttrs: fakestorage.ObjectAttrs{BucketName: "nordik-drive-photos", Name: "support_requests/1/image.png", ContentType: "image/png"}, Content: data}})
	defer server.Stop()
	client := server.Client()
	defer client.Close()
	attrs, err := client.Bucket("nordik-drive-photos").Object("support_requests/1/image.png").Attrs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checksum := sha256.Sum256(data)
	ref := notificationcontract.Attachment{Bucket: "nordik-drive-photos", Object: "support_requests/1/image.png", Generation: attrs.Generation, Size: int64(len(data)), SHA256: hex.EncodeToString(checksum[:]), Filename: "image.png", ContentType: "image/png"}
	sender := &attachmentMailer{}
	provider := &SMTPProvider{Mail: sender, Storage: client}
	payload := notificationcontract.Payload{To: "person@example.test", Subject: "Support request", HTML: "Body", Attachments: []notificationcontract.Attachment{ref}}
	if got := provider.Deliver(context.Background(), payload); got != "provider_accepted" || len(sender.Attachments) != 1 || string(sender.Attachments[0].Data) != string(data) {
		t.Fatal("verified screenshot not delivered", got)
	}
	for _, mutate := range []func(*notificationcontract.Attachment){func(r *notificationcontract.Attachment) { r.Generation++ }, func(r *notificationcontract.Attachment) { r.Size-- }, func(r *notificationcontract.Attachment) { r.ContentType = "image/jpeg" }, func(r *notificationcontract.Attachment) { r.SHA256 = hex.EncodeToString(make([]byte, 32)) }} {
		changed := ref
		mutate(&changed)
		payload.Attachments = []notificationcontract.Attachment{changed}
		if got := provider.Deliver(context.Background(), payload); got != "failed" {
			t.Fatal("invalid screenshot delivered", got)
		}
	}
	payload.Attachments = []notificationcontract.Attachment{ref}
	if got := (&SMTPProvider{Mail: sender}).Deliver(context.Background(), payload); got != "retry" {
		t.Fatal("missing object client was not retryable")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := provider.Deliver(ctx, payload); got != "retry" {
		t.Fatal("cancelled object download was not retryable")
	}
}
func TestTaskEnvelopeAndPinnedOIDCIdentity(t *testing.T) {
	var responseCode atomic.Int32
	responseCode.Store(200)
	id := uuid.NewString()
	seen := make(chan cloudtasks.CreateTaskRequest, 5)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body cloudtasks.CreateTaskRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		seen <- body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(responseCode.Load()))
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	q, err := NewCloudQueue(context.Background(), "projects/p/locations/us-west1/queues/otp", "projects/p/locations/us-west1/queues/normal", "https://notification.example.test", "tasks@example.test", option.WithEndpoint(server.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	row := Delivery{ID: id, Kind: "otp", DispatchGeneration: 2}
	if err := q.Enqueue(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	body := <-seen
	if body.Task.HttpRequest.OidcToken.ServiceAccountEmail != "tasks@example.test" || body.Task.HttpRequest.OidcToken.Audience != q.URL || body.Task.HttpRequest.Url != q.URL+"/internal/v1/deliver" || body.Task.Name != q.OTP+"/tasks/"+id+"-2" {
		t.Fatal("task identity or target incorrect")
	}
	decoded, err := base64.StdEncoding.DecodeString(body.Task.HttpRequest.Body)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]string
	if json.Unmarshal(decoded, &envelope) != nil || len(envelope) != 1 || envelope["delivery_id"] != id {
		t.Fatal("task contained message data")
	}
	responseCode.Store(409)
	row.Kind = "normal"
	if err := q.Enqueue(context.Background(), row); err != nil {
		t.Fatal("named-task retry was not idempotent", err)
	}
	<-seen
	responseCode.Store(403)
	if err := q.Enqueue(context.Background(), row); err == nil {
		t.Fatal("enqueue permission failure ignored")
	}
	<-seen
	for _, address := range []string{"http://example.test", "https://user@example.test", "https://example.test/path", "https://example.test?query=1"} {
		if _, err := NewCloudQueue(context.Background(), q.OTP, q.Normal, address, q.Account, option.WithoutAuthentication()); err == nil {
			t.Fatal("unsafe target accepted")
		}
	}
	if _, err := NewCloudQueue(context.Background(), q.OTP, q.Normal, q.URL, q.Account, option.WithCredentialsJSON([]byte("invalid"))); err == nil {
		t.Fatal("invalid credentials accepted")
	}
}
