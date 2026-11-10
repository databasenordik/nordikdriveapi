package auth

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
	"nordik-drive-api/internal/notificationcontract"
	"nordik-drive-api/internal/outbox"
	"nordik-drive-api/internal/util"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func signupFixture(t *testing.T) (*SignupVerifier, *time.Time) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := db.DB()
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.Exec(`CREATE TABLE users (id integer PRIMARY KEY AUTOINCREMENT, firstname text, lastname text, email text UNIQUE NOT NULL, community text, password text, role text, created_at datetime, updated_at datetime, email_verified_at datetime)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SignupChallenge{}, &signupLimit{}, &signupIdempotency{}, &outbox.Record{}); err != nil {
		t.Fatal(err)
	}
	encoded := `{"1":"` + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32)) + `"}`
	payloadKeys := `{"1":"` + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32)) + `"}`
	codec, err := notificationcontract.NewCodec(payloadKeys, "1")
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewSignupVerifier(db, &outbox.Writer{Codec: codec}, encoded, "1")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Hour).Add(10 * time.Minute)
	verifier.Clock = func() time.Time { return now }
	return verifier, &now
}
func signupCode(t *testing.T, s *SignupVerifier, id string) string {
	t.Helper()
	var row outbox.Record
	if err := s.DB.First(&row, "event_key = ?", "signup-otp:"+id).Error; err != nil {
		t.Fatal(err)
	}
	var command notificationcontract.Command
	if err := json.Unmarshal([]byte(row.Command), &command); err != nil {
		t.Fatal(err)
	}
	p, err := s.Outbox.Codec.Open(command)
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`<strong>([0-9]{6})</strong>`).FindStringSubmatch(p.HTML)
	if len(match) != 2 {
		t.Fatal("OTP email has no six-digit code")
	}
	return match[1]
}
func verifiedSignup(t *testing.T, s *SignupVerifier, email string) (SignupResult, SignupResult) {
	t.Helper()
	sent, err := s.Request(context.Background(), email, "192.0.2.4", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	proof, err := s.Verify(context.Background(), email, sent.ChallengeID, signupCode(t, s, sent.ChallengeID), "192.0.2.4", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	return sent, proof
}
func TestSignupVerifiesBeforeCreatingAndConsumesProofAtomically(t *testing.T) {
	s, _ := signupFixture(t)
	ctx := context.Background()
	email := "person@example.test"
	_, proof := verifiedSignup(t, s, email)
	var count int64
	if err := s.DB.Model(&Auth{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatal("account created before final signup")
	}
	user := Auth{FirstName: "Test", LastName: "Person", Email: email, Password: "synthetic-password"}
	request := map[string]string{"email": email, "password": user.Password, "proof": proof.Token}
	key := uuid.NewString()
	created, err := s.Create(ctx, user, proof.Token, key, request)
	if err != nil {
		t.Fatal(err)
	}
	if created.EmailVerifiedAt == nil {
		t.Fatal("new account verification not recorded")
	}
	replayed, err := s.Create(ctx, user, proof.Token, key, request)
	if err != nil || replayed.ID != created.ID {
		t.Fatal("lost signup response could not be replayed")
	}
	if _, err := s.Create(ctx, user, proof.Token, uuid.NewString(), request); !errors.Is(err, ErrSignupVerification) {
		t.Fatal("proof reused")
	}
	var stored Auth
	if err := s.DB.First(&stored, "id = ?", created.ID).Error; err != nil {
		t.Fatal(err)
	}
	if util.VerifyPassword(user.Password, stored.Password) != nil {
		t.Fatal("password was not hashed")
	}
	var intents int64
	_ = s.DB.Model(&outbox.Record{}).Count(&intents).Error
	if intents != 2 {
		t.Fatal("expected OTP and one welcome intent")
	}
}
func TestSignupResendExpiryPurposeAndEmailBinding(t *testing.T) {
	s, now := signupFixture(t)
	ctx := context.Background()
	email := "person@example.test"
	key := uuid.NewString()
	first, err := s.Request(ctx, email, "192.0.2.4", key)
	if err != nil {
		t.Fatal(err)
	}
	code := signupCode(t, s, first.ChallengeID)
	retry, err := s.Request(ctx, email, "192.0.2.4", key)
	if err != nil || retry.ChallengeID != first.ChallengeID {
		t.Fatal("network retry generated another code")
	}
	if _, err := s.Request(ctx, email, "192.0.2.4", uuid.NewString()); err == nil {
		t.Fatal("cooldown bypassed")
	}
	*now = now.Add(61 * time.Second)
	second, err := s.Request(ctx, email, "192.0.2.4", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(ctx, email, first.ChallengeID, code, "192.0.2.4", uuid.NewString()); err == nil {
		t.Fatal("superseded code accepted")
	}
	if _, err := s.Verify(ctx, "other@example.test", second.ChallengeID, signupCode(t, s, second.ChallengeID), "192.0.2.4", uuid.NewString()); err == nil {
		t.Fatal("cross-email code accepted")
	}
	if _, err := s.Verify(ctx, email, "password-reset", code, "192.0.2.4", uuid.NewString()); err == nil {
		t.Fatal("cross-purpose code accepted")
	}
	*now = now.Add(10 * time.Minute)
	if _, err := s.Verify(ctx, email, second.ChallengeID, signupCode(t, s, second.ChallengeID), "192.0.2.4", uuid.NewString()); err == nil {
		t.Fatal("expired code accepted")
	}
}
func TestSignupAttemptAndSendLimitsSurviveNewRequests(t *testing.T) {
	s, now := signupFixture(t)
	ctx := context.Background()
	email := "person@example.test"
	sent, err := s.Request(ctx, email, "192.0.2.4", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	code := signupCode(t, s, sent.ChallengeID)
	wrong := "000000"
	if wrong == code {
		wrong = "111111"
	}
	for range 5 {
		if _, err := s.Verify(ctx, email, sent.ChallengeID, wrong, "192.0.2.4", uuid.NewString()); err == nil {
			t.Fatal("wrong code accepted")
		}
	}
	if _, err := s.Verify(ctx, email, sent.ChallengeID, code, "192.0.2.4", uuid.NewString()); err == nil {
		t.Fatal("exhausted challenge accepted")
	}
	var challenge SignupChallenge
	_ = s.DB.First(&challenge, "id = ?", sent.ChallengeID).Error
	if challenge.Attempts != 5 || challenge.State != "exhausted" {
		t.Fatal("failed attempts rolled back")
	}
	for range 4 {
		*now = now.Add(61 * time.Second)
		if _, err := s.Request(ctx, email, "192.0.2.4", uuid.NewString()); err != nil {
			t.Fatal(err)
		}
	}
	*now = now.Add(61 * time.Second)
	if _, err := s.Request(ctx, email, "192.0.2.4", uuid.NewString()); err == nil {
		t.Fatal("email send quota reset by a new challenge")
	}
}
func TestSignupWrongCodeRetryAndProofResponseAreIdempotent(t *testing.T) {
	s, _ := signupFixture(t)
	ctx := context.Background()
	email := "person@example.test"
	sent, err := s.Request(ctx, email, "192.0.2.4", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	code := signupCode(t, s, sent.ChallengeID)
	wrong := "000000"
	if wrong == code {
		wrong = "111111"
	}
	key := uuid.NewString()
	for range 3 {
		if _, err := s.Verify(ctx, email, sent.ChallengeID, wrong, "192.0.2.4", key); err == nil {
			t.Fatal("wrong code accepted")
		}
	}
	var challenge SignupChallenge
	_ = s.DB.First(&challenge, "id = ?", sent.ChallengeID).Error
	if challenge.Attempts != 1 {
		t.Fatal("network retry consumed another attempt")
	}
	key = uuid.NewString()
	proof, err := s.Verify(ctx, email, sent.ChallengeID, code, "192.0.2.4", key)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := s.Verify(ctx, email, sent.ChallengeID, code, "192.0.2.4", key)
	if err != nil || proof.Token != retry.Token {
		t.Fatal("proof response was not replayed")
	}
	var caches []signupIdempotency
	_ = s.DB.Find(&caches).Error
	for _, cache := range caches {
		if strings.Contains(cache.Response, proof.Token) || strings.Contains(cache.Response, "signup_verification_token") {
			t.Fatal("proof response stored in plaintext")
		}
	}
}
func TestSignupOutboxFailureRollsBackAccountAndProofConsumption(t *testing.T) {
	s, _ := signupFixture(t)
	ctx := context.Background()
	email := "person@example.test"
	sent, proof := verifiedSignup(t, s, email)
	if err := s.DB.Migrator().DropTable(&outbox.Record{}); err != nil {
		t.Fatal(err)
	}
	user := Auth{FirstName: "Test", LastName: "Person", Email: email, Password: "synthetic-password"}
	key := uuid.NewString()
	request := map[string]string{"email": email, "password": user.Password}
	if _, err := s.Create(ctx, user, proof.Token, key, request); err == nil {
		t.Fatal("signup succeeded without durable welcome intent")
	}
	var count int64
	_ = s.DB.Model(&Auth{}).Count(&count).Error
	var challenge SignupChallenge
	_ = s.DB.First(&challenge, "id = ?", sent.ChallengeID).Error
	if count != 0 || challenge.State != "verified" {
		t.Fatal("failed transaction consumed proof or created account")
	}
	if err := s.DB.AutoMigrate(&outbox.Record{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, user, proof.Token, key, request); err != nil {
		t.Fatal("failed signup was not retryable", err)
	}
}
func TestConcurrentSignupAndExistingAccountProtection(t *testing.T) {
	s, _ := signupFixture(t)
	ctx := context.Background()
	email := "person@example.test"
	_, proof := verifiedSignup(t, s, email)
	user := Auth{FirstName: "Test", LastName: "Person", Email: email, Password: "synthetic-password"}
	key := uuid.NewString()
	var group sync.WaitGroup
	ids := make(chan int, 2)
	failures := make(chan error, 2)
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			created, err := s.Create(ctx, user, proof.Token, key, map[string]string{"email": email, "password": user.Password})
			if err != nil {
				failures <- err
				return
			}
			ids <- created.ID
		}()
	}
	group.Wait()
	close(ids)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	id := 0
	for next := range ids {
		if id != 0 && id != next {
			t.Fatal("duplicate accounts created")
		}
		id = next
	}
	if _, err := (&AuthService{DB: s.DB, RequireVerified: true}).CreateUser(user); !errors.Is(err, ErrSignupVerification) {
		t.Fatal("required verification was bypassed through CreateUser")
	}
}
