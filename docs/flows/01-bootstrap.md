# Flow 01: Application Bootstrap

**Source file:** `cmd/smart-router/main.go` (270 lines)

## Overview

The `main()` function is the single entry point for the Smart Router binary. It wires together every component — config, Redis, K8s client, allocator, releaser, drainer, pool manager, HTTP server, metrics server — then blocks until a shutdown signal (SIGINT/SIGTERM) arrives.

**Every Smart Router pod (all 3 replicas) runs the exact same binary.** The difference between the leader and non-leader replicas is determined at runtime by the leader election subsystem inside the pool manager.

## Startup Sequence (Line by Line)

```
main.go:28   ctx, cancel := context.WithCancel(context.Background())
```

A root context is created. **Every** background goroutine (pool manager, health checks, etc.) derives from this context. Calling `cancel()` is the kill switch that propagates to all children.

### Step 1: Load Configuration (line 33-37)

```go
cfg, err := config.Load()
```

- Reads ALL env vars (Redis, HTTP, K8s, Pool Manager, Tier Config, Leader Election, Logging)
- Parses `TIER_CONFIG` JSON into private fields (`parsedTierConfig` map + `defaultChain` slice) behind a `sync.RWMutex`
- If `TIER_CONFIG` is empty, falls back to hardcoded defaults: gold(10), standard(20), overflow(5)
- **Fatal on failure** — process exits immediately
- **Note:** At this point the config is loaded from the env var only. Redis bootstrap (Step 3b) may override it.

**See:** [Flow 02: Configuration](./02-configuration.md) for full TIER_CONFIG parsing logic.

### Step 2: Setup Logger (line 40-44)

```go
logger, err := setupLogger(cfg)
```

- Uses `zap.NewProductionConfig()` (JSON output by default)
- Reads `cfg.LogLevel` (from `LOG_LEVEL` env var, default "info")
- If `cfg.LogFormat == "console"`, switches to development encoder (human-readable)
- **Fatal on failure**

### Step 3: Create Redis Client (line 53-67)

```go
redisClient, err := redisclient.NewClient(cfg)
```

Creates a `redis.Client` (go-redis v9) with connection pool settings:

| Setting | Env Var | Default |
|---------|---------|---------|
| Pool Size | `REDIS_POOL_SIZE` | 10 |
| Min Idle Conns | `REDIS_MIN_IDLE_CONN` | 5 |
| Max Retries | `REDIS_MAX_RETRIES` | 3 |
| Dial Timeout | `REDIS_DIAL_TIMEOUT` | 5s |

Parses `REDIS_URL` (default `redis://localhost:6379`) using `redis.ParseURL()`.

Immediately pings Redis (`redisClient.Ping(ctx)`) — **fatal if Redis is unreachable** at startup.

A `defer` ensures `redisClient.Close()` runs on shutdown.

### Step 3b: Bootstrap Tier Config to Redis (line 69-72)

```go
cfg.BootstrapTierConfigToRedis(ctx, redisClient.GetRedis(), logger)
```

- Serialises the current in-memory tier config (from the env var) into the structured envelope JSON format
- Writes to Redis key `voice:tier:config` using **SETNX** (SET if Not eXists)
- **If the key didn't exist** (first deploy or key was manually deleted): the env var config is seeded into Redis. Log: `"Tier config bootstrapped to Redis"`
- **If the key already existed** (normal restart): the Redis value is loaded into memory, **overriding the env var**. This ensures Redis is the source of truth after first deploy.
- On Redis error: logs a warning and keeps the env var config (degraded but functional)

**See:** [Flow 02: Configuration](./02-configuration.md) and [Flow 15: Tier Config Refresh](./15-tier-config-refresh.md) for the full Redis-backed tier config lifecycle.

### Step 3c: Start Tier Config Refresh Goroutine (line 74-75)

```go
go runTierConfigRefresh(ctx, cfg, redisClient, logger)
```

- Starts a background goroutine that runs on **ALL replicas** (not just the leader)
- Every 30 seconds (`config.TierConfigRefreshInterval`), reads `voice:tier:config` from Redis and swaps the in-memory cache
- On Redis error or missing key: keeps current config, logs warning
- Uses the root context — stops cleanly on shutdown

