package supportrequest

import (
	"context"

	"nordik-drive-api/internal/requestscope"
)

// WithContext returns a request-local copy; shared service values stay immutable.
func (s *SupportRequestService) WithContext(ctx context.Context) any {
	if s == nil {
		return (*SupportRequestService)(nil)
	}
	copy := *s
	if s.DB != nil && s.DB.Config != nil && s.DB.Statement != nil {
		copy.DB = s.DB.WithContext(ctx)
	}
	copy.Mailer = requestscope.Service(ctx, s.Mailer)
	return &copy
}
func (c *SupportRequestController) withContext(ctx context.Context) *SupportRequestController {
	copy := *c
	copy.SupportRequestService = requestscope.Service(ctx, c.SupportRequestService)
	return &copy
}
