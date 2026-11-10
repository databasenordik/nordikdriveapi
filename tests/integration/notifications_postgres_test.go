//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"nordik-drive-api/internal/auth"
	"nordik-drive-api/internal/notificationcontract"
	"nordik-drive-api/internal/notifications"
	"nordik-drive-api/internal/outbox"
)

// Own a separate local database because the real migration deliberately names
// public/notifications. Never run this against DB_* or a remote test server.
func notificationPostgres(t *testing.T) *gorm.DB {
	t.Helper()
	connection, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil || connection == nil || connection.User == nil || connection.User.Username() != "nordik_test" || connection.Path != "/nordik_baseline_test" || (connection.Scheme != "postgres" && connection.Scheme != "postgresql") || (connection.Hostname() != "127.0.0.1" && connection.Hostname() != "localhost" && connection.Hostname() != "::1") {
		t.Fatal("notification integration requires the disposable local nordik_baseline_test fixture")
	}
	connection.RawQuery = "sslmode=disable&connect_timeout=5"
	options := &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}
	admin, err := gorm.Open(postgres.Open(connection.String()), options)
	if err != nil {
		t.Fatal(err)
	}
	name := "m3_" + strings.ReplaceAll(uuid.NewString(), "-", "") + "_test"
	if err := admin.Exec(`CREATE DATABASE "` + name + `"`).Error; err != nil {
		t.Fatal(err)
	}
	connection.Path = "/" + name
	db, err := gorm.Open(postgres.Open(connection.String()), options)
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := db.DB()
	pool.SetMaxOpenConns(4)
	t.Cleanup(func() {
		_ = pool.Close()
		_ = admin.Exec(`DROP DATABASE "` + name + `" WITH (FORCE)`).Error
		p, _ := admin.DB()
		_ = p.Close()
	})
	return db
}

