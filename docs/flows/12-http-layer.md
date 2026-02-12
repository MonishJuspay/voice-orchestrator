# Flow 12: HTTP Layer & Provider Endpoints

**Source files:**
- `internal/api/router.go` (90 lines) — Chi router, middleware stack, route definitions
- `internal/api/handlers/allocate.go` (331 lines) — 4 allocation endpoints
- `internal/api/handlers/release.go` (69 lines) — Release endpoint
- `internal/api/handlers/drain.go` (74 lines) — Drain endpoint
- `internal/api/handlers/status.go` (114 lines) — Status dashboard endpoint
- `internal/api/handlers/health.go` (56 lines) — Liveness + readiness probes
- `internal/api/handlers/pod_info.go` (86 lines) — Per-pod info endpoint

## Overview

The HTTP layer is the external interface of the smart router. It exposes a RESTful API on port 8080 that handles pod allocation, release, drain, status, health checks, and pod info queries. All endpoints are under the `/api/v1/` prefix and are served by all smart-router replicas (stateless, no leader requirement).

The router uses [Chi](https://github.com/go-chi/chi) for HTTP routing with a middleware stack for panic recovery, request IDs, logging, Prometheus metrics, and request timeouts.

## Middleware Stack (router.go:44-49)

Requests pass through middleware in this order:

```
Request → Recovery → RequestID → RealIP → Logger → Metrics → Timeout(60s) → Handler
```

| # | Middleware | Source | Purpose |
|---|-----------|--------|---------|
| 1 | `Recovery` | `middleware/recovery.go` | Catches panics, logs stack trace, returns 500, increments `panics_recovered_total` |
| 2 | `RequestID` | Chi built-in | Adds `X-Request-Id` header (generated or passed through) |
| 3 | `RealIP` | Chi built-in | Extracts real client IP from `X-Forwarded-For` / `X-Real-IP` |
| 4 | `Logger` | `middleware/logging.go` | Logs method, path, status, duration. Health/ready probes logged at DEBUG level to reduce noise |
| 5 | `Metrics` | `middleware/metrics.go` | Records `http_request_duration_seconds` histogram + `http_requests_total` counter with labels: method, endpoint (Chi route pattern), status |
| 6 | `Timeout(60s)` | Chi built-in | Cancels request context after 60 seconds |

---

## Route Table

All routes are under `/api/v1`:

| Method | Path | Handler | Purpose | Detailed Doc |
|--------|------|---------|---------|-------------|
| POST | `/allocate` | `allocateHandler.Handle` | Generic JSON allocation | [07 - Allocation](./07-allocation.md) |
| POST | `/twilio/allocate` | `allocateHandler.HandleTwilio` | Twilio webhook (form data → TwiML) | [07 - Allocation](./07-allocation.md) |
| POST | `/plivo/allocate` | `allocateHandler.HandlePlivo` | Plivo webhook (form data → Plivo XML) | [07 - Allocation](./07-allocation.md) |
| POST | `/exotel/allocate` | `allocateHandler.HandleExotel` | Exotel webhook (JSON → JSON) | [07 - Allocation](./07-allocation.md) |
| POST | `/release` | `releaseHandler.Handle` | Release pod back to pool | [08 - Release](./08-release.md) |
| POST | `/drain` | `drainHandler.Handle` | Graceful pod drain | [09 - Drain](./09-drain.md) |
| GET | `/status` | `statusHandler.Handle` | System status dashboard | Below |
| GET | `/pod/{pod_name}` | `podInfoHandler.Handle` | Per-pod info | Below |
| GET | `/health` | `healthHandler.HandleHealth` | K8s liveness probe | Below |
| GET | `/ready` | `healthHandler.HandleReady` | K8s readiness probe | Below |
| GET | `/metrics` | `promhttp.Handler()` | Prometheus scrape endpoint | [13 - Metrics](./13-metrics.md) |

---

## Provider-Specific Allocation Endpoints

The allocator has one core `Allocate()` method, but four HTTP endpoints to handle different telephony providers' webhook formats. See [07 - Allocation](./07-allocation.md) for the complete allocation flow. Here's a summary of the differences:

### `POST /api/v1/allocate` — Generic (allocate.go:35-95)

```
Input:  JSON body {call_sid, merchant_id, provider, flow, template}
Output: JSON {success, pod_name, ws_url, source_pool, was_existing}
Error:  JSON {error: "..."}
```

### `POST /api/v1/twilio/allocate` — Twilio (allocate.go:108-160)

```
Input:  Form data (CallSid) + query params (?merchant_id=&flow=&template=)
Output: TwiML XML <Response><Connect><Stream url="wss://..."/></Connect></Response>
Error:  TwiML XML <Response><Say>error message</Say><Hangup/></Response>
```

Twilio calls this webhook when an incoming call matches a TwiML Bin or TwiML App. The XML response tells Twilio to open a WebSocket stream to the voice-agent pod.

### `POST /api/v1/plivo/allocate` — Plivo (allocate.go:176-231)

```
Input:  Form data (CallUUID) + query params (?merchant_id=&flow=&template=)
Output: Plivo XML <Response><Stream bidirectional="true" ...>wss://...</Stream></Response>
Error:  Plivo XML <Response><Speak>error message</Speak><Hangup/></Response>
```

Plivo uses `CallUUID` instead of `CallSid`. The XML response format includes attributes for bidirectional streaming and audio codec.

### `POST /api/v1/exotel/allocate` — Exotel (allocate.go:247-292)

```
Input:  JSON body {CallSid, merchant_id, flow, template}
Output: JSON {url: "wss://..."}
Error:  JSON {error: "..."}
```

Exotel uses JSON for both request and response. The response is a simpler format with just the WebSocket URL.

---

## Status Endpoint

### `GET /api/v1/status` (status.go:40-113)

Returns a comprehensive system overview. Useful for debugging and monitoring.

**Response:**
```json
{
    "pools": {
        "gold": {
            "type": "exclusive",
            "assigned": 1,
            "available": 1
        },
        "standard": {
            "type": "exclusive",
            "assigned": 1,
            "available": 0
        },
        "basic": {
            "type": "shared",
            "assigned": 1,
            "available": 1
        }
    },
    "active_calls": 1,
    "is_leader": true,
    "status": "up"
}
```

**How it works:**

1. **Redis PING** — checks connectivity, sets `status` to `"up"` or `"down"`
2. **Active calls** — `SCAN voice:lease:*` with count 100, counts matches. This is a real-time scan, not using the Prometheus gauge.
3. **Pool info** — for each tier in config:
   - `SCARD voice:pool:{tier}:assigned` → assigned count
   - `SCARD voice:pool:{tier}:available` (exclusive) or `ZCARD` (shared) → available count
4. **Leader status** — calls `IsLeader()` on the leader checker interface

**Redis keys READ:** `voice:pool:{tier}:assigned`, `voice:pool:{tier}:available`, `voice:lease:*` (SCAN)

---

## Pod Info Endpoint

### `GET /api/v1/pod/{pod_name}` (pod_info.go:40-85)

Returns current state of a specific pod. Useful for debugging.

**Response:**
```json
{
    "pod_name": "voice-agent-0",
    "tier": "gold",
    "is_draining": false,
    "has_active_lease": true,
    "lease_call_sid": "CA123..."
}
```

**How it works:**

1. `GET voice:pod:tier:{pod_name}` → tier (404 if not found)
2. `EXISTS voice:pod:draining:{pod_name}` → draining status
3. `GET voice:lease:{pod_name}` → active lease + call SID

**Redis keys READ:** `voice:pod:tier:{pod}`, `voice:pod:draining:{pod}`, `voice:lease:{pod}`

---

## Health & Readiness Probes

### `GET /api/v1/health` — Liveness (health.go:33-38)

```json
{"status": "ok"}
```

**Always returns 200.** The liveness probe should NOT depend on external services. If Redis is down but the Go process is alive, K8s should NOT restart the pod — that would just cause cascading restarts.

### `GET /api/v1/ready` — Readiness (health.go:42-55)

```json
{"status": "ready"}
```

**Returns 200 only if Redis is reachable** (PING). If Redis is down, returns 503. This removes the pod from K8s service endpoints — no traffic is routed to a replica that can't talk to Redis.

```
Readiness check:
    Redis PING → OK → 200 {"status": "ready"}
    Redis PING → error → 503 {"error": "service unavailable"}
```

---

## Error Response Formats

### JSON Error (used by generic, exotel, and non-allocation endpoints)
```json
{"error": "error message here"}
```

### TwiML Error (used by Twilio endpoint)
```xml
<?xml version="1.0" encoding="UTF-8"?>
<Response>
    <Say>allocation failed</Say>
    <Hangup/>
</Response>
```

Twilio will speak the error message to the caller and hang up.

### Plivo XML Error (used by Plivo endpoint)
```xml
<?xml version="1.0" encoding="UTF-8"?>
<Response>
    <Speak>allocation failed</Speak>
    <Hangup/>
</Response>
```

Same concept as TwiML but uses Plivo's `<Speak>` element.

---

## Request Flow Diagram

```
External Request
       │
       ▼
  ┌─────────┐
  │  Nginx   │  (port 8080, domain: clairvoyance.breezelabs.app)
  └────┬─────┘
       │ /api/v1/* → proxy_pass to smart-router:8080
       ▼
  ┌──────────────────────────────────────────────────┐
  │  Chi Router (smart-router, any of 3 replicas)    │
  │                                                   │
  │  Middleware: Recovery → RequestID → RealIP        │
  │              → Logger → Metrics → Timeout(60s)    │
  │                                                   │
  │  Route Match:                                     │
  │    /api/v1/allocate         → AllocateHandler     │
  │    /api/v1/twilio/allocate  → HandleTwilio        │
  │    /api/v1/plivo/allocate   → HandlePlivo         │
  │    /api/v1/exotel/allocate  → HandleExotel        │
  │    /api/v1/release          → ReleaseHandler      │
  │    /api/v1/drain            → DrainHandler        │
  │    /api/v1/status           → StatusHandler       │
  │    /api/v1/pod/{name}       → PodInfoHandler      │
  │    /api/v1/health           → HealthHandler       │
  │    /api/v1/ready            → ReadyHandler        │
  │    /api/v1/metrics          → Prometheus           │
  └──────────────────────────────────────────────────┘
```

---

## Interaction with Other Flows

| Flow | Relationship |
|------|-------------|
| [07 - Allocation](./07-allocation.md) | 4 allocation handlers call `Allocate()` |
| [08 - Release](./08-release.md) | Release handler calls `Release()` |
| [09 - Drain](./09-drain.md) | Drain handler calls `Drain()` |
| [13 - Metrics](./13-metrics.md) | Metrics middleware + `/metrics` endpoint |
| [14 - Nginx Routing](./14-nginx-routing.md) | Nginx proxies `/api/v1/*` to smart-router |
