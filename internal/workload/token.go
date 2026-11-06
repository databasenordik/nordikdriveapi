// Package workload keeps Google workload credentials separate from user cookies.
package workload

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/idtoken"
	"google.golang.org/grpc/credentials"
)

func Source(ctx context.Context, audience string) (oauth2.TokenSource, error) {
	u, parseErr := url.Parse(audience)
	if parseErr != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("workload audience must be an HTTPS service URL")
	}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Timeout: 5 * time.Second})
	s, err := idtoken.NewTokenSource(ctx, audience)
	if err != nil {
		return nil, errors.New("workload token source initialization failed")
	}
	return oauth2.ReuseTokenSource(nil, s), nil
}

type Credentials struct{ Source oauth2.TokenSource }

var _ credentials.PerRPCCredentials = Credentials{}

func (c Credentials) GetRequestMetadata(ctx context.Context, _ ...string) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.Source == nil {
		return nil, errors.New("workload token source unavailable")
	}
	token, err := c.Source.Token()
	if err != nil || token == nil || !token.Valid() {
		return nil, errors.New("workload token unavailable")
	}
	return map[string]string{"x-serverless-authorization": "Bearer " + token.AccessToken}, nil
}
func (Credentials) RequireTransportSecurity() bool { return true }

// Verify checks the original signed token, never Cloud Run's signature-stripped
// X-Serverless-Authorization header. Only explicit service accounts are admitted.
func Verify(ctx context.Context, encoded, audience string, accounts map[string]bool) error {
	_, err := VerifiedAccount(ctx, encoded, audience, accounts)
	return err
}
func VerifiedAccount(ctx context.Context, encoded, audience string, accounts map[string]bool) (string, error) {
	return verifiedAccount(ctx, encoded, audience, accounts, idtoken.Validate)
}
func verifiedAccount(ctx context.Context, encoded, audience string, accounts map[string]bool, validate func(context.Context, string, string) (*idtoken.Payload, error)) (string, error) {
	claims, err := validate(ctx, encoded, audience)
	if err != nil {
		return "", errors.New("invalid workload token")
	}
	if claims == nil || claims.Subject == "" || (claims.Issuer != "accounts.google.com" && claims.Issuer != "https://accounts.google.com") {
		return "", errors.New("invalid workload issuer or subject")
	}
	email, _ := claims.Claims["email"].(string)
	verified, _ := claims.Claims["email_verified"].(bool)
	if !verified || !accounts[email] {
		return "", errors.New("workload caller is not allowed")
	}
	return email, nil
}
