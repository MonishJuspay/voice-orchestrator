# Flow 08: Pod Release

**Source files:**
- `internal/releaser/releaser.go` (195 lines) — Core `Release()`, pool routing, lease handling
- `internal/releaser/exclusive.go` (33 lines) — `releaseToExclusivePool()` — draining check + SADD
- `internal/releaser/shared.go` (55 lines) — `releaseToSharedPool()` — Lua ZINCRBY -1 with floor
- `internal/releaser/types.go` (23 lines) — Interface + `ErrCallNotFound`
- `internal/api/handlers/release.go` (69 lines) — HTTP handler

## Overview

Release is the reverse of allocation — when a call ends, the voice-agent pod notifies the smart router, which returns the pod to its source pool so it can serve new calls. The releaser looks up the call SID in Redis to find which pod and pool were assigned, handles the pool return (exclusive SADD or shared ZINCRBY -1), cleans up the lease and call info, and decrements the active calls gauge.

Like allocation, release runs on **all smart-router replicas** (stateless).

## Entry Point — HTTP Handler

```
POST /api/v1/release
Content-Type: application/json

{
    "call_sid": "CA123..."
}
```

### Handler — `Handle()` (release.go:31-68)

Simple JSON decode + validation:
1. Decode `{"call_sid": "..."}` from request body
2. Validate `call_sid` is not empty → 400 if missing
3. Call `releaser.Release(ctx, callSID)`
4. Return result or error

Error responses:
- `400` — missing `call_sid` or invalid body
- `404` — `ErrCallNotFound` (call SID not in Redis)
- `500` — internal error

Success response:
```json
{
    "success": true,
    "pod_name": "voice-agent-0",
    "released_to_pool": "pool:gold",
    "was_draining": false
}
```

---

## Core Release — `Release()` (releaser.go:43-166)

### Step-by-Step Walkthrough

```
                    ┌──────────────────────────┐
                    │  POST /api/v1/release     │
                    │  {call_sid: "CA123"}      │
                    └────────────┬─────────────┘
                                 │
                    ┌────────────▼─────────────┐
                    │ 1. HGETALL voice:call:sid │
                    │    → pod_name, source_pool│
                    └────────────┬─────────────┘
                           found?
                        ┌───┴───┐
                       no      yes
                       │        │
                  404 error  ┌──▼──────────────┐
                             │ 2. Parse source  │
                             │    pool + check  │
                             │    draining       │
                             └──────┬──────────┘
                              draining?
                           ┌───┴───┐
                          yes      no
                           │        │
                    skip pool  ┌────▼──────────┐
                    return     │ 3. Return pod  │
                           │   │   to pool      │
                           │   └────┬──────────┘
                           │        │
                           └───┬────┘
                               │
                      ┌────────▼──────────┐
                      │ 4. Handle lease    │
                      │    (delete or keep)│
                      └────────┬──────────┘
                               │
                      ┌────────▼──────────┐
                      │ 5. Update pod info │
                      │    status          │
                      └────────┬──────────┘
                               │
                      ┌────────▼──────────┐
                      │ 6. Delete call info│
                      │    + dec ActiveCalls│
                      └───────────────────┘
```

---

### Step 1: Look Up Call Info (releaser.go:47-64)

```go
callKey := redisclient.CallInfoKey(callSID)    // "voice:call:{callSID}"
callInfo, err := client.HGetAll(ctx, callKey).Result()
```

Reads the call hash stored during allocation:

```
HGETALL voice:call:{callSID}
→ {
    "pod_name":     "voice-agent-0",
    "source_pool":  "pool:gold",
    "merchant_id":  "9shines",
    "allocated_at": "1739280000"
  }
```

**If empty:** Returns `ErrCallNotFound` (404). This happens if:
- The call SID was never allocated
- The call info TTL (1hr) expired
- A duplicate release was called (call info already deleted)

**Validation:** Both `pod_name` and `source_pool` must be non-empty, otherwise returns error.

**Redis key READ:** `voice:call:{callSID}` (HASH)

---

### Step 2: Check Draining Status (releaser.go:67-73)

