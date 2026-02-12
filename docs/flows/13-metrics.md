# Flow 13 — Metrics & Observability

## Overview

The Smart Router exposes **11 Prometheus metrics** covering HTTP request telemetry, business operations (allocations, releases, drains), infrastructure state (pool sizes, leader status), and reliability (panics, zombies). Three middleware layers — **Recovery**, **Logger**, and **Metrics** — wrap every HTTP request to provide structured logging, panic safety, and automatic latency/count instrumentation.

All metrics are registered via `promauto` (auto-registration with the default Prometheus registry) and scraped at `GET /api/v1/metrics` via `promhttp.Handler`.

---

## Source Files

| File | Lines | Purpose |
|------|-------|---------|
| `internal/api/middleware/metrics.go` | 144 | Metric definitions + HTTP metrics middleware |
| `internal/api/middleware/logging.go` | 62 | Structured request logging middleware |
| `internal/api/middleware/recovery.go` | 42 | Panic recovery middleware |
| `internal/api/router.go` | 90 | Middleware stack ordering + route definitions |

---

## Middleware Stack (Execution Order)

Defined in `router.go:44-49`:

```
Request ──► Recovery ──► RequestID ──► RealIP ──► Logger ──► Metrics ──► Timeout(60s) ──► Handler
```

| # | Middleware | Source | Purpose |
|---|-----------|--------|---------|
| 1 | `middleware.Recovery` | `recovery.go` | Outermost — catches panics from everything below |
| 2 | `chimiddleware.RequestID` | chi built-in | Adds `X-Request-Id` header |
| 3 | `chimiddleware.RealIP` | chi built-in | Extracts real client IP from `X-Forwarded-For` / `X-Real-IP` |
| 4 | `middleware.Logger` | `logging.go` | Structured request logging |
| 5 | `middleware.Metrics` | `metrics.go` | Prometheus HTTP request duration + count |
| 6 | `chimiddleware.Timeout` | chi built-in | 60-second request deadline |

**Why Recovery is outermost:** If any middleware or handler panics, Recovery catches it before the goroutine crashes. It sits at position 1 so it wraps everything.

---

## All 11 Prometheus Metrics

### HTTP Metrics (auto-recorded by middleware)

#### 1. `http_request_duration_seconds` (Histogram)

```
metrics.go:16-23
```

| Property | Value |
|----------|-------|
| Type | Histogram |
| Labels | `method`, `endpoint`, `status` |
| Buckets | `prometheus.DefBuckets` (0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10) |
| Updated by | `Metrics` middleware — every HTTP request |

**Label details:**
- `method`: HTTP method (`GET`, `POST`)
- `endpoint`: Chi route pattern (e.g. `/api/v1/allocate`), NOT the raw URL — avoids cardinality explosion from dynamic path segments like pod names
- `status`: HTTP status code as string (`200`, `404`, `500`)

#### 2. `http_requests_total` (Counter)

```
metrics.go:25-31
```

| Property | Value |
|----------|-------|
| Type | Counter |
| Labels | `method`, `endpoint`, `status` |
| Updated by | `Metrics` middleware — every HTTP request |

Same labels as `http_request_duration_seconds`. Together these two metrics give you request rate, error rate, and latency percentiles per endpoint.

---

### Business Metrics (updated by application code)

#### 3. `smart_router_allocations_total` (Counter)

```
metrics.go:34-40
```

| Property | Value |
|----------|-------|
| Type | Counter |
| Labels | `pool`, `status` |
| Exported as | `middleware.AllocationsTotal` |

**Update sites (3):**

| File | Line | Label Values | When |
|------|------|-------------|------|
| `allocator.go` | 105 | `pool=""`, `status="no_pods"` | All tiers exhausted in fallback chain |
| `allocator.go` | 117 | `pool={sourcePool}`, `status="storage_error"` | `storeAllocation()` fails (Redis write error) |
| `allocator.go` | 136 | `pool={sourcePool}`, `status="success"` | Allocation completed successfully |

#### 4. `smart_router_releases_total` (Counter)

