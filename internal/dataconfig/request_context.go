package dataconfig

import (
	"context"

	"nordik-drive-api/internal/requestscope"
)

// WithContext returns a request-local copy; shared service values stay immutable.
func (s *DataConfigService) WithContext(ctx context.Context) any {
	if s == nil {
		return (*DataConfigService)(nil)
	}
	copy := *s
	if s.DB != nil && s.DB.Config != nil && s.DB.Statement != nil {
		copy.DB = s.DB.WithContext(ctx)
	}
	return &copy
}
func (c *DataConfigController) withContext(ctx context.Context) *DataConfigController {
	copy := *c
	copy.DataConfigService = requestscope.Service(ctx, c.DataConfigService)
	return &copy
}
