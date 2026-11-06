package jobs

import (
	"context"
	"testing"

	"gorm.io/gorm"
)

type scopedJobProvider struct {
	ctx      context.Context
	observed chan context.Context
}

func (p *scopedJobProvider) WithContext(ctx context.Context) any {
	copy := *p
	copy.ctx = ctx
	return &copy
}
func (p *scopedJobProvider) RunDailyHonours() error         { p.observed <- p.ctx; return nil }
func (p *scopedJobProvider) RunScheduledMaintenance() error { p.observed <- p.ctx; return nil }
func TestJobRegistryPropagatesContextWithoutMutatingProviders(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	provider := &scopedJobProvider{observed: make(chan context.Context, 2)}
	registry := Registry(&gorm.DB{}, nil, provider, provider, nil)
	if len(registry) != 6 {
		t.Fatal("lost a business job")
	}
	for _, name := range []string{"daily-honour", "support-maintenance"} {
		if err := registry[name](ctx); err != nil {
			t.Fatal(err)
		}
		if <-provider.observed != ctx {
			t.Fatal("worker provider lost parent context")
		}
	}
	if provider.ctx != nil {
		t.Fatal("shared provider mutated")
	}
	empty := Registry(&gorm.DB{}, nil, nil, nil, nil)
	if empty["daily-honour"](ctx) != nil || empty["support-maintenance"](ctx) != nil {
		t.Fatal("disabled optional jobs changed behavior")
	}
}
