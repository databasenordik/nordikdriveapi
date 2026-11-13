package identityclient

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"google.golang.org/grpc"
	pb "nordik-drive-api/internal/identitypb"
)

type fakeRPC struct {
	profiles   func(context.Context, *pb.BatchGetProfilesRequest) (*pb.BatchGetProfilesResponse, error)
	principals func(context.Context, *pb.BatchGetPrincipalsRequest) (*pb.BatchGetPrincipalsResponse, error)
	search     func(context.Context, *pb.SearchProfilesRequest) (*pb.SearchProfilesResponse, error)
}

func (f fakeRPC) BatchGetProfiles(ctx context.Context, r *pb.BatchGetProfilesRequest, _ ...grpc.CallOption) (*pb.BatchGetProfilesResponse, error) {
	return f.profiles(ctx, r)
}
func (f fakeRPC) BatchGetPrincipals(ctx context.Context, r *pb.BatchGetPrincipalsRequest, _ ...grpc.CallOption) (*pb.BatchGetPrincipalsResponse, error) {
	return f.principals(ctx, r)
}
func (f fakeRPC) SearchProfiles(ctx context.Context, r *pb.SearchProfilesRequest, _ ...grpc.CallOption) (*pb.SearchProfilesResponse, error) {
	return f.search(ctx, r)
}

func TestBatchesAreBoundedAndComplete(t *testing.T) {
	ids := []int64{}
	for i := int64(1); i <= 205; i++ {
		ids = append(ids, i)
	}
	ids = append(ids, 1)
	profileCalls, principalCalls := 0, 0
	client := &Client{RPC: fakeRPC{profiles: func(ctx context.Context, r *pb.BatchGetProfilesRequest) (*pb.BatchGetProfilesResponse, error) {
		profileCalls++
		if len(r.Ids) > 100 || !r.IncludeEmail {
			t.Fatal("unbounded/wrong-profile request")
		}
		out := &pb.BatchGetProfilesResponse{}
		for _, id := range r.Ids {
			out.Profiles = append(out.Profiles, &pb.Profile{Id: id, Email: fmt.Sprintf("user%d@example.test", id)})
		}
		return out, nil
	}, principals: func(ctx context.Context, r *pb.BatchGetPrincipalsRequest) (*pb.BatchGetPrincipalsResponse, error) {
		principalCalls++
		out := &pb.BatchGetPrincipalsResponse{}
		for _, id := range r.Ids {
			out.Principals = append(out.Principals, &pb.Principal{Id: id, Role: "User", Communities: []string{"north"}})
		}
		return out, nil
	}}}
	profiles, err := client.Profiles(context.Background(), ids, true)
	if err != nil || len(profiles) != 205 || profileCalls != 3 {
		t.Fatalf("profiles=%d calls=%d err=%v", len(profiles), profileCalls, err)
	}
	principals, err := client.Principals(context.Background(), ids)
	if err != nil || len(principals) != 205 || principalCalls != 3 {
		t.Fatal("principal batching failed")
	}
	if empty, err := client.Profiles(context.Background(), nil, false); err != nil || len(empty) != 0 {
		t.Fatal("empty batch failed")
	}
	if _, err := client.Profiles(context.Background(), []int64{0}, false); err == nil {
		t.Fatal("invalid profile ID accepted")
	}
	if _, err := client.Principals(context.Background(), []int64{-1}); err == nil {
		t.Fatal("invalid principal ID accepted")
	}
}

func TestProfileContractFailuresDoNotReturnPartialSuccess(t *testing.T) {
	responses := []*pb.BatchGetProfilesResponse{nil, {}, {Profiles: []*pb.Profile{nil}}, {Profiles: []*pb.Profile{{Id: 2}}}, {Profiles: []*pb.Profile{{Id: 1, Email: "private@example.test"}}}, {MissingIds: []int64{2}}, {Profiles: []*pb.Profile{{Id: 1}}, MissingIds: []int64{1}}}
	for _, response := range responses {
		client := &Client{RPC: fakeRPC{profiles: func(context.Context, *pb.BatchGetProfilesRequest) (*pb.BatchGetProfilesResponse, error) {
			return response, nil
		}}}
		if result, err := client.Profiles(context.Background(), []int64{1}, false); err == nil || result != nil {
			t.Fatal("corrupt/partial profile response succeeded")
		}
	}
	client := &Client{RPC: fakeRPC{profiles: func(context.Context, *pb.BatchGetProfilesRequest) (*pb.BatchGetProfilesResponse, error) {
		return nil, errors.New("SECRET_REMOTE_ERROR")
	}}}
	if _, err := client.Profiles(context.Background(), []int64{1}, false); err == nil || err.Error() != "identity profiles unavailable" {
		t.Fatal("remote error not redacted")
	}
	client.RPC = fakeRPC{profiles: func(context.Context, *pb.BatchGetProfilesRequest) (*pb.BatchGetProfilesResponse, error) {
		return &pb.BatchGetProfilesResponse{MissingIds: []int64{1}}, nil
	}}
	if result, err := client.Profiles(context.Background(), []int64{1}, false); err != nil || len(result) != 0 {
		t.Fatal("explicit missing profile rejected")
	}
}

