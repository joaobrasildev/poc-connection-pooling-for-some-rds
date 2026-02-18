# ============================================================================
# Dockerfile — Connection Pooling Proxy (multi-stage build)
# ============================================================================

# ── Build stage ──────────────────────────────────────────────────────────
FROM golang:1.24-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /build

# Copy go mod files first for layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the proxy binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-w -s" \
    -o /build/proxy \
    ./cmd/proxy/

# ── Runtime stage ────────────────────────────────────────────────────────
FROM alpine:3.19

RUN apk add --no-cache ca-certificates wget

WORKDIR /app

# Copy binary from builder
COPY --from=builder /build/proxy /app/proxy

# Copy configs (can be overridden via volume mount)
COPY configs/ /app/configs/

EXPOSE 1433 8080 9090

ENTRYPOINT ["/app/proxy"]
CMD ["--config", "/app/configs/proxy.yaml", "--buckets", "/app/configs/buckets.yaml"]
