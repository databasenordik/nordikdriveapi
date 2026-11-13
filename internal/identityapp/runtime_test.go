package identityapp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"nordik-drive-api/config"
	"nordik-drive-api/internal/identity"
	pb "nordik-drive-api/internal/identitypb"
	"nordik-drive-api/internal/notificationcontract"
	"nordik-drive-api/internal/outbox"
)

const testKeys = `{"1":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`

type remoteStub struct{}

func (remoteStub) Accept(context.Context, notificationcontract.Command) (string, error) {
	return "", nil
}
func (remoteStub) Status(context.Context, []string) (map[string]string, error) { return nil, nil }
func (remoteStub) Recover(context.Context) error                               { return nil }

func TestRuntimeConfigurationBoundaries(t *testing.T) {
	valid := config.Config{DBAuthMode: "iam", DBMaxOpen: 1, DBSchema: "identity,public", JWTSecret: "test-only-existing-jwt"}
	if err := ValidateRuntime(valid, false); err != nil {
		t.Fatal(err)
	}
	valid.DBSchema = "identity"
	if err := ValidateRuntime(valid, false); err != nil {
		t.Fatal(err)
	}
	valid.JWTSecret = ""
	if err := ValidateRuntime(valid, true); err != nil {
		t.Fatal("cleanup needs no JWT")
	}
	if ValidateRuntime(valid, false) == nil {
		t.Fatal("server missing JWT accepted")
	}
	for _, cfg := range []config.Config{{DBAuthMode: "password", DBMaxOpen: 1, DBSchema: "identity"}, {DBAuthMode: "iam", DBMaxOpen: 2, DBSchema: "identity"}, {DBAuthMode: "iam", DBMaxOpen: 1, DBSchema: "public"}} {
		if ValidateRuntime(cfg, true) == nil {
			t.Fatal("unsafe runtime accepted")
		}
	}
	for _, name := range []string{"IDENTITY_PROFILE_ACCOUNTS", "IDENTITY_PRINCIPAL_ACCOUNTS", "IDENTITY_EMAIL_ACCOUNTS", "SIGNUP_REQUIRE_VERIFIED_EMAIL"} {
		t.Setenv(name, "")
	}
	t.Setenv("IDENTITY_PROFILE_ACCOUNTS", "legacy@project.iam.gserviceaccount.com, audit@project.iam.gserviceaccount.com")
	options, err := LoadOptions()
	if err != nil || len(options.Policy.Profiles) != 2 || !options.RequireVerified {
		t.Fatal("caller policy/defaults changed")
	}
	t.Setenv("SIGNUP_REQUIRE_VERIFIED_EMAIL", "false")
	options, err = LoadOptions()
	if err != nil || options.RequireVerified {
		t.Fatal("compatibility flag not parsed")
	}
	t.Setenv("SIGNUP_REQUIRE_VERIFIED_EMAIL", "invalid")
	if _, err := LoadOptions(); err == nil {
		t.Fatal("invalid verification flag accepted")
	}
	t.Setenv("SIGNUP_REQUIRE_VERIFIED_EMAIL", "")
	for _, name := range []string{"IDENTITY_PROFILE_ACCOUNTS", "IDENTITY_PRINCIPAL_ACCOUNTS", "IDENTITY_EMAIL_ACCOUNTS"} {
		t.Setenv(name, "user@example.test")
		if _, err := LoadOptions(); err == nil {
			t.Fatal("non-service-account caller accepted")
		}
		t.Setenv(name, "")
	}
	for _, bad := range []string{"spaces allowed@p.iam.gserviceaccount.com", "user@@p.iam.gserviceaccount.com", "invalid/account@p.iam.gserviceaccount.com"} {
		if _, err := accounts(bad); err == nil {
			t.Fatal("invalid caller accepted")
		}
	}
}

