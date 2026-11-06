package supportschedule

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
	copy.Mailer = requestscope.Service(ctx, s.Mailer)
	return &copy
}
func (c *Controller) withContext(ctx context.Context) *Controller {
	copy := *c
	copy.Service = requestscope.Service(ctx, c.Service)
	return &copy
}
