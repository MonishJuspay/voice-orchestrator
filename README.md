# Voice Orchestrator

A production-ready Go microservice for orchestrating voice pod allocation and pool management in Kubernetes.

## 📋 Overview

Voice Orchestrator consists of two independent services:

1. **Router Service** - High-performance HTTP API for pod allocation
2. **Pool Manager Service** - Background reconciliation worker for K8s pod management

### Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                    Voice Orchestrator                        │
├──────────────────────┬──────────────────────────────────────┤
│   Router Service     │      Pool Manager Service             │
│   (HTTP API)         │      (Background Worker)              │
│                      │                                       │
│ • Fast allocation    │ • Reconciles desired vs actual pods   │
│ • Multiple replicas  │ • Single replica only                 │
│ • Reads from Redis   │ • Reads from Postgres                 │
│ • Health checks      │ • Scales K8s deployments              │
│                      │ • Syncs Redis state                   │
└──────────────────────┴───────────────────────────────────────┘
           │                           │
           ├───────────────────────────┤
           │                           │
        ┌──▼───┐    ┌──────────┐    ┌─▼────────┐
        │Redis │    │Postgres  │    │Kubernetes│
        └──────┘    └──────────┘    └──────────┘
```

### Data Flow

```
1. Merchant Config → Postgres (merchants table with desired_pod_count)
                         ↓
2. Pool Manager reads Postgres every 10s
                         ↓
3. Pool Manager scales K8s deployments (if needed)
                         ↓
4. Pool Manager syncs Redis (merchant:{id}:pod_count)
                         ↓
5. Router reads Redis for fast pod allocation
```

---

## 🚀 Quick Start

### Prerequisites

- **Go 1.25.7** (see [Installation](#installation))
- **Docker** (optional, for containerization)
- **kubectl** (optional, for K8s deployment)
- **Redis** (for caching)
- **PostgreSQL 16+** (for merchant data)
- **Kubernetes cluster** (for production deployment)

### Installation

#### Option 1: Automated Setup

```bash
# Clone the repository
git clone https://github.com/MonishJuspay/voice-orchestrator.git
cd voice-orchestrator

# Run setup script
./scripts/setup-dev.sh

# Edit environment variables
cp .env.example .env
nano .env
```

#### Option 2: Manual Setup

```bash
# Install Go 1.25.7 (Linux/macOS)
./scripts/install-go.sh

# Or download manually from https://go.dev/dl/

# Verify Go installation
go version  # Should show: go version go1.25.7 ...

# Download dependencies
make install-deps
```

### Running Locally

#### Start Dependencies

```bash
# Start Redis
docker run -d -p 6379:6379 --name redis redis:alpine

# Start PostgreSQL
docker run -d -p 5432:5432 \
  -e POSTGRES_PASSWORD=postgres \
  -e POSTGRES_DB=voice_orchestrator \
  --name postgres postgres:16-alpine
```

#### Run Services

```bash
# Terminal 1: Run Router
make run-router

# Terminal 2: Run Pool Manager
make run-pool-manager
```

#### Verify Services

```bash
# Check Router health
curl http://localhost:8080/health

# Expected: {"status": "healthy"}
```

---

## 🛠️ Development

### Build Commands

```bash
make help              # Show all available commands
make build             # Build both services
make build-router      # Build router only
make build-pool-manager # Build pool-manager only
make clean             # Clean build artifacts
```

### Testing

```bash
make test              # Run tests with coverage
make test-coverage     # Generate HTML coverage report
```

### Code Quality

```bash
make fmt               # Format code
make vet               # Run go vet
make lint              # Run golangci-lint
make verify            # Run all checks (fmt + vet + lint + test)
```

### Docker

```bash
# Build Docker images
make docker-build

# Push to registry (set DOCKER_REGISTRY env var)
make docker-push

# Example with custom registry
DOCKER_REGISTRY=myregistry.io/myorg make docker-build
```

---

## 📦 Deployment

### Kubernetes Deployment

#### Prerequisites

```bash
# Ensure kubectl is configured
kubectl cluster-info

# Create namespace (optional)
kubectl create namespace voice-orchestrator
```

#### Deploy Services

```bash
# Deploy both services
make k8s-deploy

