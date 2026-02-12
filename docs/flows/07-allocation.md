# Flow 07: Pod Allocation

**Source files:**
- `internal/allocator/allocator.go` (324 lines) — Core `Allocate()`, fallback chain, storage, rollback
- `internal/allocator/exclusive.go` (47 lines) — `tryAllocateExclusive()` via SPOP
- `internal/allocator/shared.go` (81 lines) — `tryAllocateShared()` via Lua script
- `internal/allocator/idempotency.go` (85 lines) — `CheckAndLockAllocation()` via Lua script
- `internal/allocator/merchant.go` (39 lines) — `GetMerchantConfig()` from Redis hash
- `internal/allocator/types.go` (27 lines) — Interface + error sentinels
- `internal/api/handlers/allocate.go` (331 lines) — HTTP handlers (generic, Twilio, Plivo, Exotel)

## Overview

Allocation is the core flow of the smart router — when a telephony call arrives, a pod must be assigned to handle it. The allocator receives a call SID, merchant ID, and provider info, then walks a configurable fallback chain of pools until it finds an available pod. The result is a WebSocket URL that the telephony provider uses to stream audio to the assigned voice-agent pod.

This flow runs on **all smart-router replicas** (stateless, no leader election required). Every replica can handle allocation requests concurrently because the Redis operations are atomic.

## Entry Points — HTTP Handlers

Four HTTP endpoints trigger allocation, each tailored to a different telephony provider:

| Endpoint | Handler | Input Format | Response Format |
|----------|---------|-------------|-----------------|
| `POST /api/v1/allocate` | `Handle()` | JSON body | JSON |
| `POST /api/v1/twilio/allocate` | `HandleTwilio()` | Form data (`CallSid`) + query params | TwiML XML |
| `POST /api/v1/plivo/allocate` | `HandlePlivo()` | Form data (`CallUUID`) + query params | Plivo XML |
| `POST /api/v1/exotel/allocate` | `HandleExotel()` | JSON body (`CallSid`) + query params | JSON |

### Generic Endpoint — `Handle()` (allocate.go:35-95)

```
POST /api/v1/allocate
Content-Type: application/json

{
    "call_sid":    "CA123...",        // Required
    "merchant_id": "9shines",         // Optional
    "provider":    "twilio",          // Optional, defaults to "twilio"
    "flow":        "v2",              // Optional, defaults to "v2"
    "template":    "order-confirmation" // Optional
}
```

Response:
```json
{
    "success": true,
    "pod_name": "voice-agent-0",
    "ws_url": "wss://clairvoyance.breezelabs.app/ws/pod/voice-agent-0/agent/voice/breeze-buddy/twilio/callback/order-confirmation/v2",
    "source_pool": "pool:gold",
    "was_existing": false
}
```

Error responses:
- `400` — missing `call_sid` or invalid body
- `503` — `ErrNoPodsAvailable` or `ErrDrainingPod`
- `500` — internal error

### Twilio Endpoint — `HandleTwilio()` (allocate.go:108-160)

Twilio sends an HTTP POST with **form-encoded** data. The call SID comes from the `CallSid` form field. Merchant ID, flow, and template come from **query parameters** (set in the Twilio webhook URL configuration).

Response is **TwiML XML** — Twilio's instruction format:
```xml
<Response>
    <Connect>
        <Stream url="wss://clairvoyance.breezelabs.app/ws/pod/voice-agent-0/agent/voice/breeze-buddy/twilio/callback/order-confirmation/v2"/>
    </Connect>
</Response>
```

On error, returns a TwiML `<Say>` + `<Hangup/>` so Twilio speaks the error and hangs up.

### Plivo Endpoint — `HandlePlivo()` (allocate.go:176-231)

