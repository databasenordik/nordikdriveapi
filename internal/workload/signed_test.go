package workload

import (
	"context"
	"testing"

	"golang.org/x/oauth2"
)

func TestSignedCredentialsPreserveOriginalAndPlatformTokens(t *testing.T) {
	credentials := SignedCredentials{Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "signed-test-token"})}
	headers, err := credentials.GetRequestMetadata(context.Background())
	if err != nil || headers["authorization"] != "Bearer signed-test-token" || headers["x-serverless-authorization"] != headers["authorization"] || !credentials.RequireTransportSecurity() {
		t.Fatal("signed original/platform authentication separated incorrectly")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := credentials.GetRequestMetadata(ctx); err == nil {
		t.Fatal("credential request ignored cancellation")
	}
	if _, err := (SignedCredentials{}).GetRequestMetadata(context.Background()); err == nil {
		t.Fatal("missing token source accepted")
	}
}
