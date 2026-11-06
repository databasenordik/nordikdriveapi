package file

import (
	"context"

	"nordik-drive-api/internal/requestscope"
)

// WithContext returns a request-local copy; shared service values stay immutable.
func (s *FileService) WithContext(ctx context.Context) any {
	if s == nil {
		return (*FileService)(nil)
	}
	copy := *s
	if s.DB != nil && s.DB.Config != nil && s.DB.Statement != nil {
		copy.DB = s.DB.WithContext(ctx)
	}
	copy.Mailer = requestscope.Service(ctx, s.Mailer)
	return &copy
}
func (c *FileController) withContext(ctx context.Context) *FileController {
	copy := *c
	copy.FileService = requestscope.Service(ctx, c.FileService)
	copy.LogService = requestscope.Service(ctx, c.LogService)
	return &copy
}