```
metrics.go:42-48
```

| Property | Value |
|----------|-------|
| Type | Counter |
| Labels | `pool`, `status` |
| Exported as | `middleware.ReleasesTotal` |

**Update sites (1):**

| File | Line | Label Values | When |
|------|------|-------------|------|
| `releaser.go` | 157 | `pool={sourcePool}`, `status="success"` | Release completed successfully |

#### 5. `smart_router_active_calls` (Gauge)

```
metrics.go:50-55
```

| Property | Value |
|----------|-------|
| Type | Gauge |
| Labels | none |
| Exported as | `middleware.ActiveCalls` |

**Update sites (5):**

| File | Line | Operation | When |
|------|------|-----------|------|
| `allocator.go` | 137 | `.Inc()` | Successful allocation |
| `releaser.go` | 158 | `.Dec()` | Successful release |
| `reconciler.go` | 205 | `.Dec()` | Pod removed from pool while it had an active call (orphaned call cleanup) |
| `zombie.go` | 132 | `.Dec()` | Exclusive zombie recovered (orphaned call cleaned up) |
| `zombie.go` | 211 | `.Dec()` | Merchant zombie recovered (orphaned call cleaned up) |

**Note:** Shared pool zombie recovery does NOT decrement ActiveCalls — the call may still be active on the shared pod.

#### 6. `smart_router_pool_available_pods` (GaugeVec)

```
metrics.go:57-63
```

| Property | Value |
|----------|-------|
| Type | Gauge |
| Labels | `tier` |
| Exported as | `middleware.PoolAvailablePods` |

**Update sites (1):**

| File | Line | Operation | When |
|------|------|-----------|------|
| `zombie.go` | 250 | `.Set(float64(available))` | `updatePoolMetrics()` — runs every zombie cleanup cycle (30s) |

Reads `SCARD` (exclusive) or `ZCARD` (shared) of the available pool for each configured tier.

#### 7. `smart_router_pool_assigned_pods` (GaugeVec)

```
metrics.go:65-71
```

| Property | Value |
|----------|-------|
| Type | Gauge |
| Labels | `tier` |
| Exported as | `middleware.PoolAssignedPods` |

**Update sites (1):**

| File | Line | Operation | When |
|------|------|-----------|------|
| `zombie.go` | 236 | `.Set(float64(assigned))` | `updatePoolMetrics()` — runs every zombie cleanup cycle (30s) |

Reads `SCARD` of the assigned SET for each configured tier.

---

### Infrastructure Metrics

#### 8. `smart_router_leader_status` (Gauge)

```
metrics.go:73-78
```

| Property | Value |
|----------|-------|
| Type | Gauge (0 or 1) |
| Labels | none |
| Exported as | `middleware.LeaderStatus` |

**Update sites (3):**

| File | Line | Value | When |
|------|------|-------|------|
| `manager.go` | 57 | `1` | Won leader election for the first time |
| `manager.go` | 88 | `1` | Re-won leader election after losing it |
| `manager.go` | 96 | `0` | Lost leader election |

Only 1 of the 3 smart-router replicas reports `1` at any time. Useful for alerting on leader flapping.

#### 9. `smart_router_panics_recovered_total` (Counter)

```
metrics.go:80-85
```

| Property | Value |
|----------|-------|
| Type | Counter |
| Labels | none |
| Exported as | `middleware.PanicsRecoveredTotal` |

**Update sites (1):**

| File | Line | When |
|------|------|------|
| `recovery.go` | 29 | Any HTTP handler panics — Recovery middleware catches it |

Any non-zero value here is a bug in the application code. Should be alerted on.

#### 10. `smart_router_drains_total` (Counter)

```
metrics.go:87-92
```

| Property | Value |
|----------|-------|
| Type | Counter |
| Labels | none |
| Exported as | `middleware.DrainsTotal` |

**Update sites (1):**

| File | Line | When |
|------|------|------|
| `drain.go` | 71 | Drain handler — after successful drain operation |

Counts total drain requests. During rolling updates, each voice-agent pod sends a drain request via its `preStop` hook, so this counter reflects pod terminations.

