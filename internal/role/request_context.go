package role

import (
	"context"

	"nordik-drive-api/internal/requestscope"
)

// WithContext returns a request-local copy; shared service values stay immutable.
func (s *RoleService) WithContext(ctx context.Context) any {
	if s == nil {
		return (*RoleService)(nil)
	}
	copy := *s
	if s.DB != nil && s.DB.Config != nil && s.DB.Statement != nil {
		copy.DB = s.DB.WithContext(ctx)
	}
	return &copy
}
func (c *RoleController) withContext(ctx context.Context) *RoleController {
	copy := *c
	copy.RoleService = requestscope.Service(ctx, c.RoleService)
	return &copy
}