Similar to Twilio but uses `CallUUID` (Plivo's term) from form data. Response is Plivo XML:
```xml
<Response>
    <Stream bidirectional="true" keepCallAlive="true" contentType="audio/x-mulaw;rate=8000">
        wss://clairvoyance.breezelabs.app/ws/pod/voice-agent-0/agent/voice/breeze-buddy/plivo/callback/order-confirmation/v2
    </Stream>
</Response>
```

### Exotel Endpoint — `HandleExotel()` (allocate.go:247-292)

Exotel sends JSON (not form data). Response is plain JSON:
```json
{
    "url": "wss://clairvoyance.breezelabs.app/ws/pod/voice-agent-0/agent/voice/breeze-buddy/exotel/callback/template/v2"
}
```

Note: Exotel defaults the template to `"template"` instead of `"order-confirmation"` (handled in `buildWSURL()`).

---

## Core Allocation — `Allocate()` (allocator.go:58-138)

All four HTTP handlers call the same core method:
```go
func (a *Allocator) Allocate(ctx context.Context, callSID, merchantID, provider, flow, template string) (*models.AllocationResult, error)
```

### Step-by-Step Walkthrough

```
                    ┌──────────────────────────┐
                    │  HTTP Handler (any of 4)  │
                    └────────────┬─────────────┘
                                 │
                    ┌────────────▼─────────────┐
                    │ 1. Validate call_sid      │
                    └────────────┬─────────────┘
                                 │
                    ┌────────────▼─────────────┐
                    │ 2. Idempotency check/lock │◄── Lua script (atomic)
                    │    (CheckAndLockAlloc.)   │
                    └────────────┬─────────────┘
                         ┌───────┴───────┐
                    existing?          new lock
                         │                │
                    return existing   ┌───▼───────────────┐
                    + build WSURL     │ 3. Get merchant    │
                                      │    config from Redis│
                                      └───────┬───────────┘
                                              │
                                      ┌───────▼───────────┐
                                      │ 4. Resolve fallback│
                                      │    chain           │
                                      └───────┬───────────┘
                                              │
                                      ┌───────▼───────────┐
                                      │ 5. Walk chain:     │
                                      │    try each pool   │◄─┐
                                      └───────┬───────────┘   │
                                         found?               │
                                      ┌───┴───┐               │
                                     yes      no──────────────┘
                                      │        (next pool)
                                      │
                              no pod found? → return ErrNoPodsAvailable
                                      │
                                      │ pod found
                                      │
                              ┌───────▼───────────┐
                              │ 6. Store allocation│
                              │    (call info +    │
                              │     pod info +     │
                              │     lease)         │
                              └───────┬───────────┘
                                  success?
                              ┌───┴───┐
                             yes      no → returnPodToPool() + error
                              │
                              │
                      ┌───────▼───────────┐
                      │ 7. Return result   │
                      │    + increment     │
                      │    ActiveCalls     │
                      └───────────────────┘
```

---

### Step 1: Validate Input (allocator.go:59-61)

```go
if callSID == "" {
    return nil, ErrInvalidCallSID
}
```

Only `callSID` is mandatory. Everything else has defaults.

---

### Step 2: Idempotency Check — `CheckAndLockAllocation()` (idempotency.go:40-84)

**Purpose:** Prevent double allocation for the same call SID. Telephony providers (especially Twilio) can retry the webhook if the first response is slow.

**Mechanism:** A Lua script runs atomically in Redis:

```lua
local call_key = KEYS[1]    -- "voice:call:{callSID}"

-- Check if call already has data
local existing = redis.call('HGETALL', call_key)
if #existing > 0 then
    return existing           -- Return existing allocation
end

-- No data: atomically set a lock placeholder
redis.call('HSET', call_key, '_lock', '1')
redis.call('EXPIRE', call_key, 30)    -- 30s TTL safety net
return {}                              -- Empty = lock acquired
```

**Three outcomes:**

| Outcome | Return | What Happens |
|---------|--------|-------------|
| Call already allocated | `(result, nil)` | Returns existing allocation immediately. WSURL is rebuilt from current params (allocator.go:71). |
| Lock acquired | `(nil, nil)` | Caller proceeds with allocation. |
| Lock placeholder only (race) | `(nil, nil)` | If only `_lock` field exists (no `pod_name`), treated as not-yet-allocated. Another goroutine may be mid-allocation. |
| Redis error | `(nil, err)` | **Falls through** (allocator.go:66-69). Worst case: duplicate allocation (better than rejecting a call). |

**Key detail:** The 30s TTL on the lock is a safety net. If the allocator crashes after locking but before storing, the key expires and the next retry can proceed.

**Redis key:** `voice:call:{callSID}` — HGETALL (read) or HSET+EXPIRE (write)

---

### Step 3: Get Merchant Config — `GetMerchantConfig()` (merchant.go:19-38)

```go
merchantConfig, err := GetMerchantConfig(ctx, a.redis, merchantID)
```

Looks up the merchant's allocation preferences from the Redis hash:

```
HGET voice:merchant:config {merchantID}
```

**Returns** a `MerchantConfig` struct:
```go
type MerchantConfig struct {
    Tier     string   `json:"tier"`               // Legacy, not used in chain
    Pool     string   `json:"pool,omitempty"`      // Dedicated pool name
    Fallback []string `json:"fallback,omitempty"`  // Custom fallback chain
}
```

**Edge cases:**
- Empty `merchantID` → returns empty config (no Redis call)
- Merchant not in hash (`redis.Nil`) → returns empty config
- Malformed JSON → returns error (logged as warning, empty config used)
- Redis error → returns error (logged as warning, empty config used)

In all error cases, allocation continues with the system-wide DefaultChain.

**Redis key READ:** `voice:merchant:config` (HASH, field = merchantID)

---

### Step 4: Resolve Fallback Chain — `resolveFallbackChain()` (allocator.go:150-169)

Builds the ordered list of pools to try:

```go
func (a *Allocator) resolveFallbackChain(mc MerchantConfig) []string
```

**Resolution logic:**

1. **Base chain:** If merchant has custom `Fallback` → use it. Otherwise → `config.DefaultChain`.
2. **Prepend dedicated pool:** If merchant has `Pool` set → prepend `"merchant:{pool}"` to the chain.

**Examples:**

| Merchant Config | Resulting Chain |
|----------------|-----------------|
| Empty (no merchant) | `["gold", "standard", "basic"]` (DefaultChain) |
| `{Pool: "9shines"}` | `["merchant:9shines", "gold", "standard", "basic"]` |
| `{Fallback: ["standard", "basic"]}` | `["standard", "basic"]` |
| `{Pool: "acme", Fallback: ["gold"]}` | `["merchant:acme", "gold"]` |

**No Redis calls.** Pure in-memory logic using config + merchant config.

---

### Step 5: Walk the Chain — `tryAllocateFromStep()` (allocator.go:173-201)

For each step in the chain, attempts allocation:

```go
for _, step := range chain {
    podName, sourcePool, err = a.tryAllocateFromStep(ctx, step)
    if err == nil && podName != "" {
        break
    }
}
```

Each step string is parsed to determine the pool type:

```
┌─────────────────┐
│ step string      │
└────────┬────────┘
         │
    ┌────▼─────┐
    │ Starts    │── YES ──► Merchant dedicated pool (exclusive)
    │ with      │           poolKey = "voice:merchant:{id}:pods"
    │"merchant:"│           sourcePool = "merchant:{id}"
    └────┬─────┘
         │ NO
    ┌────▼───────────┐
    │ Look up tier   │
    │ config          │
    └────┬───────────┘
         │
    ┌────▼─────┐
    │ Type?     │── "shared" ──► tryAllocateShared()
    │           │                poolKey = "voice:pool:{tier}:available" (ZSET)
    │           │── "exclusive"► tryAllocateExclusive()
    └──────────┘                 poolKey = "voice:pool:{tier}:available" (SET)
                                 sourcePool = "pool:{tier}"
```

**If step is unknown tier:** Logs debug message, returns error, moves to next step.

---

### Step 5a: Exclusive Allocation — `tryAllocateExclusive()` (exclusive.go:12-46)

For **exclusive pools** (1-pod-1-call) — both regular tier pools and merchant dedicated pools.

```go
func tryAllocateExclusive(ctx context.Context, client *redis.Client, poolKey, drainingPrefix string) (string, error)
```

**Algorithm — up to 10 attempts:**

```
for i := 0; i < 10; i++ {
    1. SPOP poolKey           → atomically remove random member from SET
       - redis.Nil? → pool empty, return ErrNoPodsAvailable
       - error? → return error

    2. EXISTS voice:pod:draining:{podName}
       - error? → SADD pod back to pool, return error
       - exists (draining)? → skip pod (do NOT return to pool), try again
       - not exists? → return podName ✅
}
// All 10 attempts hit draining pods
return ErrDrainingPod
```

**Critical design decisions:**

1. **SPOP is atomic** — no race between checking availability and removing from pool. Two concurrent allocators cannot get the same pod.

2. **Draining pods are discarded, not returned** — if a pod is SPOP'd and found to be draining, it's silently dropped. The reconciler will clean it up later (it won't be in the available pool anymore, which is correct for draining pods).

3. **10 attempts max** — prevents infinite loop if all pods in the pool are draining. With 3 production pods, 10 attempts is very generous.

4. **Redis error on draining check** — pod is returned to pool (`SADD` back). Conservative: don't allocate a pod we can't verify.

**Redis operations per attempt:**

| Operation | Key | Type |
|-----------|-----|------|
| SPOP | `voice:pool:{tier}:available` or `voice:merchant:{id}:pods` | SET |
| EXISTS | `voice:pod:draining:{podName}` | STRING |
| SADD (on redis error only) | same pool key | SET |

---

### Step 5b: Shared Allocation — `tryAllocateShared()` (shared.go:55-80)

For **shared pools** (multi-call per pod, e.g. basic tier with `max_concurrent=3`).

```go
func tryAllocateShared(ctx context.Context, client *redis.Client, tier string, maxConcurrent int) (string, error)
```

Uses a **Lua script** for atomic check-and-increment on the ZSET:

```lua
local pool_key = KEYS[1]           -- "voice:pool:{tier}:available"
local max_concurrent = tonumber(ARGV[1])

-- Get ALL pods sorted by score ascending (least connections first)
local result = redis.call('ZRANGE', pool_key, 0, -1, 'WITHSCORES')
if #result == 0 then
    return nil                       -- Pool empty
end

-- Walk pods in order of ascending score
for i = 1, #result, 2 do
    local pod_name = result[i]
    local current_score = tonumber(result[i + 1])

    if current_score >= max_concurrent then
        return nil                   -- This and all subsequent are at capacity
    end

    -- Check draining
    if redis.call('EXISTS', "voice:pod:draining:" .. pod_name) == 0 then
        redis.call('ZINCRBY', pool_key, 1, pod_name)   -- Increment score
        return pod_name              -- ✅ Allocated
    end
end

return nil                           -- All draining or at capacity
```

**Key design decisions:**

1. **Entire script is atomic** — Redis Lua scripts run single-threaded. No concurrent allocator can interfere between the ZRANGE check and ZINCRBY increment.

2. **Sorted by score ascending** — pods with fewer active calls are tried first (load balancing).

3. **Early exit on capacity** — once we hit a pod at `max_concurrent`, all subsequent pods have equal or higher scores, so we stop.

4. **Draining check inline** — each pod is checked for draining within the same atomic script.

5. **`maxConcurrent` default = 5** — if config is 0 or negative (shared.go:56-58).

**Redis operations (all inside Lua, atomic):**

| Operation | Key | Type |
|-----------|-----|------|
| ZRANGE ... WITHSCORES | `voice:pool:{tier}:available` | ZSET |
| EXISTS (per pod) | `voice:pod:draining:{podName}` | STRING |
| ZINCRBY +1 (on match) | `voice:pool:{tier}:available` | ZSET |

**ZSET score semantics:**
- Score = number of active calls on this pod
- Score 0 → idle pod
- Score 3 (with max_concurrent=3) → full pod

---

### Step 6: Store Allocation — `storeAllocation()` (allocator.go:241-287)

Once a pod is found, the allocation is persisted to Redis:

```go
func (a *Allocator) storeAllocation(ctx context.Context, callSID, podName, sourcePool, merchantID string, allocatedAt time.Time) error
```

**Four Redis writes:**

#### 6a. Store Call Info Hash (allocator.go:246-254)

```
HSET voice:call:{callSID}
    pod_name     = "voice-agent-0"
    source_pool  = "pool:gold"
    merchant_id  = "9shines"
    allocated_at = "1739280000"
```

This **overwrites** the lock placeholder from the idempotency check. The call hash now contains real allocation data.

#### 6b. Remove Lock Field (allocator.go:257)

```
HDEL voice:call:{callSID} _lock
```

Removes the `_lock` field that was set during idempotency. The hash now only contains real data.

#### 6c. Set Call Info TTL (allocator.go:260)

```
EXPIRE voice:call:{callSID} {CallInfoTTL}
```

Default: 1 hour (configurable via `CALL_INFO_TTL` env var). Replaces the 30s lock TTL with the proper call TTL. This key is used by the release flow to find which pod a call is on.

#### 6d. Update Pod Info Hash (allocator.go:265-275)

```
HSET voice:pod:{podName}
    status             = "allocated"
    allocated_call_sid = "{callSID}"
    allocated_at       = "1739280000"
    source_pool        = "pool:gold"
```

Records current state of the pod. Used by status endpoints and zombie cleanup.

#### 6e. Create Lease (allocator.go:281)

```
SET voice:lease:{podName} {callSID} EX {LeaseTTL}
```

Default: 15 minutes (configurable via `LEASE_TTL` env var).

**The lease is the heartbeat of an active call.** Zombie cleanup uses it to determine if a pod is legitimately busy:
- Lease exists → pod has active call, don't touch
- Lease expired + pod in assigned set but not in available set → zombie, recover it

**WARNING:** If `LeaseTTL` is shorter than the longest expected call, the lease expires mid-call, and zombie cleanup will recover the pod → potential double allocation. The default 15 minutes should cover most calls.

**Redis keys WRITTEN:**

| Key | Type | TTL | Operation |
|-----|------|-----|-----------|
| `voice:call:{callSID}` | HASH | CallInfoTTL (1hr) | HSET, HDEL, EXPIRE |
| `voice:pod:{podName}` | HASH | None | HSET |
| `voice:lease:{podName}` | STRING | LeaseTTL (15min) | SET EX |

---

### Step 6 Error: Rollback — `returnPodToPool()` (allocator.go:291-323)

If `storeAllocation()` fails (Redis write error), the pod is returned to its source pool to avoid leaking it:

```go
func (a *Allocator) returnPodToPool(ctx context.Context, podName, sourcePool string)
```

Parses `sourcePool` (format `"type:id"`) and reverses the allocation:

| Source Pool | Rollback Action |
|------------|-----------------|
| `"merchant:{id}"` | `SADD voice:merchant:{id}:pods {podName}` |
| `"pool:{tier}"` (exclusive) | `SADD voice:pool:{tier}:available {podName}` |
| `"pool:{tier}"` (shared) | `ZINCRBY voice:pool:{tier}:available -1 {podName}` |

**Best-effort:** If the rollback itself fails, zombie cleanup will eventually recover the pod (it will be in the assigned set but not in available, with no lease).

---

### Step 7: Build Result + Metrics (allocator.go:122-138)

```go
result := &models.AllocationResult{
    PodName:     podName,
    WSURL:       a.buildWSURL(podName, provider, flow, template),
    SourcePool:  sourcePool,
    AllocatedAt: now,
    WasExisting: false,
}

middleware.AllocationsTotal.WithLabelValues(sourcePool, "success").Inc()
middleware.ActiveCalls.Inc()
```

**Metrics updated:**
- `allocations_total{source_pool="pool:gold", result="success"}` — counter
- `active_calls` — gauge (incremented)

On failure paths:
- `allocations_total{source_pool="", result="no_pods"}` — no pods available
- `allocations_total{source_pool="pool:gold", result="storage_error"}` — storage failed

---

## WebSocket URL Construction — `buildWSURL()` (allocator.go:210-237)

Builds the URL that the telephony provider will connect to:

```
{baseURL}/ws/pod/{podName}/agent/voice/breeze-buddy/{provider}/callback/{template}[/v2]
```

**Example:**
```
wss://clairvoyance.breezelabs.app/ws/pod/voice-agent-0/agent/voice/breeze-buddy/twilio/callback/order-confirmation/v2
```

**URL segments explained:**

| Segment | Purpose |
|---------|---------|
| `wss://clairvoyance.breezelabs.app` | `VOICE_AGENT_BASE_URL` config (domain handled by nginx) |
| `/ws/pod/voice-agent-0` | Nginx uses this to route to the specific pod |
| `/agent/voice/breeze-buddy` | FastAPI app mount prefix on the voice-agent |
| `/twilio/callback/order-confirmation` | Provider-specific WebSocket route |
| `/v2` | Appended when `flow == "v2"` |

**Defaults:**

| Parameter | Default Value | Exception |
|-----------|--------------|-----------|
| `provider` | `"twilio"` | — |
| `flow` | `"v2"` | — |
| `template` | `"order-confirmation"` | `"template"` when provider is `"exotel"` |

**Nginx strips** `/ws/pod/{podName}` and forwards the remainder to the target pod's service. See [14 - Nginx Routing](./14-nginx-routing.md) for details.

---

## Complete Sequence Diagram

```
Telephony Provider          Smart Router (any replica)           Redis
       │                            │                              │
       │  POST /api/v1/allocate     │                              │
       │  {call_sid, merchant_id}   │                              │
       ├───────────────────────────►│                              │
       │                            │                              │
       │                            │  EVAL checkAndLockScript     │
       │                            │  KEYS: voice:call:{sid}      │
       │                            ├─────────────────────────────►│
       │                            │                              │
       │                            │  empty array (lock acquired) │
       │                            │◄─────────────────────────────┤
       │                            │                              │
       │                            │  HGET voice:merchant:config  │
       │                            │  field: {merchantID}         │
       │                            ├─────────────────────────────►│
       │                            │                              │
       │                            │  merchant config JSON        │
       │                            │◄─────────────────────────────┤
       │                            │                              │
       │                  resolveFallbackChain()                   │
       │                  chain = ["gold","standard","basic"]      │
       │                            │                              │
       │                  ── try "gold" (exclusive) ──             │
       │                            │                              │
       │                            │  SPOP voice:pool:gold:avail  │
       │                            ├─────────────────────────────►│
       │                            │                              │
       │                            │  "voice-agent-0"             │
       │                            │◄─────────────────────────────┤
       │                            │                              │
       │                            │  EXISTS voice:pod:draining:  │
       │                            │         voice-agent-0        │
       │                            ├─────────────────────────────►│
       │                            │                              │
       │                            │  0 (not draining)            │
       │                            │◄─────────────────────────────┤
       │                            │                              │
       │                  ── pod found! store allocation ──        │
       │                            │                              │
       │                            │  HSET voice:call:{sid}       │
       │                            │    pod_name, source_pool...  │
       │                            ├─────────────────────────────►│
       │                            │                              │
       │                            │  HDEL voice:call:{sid} _lock │
       │                            ├─────────────────────────────►│
       │                            │                              │
       │                            │  EXPIRE voice:call:{sid} 1hr │
       │                            ├─────────────────────────────►│
       │                            │                              │
       │                            │  HSET voice:pod:voice-agent-0│
       │                            │    status=allocated...       │
       │                            ├─────────────────────────────►│
       │                            │                              │
       │                            │  SET voice:lease:voice-agent │
       │                            │    -0 {sid} EX 900           │
       │                            ├─────────────────────────────►│
       │                            │                              │
       │  200 OK                    │                              │
       │  {pod_name, ws_url, ...}   │                              │
       │◄───────────────────────────┤                              │
       │                            │                              │
       │  WSS connect to ws_url     │                              │
       ├───────────────────────────────────────(via nginx)────────►│
                                                         voice-agent pod
```

---

## Redis Keys — Complete Reference

### Read

| Key | Type | When | Purpose |
|-----|------|------|---------|
| `voice:call:{callSID}` | HASH | Idempotency check | Check existing allocation |
| `voice:merchant:config` | HASH (field=merchantID) | Merchant lookup | Get merchant pool/fallback config |
| `voice:pool:{tier}:available` | SET (exclusive) | SPOP during allocation | Get available pod |
| `voice:pool:{tier}:available` | ZSET (shared) | Lua ZRANGE during allocation | Find pod with capacity |
| `voice:merchant:{id}:pods` | SET | SPOP during allocation | Get merchant dedicated pod |
| `voice:pod:draining:{pod}` | STRING | EXISTS check | Verify pod not draining |

### Write

| Key | Type | TTL | When | Purpose |
|-----|------|-----|------|---------|
| `voice:call:{callSID}` | HASH | 30s (lock) then 1hr (real) | Idempotency lock + store | Call→pod mapping |
| `voice:pod:{podName}` | HASH | None | After allocation | Pod status tracking |
| `voice:lease:{podName}` | STRING | 15min (LeaseTTL) | After allocation | Active call indicator |
| `voice:pool:{tier}:available` | SET | None | SPOP removes (exclusive) | Pod removed from available |
| `voice:pool:{tier}:available` | ZSET | None | ZINCRBY +1 (shared) | Score incremented |

### Delete (on rollback only)

| Key | Type | When | Purpose |
|-----|------|------|---------|
| `voice:pool:{tier}:available` | SET | SADD on storage failure | Return pod to exclusive pool |
| `voice:pool:{tier}:available` | ZSET | ZINCRBY -1 on storage failure | Decrement shared pool score |
| `voice:merchant:{id}:pods` | SET | SADD on storage failure | Return pod to merchant pool |

---

## Edge Cases

1. **Duplicate call SID (idempotency hit):** Returns existing allocation immediately with fresh WSURL. No new pod allocated. `WasExisting: true` in response.

2. **Lock race condition:** Two concurrent requests for the same call SID. First one acquires the lock. Second one sees only the `_lock` field (no `pod_name`), treats it as not-yet-allocated, and proceeds. Worst case: both allocate different pods for the same call. The release flow + zombie cleanup handle the orphan.

3. **Idempotency Redis error:** Falls through (allocator.go:66-69). Proceeds with allocation. Rationale: rejecting a call because of a transient Redis error is worse than a potential duplicate allocation.

4. **Merchant config error:** Uses empty config → falls back to system DefaultChain. Call still gets a pod.

5. **All pods draining in exclusive pool:** After 10 SPOP attempts all hit draining pods → `ErrDrainingPod`. Falls through to next tier in chain. If entire chain exhausted → `ErrNoPodsAvailable` (503).

6. **Shared pool at capacity:** Lua script checks all pods, all at `max_concurrent` → returns nil → `ErrNoPodsAvailable` for this step. Falls through to next tier.

7. **Storage failure after allocation:** Pod is returned to its source pool via `returnPodToPool()`. If rollback also fails, zombie cleanup recovers the pod (it's in the assigned set with no lease).

8. **Pod allocated but draining key set between SPOP and EXISTS:** Pod is correctly skipped. Loop retries with next SPOP.

9. **Shared pool: pod removed (died) between ZRANGE and ZINCRBY:** The ZINCRBY will create the member with score 1. This is harmless — the reconciler will remove it on the next sync when it sees the pod no longer exists in K8s.

10. **Empty chain (no tiers configured):** Loop doesn't execute, `podName` is empty → returns `ErrNoPodsAvailable`.

---

## Production Walkthrough

Given production config:
```
DefaultChain: ["gold", "standard", "basic"]
gold:     exclusive, target=1
standard: exclusive, target=1
basic:    shared, target=1, max_concurrent=3
```

**Scenario: Normal call from merchant "9shines" (no custom config)**

1. `CheckAndLockAllocation("CA123")` → lock acquired
2. `GetMerchantConfig("9shines")` → `redis.Nil` → empty config
3. `resolveFallbackChain({})` → `["gold", "standard", "basic"]`
4. Try "gold":
   - `SPOP voice:pool:gold:available` → "voice-agent-0"
   - `EXISTS voice:pod:draining:voice-agent-0` → 0 (not draining)
   - Pod found!
5. `storeAllocation("CA123", "voice-agent-0", "pool:gold", "9shines", now)`
6. Return `{pod: "voice-agent-0", ws_url: "wss://...", source_pool: "pool:gold"}`

**Scenario: Gold pool empty, standard pool empty**

1-3. Same as above
4. Try "gold": `SPOP` → `redis.Nil` → pool empty, next
5. Try "standard": `SPOP` → `redis.Nil` → pool empty, next
6. Try "basic" (shared):
   - Lua: ZRANGE voice:pool:basic:available → `[("voice-agent-2", 1)]` (1 active call)
   - 1 < 3 (max_concurrent) → check draining → not draining → ZINCRBY +1
   - Pod found: "voice-agent-2" (now score 2)
7. Store, return with `source_pool: "pool:basic"`

**Scenario: Merchant with dedicated pool**

Merchant config in Redis:
```json
{"pool": "acme-corp", "fallback": ["standard"]}
```

1-2. Same, but `GetMerchantConfig` returns `{Pool: "acme-corp", Fallback: ["standard"]}`
3. `resolveFallbackChain()` → `["merchant:acme-corp", "standard"]`
4. Try "merchant:acme-corp":
   - `SPOP voice:merchant:acme-corp:pods` → "voice-agent-5"
   - `EXISTS voice:pod:draining:voice-agent-5` → 0
   - Pod found!
5. Store with `source_pool: "merchant:acme-corp"`

---

## Interaction with Other Flows

| Flow | Relationship |
|------|-------------|
| [02 - Configuration](./02-configuration.md) | `DefaultChain`, `ParsedTierConfig`, `LeaseTTL`, `CallInfoTTL`, `VoiceAgentBaseURL` all drive allocation behavior |
| [06 - Tier Assignment](./06-tier-assignment.md) | Determines which pool each pod is in → directly affects which chain step finds it |
| [08 - Release](./08-release.md) | Reverse of allocation: uses `voice:call:{sid}` to find pod, returns it to pool |
| [09 - Drain](./09-drain.md) | Sets `voice:pod:draining:{pod}` → allocation skips draining pods |
| [10 - Zombie Cleanup](./10-zombie-cleanup.md) | Recovers pods with expired leases (safety net for allocation) |
| [12 - HTTP Layer](./12-http-layer.md) | All 4 handler endpoints feed into `Allocate()` |
| [14 - Nginx Routing](./14-nginx-routing.md) | Routes WebSocket connections using the `/ws/pod/{podName}` prefix in WSURL |