func TestPostgresNotificationsMigrationGrantsAtomicityAndConcurrency(t *testing.T) {
	db := notificationPostgres(t)
	if err := db.AutoMigrate(&auth.Auth{}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"file_edit_request", "form_submissions", "support_requests", "support_schedule_audit_logs"} {
		if err := db.Exec(`CREATE TABLE ` + table + ` (id bigserial PRIMARY KEY)`).Error; err != nil {
			t.Fatal(err)
		}
	}
	existing := auth.Auth{Email: "existing@example.test", Password: "existing-hash", FirstName: "Existing", LastName: "User", Role: "User"}
	if err := db.Create(&existing).Error; err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../../migrations/003_notifications.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := db.Exec(string(migration)).Error; err != nil {
			t.Fatal("additive migration rerun", err)
		}
	}
	var old auth.Auth
	if err := db.First(&old, existing.ID).Error; err != nil || old.EmailVerifiedAt != nil || old.Password != "existing-hash" {
		t.Fatal("existing account changed")
	}

	// Role names are locally generated, never the deployed IAM identities.
	prefix := "m3_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	roles := map[string]string{"notifications_user": prefix + "_n", "legacy_user": prefix + "_l", "worker_user": prefix + "_w"}
	for _, role := range roles {
		if err := db.Exec(`CREATE ROLE "` + role + `" NOLOGIN`).Error; err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, role := range roles {
			_ = db.Exec(`DROP OWNED BY "` + role + `"`).Error
			_ = db.Exec(`DROP ROLE "` + role + `"`).Error
		}
	})
	grants, err := os.ReadFile("../../migrations/003_notifications_grants.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := strings.ReplaceAll(string(grants), "\\set ON_ERROR_STOP on", "")
	for variable, role := range roles {
		sql = strings.ReplaceAll(sql, `:"`+variable+`"`, `"`+role+`"`)
	}
	if err := db.Exec(sql).Error; err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		Role, Query string
		Allowed     bool
	}{
		{"notifications_user", "SELECT count(*) FROM notifications.deliveries", true},
		{"notifications_user", "SELECT count(*) FROM public.users", false},
		{"notifications_user", "SELECT count(*) FROM public.signup_otp_challenges", false},
		{"notifications_user", "SELECT count(*) FROM public.notification_outbox", false},
		{"legacy_user", "SELECT count(*) FROM public.signup_otp_challenges", true},
		{"worker_user", "SELECT count(*) FROM public.notification_outbox", true},
		{"worker_user", "SELECT count(*) FROM public.signup_otp_challenges", false},
		{"legacy_user", "SELECT count(*) FROM notifications.deliveries", false},
	} {
		err := db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec(`SET LOCAL ROLE "` + roles[test.Role] + `"`).Error; err != nil {
				return err
			}
			return tx.Exec(test.Query).Error
		})
		if (err == nil) != test.Allowed {
			t.Fatalf("unexpected permission for %s: %s (%v)", test.Role, test.Query, err)
		}
	}
	encoded := `{"1":"` + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32)) + `"}`
	codec, err := notificationcontract.NewCodec(encoded, "1")
	if err != nil {
		t.Fatal(err)
	}
	writer := &outbox.Writer{Codec: codec}
	rollback := errors.New("synthetic business rollback")
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := writer.Send(tx, "identity", "rolled-back", []string{"test@example.test"}, "Test", "Body"); err != nil {
			return err
		}
		return rollback
	})
	var count int64
	db.Model(&outbox.Record{}).Count(&count)
	if !errors.Is(err, rollback) || count != 0 {
		t.Fatal("intent survived rolled back business write")
	}
	ctx := context.Background()
	now := time.Now().UTC()
	command, err := codec.Seal(notificationcontract.Command{Version: 1, ID: uuid.NewString(), Producer: "identity", Key: "concurrent", Kind: "normal", CreatedAt: now, Deadline: now.Add(time.Hour)}, notificationcontract.Payload{To: "test@example.test", Subject: "Hello", HTML: "Body"})
	if err != nil {
		t.Fatal(err)
	}
	store := &notifications.Store{DB: db}
	raw, _ := json.Marshal(command)
	row, err := store.Accept(ctx, notifications.Delivery{ID: command.ID, Producer: command.Producer, EventKey: command.Key, Command: string(raw), Fingerprint: command.Fingerprint, State: "queued", Kind: "normal", Deadline: command.Deadline, CreatedAt: now, UpdatedAt: now, NextAttempt: now})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wins := make(chan bool, 2)
	failures := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, send, err := store.Claim(ctx, row.ID, now)
			wins <- send
			if err != nil && !errors.Is(err, notifications.ErrBusy) {
				failures <- err
			}
		}()
	}
	wg.Wait()
	close(wins)
	close(failures)
	got := 0
	for send := range wins {
		if send {
			got++
		}
	}
	for err := range failures {
		t.Error(err)
	}
	if got != 1 {
		t.Fatal("concurrent tasks acquired the same provider attempt")
	}

	verifier, err := auth.NewSignupVerifier(db, writer, `{"1":"`+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))+`"}`, "1")
	if err != nil {
		t.Fatal(err)
	}
	sent, err := verifier.Request(ctx, "signup@example.test", "192.0.2.1", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	var intent outbox.Record
	if err := db.First(&intent, "event_key = ?", "signup-otp:"+sent.ChallengeID).Error; err != nil {
		t.Fatal(err)
	}
	var sealed notificationcontract.Command
	_ = json.Unmarshal([]byte(intent.Command), &sealed)
	payload, err := codec.Open(sealed)
	if err != nil {
		t.Fatal(err)
	}
	code := regexp.MustCompile(`<strong>([0-9]{6})</strong>`).FindStringSubmatch(payload.HTML)
	if len(code) != 2 {
		t.Fatal("code missing")
	}
	proof, err := verifier.Verify(ctx, "signup@example.test", sent.ChallengeID, code[1], "192.0.2.1", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	users := make(chan *auth.Auth, 2)
	failed := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			user := auth.Auth{Email: "signup@example.test", FirstName: "New", LastName: "User", Password: "test-pass-123"}
			created, err := verifier.Create(ctx, user, proof.Token, uuid.NewString(), user)
			if err == nil {
				users <- created
			} else {
				failed <- err
			}
		}()
	}
	wg.Wait()
	close(users)
	close(failed)
	created := 0
	for user := range users {
		created++
		if user.EmailVerifiedAt == nil {
			t.Fatal("verification timestamp missing")
		}
	}
	if created != 1 {
		t.Fatal("proof did not create exactly one account")
	}
	for err := range failed {
		if !errors.Is(err, auth.ErrSignupVerification) {
			t.Error(err)
		}
	}
	db.Model(&outbox.Record{}).Where("event_key LIKE ?", "signup-welcome:%").Count(&count)
	if count != 1 {
		t.Fatal("welcome email duplicated")
	}
}