func TestPrincipalContractFailuresAreExplicit(t *testing.T) {
	for _, response := range []*pb.BatchGetPrincipalsResponse{nil, {}, {Principals: []*pb.Principal{nil}}, {Principals: []*pb.Principal{{Id: 2}}}, {MissingIds: []int64{2}}, {Principals: []*pb.Principal{{Id: 1}}, MissingIds: []int64{1}}} {
		client := &Client{RPC: fakeRPC{principals: func(context.Context, *pb.BatchGetPrincipalsRequest) (*pb.BatchGetPrincipalsResponse, error) {
			return response, nil
		}}}
		if result, err := client.Principals(context.Background(), []int64{1}); err == nil || result != nil {
			t.Fatal("corrupt principal response succeeded")
		}
	}
	client := &Client{RPC: fakeRPC{principals: func(context.Context, *pb.BatchGetPrincipalsRequest) (*pb.BatchGetPrincipalsResponse, error) {
		return nil, errors.New("SECRET")
	}}}
	if _, err := client.Principals(context.Background(), []int64{1}); err == nil {
		t.Fatal("remote failure ignored")
	}
	client.RPC = fakeRPC{principals: func(context.Context, *pb.BatchGetPrincipalsRequest) (*pb.BatchGetPrincipalsResponse, error) {
		return &pb.BatchGetPrincipalsResponse{MissingIds: []int64{1}}, nil
	}}
	if result, err := client.Principals(context.Background(), []int64{1}); err != nil || len(result) != 0 {
		t.Fatal("explicit missing principal rejected")
	}
}

func TestSearchResolvesAllIDsOrFails(t *testing.T) {
	calls := 0
	client := &Client{RPC: fakeRPC{search: func(ctx context.Context, r *pb.SearchProfilesRequest) (*pb.SearchProfilesResponse, error) {
		calls++
		if r.Query != "A_%" || r.MatchMode != "parts" || r.Role != "Admin" || r.Limit != 100 {
			t.Fatal("search semantics changed")
		}
		if calls == 1 {
			return &pb.SearchProfilesResponse{Ids: []int64{1, 2}, NextAfterId: 2}, nil
		}
		if r.AfterId != 2 {
			t.Fatal("wrong search cursor")
		}
		return &pb.SearchProfilesResponse{Ids: []int64{3}}, nil
	}}}
	ids, err := client.Search(context.Background(), "A_%", "parts", "Admin")
	if err != nil || !reflect.DeepEqual(ids, []int64{1, 2, 3}) {
		t.Fatal("paginated search incomplete")
	}
	for _, response := range []*pb.SearchProfilesResponse{nil, {Ids: make([]int64, 101)}, {Ids: []int64{1, 1}}, {Ids: []int64{0}}, {NextAfterId: 1}, {Ids: []int64{1}, NextAfterId: 2}} {
		client.RPC = fakeRPC{search: func(context.Context, *pb.SearchProfilesRequest) (*pb.SearchProfilesResponse, error) {
			return response, nil
		}}
		if result, err := client.Search(context.Background(), "", "", ""); err == nil || result != nil {
			t.Fatal("corrupt search succeeded")
		}
	}
	client.RPC = fakeRPC{search: func(context.Context, *pb.SearchProfilesRequest) (*pb.SearchProfilesResponse, error) {
		return nil, errors.New("SECRET")
	}}
	if result, err := client.Search(context.Background(), "", "", ""); err == nil || result != nil {
		t.Fatal("failed search returned partial success")
	}
}

func TestConfigurationDoesNotSilentlyFallback(t *testing.T) {
	t.Setenv("IDENTITY_URL", "")
	directory, closeClient, err := Configured(context.Background())
	if err != nil || directory != nil || closeClient == nil {
		t.Fatal("compatibility default changed")
	}
	closeClient()
	for _, address := range []string{"not-url", "http://identity.example", "https://user:password@identity.example", "https://identity.example/path", "https://identity.example?q=x", "https://identity.example#x"} {
		t.Setenv("IDENTITY_URL", address)
		if _, _, err := Configured(context.Background()); err == nil {
			t.Fatal("invalid configured owner silently accepted")
		}
	}
}

func TestConfiguredOwnerDoesNotFallbackWhenCredentialsFail(t *testing.T) {
	t.Setenv("IDENTITY_URL", "https://identity.example")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(t.TempDir(), "missing-credentials.json"))
	if directory, _, err := Configured(context.Background()); err == nil || directory != nil {
		t.Fatal("configured owner silently fell back when workload credentials failed")
	}
}
