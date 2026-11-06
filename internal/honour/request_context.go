package honour

import (
	"context"

	"nordik-drive-api/internal/requestscope"
)

// WithContext returns a request-local copy; shared service values stay immutable.
func (s *Service) WithContext(ctx context.Context) any {
	if s == nil {
		return (*Service)(nil)
	}
	copy := *s
	if s.DB != nil && s.DB.Config != nil && s.DB.Statement != nil {
		copy.DB = s.DB.WithContext(ctx)
	}
	copy.Generator = requestscope.Service(ctx, s.Generator)
	return &copy
}
func (c *Controller) withContext(ctx context.Context) *Controller {
	copy := *c
	copy.Service = requestscope.Service(ctx, c.Service)
	return &copy
}
