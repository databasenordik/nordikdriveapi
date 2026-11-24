package chat

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"nordik-drive-api/internal/util"
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
	cc = cc.withContext(c.Request.Context())
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

	conversationID := strings.TrimSpace(c.PostForm("conversation_id"))
	ctx := c.Request.Context()
	if _, usesNIA := cc.Backend.(historyBackend); usesNIA {
		userID, ok := chatUserID(c)
		if !ok {
			return
		}
		ctx = context.WithValue(ctx, chatUserContextKey{}, userID)
	}
	engine := strings.TrimSpace(c.PostForm("engine"))
	backend, allowed := cc.comparisonBackend(c, engine)
	if !allowed {
		return
	}
	// Provider timing and question/answer logs are permanent. Only explicit
	// engine overrides and their public response fields use the temporary gate.
	capture := &LLMTiming{}
	ctx = context.WithValue(ctx, llmTimingKey{}, capture)
	result, err := backend.Chat(ctx, question, audioFile, filename, communities, conversationID)
	selected := engine
	if selected == "" {
		selected = "api"
		if _, ok := backend.(*niaChatBackend); ok {
			selected = "nia"
		}
	}
	answer := ""
	if result != nil {
		answer = result.Answer
		if result.Question != "" {
			question = result.Question
		}
	}
	engine = selected
	logComparison(ctx, selected, question, answer, capture.summary(), err != nil)
	if err != nil {
		statusCode := http.StatusInternalServerError
		var upstream *ChatHTTPError
		if errors.As(err, &upstream) {
			statusCode = upstream.Status
			if upstream.RetryAfter > 0 {
				c.Header("Retry-After", strconv.Itoa(upstream.RetryAfter))
			}
		}
		c.JSON(statusCode, gin.H{"error": err.Error()})
		return
	}

	response := gin.H{"answer": result.Answer}
	if comparisonEnabled() {
		response["timing"] = capture.summary()
		response["engine"] = engine
	}
	if result.ConversationID != "" {
		response["conversation_id"] = result.ConversationID
	}
	if result.ExchangeID != "" {
		response["exchange_id"] = result.ExchangeID
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
	cc = cc.withContext(c.Request.Context())
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

	speaker, ok := cc.Backend.(speechBackend)
	if !ok {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "speech requires NIA (CHAT_USE_NIA=true)"})
		return
	}
	userID, ok := chatUserID(c)
	if !ok {
		return
	}
	audio, err := speaker.Speak(c.Request.Context(), userID, text)
	if err != nil {
		code, message := http.StatusBadGateway, "speech request failed"
		var upstream *ChatHTTPError
		if errors.As(err, &upstream) {
			code, message = upstream.Status, upstream.Message
		}
		c.JSON(code, gin.H{"error": message})
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
