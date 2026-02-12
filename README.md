# Smart Router (Orchestration API)

Smart Router is a high-performance Go service responsible for allocating, managing, and draining voice agent pods. It acts as the intelligent layer between telephony providers (Twilio, Plivo, Exotel) and the voice agent infrastructure.

## Features

- **Provider Agnostic**: Unified `/allocate` endpoint for Twilio, Plivo, and Exotel.
- **Smart Allocation**: Tiered pod allocation (Dedicated -> Gold -> Standard -> Shared).
- **Graceful Draining**: Handles pod lifecycle and draining without dropping active calls.
- **Resilience**: Redis-backed state management and health checks.
- **Observability**: Prometheus metrics and structured logging.

## Prerequisites

- Go 1.21+
- Redis 7.0+
- Kubernetes (optional, for leader election features)

## Setup

1.  **Clone the repository** (if not already done).
2.  **Install dependencies**:
    ```bash
    make deps
    ```

## Running Locally

To run the service locally with default settings (requires running Redis on localhost:6379):

```bash
make run-local
```

You can configure the service using environment variables:

| Variable | Description | Default |
|----------|-------------|---------|
| `HTTP_PORT` | Port for API endpoints | `8080` |
| `METRICS_PORT` | Port for Prometheus metrics | `9090` |
| `REDIS_URL` | Redis connection string | `redis://localhost:6379` |
| `LOG_LEVEL` | Logging level | `info` |

## Building

To build the binary:

```bash
make build
```

This will create the `bin/smart-router` executable.

## Docker

To build the Docker image:

```bash
make docker
```

## Testing

To run unit tests:

```bash
make test
```

## API Endpoints

### Allocation
- `POST /api/v1/allocate`: Generic allocation
- `POST /api/v1/twilio/allocate`: Twilio webhook
- `POST /api/v1/plivo/allocate`: Plivo webhook
- `POST /api/v1/exotel/allocate`: Exotel webhook

### Management
- `POST /api/v1/release`: Release a pod
- `POST /api/v1/drain`: Drain a pod
- `GET /api/v1/status`: System status
- `GET /api/v1/pod/{pod_name}`: Pod details

### Observability
- `GET /health`: Liveness probe
- `GET /ready`: Readiness probe
- `GET /metrics`: Prometheus metrics