#### 11. `smart_router_zombies_recovered_total` (Counter)

```
metrics.go:94-99
```

| Property | Value |
|----------|-------|
| Type | Counter |
| Labels | none |
| Exported as | `middleware.ZombiesRecoveredTotal` |

**Update sites (3):**

| File | Line | When |
|------|------|------|
| `zombie.go` | 131 | Exclusive pool zombie recovered (pod in assigned SET but no lease/not draining) |
| `zombie.go` | 175 | Shared pool zombie recovered (pod in ZSET but not running in K8s) |
| `zombie.go` | 210 | Merchant pool zombie recovered (pod in merchant assigned SET but no lease/not draining) |

A steady non-zero rate here indicates pods are crashing or being killed without clean release. Occasional spikes during rolling updates are normal.

---

## Metrics Middleware — How It Works

```
metrics.go:103-131
```

### Step-by-Step

1. **Wrap ResponseWriter** (line 108): Creates a `metricsResponseWriter` that intercepts `WriteHeader()` to capture the status code. Default status = 200 (Go's default when `WriteHeader` isn't called explicitly).

2. **Serve the request** (line 110): Calls `next.ServeHTTP()` with the wrapped writer.

3. **Calculate duration** (line 112): `time.Since(start)`.

4. **Resolve endpoint label** (lines 116-126):
   ```go
   endpoint := r.URL.Path
   if rctx := chi.RouteContext(r.Context()); rctx != nil {
       if pattern := rctx.RoutePattern(); pattern != "" {
           endpoint = pattern
       }
   }
   endpoint = strings.TrimRight(endpoint, "/")
   ```
   Uses Chi's **route pattern** (e.g. `/api/v1/pod/{pod_name}`) instead of the raw URL (e.g. `/api/v1/pod/voice-agent-2`). This prevents cardinality explosion — without this, every unique pod name would create a new time series.

5. **Record** (lines 129-130): Observes histogram + increments counter with `{method, endpoint, status}` labels.

---

## Logger Middleware — How It Works

```
logging.go:11-49
```

### Step-by-Step

1. **Nil safety** (line 12-14): If nil logger passed, uses `zap.NewNop()` (no-op logger).

2. **Wrap ResponseWriter** (line 21): Same pattern as metrics — captures status code.

3. **Serve the request** (line 23).

4. **Health/Ready skip** (lines 28-37): If path is `/api/v1/health` or `/api/v1/ready`, logs at **DEBUG** level only. These endpoints are hit every few seconds by K8s probes — logging them at INFO would flood the logs.

5. **All other requests** (lines 40-47): Logs at **INFO** level with fields:
   - `method`, `path`, `remote_addr`, `user_agent`, `status`, `duration`

---

## Recovery Middleware — How It Works

```
recovery.go:11-41
```

### Step-by-Step

1. **Nil safety** (line 12-14): Same pattern as Logger.

2. **Defer recover** (lines 18-36): Standard Go panic recovery pattern.

3. **On panic** (lines 21-34):
   - Logs at **ERROR** level with: `error` (panic value), `method`, `path`, `stack` (full goroutine stack trace via `debug.Stack()`)
   - Increments `PanicsRecoveredTotal` counter
   - Sets `Content-Type: application/json`
   - Returns HTTP 500 with `{"error":"internal server error"}`

4. **Normal flow** (line 38): If no panic, simply calls `next.ServeHTTP()`.

---

## Scrape Endpoint

```
router.go:84
```

```
GET /api/v1/metrics → promhttp.Handler()
```

Serves the default Prometheus registry (all `promauto`-registered metrics) in Prometheus exposition format. This is what Prometheus/Victoria/Grafana scrapes.

**Access:** Available on all 3 smart-router replicas (port 8080), not just the leader. Each replica reports its own `http_*` metrics and `leader_status`, but only the leader reports meaningful `pool_*` metrics (since only the leader runs zombie cleanup which calls `updatePoolMetrics()`).

---

## Metric Flow Diagram