func TestSingleListenerKeepsHTTPAndGRPCPrivateBoundaries(t *testing.T) {
	pool, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	codec, err := notificationcontract.NewCodec(testKeys, "1")
	if err != nil {
		t.Fatal(err)
	}
	writer := &outbox.Writer{Codec: codec}
	signupKeys := `{"1":"` + strings.Repeat("AQEB", 10) + `AQE="}`
	options := Options{Audience: "https://identity.example", NotificationReader: "notifications@p.iam.gserviceaccount.com", WorkerAccount: "worker@p.iam.gserviceaccount.com", RequireVerified: true, SignupKeys: signupKeys, SignupVersion: "1", Policy: identity.Policy{Profiles: map[string]bool{"legacy@p.iam.gserviceaccount.com": true}}}
	handler, closeServer, err := New(db, pool, config.Config{JWTSecret: "test-only-existing-jwt"}, options, writer, remoteStub{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeServer()
	server := httptest.NewServer(handler)
	defer server.Close()
	request, err := http.NewRequest("POST", server.URL+"/api/user/login", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://nordik-drive-react-724838782318.us-west1.run.app")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 400 || response.Header.Get("Access-Control-Allow-Origin") == "" {
		t.Fatal("existing HTTP validation/CORS changed")
	}
	connection, err := grpc.NewClient(strings.TrimPrefix(server.URL, "http://"), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := pb.NewIdentityClient(connection).BatchGetProfiles(context.Background(), &pb.BatchGetProfilesRequest{Ids: []int64{1}}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("same-listener gRPC failed: %v", err)
	}
	for _, tc := range []struct {
		method, path string
		want         int
	}{{"GET", "/healthz", 200}, {"GET", "/api/role", 401}, {"POST", "/internal/v1/identity/cleanup", 404}, {"POST", "/internal/v1/notification-outbox/read", 403}, {"POST", "/internal/v1/identity/dispatch-notifications", 403}} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(tc.method, tc.path, nil))
		if response.Code != tc.want {
			t.Fatalf("%s HTTP %d want %d", tc.path, response.Code, tc.want)
		}
	}
	for _, audience := range []string{"http://identity.example", "https://user@identity.example", "https://identity.example/path", "https://identity.example?q=x", "https://identity.example#x"} {
		bad := options
		bad.Audience = audience
		if _, _, err := New(db, pool, config.Config{}, bad, writer, remoteStub{}); err == nil {
			t.Fatal("invalid audience accepted")
		}
	}
	if _, _, err := New(db, pool, config.Config{}, options, nil, remoteStub{}); err == nil {
		t.Fatal("missing transactional writer accepted")
	}
	bad := options
	bad.SignupKeys = "invalid"
	if _, _, err := New(db, pool, config.Config{}, bad, writer, remoteStub{}); err == nil {
		t.Fatal("bad verification keys accepted")
	}
	bad = options
	bad.SignupKeys = ""
	if _, _, err := New(db, pool, config.Config{}, bad, writer, remoteStub{}); err == nil {
		t.Fatal("mandatory verification disabled")
	}
	bad = options
	bad.Policy = identity.Policy{}
	if _, _, err := New(db, pool, config.Config{}, bad, writer, remoteStub{}); err == nil {
		t.Fatal("empty caller policy accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateDispatchRequiresWorkerAndReportsRecoveryFailure(t *testing.T) {
	for _, tc := range []struct {
		method, token string
		fail          bool
		want          int
	}{{"GET", "signed", false, 405}, {"POST", "", false, 403}, {"POST", "signed", false, 204}, {"POST", "signed", true, 503}} {
		called := false
		handler := dispatchHandler("https://identity.example", "worker", func(context.Context) error {
			called = true
			if tc.fail {
				return errors.New("SECRET")
			}
			return nil
		}, func(ctx context.Context, token, audience string, accounts map[string]bool) error {
			if token != "signed" || audience != "https://identity.example" || !accounts["worker"] {
				return errors.New("invalid")
			}
			return nil
		})
		req := httptest.NewRequest(tc.method, "/internal/v1/identity/dispatch-notifications", nil)
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != tc.want || strings.Contains(response.Body.String(), "SECRET") || called != (tc.want == 204 || tc.want == 503) {
			t.Fatal("private dispatch boundary failed")
		}
	}
}

func TestServeHandlesListenerErrorAndCancellation(t *testing.T) {
	if Serve(context.Background(), http.NotFoundHandler(), "not:a:port") == nil {
		t.Fatal("listener failure hidden")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Serve(ctx, http.NotFoundHandler(), "0"); err != nil {
		t.Fatal(err)
	}
}
