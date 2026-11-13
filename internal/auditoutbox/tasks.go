package auditoutbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"

	"google.golang.org/api/cloudtasks/v2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

type TaskPublisher struct {
	API                 *cloudtasks.Service
	Queue, URL, Account string
}

func NewTaskPublisher(ctx context.Context, queue, address, account string, options ...option.ClientOption) (*TaskPublisher, error) {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || queue == "" || account == "" {
		return nil, errors.New("audit queue, exact HTTPS audience and task identity required")
	}
	api, err := cloudtasks.NewService(ctx, options...)
	if err != nil {
		return nil, errors.New("audit task client unavailable")
	}
	return &TaskPublisher{API: api, Queue: queue, URL: address, Account: account}, nil
}
func Configure(ctx context.Context, producer string) (*Writer, error) {
	return configure(ctx, producer)
}
func configure(ctx context.Context, producer string, options ...option.ClientOption) (*Writer, error) {
	raw := os.Getenv("AUDIT_EVENTS_ENABLED")
	if raw == "" {
		return nil, nil
	}
	enabled, err := strconv.ParseBool(raw)
	if err != nil {
		return nil, errors.New("AUDIT_EVENTS_ENABLED must be a boolean")
	}
	if !enabled {
		return nil, nil
	}
	if producer != "identity" && producer != "legacy" {
		return nil, errors.New("invalid audit producer")
	}
	publisher, err := NewTaskPublisher(ctx, os.Getenv("AUDIT_QUEUE"), os.Getenv("AUDIT_URL"), os.Getenv("AUDIT_TASK_ACCOUNT"), options...)
	if err != nil {
		return nil, err
	}
	return &Writer{Producer: producer, Publisher: publisher}, nil
}
func (p *TaskPublisher) Enqueue(ctx context.Context, id string, attempt int) error {
	body, _ := json.Marshal(map[string]string{"event_id": id})
	task := &cloudtasks.Task{Name: fmt.Sprintf("%s/tasks/%s-%d", p.Queue, id, attempt), DispatchDeadline: "45s", HttpRequest: &cloudtasks.HttpRequest{HttpMethod: "POST", Url: p.URL + "/internal/v1/audit/ingest", Headers: map[string]string{"Content-Type": "application/json"}, Body: base64.StdEncoding.EncodeToString(body), OidcToken: &cloudtasks.OidcToken{Audience: p.URL, ServiceAccountEmail: p.Account}}}
	_, err := p.API.Projects.Locations.Queues.Tasks.Create(p.Queue, &cloudtasks.CreateTaskRequest{Task: task}).Context(ctx).Do()
	var apiError *googleapi.Error
	if err == nil || (errors.As(err, &apiError) && apiError.Code == 409) {
		return nil
	}
	return errors.New("audit task publication failed")
}