```go
drainingKey := redisclient.DrainingKey(podName)    // "voice:pod:draining:{podName}"
isDraining, err := client.Exists(ctx, drainingKey).Result()
wasDraining := isDraining > 0
```

**If draining:** The pod is being drained (shutting down). The release **skips returning the pod to the pool** — the pod should not receive new calls. The rest of the cleanup (lease, pod info, call info) still happens.

**Redis key READ:** `voice:pod:draining:{podName}` (STRING, EXISTS)

---

### Step 3: Return Pod to Pool (releaser.go:78-110)

The `sourcePool` string (stored during allocation) determines where the pod goes back:

```go
tier, isMerchant := parseSourcePool(sourcePool)
```

**`parseSourcePool()`** (releaser.go:173-182):

| Source Pool String | Parsed Result | Pool Type |
|-------------------|---------------|-----------|
| `"merchant:9shines"` | `tier="9shines", isMerchant=true` | Merchant dedicated |
| `"pool:gold"` | `tier="gold", isMerchant=false` | Regular (check config for exclusive/shared) |
| `"pool:basic"` | `tier="basic", isMerchant=false` | Regular (check config for exclusive/shared) |
| `"gold"` (fallback) | `tier="gold", isMerchant=false` | Treated as tier name directly |

#### 3a: Exclusive Pool Return — `releaseToExclusivePool()` (exclusive.go:13-32)

For exclusive tier pools and merchant dedicated pools:

```go
func releaseToExclusivePool(ctx context.Context, redis *redis.Client, poolKey, podName string) error
```

```
1. EXISTS voice:pod:draining:{podName}
   - error → return error
   - exists (draining) → return nil (don't add back)

2. SADD {poolKey} {podName}
   - poolKey = "voice:pool:{tier}:available" or "voice:merchant:{id}:pods"
```

**Note:** This is a **second draining check** — the first was in `Release()` at step 2. This provides defense-in-depth: even if a race condition sets the draining key between step 2 and step 3, the pod won't be added back.

**Redis operations:**

| Operation | Key | Type |
|-----------|-----|------|
| EXISTS | `voice:pod:draining:{podName}` | STRING |
| SADD | `voice:pool:{tier}:available` or `voice:merchant:{id}:pods` | SET |

#### 3b: Shared Pool Return — `releaseToSharedPool()` (shared.go:38-54)

For shared tier pools (e.g. "basic"):

```go
func releaseToSharedPool(ctx context.Context, redis *redis.Client, poolKey, podName string) (int64, error)
```

Uses a **Lua script** for atomic decrement with floor:

```lua
local pool_key = KEYS[1]      -- "voice:pool:{tier}:available"
local pod_name = ARGV[1]

-- Get current score
local current_score = redis.call('ZSCORE', pool_key, pod_name)
if current_score == false then
    return -1                   -- Pod not in ZSET (removed or never added)
end

local score = tonumber(current_score)

if score > 0 then
    local new_score = redis.call('ZINCRBY', pool_key, -1, pod_name)
    return tonumber(new_score)
else
    return 0                    -- Already at 0, don't go negative
end
```

**Returns:** New score (number of remaining active calls on this pod), or -1 if pod not in ZSET.

**Key design decisions:**

1. **Floor at 0** — prevents negative scores from race conditions or duplicate releases.
2. **ZSCORE check first** — if pod was removed from the ZSET (reconciler removed it), returns -1 without modifying anything.
3. **Atomic** — Lua script prevents concurrent releases from going below 0.

**Redis operations (all inside Lua, atomic):**

| Operation | Key | Type |
|-----------|-----|------|
| ZSCORE | `voice:pool:{tier}:available` | ZSET |
| ZINCRBY -1 | `voice:pool:{tier}:available` | ZSET |

---

### Step 4: Handle Lease (releaser.go:112-129)

The lease (`voice:lease:{podName}`) is the active-call indicator used by zombie cleanup. Its handling depends on the pool type:

```go
func shouldDeleteLease(isShared bool, newScore int64) bool {
    if !isShared {
        return true         // Exclusive: always delete
    }
    return newScore <= 0    // Shared: only delete if no calls left
}
```

