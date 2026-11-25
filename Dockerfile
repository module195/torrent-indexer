# Imagens fixadas por digest (o Dependabot atualiza): uma tag pode ser
# republicada com outro conteúdo.
FROM golang:1.27@sha256:205c7636d74e0ffbe3d007162df7d74f859f5a8c8322dfb9f907977686c126cf AS builder
WORKDIR /go/src/app

# Dependências numa camada própria: só voltam a ser baixadas quando go.mod/go.sum mudam.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o app .

FROM alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf
LABEL org.opencontainers.image.source="https://github.com/module195/torrent-indexer"

RUN apk --no-cache add ca-certificates tzdata \
    && adduser -D -H -u 10001 app

WORKDIR /app

COPY --from=builder /go/src/app/app .

# O processo não grava em disco; roda sem root para que uma falha nele não
# vire root no container.
USER 10001

EXPOSE 7006

CMD ["/app/app"]
