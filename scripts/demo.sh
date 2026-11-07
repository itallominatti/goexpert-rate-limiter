#!/usr/bin/env bash
# Demonstra o rate limiter contra a aplicação do docker compose.
#
# Uso: ./scripts/demo.sh [url]   (padrão: http://localhost:8080)
#
# Usa rajadas paralelas do curl (>= 7.66): requisições em série demoram mais
# que a janela de 1s e não chegariam a 100 req/s.
# Espera o .env padrão: IP a 10 req/s e token abc123 a 100 req/s.
set -euo pipefail

URL="${1:-http://localhost:8080}"

burst() { # burst <quantidade> [token]
	local n="$1" token="${2:-}"
	local args=(-s -Z --parallel-max 200 -o /dev/null -w "%{http_code}\n")
	[[ -n "$token" ]] && args+=(-H "API_KEY: $token")
	curl "${args[@]}" "$URL/?n=[1-$n]" | sort | uniq -c | sed 's/^/    /'
}

status() { # status [token]
	local args=(-s -o /dev/null -w "%{http_code} (Retry-After: %header{retry-after})\n")
	[[ -n "${1:-}" ]] && args+=(-H "API_KEY: $1")
	curl "${args[@]}" "$URL/" | sed 's/^/    /'
}

echo "1) Rajada de 20 requisições sem token (limite por IP: 10 req/s)"
burst 20

echo "2) Corpo da resposta 429"
printf '    '; curl -s "$URL/"; echo

echo "3) Rajada de 110 requisições com API_KEY abc123 (limite do token: 100 req/s)"
echo "   O IP já está bloqueado, mas o token tem precedência e cota própria."
burst 110 abc123

echo "4) Token desconhecido não escapa do limite: cai na regra do IP, que está bloqueado"
status token-inventado

echo "5) Três segundos depois (bem além da janela de 1s), o IP continua bloqueado"
sleep 3
status