| Pool Type | Condition | Action |
|-----------|-----------|--------|
| Exclusive | Always | `DEL voice:lease:{podName}` |
| Shared, newScore > 0 | Still has active calls | **Keep lease** (other calls still running) |
| Shared, newScore <= 0 | No more active calls | `DEL voice:lease:{podName}` |
| Draining | Always | `DEL voice:lease:{podName}` (falls through, `isShared=false`) |

**Why keep the lease for shared pools with active calls?** If we deleted the lease while other calls are still running, zombie cleanup would see the pod as orphaned and try to recover it — but the pod is legitimately serving calls.

**Redis key DELETE:** `voice:lease:{podName}` (conditionally)

---

### Step 5: Update Pod Info (releaser.go:131-149)

```go
podKey := redisclient.PodInfoKey(podName)    // "voice:pod:{podName}"
status := "available"
if wasDraining {
    status = "draining"
}

update := map[string]interface{}{
    "status":             status,
    "allocated_call_sid": "",
    "allocated_at":       "",
    "released_at":        now,
}
client.HSet(ctx, podKey, update)
```

Clears the allocation fields and sets `released_at` timestamp. Status is either `"available"` or `"draining"` depending on whether the pod was being drained.

**Best-effort:** Error is logged but doesn't fail the release. The critical work (pool return + lease) already happened.

**Redis key WRITE:** `voice:pod:{podName}` (HASH, HSET)

---

### Step 6: Delete Call Info + Metrics (releaser.go:151-158)

```go
client.Del(ctx, callKey)                                    // DEL voice:call:{callSID}
middleware.ReleasesTotal.WithLabelValues(sourcePool, "success").Inc()
middleware.ActiveCalls.Dec()
```

Removes the call→pod mapping from Redis. This prevents the idempotency check from returning stale data for a reused call SID (unlikely but defensive).

**Best-effort:** Error is logged but doesn't fail the release.

**Metrics updated:**
- `releases_total{source_pool="pool:gold", result="success"}` — counter
- `active_calls` — gauge (decremented)

**Redis key DELETE:** `voice:call:{callSID}` (HASH)

---

## Complete Sequence Diagram

```
Voice Agent Pod          Smart Router (any replica)           Redis
       │                            │                           │
       │  POST /api/v1/release      │                           │
       │  {call_sid: "CA123"}       │                           │
       ├───────────────────────────►│                           │
       │                            │                           │
       │                            │  HGETALL voice:call:CA123 │
       │                            ├──────────────────────────►│
       │                            │  {pod_name: "voice-agent- │
       │                            │   0", source_pool:        │
       │                            │   "pool:gold", ...}       │
       │                            │◄──────────────────────────┤
       │                            │                           │
       │                            │  EXISTS voice:pod:draining│
       │                            │         :voice-agent-0    │
       │                            ├──────────────────────────►│
       │                            │  0 (not draining)         │
       │                            │◄──────────────────────────┤
       │                            │                           │
       │              parseSourcePool("pool:gold")              │
       │              → tier="gold", isMerchant=false           │
       │              → config: exclusive                       │
       │                            │                           │
       │                            │  EXISTS voice:pod:draining│
       │                            │         :voice-agent-0    │
       │                            ├──────────────────────────►│
       │                            │  0 (still not draining)   │
       │                            │◄──────────────────────────┤
       │                            │                           │
       │                            │  SADD voice:pool:gold:    │
       │                            │       available           │
       │                            │       "voice-agent-0"     │
       │                            ├──────────────────────────►│
       │                            │  1 (added)                │
       │                            │◄──────────────────────────┤
       │                            │                           │
       │                            │  DEL voice:lease:         │
       │                            │      voice-agent-0        │
       │                            ├──────────────────────────►│
       │                            │                           │
       │                            │  HSET voice:pod:          │
       │                            │       voice-agent-0       │
       │                            │    status=available       │
       │                            ├──────────────────────────►│
       │                            │                           │
       │                            │  DEL voice:call:CA123     │
       │                            ├──────────────────────────►│
       │                            │                           │
       │  200 OK                    │                           │
       │  {success, pod_name,       │                           │
       │   released_to_pool, ...}   │                           │
       │◄───────────────────────────┤                           │
```

