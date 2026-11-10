package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"nordik-drive-api/internal/notificationcontract"
	"nordik-drive-api/internal/outbox"
	"nordik-drive-api/internal/util"
)

var ErrSignupVerification = errors.New("valid email verification is required")

type SignupError struct {
	Status     int
	Message    string
	RetryAfter int
}

func (e *SignupError) Error() string { return e.Message }

type SignupChallenge struct {
	ID             string `gorm:"primaryKey"`
	Email          string
	Verifier       string
	KeyVersion     string
	Attempts       int
	State          string
	ExpiresAt      time.Time
	ResendAfter    time.Time
	ProofVerifier  string
	ProofExpiresAt *time.Time
	CreatedAt      time.Time
}

func (SignupChallenge) TableName() string { return "signup_otp_challenges" }

type signupLimit struct {
	Key       string `gorm:"primaryKey"`
	Count     int
	ExpiresAt time.Time
}

func (signupLimit) TableName() string { return "signup_otp_limits" }

type signupIdempotency struct {
	Key                               string `gorm:"primaryKey"`
	Fingerprint, Response, KeyVersion string
	ExpiresAt                         time.Time
}

func (signupIdempotency) TableName() string { return "signup_idempotency" }

type SignupResult struct {
	ChallengeID string       `json:"challenge_id,omitempty"`
	ExpiresAt   time.Time    `json:"expires_at,omitempty"`
	ResendAfter time.Time    `json:"resend_after,omitempty"`
	Token       string       `json:"signup_verification_token,omitempty"`
	User        *Auth        `json:"user,omitempty"`
	Failure     *SignupError `json:"failure,omitempty"`
	OutboxID    string       `json:"-"`
}
type SignupVerifier struct {
	DB     *gorm.DB
	Outbox *outbox.Writer
	Keys   map[string][]byte
	Active string
	Remote outbox.Remote
	Clock  func() time.Time
}

