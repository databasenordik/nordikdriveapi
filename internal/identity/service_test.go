package identity

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	pb "nordik-drive-api/internal/identitypb"
)

func testService(t *testing.T) (*Service, sqlmock.Sqlmock) {
	t.Helper()
	pool, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})
	return &Service{DB: db, Policy: Policy{Profiles: map[string]bool{"legacy": true, "audit": true}, Principals: map[string]bool{"legacy": true}, Email: map[string]bool{"legacy": true}}}, mock
}
func as(account string) context.Context {
	return context.WithValue(context.Background(), callerKey{}, account)
}

func TestProfilePrivacyAndMissingIDs(t *testing.T) {
	service, mock := testService(t)
	mock.ExpectQuery("SELECT id,firstname,lastname FROM").WithArgs("{1,2}").WillReturnRows(sqlmock.NewRows([]string{"id", "firstname", "lastname"}).AddRow(1, "First", "Last"))
	result, err := service.BatchGetProfiles(as("audit"), &pb.BatchGetProfilesRequest{Ids: []int64{1, 2, 1}})
	if err != nil || len(result.Profiles) != 1 || result.Profiles[0].Email != "" || !reflect.DeepEqual(result.MissingIds, []int64{2}) {
		t.Fatalf("profiles=%v err=%v", result, err)
	}
	if _, err := service.BatchGetProfiles(as("audit"), &pb.BatchGetProfilesRequest{Ids: []int64{1}, IncludeEmail: true}); status.Code(err) != codes.PermissionDenied {
		t.Fatal("Audit received email access")
	}
	mock.ExpectQuery("SELECT id,firstname,lastname,email FROM").WithArgs("{1}").WillReturnRows(sqlmock.NewRows([]string{"id", "firstname", "lastname", "email"}).AddRow(1, "First", "Last", "recipient@example.test"))
	result, err = service.BatchGetProfiles(as("legacy"), &pb.BatchGetProfilesRequest{Ids: []int64{1}, IncludeEmail: true})
	if err != nil || result.Profiles[0].Email != "recipient@example.test" {
		t.Fatal("authorized recipient selection changed")
	}
}

func TestPrincipalParityAndFailureRedaction(t *testing.T) {
	service, mock := testService(t)
	if _, err := service.BatchGetPrincipals(as("audit"), &pb.BatchGetPrincipalsRequest{Ids: []int64{1}}); status.Code(err) != codes.PermissionDenied {
		t.Fatal("Audit received principal access")
	}
	mock.ExpectQuery("SELECT id,role,community FROM").WithArgs("{1,2}").WillReturnRows(sqlmock.NewRows([]string{"id", "role", "community"}).AddRow(1, "Admin", "{north,south}"))
	response, err := service.BatchGetPrincipals(as("legacy"), &pb.BatchGetPrincipalsRequest{Ids: []int64{1, 2}})
	if err != nil || response.Principals[0].Role != "Admin" || !reflect.DeepEqual(response.Principals[0].Communities, []string{"north", "south"}) || !reflect.DeepEqual(response.MissingIds, []int64{2}) {
		t.Fatalf("principals=%v err=%v", response, err)
	}
	for _, method := range []string{"profiles", "principals"} {
		mock.ExpectQuery("SELECT").WillReturnError(errors.New("SECRET_DATABASE_FAILURE"))
		if method == "profiles" {
			_, err = service.BatchGetProfiles(as("legacy"), &pb.BatchGetProfilesRequest{Ids: []int64{1}})
		} else {
			_, err = service.BatchGetPrincipals(as("legacy"), &pb.BatchGetPrincipalsRequest{Ids: []int64{1}})
		}
		if status.Code(err) != codes.Unavailable || strings.Contains(err.Error(), "SECRET") {
			t.Fatal("repository failure was hidden or leaked")
		}
	}
}

func TestIdentityBatchBounds(t *testing.T) {
	service, _ := testService(t)
	for _, ids := range [][]int64{nil, {0}, {-1}, make([]int64, 101)} {
		if _, err := service.BatchGetProfiles(as("legacy"), &pb.BatchGetProfilesRequest{Ids: ids}); status.Code(err) != codes.InvalidArgument {
			t.Fatal("invalid profile batch accepted")
		}
		if _, err := service.BatchGetPrincipals(as("legacy"), &pb.BatchGetPrincipalsRequest{Ids: ids}); status.Code(err) != codes.InvalidArgument {
			t.Fatal("invalid principal batch accepted")
		}
	}
	if _, err := service.BatchGetProfiles(context.Background(), nil); status.Code(err) != codes.PermissionDenied {
		t.Fatal("missing caller accepted")
	}
}