---

## Redis Keys — Complete Reference

### Read

| Key | Type | When | Purpose |
|-----|------|------|---------|
| `voice:call:{callSID}` | HASH | Step 1 | Look up pod + source pool for this call |
| `voice:pod:draining:{podName}` | STRING | Step 2 + Step 3a | Check if pod is being drained (checked twice) |

### Write

| Key | Type | When | Purpose |
|-----|------|------|---------|
| `voice:pool:{tier}:available` | SET | Step 3a (exclusive) | SADD pod back to available pool |
| `voice:pool:{tier}:available` | ZSET | Step 3b (shared) | ZINCRBY -1 to decrement active count |
| `voice:merchant:{id}:pods` | SET | Step 3a (merchant) | SADD pod back to merchant pool |
| `voice:pod:{podName}` | HASH | Step 5 | Update status to "available"/"draining" |

### Delete

| Key | Type | When | Purpose |
|-----|------|------|---------|
| `voice:lease:{podName}` | STRING | Step 4 | Remove active-call indicator |
| `voice:call:{callSID}` | HASH | Step 6 | Remove call→pod mapping |

---

## Edge Cases

1. **Call SID not found:** Returns `ErrCallNotFound` (404). Can happen if call info TTL (1hr) expired, or duplicate release. No side effects.

2. **Duplicate release (same call SID twice):** First release succeeds and deletes call info. Second release gets empty HGETALL → 404. The first release already returned the pod to the pool. No double-return.

3. **Pod is draining:** Release still cleans up call info, lease, and pod info. But the pod is **not** returned to the available pool. The drain flow handles the pod's lifecycle from here.

4. **Draining key set between step 2 and step 3:** The `releaseToExclusivePool()` function does a **second draining check** before SADD. If draining was set in this window, the pod is correctly skipped. For shared pools, this race is not checked — the ZINCRBY -1 still runs. This is acceptable because the shared pool's reconciler/zombie flow will handle cleanup.

5. **Shared pool: pod removed from ZSET:** Lua script ZSCORE returns false → returns -1. Release still succeeds (lease deleted, call info cleaned), but no pool modification. Pod is already gone from the pool.

6. **Shared pool: score already 0:** Lua script doesn't decrement below 0 (floor). Returns 0. Lease is deleted (newScore <= 0).

7. **Redis error on non-critical steps:** Steps 5 (pod info update) and 6 (call info delete) are best-effort — errors are logged but the release still returns success. The critical operations (pool return + lease delete) happen first.

8. **Redis error on pool return:** Returns error to caller (500). Lease and call info are NOT cleaned up. This is conservative — if we can't return the pod to the pool, we leave all state intact. Zombie cleanup can recover later.

---

## Allocation ↔ Release Symmetry

| What Allocation Does | What Release Undoes |
|---------------------|---------------------|
| SPOP from exclusive pool | SADD back to exclusive pool |
| ZINCRBY +1 on shared pool | ZINCRBY -1 on shared pool |
| HSET voice:call:{sid} | DEL voice:call:{sid} |
| HSET voice:pod:{pod} status=allocated | HSET voice:pod:{pod} status=available |
| SET voice:lease:{pod} (with TTL) | DEL voice:lease:{pod} |
| ActiveCalls.Inc() | ActiveCalls.Dec() |

---

## Interaction with Other Flows

| Flow | Relationship |
|------|-------------|
| [07 - Allocation](./07-allocation.md) | Creates the state that release cleans up (call info, pod info, lease, pool removal) |
| [09 - Drain](./09-drain.md) | Sets draining key → release skips pool return for draining pods |
| [10 - Zombie Cleanup](./10-zombie-cleanup.md) | Safety net — if release never fires (e.g. crash), zombie cleanup recovers the pod |
| [12 - HTTP Layer](./12-http-layer.md) | `POST /api/v1/release` handler feeds into `Release()` |