```
                          ┌─────────────────────────────────────┐
                          │         HTTP Request Arrives         │
                          └──────────────┬──────────────────────┘
                                         │
                          ┌──────────────▼──────────────────────┐
                          │     Recovery Middleware (outermost)   │
                          │  On panic: PanicsRecoveredTotal.Inc() │
                          └──────────────┬──────────────────────┘
                                         │
                          ┌──────────────▼──────────────────────┐
                          │         Logger Middleware             │
                          │  health/ready → DEBUG, else → INFO   │
                          └──────────────┬──────────────────────┘
                                         │
                          ┌──────────────▼──────────────────────┐
                          │         Metrics Middleware            │
                          │  request_duration.Observe()           │
                          │  request_count.Inc()                  │
                          └──────────────┬──────────────────────┘
                                         │
                    ┌────────────────────┬┴───────────────────────┐
                    │                    │                         │
             ┌──────▼──────┐    ┌───────▼───────┐    ┌───────────▼────────┐
             │  /allocate   │    │   /release    │    │     /drain         │
             │              │    │               │    │                    │
             │ AllocTotal++ │    │ ReleaseTotal++│    │ DrainsTotal++      │
             │ ActiveCalls++│    │ ActiveCalls-- │    │                    │
             └──────────────┘    └───────────────┘    └────────────────────┘

        ┌─────────────────────────────────────────────────────────┐
        │              Background (Leader-Only)                    │
        │                                                         │
        │  ┌──────────────────┐  ┌──────────────────────────────┐ │
        │  │  Pool Manager     │  │  Zombie Cleanup (every 30s)  │ │
        │  │  LeaderStatus=1   │  │  ZombiesRecoveredTotal++     │ │
        │  │  (or 0 on loss)   │  │  ActiveCalls-- (if orphaned) │ │
        │  └──────────────────┘  │  PoolAvailablePods.Set()      │ │
        │                        │  PoolAssignedPods.Set()        │ │
        │  ┌──────────────────┐  └──────────────────────────────┘ │
        │  │  Reconciler       │                                   │
        │  │  ActiveCalls--    │                                   │
        │  │  (pod death +     │                                   │
        │  │   orphaned call)  │                                   │
        │  └──────────────────┘                                    │
        └─────────────────────────────────────────────────────────┘
```

---

## Alerting Recommendations

| Metric | Alert Condition | Meaning |
|--------|----------------|---------|
| `smart_router_panics_recovered_total` | Any increment | Bug in application code |
| `smart_router_active_calls` | Sustained mismatch with actual calls | ActiveCalls gauge drifted — zombie cleanup should self-heal |
| `smart_router_leader_status` | No replica reports `1` for >30s | Leader election stuck — all background jobs halted |
| `smart_router_leader_status` | Multiple replicas report `1` | Split-brain (shouldn't happen with K8s Lease) |
| `smart_router_zombies_recovered_total` | High sustained rate outside rolling updates | Pods crashing repeatedly |
| `smart_router_pool_available_pods` | 0 for all tiers | No capacity — calls will fail |
| `http_request_duration_seconds` (p99) | >5s on `/api/v1/allocate` | Redis latency or pod exhaustion causing retries |

---

## Interaction with Other Flows

| Flow | Metrics Updated | Doc |
|------|----------------|-----|
| Allocation | `allocations_total`, `active_calls`, `http_*` | [07-allocation.md](07-allocation.md) |
| Release | `releases_total`, `active_calls`, `http_*` | [08-release.md](08-release.md) |
| Drain | `drains_total`, `http_*` | [09-drain.md](09-drain.md) |
| Zombie Cleanup | `zombies_recovered_total`, `active_calls`, `pool_available_pods`, `pool_assigned_pods` | [10-zombie-cleanup.md](10-zombie-cleanup.md) |
| Leader Election | `leader_status` | [03-leader-election.md](03-leader-election.md) |
| Reconciler | `active_calls` | [05-reconciler.md](05-reconciler.md) |
| Bootstrap | All metrics registered at import time via `promauto` | [01-bootstrap.md](01-bootstrap.md) |
