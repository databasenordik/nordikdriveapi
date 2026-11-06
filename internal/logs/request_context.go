package logs

import (
	"context"

	"nordik-drive-api/internal/requestscope"
)

// WithContext returns a request-local copy; shared service values stay immutable.
func (s *LogService) WithContext(ctx context.Context) any {
	if s == nil {
		return (*LogService)(nil)
	}
	copy := *s
	if s.DB != nil && s.DB.Config != nil && s.DB.Statement != nil {
		copy.DB = s.DB.WithContext(ctx)
	}
	return &copy
}
func (c *LogController) withContext(ctx context.Context) *LogController {
	copy := *c
	copy.LogService = requestscope.Service(ctx, c.LogService)
	return &copy
}
