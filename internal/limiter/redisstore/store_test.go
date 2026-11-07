package redisstore_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/itallominatti/goexpert-rate-limiter/internal/limiter"
	"github.com/itallominatti/goexpert-rate-limiter/internal/limiter/redisstore"
	"github.com/itallominatti/goexpert-rate-limiter/internal/limiter/storetest"
)

// newClient conecta no Redis indicado por REDIS_ADDR. Sem a variável os testes
// de integração são pulados; no docker compose (serviço test) ela aponta para
// o Redis real.
func newClient(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("REDIS_ADDR not set; skipping Redis integration tests (run them with: docker compose run --rm test)")
	}
	client := redis.NewClient(&redis.Options{Addr: addr, Password: os.Getenv("REDIS_PASSWORD")})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("redis at %s is unreachable: %v", addr, err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func newStore(t *testing.T, client *redis.Client) limiter.Store {
	prefix := storetest.UniquePrefix(t)
	t.Cleanup(func() {
		ctx := context.Background()
		iter := client.Scan(ctx, 0, prefix+"*", 100).Iterator()
		for iter.Next(ctx) {
			client.Del(ctx, iter.Val())
		}
	})
	return redisstore.New(client, redisstore.WithPrefix(prefix))
}

func TestStoreConformance(t *testing.T) {
	client := newClient(t)
	storetest.Run(t, func(t *testing.T) limiter.Store { return newStore(t, client) })
}

func TestKeysHaveTTLAndHashTag(t *testing.T) {
	client := newClient(t)
	prefix := storetest.UniquePrefix(t)
	s := redisstore.New(client, redisstore.WithPrefix(prefix))
	ctx := context.Background()
	rule := limiter.Rule{Limit: 1, Window: time.Second, BlockDuration: 5 * time.Minute}
	t.Cleanup(func() { client.Del(ctx, prefix+":{ip:10.0.0.1}:count", prefix+":{ip:10.0.0.1}:block") })

	if _, err := s.Take(ctx, "ip:10.0.0.1", rule); err != nil {
		t.Fatal(err)
	}
	ttl, err := client.PTTL(ctx, prefix+":{ip:10.0.0.1}:count").Result()
	if err != nil || ttl <= 0 || ttl > time.Second {
		t.Fatalf("counter TTL = %s (err %v), want within (0, 1s]", ttl, err)
	}

	if _, err := s.Take(ctx, "ip:10.0.0.1", rule); err != nil {
		t.Fatal(err)
	}
	ttl, err = client.PTTL(ctx, prefix+":{ip:10.0.0.1}:block").Result()
	if err != nil || ttl <= 4*time.Minute || ttl > 5*time.Minute {
		t.Fatalf("block TTL = %s (err %v), want about 5m", ttl, err)
	}
	if n, _ := client.Exists(ctx, prefix+":{ip:10.0.0.1}:count").Result(); n != 0 {
		t.Fatal("counter should be removed once the key is blocked")
	}
}

func TestUnreachableRedisReturnsError(t *testing.T) {
	client := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 200 * time.Millisecond,
		MaxRetries:  -1,
	})
	defer client.Close()

	_, err := redisstore.New(client).Take(context.Background(), "ip:1.1.1.1", limiter.Rule{Limit: 1, Window: time.Second})
	if err == nil {
		t.Fatal("take against an unreachable Redis should fail")
	}
}
