.PHONY: build test lint clean docker run-local deps

# Build the binary
build:
	go build -o bin/smart-router cmd/smart-router/main.go

# Run tests
test:
	go test -v ./...

# Run tests with coverage
test-coverage:
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

# Run linter
lint:
	golangci-lint run ./...

# Clean build artifacts
clean:
	rm -rf bin/ coverage.out coverage.html

# Download dependencies
deps:
	go mod download
	go mod tidy

# Build Docker image
docker:
	docker build -t smart-router:latest .

# Run locally with Redis
run-local: build
	REDIS_URL=redis://localhost:6379 \
	NAMESPACE=default \
	POD_LABEL_SELECTOR=app=voice-agent \
	VOICE_AGENT_BASE_URL=wss://localhost:8081 \
	./bin/smart-router

# Format code
fmt:
	go fmt ./...

# Vet code
vet:
	go vet ./...

# Check everything
check: fmt vet lint test

# Install tools
install-tools:
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest

.DEFAULT_GOAL := build