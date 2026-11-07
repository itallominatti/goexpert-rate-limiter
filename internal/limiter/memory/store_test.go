package memory_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/itallominatti/goexpert-rate-limiter/internal/limiter"
	"github.com/itallominatti/goexpert-rate-limiter/internal/limiter/memory"
	"github.com/itallominatti/goexpert-rate-limiter/internal/limiter/storetest"
)

func TestStoreConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) limiter.Store { return memory.New() })
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestSweepRemovesExpiredEntries(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	s := memory.New(memory.WithClock(clock.Now), memory.WithSweepInterval(time.Minute))
	ctx := context.Background()

	window := limiter.Rule{Limit: 10, Window: time.Second}
	blocking := limiter.Rule{Limit: 1, Window: time.Second, BlockDuration: 5 * time.Minute}

	for _, key := range []string{"a", "b", "c"} {
		if _, err := s.Take(ctx, key, window); err != nil {
			t.Fatal(err)
		}
	}
	s.Take(ctx, "blocked", blocking)
	s.Take(ctx, "blocked", blocking)
	if got := s.Len(); got != 4 {
		t.Fatalf("len = %d, want 4", got)
	}

	// Depois do intervalo de limpeza, as janelas expiraram mas o bloqueio de
	// 5 minutos ainda vale: só "blocked" (e a chave nova) sobrevivem.
	clock.Advance(2 * time.Minute)
	s.Take(ctx, "fresh", window)
	if got := s.Len(); got != 2 {
		t.Fatalf("len after sweep = %d, want 2", got)
	}

	res, _ := s.Take(ctx, "blocked", blocking)
	if res.Allowed {
		t.Fatal("sweep must not drop an active block")
	}
}

func TestDeterministicBlockWithFakeClock(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	s := memory.New(memory.WithClock(clock.Now))
	ctx := context.Background()
	rule := limiter.Rule{Limit: 2, Window: time.Second, BlockDuration: 5 * time.Minute}

	s.Take(ctx, "k", rule)
	s.Take(ctx, "k", rule)
	if res, _ := s.Take(ctx, "k", rule); res.Allowed || res.RetryAfter != 5*time.Minute {
		t.Fatalf("third request: allowed=%v retry=%s, want denied with 5m", res.Allowed, res.RetryAfter)
	}

	clock.Advance(4*time.Minute + 59*time.Second)
	if res, _ := s.Take(ctx, "k", rule); res.Allowed || res.RetryAfter != time.Second {
		t.Fatalf("near the end of the block: allowed=%v retry=%s, want denied with 1s", res.Allowed, res.RetryAfter)
	}

	clock.Advance(time.Second)
	if res, _ := s.Take(ctx, "k", rule); !res.Allowed || res.Count != 1 {
		t.Fatalf("after the block: allowed=%v count=%d, want allowed with count 1", res.Allowed, res.Count)
	}
}
