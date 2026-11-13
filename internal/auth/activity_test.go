package auth

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"nordik-drive-api/internal/auditoutbox"
	"nordik-drive-api/internal/outbox"
	"nordik-drive-api/internal/util"
)

func TestPasswordResetAndOTPRollBackWhenAuditIntentCannotPersist(t *testing.T) {
	verifier, _ := signupFixture(t)
	db := verifier.DB
	if err := db.AutoMigrate(&OTP{}); err != nil {
		t.Fatal(err)
	}
	original, err := util.HashPassword("before-synthetic-password")
	if err != nil {
		t.Fatal(err)
	}
	user := Auth{FirstName: "Existing", LastName: "User", Email: "existing@example.test", Password: original, Role: "User"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	service := &AuthService{DB: db, Outbox: verifier.Outbox, Audit: &auditoutbox.Writer{Producer: "identity"}}
	if _, _, err := service.SendOTP(user.Email); err == nil {
		t.Fatal("OTP committed without audit intent")
	}
	var count int64
	if err := db.Model(&OTP{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatal("OTP did not roll back", err)
	}
	if err := db.Model(&outbox.Record{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatal("email intent escaped rollback", err)
	}
	if err := db.AutoMigrate(&auditoutbox.Record{}); err != nil {
		t.Fatal(err)
	}
	_, code, err := service.SendOTP(user.Email)
	if err != nil {
		t.Fatal(err)
	}
	var event auditoutbox.Record
	if err := db.First(&event).Error; err != nil || strings.Contains(event.Payload, code) {
		t.Fatal("OTP was exposed in activity", err)
	}
	if err := db.Migrator().DropTable(&auditoutbox.Record{}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ResetPassword(user.Email, code, "after-synthetic-password"); err == nil {
		t.Fatal("password changed without audit intent")
	}
	var unchanged Auth
	if err := db.First(&unchanged, user.ID).Error; err != nil || unchanged.Password != original {
		t.Fatal("password did not roll back", err)
	}
	if err := db.Model(&outbox.Record{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatal("failed reset committed email", err)
	}
	if err := db.AutoMigrate(&auditoutbox.Record{}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ResetPassword(user.Email, code, "after-synthetic-password"); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&unchanged, user.ID).Error; err != nil || util.VerifyPassword("after-synthetic-password", unchanged.Password) != nil {
		t.Fatal("retry did not complete reset", err)
	}
}

func TestVerifiedSignupAuditFailurePreservesProofForRetry(t *testing.T) {
	verifier, _ := signupFixture(t)
	sent, proof := verifiedSignup(t, verifier, "signup@example.test")
	verifier.Audit = &auditoutbox.Writer{Producer: "identity"}
	key := uuid.NewString()
	user := Auth{FirstName: "Signup", LastName: "User", Email: "signup@example.test", Password: "synthetic-password"}
	request := map[string]string{"email": user.Email, "password": user.Password}
	if _, err := verifier.Create(context.Background(), user, proof.Token, key, request); err == nil {
		t.Fatal("signup committed without audit intent")
	}
	var count int64
	var challenge SignupChallenge
	if err := verifier.DB.Model(&Auth{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatal("account escaped audit rollback", err)
	}
	if err := verifier.DB.First(&challenge, "id = ?", sent.ChallengeID).Error; err != nil || challenge.State != "verified" {
		t.Fatal("proof consumed on failed audit", err)
	}
	if err := verifier.DB.AutoMigrate(&auditoutbox.Record{}); err != nil {
		t.Fatal(err)
	}
	result, err := verifier.Create(context.Background(), user, proof.Token, key, request)
	if err != nil || result == nil {
		t.Fatal("same verified signup could not retry", err)
	}
	var event auditoutbox.Record
	if err := verifier.DB.First(&event).Error; err != nil || strings.Contains(event.Payload, proof.Token) || strings.Contains(event.Payload, user.Password) {
		t.Fatal("signup proof or password exposed in audit", err)
	}
}
