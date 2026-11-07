// Package storetest é a suíte de conformidade de limiter.Store.
//
// Qualquer nova estratégia de persistência deve passar por Run; é ela que
// garante que trocar o Redis por outro mecanismo não muda o comportamento do
// rate limiter. Os tempos são reais (curtos), para que a mesma suíte valha
// para armazenamentos externos que não aceitam relógio falso.
package storetest

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/itallominatti/goexpert-rate-limiter/internal/limiter"
)

// Factory devolve um Store pronto para uso e isolado dos demais subtestes
// (por exemplo, com um prefixo de chave exclusivo).
type Factory func(t *testing.T) limiter.Store

// Run executa a suíte completa.
func Run(t *testing.T, newStore Factory) {
	t.Run("AllowsUpToLimitThenDenies", func(t *testing.T) {
		s := newStore(t)
		rule := limiter.Rule{Limit: 5, Window: time.Second, BlockDuration: time.Minute}

		for i := int64(1); i <= rule.Limit; i++ {
			res := take(t, s, "k", rule)
			if !res.Allowed {
				t.Fatalf("request %d should be allowed", i)
			}
			if res.Count != i {
				t.Fatalf("request %d: count = %d", i, res.Count)
			}
			if res.RetryAfter <= 0 || res.RetryAfter > rule.Window {
				t.Fatalf("request %d: retry after %s outside (0, %s]", i, res.RetryAfter, rule.Window)
			}
		}

		res := take(t, s, "k", rule)
		if res.Allowed {
			t.Fatal("request over the limit should be denied")
		}
		assertRetryNear(t, res.RetryAfter, rule.BlockDuration)
	})

	t.Run("KeysAreIndependent", func(t *testing.T) {
		s := newStore(t)
		rule := limiter.Rule{Limit: 1, Window: time.Second, BlockDuration: time.Minute}

		if !take(t, s, "a", rule).Allowed {
			t.Fatal("first request for a should be allowed")
		}
		if take(t, s, "a", rule).Allowed {
			t.Fatal("second request for a should be denied")
		}
		if !take(t, s, "b", rule).Allowed {
			t.Fatal("b must not be affected by a")
		}
	})

	t.Run("WindowResets", func(t *testing.T) {
		s := newStore(t)
		// Sem bloqueio extra, para que esperar a janela acabar seja seguro.
		rule := limiter.Rule{Limit: 2, Window: 150 * time.Millisecond}

		take(t, s, "k", rule)
		res := take(t, s, "k", rule)
		time.Sleep(res.RetryAfter)

		res = takeAfterRetry(t, s, "k", rule)
		if res.Count != 1 {
			t.Fatalf("new window should start from 1, got count=%d", res.Count)
		}
	})

	t.Run("BlockOutlivesWindowAndExpires", func(t *testing.T) {
		s := newStore(t)
		rule := limiter.Rule{Limit: 1, Window: 100 * time.Millisecond, BlockDuration: time.Second}

		take(t, s, "k", rule)
		if take(t, s, "k", rule).Allowed {
			t.Fatal("second request should exceed the limit")
		}

		// A janela já acabou, mas o bloqueio continua valendo.
		time.Sleep(250 * time.Millisecond)
		res := take(t, s, "k", rule)
		if res.Allowed {
			t.Fatal("key should still be blocked after the window ended")
		}
		if res.Count != 0 {
			t.Fatalf("blocked requests must not be counted, count = %d", res.Count)
		}
		if res.RetryAfter <= 0 || res.RetryAfter > rule.BlockDuration {
			t.Fatalf("retry after %s outside (0, %s]", res.RetryAfter, rule.BlockDuration)
		}

		time.Sleep(res.RetryAfter)
		res = takeAfterRetry(t, s, "k", rule)
		if res.Count != 1 {
			t.Fatalf("after the block a fresh window should start, got count=%d", res.Count)
		}
	})

	t.Run("ZeroBlockDeniesUntilWindowEnds", func(t *testing.T) {
		s := newStore(t)
		rule := limiter.Rule{Limit: 1, Window: 200 * time.Millisecond}

		take(t, s, "k", rule)
		var res limiter.Result
		for i := 0; i < 3; i++ {
			res = take(t, s, "k", rule)
			if res.Allowed {
				t.Fatal("requests over the limit should be denied until the window ends")
			}
			if res.RetryAfter <= 0 || res.RetryAfter > rule.Window {
				t.Fatalf("retry after %s outside (0, %s]", res.RetryAfter, rule.Window)
			}
		}

		time.Sleep(res.RetryAfter)
		takeAfterRetry(t, s, "k", rule)
	})

	t.Run("ConcurrentTakesNeverExceedLimit", func(t *testing.T) {
		s := newStore(t)
		const limit, workers, perWorker = 50, 20, 15
		rule := limiter.Rule{Limit: limit, Window: 10 * time.Second, BlockDuration: time.Minute}

		var allowed atomic.Int64
		var wg sync.WaitGroup
		errs := make(chan error, workers*perWorker)
		start := make(chan struct{})
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for i := 0; i < perWorker; i++ {
					res, err := s.Take(context.Background(), "k", rule)
					if err != nil {
						errs <- err
						return
					}
					if res.Allowed {
						allowed.Add(1)
					}
				}
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("take: %v", err)
		}

		if got := allowed.Load(); got != limit {
			t.Fatalf("%d of %d concurrent requests were allowed, want exactly %d", got, workers*perWorker, limit)
		}
	})

	t.Run("HonoursCanceledContext", func(t *testing.T) {
		s := newStore(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := s.Take(ctx, "k", limiter.Rule{Limit: 1, Window: time.Second})
		if err == nil {
			t.Fatal("take with a canceled context should fail")
		}
	})
}

func take(t *testing.T, s limiter.Store, key string, rule limiter.Rule) limiter.Result {
	t.Helper()
	res, err := s.Take(context.Background(), key, rule)
	if err != nil {
		t.Fatalf("take %s: %v", key, err)
	}
	return res
}

// takeAfterRetry espera, guiada pelo RetryAfter devolvido pelo Store, até a
// chave voltar a ser aceita. Em vez de um sleep fixo, confia no próprio
// armazenamento: o Redis expira chaves pelo relógio de parede, que pode dar
// saltos (por exemplo na VM do Docker Desktop/WSL2) e alongar um TTL.
// Só deve ser usada quando as tentativas negadas não renovam o bloqueio.
func takeAfterRetry(t *testing.T, s limiter.Store, key string, rule limiter.Rule) limiter.Result {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		res := take(t, s, key, rule)
		if res.Allowed {
			return res
		}
		if time.Now().After(deadline) {
			t.Fatalf("key %s still denied after waiting (last retry after %s)", key, res.RetryAfter)
		}
		time.Sleep(res.RetryAfter + 10*time.Millisecond)
	}
}

func assertRetryNear(t *testing.T, got, want time.Duration) {
	t.Helper()
	if diff := want - got; diff < 0 || diff > 50*time.Millisecond {
		t.Fatalf("retry after = %s, want about %s", got, want)
	}
}

// UniquePrefix gera um prefixo de chave exclusivo para o teste corrente.
func UniquePrefix(t *testing.T) string {
	return fmt.Sprintf("test:%s:%d", t.Name(), time.Now().UnixNano())
}
