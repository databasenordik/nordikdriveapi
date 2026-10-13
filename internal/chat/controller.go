package chat

import (
	"errors"
	"net/http"
	"nordik-drive-api/internal/util"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

type ChatController struct {
	ChatService *ChatService
	Backend     ChatBackend
}

func NewChatController(cs *ChatService, backends ...ChatBackend) *ChatController {
	var backend ChatBackend = legacyChatBackend{service: cs}
	if len(backends) > 0 && backends[0] != nil {
		backend = backends[0]
	}
	return &ChatController{ChatService: cs, Backend: backend}
}

func (cc *ChatController) Chat(c *gin.Context) {
	question := c.PostForm("question")
	filename := c.PostForm("filename")
	audioFile, _ := c.FormFile("audio")
	communities := c.PostFormArray("communities")
	includeDebug := shouldIncludeChatDebug(c)

	communities = util.ParseCommaSeparatedCommunities(communities)

	if filename == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "filename is required"})
		return
	}

	result, err := cc.Backend.Chat(c.Request.Context(), question, audioFile, filename, communities, c.PostForm("conversation_id"))
	if err != nil {
		statusCode := http.StatusInternalServerError
		var upstream *ChatHTTPError
		if errors.As(err, &upstream) {
			statusCode = upstream.Status
		}
		c.JSON(statusCode, gin.H{"error": err.Error()})
		return
	}

	response := gin.H{"answer": result.Answer}
	if result.ConversationID != "" {
		response["conversation_id"] = result.ConversationID
	}
	if result.MatchedRowID != nil {
		response["matched_row_id"] = *result.MatchedRowID
	}
	if includeDebug && result.Debug != nil {
		response["debug"] = result.Debug
	}

	c.JSON(http.StatusOK, response)
}

func (cc *ChatController) Describe(c *gin.Context) {
	idStr := strings.TrimSpace(c.Param("id"))
	if idStr == "" {
		idStr = strings.TrimSpace(c.Query("id"))
	}

	id, err := strconv.Atoi(idStr)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "valid id is required"})
		return
	}

	answer, err := cc.ChatService.DescribeRow(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"id":     id,
		"answer": answer,
	})
}

func (cc *ChatController) TTS(c *gin.Context) {
	text := strings.TrimSpace(c.PostForm("text"))
	if text == "" {
		text = strings.TrimSpace(c.PostForm("answer"))
	}

	if text == "" {
		var body ttsJSON
		if err := c.ShouldBindJSON(&body); err == nil {
			if body.Text != "" {
				text = strings.TrimSpace(body.Text)
			} else {
				text = strings.TrimSpace(body.Answer)
			}
		}
	}

	if text == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "text is required"})
		return
	}

	audio, err := cc.ChatService.TTS(text)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Return binary audio so frontend can fetch -> Blob -> cache & replay
	c.Data(http.StatusOK, audio.MimeType, audio.Data)
}

func shouldIncludeChatDebug(c *gin.Context) bool {
	return parseBoolish(c.Query("debug")) || parseBoolish(c.GetHeader("X-Chat-Debug"))
}

func parseBoolish(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
