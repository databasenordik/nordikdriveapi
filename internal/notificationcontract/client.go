package notificationcontract

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

type Client struct {
	URL    string
	Tokens oauth2.TokenSource
	HTTP   *http.Client
}

func NewClient(address string, tokens oauth2.TokenSource) (*Client, error) {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || tokens == nil {
		return nil, ErrInvalid
	}
	return &Client{URL: strings.TrimRight(address, "/"), Tokens: tokens, HTTP: &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (c *Client) call(ctx context.Context, path string, input, output any) error {
	b, err := json.Marshal(input)
	if err != nil {
		return err
	}
	r, err := http.NewRequestWithContext(ctx, "POST", c.URL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	token, err := c.Tokens.Token()
	if err != nil || token == nil || !token.Valid() {
		return errors.New("notification credentials unavailable")
	}
	r.Header.Set("Authorization", "Bearer "+token.AccessToken)
	// Cloud Run verifies this header; the original Authorization remains signed
	// for endpoint-specific application checks.
	r.Header.Set("X-Serverless-Authorization", "Bearer "+token.AccessToken)
	r.Header.Set("Content-Type", "application/json")
	response, err := c.HTTP.Do(r)
	if err != nil {
		return errors.New("notification service unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return errors.New("notification operation not accepted")
	}
	if output != nil {
		return json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(output)
	}
	return nil
}
func (c *Client) Accept(ctx context.Context, command Command) (string, error) {
	var result struct {
		ID string `json:"delivery_id"`
	}
	err := c.call(ctx, "/internal/v1/notifications", command, &result)
	if err == nil && result.ID == "" {
		err = ErrInvalid
	}
	return result.ID, err
}
func (c *Client) Status(ctx context.Context, ids []string) (map[string]string, error) {
	var result map[string]string
	err := c.call(ctx, "/internal/v1/status", map[string]any{"ids": ids}, &result)
	return result, err
}
func (c *Client) Recover(ctx context.Context) error {
	return c.call(ctx, "/internal/v1/recover", struct{}{}, nil)
}
