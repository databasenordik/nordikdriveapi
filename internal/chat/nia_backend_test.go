package chat

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"google.golang.org/genai"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	niapb "nordik-drive-api/internal/nia/pb"
)

type testNIAServer struct {
	niapb.UnimplementedAssistantServer
	query func(context.Context, *niapb.QueryRequest) (*niapb.QueryResponse, error)
}

func (s testNIAServer) Query(ctx context.Context, request *niapb.QueryRequest) (*niapb.QueryResponse, error) {
	return s.query(ctx, request)
}

func startTestNIA(t *testing.T, query func(context.Context, *niapb.QueryRequest) (*niapb.QueryResponse, error)) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	niapb.RegisterAssistantServer(server, testNIAServer{query: query})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return listener.Addr().String()
}

func setupNIAController(t *testing.T, target string) (*gin.Engine, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, cleanup := newMockDBChatSvc(t)
	t.Cleanup(cleanup)
	mock.ExpectQuery(`(?i)select.*id.*version.*description.*from.*file.*where.*filename.*order.*version.*desc.*limit`).WithArgs("sheet.xlsx", 1).
		WillReturnRows(sqlmock.NewRows([]string{"id", "version", "description"}).AddRow(49, 3, "Test dataset"))
	t.Setenv("CHAT_USE_NIA", "true")
	t.Setenv("NIA_GRPC_TARGET", target)
	t.Setenv("NIA_GRPC_TLS", "false")
	backend, closeBackend, err := NewConfiguredChatBackend(&ChatService{DB: db})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeBackend)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("userID", float64(7)) })
	router.POST("/api/chat", NewChatController(&ChatService{DB: db}, backend).Chat)
	return router, mock
}

