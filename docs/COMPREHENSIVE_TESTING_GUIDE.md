# Smart Router - Comprehensive E2E Testing & Architecture Guide

> **Repository**: orchestration-api-go (Smart Router) + orchestration-clair (Voice Agent)
> **Cluster**: gke_breeze-automatic-prod_asia-south1_breeze-automatic-mum-01
> **Namespace**: voice-system
> **Last Updated**: February 8, 2026
> **Purpose**: Deep reference doc for verifying Smart Router production readiness. Written so that another engineer or AI agent can independently validate every component.

---

## Table of Contents

1. [Architecture Overview](#1-architecture-overview)
2. [Infrastructure & Current State](#2-infrastructure--current-state)
3. [Prerequisites & Access](#3-prerequisites--access)
4. [Call Lifecycle (End-to-End)](#4-call-lifecycle-end-to-end)
5. [Tier System & Pool Management](#5-tier-system--pool-management)
6. [API Contract Reference](#6-api-contract-reference)
7. [Feature Flags & Rollout Controls](#7-feature-flags--rollout-controls)
8. [Redis Key Reference](#8-redis-key-reference)
9. [Test Scenarios](#9-test-scenarios)
10. [Redis Inspection Commands](#10-redis-inspection-commands)
11. [Build & Deploy Reference](#11-build--deploy-reference)
12. [Bug Fixes Applied (Session Record)](#12-bug-fixes-applied-session-record)
13. [Known Gaps & Risks](#13-known-gaps--risks)

---

## 1. Architecture Overview

### System Diagram

```
                          INTERNET
                             |
                    [GCP HTTPS Load Balancer]
                    clairvoyance.breezelabs.app
                    buddy.breezelabs.app
                             |
                    [nginx-router] (2 replicas)
                     /       |       \           \
                    /        |        \           \
        /api/v1/*    /twilio/*   /plivo/*    /ws/pod/{name}/{sid}
                    |        |        |              |
                [smart-router] (3 replicas)    [voice-agent-{N}]
                    Go, Chi router               via headless svc
                    Leader election              DNS: {name}.voice-agent:8000
                         |
                    [Redis 10.100.0.4:6379]
                    External, single instance
                    All pool state lives here
```

### Component Roles

| Component | Replicas | Type | Purpose |
|-----------|----------|------|---------|
| **nginx-router** | 2 | Deployment | L7 routing: API requests to smart-router, WebSocket connections to specific voice-agent pods |
| **smart-router** | 3 | Deployment | Pod allocation/release, pool management, tier routing, K8s watcher, leader election |
| **voice-agent** | 5 (0-4) | StatefulSet | Voice call handling (STT, TTS, LLM), WebSocket audio streaming |
| **Redis** | 1 | External | Pool state, call records, leases, pod info, draining flags |

### Key Design Decisions

- **1-pod-1-call for exclusive tiers**: Gold and standard pods handle exactly one call at a time.
- **Shared tier (basic)**: Basic pods can handle up to `max_concurrent` calls simultaneously (currently 3).
- **Fallback chain**: If a merchant's preferred tier is exhausted, allocation falls through: Dedicated -> Gold -> Standard -> Overflow -> Basic.
- **Triple-redundant release**: The voice agent releases pods via 3 independent mechanisms (WebSocket close, call completion handler, status callback handler). All are idempotent.
- **Smart Router client has circuit breaker**: If Smart Router fails repeatedly, the voice agent's client opens the circuit and falls back to normal (non-isolated) operation.

---

## 2. Infrastructure & Current State

### Cluster Access

```bash
# Set context
kubectl config use-context gke_breeze-automatic-prod_asia-south1_breeze-automatic-mum-01
kubectl config set-context --current --namespace=voice-system
```

### Current Pods (as of last deploy)

```
NAME                            READY   STATUS    RESTARTS
nginx-router-6f54f6777-9hnpg    1/1     Running   0
nginx-router-6f54f6777-dw7wj    1/1     Running   0
smart-router-65f6d94994-97f5w   1/1     Running   0
smart-router-65f6d94994-lqjvw   1/1     Running   0
smart-router-65f6d94994-rzp9p   1/1     Running   0
voice-agent-0                   1/1     Running   0
voice-agent-1                   1/1     Running   0
voice-agent-2                   1/1     Running   0
voice-agent-3                   1/1     Running   0
voice-agent-4                   1/1     Running   0
```

### Services

| Service | Type | ClusterIP | Ports | Purpose |
|---------|------|-----------|-------|---------|
| nginx-router | ClusterIP | 34.118.225.185 | 80/TCP | Ingress router |
| smart-router | ClusterIP | 34.118.227.34 | 8080/TCP, 9090/TCP | Smart Router API + metrics |
| voice-agent | Headless | None | 8000/TCP | Pod-addressable via `{pod}.voice-agent:8000` |

### External Endpoints

| Endpoint | URL | Method |
|----------|-----|--------|
| Health Check | https://clairvoyance.breezelabs.app/health | GET |
| Ready Check | https://clairvoyance.breezelabs.app/ready | GET |
| Allocate Pod | https://clairvoyance.breezelabs.app/api/v1/allocate | POST |
| Release Pod | https://clairvoyance.breezelabs.app/api/v1/release | POST |
| Pool Status | https://clairvoyance.breezelabs.app/api/v1/status | GET |
| Drain Pod | https://clairvoyance.breezelabs.app/api/v1/drain | POST |
| Pod Info | https://clairvoyance.breezelabs.app/api/v1/pod/{name} | GET |
| Twilio Webhook | https://clairvoyance.breezelabs.app/agent/voice/breeze-buddy/twilio/callback/ | POST |
| Plivo Webhook | https://clairvoyance.breezelabs.app/agent/voice/breeze-buddy/plivo/callback/ | POST |
| Exotel Webhook | https://clairvoyance.breezelabs.app/agent/voice/breeze-buddy/exotel/callback/ | POST |
| WebSocket | wss://buddy.breezelabs.app/ws/pod/{pod_name}/{call_sid} | WebSocket |
| Metrics | http://smart-router:9090/metrics (cluster-internal) | GET |

### ConfigMaps

**smart-router-config**:
```yaml
HTTP_PORT: "8080"
LEADER_ELECTION_ENABLED: "true"
LOG_LEVEL: info
METRICS_PORT: "9090"
NAMESPACE: voice-system
POD_LABEL_SELECTOR: app=voice-agent
TIER_CONFIG: '{"gold":{"type":"exclusive","target":1},"standard":{"type":"exclusive","target":1},"basic":{"type":"shared","target":1,"max_concurrent":3}}'
```

**voice-agent-config** (relevant fields):
```yaml
ENABLE_VOICE_AGENT_POD_ISOLATION: "true"
REDIS_PREFIX: "voice:"
POD_NAMESPACE: voice-system
# SMART_ROUTER_BASE_URL not set — defaults to http://smart-router:8080 (correct for in-cluster)
```

### Secrets

**smart-router-secrets**:
```
REDIS_URL: redis://10.100.0.4:6379
VOICE_AGENT_BASE_URL: wss://buddy.breezelabs.app
```

**voice-agent deployment** (from downward API):
```
POD_NAME: from metadata.name (e.g., "voice-agent-0")
POD_IP: from status.podIP
```

---

## 3. Prerequisites & Access

### Required Tools

| Tool | Purpose | Notes |
|------|---------|-------|
| `kubectl` | Cluster access | Context must be set to breeze-automatic-mum-01 |
| `gcloud` | Auth for GAR pushes | `gcloud auth print-access-token` for Docker login |
| `podman` (or `docker`) | Container builds | Go is NOT installed locally — builds happen inside Docker |
| `curl` | API testing | All endpoints accessible via clairvoyance.breezelabs.app |
| `redis-cli` | Redis inspection | Use via `kubectl exec` into any smart-router pod, or ephemeral pod |

### Go is NOT Installed Locally

The Smart Router is built inside Docker using a multi-stage Dockerfile. You do NOT need Go installed on your local machine. All compilation happens in the container.

### Redis Access Pattern

Redis is external at `10.100.0.4:6379`. Access it from within the cluster:

```bash
# Option 1: Exec into a smart-router pod
kubectl exec -it $(kubectl get pods -n voice-system -l app=smart-router -o jsonpath='{.items[0].metadata.name}') -n voice-system -- sh

# Then use wget for HTTP endpoints (smart-router container is distroless, no redis-cli)
wget -qO- http://localhost:8080/api/v1/status

# Option 2: Ephemeral redis-cli pod
kubectl run redis-cli -n voice-system --rm -it --restart=Never --image=redis:7-alpine \
  -- redis-cli -h 10.100.0.4
```

---

## 4. Call Lifecycle (End-to-End)

This describes how a real call flows through the system. Understanding this is critical for testing.

### Phase 1: Call Initiation (Voice Agent -> Telephony Provider)

1. Voice agent's `calls.py` initiates an outbound call via `provider.make_call()`.
2. The webhook URL is set to point to the nginx-router:
   - Twilio: `https://clairvoyance.breezelabs.app/agent/voice/breeze-buddy/twilio/callback/{template}?merchant_id={id}`
   - Plivo: `https://clairvoyance.breezelabs.app/agent/voice/breeze-buddy/plivo/callback/{template}?merchant_id={id}`
   - Exotel: `https://clairvoyance.breezelabs.app/agent/voice/breeze-buddy/exotel/callback/{template}?merchant_id={id}`
3. **No pod is pre-allocated at this stage.** Allocation happens when the customer answers.

### Phase 2: Customer Answers -> Pod Allocation

1. Telephony provider calls the webhook URL.
2. nginx-router proxies to the appropriate smart-router endpoint:
   - `/agent/voice/breeze-buddy/twilio/callback/` -> `smart-router /api/v1/twilio/allocate`
   - `/agent/voice/breeze-buddy/plivo/callback/` -> `smart-router /api/v1/plivo/allocate`
   - `/agent/voice/breeze-buddy/exotel/callback/` -> `smart-router /api/v1/exotel/allocate`
3. Smart Router:
   a. Checks idempotency (has this call_sid been allocated before?)
   b. Looks up merchant config in Redis (`voice:merchant:config` hash, field = merchant_id)
   c. Runs fallback chain: Dedicated -> Gold -> Standard -> Overflow -> Basic
   d. Stores allocation in Redis (call info, pod info, lease)
   e. Returns provider-specific response with WebSocket URL

### Phase 3: Audio Streaming

1. Provider connects WebSocket to the URL returned in Phase 2:
   `wss://buddy.breezelabs.app/ws/pod/{pod_name}/{call_sid}`
2. nginx-router regex matches this URL pattern and proxies to:
   `http://{pod_name}.voice-agent:8000` (via K8s headless service DNS)
3. Voice agent on that specific pod handles the call (STT, LLM, TTS).

### Phase 4: Call Release (Triple Redundancy)

The voice agent releases the pod via Smart Router's `/api/v1/release` endpoint. Three independent mechanisms ensure release:

1. **WebSocket `finally` block**: When the WS connection closes (for any reason), `smart_router_client.release_pod(call_sid)` is called.
2. **Call completion handler**: `handle_call_completion()` in `calls.py` calls `release_pod(call_sid)` when the call ends.
3. **Status callback**: When the provider sends a status callback indicating call ended, `release_pod(call_sid)` is called.
4. **Unanswered call cleanup**: `handle_unanswered_calls()` releases pods that were allocated but never connected via WebSocket.

All release calls are **idempotent** — calling multiple times for the same call_sid is safe.

### Phase 2 (Alternative): Voice Agent's Own Smart Router Client

The voice agent also has its own Smart Router client (`smart_router_client.py`) that can allocate pods via `POST /api/v1/allocate` (JSON API). This path is used when:
- The voice agent's `allocate.py` handler (`/voice/allocate`) receives a request
- Feature flags allow Smart Router usage (see Section 7)

This is the path used when the voice agent orchestrates the allocation itself (as opposed to the provider webhook path above).

---

## 5. Tier System & Pool Management

### Current Tier Configuration

```json
{
  "gold":     {"type": "exclusive", "target": 1},
  "standard": {"type": "exclusive", "target": 1},
  "basic":    {"type": "shared",    "target": 1, "max_concurrent": 3}
}
```

- **gold** (exclusive, target 1): 1 pod reserved for gold-tier merchants. 1 call at a time.
- **standard** (exclusive, target 1): 1 pod reserved for standard-tier merchants. 1 call at a time.
- **basic** (shared, target 1): 1 pod that can handle up to 3 concurrent calls.
- **overflow**: Not in the config (target 0), but the code supports it as a fallback tier.

With 5 voice-agent pods (0-4) and targets summing to 3, the remaining 2 pods are assigned to basic or overflow by the tier assigner.

### How Pods Are Assigned to Tiers

The Smart Router's **Kubernetes Watcher** (runs on leader only) monitors the `app=voice-agent` pods. When pods appear, the **Tier Assigner** distributes them across tiers to meet targets:

1. Iterate tiers in order (gold -> standard -> basic)
2. For each tier, assign unassigned pods until `target` is reached
3. Remaining pods go to the last tier or overflow

### Fallback Chain (Allocation Order)

When a call needs a pod, the allocator tries pools in this order:

1. **Dedicated pool** (if merchant has `tier: "dedicated"` with a specific pool)
2. **Gold pool** (if merchant tier is gold or dedicated)
3. **Standard pool** (always tried)
4. **Overflow pool** (always tried)
5. **Basic shared pool** (last resort, shared across calls)

For a default merchant (no config), the chain is: Standard -> Overflow -> Basic.

### Pool Types

**Exclusive (SET-based)**:
- Redis key: `voice:pool:{tier}:available` (Redis SET)
- Allocation: `SPOP` (atomic random pop) + draining check
- If popped pod is draining, retry with another
- Release: `SADD` back to available set

**Shared (ZSET-based)**:
- Redis key: `voice:pool:basic:available` (Redis Sorted Set)
- Score = number of active calls on that pod
- Allocation: Lua script iterates members sorted by score ASC, picks first non-draining pod with score < max_concurrent, then `ZINCRBY +1`
- Release: Lua script `ZINCRBY -1` with floor at 0

### Pool Manager Background Tasks (Leader Only)

| Task | Interval | Purpose |
|------|----------|---------|
| K8s Watcher | Real-time (informer) | Detect pod add/delete/update events |
| Reconciler | 60s | Sync Redis state with K8s reality |
| Zombie Cleanup | 30s | Find pods with expired leases, return to available pool |
| Tier Assigner | On pod events | Assign new pods to tiers based on config targets |

---

## 6. API Contract Reference

### POST /api/v1/allocate

**Request** (JSON):
```json
{
  "call_sid": "CA123abc",        // Required
  "merchant_id": "merchant_xyz"  // Optional, defaults to "" -> standard tier
}
```

Note: The voice agent client also sends a `"provider"` field, but the Go handler ignores it (Go's JSON decoder silently drops unknown fields). Provider-specific routing is handled by the dedicated endpoints below.

**Response** (200 OK):
```json
{
  "success": true,
  "pod_name": "voice-agent-2",
  "ws_url": "wss://buddy.breezelabs.app/ws/pod/voice-agent-2/CA123abc",
  "source_pool": "pool:standard",
  "was_existing": false
}
```

**Error Responses**:
- `400`: Missing call_sid, invalid request body
- `503`: No pods available, pod is draining
- `500`: Internal allocation failure

### POST /api/v1/release

**Request** (JSON):
```json
{
  "call_sid": "CA123abc"
}
```

Note: The voice agent client also sends a `"reason"` field, but the Go handler ignores it (the `ReleaseRequest` struct only has `call_sid`). This is harmless.

**Response** (200 OK):
```json
{
  "success": true,
  "pod_name": "voice-agent-2",
  "released_to_pool": "pool:standard",
  "was_draining": false
}
```

**Error Responses**:
- `400`: Missing call_sid
- `404`: Call not found (already released or never allocated)
- `500`: Internal release failure

### POST /api/v1/twilio/allocate

**Request**: Form-encoded (Twilio webhook format)
- `CallSid` from form body
- `merchant_id` from query parameter

**Response**: TwiML XML
```xml
<Response>
  <Connect>
    <Stream url="wss://buddy.breezelabs.app/ws/pod/voice-agent-1/CA123abc"></Stream>
  </Connect>
</Response>
```

**Error**: TwiML with `<Say>` message + `<Hangup/>`

### POST /api/v1/plivo/allocate

**Request**: Form-encoded (Plivo webhook format)
- `CallUUID` from form body
- `merchant_id` from query parameter

**Response**: Plivo XML
```xml
<Response>
  <Stream bidirectional="true" keepCallAlive="true" contentType="audio/x-mulaw;rate=8000">
    wss://buddy.breezelabs.app/ws/pod/voice-agent-1/UUID123
  </Stream>
</Response>
```

### POST /api/v1/exotel/allocate

**Request** (JSON):
```json
{
  "CallSid": "exotel-sid-123",
  "merchant_id": "merchant_xyz"
}
```

**Response** (JSON):
```json
{
  "url": "wss://buddy.breezelabs.app/ws/pod/voice-agent-1/exotel-sid-123"
}
```

### POST /api/v1/drain

**Request** (JSON):
```json
{
  "pod_name": "voice-agent-1"
}
```

**Response** (200 OK):
```json
{
  "success": true,
  "pod_name": "voice-agent-1",
  "has_active_call": false,
  "message": "pod drained"
}
```

### GET /api/v1/status

**Response** (200 OK):
```json
{
  "pools": {},
  "active_calls": 0,
  "is_leader": true,
  "status": "up"
}
```

Note: `pools` is currently always empty (the StatusHandler doesn't populate it — it's a TODO). Use Redis inspection commands (Section 10) to see actual pool state.

### GET /api/v1/pod/{pod_name}

Returns pod info from Redis.

### GET /health

Returns `{"status":"ok"}` — used for K8s liveness probe.

### GET /ready

Returns `{"status":"ready"}` — used for K8s readiness probe.

---

## 7. Feature Flags & Rollout Controls

The voice agent has multiple feature flags that control whether Smart Router is used:

### Environment Variable Flags (set in configmap/deployment)

| Flag | Current Value | Purpose |
|------|---------------|---------|
| `ENABLE_VOICE_AGENT_POD_ISOLATION` | `"true"` | Master switch. If false, Smart Router is never called. |
| `SMART_ROUTER_BASE_URL` | Not set (defaults to `http://smart-router:8080`) | Smart Router API URL |
| `SMART_ROUTER_ENABLE_FALLBACK` | `"true"` (default) | If true, voice agent falls back to non-isolated mode when Smart Router fails |
| `SMART_ROUTER_TIMEOUT_MS` | `3000` (default) | Timeout for Smart Router API calls |
| `SMART_ROUTER_RETRY_ATTEMPTS` | `3` (default) | Number of retries for failed allocations |
| `SMART_ROUTER_CB_FAILURE_THRESHOLD` | `5` (default) | Failures before circuit breaker opens |
| `SMART_ROUTER_CB_RECOVERY_TIMEOUT` | `30` (default) | Seconds before circuit breaker enters half-open |

### Redis Live Config Flags (read at runtime, no restart needed)

These are read from Redis by the voice agent's live config store. They can be changed live:

| Flag | Redis Key/Source | Purpose |
|------|------------------|---------|
| `SMART_ROUTER_PERCENTAGE` | Live config store | 0-100, percentage of calls routed through Smart Router. Hash-based on merchant_id for consistency. |
| `SMART_ROUTER_MERCHANTS` | Live config store | List of merchant_ids that should always use Smart Router (regardless of percentage). |
| `SMART_ROUTER_ENABLE_TWILIO_DIRECT` | Live config store | If true, Twilio calls route directly to Smart Router's `/api/v1/twilio/allocate` endpoint. |

### Decision Flow in Voice Agent

```python
async def _should_use_smart_router(merchant_id):
    if not ENABLE_VOICE_AGENT_POD_ISOLATION:  # env var
        return False
    if circuit_breaker.is_open:
        return False
    percentage = await SMART_ROUTER_PERCENTAGE()  # live config
    if percentage == 0:
        return False
    if percentage < 100 and merchant_id:
        hash_val = md5(merchant_id) % 100
        if hash_val >= percentage:
            return False
    targeted = await SMART_ROUTER_MERCHANTS()  # live config
    if targeted and merchant_id not in targeted:
        return False
    return True
```

### How to Enable Smart Router for Testing

To enable Smart Router for ALL calls:
1. `ENABLE_VOICE_AGENT_POD_ISOLATION` is already `"true"` in the configmap.
2. Set `SMART_ROUTER_PERCENTAGE` to `100` in the live config store (Redis).
3. Ensure `SMART_ROUTER_MERCHANTS` is empty (no merchant targeting).

To enable for SPECIFIC merchants only:
1. Set `SMART_ROUTER_PERCENTAGE` to `100`.
2. Set `SMART_ROUTER_MERCHANTS` to `["merchant_abc", "merchant_xyz"]` in Redis.

To test via direct API calls (bypassing voice agent):
- Call `https://clairvoyance.breezelabs.app/api/v1/allocate` directly with curl.
- This bypasses all feature flags — the smart-router itself has no feature flags.

---

## 8. Redis Key Reference

### Pool Keys

| Key Pattern | Type | Description |
|-------------|------|-------------|
| `voice:pool:{tier}:available` | SET (exclusive) or ZSET (shared) | Available pods for allocation |
| `voice:pool:{tier}:assigned` | SET | All pods assigned to this tier |

### Pod Keys

| Key Pattern | Type | Fields | Description |
|-------------|------|--------|-------------|
| `voice:pod:{pod_name}` | HASH | status, allocated_call_sid, allocated_at, source_pool | Pod current state |
| `voice:pod:tier:{pod_name}` | STRING | tier name | Which tier this pod belongs to |
| `voice:pod:draining:{pod_name}` | STRING | "1" | Pod is draining (TTL: 6 min) |
| `voice:lease:{pod_name}` | STRING | call_sid | Active lease (TTL: 24h) |

### Call Keys

| Key Pattern | Type | Fields | Description |
|-------------|------|--------|-------------|
| `voice:call:{call_sid}` | HASH | pod_name, source_pool, merchant_id, allocated_at | Call allocation record (TTL: 24h) |
| `voice:call:{call_sid}` field `_lock` | HASH field | "1" | Temporary lock during allocation (TTL: 30s, deleted after allocation) |

### Merchant Config

| Key Pattern | Type | Fields | Description |
|-------------|------|--------|-------------|
| `voice:merchant:config` | HASH | merchant_id -> JSON | Merchant tier configuration |

Example merchant config value: `{"tier": "gold"}` or `{"tier": "dedicated", "pool": "merchant_acme"}`

### Pod Metadata

| Key Pattern | Type | Description |
|-------------|------|-------------|
| `voice:pod:metadata` | HASH | pod_name -> JSON metadata |

---

## 9. Test Scenarios

### Prerequisites for All Tests

```bash
# 1. Verify cluster access
kubectl get pods -n voice-system

# 2. Verify smart-router is healthy
curl -s https://clairvoyance.breezelabs.app/health
# Expected: {"status":"ok"}

# 3. Verify pool status
curl -s https://clairvoyance.breezelabs.app/api/v1/status | python3 -m json.tool
# Expected: active_calls: 0, status: "up"

# 4. Check Redis connectivity (from smart-router pod)
kubectl exec -it $(kubectl get pods -n voice-system -l app=smart-router -o jsonpath='{.items[0].metadata.name}') -n voice-system -- wget -qO- http://localhost:8080/health
```

### Cleanup Between Tests

After each test scenario, ensure all test calls are released:

```bash
# Release a test call
curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/release \
  -H "Content-Type: application/json" \
  -d '{"call_sid": "YOUR_TEST_CALL_SID"}'

# Verify 0 active calls
curl -s https://clairvoyance.breezelabs.app/api/v1/status | python3 -m json.tool
```

---

### 9.1 Basic Allocation & Release (Default Merchant)

**Goal**: Verify a call with no merchant config allocates from standard pool, then falls back.

```bash
# Step 1: Allocate
curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/allocate \
  -H "Content-Type: application/json" \
  -d '{"call_sid": "test-default-001", "merchant_id": ""}' | python3 -m json.tool

# Expected:
# {
#   "success": true,
#   "pod_name": "voice-agent-X",
#   "ws_url": "wss://buddy.breezelabs.app/ws/pod/voice-agent-X/test-default-001",
#   "source_pool": "pool:standard",
#   "was_existing": false
# }

# Step 2: Verify Redis state
kubectl run redis-check -n voice-system --rm -it --restart=Never --image=redis:7-alpine \
  -- redis-cli -h 10.100.0.4 HGETALL voice:call:test-default-001

# Expected: pod_name, source_pool, merchant_id, allocated_at fields

# Step 3: Release
curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/release \
  -H "Content-Type: application/json" \
  -d '{"call_sid": "test-default-001"}' | python3 -m json.tool

# Expected:
# {
#   "success": true,
#   "pod_name": "voice-agent-X",
#   "released_to_pool": "pool:standard",
#   "was_draining": false
# }

# Step 4: Verify pod returned to available pool
kubectl run redis-check2 -n voice-system --rm -it --restart=Never --image=redis:7-alpine \
  -- redis-cli -h 10.100.0.4 SMEMBERS voice:pool:standard:available

# Expected: voice-agent-X should be back in the set
```

**VERIFIED**: This scenario was tested and works correctly.

---

### 9.2 Gold Merchant Allocation

**Goal**: Verify a merchant configured for gold tier gets allocated from gold pool.

```bash
# Step 1: Set merchant config in Redis
kubectl run redis-set -n voice-system --rm -it --restart=Never --image=redis:7-alpine \
  -- redis-cli -h 10.100.0.4 HSET voice:merchant:config gold-merchant-001 '{"tier":"gold"}'

# Step 2: Allocate
curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/allocate \
  -H "Content-Type: application/json" \
  -d '{"call_sid": "test-gold-001", "merchant_id": "gold-merchant-001"}' | python3 -m json.tool

# Expected:
# source_pool: "pool:gold"
# pod_name: one of the pods assigned to gold tier

# Step 3: Verify, then release
curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/release \
  -H "Content-Type: application/json" \
  -d '{"call_sid": "test-gold-001"}'

# Step 4: Clean up merchant config (optional)
kubectl run redis-del -n voice-system --rm -it --restart=Never --image=redis:7-alpine \
  -- redis-cli -h 10.100.0.4 HDEL voice:merchant:config gold-merchant-001
```

---

### 9.3 Shared Pool (Basic Tier) - Multiple Concurrent Calls

**Goal**: Verify basic tier pod handles multiple calls up to max_concurrent (3).

```bash
# Step 1: Find which pod is in basic pool
kubectl run redis-basic -n voice-system --rm -it --restart=Never --image=redis:7-alpine \
  -- redis-cli -h 10.100.0.4 ZRANGE voice:pool:basic:available 0 -1 WITHSCORES

# Note the pod name and its current score

# Step 2: Set merchant config for basic tier
kubectl run redis-set -n voice-system --rm -it --restart=Never --image=redis:7-alpine \
  -- redis-cli -h 10.100.0.4 HSET voice:merchant:config basic-merchant '{"tier":"basic"}'

# Step 3: Allocate 3 concurrent calls
for i in 1 2 3; do
  echo "=== Call $i ==="
  curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/allocate \
    -H "Content-Type: application/json" \
    -d "{\"call_sid\": \"test-shared-$i\", \"merchant_id\": \"basic-merchant\"}" | python3 -m json.tool
done

# Expected: All 3 should get the same pod_name (shared), source_pool: "pool:basic"
# ZSET score for that pod should increment to 1, 2, 3

# Step 4: Verify ZSET score
kubectl run redis-score -n voice-system --rm -it --restart=Never --image=redis:7-alpine \
  -- redis-cli -h 10.100.0.4 ZRANGE voice:pool:basic:available 0 -1 WITHSCORES

# Expected: pod score = 3

# Step 5: Try 4th call (should fail or fall through to another pool)
curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/allocate \
  -H "Content-Type: application/json" \
  -d '{"call_sid": "test-shared-4", "merchant_id": "basic-merchant"}' | python3 -m json.tool

# Expected: Either 503 (no pods) if no other basic pods, or allocated from another pool via fallback

# Step 6: Release all test calls
for i in 1 2 3 4; do
  curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/release \
    -H "Content-Type: application/json" \
    -d "{\"call_sid\": \"test-shared-$i\"}"
done

# Step 7: Verify ZSET score back to 0
kubectl run redis-score2 -n voice-system --rm -it --restart=Never --image=redis:7-alpine \
  -- redis-cli -h 10.100.0.4 ZRANGE voice:pool:basic:available 0 -1 WITHSCORES

# Clean up
kubectl run redis-del -n voice-system --rm -it --restart=Never --image=redis:7-alpine \
  -- redis-cli -h 10.100.0.4 HDEL voice:merchant:config basic-merchant
```

---

### 9.4 Idempotency (Same call_sid Twice)

**Goal**: Verify that allocating the same call_sid twice returns the same pod (idempotent).

```bash
# Step 1: First allocation
curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/allocate \
  -H "Content-Type: application/json" \
  -d '{"call_sid": "test-idempotent-001"}' | python3 -m json.tool

# Note the pod_name

# Step 2: Second allocation (same call_sid)
curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/allocate \
  -H "Content-Type: application/json" \
  -d '{"call_sid": "test-idempotent-001"}' | python3 -m json.tool

# Expected:
# Same pod_name as Step 1
# was_existing: true

# Step 3: Clean up
curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/release \
  -H "Content-Type: application/json" \
  -d '{"call_sid": "test-idempotent-001"}'
```

---

### 9.5 Concurrent Race Condition (Same call_sid)

**Goal**: Verify that 5 concurrent allocations for the same call_sid only allocate one pod.

```bash
# Step 1: Fire 5 concurrent requests
for i in {1..5}; do
  curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/allocate \
    -H "Content-Type: application/json" \
    -d '{"call_sid": "test-race-001"}' &
done
wait

# Expected: All 5 should return the same pod_name.
# Only 1 pod should be consumed from the pool.

# Step 2: Check Redis — only 1 pod should be allocated
kubectl run redis-race -n voice-system --rm -it --restart=Never --image=redis:7-alpine \
  -- redis-cli -h 10.100.0.4 HGETALL voice:call:test-race-001

# Step 3: Clean up
curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/release \
  -H "Content-Type: application/json" \
  -d '{"call_sid": "test-race-001"}'
```

This tests the atomic Lua-based idempotency lock (Bug Fix #3 in Section 12).

---

### 9.6 Infrastructure Failure & Recovery

**Goal**: Verify system recovers from crashes, restarts, and dependency failures.

#### Scenario A: Smart Router Crash/Restart
1. **Action**: Delete a smart-router pod (`kubectl delete pod ...`).
2. **Behavior**: Kubernetes restarts it. New leader elected (if leader died).
3. **Verification**: `syncAllPods` runs on startup, repopulating Redis pools from K8s state.
4. **Test**: Run `GET /status` after restart; mapped pools should match running voice-agents.

#### Scenario B: Rolling Update of Voice Agents
1. **Action**: `kubectl rollout restart statefulset/voice-agent`.
2. **Behavior**: Pods terminate one by one.
3. **Verification**:
   - `reconciler.go` detects deletion.
   - **Orphan Cleanup**: Checks valid `allocated_call_sid` and removes `voice:call:{sid}` key.
   - **Re-add**: When new pod is Ready, `watcher.go` adds it back to pool.

#### Scenario C: Redis Data Loss (Simulated Flush)
1. **Action**: `redis-cli FLUSHDB` (Simulating total data loss).
2. **Behavior**: System momentarily empty.
3. **Recovery**: `manager.go` ticker (1-min) triggers `syncAllPods`.
4. **Verification**: Wait 60s. Pools should automatically repopulate without service restart.


---

### 9.6 Release Idempotency (Release Same Call Twice)

**Goal**: Verify releasing the same call twice doesn't cause errors.

```bash
# Step 1: Allocate
curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/allocate \
  -H "Content-Type: application/json" \
  -d '{"call_sid": "test-release-idem-001"}'

# Step 2: First release
curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/release \
  -H "Content-Type: application/json" \
  -d '{"call_sid": "test-release-idem-001"}' | python3 -m json.tool

# Expected: 200, success: true

# Step 3: Second release (same call_sid)
curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/release \
  -H "Content-Type: application/json" \
  -d '{"call_sid": "test-release-idem-001"}' | python3 -m json.tool

# Expected: 404 (call not found) — this is correct behavior
# The voice agent client treats 404 as success (already released)
```

---

### 9.7 Release Non-Existent Call

**Goal**: Verify releasing a call that was never allocated returns 404.

```bash
curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/release \
  -H "Content-Type: application/json" \
  -d '{"call_sid": "never-allocated-call"}'

# Expected: HTTP 404, {"error": "call not found"}
```

---

### 9.8 Pool Exhaustion & Fallback

**Goal**: Exhaust all exclusive pools, verify calls fall through to basic shared pool.

```bash
# Step 1: Check which pods are in which pools
kubectl run redis-pools -n voice-system --rm -it --restart=Never --image=redis:7-alpine -- sh -c '
  echo "=== Gold Available ===" && redis-cli -h 10.100.0.4 SMEMBERS voice:pool:gold:available
  echo "=== Standard Available ===" && redis-cli -h 10.100.0.4 SMEMBERS voice:pool:standard:available
  echo "=== Basic Available ===" && redis-cli -h 10.100.0.4 ZRANGE voice:pool:basic:available 0 -1 WITHSCORES
  echo "=== Overflow Available ===" && redis-cli -h 10.100.0.4 SMEMBERS voice:pool:overflow:available
'

# Step 2: Allocate calls to exhaust standard (should take 1 call)
curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/allocate \
  -H "Content-Type: application/json" \
  -d '{"call_sid": "exhaust-std-001"}' | python3 -m json.tool

# Note: source_pool should be "pool:standard"

# Step 3: Allocate another — standard exhausted, should fall to overflow or basic
curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/allocate \
  -H "Content-Type: application/json" \
  -d '{"call_sid": "exhaust-std-002"}' | python3 -m json.tool

# Note the source_pool — should be "pool:overflow" or "pool:basic"

# Step 4: Keep allocating until 503
# (Number of calls depends on pool state — with 5 pods total, you'll hit the limit)

# Step 5: Clean up — release all test calls
for i in 001 002 003 004 005 006; do
  curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/release \
    -H "Content-Type: application/json" \
    -d "{\"call_sid\": \"exhaust-std-$i\"}" 2>/dev/null
done
```

---

### 9.9 Draining a Pod

**Goal**: Verify draining marks pod as unavailable, and it's skipped during allocation.

```bash
# Step 1: Find an available pod
kubectl run redis-avail -n voice-system --rm -it --restart=Never --image=redis:7-alpine \
  -- redis-cli -h 10.100.0.4 SMEMBERS voice:pool:standard:available

# Step 2: Drain that pod
curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/drain \
  -H "Content-Type: application/json" \
  -d '{"pod_name": "voice-agent-X"}' | python3 -m json.tool

# Expected: success: true

# Step 3: Verify draining key exists in Redis
kubectl run redis-drain -n voice-system --rm -it --restart=Never --image=redis:7-alpine \
  -- redis-cli -h 10.100.0.4 EXISTS voice:pod:draining:voice-agent-X

# Expected: 1

# Step 4: Try to allocate — should NOT get the draining pod
curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/allocate \
  -H "Content-Type: application/json" \
  -d '{"call_sid": "test-drain-skip-001"}' | python3 -m json.tool

# Verify pod_name is NOT the draining pod

# Step 5: Wait for draining TTL to expire (6 minutes) or manually delete
kubectl run redis-undrain -n voice-system --rm -it --restart=Never --image=redis:7-alpine \
  -- redis-cli -h 10.100.0.4 DEL voice:pod:draining:voice-agent-X

# Step 6: Clean up
curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/release \
  -H "Content-Type: application/json" \
  -d '{"call_sid": "test-drain-skip-001"}'
```

---

### 9.10 Twilio Webhook (Provider-Specific)

**Goal**: Verify Twilio webhook returns proper TwiML XML.

```bash
# Step 1: Simulate Twilio webhook
curl -s -X POST "https://clairvoyance.breezelabs.app/agent/voice/breeze-buddy/twilio/callback/test-template?merchant_id=test-merchant" \
  -d "CallSid=test-twilio-001&AccountSid=ACtest&From=%2B1234567890&To=%2B0987654321"

# Expected: XML response like:
# <Response>
#   <Connect>
#     <Stream url="wss://buddy.breezelabs.app/ws/pod/voice-agent-X/test-twilio-001"></Stream>
#   </Connect>
# </Response>

# Step 2: Verify allocation in Redis
kubectl run redis-twilio -n voice-system --rm -it --restart=Never --image=redis:7-alpine \
  -- redis-cli -h 10.100.0.4 HGETALL voice:call:test-twilio-001

# Step 3: Clean up
curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/release \
  -H "Content-Type: application/json" \
  -d '{"call_sid": "test-twilio-001"}'
```

**VERIFIED**: This scenario was tested and works.

---

### 9.11 Plivo Webhook (Provider-Specific)

**Goal**: Verify Plivo webhook returns proper Plivo XML.

```bash
curl -s -X POST "https://clairvoyance.breezelabs.app/agent/voice/breeze-buddy/plivo/callback/test-template?merchant_id=test-merchant" \
  -d "CallUUID=test-plivo-001&From=1234567890&To=0987654321"

# Expected: Plivo XML response with bidirectional Stream

# Clean up
curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/release \
  -H "Content-Type: application/json" \
  -d '{"call_sid": "test-plivo-001"}'
```

---

### 9.12 Exotel Webhook (Provider-Specific)

**Goal**: Verify Exotel webhook returns JSON with WebSocket URL.

```bash
curl -s -X POST https://clairvoyance.breezelabs.app/agent/voice/breeze-buddy/exotel/callback/test-template \
  -H "Content-Type: application/json" \
  -d '{"CallSid": "test-exotel-001", "merchant_id": "test-merchant"}' | python3 -m json.tool

# Expected:
# { "url": "wss://buddy.breezelabs.app/ws/pod/voice-agent-X/test-exotel-001" }

# Clean up
curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/release \
  -H "Content-Type: application/json" \
  -d '{"call_sid": "test-exotel-001"}'
```

---

### 9.13 Input Validation

**Goal**: Verify proper error handling for malformed requests.

```bash
# Missing call_sid
curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/allocate \
  -H "Content-Type: application/json" \
  -d '{"merchant_id": "test"}'
# Expected: 400, "call_sid is required"

# Empty call_sid
curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/allocate \
  -H "Content-Type: application/json" \
  -d '{"call_sid": ""}'
# Expected: 400, "call_sid is required"

# Malformed JSON
curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/allocate \
  -H "Content-Type: application/json" \
  -d '{"invalid json'
# Expected: 400, "invalid request body"

# Missing body entirely
curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/allocate \
  -H "Content-Type: application/json"
# Expected: 400
```

---

### 9.14 Merchant Config CRUD

**Goal**: Verify merchant configuration affects allocation tier.

```bash
# Step 1: Set gold merchant config
kubectl run redis-merchant -n voice-system --rm -it --restart=Never --image=redis:7-alpine \
  -- redis-cli -h 10.100.0.4 HSET voice:merchant:config merchant-gold-test '{"tier":"gold"}'

# Step 2: Allocate for gold merchant
curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/allocate \
  -H "Content-Type: application/json" \
  -d '{"call_sid": "test-merchant-gold", "merchant_id": "merchant-gold-test"}' | python3 -m json.tool
# Expected: source_pool should be "pool:gold"

# Step 3: Release
curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/release \
  -H "Content-Type: application/json" \
  -d '{"call_sid": "test-merchant-gold"}'

# Step 4: Change merchant to standard
kubectl run redis-merchant2 -n voice-system --rm -it --restart=Never --image=redis:7-alpine \
  -- redis-cli -h 10.100.0.4 HSET voice:merchant:config merchant-gold-test '{"tier":"standard"}'

# Step 5: Allocate again — should come from standard
curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/allocate \
  -H "Content-Type: application/json" \
  -d '{"call_sid": "test-merchant-std", "merchant_id": "merchant-gold-test"}' | python3 -m json.tool
# Expected: source_pool should be "pool:standard"

# Step 6: Clean up
curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/release \
  -H "Content-Type: application/json" \
  -d '{"call_sid": "test-merchant-std"}'

kubectl run redis-cleanup -n voice-system --rm -it --restart=Never --image=redis:7-alpine \
  -- redis-cli -h 10.100.0.4 HDEL voice:merchant:config merchant-gold-test
```

---

### 9.15 Scale Up Voice Agents

**Goal**: Verify new pods are detected and added to pools.

```bash
# Step 1: Record current state
kubectl get statefulset voice-agent -n voice-system
kubectl run redis-pre -n voice-system --rm -it --restart=Never --image=redis:7-alpine -- sh -c '
  for tier in gold standard basic overflow; do
    echo "=== $tier:assigned ===" && redis-cli -h 10.100.0.4 SMEMBERS voice:pool:$tier:assigned
  done
'

# Step 2: Scale up
kubectl scale statefulset voice-agent -n voice-system --replicas=7

# Step 3: Wait for new pods to be ready
kubectl get pods -n voice-system -w

# Step 4: Check Smart Router logs for pod detection
kubectl logs deployment/smart-router -n voice-system --tail=50 | grep -i "pod"

# Step 5: Verify new pods appear in Redis pools
kubectl run redis-post -n voice-system --rm -it --restart=Never --image=redis:7-alpine -- sh -c '
  for tier in gold standard basic overflow; do
    echo "=== $tier:assigned ===" && redis-cli -h 10.100.0.4 SMEMBERS voice:pool:$tier:assigned
  done
'

# Step 6: Test allocation to new pod
curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/allocate \
  -H "Content-Type: application/json" \
  -d '{"call_sid": "test-scale-001"}' | python3 -m json.tool

# Step 7: Scale back down
kubectl scale statefulset voice-agent -n voice-system --replicas=5

# Step 8: Clean up
curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/release \
  -H "Content-Type: application/json" \
  -d '{"call_sid": "test-scale-001"}'
```

---

### 9.16 Pod Crash Recovery

**Goal**: Verify pool manager recovers pod state after a pod restart.

```bash
# Step 1: Note pod state
kubectl run redis-pre -n voice-system --rm -it --restart=Never --image=redis:7-alpine \
  -- redis-cli -h 10.100.0.4 HGETALL voice:pod:voice-agent-2

# Step 2: Delete the pod (simulate crash)
kubectl delete pod voice-agent-2 -n voice-system

# Step 3: Wait for StatefulSet to recreate it
kubectl get pods -n voice-system -w

# Step 4: Check Smart Router logs
kubectl logs deployment/smart-router -n voice-system --tail=50 | grep -i "voice-agent-2"

# Step 5: Verify pod re-registered in pool
kubectl run redis-post -n voice-system --rm -it --restart=Never --image=redis:7-alpine -- sh -c '
  echo "=== Pod Info ===" && redis-cli -h 10.100.0.4 HGETALL voice:pod:voice-agent-2
  echo "=== Pod Tier ===" && redis-cli -h 10.100.0.4 GET voice:pod:tier:voice-agent-2
'

# Expected: Pod should be back in its assigned pool as "available"
```

---

### 9.17 Rolling Update - Smart Router

**Goal**: Verify zero downtime during Smart Router rolling update.

```bash
# Step 1: Start monitoring health in a separate terminal
while true; do
  STATUS=$(curl -s -o /dev/null -w "%{http_code}" https://clairvoyance.breezelabs.app/health)
  echo "$(date +%H:%M:%S) - $STATUS"
  sleep 1
done

# Step 2: Trigger rolling update
kubectl rollout restart deployment smart-router -n voice-system

# Step 3: Watch pods cycle
kubectl get pods -n voice-system -l app=smart-router -w

# Step 4: Verify leader election transfers
for pod in $(kubectl get pods -n voice-system -l app=smart-router -o jsonpath='{.items[*].metadata.name}'); do
  echo "=== $pod ==="
  kubectl logs -n voice-system $pod --tail=5 | grep -i leader
done

# Expected: No health check failures during rollout
```

---

### 9.18 Concurrent Allocations for Different Calls

**Goal**: Verify 20 concurrent allocations for different call_sids all succeed.

```bash
# Step 1: Fire 20 concurrent requests
for i in $(seq -w 1 20); do
  curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/allocate \
    -H "Content-Type: application/json" \
    -d "{\"call_sid\": \"concurrent-diff-$i\"}" -o /tmp/alloc-$i.json &
done
wait

# Step 2: Check results
for i in $(seq -w 1 20); do
  echo "=== Call $i ===" && cat /tmp/alloc-$i.json | python3 -m json.tool 2>/dev/null
done

# Expected: Each should succeed (until pools are exhausted).
# With 5 pods (1 gold + 1 standard + rest in basic/overflow), exclusive pools hold ~4 calls,
# basic can hold up to 3 more per pod. Total capacity depends on pool distribution.

# Step 3: Check active calls
curl -s https://clairvoyance.breezelabs.app/api/v1/status | python3 -m json.tool

# Step 4: Release all
for i in $(seq -w 1 20); do
  curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/release \
    -H "Content-Type: application/json" \
    -d "{\"call_sid\": \"concurrent-diff-$i\"}" &
done
wait

# Step 5: Verify 0 active calls
curl -s https://clairvoyance.breezelabs.app/api/v1/status | python3 -m json.tool

# Clean up temp files
rm -f /tmp/alloc-*.json
```

---

### 9.19 Health & Readiness Probes

**Goal**: Verify K8s probes work correctly.

```bash
# Liveness
curl -s https://clairvoyance.breezelabs.app/health
# Expected: {"status":"ok"}

# Readiness
curl -s https://clairvoyance.breezelabs.app/ready
# Expected: {"status":"ready"}

# Internal (from within cluster)
kubectl exec -it $(kubectl get pods -n voice-system -l app=smart-router -o jsonpath='{.items[0].metadata.name}') -n voice-system -- wget -qO- http://localhost:8080/health
kubectl exec -it $(kubectl get pods -n voice-system -l app=smart-router -o jsonpath='{.items[0].metadata.name}') -n voice-system -- wget -qO- http://localhost:8080/ready
```

**VERIFIED**: Both probes work.

---

### 9.20 WebSocket URL Format Verification

**Goal**: Verify the WebSocket URL format matches nginx-router's expected pattern.

The smart router builds ws_url as:
`wss://buddy.breezelabs.app/ws/pod/{pod_name}/{call_sid}`

The nginx-router regex is:
`^/ws/pod/(?<pod_name>[^/]+)/(?<call_sid>.*)$`

This proxies to: `http://{pod_name}.voice-agent:8000`

```bash
# Step 1: Allocate and check ws_url format
RESULT=$(curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/allocate \
  -H "Content-Type: application/json" \
  -d '{"call_sid": "test-wsurl-001"}')
echo "$RESULT" | python3 -m json.tool

# Verify ws_url matches pattern: wss://buddy.breezelabs.app/ws/pod/voice-agent-N/test-wsurl-001

# Step 2: Test DNS resolution of pod
kubectl run dns-test -n voice-system --rm -it --restart=Never --image=busybox \
  -- nslookup voice-agent-0.voice-agent.voice-system.svc.cluster.local

# Expected: Should resolve to the pod's IP

# Step 3: Clean up
curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/release \
  -H "Content-Type: application/json" \
  -d '{"call_sid": "test-wsurl-001"}'
```

---

## 10. Redis Inspection Commands

These commands let you inspect the full Redis state at any time. Run from an ephemeral redis-cli pod:

```bash
kubectl run redis-inspect -n voice-system --rm -it --restart=Never --image=redis:7-alpine -- sh
```

Then inside the pod:

```bash
# Connect
redis-cli -h 10.100.0.4

# === Pool State ===

# List all voice keys
KEYS voice:*

# Exclusive pools (Gold, Standard, Overflow)
SMEMBERS voice:pool:gold:available
SMEMBERS voice:pool:gold:assigned
SMEMBERS voice:pool:standard:available
SMEMBERS voice:pool:standard:assigned
SMEMBERS voice:pool:overflow:available
SMEMBERS voice:pool:overflow:assigned

# Shared pool (Basic) — ZSET with scores
ZRANGE voice:pool:basic:available 0 -1 WITHSCORES
SMEMBERS voice:pool:basic:assigned

# === Pod State ===

# Check individual pod info
HGETALL voice:pod:voice-agent-0
HGETALL voice:pod:voice-agent-1
HGETALL voice:pod:voice-agent-2
HGETALL voice:pod:voice-agent-3
HGETALL voice:pod:voice-agent-4

# Check pod tier assignments
GET voice:pod:tier:voice-agent-0
GET voice:pod:tier:voice-agent-1
GET voice:pod:tier:voice-agent-2
GET voice:pod:tier:voice-agent-3
GET voice:pod:tier:voice-agent-4

# Check draining flags
EXISTS voice:pod:draining:voice-agent-0
EXISTS voice:pod:draining:voice-agent-1
# (etc.)

# Check leases
GET voice:lease:voice-agent-0
GET voice:lease:voice-agent-1
# (etc.)
TTL voice:lease:voice-agent-0

# === Call State ===

# List all active calls
KEYS voice:call:*

# Check specific call
HGETALL voice:call:{call_sid}

# === Merchant Config ===

# List all merchant configs
HGETALL voice:merchant:config

# Get specific merchant
HGET voice:merchant:config {merchant_id}

# Set merchant config
HSET voice:merchant:config merchant-abc '{"tier":"gold"}'

# === Active Call Count ===
# Count leases (approximate active calls)
KEYS voice:lease:*
```

### Quick Health Check Script

```bash
kubectl run redis-health -n voice-system --rm -it --restart=Never --image=redis:7-alpine -- sh -c '
  echo "=== Pool State ==="
  for tier in gold standard basic overflow; do
    echo "--- $tier ---"
    echo "Assigned:" $(redis-cli -h 10.100.0.4 SMEMBERS voice:pool:$tier:assigned)
    if [ "$tier" = "basic" ]; then
      echo "Available (ZSET):" $(redis-cli -h 10.100.0.4 ZRANGE voice:pool:$tier:available 0 -1 WITHSCORES)
    else
      echo "Available (SET):" $(redis-cli -h 10.100.0.4 SMEMBERS voice:pool:$tier:available)
    fi
  done
  echo ""
  echo "=== Pod Status ==="
  for i in 0 1 2 3 4; do
    STATUS=$(redis-cli -h 10.100.0.4 HGET voice:pod:voice-agent-$i status)
    TIER=$(redis-cli -h 10.100.0.4 GET voice:pod:tier:voice-agent-$i)
    CALL=$(redis-cli -h 10.100.0.4 HGET voice:pod:voice-agent-$i allocated_call_sid)
    DRAIN=$(redis-cli -h 10.100.0.4 EXISTS voice:pod:draining:voice-agent-$i)
    LEASE=$(redis-cli -h 10.100.0.4 GET voice:lease:voice-agent-$i)
    echo "voice-agent-$i: tier=$TIER status=$STATUS call=$CALL draining=$DRAIN lease=$LEASE"
  done
  echo ""
  echo "=== Active Calls ==="
  redis-cli -h 10.100.0.4 KEYS "voice:call:*"
  echo ""
  echo "=== Active Leases ==="
  redis-cli -h 10.100.0.4 KEYS "voice:lease:*"
'
```

---

## 11. Build & Deploy Reference

### Smart Router Build & Deploy

```bash
# Working directory
cd /Users/harsh.tiwari/Documents/breeze-repos/orchestration-api-go

# 1. Build (Go compiles inside Docker — no local Go needed)
podman build --platform=linux/amd64 \
  -t asia-south1-docker.pkg.dev/breeze-automatic-prod/smart-router/smart-router:latest .

# 2. Push to GAR
gcloud auth print-access-token | podman login -u oauth2accesstoken --password-stdin asia-south1-docker.pkg.dev
podman push asia-south1-docker.pkg.dev/breeze-automatic-prod/smart-router/smart-router:latest

# 3. Deploy
kubectl rollout restart deployment/smart-router -n voice-system
kubectl rollout status deployment/smart-router -n voice-system --timeout=120s

# 4. Verify
kubectl get pods -n voice-system -l app=smart-router
kubectl logs deployment/smart-router -n voice-system --tail=20
curl -s https://clairvoyance.breezelabs.app/health
```

### Voice Agent (Reference Only — NOT modified in this session)

```bash
cd /Users/harsh.tiwari/Documents/breeze-repos/orchestration-clair

# Build
podman build --platform=linux/amd64 \
  -t asia-south1-docker.pkg.dev/breeze-automatic-prod/clairvoyance/orchestration-clair:latest .

# Push
podman push asia-south1-docker.pkg.dev/breeze-automatic-prod/clairvoyance/orchestration-clair:latest

# Deploy
kubectl rollout restart statefulset/voice-agent -n voice-system
```

### Tier Config Update

To change tier configuration:

```bash
# 1. Edit the configmap
kubectl edit configmap smart-router-config -n voice-system
# Modify the TIER_CONFIG JSON string

# 2. Restart smart-router to pick up changes
kubectl rollout restart deployment/smart-router -n voice-system
```

Example TIER_CONFIG values:

```json
# Current (conservative, 5 pods):
{"gold":{"type":"exclusive","target":1},"standard":{"type":"exclusive","target":1},"basic":{"type":"shared","target":1,"max_concurrent":3}}

# Scaled up (10+ pods):
{"gold":{"type":"exclusive","target":2},"standard":{"type":"exclusive","target":3},"basic":{"type":"shared","target":3,"max_concurrent":5}}
```

---

## 12. Bug Fixes Applied (Session Record)

These 7 fixes were applied, built, deployed, and verified on February 8, 2026:

### Fix #1: Missing `time` Import

**File**: `internal/poolmanager/manager.go`
**Issue**: Missing `"time"` in import block caused compilation error.
**Fix**: Added `"time"` to the import block.

### Fix #2: `isPodEligible` Swallows Redis Errors

**File**: `internal/poolmanager/watcher.go`
**Issue**: `isPodEligible()` ignored Redis errors (assigned them to `_`), which meant a busy pod could be added to the available pool if Redis had a transient error.
**Fix**: Returns `false` on any Redis error (fail-safe behavior). Added structured error logging.

### Fix #3: Race Condition in Idempotency Check

**Files**: `internal/allocator/idempotency.go`, `internal/allocator/allocator.go`
**Issue**: Two concurrent requests with the same `call_sid` could both pass the idempotency check (HGETALL returns empty for both), leading to double allocation.
**Fix**: Replaced `CheckExistingAllocation` with `CheckAndLockAllocation` — a Lua script that atomically checks for existing allocation AND acquires a 30-second placeholder lock if none exists. `storeAllocation` then overwrites the lock with real data, deletes the `_lock` field, and sets 24h TTL.

### Fix #4: Shared Lua Script Only Checks First ZSET Member

**File**: `internal/allocator/shared.go`
**Issue**: The Lua script for shared allocation only checked the first ZSET member. If that pod was draining, it returned nil (no pod available) even though other pods were available.
**Fix**: Rewrote Lua to iterate ALL ZSET members sorted by score ASC, skip draining pods, and allocate the first non-draining pod with capacity. Early exit if score >= max_concurrent.

### Fix #5: Zombie Cleanup Rejects Shared Pods

**File**: `internal/poolmanager/zombie.go`
**Issue**: Zombie cleanup used `isPodEligible()` for shared-tier pods, but shared pods legitimately have leases while handling multiple calls. This caused the cleanup to reject valid shared pods from being recovered.
**Fix**: Removed `isPodEligible()` check for shared tiers. The draining check at the outer loop (line 62) is sufficient.

### Fix #6: Hardcoded `SRem` in `removePodFromPool`

**File**: `internal/poolmanager/reconciler.go`
**Issue**: `removePodFromPool()` used hardcoded `SRem` for all tiers, which breaks if a tier is shared (ZSET, not SET).
**Fix**: Replaced with type-aware dynamic loop that checks `ParsedTierConfig` for tier type and uses `ZRem` for shared tiers, `SRem` for exclusive tiers. Includes fallback for tiers not in the config (gold, standard, overflow).

### Fix #7: `fmt.Printf` Instead of Structured Logger

**Files**: `internal/releaser/releaser.go`, `cmd/smart-router/main.go`
**Issue**: Releaser used `fmt.Printf` for error logging, which doesn't go through the structured zap logger.
**Fix**: Added `*zap.Logger` field to `Releaser` struct, updated `NewReleaser` to accept logger, replaced 3 `fmt.Printf` calls with `r.logger.Warn(...)`. Updated `main.go` to pass logger.

---

## 13. Known Gaps & Risks

### API Contract Mismatches (Harmless)

1. **Allocate request**: Voice agent sends `provider` field but Go struct only has `call_sid` and `merchant_id`. The `provider` field is silently ignored. Provider-specific handling is done via dedicated endpoints.
2. **Release request**: Voice agent sends `reason` field but Go struct only has `call_sid`. The `reason` field is silently ignored.

Both are harmless because Go's JSON decoder drops unknown fields by default.

### Status Endpoint Limitation

The `/api/v1/status` endpoint's `pools` field is always empty `{}`. Pool details must be inspected directly via Redis (see Section 10). This is a TODO in the code.

### Redis Single Point of Failure

Redis at `10.100.0.4:6379` is an external single instance, not clustered. If Redis goes down:
- Smart Router cannot allocate or release pods
- Health endpoints will still return 200 (they don't depend on Redis)
- Readiness should ideally fail (check if it does)
- All pool state is lost if Redis data is flushed

### Draining TTL vs Pod Termination

Draining keys have a 6-minute TTL (`voice:pod:draining:{name}`). If a pod takes longer than 6 minutes to terminate, the draining flag expires and the pod could be re-added to the available pool while still shutting down. The smart-router deployment has a `terminationGracePeriodSeconds: 30` which is well within the 6-minute window. Voice agent has `terminationGracePeriodSeconds: 60`. Both should be safe.

### VOICE_AGENT_BASE_URL

This is set to `wss://buddy.breezelabs.app` in the `smart-router-secrets`. If this value is wrong, all WebSocket URLs will be wrong and calls will fail. Verify by checking the ws_url in any allocation response.

### Stale Redis Data

If pods are removed or tiers change without proper cleanup, stale data can accumulate in Redis. The reconciler (every 60s) and zombie cleanup (every 30s) help, but manual inspection is recommended after major changes. Use the "Quick Health Check Script" from Section 10.

### Not Yet Tested

The following scenarios have NOT been verified in production:
- Real Twilio/Plivo/Exotel call flow (only simulated webhooks)
- WebSocket audio streaming through the nginx-router proxy
- Voice agent auto-release on WebSocket disconnect
- Redis failure/recovery behavior
- High-load concurrent allocation (>20 simultaneous)
- Rolling update of voice-agent during active calls

---

*End of document.*
