FROM golang:1.26-alpine AS builder

WORKDIR /src

# Сначала тянем зависимости
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG DATE=unknown

RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w \
      -X github.com/evgenza/otus-app/internal/version.Version=${VERSION} \
      -X github.com/evgenza/otus-app/internal/version.Date=${DATE}" \
    -o /out/app ./cmd/app \
    && CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/gateway ./cmd/gateway \
    && CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/chaosproxy ./cmd/chaosproxy

FROM alpine:3.20

RUN apk add --no-cache curl ca-certificates \
    && addgroup -S app && adduser -S app -G app

WORKDIR /app
COPY --from=builder /out/app /app/app
COPY --from=builder /out/gateway /app/gateway
COPY --from=builder /out/chaosproxy /app/chaosproxy

USER app
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD curl -fsS http://localhost:8080/health || exit 1

ENTRYPOINT ["/app/app"]
