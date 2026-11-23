package chat

import (
	"context"
	"net/http"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	niapb "nordik-drive-api/internal/nia/pb"
	"nordik-drive-api/internal/observability"
)

// Speech comes from NIA, whose backend/reasoning.json sets the voice and the other options.
//
// Speech failures are deliberately not recorded in the chat circuit breaker: speech being down
// must not stop typed questions from being answered.

// gRPC's 4 MiB default is about 8 minutes of MP3, or 90 seconds of WAV.
const niaMaxSpeechBytes = 64 << 20

type speechBackend interface {
	Speak(ctx context.Context, userID int64, text string) (*TTSAudio, error)
}

func (b *niaChatBackend) Speak(ctx context.Context, userID int64, text string) (*TTSAudio, error) {
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "x-request-id", observability.RequestID(ctx))
	// Form values need not be UTF-8, and protobuf refuses strings that are not.
	reply, err := b.client.Speak(ctx, &niapb.SpeakRequest{Text: strings.ToValidUTF8(text, "\uFFFD"), UserId: userID},
		grpc.MaxCallRecvMsgSize(niaMaxSpeechBytes))
	if err != nil {
		return nil, mapSpeechError(err, "speech request failed")
	}
	// The type becomes this API's Content-Type, so it must be audio.
	if len(reply.GetAudio()) == 0 || !strings.HasPrefix(reply.GetMimeType(), "audio/") {
		return nil, &ChatHTTPError{Status: http.StatusBadGateway, Message: "NIA returned no audio"}
	}
	return &TTSAudio{MimeType: reply.GetMimeType(), Data: reply.GetAudio()}, nil
}

func (b *niaChatBackend) transcribe(ctx context.Context, userID int64, audio []byte, mimeType string) (string, error) {
	ctx = metadata.AppendToOutgoingContext(ctx, "x-request-id", observability.RequestID(ctx))
	reply, err := b.client.Transcribe(ctx, &niapb.TranscribeRequest{
		Audio: audio, MimeType: strings.ToValidUTF8(mimeType, ""), UserId: userID,
	})
	if err != nil {
		return "", mapSpeechError(err, "audio transcription failed")
	}
	if strings.TrimSpace(reply.GetText()) == "" {
		return "", &ChatHTTPError{Status: http.StatusBadRequest, Message: "no speech was detected"}
	}
	return reply.GetText(), nil
}

func mapSpeechError(err error, failed string) error {
	switch status.Code(err) {
	case codes.InvalidArgument:
		return &ChatHTTPError{Status: http.StatusBadRequest, Message: status.Convert(err).Message()}
	case codes.ResourceExhausted:
		return &ChatHTTPError{Status: http.StatusTooManyRequests, Message: "speech is busy; try again in a moment"}
	case codes.Canceled:
		return &ChatHTTPError{Status: http.StatusRequestTimeout, Message: "speech request canceled"}
	case codes.DeadlineExceeded:
		return &ChatHTTPError{Status: http.StatusGatewayTimeout, Message: "speech request timed out"}
	case codes.Unavailable:
		return &ChatHTTPError{Status: http.StatusServiceUnavailable, Message: "speech is temporarily unavailable"}
	default:
		return &ChatHTTPError{Status: http.StatusBadGateway, Message: failed}
	}
}
