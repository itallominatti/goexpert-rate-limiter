package server_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/itallominatti/goexpert-rate-limiter/internal/limiter"
	"github.com/itallominatti/goexpert-rate-limiter/internal/limiter/memory"
	"github.com/itallominatti/goexpert-rate-limiter/internal/limiter/redisstore"
	"github.com/itallominatti/goexpert-rate-limiter/internal/middleware"
	"github.com/itallominatti/goexpert-rate-limiter/internal/server"
)

const blockDuration = 3 * time.Second

// stores devolve as estratégias disponíveis no ambiente: memória sempre e
// Redis quando REDIS_ADDR está definido (serviço test do docker compose).
// Os mesmos cenários de ponta a ponta rodam contra cada uma.
func stores(t *testing.T) map[string]func(t *testing.T) limiter.Store {
	out := map[string]func(t *testing.T) limiter.Store{
		"memory": func(*testing.T) limiter.Store { return memory.New() },
	}
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Log("REDIS_ADDR not set: end-to-end tests run only against the memory store")
		return out
	}
	out["redis"] = func(t *testing.T) limiter.Store {
		client := redis.NewClient(&redis.Options{Addr: addr, Password: os.Getenv("REDIS_PASSWORD")})
		if err := client.Ping(context.Background()).Err(); err != nil {
			t.Fatalf("redis at %s is unreachable: %v", addr, err)
		}
		prefix := fmt.Sprintf("e2e:%s:%d", t.Name(), time.Now().UnixNano())
		t.Cleanup(func() {
			ctx := context.Background()
			iter := client.Scan(ctx, 0, prefix+"*", 100).Iterator()
			for iter.Next(ctx) {
				client.Del(ctx, iter.Val())
			}
			client.Close()
		})
		return redisstore.New(client, redisstore.WithPrefix(prefix))
	}
	return out
}

func startServer(t *testing.T, store limiter.Store) *httptest.Server {
	t.Helper()
	l, err := limiter.New(store, limiter.Config{
		IP: limiter.Rule{Limit: 10, Window: time.Second, BlockDuration: blockDuration},
		Tokens: map[string]limiter.Rule{
			"abc123": {Limit: 100, Window: time.Second, BlockDuration: blockDuration},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.NewHandler(middleware.RateLimit(l, middleware.Options{})))
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, url, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		// Grava o header exatamente como o enunciado: "API_KEY: <TOKEN>".
		req.Header["API_KEY"] = []string{token}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func countStatuses(t *testing.T, url, token string, n int) map[int]int {
	t.Helper()
	got := map[int]int{}
	for i := 0; i < n; i++ {
		code, _ := get(t, url, token)
		got[code]++
	}
	return got
}

func TestEndToEnd(t *testing.T) {
	for name, newStore := range stores(t) {
		t.Run(name, func(t *testing.T) {
			t.Run("IPLimitBlockAndRecovery", func(t *testing.T) {
				srv := startServer(t, newStore(t))

				if got := countStatuses(t, srv.URL, "", 10); got[http.StatusOK] != 10 {
					t.Fatalf("first 10 requests: %v", got)
				}
				code, body := get(t, srv.URL, "")
				if code != http.StatusTooManyRequests || body != middleware.LimitExceededMessage {
					t.Fatalf("11th request: %d %q", code, body)
				}

				// Passou a janela de 1s: o bloqueio continua.
				time.Sleep(1200 * time.Millisecond)
				if code, _ := get(t, srv.URL, ""); code != http.StatusTooManyRequests {
					t.Fatalf("during the block: %d, want 429", code)
				}

				// Fim do bloqueio: volta a aceitar. Requisições durante o
				// bloqueio não contam, então dá para sondar até liberar; isso
				// tolera saltos do relógio de parede, que o Redis usa nos TTLs.
				time.Sleep(blockDuration - 1200*time.Millisecond)
				deadline := time.Now().Add(10 * time.Second)
				for {
					code, _ := get(t, srv.URL, "")
					if code == http.StatusOK {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("still %d long after the %s block should have ended", code, blockDuration)
					}
					time.Sleep(100 * time.Millisecond)
				}
				if got := countStatuses(t, srv.URL, "", 10); got[http.StatusOK] != 9 || got[http.StatusTooManyRequests] != 1 {
					t.Fatalf("after the block, a fresh window of 10 is expected: %v", got)
				}
			})

			t.Run("TokenOverridesIP", func(t *testing.T) {
				srv := startServer(t, newStore(t))

				got := countStatuses(t, srv.URL, "abc123", 101)
				if got[http.StatusOK] != 100 || got[http.StatusTooManyRequests] != 1 {
					t.Fatalf("token with limit 100 from an IP limited to 10: %v", got)
				}
				// O IP continua com sua cota própria.
				if got := countStatuses(t, srv.URL, "", 10); got[http.StatusOK] != 10 {
					t.Fatalf("IP after token traffic: %v", got)
				}
				// Token desconhecido cai no limite do IP, que acabou de ser consumido.
				if code, _ := get(t, srv.URL, "unknown"); code != http.StatusTooManyRequests {
					t.Fatalf("unknown token: %d, want 429", code)
				}
			})

			t.Run("HealthzIsNotLimited", func(t *testing.T) {
				srv := startServer(t, newStore(t))
				countStatuses(t, srv.URL, "", 11)
				if code, _ := get(t, srv.URL+"/healthz", ""); code != http.StatusOK {
					t.Fatalf("/healthz: %d", code)
				}
			})

			t.Run("ConcurrentClients", func(t *testing.T) {
				srv := startServer(t, newStore(t))
				var mu sync.Mutex
				got := map[int]int{}
				var wg sync.WaitGroup
				for i := 0; i < 60; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						code := -1 // erro de transporte
						if resp, err := http.Get(srv.URL); err == nil {
							resp.Body.Close()
							code = resp.StatusCode
						}
						mu.Lock()
						got[code]++
						mu.Unlock()
					}()
				}
				wg.Wait()
				if got[http.StatusOK] != 10 || got[http.StatusTooManyRequests] != 50 {
					t.Fatalf("60 concurrent requests from one IP: %v, want 10 OK / 50 429", got)
				}
			})
		})
	}
}
