package chat

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	niapb "nordik-drive-api/internal/nia/pb"
)

func (s testNIAServer) History(ctx context.Context, req *niapb.HistoryRequest) (*niapb.HistoryResponse, error) {
	if req.UserId != 7 || req.SelectedFileId != 49 {
		return nil, status.Error(codes.NotFound, "foreign conversation")
	}
	if req.ConversationId != "" {
		return nil, status.Error(codes.NotFound, "foreign conversation")
	}
	data, _ := json.Marshal(map[string]any{"conversation_id": "nia-owned-id", "entries": []any{}, "limit": req.Limit})
	return &niapb.HistoryResponse{ResultJson: data}, nil
}

func TestHistoryUsesNIAStorage(t *testing.T) {
	for _, tc := range []struct {
		query                 string
		wantStatus, wantLimit int
		lookup                bool
	}{
		{"", 200, 10, true}, {"&limit=2", 200, 2, true}, {"&limit=0", 400, 0, false},
		{"&limit=101", 400, 0, false}, {"&limit=bad", 400, 0, false},
		{"&conversation_id=bad", 400, 0, false},
		{"&conversation_id=00000000-0000-0000-0000-000000000001", 404, 0, true},
	} {
		t.Run(tc.query, func(t *testing.T) {
			target := startTestNIA(t, func(context.Context, *niapb.QueryRequest) (*niapb.QueryResponse, error) { return nil, nil })
			db, mock, cleanup := newMockDBChatSvc(t)
			t.Cleanup(cleanup)
			if tc.lookup {
				mock.ExpectQuery(`(?i)select.*id.*version.*description.*from.*file.*where.*filename.*order.*version.*desc.*limit`).WithArgs("sheet.xlsx", 1).
					WillReturnRows(sqlmock.NewRows([]string{"id", "version", "description"}).AddRow(49, 3, "Test dataset"))
			}
			conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { conn.Close() })
			service := &ChatService{DB: db}
			backend := &niaChatBackend{service: service, client: niapb.NewAssistantClient(conn), timeout: time.Second}
			router := gin.New()
			router.Use(func(c *gin.Context) { c.Set("userID", float64(7)) })
			router.GET("/history", NewChatController(service, backend).History)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest("GET", "/history?filename=sheet.xlsx"+tc.query, nil))
			if rec.Code != tc.wantStatus {
				t.Fatalf("%d: %s", rec.Code, rec.Body.String())
			}
			if tc.wantStatus == 200 {
				var body struct {
					ID    string `json:"conversation_id"`
					Limit int    `json:"limit"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if body.ID != "nia-owned-id" || body.Limit != tc.wantLimit {
					t.Fatalf("unexpected history: %s", rec.Body.String())
				}
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
