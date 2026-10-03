FROM golang:1.24.1-alpine3.21 AS builder

WORKDIR /src
COPY go.mod ./
COPY . .
RUN mkdir -p /out && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/docker-watcher ./cmd/watcher

FROM alpine:3.24.0
RUN apk add --no-cache ca-certificates docker-cli=29.8.2-r0 git

WORKDIR /app
COPY --from=builder /out/docker-watcher /usr/local/bin/docker-watcher
ENTRYPOINT ["/usr/local/bin/docker-watcher"]