**See:** [Flow 15: Tier Config Refresh](./15-tier-config-refresh.md)

### Step 4: Create Kubernetes Client (line 78-89)

```go
var k8sClient *kubernetes.Clientset
if cfg.LeaderElectionEnabled {
    k8sClient, err = createK8sClient()
}
```

- Only attempts K8s client creation if `LEADER_ELECTION_ENABLED=true` (default)
- Uses `rest.InClusterConfig()` — **only works inside a K8s pod**
- If it fails (e.g. running locally), **logs a warning and disables leader election** rather than crashing
- This allows the binary to run locally for development without K8s

### Step 5: Create Components (line 91-102)

```go
alloc := allocator.NewAllocator(redisClient.GetRedis(), cfg, logger)
rel := releaser.NewReleaser(redisClient, cfg, logger)
podDrainer := drainer.NewDrainer(redisClient, cfg, logger)
```

All three are stateless — they hold references to the Redis client, config, and logger. No goroutines are started yet.

```go
var poolManager *poolmanager.Manager
if cfg.LeaderElectionEnabled && k8sClient != nil {
    poolManager = poolmanager.NewManager(k8sClient, redisClient.GetRedis(), cfg, logger)
}
```

Pool Manager is only created if leader election is enabled AND a K8s client was successfully created. It's `nil` otherwise (for local dev).

### Step 6: Create HTTP Router (line 104-109)

```go
var leader api.LeaderChecker
if poolManager != nil {
    leader = poolManager   // poolManager implements IsLeader()
}
router := api.NewRouter(alloc, rel, podDrainer, redisClient, cfg, logger, leader)
```

- The `leader` variable is nil-safe — `api.NewRouter` uses the `LeaderChecker` interface
- When `leader == nil`, status handler reports `is_leader: false`
- The router is a Chi router with middleware stack + all API routes

**See:** [Flow 12: HTTP Layer](./12-http-layer.md) for full route definitions.

### Step 7: Start HTTP Server (line 111-118)

```go
httpServer := &http.Server{
    Addr:         ":" + cfg.HTTPPort,        // default :8080
    Handler:      router,
    ReadTimeout:  cfg.ReadTimeout,           // default 5s
    WriteTimeout: cfg.WriteTimeout,          // default 10s
    IdleTimeout:  cfg.IdleTimeout,           // default 60s
}
```

### Step 8: Start Metrics Server (line 120-133)

```go
if cfg.MetricsPort != cfg.HTTPPort {
    metricsMux := http.NewServeMux()
    metricsMux.Handle("/metrics", promhttp.Handler())
    metricsMux.HandleFunc("/health", ...)
    metricsServer = &http.Server{ Addr: ":" + cfg.MetricsPort }
}
```

- Only created if metrics port differs from HTTP port (default: 9090 vs 8080)
- Serves `/metrics` (Prometheus scrape endpoint) and a simple `/health` endpoint
- Uses a **separate minimal mux** — no business routes on the metrics port

### Step 9: Start Background Goroutines (line 135-167)

```go
go runHealthChecks(ctx, redisClient, logger)   // line 136
go poolManager.Run(ctx)                         // line 142 (if poolManager != nil)
go httpServer.ListenAndServe()                  // line 154
go metricsServer.ListenAndServe()               // line 162 (if exists)
```

**5 goroutines launched** (the tier config refresh goroutine was already started in Step 3c):

1. **Tier Config Refresh** (started in Step 3c) — reads `voice:tier:config` from Redis every 30s, swaps in-memory cache
2. **Health Check** — pings Redis every 30s, logs warnings on failure (line 235-249)
3. **Pool Manager** — runs leader election + leader workload (watcher, reconciler, zombie cleanup)
4. **HTTP Server** — serves API traffic on port 8080
5. **Metrics Server** — serves Prometheus metrics on port 9090

### Step 10: Wait for Shutdown (line 175-201)

```go
quit := make(chan os.Signal, 1)
signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
<-quit   // BLOCKS HERE until signal received
```

When a signal arrives:

