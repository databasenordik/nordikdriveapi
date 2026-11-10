package auth

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"nordik-drive-api/internal/requestscope"
)

func signupFailure(c *gin.Context, err error) {
	var failure *SignupError
	if errors.As(err, &failure) {
		if failure.RetryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(failure.RetryAfter))
		}
		c.JSON(failure.Status, gin.H{"error": failure.Message})
		return
	}
	if errors.Is(err, ErrSignupVerification) {
		c.JSON(http.StatusBadRequest, gin.H{"error": ErrSignupVerification.Error()})
		return
	}
	c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Signup verification is temporarily unavailable"})
}
func RegisterSignupOTP(r *gin.Engine, service *AuthService) {
	request := func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 8*1024)
		c.Header("Cache-Control", "no-store")
		if service.Signup == nil {
			c.JSON(503, gin.H{"error": "Signup verification is not configured"})
			return
		}
		var body struct {
			Email string `json:"email" binding:"required,email"`
		}
		if c.ShouldBindJSON(&body) != nil {
			c.JSON(400, gin.H{"error": "Enter a valid email address"})
			return
		}
		verifier := *service.Signup
		verifier.DB = service.DB.WithContext(c.Request.Context())
		result, err := verifier.Request(c.Request.Context(), body.Email, c.GetHeader("X-Nordik-Client-IP"), c.GetHeader("Idempotency-Key"))
		if err != nil {
			signupFailure(c, err)
			return
		}
		c.JSON(202, gin.H{"challenge_id": result.ChallengeID, "expires_at": result.ExpiresAt, "resend_after": result.ResendAfter})
	}
	verify := func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 8*1024)
		c.Header("Cache-Control", "no-store")
		if service.Signup == nil {
			c.JSON(503, gin.H{"error": "Signup verification is not configured"})
			return
		}
		var body struct {
			Email string `json:"email" binding:"required,email"`
			ID    string `json:"challenge_id" binding:"required"`
			OTP   string `json:"otp" binding:"required,len=6"`
		}
		if c.ShouldBindJSON(&body) != nil {
			c.JSON(400, gin.H{"error": "Enter the email, challenge and six-digit code"})
			return
		}
		result, err := service.Signup.Verify(c.Request.Context(), body.Email, body.ID, body.OTP, c.GetHeader("X-Nordik-Client-IP"), c.GetHeader("Idempotency-Key"))
		if err != nil {
			signupFailure(c, err)
			return
		}
		c.JSON(200, gin.H{"signup_verification_token": result.Token, "expires_at": result.ExpiresAt})
	}
	r.POST("/api/user/signup/otp/request", request)
	r.POST("/api/user/signup/otp/verify", verify)
}
func (s *AuthService) SignupVerificationRequired() bool { return s.RequireVerified }
func (s *AuthService) CreateVerifiedUser(user Auth, proof, key string, request any) (*Auth, error) {
	if s.Signup == nil {
		return nil, ErrSignupVerification
	}
	copy := *s.Signup
	copy.DB = s.DB
	return copy.Create(requestscope.Context(s.DB), user, proof, key, request)
}
