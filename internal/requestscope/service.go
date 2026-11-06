// Package requestscope binds a fresh service value to a request without mutating
// the shared controller or changing the existing domain interfaces.
package requestscope

import (
	"context"

	"gorm.io/gorm"
)

type contextual interface{ WithContext(context.Context) any }

func Service[T any](ctx context.Context, service T) T {
	if provider, ok := any(service).(contextual); ok {
		if scoped, ok := provider.WithContext(ctx).(T); ok {
			return scoped
		}
		panic("requestscope: incompatible service returned by WithContext")
	}
	return service
}

func Context(db *gorm.DB) context.Context {
	if db != nil && db.Statement != nil && db.Statement.Context != nil {
		return db.Statement.Context
	}
	return context.Background()
}
