package auth

import (
	"context"

	"nordik-drive-api/internal/requestscope"
)

// WithContext returns a request-local copy; shared service values stay immutable.
func (s *AuthService) WithContext(ctx context.Context) any {
	if s == nil {
		return (*AuthService)(nil)
	}
	copy := *s
	if s.DB != nil && s.DB.Config != nil && s.DB.Statement != nil {
		copy.DB = s.DB.WithContext(ctx)
	}
	copy.Mailer = requestscope.Service(ctx, s.Mailer)
	return &copy
}
func (c *AuthController) withContext(ctx context.Context) *AuthController {
	copy := *c
	copy.AuthService = requestscope.Service(ctx, c.AuthService)
	copy.LS = requestscope.Service(ctx, c.LS)
	return &copy
}
