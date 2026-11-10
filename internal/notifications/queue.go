package notifications

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"google.golang.org/api/cloudtasks/v2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

type CloudQueue struct {
	API                       *cloudtasks.Service
	OTP, Normal, URL, Account string
}

func NewCloudQueue(ctx context.Context, otp, normal, address, account string, options ...option.ClientOption) (*CloudQueue, error) {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || otp == "" || normal == "" || account == "" {
		return nil, errors.New("invalid notification queue configuration")
	}
	api, err := cloudtasks.NewService(ctx, options...)
	if err != nil {
		return nil, errors.New("task client initialization failed")
	}
	return &CloudQueue{API: api, OTP: otp, Normal: normal, URL: address, Account: account}, nil
}
func (q *CloudQueue) Enqueue(ctx context.Context, row Delivery) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	parent := q.Normal
	if row.Kind == "otp" {
		parent = q.OTP
	}
	body, _ := json.Marshal(map[string]string{"delivery_id": row.ID})
	task := &cloudtasks.Task{
		Name:             fmt.Sprintf("%s/tasks/%s-%d", parent, row.ID, row.DispatchGeneration),
		DispatchDeadline: "45s",
		HttpRequest:      &cloudtasks.HttpRequest{HttpMethod: "POST", Url: q.URL + "/internal/v1/deliver", Headers: map[string]string{"Content-Type": "application/json"}, Body: base64.StdEncoding.EncodeToString(body), OidcToken: &cloudtasks.OidcToken{ServiceAccountEmail: q.Account, Audience: q.URL}},
	}
	_, err := q.API.Projects.Locations.Queues.Tasks.Create(parent, &cloudtasks.CreateTaskRequest{Task: task}).Context(ctx).Do()
	var apiError *googleapi.Error
	if errors.As(err, &apiError) && apiError.Code == 409 {
		return nil
	}
	return err
}
