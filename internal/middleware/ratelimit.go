// Package middleware adapta o limiter ao net/http.
package middleware

import (
	"context"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/itallominatti/goexpert-rate-limiter/internal/limiter"
)

const (
	// TokenHeader é o header que carrega o token de acesso.
	TokenHeader = "API_KEY"
	// LimitExceededMessage é o corpo exato da resposta 429.
	LimitExceededMessage = "you have reached the maximum number of requests or actions allowed within a certain time frame"
	// UnavailableMessage é o corpo da resposta 503 quando o armazenamento falha.
	UnavailableMessage = "rate limiter unavailable"
)

// Allower é o que o middleware precisa do limiter.
type Allower interface {
	Allow(ctx context.Context, req limiter.Request) (limiter.Decision, error)
}

// Options configura o middleware.
type Options struct {
	// IPResolver extrai o IP do cliente. Padrão: só RemoteAddr.
	IPResolver *IPResolver
	// FailOpen deixa a requisição passar quando o armazenamento falha. Com
	// false (padrão) a requisição recebe 503.
	FailOpen bool
	// Logger recebe os eventos de bloqueio e de falha.
	Logger *slog.Logger
}

// RateLimit devolve um middleware que aplica o limiter a cada requisição.
func RateLimit(l Allower, opts Options) func(http.Handler) http.Handler {
	resolver := opts.IPResolver
	if resolver == nil {
		resolver = NewIPResolver(nil)
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := resolver.ClientIP(r)
			req := limiter.Request{IP: ip, Token: strings.TrimSpace(r.Header.Get(TokenHeader))}

			d, err := l.Allow(r.Context(), req)
			if err != nil {
				if opts.FailOpen {
					logger.Warn("rate limiter failed, letting request through", "ip", ip, "error", err)
					next.ServeHTTP(w, r)
					return
				}
				logger.Error("rate limiter failed, rejecting request", "ip", ip, "error", err)
				writeText(w, http.StatusServiceUnavailable, UnavailableMessage)
				return
			}

			h := w.Header()
			h.Set("X-RateLimit-Limit", strconv.FormatInt(d.Limit, 10))
			h.Set("X-RateLimit-Remaining", strconv.FormatInt(d.Remaining, 10))

			if !d.Allowed {
				h.Set("Retry-After", strconv.FormatInt(retryAfterSeconds(d.RetryAfter), 10))
				logger.Info("request rate limited", "kind", d.Kind, "ip", ip, "retry_after", d.RetryAfter.String())
				writeText(w, http.StatusTooManyRequests, LimitExceededMessage)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// retryAfterSeconds arredonda para cima: o header aceita segundos inteiros e
// "0" convidaria o cliente a tentar de novo cedo demais.
func retryAfterSeconds(d time.Duration) int64 {
	s := int64(math.Ceil(d.Seconds()))
	if s < 1 {
		return 1
	}
	return s
}
