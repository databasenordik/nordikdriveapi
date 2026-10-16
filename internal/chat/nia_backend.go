package chat

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	niapb "nordik-drive-api/internal/nia/pb"
)

type niaChatBackend struct {
	service *ChatService
	client  niapb.AssistantClient
	timeout time.Duration
}

// A single gRPC connection is reused across requests. No identity token is attached.
func NewConfiguredChatBackend(service *ChatService) (ChatBackend, func(), error) {
	enabled, err := envBool("CHAT_USE_NIA", false)
	if err != nil || !enabled {
		return nil, func() {}, err
	}
	target := strings.TrimSpace(os.Getenv("NIA_GRPC_TARGET"))
	if target == "" || strings.Contains(target, "://") {
		return nil, nil, fmt.Errorf("NIA_GRPC_TARGET must be a host:port without an HTTP URL scheme")
	}
	useTLS, err := envBool("NIA_GRPC_TLS", true)
	if err != nil {
		return nil, nil, err
	}
	seconds := 300
	if raw := strings.TrimSpace(os.Getenv("NIA_GRPC_TIMEOUT_SECONDS")); raw != "" {
		seconds, err = strconv.Atoi(raw)
		if err != nil || seconds < 1 || seconds > 3600 {
			return nil, nil, fmt.Errorf("NIA_GRPC_TIMEOUT_SECONDS must be between 1 and 3600")
		}
	}
	var transport credentials.TransportCredentials = insecure.NewCredentials()
	if useTLS {
		transport = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	}
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(transport))
	if err != nil {
		return nil, nil, fmt.Errorf("configure NIA gRPC: %w", err)
	}
	return &niaChatBackend{service: service, client: niapb.NewAssistantClient(conn), timeout: time.Duration(seconds) * time.Second}, func() { _ = conn.Close() }, nil
}

func envBool(name string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false", name)
	}
	return value, nil
}

func (b *niaChatBackend) Chat(ctx context.Context, question string, audio *multipart.FileHeader, filename string, communities []string, conversationID string) (*ChatResult, error) {
	userID, _ := ctx.Value(chatUserContextKey{}).(int64)
	if userID <= 0 {
		return nil, &ChatHTTPError{Status: http.StatusUnauthorized, Message: "user identity is required"}
	}
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	file, err := b.service.lookupChatFile(filename)
	if err != nil {
		return nil, &ChatHTTPError{Status: http.StatusBadRequest, Message: err.Error()}
	}
	if audio != nil {
		stream, err := audio.Open()
		if err != nil {
			return nil, &ChatHTTPError{Status: http.StatusBadRequest, Message: "could not read audio"}
		}
		defer stream.Close()
		const maxAudio = 20 << 20
		data, err := io.ReadAll(io.LimitReader(stream, maxAudio+1))
		if err != nil || len(data) == 0 || len(data) > maxAudio {
			return nil, &ChatHTTPError{Status: http.StatusBadRequest, Message: "audio must be between 1 byte and 20 MiB"}
		}
		transcript, _, err := b.service.generateFromPrompt(ctx,
			"Transcribe the spoken question. Return only the transcription, without answering it or adding commentary.", data, audio.Header.Get("Content-Type"))
		if err != nil {
			return nil, &ChatHTTPError{Status: http.StatusBadGateway, Message: "audio transcription failed"}
		}
		question = transcript
	}
	question = strings.TrimSpace(question)
	if question == "" {
		return nil, &ChatHTTPError{Status: http.StatusBadRequest, Message: "question or audio is required"}
	}
	reply, err := b.client.Query(ctx, &niapb.QueryRequest{
		Question: question, SelectedFileId: int64(file.ID), Communities: communities, ConversationId: conversationID, UserId: userID,
	})
	if err != nil {
		return nil, mapNIAError(err)
	}
	switch reply.GetStatus() {
	case "planner_unavailable", "planner_failed", "planner_invalid_output":
		return nil, &ChatHTTPError{Status: http.StatusServiceUnavailable, Message: "NIA could not prepare an answer"}
	case "dataset_unavailable", "access_restricted":
		return nil, &ChatHTTPError{Status: http.StatusBadRequest, Message: reply.GetAnswer()}
	case "dataset_scope_conflict", "dataset_mismatch", "legacy_unscoped_conversation":
		return nil, &ChatHTTPError{Status: http.StatusConflict, Message: reply.GetAnswer()}
	case "invalid_plan":
		return nil, &ChatHTTPError{Status: http.StatusBadGateway, Message: "NIA returned an invalid query plan"}
	}
	if reply.GetStatus() == "" || strings.TrimSpace(reply.GetAnswer()) == "" {
		return nil, &ChatHTTPError{Status: http.StatusBadGateway, Message: "NIA returned an empty answer"}
	}
	return &ChatResult{Question: question, Answer: reply.GetAnswer(), ConversationID: reply.GetConversationId(), ExchangeID: reply.GetExchangeId()}, nil
}

func (b *niaChatBackend) History(ctx context.Context, userID int64, fileID uint, conversationID string, limit int) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	reply, err := b.client.History(ctx, &niapb.HistoryRequest{
		UserId: userID, SelectedFileId: int64(fileID), ConversationId: conversationID, Limit: int32(limit),
	})
	if err != nil {
		return nil, mapNIAError(err)
	}
	if !json.Valid(reply.ResultJson) {
		return nil, &ChatHTTPError{Status: http.StatusBadGateway, Message: "NIA returned invalid history"}
	}
	return json.RawMessage(reply.ResultJson), nil
}

func mapNIAError(err error) error {
	switch status.Code(err) {
	case codes.NotFound:
		return &ChatHTTPError{Status: http.StatusNotFound, Message: "conversation not found"}
	case codes.PermissionDenied:
		return &ChatHTTPError{Status: http.StatusForbidden, Message: "conversation access denied"}
	case codes.InvalidArgument:
		return &ChatHTTPError{Status: http.StatusBadRequest, Message: status.Convert(err).Message()}
	case codes.DeadlineExceeded:
		return &ChatHTTPError{Status: http.StatusGatewayTimeout, Message: "NIA request timed out"}
	case codes.Unavailable:
		return &ChatHTTPError{Status: http.StatusServiceUnavailable, Message: "NIA is unavailable"}
	case codes.Canceled:
		return &ChatHTTPError{Status: http.StatusRequestTimeout, Message: "chat request canceled"}
	default:
		return &ChatHTTPError{Status: http.StatusBadGateway, Message: "NIA request failed"}
	}
}
