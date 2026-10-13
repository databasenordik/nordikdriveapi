package chat

import (
	"context"
	"mime/multipart"
)

type ChatBackend interface {
	Chat(context.Context, string, *multipart.FileHeader, string, []string, string) (*ChatResult, error)
}

type legacyChatBackend struct{ service *ChatService }

func (b legacyChatBackend) Chat(_ context.Context, question string, audio *multipart.FileHeader, filename string, communities []string, _ string) (*ChatResult, error) {
	return b.service.Chat(question, audio, filename, communities)
}

type ChatHTTPError struct {
	Status  int
	Message string
}

func (e *ChatHTTPError) Error() string { return e.Message }
