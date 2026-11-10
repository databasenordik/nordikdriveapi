package auth

import (
	"nordik-drive-api/internal/logs"
	"nordik-drive-api/internal/middlewares"

	"github.com/gin-gonic/gin"
)

func RegisterRoutes(r *gin.Engine, as *AuthService, ls *logs.LogService) {
	RegisterSignupOTP(r, as)
	controller := &AuthController{AuthService: as, LS: ls}

	userGroup := r.Group("/api/user")

	{
		userGroup.POST("/login", controller.Login)
		userGroup.POST("/signup", controller.SignUp)
		userGroup.GET("/me", controller.Me)
		userGroup.POST("/logout", controller.Logout)
		userGroup.POST("/refresh", controller.Refresh)
		userGroup.POST("/verify-password", middlewares.AuthMiddleware(), controller.VerifyPassword)
		userGroup.GET("", middlewares.AuthMiddleware(), controller.GetUsers)
		userGroup.POST("/send-otp", controller.SendOTP)
		userGroup.POST("/reset-password", controller.ResetPassword)
	}

	//requestGroup := r.Group("/requests")
	// requestGroup.Use(middlewares.AuthMiddleware())
	// {
	// 	requestGroup.GET("", controller.GetAllRequests)
	// 	requestGroup.GET("/user", controller.GetAllAccessByUser)
	// 	requestGroup.PUT("/update", controller.ProcessRequests)
	// }
}
