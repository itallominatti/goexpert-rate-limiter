// Package memory implementa limiter.Store em memória do processo.
//
// Serve para testes e para rodar uma instância única sem Redis. Como o estado
// vive no processo, não é compartilhado entre réplicas da aplicação.
package memory

import (
	"context"
	"sync"
	"time"

	"github.com/itallominatti/goexpert-rate-limiter/internal/limiter"
)

const defaultSweepInterval = time.Minute

type entry struct {
	count        int64
	windowEnd    time.Time
	blockedUntil time.Time
}

// Store guarda contadores e bloqueios num mapa protegido por mutex.
type Store struct {
	mu            sync.Mutex
	entries       map[string]*entry
	now           func() time.Time
	sweepInterval time.Duration
	nextSweep     time.Time
}

// Option configura o Store.
type Option func(*Store)

// WithClock troca o relógio, para testes determinísticos.
func WithClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}

// WithSweepInterval define de quanto em quanto tempo as entradas expiradas
// são removidas do mapa (a limpeza acontece durante Take, sem goroutine).
func WithSweepInterval(d time.Duration) Option {
	return func(s *Store) { s.sweepInterval = d }
}

// New cria um Store vazio.
func New(opts ...Option) *Store {
	s := &Store{
		entries:       make(map[string]*entry),
		now:           time.Now,
		sweepInterval: defaultSweepInterval,
	}
	for _, opt := range opts {
		opt(s)
	}
	s.nextSweep = s.now().Add(s.sweepInterval)
	return s
}

var _ limiter.Store = (*Store)(nil)

// Take implementa limiter.Store com o mesmo algoritmo do script Lua do Redis.
func (s *Store) Take(ctx context.Context, key string, rule limiter.Rule) (limiter.Result, error) {
	if err := ctx.Err(); err != nil {
		return limiter.Result{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	if !now.Before(s.nextSweep) {
		s.sweep(now)
		s.nextSweep = now.Add(s.sweepInterval)
	}

	e, ok := s.entries[key]
	if !ok {
		e = &entry{}
		s.entries[key] = e
	}

	if now.Before(e.blockedUntil) {
		return limiter.Result{Allowed: false, RetryAfter: e.blockedUntil.Sub(now)}, nil
	}

	if !now.Before(e.windowEnd) {
		e.count = 0
		e.windowEnd = now.Add(rule.Window)
	}
	e.count++

	if e.count > rule.Limit {
		count := e.count
		if rule.BlockDuration <= 0 {
			// Sem bloqueio extra: o contador continua acima do limite e
			// tudo é negado até a janela corrente terminar.
			return limiter.Result{Allowed: false, Count: count, RetryAfter: e.windowEnd.Sub(now)}, nil
		}
		// Bloqueia e zera o contador: ao fim do bloqueio começa uma janela nova.
		e.blockedUntil = now.Add(rule.BlockDuration)
		e.count = 0
		e.windowEnd = time.Time{}
		return limiter.Result{Allowed: false, Count: count, RetryAfter: rule.BlockDuration}, nil
	}

	return limiter.Result{Allowed: true, Count: e.count, RetryAfter: e.windowEnd.Sub(now)}, nil
}

// Len devolve quantas chaves estão no mapa (para testes da limpeza).
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

func (s *Store) sweep(now time.Time) {
	for key, e := range s.entries {
		if !now.Before(e.windowEnd) && !now.Before(e.blockedUntil) {
			delete(s.entries, key)
		}
	}
}
