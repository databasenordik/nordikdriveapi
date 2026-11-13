package identityclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

type tokenFailure struct{}

func (tokenFailure) Token() (*oauth2.Token, error) { return nil, errors.New("SECRET") }

func TestPrivateDispatchUsesOnlyFixedOperationAndSignedWorkloadToken(t *testing.T) {
	for _, status := range []int{204, 403, 503} {
		client := &Dispatcher{URL: "https://identity.example", Tokens: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-signed-google-token"}), HTTP: &http.Client{Transport: roundTrip(func(request *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(request.Body)
			if request.URL.Path != "/internal/v1/identity/dispatch-notifications" || request.Method != "POST" || string(body) != "{}" || request.Header.Get("Authorization") != "Bearer test-signed-google-token" || request.Header.Get("X-Serverless-Authorization") != request.Header.Get("Authorization") {
				t.Fatal("private operation or caller credentials changed")
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("SECRET_RESPONSE"))}, nil
		})}}
		err := client.Run(context.Background())
		if (err == nil) != (status == 204) || (err != nil && strings.Contains(err.Error(), "SECRET")) {
			t.Fatal("dispatch failure not explicit/redacted")
		}
	}
	client := &Dispatcher{URL: "https://identity.example", Tokens: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "token"}), HTTP: &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) { return nil, errors.New("SECRET_TRANSPORT_ERROR") })}}
	if err := client.Run(context.Background()); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatal("transport error leaked or hidden")
	}
	client.URL = "%"
	if client.Run(context.Background()) == nil {
		t.Fatal("bad request accepted")
	}
	client.Tokens = tokenFailure{}
	if client.Run(context.Background()) == nil {
		t.Fatal("token failure accepted")
	}
	client.Tokens = oauth2.StaticTokenSource(&oauth2.Token{})
	if client.Run(context.Background()) == nil {
		t.Fatal("invalid token accepted")
	}
	if (*Dispatcher)(nil).Run(context.Background()) == nil {
		t.Fatal("nil dispatcher accepted")
	}
	if value, err := NewDispatcher(context.Background(), ""); err != nil || value != nil {
		t.Fatal("compatibility default changed")
	}
	for _, address := range []string{"http://identity.example", "https://user:password@identity.example", "https://identity.example/path", "https://identity.example?q=x", "https://identity.example#x"} {
		if _, err := NewDispatcher(context.Background(), address); err == nil {
			t.Fatal("invalid owner origin accepted")
		}
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(t.TempDir(), "missing.json"))
	if _, err := NewDispatcher(context.Background(), "https://identity.example"); err == nil {
		t.Fatal("missing workload credentials silently accepted")
	}
}

type directoryStub struct {
	calls  int
	failed bool
}

func (s *directoryStub) Profiles(ctx context.Context, ids []int64, email bool) (map[int64]Profile, error) {
	s.calls++
	if s.failed {
		return nil, errors.New("unavailable")
	}
	return map[int64]Profile{1: {ID: 1, Firstname: "Name", Email: "recipient@example.test"}}, nil
}
func (s *directoryStub) Principals(context.Context, []int64) (map[int64]Principal, error) {
	panic("unexpected principal lookup")
}
func (s *directoryStub) Search(context.Context, string, string, string) ([]int64, error) {
	panic("unexpected search")
}

func TestSnapshotPerformsNoRemoteWorkInsideBusinessTransaction(t *testing.T) {
	owner := &directoryStub{}
	snapshot, err := SnapshotProfiles(context.Background(), owner, []int64{1, 2}, true)
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := snapshot.Profiles(context.Background(), []int64{1, 2}, true)
	if err != nil || len(profiles) != 1 || profiles[1].Email == "" {
		t.Fatal("snapshot lost explicit missing IDs or recipient")
	}
	profiles[1] = Profile{ID: 1, Firstname: "mutated"}
	profiles, err = snapshot.Profiles(context.Background(), []int64{1}, false)
	if err != nil || profiles[1].Email != "" || profiles[1].Firstname != "Name" || owner.calls != 1 {
		t.Fatal("snapshot was mutable, leaked email, or made another RPC")
	}
	if _, err := snapshot.Profiles(context.Background(), []int64{3}, true); err == nil {
		t.Fatal("unprepared recipient caused hidden remote lookup")
	}
	if _, err := snapshot.Principals(context.Background(), []int64{1}); err == nil {
		t.Fatal("unprepared principals accepted")
	}
	if _, err := snapshot.Search(context.Background(), "", "", ""); err == nil {
		t.Fatal("unprepared search accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := snapshot.Profiles(ctx, []int64{1}, false); !errors.Is(err, context.Canceled) {
		t.Fatal("snapshot ignored cancellation")
	}
	public, err := SnapshotProfiles(context.Background(), owner, []int64{1}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := public.Profiles(context.Background(), []int64{1}, true); err == nil {
		t.Fatal("snapshot upgraded email permission")
	}
	owner.failed = true
	if _, err := SnapshotProfiles(context.Background(), owner, []int64{1}, true); err == nil {
		t.Fatal("snapshot hid owner outage")
	}
	if _, err := SnapshotProfiles(context.Background(), nil, []int64{1}, true); err == nil {
		t.Fatal("missing directory accepted")
	}
}
