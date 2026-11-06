package requestscope

import (
	"context"
	"testing"

	"gorm.io/gorm"
)

type service struct{ ctx context.Context }

func (s *service) WithContext(ctx context.Context) any { return &service{ctx: ctx} }

type badService struct{}

func (badService) WithContext(context.Context) any { return "wrong type" }
func TestScopeDoesNotMutateOriginalOrRequireMocksToImplementIt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	original := &service{}
	copy := Service(ctx, original)
	if original.ctx != nil || copy == original || copy.ctx != ctx {
		t.Fatal("shared service mutated")
	}
	if Service(ctx, 7) != 7 {
		t.Fatal("non-contextual mock changed")
	}
	if Context(nil) == nil || Context(&gorm.DB{}) == nil {
		t.Fatal("missing fallback context")
	}
	db := &gorm.DB{Statement: &gorm.Statement{Context: ctx}}
	if Context(db) != ctx {
		t.Fatal("request context lost")
	}
}
func TestIncompatibleProviderIsProgrammingError(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("incompatible provider silently accepted")
		}
	}()
	Service(context.Background(), badService{})
}
