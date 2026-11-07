package middleware_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/itallominatti/goexpert-rate-limiter/internal/limiter"
	"github.com/itallominatti/goexpert-rate-limiter/internal/limiter/memory"
	"github.com/itallominatti/goexpert-rate-limiter/internal/middleware"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
}

func newHandler(t *testing.T, opts middleware.Options) http.Handler {
	t.Helper()
	l, err := limiter.New(memory.New(), limiter.Config{
		IP: limiter.Rule{Limit: 10, Window: time.Second, BlockDuration: 5 * time.Minute},
		Tokens: map[string]limiter.Rule{
			"abc123": {Limit: 100, Window: time.Second, BlockDuration: 5 * time.Minute},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return middleware.RateLimit(l, opts)(okHandler())
}

func do(h http.Handler, ip, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = ip + ":12345"
	if token != "" {
		r.Header.Set("API_KEY", token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestReturns429WithExactBodyAfterIPLimit(t *testing.T) {
	h := newHandler(t, middleware.Options{})

	for i := 1; i <= 10; i++ {
		w := do(h, "192.168.1.1", "")
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: status %d", i, w.Code)
		}
		if got, want := w.Header().Get("X-RateLimit-Remaining"), fmt.Sprint(10-i); got != want {
			t.Fatalf("request %d: remaining %s, want %s", i, got, want)
		}
	}

	w := do(h, "192.168.1.1", "")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("11th request: status %d, want 429", w.Code)
	}
	if got := w.Body.String(); got != middleware.LimitExceededMessage {
		t.Fatalf("body = %q", got)
	}
	if got := w.Body.String(); got != "you have reached the maximum number of requests or actions allowed within a certain time frame" {
		t.Fatalf("body differs from the specification: %q", got)
	}
	if got := w.Header().Get("Retry-After"); got != "300" {
		t.Fatalf("Retry-After = %q, want 300", got)
	}
	if got := w.Header().Get("X-RateLimit-Limit"); got != "10" {
		t.Fatalf("X-RateLimit-Limit = %q", got)
	}

	// Outro IP segue livre.
	if w := do(h, "192.168.1.2", ""); w.Code != http.StatusOK {
		t.Fatalf("other IP: status %d", w.Code)
	}
}

func TestTokenHeaderOverridesIPLimit(t *testing.T) {
	h := newHandler(t, middleware.Options{})

	for i := 1; i <= 100; i++ {
		if w := do(h, "192.168.1.1", "abc123"); w.Code != http.StatusOK {
			t.Fatalf("token request %d: status %d (IP limit is 10, token limit is 100)", i, w.Code)
		}
	}
	w := do(h, "192.168.1.1", "abc123")
	if w.Code != http.StatusTooManyRequests || w.Header().Get("X-RateLimit-Limit") != "100" {
		t.Fatalf("101st token request: status %d limit %s", w.Code, w.Header().Get("X-RateLimit-Limit"))
	}
}

func TestHeaderNameIsCaseInsensitive(t *testing.T) {
	h := newHandler(t, middleware.Options{})
	ok := 0
	for i := 0; i < 20; i++ {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("api_key", "abc123")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code == http.StatusOK {
			ok++
		}
	}
	if ok != 20 {
		t.Fatalf("%d of 20 allowed; lower-case api_key header should map to the token", ok)
	}
}

func TestConcurrentRequestsFromSameIP(t *testing.T) {
	h := newHandler(t, middleware.Options{})
	var ok, limited atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch do(h, "192.168.1.1", "").Code {
			case http.StatusOK:
				ok.Add(1)
			case http.StatusTooManyRequests:
				limited.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 10 || limited.Load() != 190 {
		t.Fatalf("ok=%d limited=%d, want 10/190", ok.Load(), limited.Load())
	}
}

type brokenLimiter struct{}

func (brokenLimiter) Allow(context.Context, limiter.Request) (limiter.Decision, error) {
	return limiter.Decision{}, errors.New("redis down")
}

func TestStoreFailureFailClosed(t *testing.T) {
	h := middleware.RateLimit(brokenLimiter{}, middleware.Options{})(okHandler())
	w := do(h, "1.1.1.1", "")
	if w.Code != http.StatusServiceUnavailable || w.Body.String() != middleware.UnavailableMessage {
		t.Fatalf("status %d body %q, want 503", w.Code, w.Body.String())
	}
}

func TestStoreFailureFailOpen(t *testing.T) {
	h := middleware.RateLimit(brokenLimiter{}, middleware.Options{FailOpen: true})(okHandler())
	if w := do(h, "1.1.1.1", ""); w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 with FailOpen", w.Code)
	}
}

func TestUsesTrustedProxyHeaders(t *testing.T) {
	trusted, _ := middleware.ParseTrustedProxies("10.0.0.0/8")
	h := newHandler(t, middleware.Options{IPResolver: middleware.NewIPResolver(trusted)})

	send := func(client string) int {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.0.0.2:8080" // o proxy
		r.Header.Set("X-Forwarded-For", client)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}

	for i := 0; i < 10; i++ {
		send("198.51.100.1")
	}
	if send("198.51.100.1") != http.StatusTooManyRequests {
		t.Fatal("client behind the proxy should be limited by its own IP")
	}
	if send("198.51.100.2") != http.StatusOK {
		t.Fatal("a different client behind the same proxy must not share the limit")
	}
}
