.PHONY: help install-deps build build-router build-pool-manager run-router run-pool-manager test clean docker-build lint fmt

# Variables
GO := go
GOFLAGS := -v
BINARY_DIR := bin
ROUTER_BINARY := $(BINARY_DIR)/router
POOL_MANAGER_BINARY := $(BINARY_DIR)/pool-manager
DOCKER_REGISTRY ?= ghcr.io/monishjuspay
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT_SHA := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME := $(shell date -u '+%Y-%m-%d_%H:%M:%S')

# Build flags
LDFLAGS := -ldflags "-X main.Version=$(VERSION) -X main.CommitSHA=$(COMMIT_SHA) -X main.BuildTime=$(BUILD_TIME)"

help: ## Show this help message
	@echo "Voice Orchestrator - Available targets:"
	@echo ""
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'
	@echo ""

install-deps: ## Download Go dependencies
	@echo "==> Downloading Go dependencies..."
	$(GO) mod download
	$(GO) mod tidy
	@echo "==> Dependencies installed successfully!"

build: build-router build-pool-manager ## Build both router and pool-manager binaries

build-router: ## Build router binary
	@echo "==> Building router..."
	@mkdir -p $(BINARY_DIR)
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $(ROUTER_BINARY) ./cmd/router
	@echo "==> Router binary created at $(ROUTER_BINARY)"

build-pool-manager: ## Build pool-manager binary
	@echo "==> Building pool-manager..."
	@mkdir -p $(BINARY_DIR)
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $(POOL_MANAGER_BINARY) ./cmd/pool-manager
	@echo "==> Pool manager binary created at $(POOL_MANAGER_BINARY)"

run-router: build-router ## Run router service locally
	@echo "==> Running router service..."
	./$(ROUTER_BINARY)

run-pool-manager: build-pool-manager ## Run pool-manager service locally
	@echo "==> Running pool-manager service..."
	./$(POOL_MANAGER_BINARY)

test: ## Run tests with coverage
	@echo "==> Running tests..."
	$(GO) test -v -race -coverprofile=coverage.out -covermode=atomic ./...
	@echo "==> Coverage report:"
	$(GO) tool cover -func=coverage.out

test-coverage: test ## Generate HTML coverage report
	@echo "==> Generating HTML coverage report..."
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "==> Coverage report saved to coverage.html"

clean: ## Clean build artifacts
	@echo "==> Cleaning build artifacts..."
	rm -rf $(BINARY_DIR)
	rm -f coverage.out coverage.html
	$(GO) clean -cache -testcache
	@echo "==> Clean complete!"

docker-build: ## Build Docker images for both services
	@echo "==> Building Docker images..."
	docker build -f docker/router.Dockerfile -t $(DOCKER_REGISTRY)/voice-orchestrator-router:$(VERSION) .
	docker build -f docker/pool-manager.Dockerfile -t $(DOCKER_REGISTRY)/voice-orchestrator-pool-manager:$(VERSION) .
	@echo "==> Docker images built successfully!"
	@echo "    - $(DOCKER_REGISTRY)/voice-orchestrator-router:$(VERSION)"
	@echo "    - $(DOCKER_REGISTRY)/voice-orchestrator-pool-manager:$(VERSION)"

docker-push: docker-build ## Push Docker images to registry
	@echo "==> Pushing Docker images..."
	docker push $(DOCKER_REGISTRY)/voice-orchestrator-router:$(VERSION)
	docker push $(DOCKER_REGISTRY)/voice-orchestrator-pool-manager:$(VERSION)
	@echo "==> Docker images pushed successfully!"

lint: ## Run golangci-lint
	@echo "==> Running linter..."
	@which golangci-lint > /dev/null || (echo "golangci-lint not found. Install from https://golangci-lint.run/usage/install/" && exit 1)
	golangci-lint run --timeout=5m ./...

fmt: ## Format code with gofmt and goimports
	@echo "==> Formatting code..."
	@which goimports > /dev/null || $(GO) install golang.org/x/tools/cmd/goimports@latest
	gofmt -s -w .
	goimports -w .
	@echo "==> Code formatted successfully!"

vet: ## Run go vet
	@echo "==> Running go vet..."
	$(GO) vet ./...

verify: fmt vet lint test ## Run all verification steps (fmt, vet, lint, test)

k8s-deploy-router: ## Deploy router to Kubernetes
	@echo "==> Deploying router to Kubernetes..."
	kubectl apply -f deployments/router/

k8s-deploy-pool-manager: ## Deploy pool-manager to Kubernetes
	@echo "==> Deploying pool-manager to Kubernetes..."
	kubectl apply -f deployments/pool-manager/

k8s-deploy: k8s-deploy-router k8s-deploy-pool-manager ## Deploy both services to Kubernetes

k8s-delete: ## Delete deployments from Kubernetes
	@echo "==> Deleting deployments from Kubernetes..."
	kubectl delete -f deployments/router/ --ignore-not-found=true
	kubectl delete -f deployments/pool-manager/ --ignore-not-found=true

dev-setup: install-deps ## Setup local development environment
	@echo "==> Setting up local development environment..."
	@cp -n .env.example .env 2>/dev/null || true
	@echo "==> Development environment ready!"
	@echo "==> Edit .env file with your configuration"

deps-upgrade: ## Upgrade all dependencies
	@echo "==> Upgrading dependencies..."
	$(GO) get -u ./...
	$(GO) mod tidy
	@echo "==> Dependencies upgraded!"

version: ## Show version information
	@echo "Version:    $(VERSION)"
	@echo "Commit:     $(COMMIT_SHA)"
	@echo "Build Time: $(BUILD_TIME)"
