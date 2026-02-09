# Multi-stage build: compile static Go binary for ARM64 (Graviton)
FROM golang:1.25-alpine AS builder

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build \
    -ldflags="-s -w" \
    -o msn-proxy \
    ./cmd/msn-proxy

# ---
# Runtime: scratch (static binary, nothing else)
FROM scratch
COPY --from=builder /build/msn-proxy /msn-proxy
ENTRYPOINT ["/msn-proxy"]
