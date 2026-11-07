# syntax=docker/dockerfile:1

# Imagem Debian (e não Alpine) porque o estágio de testes usa o race detector,
# que precisa de cgo/gcc.
FROM golang:1.23-bookworm AS base
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# Estágio de testes. O build falha se o código não estiver formatado com gofmt
# ou se o go vet apontar problemas; os testes rodam com -race.
# Com REDIS_ADDR definido (serviço "test" do compose) inclui os testes de
# integração contra o Redis real; sem ele, esses testes são pulados.
FROM base AS test
RUN test -z "$(gofmt -l .)" || (echo "arquivos sem gofmt:" && gofmt -l . && exit 1)
RUN go vet ./...
CMD ["go", "test", "-race", "-count=1", "-v", "./..."]

FROM base AS build
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# Imagem final: binário estático, sem shell, usuário não-root.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /server
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/server"]
