package limiter_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/itallominatti/goexpert-rate-limiter/internal/limiter"
	"github.com/itallominatti/goexpert-rate-limiter/internal/limiter/memory"
)

type clock struct{ now time.Time }

func (c *clock) Now() time.Time          { return c.now }
func (c *clock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func newLimiter(t *testing.T, cfg limiter.Config) (*limiter.Limiter, *clock) {
	t.Helper()
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	l, err := limiter.New(memory.New(memory.WithClock(c.Now)), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return l, c
}

func allowN(t *testing.T, l *limiter.Limiter, req limiter.Request, n int) (allowed int) {
	t.Helper()
	for i := 0; i < n; i++ {
		d, err := l.Allow(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if d.Allowed {
			allowed++
		}
	}
	return allowed
}

var (
	ipRule    = limiter.Rule{Limit: 10, Window: time.Second, BlockDuration: 5 * time.Minute}
	tokenRule = limiter.Rule{Limit: 100, Window: time.Second, BlockDuration: 5 * time.Minute}
)

func TestIPLimit(t *testing.T) {
	l, _ := newLimiter(t, limiter.Config{IP: ipRule})
	req := limiter.Request{IP: "192.168.0.1"}

	for i := int64(1); i <= 10; i++ {
		d, err := l.Allow(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if !d.Allowed || d.Kind != limiter.KindIP || d.Limit != 10 || d.Remaining != 10-i {
			t.Fatalf("request %d: %+v", i, d)
		}
	}

	d, _ := l.Allow(context.Background(), req)
	if d.Allowed || d.Remaining != 0 || d.RetryAfter != 5*time.Minute {
		t.Fatalf("11th request should be denied with a 5m block, got %+v", d)
	}
}

func TestBlockPersistsAcrossWindowsAndExpires(t *testing.T) {
	l, c := newLimiter(t, limiter.Config{IP: ipRule})
	req := limiter.Request{IP: "192.168.0.1"}
	allowN(t, l, req, 11)

	// Mesmo bem depois da janela de 1s, tudo é rejeitado durante o bloqueio.
	for _, step := range []time.Duration{time.Second, time.Minute, 3*time.Minute + 58*time.Second} {
		c.Advance(step)
		if got := allowN(t, l, req, 1); got != 0 {
			t.Fatalf("request allowed %s into the block", step)
		}
	}

	c.Advance(time.Second) // 5 minutos completos desde o bloqueio
	if got := allowN(t, l, req, 15); got != 10 {
		t.Fatalf("after the block, %d requests allowed, want 10", got)
	}
}

func TestTokenOverridesIP(t *testing.T) {
	l, _ := newLimiter(t, limiter.Config{IP: ipRule, Tokens: map[string]limiter.Rule{"abc123": tokenRule}})
	req := limiter.Request{IP: "192.168.0.1", Token: "abc123"}

	if got := allowN(t, l, req, 150); got != 100 {
		t.Fatalf("token: %d requests allowed, want 100 (token limit, not IP limit 10)", got)
	}

	d, _ := l.Allow(context.Background(), req)
	if d.Allowed || d.Kind != limiter.KindToken || d.Limit != 100 {
		t.Fatalf("token over the limit: %+v", d)
	}

	// O IP não foi consumido pelas requisições com token.
	if got := allowN(t, l, limiter.Request{IP: "192.168.0.1"}, 10); got != 10 {
		t.Fatalf("IP without token: %d allowed, want 10", got)
	}
}

func TestTokenWithLowerLimitStillWins(t *testing.T) {
	strict := limiter.Rule{Limit: 2, Window: time.Second, BlockDuration: time.Minute}
	l, _ := newLimiter(t, limiter.Config{IP: ipRule, Tokens: map[string]limiter.Rule{"slow": strict}})

	if got := allowN(t, l, limiter.Request{IP: "10.0.0.1", Token: "slow"}, 10); got != 2 {
		t.Fatalf("%d allowed, want 2: token rule applies even when stricter than the IP rule", got)
	}
}

func TestBlockedIPCanStillUseValidToken(t *testing.T) {
	l, _ := newLimiter(t, limiter.Config{IP: ipRule, Tokens: map[string]limiter.Rule{"abc123": tokenRule}})
	allowN(t, l, limiter.Request{IP: "10.0.0.1"}, 11)

	if got := allowN(t, l, limiter.Request{IP: "10.0.0.1", Token: "abc123"}, 1); got != 1 {
		t.Fatal("a valid token is limited by its own rule, independent of the IP block")
	}
	if got := allowN(t, l, limiter.Request{IP: "10.0.0.1"}, 1); got != 0 {
		t.Fatal("the IP itself should remain blocked")
	}
}

func TestUnknownTokenFallsBackToIP(t *testing.T) {
	l, _ := newLimiter(t, limiter.Config{IP: ipRule, Tokens: map[string]limiter.Rule{"abc123": tokenRule}})

	allowed := 0
	for i := 0; i < 20; i++ {
		// Inventar um token novo a cada requisição não escapa do limite por IP.
		allowed += allowN(t, l, limiter.Request{IP: "10.0.0.1", Token: strings.Repeat("x", i+1)}, 1)
	}
	if allowed != 10 {
		t.Fatalf("%d allowed, want 10 (IP limit)", allowed)
	}
}

func TestTokensAndIPsAreIndependent(t *testing.T) {
	l, _ := newLimiter(t, limiter.Config{
		IP: ipRule,
		Tokens: map[string]limiter.Rule{
			"abc123": tokenRule,
			"def456": {Limit: 50, Window: time.Second, BlockDuration: time.Minute},
		},
	})

	if got := allowN(t, l, limiter.Request{IP: "10.0.0.1"}, 20); got != 10 {
		t.Fatalf("ip 10.0.0.1: %d", got)
	}
	if got := allowN(t, l, limiter.Request{IP: "10.0.0.2"}, 20); got != 10 {
		t.Fatalf("ip 10.0.0.2: %d", got)
	}
	if got := allowN(t, l, limiter.Request{IP: "10.0.0.1", Token: "def456"}, 80); got != 50 {
		t.Fatalf("token def456: %d", got)
	}
	// O mesmo token vindo de outro IP compartilha o contador do token.
	if got := allowN(t, l, limiter.Request{IP: "10.0.0.3", Token: "abc123"}, 60); got != 60 {
		t.Fatalf("token abc123 from ip 3: %d", got)
	}
	if got := allowN(t, l, limiter.Request{IP: "10.0.0.4", Token: "abc123"}, 60); got != 40 {
		t.Fatalf("token abc123 from ip 4: %d", got)
	}
}

func TestPerTokenBlockDuration(t *testing.T) {
	short := limiter.Rule{Limit: 1, Window: time.Second, BlockDuration: 10 * time.Second}
	l, c := newLimiter(t, limiter.Config{IP: ipRule, Tokens: map[string]limiter.Rule{"short": short}})
	req := limiter.Request{IP: "10.0.0.1", Token: "short"}

	allowN(t, l, req, 2)
	c.Advance(9 * time.Second)
	if allowN(t, l, req, 1) != 0 {
		t.Fatal("still inside the 10s block")
	}
	c.Advance(time.Second)
	if allowN(t, l, req, 1) != 1 {
		t.Fatal("the 10s block should have expired")
	}
}

func TestNoIdentity(t *testing.T) {
	l, _ := newLimiter(t, limiter.Config{IP: ipRule})
	if _, err := l.Allow(context.Background(), limiter.Request{}); !errors.Is(err, limiter.ErrNoIdentity) {
		t.Fatalf("err = %v, want ErrNoIdentity", err)
	}
}

type failingStore struct{}

func (failingStore) Take(context.Context, string, limiter.Rule) (limiter.Result, error) {
	return limiter.Result{}, errors.New("boom")
}

func TestStoreErrorIsReturned(t *testing.T) {
	l, err := limiter.New(failingStore{}, limiter.Config{IP: ipRule})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Allow(context.Background(), limiter.Request{IP: "1.1.1.1"}); err == nil {
		t.Fatal("store errors must be propagated")
	}
}

type recordingStore struct{ keys []string }

func (s *recordingStore) Take(_ context.Context, key string, _ limiter.Rule) (limiter.Result, error) {
	s.keys = append(s.keys, key)
	return limiter.Result{Allowed: true, Count: 1}, nil
}

func TestTokenNeverReachesTheStoreInPlainText(t *testing.T) {
	s := &recordingStore{}
	l, err := limiter.New(s, limiter.Config{IP: ipRule, Tokens: map[string]limiter.Rule{"super-secret": tokenRule}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Allow(context.Background(), limiter.Request{IP: "1.1.1.1", Token: "super-secret"}); err != nil {
		t.Fatal(err)
	}
	if len(s.keys) != 1 || strings.Contains(s.keys[0], "super-secret") || !strings.HasPrefix(s.keys[0], "token:") {
		t.Fatalf("store key = %q, want a token fingerprint", s.keys)
	}
}

func TestNewValidatesConfig(t *testing.T) {
	cases := map[string]limiter.Config{
		"zero IP limit":    {IP: limiter.Rule{Limit: 0, Window: time.Second}},
		"tiny window":      {IP: limiter.Rule{Limit: 1, Window: time.Microsecond}},
		"negative block":   {IP: limiter.Rule{Limit: 1, Window: time.Second, BlockDuration: -time.Second}},
		"empty token":      {IP: ipRule, Tokens: map[string]limiter.Rule{"": tokenRule}},
		"bad token limit":  {IP: ipRule, Tokens: map[string]limiter.Rule{"abc": {Limit: -1, Window: time.Second}}},
		"bad token window": {IP: ipRule, Tokens: map[string]limiter.Rule{"abc": {Limit: 1}}},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := limiter.New(memory.New(), cfg); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
	if _, err := limiter.New(nil, limiter.Config{IP: ipRule}); err == nil {
		t.Fatal("nil store should be rejected")
	}
}
