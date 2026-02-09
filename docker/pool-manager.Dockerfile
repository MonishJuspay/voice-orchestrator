# Build stage
FROM golang:1.25.7-alpine AS builder

# Install build dependencies
RUN apk add --no-cache git ca-certificates tzdata

# Set working directory
WORKDIR /build

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Build the pool-manager binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-w -s" \
    -o /pool-manager \
    ./cmd/pool-manager

# Final stage
FROM alpine:3.19

# Install runtime dependencies
RUN apk add --no-cache ca-certificates tzdata

# Create non-root user
RUN addgroup -g 1000 appuser && \
    adduser -D -u 1000 -G appuser appuser

# Copy binary from builder
COPY --from=builder /pool-manager /usr/local/bin/pool-manager

# Change ownership
RUN chown appuser:appuser /usr/local/bin/pool-manager

# Switch to non-root user
USER appuser

# No ports exposed (background worker)

# Health check not applicable for background worker

# Run the binary
ENTRYPOINT ["/usr/local/bin/pool-manager"]
