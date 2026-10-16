package chat

import (
	"nordik-drive-api/internal/middlewares"

	"github.com/gin-gonic/gin"
)

func RegisterRoutes(r *gin.Engine, chatService *ChatService, backends ...ChatBackend) {
	chatController := NewChatController(chatService, backends...)

	userGroup := r.Group("/api/chat")
	userGroup.Use(middlewares.AuthMiddleware())
	{
		userGroup.POST("", chatController.Chat)
		userGroup.GET("/history", chatController.History)
		userGroup.POST("/tts", chatController.TTS)
		userGroup.GET("/describe/:id", chatController.Describe)
	}

}
