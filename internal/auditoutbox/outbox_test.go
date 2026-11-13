package auditoutbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"nordik-drive-api/internal/activity"
)

type publisherStub struct {
	ids      []string
	attempts []int
	err      error
	check    func()
}

func (p *publisherStub) Enqueue(_ context.Context, id string, attempt int) error {
	if p.check != nil {
		p.check()
	}
	p.ids = append(p.ids, id)
	p.attempts = append(p.attempts, attempt)
	return p.err
}
func fixture(t *testing.T) (*gorm.DB, *Writer, *publisherStub) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := db.DB()
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.AutoMigrate(&Record{}); err != nil {
		t.Fatal(err)
	}
	p := &publisherStub{}
	return db, &Writer{Producer: "legacy", Publisher: p}, p
}
func event() activity.Log {
	return activity.Log{Level: "INFO", Service: "file", Action: "DELETE_FILE", Message: "File deleted : synthetic.csv"}
}
func put(t *testing.T, db *gorm.DB, w *Writer) Record {
	t.Helper()
	if err := db.Transaction(func(tx *gorm.DB) error {
		return w.Put(tx, event(), map[string]any{"password": "canary", "filename": "synthetic.csv"})
	}); err != nil {
		t.Fatal(err)
	}
	var row Record
	if err := db.Order("created_at DESC").First(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row
}
func TestCapturePublishesOnlyCommittedEventsAndRetainsFailedPublication(t *testing.T) {
	db, w, p := fixture(t)
	ctx, pending := Capture(context.Background())
	db = db.WithContext(ctx)
	rollback := errors.New("business failure")
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := w.Put(tx, event(), nil); err != nil {
			return err
		}
		return rollback
	}); !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	if err := pending.Publish(ctx, db, p); err != nil || len(p.ids) != 0 {
		t.Fatal("rolled-back intent published", err)
	}
	row := put(t, db, w)
	if strings.Contains(row.Payload, "canary") || !strings.Contains(row.Payload, "[REDACTED]") {
		t.Fatal("audit metadata leaked")
	}
	p.err = errors.New("queue unavailable")
	if pending.Publish(ctx, db, p) == nil {
		t.Fatal("queue outage hidden")
	}
	var retained Record
	db.First(&retained, "id = ?", row.ID)
	if retained.Payload != row.Payload || retained.State != "pending" || retained.Attempts != 1 || retained.LeaseUntil != nil {
		t.Fatal("failed intent discarded", retained.State)
	}
	db.Model(&Record{}).Where("id = ?", row.ID).Update("next_attempt", time.Now().Add(-time.Minute))
	p.err = nil
	pool, _ := db.DB()
	p.check = func() {
		if pool.Stats().InUse != 0 {
			t.Error("network publication held a database connection")
		}
	}
	if err := (&Dispatcher{DB: db, Publisher: p}).Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(p.ids) != 2 || p.attempts[1] != 2 {
		t.Fatal("automatic repair did not create a fresh task generation")
	}
	if err := pending.Publish(ctx, db, nil); err != nil {
		t.Fatal(err)
	}
	_, empty := Capture(ctx)
	if err := empty.Publish(ctx, db, p); err != nil {
		t.Fatal(err)
	}
}
func request(handler http.Handler, path, body, token, method string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "https://owner.example"+path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, r)
	return response
}
func TestSourceAcknowledgmentErasesPayloadOnlyForMatchingDurableReceipt(t *testing.T) {
	db, w, _ := fixture(t)
	row := put(t, db, w)
	authorize := func(_ context.Context, token string) error {
		if token != "signed-audit" {
			return errors.New("forbidden")
		}
		return nil
	}
	handler := SourceHandler(db, authorize)
	input := fmt.Sprintf(`{"event_id":%q}`, row.ID)
	response := request(handler, "/internal/v1/audit-outbox/read", input, "signed-audit", "POST")
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	var record SourceRecord
	if json.Unmarshal(response.Body.Bytes(), &record) != nil || record.Event == nil || record.Receipt.Fingerprint != row.Fingerprint {
		t.Fatal("invalid source response")
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("event response cacheable")
	}
	receipt := Receipt{ID: row.ID, Fingerprint: row.Fingerprint, LogID: 91}
	encoded, _ := json.Marshal(receipt)
	for range 2 {
		if response := request(handler, "/internal/v1/audit-outbox/ack", string(encoded), "signed-audit", "POST"); response.Code != 204 {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	db.First(&row, "id = ?", row.ID)
	if row.Payload != "" || row.State != "ingested" || row.LogID != 91 {
		t.Fatal("acknowledged payload retained")
	}
	response = request(handler, "/internal/v1/audit-outbox/read", input, "signed-audit", "POST")
	if json.Unmarshal(response.Body.Bytes(), &record) != nil {
		t.Fatal("receipt unavailable")
	}
	// Decode into a fresh value: omitted event fields must not be mistaken for a
	// previous payload by clients.
	var fresh SourceRecord
	_ = json.Unmarshal(response.Body.Bytes(), &fresh)
	if fresh.Event != nil || fresh.Receipt.LogID != 91 {
		t.Fatal("ingested payload exposed")
	}
	for _, receipt := range []Receipt{{ID: row.ID, Fingerprint: row.Fingerprint, LogID: 92}, {ID: row.ID, Fingerprint: "wrong", LogID: 91}, {ID: row.ID, Fingerprint: row.Fingerprint}} {
		encoded, _ := json.Marshal(receipt)
		if request(handler, "/internal/v1/audit-outbox/ack", string(encoded), "signed-audit", "POST").Code != 409 {
			t.Fatal("invalid receipt accepted")
		}
	}
}
func TestSourceAndDispatchRejectInvalidRequestsAndDatabaseFailures(t *testing.T) {
	db, w, p := fixture(t)
	row := put(t, db, w)
	allow := func(context.Context, string) error { return nil }
	handler := SourceHandler(db, allow)
	for _, test := range []struct {
		path, body, token, method string
		code                      int
	}{
		{"/internal/v1/audit-outbox/read", `{}`, "", "GET", 405},
		{"/internal/v1/audit-outbox/read", `{`, "", "POST", 400},
		{"/internal/v1/audit-outbox/read", `{"unknown":1}`, "", "POST", 400},
		{"/internal/v1/audit-outbox/read", `{"event_id":"bad"}`, "", "POST", 400},
		{"/internal/v1/audit-outbox/read", fmt.Sprintf(`{"event_id":%q}`, uuid.NewString()), "", "POST", 404},
		{"/wrong", fmt.Sprintf(`{"event_id":%q}`, row.ID), "", "POST", 404},
	} {
		if response := request(handler, test.path, test.body, test.token, test.method); response.Code != test.code {
			t.Fatalf("%s status=%d expected=%d", test.body, response.Code, test.code)
		}
	}
	deny := func(context.Context, string) error { return errors.New("denied") }
	if request(SourceHandler(db, deny), "/internal/v1/audit-outbox/read", `{}`, "", "POST").Code != 403 {
		t.Fatal("unauthorized read")
	}
	dispatch := DispatchHandler(&Dispatcher{DB: db, Publisher: p}, allow)
	if request(dispatch, "/internal/v1/audit-outbox/dispatch", `{}`, "", "POST").Code != 204 {
		t.Fatal("dispatch failed")
	}
	if request(dispatch, "/wrong", `{}`, "", "POST").Code != 404 || request(dispatch, "/internal/v1/audit-outbox/dispatch", `{}`, "", "GET").Code != 405 || request(DispatchHandler(nil, deny), "/internal/v1/audit-outbox/dispatch", `{}`, "", "POST").Code != 403 {
		t.Fatal("dispatch guard failed")
	}
	db.Model(&row).Update("payload", "invalid")
	if request(handler, "/internal/v1/audit-outbox/read", fmt.Sprintf(`{"event_id":%q}`, row.ID), "", "POST").Code != 503 {
		t.Fatal("corrupt event exposed")
	}
	pool, _ := db.DB()
	_ = pool.Close()
	if request(handler, "/internal/v1/audit-outbox/read", fmt.Sprintf(`{"event_id":%q}`, row.ID), "", "POST").Code != 503 {
		t.Fatal("database outage hidden")
	}
	if request(dispatch, "/internal/v1/audit-outbox/dispatch", `{}`, "", "POST").Code != 503 {
		t.Fatal("dispatch database outage hidden")
	}
}
func TestWriterAndDispatcherFailures(t *testing.T) {
	db, w, p := fixture(t)
	var absent *Writer
	if absent.Put(db, event(), nil) != nil {
		t.Fatal("disabled writer failed")
	}
	w.Producer = "invalid"
	if w.Put(db, event(), nil) == nil {
		t.Fatal("invalid producer accepted")
	}
	w.Producer = "legacy"
	ctx, pending := Capture(context.Background())
	span := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{1}})
	ctx = trace.ContextWithSpanContext(ctx, span)
	db = db.WithContext(ctx)
	row := put(t, db, w)
	if !strings.Contains(row.Payload, "trace_id") {
		t.Fatal("trace lost")
	}
	cancelled, stop := context.WithCancel(ctx)
	stop()
	if (&Dispatcher{DB: db, Publisher: p}).Run(cancelled) == nil {
		t.Fatal("cancelled reconciliation succeeded")
	}
	if (&Dispatcher{DB: db}).Run(ctx) == nil {
		t.Fatal("absent publisher accepted")
	}
	pool, _ := db.DB()
	_ = pool.Close()
	if w.Put(db, event(), nil) == nil || pending.Publish(ctx, db, p) == nil {
		t.Fatal("database error hidden")
	}
}

func TestReconciliationProcessesMoreThanOneBatchAutomatically(t *testing.T) {
	db, w, p := fixture(t)
	for range 101 {
		put(t, db, w)
	}
	if err := (&Dispatcher{DB: db, Publisher: p}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.ids) != 101 {
		t.Fatal("reconciliation stopped after one batch", len(p.ids))
	}
}
