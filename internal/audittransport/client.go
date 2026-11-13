// Package audittransport is the signed private HTTP contract between owners and Audit.
package audittransport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/oauth2"
	"nordik-drive-api/internal/auditoutbox"
	"nordik-drive-api/internal/workload"
)

var ErrNotFound = errors.New("audit owner event not found")

type Client struct {
	URL    string
	Tokens oauth2.TokenSource
	HTTP   *http.Client
}

func New(ctx context.Context, address string) (*Client, error) {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("exact HTTPS audit owner audience required")
	}
	tokens, err := workload.Source(ctx, address)
	if err != nil {
		return nil, errors.New("audit owner identity unavailable")
	}
	return &Client{URL: address, Tokens: tokens, HTTP: &http.Client{Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (c *Client) call(ctx context.Context, path string, input, output any) error {
	b, err := json.Marshal(input)
	if err != nil {
		return errors.New("invalid audit request")
	}
	if c.Tokens == nil {
		return errors.New("audit caller token unavailable")
	}
	token, err := c.Tokens.Token()
	if err != nil || token == nil || !token.Valid() {
		return errors.New("audit caller token unavailable")
	}
	request, err := http.NewRequestWithContext(ctx, "POST", c.URL+path, bytes.NewReader(b))
	if err != nil {
		return errors.New("invalid audit owner URL")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token.AccessToken)
	request.Header.Set("X-Serverless-Authorization", "Bearer "+token.AccessToken)
	response, err := c.HTTP.Do(request)
	if err != nil {
		return errors.New("audit owner unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode == 404 {
		return ErrNotFound
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return errors.New("audit owner rejected request")
	}
	if output != nil {
		decoder := json.NewDecoder(io.LimitReader(response.Body, 128*1024))
		decoder.DisallowUnknownFields()
		if decoder.Decode(output) != nil || decoder.Decode(&struct{}{}) != io.EOF {
			return errors.New("invalid audit owner response")
		}
	}
	return nil
}
func (c *Client) Read(ctx context.Context, id string) (auditoutbox.SourceRecord, error) {
	ctx, stop := context.WithTimeout(ctx, 8*time.Second)
	defer stop()
	var record auditoutbox.SourceRecord
	err := c.call(ctx, "/internal/v1/audit-outbox/read", map[string]string{"event_id": id}, &record)
	return record, err
}
func (c *Client) Ack(ctx context.Context, receipt auditoutbox.Receipt) error {
	ctx, stop := context.WithTimeout(ctx, 8*time.Second)
	defer stop()
	return c.call(ctx, "/internal/v1/audit-outbox/ack", receipt, nil)
}
func (c *Client) Dispatch(ctx context.Context) error {
	ctx, stop := context.WithTimeout(ctx, 25*time.Second)
	defer stop()
	return c.call(ctx, "/internal/v1/audit-outbox/dispatch", struct{}{}, nil)
}
