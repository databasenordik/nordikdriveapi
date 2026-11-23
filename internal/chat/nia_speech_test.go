package chat

import (
	"bytes"
	"context"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	niapb "nordik-drive-api/internal/nia/pb"
)

type testSpeechServer struct {
	testNIAServer
	speak      func(context.Context, *niapb.SpeakRequest) (*niapb.SpeakResponse, error)
	transcribe func(context.Context, *niapb.TranscribeRequest) (*niapb.TranscribeResponse, error)
}

func (s *testSpeechServer) Speak(ctx context.Context, request *niapb.SpeakRequest) (*niapb.SpeakResponse, error) {
	return s.speak(ctx, request)
}

func (s *testSpeechServer) Transcribe(ctx context.Context, request *niapb.TranscribeRequest) (*niapb.TranscribeResponse, error) {
	return s.transcribe(ctx, request)
}

func startTestSpeech(t *testing.T, implementation niapb.AssistantServer) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// NIA accepts 21 MiB, for a 20 MiB recording and its envelope.
	server := grpc.NewServer(grpc.MaxRecvMsgSize(21 << 20))
	niapb.RegisterAssistantServer(server, implementation)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return listener.Addr().String()
}

func newTestSpeechBackend(t *testing.T, implementation niapb.AssistantServer) *niaChatBackend {
	t.Helper()
	t.Setenv("CHAT_USE_NIA", "true")
	t.Setenv("NIA_GRPC_TARGET", startTestSpeech(t, implementation))
	t.Setenv("NIA_GRPC_TLS", "false")
	backend, closeBackend, err := NewConfiguredChatBackend(&ChatService{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeBackend)
	return backend.(*niaChatBackend)
}

func requestSpeech(backend ChatBackend, userID any, contentType string, body []byte) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		if userID != nil {
			c.Set("userID", userID)
		}
	})
	router.POST("/api/chat/tts", NewChatController(&ChatService{}, backend).TTS)
	request := httptest.NewRequest(http.MethodPost, "/api/chat/tts", bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func TestTTS_SpeaksThroughNIAForEveryInputShape(t *testing.T) {
	var spoken *niapb.SpeakRequest
	backend := newTestSpeechBackend(t, &testSpeechServer{speak: func(_ context.Context, request *niapb.SpeakRequest) (*niapb.SpeakResponse, error) {
		spoken = request
		return &niapb.SpeakResponse{Audio: []byte{0xff, 0xf3, 1}, MimeType: "audio/mpeg"}, nil
	}})
	var multipartBody bytes.Buffer
	form := multipart.NewWriter(&multipartBody)
	// What nordik-drive sends. Only the text is used.
	for key, value := range map[string]string{"text": "Hello", "model": "gemini-2.5-flash-tts", "voice": "Albenib"} {
		if err := form.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	for name, input := range map[string]struct {
		contentType string
		body        []byte
	}{
		"multipart":   {form.FormDataContentType(), multipartBody.Bytes()},
		"form answer": {"application/x-www-form-urlencoded", []byte(url.Values{"answer": {"Hello"}}.Encode())},
		"json text":   {"application/json", []byte(`{"text":"Hello"}`)},
		"json answer": {"application/json", []byte(`{"answer":"Hello"}`)},
	} {
		t.Run(name, func(t *testing.T) {
			spoken = nil
			response := requestSpeech(backend, float64(7), input.contentType, input.body)
			if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "audio/mpeg" || !bytes.Equal(response.Body.Bytes(), []byte{0xff, 0xf3, 1}) {
				t.Fatalf("%d %q %v", response.Code, response.Header().Get("Content-Type"), response.Body.Bytes())
			}
			if spoken == nil || spoken.Text != "Hello" || spoken.UserId != 7 {
				t.Fatalf("unexpected speech request: %v", spoken)
			}
		})
	}
}

func TestTTS_RequiresNIAAndAUser(t *testing.T) {
	body := []byte(`{"text":"Hello"}`)
	if response := requestSpeech(nil, float64(7), "application/json", body); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("without NIA: %d", response.Code)
	}
	backend := newTestSpeechBackend(t, &testSpeechServer{})
	if response := requestSpeech(backend, nil, "application/json", body); response.Code != http.StatusUnauthorized {
		t.Fatalf("without a user: %d", response.Code)
	}
}

func TestTTS_ForwardsGeminiWAVWithoutChangingIt(t *testing.T) {
	wav := []byte("RIFF\x04\x00\x00\x00WAVE")
	backend := newTestSpeechBackend(t, &testSpeechServer{speak: func(context.Context, *niapb.SpeakRequest) (*niapb.SpeakResponse, error) {
		return &niapb.SpeakResponse{Audio: wav, MimeType: "audio/wav"}, nil
	}})
	response := requestSpeech(backend, float64(7), "application/json", []byte(`{"text":"Read this answer."}`))
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "audio/wav" || !bytes.Equal(response.Body.Bytes(), wav) {
		t.Fatalf("%d %q %v", response.Code, response.Header().Get("Content-Type"), response.Body.Bytes())
	}
}

func TestTTS_LongAudioExceedsTheDefaultReceiveLimit(t *testing.T) {
	const size = 10 << 20
	backend := newTestSpeechBackend(t, &testSpeechServer{speak: func(context.Context, *niapb.SpeakRequest) (*niapb.SpeakResponse, error) {
		return &niapb.SpeakResponse{Audio: make([]byte, size), MimeType: "audio/mpeg"}, nil
	}})
	response := requestSpeech(backend, float64(7), "application/json", []byte(`{"text":"A long answer"}`))
	if response.Code != http.StatusOK || response.Body.Len() != size {
		t.Fatalf("%d, %d bytes", response.Code, response.Body.Len())
	}
}

func TestTTS_InvalidUTF8IsStillSpoken(t *testing.T) {
	backend := newTestSpeechBackend(t, &testSpeechServer{speak: func(context.Context, *niapb.SpeakRequest) (*niapb.SpeakResponse, error) {
		return &niapb.SpeakResponse{Audio: []byte{1}, MimeType: "audio/mpeg"}, nil
	}})
	response := requestSpeech(backend, float64(7), "application/x-www-form-urlencoded", []byte("text=Hello%FF"))
	if response.Code != http.StatusOK {
		t.Fatalf("%d %s", response.Code, response.Body)
	}
}

func TestTTS_FailuresAreMappedAndNeverLeak(t *testing.T) {
	for _, test := range []struct {
		code codes.Code
		want int
	}{
		{codes.InvalidArgument, http.StatusBadRequest},
		{codes.ResourceExhausted, http.StatusTooManyRequests},
		{codes.DeadlineExceeded, http.StatusGatewayTimeout},
		{codes.Canceled, http.StatusRequestTimeout},
		{codes.Unavailable, http.StatusServiceUnavailable},
		{codes.Internal, http.StatusBadGateway},
		{codes.Unimplemented, http.StatusBadGateway},
	} {
		t.Run(test.code.String(), func(t *testing.T) {
			message := "private provider detail"
			if test.code == codes.InvalidArgument {
				message = "text must be at most 16000 characters"
			}
			backend := newTestSpeechBackend(t, &testSpeechServer{speak: func(context.Context, *niapb.SpeakRequest) (*niapb.SpeakResponse, error) {
				return nil, status.Error(test.code, message)
			}})
			response := requestSpeech(backend, float64(7), "application/json", []byte(`{"text":"Hello"}`))
			if response.Code != test.want || strings.Contains(response.Body.String(), "private") {
				t.Fatalf("%d %s", response.Code, response.Body)
			}
		})
	}
}

func TestTTS_RefusesAReplyThatIsNotAudio(t *testing.T) {
	for name, reply := range map[string]*niapb.SpeakResponse{
		"empty": {MimeType: "audio/mpeg"},
		"html":  {Audio: []byte("<script></script>"), MimeType: "text/html"},
	} {
		t.Run(name, func(t *testing.T) {
			backend := newTestSpeechBackend(t, &testSpeechServer{speak: func(context.Context, *niapb.SpeakRequest) (*niapb.SpeakResponse, error) {
				return reply, nil
			}})
			if response := requestSpeech(backend, float64(7), "application/json", []byte(`{"text":"Hello"}`)); response.Code != http.StatusBadGateway {
				t.Fatalf("%d %s", response.Code, response.Body)
			}
		})
	}
}

func TestTTS_FailuresDoNotStopTypedQuestions(t *testing.T) {
	backend := newTestSpeechBackend(t, &testSpeechServer{speak: func(context.Context, *niapb.SpeakRequest) (*niapb.SpeakResponse, error) {
		return nil, status.Error(codes.Unavailable, "down")
	}})
	// The chat breaker opens after six recorded availability failures.
	for i := 0; i < 10; i++ {
		if _, err := backend.Speak(context.Background(), 7, "Hello"); err == nil {
			t.Fatal("expected a failure")
		}
	}
	if _, retry := backend.breaker.Allow(); retry > 0 {
		t.Fatal("speech failures opened the chat circuit breaker")
	}
}

func TestTTS_CancellationReachesNIA(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	backend := newTestSpeechBackend(t, &testSpeechServer{speak: func(ctx context.Context, _ *niapb.SpeakRequest) (*niapb.SpeakResponse, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := backend.Speak(ctx, 7, "Hello")
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("NIA was not called")
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("expected cancellation")
	}
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("cancellation did not reach NIA")
	}
}

func TestTranscribe_CarriesA20MiBRecording(t *testing.T) {
	backend := newTestSpeechBackend(t, &testSpeechServer{transcribe: func(_ context.Context, request *niapb.TranscribeRequest) (*niapb.TranscribeResponse, error) {
		if len(request.Audio) != 20<<20 || request.UserId != 7 {
			t.Errorf("%d bytes for user %d", len(request.Audio), request.UserId)
		}
		return &niapb.TranscribeResponse{Text: "words"}, nil
	}})
	if text, err := backend.transcribe(context.Background(), 7, make([]byte, 20<<20), "audio/webm"); err != nil || text != "words" {
		t.Fatalf("%q %v", text, err)
	}
}

func TestTranscribe_SilenceAndFailuresAreNotQuestions(t *testing.T) {
	for name, test := range map[string]struct {
		text string
		err  error
		want int
	}{
		"silence":     {text: " ", want: http.StatusBadRequest},
		"invalid":     {err: status.Error(codes.InvalidArgument, "audio could not be transcribed"), want: http.StatusBadRequest},
		"unavailable": {err: status.Error(codes.Unavailable, "down"), want: http.StatusServiceUnavailable},
		"failed":      {err: status.Error(codes.Internal, "private detail"), want: http.StatusBadGateway},
	} {
		t.Run(name, func(t *testing.T) {
			backend := newTestSpeechBackend(t, &testSpeechServer{transcribe: func(context.Context, *niapb.TranscribeRequest) (*niapb.TranscribeResponse, error) {
				if test.err != nil {
					return nil, test.err
				}
				return &niapb.TranscribeResponse{Text: test.text}, nil
			}})
			_, err := backend.transcribe(context.Background(), 7, []byte{1}, "audio/webm")
			failure, ok := err.(*ChatHTTPError)
			if !ok || failure.Status != test.want || strings.Contains(failure.Message, "private") {
				t.Fatalf("%v", err)
			}
		})
	}
}
