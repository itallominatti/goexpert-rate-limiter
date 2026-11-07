// Package redisstore implementa limiter.Store sobre o Redis.
//
// Toda a decisão (checar bloqueio, incrementar a janela e bloquear) roda num
// único script Lua, executado de forma atômica pelo Redis. Assim o limite vale
// mesmo com várias réplicas da aplicação atrás de um balanceador.
package redisstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/itallominatti/goexpert-rate-limiter/internal/limiter"
)

// DefaultPrefix é o prefixo das chaves criadas no Redis.
const DefaultPrefix = "ratelimit"

// takeScript recebe:
//
//	KEYS[1] contador da janela   KEYS[2] chave de bloqueio
//	ARGV[1] limite   ARGV[2] janela (ms)   ARGV[3] bloqueio (ms)
//
// e devolve {permitido (0/1), contagem, retry_after (ms)}.
var takeScript = redis.NewScript(`
local blocked = redis.call('PTTL', KEYS[2])
if blocked > 0 then
  return {0, 0, blocked}
end

local limit = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local block = tonumber(ARGV[3])

local count = redis.call('INCR', KEYS[1])
local ttl = redis.call('PTTL', KEYS[1])
if ttl < 0 then
  redis.call('PEXPIRE', KEYS[1], window)
  ttl = window
end

if count > limit then
  if block > 0 then
    redis.call('SET', KEYS[2], '1', 'PX', block)
    redis.call('DEL', KEYS[1])
    return {0, count, block}
  end
  return {0, count, ttl}
end

return {1, count, ttl}
`)

// Store é a estratégia de persistência baseada em Redis.
type Store struct {
	client redis.Scripter
	prefix string
}

// Option configura o Store.
type Option func(*Store)

// WithPrefix troca o prefixo das chaves (útil para isolar testes).
func WithPrefix(prefix string) Option {
	return func(s *Store) { s.prefix = prefix }
}

// New cria o Store. client pode ser *redis.Client, *redis.ClusterClient ou
// *redis.Ring.
func New(client redis.Scripter, opts ...Option) *Store {
	s := &Store{client: client, prefix: DefaultPrefix}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

var _ limiter.Store = (*Store)(nil)

// Take implementa limiter.Store.
func (s *Store) Take(ctx context.Context, key string, rule limiter.Rule) (limiter.Result, error) {
	// A hash tag {key} põe contador e bloqueio no mesmo slot, o que permite
	// usar o mesmo script num Redis Cluster.
	base := fmt.Sprintf("%s:{%s}", s.prefix, key)
	keys := []string{base + ":count", base + ":block"}

	raw, err := takeScript.Run(ctx, s.client, keys,
		rule.Limit, rule.Window.Milliseconds(), rule.BlockDuration.Milliseconds(),
	).Int64Slice()
	if err != nil {
		return limiter.Result{}, fmt.Errorf("redis take: %w", err)
	}
	if len(raw) != 3 {
		return limiter.Result{}, errors.New("redis take: unexpected script reply")
	}

	return limiter.Result{
		Allowed:    raw[0] == 1,
		Count:      raw[1],
		RetryAfter: time.Duration(raw[2]) * time.Millisecond,
	}, nil
}