# Or deploy individually
make k8s-deploy-router
make k8s-deploy-pool-manager
```

#### Configure Secrets

```bash
# Edit secrets with real credentials
kubectl edit secret router-secret -n default
kubectl edit secret pool-manager-secret -n default
```

#### Verify Deployment

```bash
# Check pods
kubectl get pods -l app=voice-orchestrator

# Check services
kubectl get svc router-service

# View logs
kubectl logs -l component=router -f
kubectl logs -l component=pool-manager -f
```

#### Cleanup

```bash
make k8s-delete
```

---

## 🔧 Configuration

All configuration is via environment variables. See [`.env.example`](.env.example) for all options.

### Router Service

| Variable | Description | Default |
|----------|-------------|---------|
| `ENVIRONMENT` | Environment (development/production) | `development` |
| `LOG_LEVEL` | Log level (debug/info/warn/error) | `info` |
| `HTTP_PORT` | HTTP server port | `8080` |
| `HTTP_READ_TIMEOUT` | Read timeout | `30s` |
| `HTTP_WRITE_TIMEOUT` | Write timeout | `30s` |
| `REDIS_ADDR` | Redis address | `localhost:6379` |
| `POSTGRES_HOST` | Postgres host | `localhost` |
| `K8S_NAMESPACE` | K8s namespace | `default` |

### Pool Manager Service

| Variable | Description | Default |
|----------|-------------|---------|
| `RECONCILE_INTERVAL` | Reconciliation interval | `10s` |
| `REDIS_ADDR` | Redis address | `localhost:6379` |
| `POSTGRES_HOST` | Postgres host | `localhost` |
| `K8S_NAMESPACE` | K8s namespace | `default` |
| `K8S_IN_CLUSTER` | Running in K8s cluster | `false` |

---

## 📚 API Documentation

### Router Endpoints

#### Health Check

```bash
GET /health
```

**Response:**
```json
{
  "status": "healthy"
}
```

#### Readiness Check

```bash
GET /ready
```

**Response:**
```json
{
  "status": "ready",
  "redis": "ok",
  "postgres": "ok",
  "kubernetes": "ok"
}
```

#### Allocate Pods (TODO)

```bash
POST /api/v1/allocate
Content-Type: application/json

{
  "merchant_id": "merchant-123",
  "pod_count": 5
}
```

**Response:**
```json
{
  "merchant_id": "merchant-123",
  "allocated_pods": [
    {
      "pod_id": "pod-1",
      "ip": "10.0.1.5",
      "status": "ready"
    }
  ]
}
```

#### Admin API (TODO)

- `POST /api/v1/admin/merchants` - Create merchant
- `GET /api/v1/admin/merchants/:id` - Get merchant
- `PUT /api/v1/admin/merchants/:id` - Update merchant
- `DELETE /api/v1/admin/merchants/:id` - Delete merchant

---

## 🏗️ Project Structure

```
voice-orchestrator/
├── cmd/
│   ├── router/              # Router entry point
│   └── pool-manager/        # Pool manager entry point
├── internal/
│   ├── app/
│   │   ├── router/          # HTTP handlers & server
│   │   ├── poolmanager/     # Reconciliation logic
│   │   └── admin/           # Admin API handlers
│   ├── datastore/
│   │   ├── redis/           # Redis client & operations
│   │   └── postgres/        # Postgres client & queries
│   ├── domain/              # Domain models & DTOs
│   ├── k8s/                 # Kubernetes client wrapper
│   └── config/              # Configuration management
├── pkg/
│   └── logger/              # Structured logging (zap)
├── deployments/
│   ├── router/              # K8s manifests for router
│   └── pool-manager/        # K8s manifests for pool-manager
├── docker/                  # Dockerfiles
├── scripts/                 # Helper scripts
├── migrations/              # Database migrations
├── Makefile                 # Build automation
├── go.mod                   # Go module definition
└── README.md                # This file
```

---

## 🧪 Testing

### Run All Tests

```bash
make test
```

### Run Specific Tests

```bash
go test -v ./internal/domain/...
go test -v ./internal/app/router/...
```

### Coverage Report

```bash
make test-coverage
open coverage.html  # macOS
xdg-open coverage.html  # Linux
```

---

## 🐛 Troubleshooting

### Build Issues

**Issue:** `go: cannot find module`

```bash
# Solution: Download dependencies
make install-deps
```

**Issue:** `Go version mismatch`

```bash
# Solution: Install Go 1.25.7
./scripts/install-go.sh
```

### Runtime Issues

**Issue:** `Cannot connect to Redis`

```bash
# Check Redis is running
docker ps | grep redis

