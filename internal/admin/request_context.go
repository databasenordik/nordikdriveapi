package admin

import (
	"context"

	"nordik-drive-api/internal/requestscope"
)

// WithContext returns a request-local copy; shared service values stay immutable.
func (s *AdminService) WithContext(ctx context.Context) any {
	if s == nil {
		return (*AdminService)(nil)
	}
	copy := *s
	if s.DB != nil && s.DB.Config != nil && s.DB.Statement != nil {
		copy.DB = s.DB.WithContext(ctx)
	}
	return &copy
}
func (c *AdminController) withContext(ctx context.Context) *AdminController {
	copy := *c
	copy.AdminService = requestscope.Service(ctx, c.AdminService)
	return &copy
}
