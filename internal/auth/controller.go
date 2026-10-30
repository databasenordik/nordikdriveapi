package auth

import (
	"fmt"
	"net/http"
	"nordik-drive-api/config"
	"nordik-drive-api/internal/logs"
	"nordik-drive-api/internal/session"
	"nordik-drive-api/internal/util"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/lib/pq"
)

type AuthController struct {
	AuthService AuthServicePort
	LS          LogServicePort
}

var hash = util.HashPassword

func (ac *AuthController) SignUp(c *gin.Context) {
	var req struct {
		FirstName string   `json:"firstname" binding:"required"`
		LastName  string   `json:"lastname" binding:"required"`
		Email     string   `json:"email" binding:"required,email"`
		Password  string   `json:"password" binding:"required,min=6"`
		Community []string `json:"community"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	password, err := hash(req.Password)

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	user := Auth{
		FirstName: req.FirstName,
		LastName:  req.LastName,
		Email:     req.Email,
		Password:  password,
	}

	if len(req.Community) > 0 {
		user.Community = pq.StringArray(req.Community)
	}

	newuser, err := ac.AuthService.CreateUser(user)

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	uid := uint(newuser.ID)

	log := logs.SystemLog{
		Level:       "INFO",
		Service:     "auth",
		Action:      "SIGNUP",
		Message:     fmt.Sprintf("Account created with email %s", user.Email),
		UserID:      &uid,
		Communities: user.Community,
	}

	if err := ac.LS.Log(log, map[string]any{
		"email": user.Email, "firstname": user.FirstName, "lastname": user.LastName,
	}); err != nil {
		fmt.Printf("Failed to insert log: %v\n", err)
	}

	c.JSON(http.StatusCreated, gin.H{
		"message": "User created successfully",
		"user": map[string]interface{}{
			"id":        newuser.ID,
			"firstname": newuser.FirstName,
			"lastname":  newuser.LastName,
			"email":     newuser.Email,
			"community": newuser.Community,
		},
	})
}

type LoginRequest struct {
	Email      string `json:"email" binding:"required,email"`
	Password   string `json:"password" binding:"required"`
	RememberMe bool   `json:"rememberMe"`
}

func (ac *AuthController) Login(c *gin.Context) {
	var req LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	user, err := ac.AuthService.GetUser(req.Email)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Oops! We couldn’t log you in. Please check your username and password and try again."})
		return
	}

	if err := util.VerifyPassword(req.Password, user.Password); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Oops! We couldn’t log you in. Please check your username and password and try again."})
		return
	}

	cfg := config.LoadConfig()

	// Short-lived access token
	accessExp := time.Now().Add(15 * time.Minute)
	accessToken := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id":     user.ID,
		"communities": user.Community,
		"exp":         accessExp.Unix(),
	})
	accessTokenString, _ := accessToken.SignedString([]byte(cfg.JWTSecret))

	// Refresh token (longer if RememberMe is true)
	refreshDuration := 24 * time.Hour
	if req.RememberMe {
		refreshDuration = 30 * 24 * time.Hour
	}
	refreshExp := time.Now().Add(refreshDuration)
	refreshToken := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id":     user.ID,
		"communities": user.Community,
		"exp":         refreshExp.Unix(),
	})
	refreshTokenString, _ := refreshToken.SignedString([]byte(cfg.JWTSecret))

	httpOnly := true
	secure := true // Must be true for HTTPS
	accessCookie := &http.Cookie{
		Name:     "access_token",
		Value:    accessTokenString,
		Path:     "/",
		HttpOnly: httpOnly,
		Secure:   secure,
		SameSite: http.SameSiteNoneMode, // required for cross-site cookies
	}
	refreshCookie := &http.Cookie{
		Name:     "refresh_token",
		Value:    refreshTokenString,
		Path:     "/",
		HttpOnly: httpOnly,
		Secure:   secure,
		SameSite: http.SameSiteNoneMode,
	}
	http.SetCookie(c.Writer, accessCookie)
	http.SetCookie(c.Writer, refreshCookie)

	uid := uint(user.ID)

	log := logs.SystemLog{
		Level:       "INFO",
		Service:     "auth",
		Action:      "LOGIN",
		Message:     fmt.Sprintf("User logged in with email: %s", user.Email),
		UserID:      &uid,
		Communities: user.Community,
	}

	if ac.LS != nil {
		if err := ac.LS.Log(log, map[string]any{"email": req.Email, "rememberMe": req.RememberMe}); err != nil {
			fmt.Printf("Failed to insert log: %v\n", err)
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "Login successful",
		"data": LoginResponse{
			ID:        user.ID,
			FirstName: user.FirstName,
			LastName:  user.LastName,
			Email:     user.Email,
			Role:      user.Role,
			Community: user.Community,
		},
	})
}

func (ac *AuthController) Logout(c *gin.Context) {
	accessCookie := &http.Cookie{
		Name:     "access_token",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteNoneMode,
		MaxAge:   -1,
	}
	refreshCookie := &http.Cookie{
		Name:     "refresh_token",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteNoneMode,
		MaxAge:   -1,
	}
	http.SetCookie(c.Writer, accessCookie)
	http.SetCookie(c.Writer, refreshCookie)

	c.JSON(http.StatusOK, gin.H{"message": "Logged out"})
}

func (ac *AuthController) Me(c *gin.Context) {
	cfg := config.LoadConfig()

	accessToken, err := c.Cookie("access_token")
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Missing access token"})
		return
	}

	claims, err := session.Validate(accessToken, cfg.JWTSecret)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired token"})
		return
	}

	user, err := ac.AuthService.GetUserByID(claims.UserID)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "User not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"user": LoginResponse{
			ID:        user.ID,
			FirstName: user.FirstName,
			LastName:  user.LastName,
			Email:     user.Email,
			Role:      user.Role,
			Community: user.Community,
		},
	})
}

// Refresh endpoint to generate new access token
func (ac *AuthController) Refresh(c *gin.Context) {
	cfg := config.LoadConfig()

	refreshToken, err := c.Cookie("refresh_token")
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Missing refresh token"})
		return
	}

	claims, err := session.Validate(refreshToken, cfg.JWTSecret)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid refresh token"})
		return
	}

	// Generate new access token
	accessExp := time.Now().Add(15 * time.Minute)
	accessToken := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id":     claims.UserID,
		"communities": claims.Communities,
		"exp":         accessExp.Unix(),
	})
	accessTokenString, _ := accessToken.SignedString([]byte(cfg.JWTSecret))

	httpOnly := true
	secure := true // Must be true for HTTPS
	accessCookie := &http.Cookie{
		Name:     "access_token",
		Value:    accessTokenString,
		Path:     "/",
		HttpOnly: httpOnly,
		Secure:   secure,
		SameSite: http.SameSiteNoneMode, // required for cross-site cookies
	}

	http.SetCookie(c.Writer, accessCookie)

	c.JSON(http.StatusOK, gin.H{"message": "Access token refreshed"})
}

func (ac *AuthController) GetUsers(c *gin.Context) {

	userIDVal, exists := c.Get("userID")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user ID not found"})
		return
	}

	_, ok := userIDVal.(float64)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}

	users, err := ac.AuthService.GetAllUsers()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "Fetched all users successfully",
		"users":   users,
	})
}

func (ac *AuthController) VerifyPassword(c *gin.Context) {
	var req VerifyPasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	userIDVal, exists := c.Get("userID")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user ID not found"})
		return
	}

	userID, ok := userIDVal.(float64)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
		return
	}

	user, err := ac.AuthService.GetUserByID(int(userID))
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid credentials"})
		return
	}

	if err := util.VerifyPassword(req.Password, user.Password); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid credentials"})
		return
	}

	uid := uint(user.ID)

	logs := logs.SystemLog{
		Level:       "INFO",
		Service:     "auth",
		Action:      "PASSWORD_VERIFICATION",
		Message:     fmt.Sprintf("Verified password for file access by : %s", user.Email),
		UserID:      &uid,
		Communities: user.Community,
	}

	if err := ac.LS.Log(logs, map[string]any{"email": user.Email}); err != nil {
		fmt.Printf("Failed to insert log: %v\n", err)
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "Password verified successfully",
		"data": VerifyPasswordResponse{
			Match: true,
		},
	})
}

func (ac *AuthController) SendOTP(c *gin.Context) {
	var req SendOTPRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	user, _, err := ac.AuthService.SendOTP(req.Email)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if user == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user not found"})
		return
	}

	uid := uint(user.ID)

	logs := logs.SystemLog{
		Level:       "INFO",
		Service:     "auth",
		Action:      "SEND_OTP",
		Message:     fmt.Sprintf("Sent OTP to email: %s", req.Email),
		UserID:      &uid,
		Communities: user.Community,
	}

	if err := ac.LS.Log(logs, map[string]any{"email": req.Email}); err != nil {
		fmt.Printf("Failed to insert log: %v\n", err)
	}

	c.JSON(http.StatusOK, gin.H{"message": "OTP sent successfully"})
}

// POST /api/user/reset-password
func (ac *AuthController) ResetPassword(c *gin.Context) {
	var req ResetPasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	user, err := ac.AuthService.ResetPassword(req.Email, req.OTP, req.Password)

	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if user == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user not found"})
		return
	}

	uid := uint(user.ID)
	logs := logs.SystemLog{
		Level:       "INFO",
		Service:     "auth",
		Action:      "RESET_PASSWORD",
		Message:     fmt.Sprintf("Password reset for email: %s", req.Email),
		UserID:      &uid,
		Communities: user.Community,
	}

	if err := ac.LS.Log(logs, map[string]any{"email": req.Email}); err != nil {
		fmt.Printf("Failed to insert log: %v\n", err)
	}

	c.JSON(http.StatusOK, gin.H{"message": "Password reset successfully"})
}

// func (ac *AuthController) GetAllRequests(c *gin.Context) {
// 	userIDVal, exists := c.Get("userID")
// 	if !exists {
// 		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
// 		return
// 	}
// 	userID := int(userIDVal.(float64))

// 	accessRequests, err := ac.AuthService.GetAccessRequests(userID)
// 	if err != nil {
// 		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
// 		return
// 	}

// 	if accessRequests == nil {
// 		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
// 		return
// 	}

// 	c.JSON(http.StatusOK, gin.H{
// 		"requests": accessRequests,
// 	})
// }

// func (ac *AuthController) GetAllAccessByUser(c *gin.Context) {
// 	userIDVal, exists := c.Get("userID")
// 	if !exists {
// 		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
// 		return
// 	}
// 	userID := int(userIDVal.(float64))

// 	accessRequests, err := ac.AuthService.GetUserAccess(userID)
// 	if err != nil {
// 		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
// 		return
// 	}

// 	c.JSON(http.StatusOK, gin.H{
// 		"access": accessRequests,
// 	})
// }

// func (ac *AuthController) ProcessRequests(c *gin.Context) {
// 	var requests []RequestAction

// 	if err := c.ShouldBindJSON(&requests); err != nil {
// 		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
// 		return
// 	}

// 	if err := ac.AuthService.ProcessRequests(requests); err != nil {
// 		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
// 		return
// 	}

// 	c.JSON(http.StatusOK, gin.H{"message": "Requests processed successfully"})
// }
