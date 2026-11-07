# Rate Limiter em Go

Rate limiter em Go usado como middleware HTTP. Ele limita as requisições por segundo por **endereço IP** e por **token de acesso** (header `API_KEY`). A contagem fica no **Redis**, atrás de uma interface de persistência (padrão Strategy) que permite trocar o Redis por outro mecanismo sem mexer na regra de negócio.

Quando o limite é excedido, a resposta é imediata:

```
HTTP/1.1 429 Too Many Requests
Retry-After: 300

you have reached the maximum number of requests or actions allowed within a certain time frame
```

e o IP ou token infrator fica **bloqueado** pelo tempo configurado (padrão: 5 minutos). Tudo o que ele mandar nesse período é rejeitado.

## Executando

Pré-requisito: Docker com Docker Compose.

```bash
docker compose up --build
```

Esse comando sobe a aplicação em `http://localhost:8080` e o Redis. A configuração vem do arquivo [`.env`](.env) da raiz (seção [Configuração](#configuração)).

```bash
curl -i http://localhost:8080/
curl -i -H "API_KEY: abc123" http://localhost:8080/
```

### Vendo o limiter funcionar

Com o `.env` padrão (IP a 10 req/s, token `abc123` a 100 req/s, bloqueio de 5 min), o script [`scripts/demo.sh`](scripts/demo.sh) dispara rajadas paralelas com o `curl`:

```bash
./scripts/demo.sh
```

```
1) Rajada de 20 requisições sem token (limite por IP: 10 req/s)
         10 200
         10 429
2) Corpo da resposta 429
    you have reached the maximum number of requests or actions allowed within a certain time frame
3) Rajada de 110 requisições com API_KEY abc123 (limite do token: 100 req/s)
   O IP já está bloqueado, mas o token tem precedência e cota própria.
        100 200
         10 429
4) Token desconhecido não escapa do limite: cai na regra do IP, que está bloqueado
    429 (Retry-After: 300)
5) Três segundos depois (bem além da janela de 1s), o IP continua bloqueado
    429 (Retry-After: 297)
```

As rajadas são paralelas porque `curl` em série, um processo por requisição, demora mais que a janela de 1 segundo e não chega a 100 req/s. O comando equivalente, sem o script:

```bash
curl -s -Z -o /dev/null -H "API_KEY: abc123" -w "%{http_code}\n" "http://localhost:8080/?n=[1-110]" | sort | uniq -c
```

Variáveis do shell sobrescrevem o `.env`. Para ver o bloqueio acabar sem esperar 5 minutos:

```bash
RATE_LIMIT_BLOCK_DURATION=5s docker compose up -d
```

Para ver as chaves no Redis: `docker compose exec redis redis-cli --scan --pattern 'ratelimit:*'`. Para zerar os contadores: `docker compose exec redis redis-cli FLUSHALL`.

### Rotas

| Rota | Limitada | Descrição |
|---|---|---|
| `/` (qualquer caminho e método) | sim | Responde `200` com um JSON simples. Representa a aplicação protegida. |
| `GET /healthz` | não | Health check. Fica fora do limiter para que monitoramento não consuma nem seja barrado pela cota de ninguém. |

Toda resposta limitada traz `X-RateLimit-Limit` e `X-RateLimit-Remaining`; o `429` traz também `Retry-After` (em segundos, arredondado para cima).

## Testes

Só com Docker (é o que o avaliador precisa):

```bash
docker compose run --rm --build test
```

O serviço `test` (profile `test`) usa o estágio `test` do [Dockerfile](Dockerfile) e sobe o Redis do compose. O estágio falha o build se algum arquivo não estiver formatado com `gofmt` ou se o `go vet` apontar problema, e roda `go test -race -count=1 -v ./...` com `REDIS_ADDR=redis:6379`, de modo que **os testes de integração rodam contra o Redis real**.

Sem o compose (apenas testes sem Redis; os de integração são pulados):

```bash
docker build --target test -t rate-limiter-test . && docker run --rm rate-limiter-test
```

Com Go 1.23+ instalado: `go test -race ./...`. Para incluir o Redis, defina `REDIS_ADDR=localhost:6379` e tenha um Redis acessível nesse endereço.

### O que os testes cobrem

| Pacote | Cobertura |
|---|---|
| `internal/limiter` | Limite por IP; bloqueio que sobrevive às janelas e expira; **token sobrepõe IP** (100 com token × 10 sem token); token mais restritivo que o IP também vence; IP bloqueado continua usando um token válido; token desconhecido cai no limite do IP; contadores independentes; bloqueio por token; o token nunca chega em texto puro ao armazenamento. Usa a estratégia em memória com relógio falso, sem nenhum `sleep`. |
| `internal/limiter/storetest` | **Suíte de conformidade** de qualquer `Store`: limite, independência de chaves, renovação da janela, bloqueio além da janela, bloqueio zero, **200 goroutines disputando um limite de 50** (exatamente 50 passam) e contexto cancelado. |
| `internal/limiter/memory` | Roda a suíte de conformidade, mais a limpeza de entradas expiradas e o bloqueio com relógio falso. |
| `internal/limiter/redisstore` | Roda a **mesma suíte contra o Redis real**, além de TTL das chaves, hash tag e erro com Redis fora do ar. |
| `internal/middleware` | Corpo e status exatos do 429, `Retry-After`, precedência do header `API_KEY`, 200 requisições concorrentes de um mesmo IP (10 × 200 e 190 × 429), *fail-closed*/*fail-open*, extração de IP com 16 cenários de `X-Forwarded-For`/`X-Real-IP`. |
| `internal/server` | Ponta a ponta com servidor HTTP real, **com a estratégia de memória e com o Redis**: 429 por IP, bloqueio persistindo após a janela e liberação no fim, token com limite maior passando, `/healthz` livre e clientes concorrentes. |
| `internal/config` | Padrões, todas as variáveis, erros reportados de uma vez, formato de tokens (sem vazar o token nas mensagens) e leitura do `.env` sem sobrescrever o ambiente. |

## Configuração

As variáveis podem vir do ambiente ou do arquivo `.env` na raiz; o ambiente tem prioridade. Rodando o binário direto, a aplicação lê o `.env` do diretório atual. No `docker compose`, os valores do `.env` (ou do shell) são repassados ao contêiner pelo [`docker-compose.yaml`](docker-compose.yaml), e `REDIS_ADDR` é sempre `redis:6379`.

O `.env` é versionado de propósito para o projeto rodar sem passo extra; ele não contém segredos. Para valores locais que não devem ir para o Git, use variáveis do shell.

| Variável | Padrão | Descrição |
|---|---|---|
| `RATE_LIMIT_IP` | `10` | Requisições aceitas por IP a cada janela (por segundo, com a janela padrão). |
| `RATE_LIMIT_TOKENS` | vazio | Limites por token, com precedência sobre o IP. Formato abaixo. |
| `RATE_LIMIT_BLOCK_DURATION` | `5m` | Tempo de bloqueio depois de exceder o limite. Aceita duração do Go (`30s`, `5m`, `1h`) ou segundos (`300`). `0` desliga o bloqueio extra: a identidade só espera a janela corrente acabar. |
| `RATE_LIMIT_WINDOW` | `1s` | Tamanho da janela de contagem. O enunciado pede requisições por segundo; mudar a janela serve para cenários como "100 por minuto". |
| `RATE_LIMIT_STORE` | `redis` | Estratégia de persistência: `redis` ou `memory`. |
| `RATE_LIMIT_TRUSTED_PROXIES` | vazio | CIDRs ou IPs de proxies confiáveis, separados por vírgula. Ver [IP do cliente](#ip-do-cliente). |
| `RATE_LIMIT_FAIL_OPEN` | `false` | O que fazer se o Redis falhar: `false` responde `503 rate limiter unavailable`; `true` deixa a requisição passar. |
| `REDIS_ADDR` | `localhost:6379` | Endereço do Redis. |
| `REDIS_PASSWORD` | vazio | Senha do Redis. |
| `REDIS_DB` | `0` | Banco do Redis. |
| `PORT` | `8080` | Porta HTTP. |

Valores inválidos impedem a aplicação de subir, e todos os erros aparecem juntos na mesma mensagem.

### Tokens

```env
RATE_LIMIT_TOKENS=abc123:100,def456:50:1m
```

Cada entrada é `token:limite` ou `token:limite:bloqueio`, separadas por vírgula:

- `abc123:100`: 100 requisições por janela, com o bloqueio padrão (`RATE_LIMIT_BLOCK_DURATION`);
- `def456:50:1m`: 50 requisições por janela, com bloqueio próprio de 1 minuto.

O token não pode conter `:` nem `,`. Ele é enviado no header `API_KEY`:

```bash
curl -H "API_KEY: abc123" http://localhost:8080/
```

> O nome do header tem sublinhado. Alguns proxies (o nginx, por padrão) descartam headers assim; nesse caso habilite `underscores_in_headers on;`.

## Regras de limitação

**Precedência Token > IP.** Se a requisição traz um token configurado, vale somente a regra do token, com contador próprio, e o IP não é contado. Com IP a 10 req/s e o token `abc123` a 100 req/s, a requisição com o token pode fazer 100 req/s. A regra do token vale também quando é mais restritiva que a do IP.

- Um IP bloqueado continua conseguindo usar um token válido; o token tem a sua própria cota.
- O contador do token é global: o mesmo token usado de vários IPs divide os mesmos 100 req/s.
- **Um token desconhecido é ignorado** e a requisição cai no limite do IP. Assim, inventar um token novo a cada requisição não é uma forma de escapar do limite.

**Algoritmo.** Janela fixa por identidade, mais uma chave de bloqueio com TTL:

1. se existe bloqueio para a identidade, nega sem contar (o `Retry-After` é o tempo restante do bloqueio);
2. senão incrementa o contador da janela, que expira junto com a janela;
3. se o contador passou do limite, cria o bloqueio com TTL = tempo de bloqueio, apaga o contador e nega.

No Redis isso é **um único script Lua** ([`redisstore/store.go`](internal/limiter/redisstore/store.go)), executado atomicamente: nenhuma requisição concorrente, mesmo vinda de várias réplicas da aplicação, passa do limite. As chaves são:

```
ratelimit:{ip:203.0.113.7}:count     contador da janela (TTL = janela)
ratelimit:{ip:203.0.113.7}:block     bloqueio            (TTL = tempo de bloqueio)
ratelimit:{token:3f1c0d...}:count    o token aparece só como hash SHA-256 truncado
```

A *hash tag* `{...}` coloca as duas chaves no mesmo slot, de modo que o script também funciona num Redis Cluster. O token nunca é gravado em texto puro no Redis nem nos logs.

### IP do cliente

Por padrão o IP é o da conexão TCP (`RemoteAddr`), e os headers `X-Forwarded-For`/`X-Real-IP` são **ignorados**: qualquer cliente pode forjá-los, e confiar neles sem critério permitiria escapar do limite trocando o header a cada requisição.

Atrás de um proxy ou balanceador, configure `RATE_LIMIT_TRUSTED_PROXIES` com os endereços dele (ex.: `10.0.0.0/8`). Se a conexão vier de um proxy confiável:

1. o `X-Forwarded-For` é percorrido **da direita para a esquerda**, pulando os proxies confiáveis; o primeiro endereço não confiável é o cliente. Entradas mais à esquerda foram escritas pelo próprio cliente e são ignoradas;
2. se não houver `X-Forwarded-For`, usa-se o `X-Real-IP`;
3. se nenhum dos dois for um IP válido, vale o IP da conexão.

Endereços IPv4 mapeados em IPv6 (`::ffff:1.2.3.4`) são normalizados para IPv4.

Exemplo, com `RATE_LIMIT_TRUSTED_PROXIES=10.0.0.0/8` e conexão vinda de `10.0.0.2`:

| `X-Forwarded-For` | IP considerado |
|---|---|
| `198.51.100.20` | `198.51.100.20` |
| `6.6.6.6, 198.51.100.20` | `198.51.100.20` (o `6.6.6.6` foi forjado pelo cliente) |
| `198.51.100.20, 10.9.9.9` | `198.51.100.20` (`10.9.9.9` é um proxy confiável) |

## Trocando a estratégia de persistência

A regra de negócio ([`internal/limiter`](internal/limiter/limiter.go)) depende só desta interface:

```go
type Store interface {
	Take(ctx context.Context, key string, rule Rule) (Result, error)
}
```

`Take` conta uma requisição para `key` e devolve se ela cabe no limite, de forma atômica. Já existem duas implementações:

| Estratégia | Pacote | Uso |
|---|---|---|
| Redis | [`internal/limiter/redisstore`](internal/limiter/redisstore/store.go) | Padrão; estado compartilhado entre réplicas. |
| Memória | [`internal/limiter/memory`](internal/limiter/memory/store.go) | Testes e instância única. `RATE_LIMIT_STORE=memory` sobe a aplicação sem Redis. |

Para usar outro mecanismo (Memcached, Postgres, DynamoDB...):

1. crie um pacote que implemente `limiter.Store`;
2. valide-o com a suíte de conformidade, a mesma que as duas estratégias atuais passam:

   ```go
   func TestStoreConformance(t *testing.T) {
   	storetest.Run(t, func(t *testing.T) limiter.Store { return meustore.New(...) })
   }
   ```

3. acrescente um `case` em `newStore`, em [`cmd/server/main.go`](cmd/server/main.go), e um valor para `RATE_LIMIT_STORE`.

O limiter, o middleware e os testes de regra de negócio não mudam.

## Estrutura

```
cmd/server/                  configuração, escolha da estratégia, servidor HTTP e graceful shutdown
internal/config/             variáveis de ambiente e .env
internal/limiter/            regra de negócio: identidade, precedência Token > IP e interface Store
internal/limiter/redisstore/ estratégia Redis (script Lua atômico)
internal/limiter/memory/     estratégia em memória
internal/limiter/storetest/  suíte de conformidade das estratégias
internal/middleware/         middleware HTTP e extração do IP do cliente
internal/server/             rotas da aplicação de exemplo e testes ponta a ponta
```

O middleware não conhece Redis e o limiter não conhece HTTP: o middleware só traduz a requisição em `limiter.Request{IP, Token}` e a decisão em resposta HTTP.

## Decisões e observações

- **Fail-closed por padrão.** Se o Redis cair, as requisições recebem `503` em vez de passar sem limite. `RATE_LIMIT_FAIL_OPEN=true` inverte isso para quem prefere disponibilidade.
- **Imagem final** `distroless/static`, com binário estático, sem shell e rodando como usuário não-root.
- **Relógio do Redis.** O Redis expira chaves pelo relógio de parede. Na VM do Docker Desktop/WSL2 esse relógio pode dar saltos de mais de um segundo, o que alonga um TTL. Por isso os testes que esperam um bloqueio ou uma janela terminar acompanham o `Retry-After` devolvido pelo armazenamento em vez de usar um `sleep` fixo; os testes de regra de negócio usam relógio falso.
