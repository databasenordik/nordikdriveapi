package logocontent

import (
	"context"

	"nordik-drive-api/internal/requestscope"
)

// WithContext returns a request-local copy; shared service values stay immutable.
func (s *LogoContentService) WithContext(ctx context.Context) any {
	if s == nil {
		return (*LogoContentService)(nil)
	}
	copy := *s
	if s.DB != nil && s.DB.Config != nil && s.DB.Statement != nil {
		copy.DB = s.DB.WithContext(ctx)
	}
	return &copy
}
func (c *LogoContentController) withContext(ctx context.Context) *LogoContentController {
	copy := *c
	copy.Service = requestscope.Service(ctx, c.Service)
	return &copy
}
