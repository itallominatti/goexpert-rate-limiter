// Package limiter contém a regra de negócio do rate limiter: decide qual
// identidade (token ou IP) e qual regra se aplicam a uma requisição e delega
// a contagem a uma estratégia de persistência (Store).
//
// O pacote não sabe nada de HTTP nem de Redis: o middleware traduz a
// requisição em um Request e cada Store implementa a contagem atômica.
package limiter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// Rule descreve quantas requisições são aceitas por janela e por quanto tempo
// a identidade fica bloqueada depois de exceder o limite.
type Rule struct {
	// Limit é o máximo de requisições aceitas dentro de Window.
	Limit int64
	// Window é o tamanho da janela fixa de contagem (1s = requisições por segundo).
	Window time.Duration
	// BlockDuration é o tempo em que a identidade fica bloqueada depois de
	// exceder o limite. Zero significa "sem bloqueio extra": a identidade só
	// espera a janela corrente terminar.
	BlockDuration time.Duration
}

// Validate garante que a regra é utilizável por qualquer Store.
func (r Rule) Validate() error {
	switch {
	case r.Limit <= 0:
		return fmt.Errorf("limit must be greater than zero, got %d", r.Limit)
	case r.Window < time.Millisecond:
		return fmt.Errorf("window must be at least 1ms, got %s", r.Window)
	case r.BlockDuration < 0:
		return fmt.Errorf("block duration must not be negative, got %s", r.BlockDuration)
	}
	return nil
}

// Result é a resposta de um Store para uma tentativa de consumo.
type Result struct {
	// Allowed indica se a requisição cabe no limite.
	Allowed bool
	// Count é quantas requisições já foram contadas na janela corrente
	// (zero quando a identidade já estava bloqueada).
	Count int64
	// RetryAfter é quanto falta para a identidade voltar a ser aceita quando
	// Allowed é false, ou quanto falta para a janela corrente terminar quando
	// Allowed é true.
	RetryAfter time.Duration
}

// Store é a estratégia de persistência do rate limiter (padrão Strategy).
//
// Take conta uma requisição para key segundo rule e deve ser atômico, mesmo
// com várias instâncias da aplicação compartilhando o mesmo armazenamento:
//
//  1. se key está bloqueada, nega sem contar;
//  2. senão incrementa o contador da janela fixa (que expira em rule.Window);
//  3. se o contador passou de rule.Limit, bloqueia key por rule.BlockDuration
//     e nega.
//
// Para trocar o Redis por outro mecanismo basta implementar esta interface;
// o pacote storetest traz a suíte de conformidade que toda implementação
// deve passar.
type Store interface {
	Take(ctx context.Context, key string, rule Rule) (Result, error)
}

// Kind identifica qual identidade foi usada na decisão.
type Kind string

const (
	KindIP    Kind = "ip"
	KindToken Kind = "token"
)

// Request são os dados da requisição relevantes para o limiter.
type Request struct {
	IP    string
	Token string
}

// Decision é o resultado de Allow.
type Decision struct {
	Allowed    bool
	Kind       Kind
	Limit      int64
	Remaining  int64
	RetryAfter time.Duration
}

// ErrNoIdentity indica uma requisição sem IP e sem token válido.
var ErrNoIdentity = errors.New("limiter: request has no identity to limit")

// Config agrupa as regras do limiter.
type Config struct {
	// IP é a regra aplicada por endereço IP.
	IP Rule
	// Tokens mapeia cada token (header API_KEY) para a sua regra, que tem
	// precedência sobre a regra de IP.
	Tokens map[string]Rule
}

// Limiter aplica as regras de Config usando um Store.
type Limiter struct {
	store  Store
	ip     Rule
	tokens map[string]Rule
}

// New valida a configuração e cria um Limiter.
func New(store Store, cfg Config) (*Limiter, error) {
	if store == nil {
		return nil, errors.New("limiter: store is required")
	}
	if err := cfg.IP.Validate(); err != nil {
		return nil, fmt.Errorf("limiter: invalid IP rule: %w", err)
	}
	tokens := make(map[string]Rule, len(cfg.Tokens))
	for token, rule := range cfg.Tokens {
		if token == "" {
			return nil, errors.New("limiter: empty token in configuration")
		}
		if err := rule.Validate(); err != nil {
			return nil, fmt.Errorf("limiter: invalid rule for token %s: %w", Fingerprint(token), err)
		}
		tokens[token] = rule
	}
	return &Limiter{store: store, ip: cfg.IP, tokens: tokens}, nil
}

// Allow decide se a requisição pode seguir.
//
// Precedência: se a requisição traz um token configurado, vale somente a regra
// do token, com contador próprio, e o IP não é contado. Um token desconhecido
// é ignorado e a requisição cai na regra de IP, para que inventar tokens não
// seja uma forma de escapar do limite.
func (l *Limiter) Allow(ctx context.Context, req Request) (Decision, error) {
	kind, key, rule, ok := l.resolve(req)
	if !ok {
		return Decision{}, ErrNoIdentity
	}

	res, err := l.store.Take(ctx, key, rule)
	if err != nil {
		return Decision{}, fmt.Errorf("limiter: store take %s: %w", kind, err)
	}

	remaining := rule.Limit - res.Count
	if !res.Allowed || remaining < 0 {
		remaining = 0
	}
	return Decision{
		Allowed:    res.Allowed,
		Kind:       kind,
		Limit:      rule.Limit,
		Remaining:  remaining,
		RetryAfter: res.RetryAfter,
	}, nil
}

func (l *Limiter) resolve(req Request) (Kind, string, Rule, bool) {
	if req.Token != "" {
		if rule, ok := l.tokens[req.Token]; ok {
			return KindToken, string(KindToken) + ":" + Fingerprint(req.Token), rule, true
		}
	}
	if req.IP != "" {
		return KindIP, string(KindIP) + ":" + req.IP, l.ip, true
	}
	return "", "", Rule{}, false
}

// Fingerprint devolve um identificador curto e não reversível do token, usado
// nas chaves do armazenamento e em mensagens de erro para que o token em si
// nunca apareça no Redis nem em logs.
func Fingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}
