package lookup

import (
	"context"

	"nordik-drive-api/internal/requestscope"
)

// WithContext returns a request-local copy; shared service values stay immutable.
func (s *LookupService) WithContext(ctx context.Context) any {
	if s == nil {
		return (*LookupService)(nil)
	}
	copy := *s
	if s.DB != nil && s.DB.Config != nil && s.DB.Statement != nil {
		copy.DB = s.DB.WithContext(ctx)
	}
	return &copy
}
func (c *LookupController) withContext(ctx context.Context) *LookupController {
	copy := *c
	copy.Service = requestscope.Service(ctx, c.Service)
	return &copy
}
