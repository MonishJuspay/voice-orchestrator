#!/bin/bash
set -e

echo "==> Voice Orchestrator - Development Setup"
echo ""

# Check if Go is installed
if ! command -v go &> /dev/null; then
    echo "❌ Go is not installed"
    echo "Please install Go 1.25.7 first:"
    echo "  - macOS: brew install go@1.25"
    echo "  - Linux: Run scripts/install-go.sh"
    echo "  - Or download from: https://go.dev/dl/"
    exit 1
fi

GO_VERSION=$(go version | awk '{print $3}' | sed 's/go//')
echo "✅ Go version: $GO_VERSION"
echo ""

# Create .env file if it doesn't exist
if [ ! -f .env ]; then
    echo "==> Creating .env file from .env.example..."
    cp .env.example .env
    echo "✅ Created .env file"
    echo "⚠️  Please edit .env with your local configuration"
else
    echo "✅ .env file already exists"
fi
echo ""

# Download dependencies
echo "==> Downloading Go dependencies..."
go mod download
go mod tidy
echo "✅ Dependencies downloaded"
echo ""

# Check if Docker is running (optional)
if command -v docker &> /dev/null; then
    if docker info &> /dev/null; then
        echo "✅ Docker is running"
    else
        echo "⚠️  Docker is installed but not running"
        echo "   Start Docker Desktop to use containerization features"
    fi
else
    echo "⚠️  Docker not found (optional for local development)"
fi
echo ""

# Check if kubectl is installed (optional)
if command -v kubectl &> /dev/null; then
    echo "✅ kubectl is installed"
else
    echo "⚠️  kubectl not found (optional for K8s deployment)"
fi
echo ""

# Check if golangci-lint is installed (optional)
if command -v golangci-lint &> /dev/null; then
    echo "✅ golangci-lint is installed"
else
    echo "⚠️  golangci-lint not found (optional for linting)"
    echo "   Install with: brew install golangci-lint"
fi
echo ""

echo "==> Setup complete! 🎉"
echo ""
echo "Next steps:"
echo "  1. Edit .env with your local configuration"
echo "  2. Start local Redis:     docker run -d -p 6379:6379 redis:alpine"
echo "  3. Start local Postgres:  docker run -d -p 5432:5432 -e POSTGRES_PASSWORD=postgres postgres:16-alpine"
echo "  4. Build binaries:        make build"
echo "  5. Run router:            make run-router"
echo "  6. Run pool-manager:      make run-pool-manager"
echo ""
echo "Available make targets:"
echo "  make help          - Show all available commands"
echo "  make build         - Build both services"
echo "  make test          - Run tests"
echo "  make lint          - Run linter"
echo "  make docker-build  - Build Docker images"
echo ""
