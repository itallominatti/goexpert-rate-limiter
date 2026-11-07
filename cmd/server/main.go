package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/itallominatti/goexpert-rate-limiter/internal/config"
	"github.com/itallominatti/goexpert-rate-limiter/internal/limiter"
	"github.com/itallominatti/goexpert-rate-limiter/internal/limiter/memory"
	"github.com/itallominatti/goexpert-rate-limiter/internal/limiter/redisstore"
	"github.com/itallominatti/goexpert-rate-limiter/internal/middleware"
	"github.com/itallominatti/goexpert-rate-limiter/internal/server"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("server stopped with error", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load(".env")
	if err != nil {
		return err
	}

	store, closeStore, err := newStore(cfg)
	if err != nil {
		return err
	}
	defer closeStore()

	lim, err := limiter.New(store, cfg.Limiter)
	if err != nil {
		return err
	}

	rateLimit := middleware.RateLimit(lim, middleware.Options{
		IPResolver: middleware.NewIPResolver(cfg.TrustedProxies),
		FailOpen:   cfg.FailOpen,
		Logger:     logger,
	})

	srv := &http.Server{
		Addr:              net.JoinHostPort("", cfg.Port),
		Handler:           server.NewHandler(rateLimit),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("server listening",
			"port", cfg.Port,
			"store", cfg.Store,
			"ip_limit", cfg.Limiter.IP.Limit,
			"window", cfg.Limiter.IP.Window.String(),
			"block_duration", cfg.Limiter.IP.BlockDuration.String(),
			"tokens", len(cfg.Limiter.Tokens),
		)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	logger.Info("shutting down")
	return srv.Shutdown(shutdownCtx)
}

// newStore escolhe a estratégia de persistência. Para adicionar outra (ex.:
// Memcached, Postgres), implemente limiter.Store e acrescente um case aqui.
func newStore(cfg config.Config) (limiter.Store, func(), error) {
	switch cfg.Store {
	case config.StoreMemory:
		return memory.New(), func() {}, nil
	case config.StoreRedis:
		client := redis.NewClient(&redis.Options{
			Addr:     cfg.Redis.Addr,
			Password: cfg.Redis.Password,
			DB:       cfg.Redis.DB,
		})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.Ping(ctx).Err(); err != nil {
			client.Close()
			return nil, nil, fmt.Errorf("connecting to redis at %s: %w", cfg.Redis.Addr, err)
		}
		return redisstore.New(client), func() { client.Close() }, nil
	default:
		return nil, nil, fmt.Errorf("unknown store %q", cfg.Store)
	}
}
