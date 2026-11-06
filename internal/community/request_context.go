package community

import (
	"context"

	"nordik-drive-api/internal/requestscope"
)

// WithContext returns a request-local copy; shared service values stay immutable.
func (s *CommunityService) WithContext(ctx context.Context) any {
	if s == nil {
		return (*CommunityService)(nil)
	}
	copy := *s
	if s.DB != nil && s.DB.Config != nil && s.DB.Statement != nil {
		copy.DB = s.DB.WithContext(ctx)
	}
	return &copy
}
func (c *CommunityController) withContext(ctx context.Context) *CommunityController {
	copy := *c
	copy.CommunityService = requestscope.Service(ctx, c.CommunityService)
	return &copy
}
