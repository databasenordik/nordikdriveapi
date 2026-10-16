package chat

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type chatUserContextKey struct{}

type historyBackend interface {
	History(context.Context, int64, uint, string, int) (json.RawMessage, error)
}

func chatUserID(c *gin.Context) (int64, bool) {
	value, exists := c.Get("userID")
	id, ok := value.(float64)
	if !exists || !ok || math.IsNaN(id) || math.IsInf(id, 0) || id < 1 || id >= math.MaxInt64 || math.Trunc(id) != id {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user identity"})
		return 0, false
	}
	return int64(id), true
}

// History reads the existing NIA conversation/messages through gRPC.
func (cc *ChatController) History(c *gin.Context) {
	userID, ok := chatUserID(c)
	if !ok {
		return
	}
	limit := 10
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			c.JSON(400, gin.H{"error": "limit must be between 1 and 100"})
			return
		}
		limit = n
	}
	id := strings.TrimSpace(c.Query("conversation_id"))
	if id != "" {
		if _, err := uuid.Parse(id); err != nil {
			c.JSON(400, gin.H{"error": "invalid conversation_id"})
			return
		}
	}
	filename := strings.TrimSpace(c.Query("filename"))
	if filename == "" {
		c.JSON(400, gin.H{"error": "filename is required"})
		return
	}
	backend, ok := cc.Backend.(historyBackend)
	if !ok {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "NIA history is not configured"})
		return
	}
	file, err := cc.ChatService.lookupChatFile(filename)
	if err != nil {
		c.JSON(400, gin.H{"error": "file not found"})
		return
	}
	result, err := backend.History(c.Request.Context(), userID, file.ID, id, limit)
	if err != nil {
		statusCode := http.StatusBadGateway
		if upstream, ok := err.(*ChatHTTPError); ok {
			statusCode = upstream.Status
		}
		c.JSON(statusCode, gin.H{"error": err.Error()})
		return
	}
	c.Data(http.StatusOK, "application/json", result)
}
