# Stage 1: Build the binary
FROM golang:alpine AS builder

WORKDIR /build

# Install build dependencies
RUN apk add --no-cache git ca-certificates tzdata

# Cache Go modules
COPY go.mod go.sum ./
RUN go mod download

# Copy source code and build statically linked binary
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /build/bin/findr ./cmd/findr

# Stage 2: Minimal production container
FROM alpine:latest

# Install runtime dependencies: ca-certificates for TLS, tzdata, curl for healthcheck
RUN apk add --no-cache ca-certificates tzdata curl

# Create non-root application user
RUN addgroup -g 1000 findr && \
    adduser -u 1000 -G findr -s /bin/sh -D findr

# Copy compiled binary from builder
COPY --from=builder /build/bin/findr /usr/local/bin/findr

# Create required mount directories with correct ownership
RUN mkdir -p /config /music /downloads && \
    chown -R findr:findr /config /music /downloads

WORKDIR /config
USER findr:findr

# Expose default HTTP daemon port
EXPOSE 8686

# Healthcheck for docker stack orchestrators
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD curl -f http://localhost:8686/healthz || exit 1

# Default entrypoint is the findr CLI
ENTRYPOINT ["/usr/local/bin/findr"]

# Default command runs in daemon mode
CMD ["daemon", "--config", "/config/config.yaml"]
