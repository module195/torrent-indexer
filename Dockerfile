FROM golang:1.25 AS builder
WORKDIR /go/src/app

# Dependências numa camada própria: só voltam a ser baixadas quando go.mod/go.sum mudam.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o app .

# Tag fixa para builds reprodutíveis (alpine:latest mudava de versão sem aviso).
FROM alpine:3.24
LABEL org.opencontainers.image.source="https://github.com/module195/torrent-indexer"

RUN apk --no-cache add ca-certificates tzdata

WORKDIR /root/

COPY --from=builder /go/src/app/app .

EXPOSE 7006

CMD ["/root/app"]
