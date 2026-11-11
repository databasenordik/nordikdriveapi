package notifications

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"net/http"
	"net/http/httptest"
	"nordik-drive-api/internal/notificationcontract"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeQueue struct {
	mu    sync.Mutex
	Fail  bool
	Calls int
}

func (q *fakeQueue) Enqueue(context.Context, Delivery) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.Calls++
	if q.Fail {
		return errors.New("queue down")
	}
	return nil
}

type providerFunc func(context.Context, notificationcontract.Payload) string

func (f providerFunc) Deliver(ctx context.Context, p notificationcontract.Payload) string {
	return f(ctx, p)
}
func serviceFixture(t *testing.T) (*Service, *Store, *fakeQueue, *time.Time) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := db.DB()
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.Exec("ATTACH DATABASE ':memory:' AS notifications").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Delivery{}, &Attempt{}, &ReplayRecord{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE UNIQUE INDEX notifications.delivery_event ON deliveries(producer,event_key)").Error; err != nil {
		t.Fatal(err)
	}
	codec, err := notificationcontract.NewCodec(`{"1":"`+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32))+`"}`, "1")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	queue := &fakeQueue{}
	store := &Store{DB: db}
	service := &Service{Store: store, Codec: codec, Queue: queue, Clock: func() time.Time { return now }, Provider: providerFunc(func(context.Context, notificationcontract.Payload) string { return "provider_accepted" })}
	return service, store, queue, &now
}
func sealedCommand(t *testing.T, s *Service, key string) notificationcontract.Command {
	t.Helper()
	command, err := s.Codec.Seal(notificationcontract.Command{Version: 1, ID: uuid.NewString(), Producer: "identity", Key: key, Kind: "normal", CreatedAt: s.now(), Deadline: s.now().Add(time.Hour)}, notificationcontract.Payload{To: "person@example.test", Subject: "Hello", HTML: "Test body"})
	if err != nil {
		t.Fatal(err)
	}
	return command
}
func TestDurableIntakeRecoveryAndDuplicateTasks(t *testing.T) {
	s, store, queue, now := serviceFixture(t)
	ctx := context.Background()
	queue.Fail = true
	command := sealedCommand(t, s, "event-1")
	id, err := s.Accept(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	var row Delivery
	if err := store.DB.First(&row, "id = ?", id).Error; err != nil || row.State != "pending_enqueue" {
		t.Fatal("intake was not durable during enqueue failure")
	}
	queue.Fail = false
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	var calls int
	s.Provider = providerFunc(func(ctx context.Context, p notificationcontract.Payload) string {
		calls++
		var n int
		if err := store.DB.WithContext(ctx).Raw("SELECT 1").Scan(&n).Error; err != nil {
			t.Error("SQL connection was held during provider work")
		}
		return "provider_accepted"
	})
	if err := s.Deliver(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := s.Deliver(ctx, id); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("duplicate provider call")
	}
	if duplicate, err := s.Accept(ctx, command); err != nil || duplicate != id {
		t.Fatal("duplicate intake did not return original receipt")
	}
	changed := command
	payload := notificationcontract.Payload{To: "person@example.test", Subject: "Different", HTML: "body"}
	changed, err = s.Codec.Seal(changed, payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Accept(ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatal("conflicting command accepted")
	}
	*now = now.Add(3 * time.Minute)
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("recovery replayed accepted email")
	}
}
func TestDeliveryCrashAfterProviderAcceptanceBecomesUnknown(t *testing.T) {
	s, store, _, now := serviceFixture(t)
	ctx := context.Background()
	id, err := s.Accept(ctx, sealedCommand(t, s, "crash"))
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	s.Provider = providerFunc(func(context.Context, notificationcontract.Payload) string {
		calls++
		if err := store.DB.Migrator().DropTable(&Attempt{}); err != nil {
			t.Fatal(err)
		}
		return "provider_accepted"
	})
	if err := s.Deliver(ctx, id); err == nil {
		t.Fatal("expected outcome-write failure")
	}
	if err := store.DB.AutoMigrate(&Attempt{}); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(2 * time.Minute)
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Deliver(ctx, id); err != nil {
		t.Fatal(err)
	}
	states, err := store.Statuses(ctx, []string{id})
	if err != nil || states[id] != "unknown" || calls != 1 {
		t.Fatal("ambiguous acceptance was automatically resent")
	}
}
func TestDeliveryLimitsExpirySupersessionAndOutcomeClassification(t *testing.T) {
	for _, outcome := range []string{"failed", "unknown", "invalid", "retry"} {
		t.Run(outcome, func(t *testing.T) {
			s, store, _, now := serviceFixture(t)
			ctx := context.Background()
			id, err := s.Accept(ctx, sealedCommand(t, s, "outcome"))
			if err != nil {
				t.Fatal(err)
			}
			s.Provider = providerFunc(func(context.Context, notificationcontract.Payload) string { return outcome })
			for range 6 {
				_ = s.Deliver(ctx, id)
				*now = now.Add(61 * time.Second)
			}
			var row Delivery
			if err := store.DB.First(&row, "id = ?", id).Error; err != nil {
				t.Fatal(err)
			}
			if outcome == "retry" {
				if row.Attempts != 5 || row.State != "failed" {
					t.Fatal("safe retry hard limit was bypassed")
				}
			} else if row.Attempts != 1 {
				t.Fatal("terminal outcome retried")
			}
		})
	}
	s, store, _, now := serviceFixture(t)
	ctx := context.Background()
	old := sealedCommand(t, s, "old")
	old.Kind = "otp"
	old.Deadline = old.CreatedAt.Add(10 * time.Minute)
	old.Supersession = "email-group"
	old.Sequence = 1
	p, _ := s.Codec.Open(sealedCommand(t, s, "payload"))
	old, _ = s.Codec.Seal(old, p)
	id, err := s.Accept(ctx, old)
	if err != nil {
		t.Fatal(err)
	}
	newer := old
	newer.ID = uuid.NewString()
	newer.Key = "new"
	newer.Sequence = 2
	newer, _ = s.Codec.Seal(newer, p)
	newID, err := s.Accept(ctx, newer)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Deliver(ctx, id); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(11 * time.Minute)
	if err := s.Deliver(ctx, newID); err != nil {
		t.Fatal(err)
	}
	states, _ := store.Statuses(ctx, []string{id, newID})
	if states[id] != "superseded" || states[newID] != "expired" {
		t.Fatal("stale OTP was delivered")
	}
}
func TestConcurrentTaskClaimAndCancelledRequestPersistence(t *testing.T) {
	s, store, _, _ := serviceFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	id, err := s.Accept(ctx, sealedCommand(t, s, "concurrent"))
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	s.Provider = providerFunc(func(context.Context, notificationcontract.Payload) string {
		calls.Add(1)
		close(entered)
		<-release
		cancel()
		return "provider_accepted"
	})
	finished := make(chan error, 1)
	go func() { finished <- s.Deliver(ctx, id) }()
	<-entered
	if err := s.Deliver(context.Background(), id); !errors.Is(err, ErrBusy) {
		t.Fatal("second task acquired active delivery")
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	states, err := store.Statuses(context.Background(), []string{id})
	if err != nil || states[id] != "provider_accepted" || calls.Load() != 1 {
		t.Fatal("known result lost after caller cancellation")
	}
}
func TestPrivateHTTPAuthorizationAndContracts(t *testing.T) {
	s, _, _, _ := serviceFixture(t)
	handler := Handler(s, func(_ context.Context, token, role string) (string, error) {
		if token == role {
			return "fixture@example.test", nil
		}
		return "", errors.New("forbidden")
	})
	command := sealedCommand(t, s, "http")
	encoded, _ := json.Marshal(command)
	request := func(path, role, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		if role != "" {
			r.Header.Set("Authorization", "Bearer "+role)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := request("/internal/v1/notifications", "delivery", string(encoded)); w.Code != 403 {
		t.Fatal("task identity admitted to intake")
	}
	w := request("/internal/v1/notifications", "producer:identity", string(encoded))
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var result map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	if w := request("/internal/v1/deliver", "producer:identity", `{"delivery_id":"`+result["delivery_id"]+`"}`); w.Code != 403 {
		t.Fatal("producer admitted to delivery")
	}
	if w := request("/internal/v1/deliver", "delivery", `{"delivery_id":"`+result["delivery_id"]+`"}`); w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request("/internal/v1/status", "recovery", `{"ids":["`+result["delivery_id"]+`"]}`); w.Code != 200 || !strings.Contains(w.Body.String(), "provider_accepted") {
		t.Fatal("status contract failed")
	}
	for _, body := range []string{"{", `{} {}`, `{"unknown":true}`} {
		if w := request("/internal/v1/deliver", "delivery", body); w.Code != 400 {
			t.Fatal("invalid request accepted")
		}
	}
	if w := request("/internal/v1/recover", "recovery", ""); w.Code != 204 {
		t.Fatal("recovery failed")
	}
}