1. **Cancel root context** (`cancel()` at line 179) — stops all background goroutines
2. **Create shutdown context** with `cfg.ShutdownTimeout` (default 30s)
3. **Shutdown HTTP server** gracefully — waits for in-flight requests to complete
4. **Shutdown metrics server** gracefully
5. Process exits

## Component Wiring Diagram

```
                        ┌──────────────────────────────────────────────┐
                        │              main.go                          │
                        │                                               │
  config.Load() ────►   │  cfg ──────────────────────────────────────┐  │
                        │   │                                        │  │
  redisclient.New() ──► │  redis ────────────────────────────────┐   │  │
                        │   │                                    │   │  │
                        │   ├── BootstrapTierConfigToRedis()      │   │  │
                        │   │   (SETNX voice:tier:config)        │   │  │
                        │   │                                    │   │  │
                        │   ├── runTierConfigRefresh() goroutine  │   │  │
                        │   │   (GET voice:tier:config every 30s) │   │  │
                        │   │                                    │   │  │
  createK8sClient() ──► │  k8s ──────────────────┐              │   │  │
                        │   │                     │              │   │  │
                        │   ▼                     ▼              ▼   ▼  │
                        │  ┌───────────────────────────────────────┐ │  │
                        │  │  poolmanager.Manager (Leader-Only)    │ │  │
                        │  │  ├── watchPods()                     │ │  │
                        │  │  ├── syncAllPods() (reconciler)      │ │  │
                        │  │  └── runZombieCleanup()              │ │  │
                        │  └───────────────────────────────────────┘ │  │
                        │                                            │  │
                        │  ┌────────────────────────────────────┐    │  │
                        │  │  HTTP Router (all replicas)        │    │  │
                        │  │  ├── allocator  ◄─── redis + cfg   │    │  │
                        │  │  ├── releaser   ◄─── redis + cfg   │    │  │
                        │  │  ├── drainer    ◄─── redis + cfg   │    │  │
                        │  │  └── handlers   ◄─── redis + cfg   │    │  │
                        │  └────────────────────────────────────┘    │  │
                        │                                            │  │
                        │  ┌────────────┐  ┌─────────────────────┐   │  │
                        │  │ HTTP :8080 │  │ Metrics :9090       │   │  │
                        │  └────────────┘  │ /metrics, /health   │   │  │
                        │                  └─────────────────────┘   │  │
                        └──────────────────────────────────────────────┘
```

## Key Design Decisions

1. **Fatal vs Warn on startup failures:** Config and Redis failures are fatal (can't serve traffic). K8s client failure is a warning (allows local dev).

2. **Nil-safe leader checker:** The `api.LeaderChecker` interface is nil-checked at the router level, so the entire API works without leader election.

3. **Separate metrics server:** Prometheus scrapes are isolated from business traffic on a different port, preventing metrics fetches from affecting API latency.

4. **Root context propagation:** All goroutines share one root context. A single `cancel()` call cleanly stops everything — no orphaned goroutines.

## Redis Keys Touched

**At bootstrap time, main.go touches one Redis key:**

| Key | Operation | Purpose |
|-----|-----------|---------|
| `voice:tier:config` | SETNX (Step 3b) | Seeds tier config on first deploy |
| `voice:tier:config` | GET (Step 3b, if key existed) | Loads existing config from Redis |

After startup, the tier config refresh goroutine (Step 3c) reads `voice:tier:config` every 30 seconds.

All other key creation happens later:
- Pool Manager writes keys when it discovers pods (after leader election)
- Allocator/Releaser/Drainer write keys when handling API requests

## Interaction with Other Flows

| Flow | Relationship |
|------|-------------|
| [02 - Configuration](./02-configuration.md) | `config.Load()` is called first, everything depends on it |
| [03 - Leader Election](./03-leader-election.md) | `poolManager.Run(ctx)` starts leader election |
| [12 - HTTP Layer](./12-http-layer.md) | `api.NewRouter()` creates the full Chi router |
| [13 - Metrics](./13-metrics.md) | Metrics server started on separate port |
| [15 - Tier Config Refresh](./15-tier-config-refresh.md) | Bootstrap + background refresh started in Steps 3b/3c |