func requestChat(router http.Handler, communities string) *httptest.ResponseRecorder {
	form := url.Values{"filename": {"sheet.xlsx"}, "question": {"How many records are in this list?"}}
	if communities != "" {
		form.Set("communities", communities)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	return recorder
}

func TestNIAChatController_TranslatesFormAndPreservesAnswer(t *testing.T) {
	target := startTestNIA(t, func(ctx context.Context, req *niapb.QueryRequest) (*niapb.QueryResponse, error) {
		if req.UserId != 7 || req.SelectedFileId != 49 || req.Question == "" || len(req.Communities) != 2 {
			t.Errorf("unexpected query: %v", req)
		}
		md, _ := metadata.FromIncomingContext(ctx)
		if len(md.Get("authorization")) != 0 {
			t.Error("unexpected service authentication")
		}
		return &niapb.QueryResponse{Status: "answered", Answer: "NIA answer", ConversationId: "conversation-1", ExchangeId: "exchange-1"}, nil
	})
	router, mock := setupNIAController(t, target)
	response := requestChat(router, "Garden River, Batchewana")
	if response.Code != 200 {
		t.Fatalf("%d: %s", response.Code, response.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["answer"] != "NIA answer" || body["conversation_id"] != "conversation-1" || body["exchange_id"] != "exchange-1" {
		t.Fatalf("response: %v", body)
	}
	if _, exists := body["matched_row_id"]; exists {
		t.Fatal("invented a row match")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestNIAChatController_MapsUpstreamFailures(t *testing.T) {
	cases := []struct {
		name  string
		reply *niapb.QueryResponse
		err   error
		code  int
	}{
		{"unavailable", nil, status.Error(codes.Unavailable, "offline"), 503},
		{"deadline", nil, status.Error(codes.DeadlineExceeded, "late"), 504},
		{"bad file", nil, status.Error(codes.InvalidArgument, "invalid dataset"), 400},
		{"planner unavailable", &niapb.QueryResponse{Status: "planner_unavailable", Answer: "missing provider"}, nil, 503},
		{"scope conflict", &niapb.QueryResponse{Status: "dataset_scope_conflict", Answer: "change conversation"}, nil, 409},
		{"clarification", &niapb.QueryResponse{Status: "clarification", Answer: "Which year?"}, nil, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := startTestNIA(t, func(context.Context, *niapb.QueryRequest) (*niapb.QueryResponse, error) { return tc.reply, tc.err })
			router, mock := setupNIAController(t, target)
			response := requestChat(router, "")
			if response.Code != tc.code {
				t.Fatalf("%d: %s", response.Code, response.Body)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestConfiguredChatBackend_FlagAndValidation(t *testing.T) {
	t.Setenv("CHAT_USE_NIA", "false")
	t.Setenv("NIA_GRPC_TARGET", "")
	backend, closeBackend, err := NewConfiguredChatBackend(&ChatService{})
	if err != nil || backend != nil || closeBackend == nil {
		t.Fatal("legacy flag should need no NIA config")
	}
	controller := NewChatController(&ChatService{}, backend)
	if _, ok := controller.Backend.(legacyChatBackend); !ok {
		t.Fatal("legacy backend not selected")
	}
	t.Setenv("CHAT_USE_NIA", "true")
	if _, _, err := NewConfiguredChatBackend(&ChatService{}); err == nil {
		t.Fatal("missing target accepted")
	}
	t.Setenv("CHAT_USE_NIA", "typo")
	if _, _, err := NewConfiguredChatBackend(&ChatService{}); err == nil {
		t.Fatal("invalid flag accepted")
	}
}

func TestNIABackend_TranscribesAudioBeforeQuery(t *testing.T) {
	const transcript = "How many records are in this list?"
	target := startTestNIA(t, func(ctx context.Context, req *niapb.QueryRequest) (*niapb.QueryResponse, error) {
		if req.Question != transcript || req.SelectedFileId != 49 {
			t.Errorf("unexpected transcribed query: %v", req)
		}
		return &niapb.QueryResponse{Status: "answered", Answer: "Audio answer"}, nil
	})
	db, mock, cleanup := newMockDBChatSvc(t)
	t.Cleanup(cleanup)
	mock.ExpectQuery(`(?i)select.*id.*version.*description.*from.*file.*where.*filename.*order.*version.*desc.*limit`).WithArgs("sheet.xlsx", 1).
		WillReturnRows(sqlmock.NewRows([]string{"id", "version", "description"}).AddRow(49, 3, "Test dataset"))
	t.Setenv("CHAT_USE_NIA", "true")
	t.Setenv("NIA_GRPC_TARGET", target)
	t.Setenv("NIA_GRPC_TLS", "false")
	backend, closeBackend, err := NewConfiguredChatBackend(&ChatService{DB: db, Client: &genai.Client{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeBackend)
	oldGenerate := genaiGenerateContentHook
	t.Cleanup(func() { genaiGenerateContentHook = oldGenerate })
	genaiGenerateContentHook = func(_ *genai.Client, _ context.Context, _ string, contents []*genai.Content, _ *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		foundAudio := false
		for _, content := range contents {
			for _, part := range content.Parts {
				if part.InlineData != nil && part.InlineData.MIMEType == "audio/webm" && string(part.InlineData.Data) == "spoken question" {
					foundAudio = true
				}
			}
		}
		if !foundAudio {
			t.Error("transcription did not receive audio")
		}
		return rawAnswer(transcript), nil
	}
	audio := makeAudioFileHeader(t, "audio/webm", []byte("spoken question"))
	reply, err := backend.Chat(context.WithValue(context.Background(), chatUserContextKey{}, int64(7)), "", audio, "sheet.xlsx", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if reply.Answer != "Audio answer" {
		t.Fatalf("unexpected answer: %v", reply)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestNIAGrpc_ClientDeadline(t *testing.T) {
	target := startTestNIA(t, func(ctx context.Context, req *niapb.QueryRequest) (*niapb.QueryResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = niapb.NewAssistantClient(conn).Query(ctx, &niapb.QueryRequest{Question: "count", SelectedFileId: 49})
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("expected deadline, got %v", err)
	}
}

// Run against the Python container to verify Go -> protobuf -> Python core.
func TestNIAChatController_PythonIntegration(t *testing.T) {
	target := os.Getenv("NIA_TEST_TARGET")
	if target == "" {
		t.Skip("set NIA_TEST_TARGET to a fixture-backed Python gRPC server")
	}
	router, mock := setupNIAController(t, target)
	response := requestChat(router, "Garden River")
	if response.Code != 200 {
		t.Fatalf("%d: %s", response.Code, response.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(fmtString(body["answer"])) == "" {
		t.Fatal("missing answer")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func fmtString(value any) string { text, _ := value.(string); return text }

func TestNIAUserFileHistory_PythonIntegration(t *testing.T) {
	target := os.Getenv("NIA_TEST_TARGET")
	if target == "" {
		t.Skip("set NIA_TEST_TARGET to a fixture-backed Python gRPC server")
	}
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	client := niapb.NewAssistantClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	exchangeIDs := []string{}
	ask := func(user, file int64) string {
		reply, err := client.Query(ctx, &niapb.QueryRequest{UserId: user, SelectedFileId: file, Question: "How many records are in this list?"})
		if err != nil {
			t.Fatal(err)
		}
		if reply.ConversationId == "" {
			t.Fatal("missing conversation ID")
		}
		if reply.ExchangeId == "" {
			t.Fatal("missing exchange ID")
		}
		exchangeIDs = append(exchangeIDs, reply.ExchangeId)
		return reply.ConversationId
	}
	first := ask(101, 49)
	if ask(101, 49) != first {
		t.Fatal("same user/file did not resume")
	}
	if exchangeIDs[0] == exchangeIDs[1] {
		t.Fatal("different questions reused an exchange ID")
	}
	if ask(102, 49) == first || ask(101, 91) == first {
		t.Fatal("user/file isolation failed")
	}
	history, err := client.History(ctx, &niapb.HistoryRequest{UserId: 101, SelectedFileId: 49, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		ID      string `json:"conversation_id"`
		Entries []struct {
			ExchangeID string `json:"exchange_id"`
		} `json:"entries"`
		HasMore bool `json:"has_more"`
	}
	if err := json.Unmarshal(history.ResultJson, &body); err != nil {
		t.Fatal(err)
	}
	if body.ID != first || len(body.Entries) != 1 || !body.HasMore {
		t.Fatalf("incorrect history: %s", history.ResultJson)
	}
	if body.Entries[0].ExchangeID != exchangeIDs[1] {
		t.Fatal("history did not return the latest Q/A pair")
	}
	_, err = client.History(ctx, &niapb.HistoryRequest{UserId: 102, SelectedFileId: 49, ConversationId: first})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("foreign history: %v", err)
	}
}
