// Package config lê a configuração do rate limiter de variáveis de ambiente,
// com um arquivo .env opcional na raiz do projeto.
package config

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"github.com/itallominatti/goexpert-rate-limiter/internal/limiter"
	"github.com/itallominatti/goexpert-rate-limiter/internal/middleware"
)

// Estratégias de persistência disponíveis.
const (
	StoreRedis  = "redis"
	StoreMemory = "memory"
)

// Config é a configuração completa da aplicação.
type Config struct {
	Port           string
	Limiter        limiter.Config
	Store          string
	TrustedProxies []netip.Prefix
	FailOpen       bool
	Redis          RedisConfig
}

// RedisConfig descreve a conexão com o Redis.
type RedisConfig struct {
	Addr     string
	Password string
	DB       int
}

// Load lê o arquivo .env (se existir) sem sobrescrever variáveis já
// definidas no ambiente e então monta a configuração.
func Load(envFile string) (Config, error) {
	if err := godotenv.Load(envFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("config: reading %s: %w", envFile, err)
	}
	return FromLookup(os.LookupEnv)
}

// FromLookup monta a configuração a partir de uma função de busca, o que
// permite testar sem mexer no ambiente do processo.
func FromLookup(lookup func(string) (string, bool)) (Config, error) {
	get := func(key, fallback string) string {
		if v, ok := lookup(key); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		return fallback
	}

	var errs []error
	fail := func(err error) { errs = append(errs, err) }

	ipLimit, err := strconv.ParseInt(get("RATE_LIMIT_IP", "10"), 10, 64)
	if err != nil || ipLimit <= 0 {
		fail(fmt.Errorf("RATE_LIMIT_IP must be a positive integer, got %q", get("RATE_LIMIT_IP", "")))
	}
	window, err := parseDuration(get("RATE_LIMIT_WINDOW", "1s"))
	if err != nil || window < time.Millisecond {
		fail(fmt.Errorf("RATE_LIMIT_WINDOW must be a duration of at least 1ms, got %q", get("RATE_LIMIT_WINDOW", "")))
	}
	block, err := parseDuration(get("RATE_LIMIT_BLOCK_DURATION", "5m"))
	if err != nil || block < 0 {
		fail(fmt.Errorf("RATE_LIMIT_BLOCK_DURATION must be a non-negative duration, got %q", get("RATE_LIMIT_BLOCK_DURATION", "")))
	}

	defaults := limiter.Rule{Limit: ipLimit, Window: window, BlockDuration: block}
	tokens, err := ParseTokens(get("RATE_LIMIT_TOKENS", ""), defaults)
	if err != nil {
		fail(err)
	}

	store := strings.ToLower(get("RATE_LIMIT_STORE", StoreRedis))
	if store != StoreRedis && store != StoreMemory {
		fail(fmt.Errorf("RATE_LIMIT_STORE must be %q or %q, got %q", StoreRedis, StoreMemory, store))
	}

	trusted, err := middleware.ParseTrustedProxies(get("RATE_LIMIT_TRUSTED_PROXIES", ""))
	if err != nil {
		fail(fmt.Errorf("RATE_LIMIT_TRUSTED_PROXIES: %w", err))
	}

	failOpen, err := strconv.ParseBool(get("RATE_LIMIT_FAIL_OPEN", "false"))
	if err != nil {
		fail(fmt.Errorf("RATE_LIMIT_FAIL_OPEN must be a boolean, got %q", get("RATE_LIMIT_FAIL_OPEN", "")))
	}

	db, err := strconv.Atoi(get("REDIS_DB", "0"))
	if err != nil || db < 0 {
		fail(fmt.Errorf("REDIS_DB must be a non-negative integer, got %q", get("REDIS_DB", "")))
	}

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}

	pw, _ := lookup("REDIS_PASSWORD")
	return Config{
		Port: get("PORT", "8080"),
		Limiter: limiter.Config{
			IP:     defaults,
			Tokens: tokens,
		},
		Store:          store,
		TrustedProxies: trusted,
		FailOpen:       failOpen,
		Redis: RedisConfig{
			Addr:     get("REDIS_ADDR", "localhost:6379"),
			Password: pw,
			DB:       db,
		},
	}, nil
}

// ParseTokens interpreta RATE_LIMIT_TOKENS.
//
// Formato: entradas separadas por vírgula, cada uma "token:limite" ou
// "token:limite:bloqueio". Exemplo: "abc123:100,def456:50:1m". A janela é a
// mesma do IP (RATE_LIMIT_WINDOW) e, sem o terceiro campo, o bloqueio também
// (RATE_LIMIT_BLOCK_DURATION).
func ParseTokens(spec string, defaults limiter.Rule) (map[string]limiter.Rule, error) {
	tokens := make(map[string]limiter.Rule)
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.Split(entry, ":")
		if len(parts) != 2 && len(parts) != 3 {
			return nil, fmt.Errorf("RATE_LIMIT_TOKENS: entry #%d must be token:limit or token:limit:block", len(tokens)+1)
		}
		token := strings.TrimSpace(parts[0])
		if token == "" {
			return nil, fmt.Errorf("RATE_LIMIT_TOKENS: entry #%d has an empty token", len(tokens)+1)
		}
		ref := limiter.Fingerprint(token) // nunca expõe o token em mensagens de erro
		if _, dup := tokens[token]; dup {
			return nil, fmt.Errorf("RATE_LIMIT_TOKENS: token %s is configured twice", ref)
		}

		limit, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil || limit <= 0 {
			return nil, fmt.Errorf("RATE_LIMIT_TOKENS: token %s must have a positive integer limit", ref)
		}
		rule := limiter.Rule{Limit: limit, Window: defaults.Window, BlockDuration: defaults.BlockDuration}
		if len(parts) == 3 {
			block, err := parseDuration(strings.TrimSpace(parts[2]))
			if err != nil || block < 0 {
				return nil, fmt.Errorf("RATE_LIMIT_TOKENS: token %s has an invalid block duration", ref)
			}
			rule.BlockDuration = block
		}
		tokens[token] = rule
	}
	return tokens, nil
}

// parseDuration aceita durações do Go ("300ms", "5m", "1h30m") e inteiros,
// interpretados como segundos ("300" = 5 minutos).
func parseDuration(s string) (time.Duration, error) {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	return time.ParseDuration(s)
}