func NewSignupVerifier(db *gorm.DB, writer *outbox.Writer, encoded, active string) (*SignupVerifier, error) {
	codec, err := notificationcontract.NewCodec(encoded, active)
	if err != nil || writer == nil || writer.Codec == nil {
		return nil, errors.New("signup verification requires purpose-specific keys and an outbox")
	}
	for _, authKey := range codec.Keys {
		for _, payloadKey := range writer.Codec.Keys {
			if hmac.Equal(authKey, payloadKey) {
				return nil, errors.New("signup verification and notification payload keys must be distinct")
			}
		}
	}
	return &SignupVerifier{DB: db, Outbox: writer, Keys: codec.Keys, Active: active}, nil
}
func keyed(key []byte, purpose, value string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(purpose + "\x00" + value))
	return hex.EncodeToString(m.Sum(nil))
}
func (s *SignupVerifier) protect(keyVersion, context, value string) (string, error) {
	derived, _ := hex.DecodeString(keyed(s.Keys[keyVersion], "signup-cache-key", "v1"))
	block, err := aes.NewCipher(derived)
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(aead.Seal(nonce, nonce, []byte(value), []byte(context))), nil
}
func (s *SignupVerifier) unprotect(keyVersion, context, value string) (string, error) {
	if len(s.Keys[keyVersion]) != 32 {
		return "", ErrSignupVerification
	}
	derived, _ := hex.DecodeString(keyed(s.Keys[keyVersion], "signup-cache-key", "v1"))
	block, err := aes.NewCipher(derived)
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	b, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(b) < aead.NonceSize()+aead.Overhead() {
		return "", ErrSignupVerification
	}
	plain, err := aead.Open(nil, b[:aead.NonceSize()], b[aead.NonceSize():], []byte(context))
	return string(plain), err
}
func (s *SignupVerifier) dbNow(tx *gorm.DB) (time.Time, error) {
	if s.Clock != nil {
		return s.Clock().UTC(), nil
	}
	if tx.Dialector.Name() != "postgres" {
		return time.Now().UTC(), nil
	}
	var now time.Time
	err := tx.Raw("SELECT clock_timestamp()").Scan(&now).Error
	return now.UTC(), err
}
func signupLocked(tx *gorm.DB) *gorm.DB {
	if tx.Dialector.Name() == "postgres" {
		return tx.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	return tx
}
func signupEmail(email string) (string, error) {
	email = strings.TrimSpace(email)
	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email || len(email) > 100 || strings.ContainsAny(email, "\r\n") {
		return "", &SignupError{Status: 400, Message: "Enter a valid email address"}
	}
	return email, nil
}

// execute serializes duplicate requests and commits negative verification results
// too, so failed-code counters cannot be undone by returning an HTTP error.
func (s *SignupVerifier) execute(ctx context.Context, operation, key string, input any, fn func(*gorm.DB, time.Time) (SignupResult, error)) (SignupResult, error) {
	var result SignupResult
	if len(key) < 16 || len(key) > 128 {
		return result, &SignupError{Status: 400, Message: "A unique Idempotency-Key is required"}
	}
	request, _ := json.Marshal(input)
	digest := sha256.Sum256([]byte(operation + "\x00" + key))
	cacheKey := hex.EncodeToString(digest[:])
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now, err := s.dbNow(tx)
		if err != nil {
			return err
		}
		cache := signupIdempotency{Key: cacheKey, Fingerprint: keyed(s.Keys[s.Active], "signup-request", string(request)), KeyVersion: s.Active, ExpiresAt: now.Add(24 * time.Hour)}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&cache).Error; err != nil {
			return err
		}
		if err := signupLocked(tx).First(&cache, "key = ?", cacheKey).Error; err != nil {
			return err
		}
		if len(s.Keys[cache.KeyVersion]) != 32 || !hmac.Equal([]byte(cache.Fingerprint), []byte(keyed(s.Keys[cache.KeyVersion], "signup-request", string(request)))) || !cache.ExpiresAt.After(now) {
			return &SignupError{Status: 409, Message: "Use a new key for a different or expired request"}
		}
		if cache.Response != "" {
			plain, err := s.unprotect(cache.KeyVersion, cache.Key+cache.Fingerprint, cache.Response)
			if err != nil {
				return err
			}
			return json.Unmarshal([]byte(plain), &result)
		}
		result, err = fn(tx, now)
		if err != nil {
			return err
		}
		b, err := json.Marshal(result)
		if err != nil {
			return err
		}
		cache.Response, err = s.protect(cache.KeyVersion, cache.Key+cache.Fingerprint, string(b))
		if err != nil {
			return err
		}
		return tx.Model(&cache).Update("response", cache.Response).Error
	})
	if err == nil && result.Failure != nil {
		return result, result.Failure
	}
	return result, err
}
func (s *SignupVerifier) limit(tx *gorm.DB, kind, subject string, maximum int, window time.Duration, now time.Time) error {
	start := now.Truncate(window)
	key := keyed(s.Keys[s.Active], "signup-limit", fmt.Sprintf("%s:%s:%d", kind, subject, start.Unix()))
	row := signupLimit{Key: key, Count: 1, ExpiresAt: start.Add(window)}
	query := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "key"}}, DoUpdates: clause.Assignments(map[string]any{"count": gorm.Expr("signup_otp_limits.count + 1")}), Where: clause.Where{Exprs: []clause.Expression{clause.Lt{Column: clause.Column{Table: "signup_otp_limits", Name: "count"}, Value: maximum}}}}).Create(&row)
	if query.Error != nil {
		return query.Error
	}
	if query.RowsAffected == 0 {
		return &SignupError{Status: 429, Message: "Too many verification requests. Please try later.", RetryAfter: max(1, int(start.Add(window).Sub(now).Seconds()))}
	}
	return nil
}
func (s *SignupVerifier) lockEmail(tx *gorm.DB, email string) error {
	if tx.Dialector.Name() != "postgres" {
		return nil
	}
	hash := sha256.Sum256([]byte("signup:" + email))
	return tx.Exec("SELECT pg_advisory_xact_lock(?)", int64(uint64(hash[0])<<56|uint64(hash[1])<<48|uint64(hash[2])<<40|uint64(hash[3])<<32|uint64(hash[4])<<24|uint64(hash[5])<<16|uint64(hash[6])<<8|uint64(hash[7]))).Error
}
func (s *SignupVerifier) Request(ctx context.Context, email, ip, key string) (SignupResult, error) {
	email, err := signupEmail(email)
	if err != nil {
		return SignupResult{}, err
	}
	if net.ParseIP(ip) == nil {
		return SignupResult{}, &SignupError{Status: 400, Message: "Client address unavailable"}
	}
	result, err := s.execute(ctx, "request", key, map[string]string{"email": email}, func(tx *gorm.DB, now time.Time) (SignupResult, error) {
		if err := s.lockEmail(tx, email); err != nil {
			return SignupResult{}, err
		}
		var previous SignupChallenge
		found := tx.Where("email = ?", email).Order("created_at DESC").First(&previous).Error
		if found != nil && !errors.Is(found, gorm.ErrRecordNotFound) {
			return SignupResult{}, found
		}
		if found == nil && previous.ResendAfter.After(now) {
			return SignupResult{}, &SignupError{Status: 429, Message: "Please wait before requesting another code", RetryAfter: max(1, int(previous.ResendAfter.Sub(now).Seconds()))}
		}
		for _, entry := range []struct {
			k, subject string
			limit      int
			window     time.Duration
		}{{"send-email-hour", email, 5, time.Hour}, {"send-email-day", email, 20, 24 * time.Hour}, {"send-ip", ip, 50, 15 * time.Minute}} {
			if err := s.limit(tx, entry.k, entry.subject, entry.limit, entry.window, now); err != nil {
				return SignupResult{}, err
			}
		}
		if err := tx.Model(&SignupChallenge{}).Where("email = ? AND state IN ?", email, []string{"pending", "verified"}).Updates(map[string]any{"state": "superseded", "proof_verifier": ""}).Error; err != nil {
			return SignupResult{}, err
		}
		number, err := rand.Int(rand.Reader, big.NewInt(1000000))
		if err != nil {
			return SignupResult{}, err
		}
		code := fmt.Sprintf("%06d", number.Int64())
		challenge := SignupChallenge{ID: uuid.NewString(), Email: email, KeyVersion: s.Active, State: "pending", CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute), ResendAfter: now.Add(time.Minute)}
		challenge.Verifier = keyed(s.Keys[s.Active], "signup-otp", challenge.ID+"\x00"+email+"\x00"+code)
		if err := tx.Create(&challenge).Error; err != nil {
			return SignupResult{}, err
		}
		id, err := s.Outbox.Put(tx, notificationcontract.Command{Producer: "identity", Key: "signup-otp:" + challenge.ID, Kind: "otp", CreatedAt: now, Deadline: challenge.ExpiresAt, Supersession: keyed(s.Keys[s.Active], "signup-supersession", email), Sequence: now.UnixMicro()}, notificationcontract.Payload{To: email, Subject: "Verify your Nordik Drive email", HTML: fmt.Sprintf("<p>Your signup verification code is <strong>%s</strong>.</p><p>It expires in 10 minutes. Use the latest code you requested.</p>", code)})
		return SignupResult{ChallengeID: challenge.ID, ExpiresAt: challenge.ExpiresAt, ResendAfter: challenge.ResendAfter, OutboxID: id}, err
	})
	if err == nil && result.OutboxID != "" && s.Remote != nil {
		s.fastDispatch(ctx, result.OutboxID)
	}
	return result, err
}
func (s *SignupVerifier) fastDispatch(ctx context.Context, id string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var record outbox.Record
	if s.DB.WithContext(ctx).First(&record, "id = ?", id).Error != nil {
		return
	}
	var command notificationcontract.Command
	if json.Unmarshal([]byte(record.Command), &command) != nil {
		return
	}
	if delivery, err := s.Remote.Accept(ctx, command); err == nil {
		_ = s.DB.WithContext(ctx).Model(&record).Where("state = ?", "pending").Updates(map[string]any{"state": "accepted", "delivery_id": delivery, "lease_until": nil}).Error
	}
}
func (s *SignupVerifier) Verify(ctx context.Context, email, id, code, ip, key string) (SignupResult, error) {
	email, err := signupEmail(email)
	if err != nil {
		return SignupResult{}, err
	}
	if net.ParseIP(ip) == nil {
		return SignupResult{}, &SignupError{Status: 400, Message: "Client address unavailable"}
	}
	return s.execute(ctx, "verify", key, map[string]string{"email": email, "challenge_id": id, "otp": code}, func(tx *gorm.DB, now time.Time) (SignupResult, error) {
		if err := s.lockEmail(tx, email); err != nil {
			return SignupResult{}, err
		}
		for _, entry := range []struct {
			k, subject string
			limit      int
		}{{"verify-email", email, 10}, {"verify-ip", ip, 100}} {
			if err := s.limit(tx, entry.k, entry.subject, entry.limit, 15*time.Minute, now); err != nil {
				return SignupResult{}, err
			}
		}
		invalid := SignupResult{Failure: &SignupError{Status: 400, Message: "The code is invalid or expired. Request a new code if needed."}}
		var challenge SignupChallenge
		err := signupLocked(tx).First(&challenge, "id = ? AND email = ?", id, email).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return invalid, nil
		}
		if err != nil {
			return SignupResult{}, err
		}
		if challenge.State != "pending" || challenge.Attempts >= 5 || !challenge.ExpiresAt.After(now) {
			return invalid, nil
		}
		challenge.Attempts++
		candidate := keyed(s.Keys[challenge.KeyVersion], "signup-otp", challenge.ID+"\x00"+email+"\x00"+code)
		if len(s.Keys[challenge.KeyVersion]) != 32 || len(code) != 6 || !hmac.Equal([]byte(challenge.Verifier), []byte(candidate)) {
			if challenge.Attempts >= 5 {
				challenge.State = "exhausted"
			}
			return invalid, tx.Save(&challenge).Error
		}
		random := make([]byte, 32)
		if _, err := rand.Read(random); err != nil {
			return SignupResult{}, err
		}
		token := challenge.ID + "." + base64.RawURLEncoding.EncodeToString(random)
		proofExpiry := now.Add(10 * time.Minute)
		challenge.ProofExpiresAt = &proofExpiry
		challenge.State = "verified"
		challenge.Verifier = ""
		challenge.ProofVerifier = keyed(s.Keys[challenge.KeyVersion], "signup-proof", email+"\x00"+token)
		return SignupResult{Token: token, ExpiresAt: proofExpiry}, tx.Save(&challenge).Error
	})
}
func (s *SignupVerifier) Create(ctx context.Context, user Auth, proof, key string, request any) (*Auth, error) {
	result, err := s.execute(ctx, "signup", key, request, func(tx *gorm.DB, now time.Time) (SignupResult, error) {
		parts := strings.Split(proof, ".")
		if len(parts) != 2 {
			return SignupResult{}, ErrSignupVerification
		}
		var challenge SignupChallenge
		if err := signupLocked(tx).First(&challenge, "id = ? AND email = ?", parts[0], user.Email).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return SignupResult{}, ErrSignupVerification
			}
			return SignupResult{}, err
		}
		if len(s.Keys[challenge.KeyVersion]) != 32 || challenge.State != "verified" || challenge.ProofExpiresAt == nil || !challenge.ProofExpiresAt.After(now) || !hmac.Equal([]byte(challenge.ProofVerifier), []byte(keyed(s.Keys[challenge.KeyVersion], "signup-proof", user.Email+"\x00"+proof))) {
			return SignupResult{}, ErrSignupVerification
		}
		if len(user.Password) < 6 {
			return SignupResult{}, &SignupError{Status: 400, Message: "Password must have at least six characters"}
		}
		hashed, err := util.HashPassword(user.Password)
		if err != nil {
			return SignupResult{}, err
		}
		user.Password = hashed
		if user.Role == "" {
			user.Role = "User"
		}
		user.EmailVerifiedAt = &now
		if err := tx.Create(&user).Error; err != nil {
			if strings.Contains(err.Error(), "duplicate key") || strings.Contains(strings.ToLower(err.Error()), "unique constraint") {
				return SignupResult{}, &SignupError{Status: 409, Message: "An account with this email already exists. Please log in."}
			}
			return SignupResult{}, err
		}
		if err := s.Outbox.Send(tx, "identity", fmt.Sprintf("signup-welcome:%d", user.ID), []string{user.Email}, "Your database access account has been created", BuildSignupEmailBody(user.FirstName, user.LastName, user.Email)); err != nil {
			return SignupResult{}, err
		}
		if err := tx.Model(&challenge).Updates(map[string]any{"state": "consumed", "proof_verifier": ""}).Error; err != nil {
			return SignupResult{}, err
		}
		return SignupResult{User: &user}, nil
	})
	return result.User, err
}
