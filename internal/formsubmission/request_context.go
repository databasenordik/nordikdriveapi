package formsubmission

import (
	"context"

	"nordik-drive-api/internal/requestscope"
)

// WithContext returns a request-local copy; shared service values stay immutable.
func (s *FormSubmissionService) WithContext(ctx context.Context) any {
	if s == nil {
		return (*FormSubmissionService)(nil)
	}
	copy := *s
	if s.DB != nil && s.DB.Config != nil && s.DB.Statement != nil {
		copy.DB = s.DB.WithContext(ctx)
	}
	copy.Mailer = requestscope.Service(ctx, s.Mailer)
	return &copy
}
func (c *FormSubmissionController) withContext(ctx context.Context) *FormSubmissionController {
	copy := *c
	copy.FormSubmissionService = requestscope.Service(ctx, c.FormSubmissionService)
	return &copy
}
