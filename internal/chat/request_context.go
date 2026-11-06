package chat

import (
	"context"

	"golang.org/x/sync/singleflight"

	"nordik-drive-api/internal/requestscope"
)

func (cs *ChatService) WithContext(ctx context.Context) any {
	if cs == nil {
		return (*ChatService)(nil)
	}
	db := cs.DB
	if db != nil && db.Config != nil && db.Statement != nil {
		db = db.WithContext(ctx)
	}
	return &ChatService{DB: db, APIKey: cs.APIKey, Client: cs.Client, ProjectID: cs.ProjectID, Location: cs.Location, QueryStrategy: cs.QueryStrategy, state: cs.sharedState()}
}

func coalesced(ctx context.Context, group *singleflight.Group, key string, operation func() (any, error)) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := group.DoChan(key, operation)
	select {
	case outcome := <-result:
		return outcome.Val, outcome.Err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (c *ChatController) withContext(ctx context.Context) *ChatController {
	copy := *c
	copy.ChatService = requestscope.Service(ctx, c.ChatService)
	return &copy
}
