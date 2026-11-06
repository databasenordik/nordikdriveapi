package workload

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

type badSource struct{}

func (badSource) Token() (*oauth2.Token, error) { return nil, errors.New("secret-canary") }
func TestCredentialsUseSeparateHeaderAndFailClosed(t *testing.T) {
	for _, source := range []oauth2.TokenSource{badSource{}, oauth2.StaticTokenSource(&oauth2.Token{})} {
		_, err := (Credentials{source}).GetRequestMetadata(context.Background())
		if err == nil || err.Error() == "secret-canary" {
			t.Fatal("unsafe token failure")
		}
	}
	credentials := Credentials{oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fixture-id-token", Expiry: time.Now().Add(time.Hour)})}
	headers, err := credentials.GetRequestMetadata(context.Background())
	if err != nil || headers["x-serverless-authorization"] != "Bearer fixture-id-token" || headers["authorization"] != "" || !credentials.RequireTransportSecurity() {
		t.Fatal("workload and user credentials mixed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := credentials.GetRequestMetadata(ctx); err == nil {
		t.Fatal("cancelled call obtained credentials")
	}
}
func TestRejectInvalidAudienceAndUnsignedOperatorToken(t *testing.T) {
	if _, err := Source(context.Background(), "http://service"); err == nil {
		t.Fatal("insecure audience accepted")
	}
	if Verify(context.Background(), "unsigned", "https://service", map[string]bool{}) == nil {
		t.Fatal("unsigned operator token accepted")
	}
}
