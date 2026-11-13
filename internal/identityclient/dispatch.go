package identityclient

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/oauth2"
	"nordik-drive-api/internal/workload"
)

type Dispatcher struct {
	URL    string
	Tokens oauth2.TokenSource
	HTTP   *http.Client
}

func NewDispatcher(ctx context.Context, address string) (*Dispatcher, error) {
	if address == "" {
		return nil, nil
	}
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("Identity dispatch requires an HTTPS owner origin")
	}
	tokens, err := workload.Source(ctx, address)
	if err != nil {
		return nil, err
	}
	return &Dispatcher{URL: address, Tokens: tokens, HTTP: &http.Client{Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (d *Dispatcher) Run(ctx context.Context) error {
	if d == nil || d.Tokens == nil || d.HTTP == nil {
		return errors.New("Identity dispatch unavailable")
	}
	token, err := d.Tokens.Token()
	if err != nil || token == nil || !token.Valid() {
		return errors.New("Identity dispatch credentials unavailable")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL+"/internal/v1/identity/dispatch-notifications", bytes.NewBufferString("{}"))
	if err != nil {
		return errors.New("Identity dispatch unavailable")
	}
	request.Header.Set("Authorization", "Bearer "+token.AccessToken)
	request.Header.Set("X-Serverless-Authorization", "Bearer "+token.AccessToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := d.HTTP.Do(request)
	if err != nil {
		return errors.New("Identity dispatch unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != 204 {
		return errors.New("Identity dispatch incomplete")
	}
	return nil
}
