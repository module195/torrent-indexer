# Imagens fixadas por digest (o Dependabot atualiza): uma tag pode ser
# republicada com outro conteúdo.
FROM golang:1.25@sha256:699337d620559a59b4a2bb298ad59611e535d2ee755a34cf2d2a98f37578dc80 AS builder
WORKDIR /go/src/app

# Dependências numa camada própria: só voltam a ser baixadas quando go.mod/go.sum mudam.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o app .

FROM alpine:3.24@sha256:3f4d6b32e26703c6082839234f43769d5046b3ac7c910033911d1e71d8cd630a
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
