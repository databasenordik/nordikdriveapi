package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"nordik-drive-api/internal/notificationcontract"
)

func TestProducerSourceAuthorizesSealedReadAndMonotonicReceipt(t *testing.T) {
	db, writer, _ := outboxFixture(t)
	id, err := writer.Put(db, notificationcontract.Command{Producer: "identity", Key: "source-test", Kind: "otp", Deadline: time.Now().Add(10 * time.Minute)}, notificationcontract.Payload{To: "private@example.test", Subject: "Test", HTML: "protected-body"})
	if err != nil {
		t.Fatal(err)
	}
	handler := sourceHandler(db, func(_ context.Context, token string) error {
		if token != "reader" {
			return errors.New("forbidden")
		}
		return nil
	})
	request := func(path, token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	readBody := `{"delivery_id":"` + id + `"}`
	if w := request("/internal/v1/notification-outbox/read", "other", readBody); w.Code != 403 {
		t.Fatal("unauthorized source read", w.Code)
	}
	w := request("/internal/v1/notification-outbox/read", "reader", readBody)
	var result notificationcontract.SourceRecord
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Command.Ciphertext == "" || strings.Contains(w.Body.String(), "private@example.test") || strings.Contains(w.Body.String(), "protected-body") {
		t.Fatal("producer did not return only its sealed command", w.Code)
	}
	receipt := `{"outbox_id":"` + id + `","delivery_id":"` + id + `","state":"unknown"}`
	if w := request("/internal/v1/notification-outbox/receipt", "reader", receipt); w.Code != 204 {
		t.Fatal("uncertain receipt failed", w.Code)
	}
	stale := strings.Replace(receipt, `"unknown"`, `"queued"`, 1)
	if w := request("/internal/v1/notification-outbox/receipt", "reader", stale); w.Code != 204 {
		t.Fatal(w.Code)
	}
	var row Record
	if err := db.First(&row, "id = ?", id).Error; err != nil || row.State != "unknown" || !row.PayloadErased {
		t.Fatal("stale receipt reactivated an uncertain send", err)
	}
	var command notificationcontract.Command
	_ = json.Unmarshal([]byte(row.Command), &command)
	if command.Ciphertext != "" {
		t.Fatal("terminal OTP retained payload")
	}
	for _, body := range []string{`{}`, readBody + readBody, `{"delivery_id":"not-uuid"}`, `{"delivery_id":"` + id + `","extra":true}`} {
		if w := request("/internal/v1/notification-outbox/read", "reader", body); w.Code != 400 {
			t.Fatal("invalid source body accepted", w.Code)
		}
	}
	other := strings.Replace(receipt, `"delivery_id":"`+id+`"`, `"delivery_id":"`+uuid.NewString()+`"`, 1)
	if w := request("/internal/v1/notification-outbox/receipt", "reader", other); w.Code != 409 {
		t.Fatal("conflicting canonical receipt accepted", w.Code)
	}
	if _, err := NewSourceHandler(db, "", ""); err == nil {
		t.Fatal("missing source authorization accepted")
	}
}

func TestExpiredProducerIntentIsNotExposedForDelivery(t *testing.T) {
	db, writer, _ := outboxFixture(t)
	created := time.Now().Add(-20 * time.Minute)
	id, err := writer.Put(db, notificationcontract.Command{Producer: "identity", Key: "expired-source", Kind: "otp", CreatedAt: created, Deadline: created.Add(10 * time.Minute)}, notificationcontract.Payload{To: "private@example.test", Subject: "Expired", HTML: "old-code"})
	if err != nil {
		t.Fatal(err)
	}
	h := sourceHandler(db, func(context.Context, string) error { return nil })
	r := httptest.NewRequest(http.MethodPost, "/internal/v1/notification-outbox/read", strings.NewReader(`{"delivery_id":"`+id+`"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var record notificationcontract.SourceRecord
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &record) != nil || record.State != "expired" || record.Command.Ciphertext != "" {
		t.Fatal("expired source could be delivered")
	}
}