func TestSearchPreservesNamePartsWildcardsAndPagination(t *testing.T) {
	service, mock := testService(t)
	mock.ExpectQuery("SELECT.*id.*FROM.*users.*firstname.*ILIKE.*lastname.*ILIKE").WithArgs(int64(0), "%A_%", "%A_%", 2).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1).AddRow(2))
	response, err := service.SearchProfiles(as("audit"), &pb.SearchProfilesRequest{Query: "A_", Limit: 1})
	if err != nil || !reflect.DeepEqual(response.Ids, []int64{1}) || response.NextAfterId != 1 {
		t.Fatalf("search=%v err=%v", response, err)
	}
	mock.ExpectQuery("SELECT.*id.*FROM.*users.*COALESCE.*firstname.*role").WithArgs(int64(1), "%First Last%", "Admin", 101).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(2))
	response, err = service.SearchProfiles(as("legacy"), &pb.SearchProfilesRequest{Query: "First Last", MatchMode: "full", Role: "Admin", AfterId: 1})
	if err != nil || response.NextAfterId != 0 || len(response.Ids) != 1 {
		t.Fatalf("full search=%v err=%v", response, err)
	}
	mock.ExpectQuery("SELECT.*id.*FROM.*users").WithArgs(int64(0), 101).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	if _, err := service.SearchProfiles(as("audit"), &pb.SearchProfilesRequest{}); err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("SELECT").WillReturnError(errors.New("SECRET_SEARCH_FAILURE"))
	if _, err := service.SearchProfiles(as("audit"), &pb.SearchProfilesRequest{}); status.Code(err) != codes.Unavailable || strings.Contains(err.Error(), "SECRET") {
		t.Fatal("search failure not explicit/redacted")
	}
	for _, request := range []*pb.SearchProfilesRequest{nil, {Query: strings.Repeat("a", 1025)}, {AfterId: -1}, {Limit: 101}, {MatchMode: "unapproved"}} {
		if _, err := service.SearchProfiles(as("audit"), request); status.Code(err) != codes.InvalidArgument {
			t.Fatal("invalid search accepted")
		}
	}
	if _, err := service.SearchProfiles(as("audit"), &pb.SearchProfilesRequest{Role: "Admin"}); status.Code(err) != codes.PermissionDenied {
		t.Fatal("Audit inferred principal membership")
	}
}

func TestGRPCRequiresOriginalTokenAndMethodPolicy(t *testing.T) {
	service, _ := testService(t)
	verify := func(ctx context.Context, token, audience string, accounts map[string]bool) (string, error) {
		if audience != "https://identity.example" || !accounts[token] {
			return "", errors.New("invalid signed token")
		}
		return token, nil
	}
	server, err := newGRPC(service, "https://identity.example", verify)
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1 << 20)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); _ = listener.Close() })
	connection, err := grpc.NewClient("passthrough:///identity", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	client := pb.NewIdentityClient(connection)
	for _, tc := range []struct {
		headers []string
		code    codes.Code
	}{{nil, codes.Unauthenticated}, {[]string{"x-serverless-authorization", "Bearer audit"}, codes.Unauthenticated}, {[]string{"authorization", "Bearer stranger"}, codes.PermissionDenied}, {[]string{"authorization", "Bearer audit", "authorization", "Bearer audit"}, codes.Unauthenticated}} {
		ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs(tc.headers...))
		if _, err := client.BatchGetProfiles(ctx, &pb.BatchGetProfilesRequest{Ids: []int64{1}}); status.Code(err) != tc.code {
			t.Fatalf("code %v want %v", status.Code(err), tc.code)
		}
	}
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", "Bearer audit"))
	if _, err := client.BatchGetPrincipals(ctx, &pb.BatchGetPrincipalsRequest{Ids: []int64{1}}); status.Code(err) != codes.PermissionDenied {
		t.Fatal("wrong method grant accepted")
	}
	if _, err := client.BatchGetProfiles(ctx, &pb.BatchGetProfilesRequest{Ids: []int64{1}, IncludeEmail: true}); status.Code(err) != codes.PermissionDenied {
		t.Fatal("email grant bypassed")
	}
	if _, err := client.BatchGetProfiles(ctx, &pb.BatchGetProfilesRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatal("valid caller did not reach bounded contract")
	}
	if _, err := newGRPC(nil, "https://identity.example", verify); err == nil {
		t.Fatal("missing service accepted")
	}
	if _, err := newGRPC(service, "", verify); err == nil {
		t.Fatal("missing audience accepted")
	}
	if _, err := newGRPC(service, "https://identity.example", nil); err == nil {
		t.Fatal("missing verifier accepted")
	}
	production, err := NewGRPC(service, "https://identity.example")
	if err != nil {
		t.Fatal(err)
	}
	production.Stop()
}
