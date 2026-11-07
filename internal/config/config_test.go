package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/itallominatti/goexpert-rate-limiter/internal/limiter"
)

func lookupFrom(env map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}
}

func TestDefaults(t *testing.T) {
	cfg, err := FromLookup(lookupFrom(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := limiter.Rule{Limit: 10, Window: time.Second, BlockDuration: 5 * time.Minute}
	if cfg.Limiter.IP != want {
		t.Fatalf("IP rule = %+v, want %+v", cfg.Limiter.IP, want)
	}
	if cfg.Port != "8080" || cfg.Store != StoreRedis || cfg.Redis.Addr != "localhost:6379" || cfg.FailOpen || len(cfg.Limiter.Tokens) != 0 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestFullConfig(t *testing.T) {
	cfg, err := FromLookup(lookupFrom(map[string]string{
		"PORT":                       "9090",
		"RATE_LIMIT_IP":              "5",
		"RATE_LIMIT_WINDOW":          "2s",
		"RATE_LIMIT_BLOCK_DURATION":  "300",
		"RATE_LIMIT_TOKENS":          " abc123:100 , def456:50:1m ,",
		"RATE_LIMIT_STORE":           "Memory",
		"RATE_LIMIT_TRUSTED_PROXIES": "10.0.0.0/8",
		"RATE_LIMIT_FAIL_OPEN":       "true",
		"REDIS_ADDR":                 "redis:6379",
		"REDIS_PASSWORD":             "s3cret",
		"REDIS_DB":                   "2",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != "9090" || cfg.Store != StoreMemory || !cfg.FailOpen || len(cfg.TrustedProxies) != 1 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if cfg.Limiter.IP != (limiter.Rule{Limit: 5, Window: 2 * time.Second, BlockDuration: 5 * time.Minute}) {
		t.Fatalf("IP rule = %+v", cfg.Limiter.IP)
	}
	if got := cfg.Limiter.Tokens["abc123"]; got != (limiter.Rule{Limit: 100, Window: 2 * time.Second, BlockDuration: 5 * time.Minute}) {
		t.Fatalf("abc123 = %+v", got)
	}
	if got := cfg.Limiter.Tokens["def456"]; got != (limiter.Rule{Limit: 50, Window: 2 * time.Second, BlockDuration: time.Minute}) {
		t.Fatalf("def456 = %+v", got)
	}
	if cfg.Redis != (RedisConfig{Addr: "redis:6379", Password: "s3cret", DB: 2}) {
		t.Fatalf("redis = %+v", cfg.Redis)
	}
}

func TestInvalidValuesAreAllReported(t *testing.T) {
	_, err := FromLookup(lookupFrom(map[string]string{
		"RATE_LIMIT_IP":             "0",
		"RATE_LIMIT_WINDOW":         "soon",
		"RATE_LIMIT_BLOCK_DURATION": "-1m",
		"RATE_LIMIT_STORE":          "postgres",
		"RATE_LIMIT_FAIL_OPEN":      "maybe",
		"REDIS_DB":                  "x",
	}))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, key := range []string{"RATE_LIMIT_IP", "RATE_LIMIT_WINDOW", "RATE_LIMIT_BLOCK_DURATION", "RATE_LIMIT_STORE", "RATE_LIMIT_FAIL_OPEN", "REDIS_DB"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error does not mention %s: %v", key, err)
		}
	}
}

func TestParseTokensErrors(t *testing.T) {
	defaults := limiter.Rule{Limit: 10, Window: time.Second, BlockDuration: time.Minute}
	for _, spec := range []string{
		"secret",
		"secret:",
		":10",
		"secret:0",
		"secret:-5",
		"secret:ten",
		"secret:10:forever",
		"secret:10:1m:extra",
		"secret:10,secret:20",
	} {
		t.Run(spec, func(t *testing.T) {
			_, err := ParseTokens(spec, defaults)
			if err == nil {
				t.Fatal("expected an error")
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("error leaks the token: %v", err)
			}
		})
	}
}

func TestLoadReadsDotEnvWithoutOverridingEnvironment(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	content := "RATE_LIMIT_IP=7\nRATE_LIMIT_TOKENS=tok:70\nPORT=7070\n"
	if err := os.WriteFile(envFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PORT", "6060") // o ambiente real tem prioridade sobre o .env
	// t.Setenv registra a restauração; garante que as chaves lidas do .env
	// não vazem para outros testes.
	t.Setenv("RATE_LIMIT_IP", "")
	t.Setenv("RATE_LIMIT_TOKENS", "")
	os.Unsetenv("RATE_LIMIT_IP")
	os.Unsetenv("RATE_LIMIT_TOKENS")

	cfg, err := Load(envFile)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != "6060" || cfg.Limiter.IP.Limit != 7 || cfg.Limiter.Tokens["tok"].Limit != 70 {
		t.Fatalf("unexpected config: port=%s ip=%d tokens=%v", cfg.Port, cfg.Limiter.IP.Limit, cfg.Limiter.Tokens)
	}
}

func TestLoadWithoutDotEnv(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "missing.env")); err != nil {
		t.Fatalf("a missing .env must not be an error: %v", err)
	}
}