# Start Redis if needed
docker run -d -p 6379:6379 --name redis redis:alpine
```

**Issue:** `Cannot connect to Postgres`

```bash
# Check Postgres is running
docker ps | grep postgres

# Start Postgres if needed
docker run -d -p 5432:5432 \
  -e POSTGRES_PASSWORD=postgres \
  -e POSTGRES_DB=voice_orchestrator \
  --name postgres postgres:16-alpine
```

**Issue:** `K8s client error: unable to load in-cluster configuration`

```bash
# Solution: Set K8S_IN_CLUSTER=false in .env
echo "K8S_IN_CLUSTER=false" >> .env
```

### Kubernetes Issues

**Issue:** `ImagePullBackOff`

```bash
# Check image exists
docker images | grep voice-orchestrator

# Build and push images
make docker-build docker-push
```

**Issue:** `CrashLoopBackOff`

```bash
# Check logs
kubectl logs -l component=router --tail=50

# Check secrets
kubectl get secret router-secret -o yaml
```

---

## 📊 Monitoring & Observability

### Logs

**Structured JSON logging** (production):
```json
{
  "level": "info",
  "ts": "2024-01-15T10:30:45.123Z",
  "caller": "router/handler.go:45",
  "msg": "Request processed",
  "merchant_id": "merchant-123",
  "duration_ms": 15
}
```

**Console logging** (development):
```
2024-01-15T10:30:45.123Z  INFO  router/handler.go:45  Request processed  merchant_id=merchant-123 duration_ms=15
```

### Metrics (Future)

- Prometheus integration planned
- Metrics endpoints: `/metrics`
- Key metrics: request rate, latency, pod allocation success rate

---

## 🤝 Contributing

### Development Workflow

1. **Clone repository**
   ```bash
   git clone https://github.com/MonishJuspay/voice-orchestrator.git
   cd voice-orchestrator
   ```

2. **Setup environment**
   ```bash
   ./scripts/setup-dev.sh
   ```

3. **Create feature branch**
   ```bash
   git checkout -b feature/my-feature
   ```

4. **Make changes and test**
   ```bash
   make verify  # Run all checks
   ```

5. **Commit and push**
   ```bash
   git add .
   git commit -m "Add feature: description"
   git push origin feature/my-feature
   ```

6. **Create pull request**

### Code Standards

- **Format code**: `make fmt` before committing
- **Pass linting**: `make lint` must pass
- **Write tests**: Maintain >80% coverage
- **Document changes**: Update README if needed

---

## 📝 License

Copyright © 2024 Juspay Technologies. All rights reserved.

---

## 🙋 Support

- **Issues**: [GitHub Issues](https://github.com/MonishJuspay/voice-orchestrator/issues)
- **Documentation**: [Wiki](https://github.com/MonishJuspay/voice-orchestrator/wiki)
- **Contact**: Monish P <monish.p@juspay.in> Harsh Tiwari

---

## 🗺️ Roadmap

### Current Status (v0.1.0)

✅ Project structure and boilerplate  
✅ Configuration management  
✅ Logging infrastructure  
✅ Docker & Kubernetes manifests  
✅ CI/CD pipelines  
⏳ Business logic implementation (in progress)

### Planned Features

- [ ] Complete pod allocation logic
- [ ] Redis caching layer
- [ ] Postgres repository implementation
- [ ] K8s deployment scaling
- [ ] Prometheus metrics
- [ ] Admin UI
- [ ] Rate limiting
- [ ] Authentication & authorization
- [ ] Database migrations
- [ ] Grafana dashboards

---

**Built with ❤️ using Go 1.25.7**
