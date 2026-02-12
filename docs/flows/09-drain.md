# Flow 09: Pod Drain

**Source files:**
- `internal/drainer/drainer.go` (191 lines) — Core `Drain()`, pool removal, rollback
- `internal/api/handlers/drain.go` (74 lines) — HTTP handler

## Overview

Drain is the graceful shutdown mechanism for voice-agent pods during rolling updates. When a pod needs to be replaced (update, scale-down, manual restart), it's drained first: removed from the available pool so no new calls are routed to it, and marked with a `voice:pod:draining:{pod}` key that tells the allocator, releaser, and zombie cleanup to treat this pod specially.

**Critical invariant:** Active calls on a draining pod are allowed to complete. The pod is only actually killed by Kubernetes after the preStop hook finishes and the termination grace period expires.

Like allocation and release, drain runs on **all smart-router replicas** (stateless).

## Entry Point — HTTP Handler

```
POST /api/v1/drain
Content-Type: application/json

{
    "pod_name": "voice-agent-0"
}
```

### Handler — `Handle()` (drain.go:37-73)

1. Decode `{"pod_name": "..."}` from request body
2. Validate `pod_name` is not empty → 400 if missing
3. Call `drainer.Drain(ctx, podName)`
4. Increment `drains_total` metric
5. Return result or error

**Who calls this endpoint?** The voice-agent pod's Kubernetes `preStop` hook. In the smart-router deployment manifest (`k8s/deployment.yaml`), the preStop lifecycle hook runs:
```
curl -X POST http://smart-router:8080/api/v1/drain -H 'Content-Type: application/json' -d '{"pod_name": "<pod-name>"}'
```

Success response:
```json
{
    "success": true,
    "pod_name": "voice-agent-0",
    "has_active_call": true,
    "message": "Pod voice-agent-0 is draining with active call in progress. Will complete when call ends."
}
```

Error responses:
- `400` — missing `pod_name` or invalid body
- `500` — internal error (pod not found, Redis failure)

---

## Core Drain — `Drain()` (drainer.go:45-108)

### Step-by-Step Walkthrough

```
                    ┌──────────────────────────┐
                    │  POST /api/v1/drain       │
                    │  {pod_name: "voice-agent-0"}│
                    └────────────┬─────────────┘
                                 │
                    ┌────────────▼─────────────┐
                    │ 1. Check active lease     │
                    │    EXISTS voice:lease:{pod}│
                    └────────────┬─────────────┘
                                 │
                    ┌────────────▼─────────────┐
                    │ 2. Get pod's tier         │
                    │    GET voice:pod:tier:{pod}│
                    └────────────┬─────────────┘
                          found?
                       ┌───┴───┐
                      no      yes
                      │        │
                  error: pod  ┌▼──────────────┐
                  not found   │ 3. Remove from │
                              │    available   │
                              │    pool        │
                              └───────┬───────┘
                                      │
                              ┌───────▼───────┐
                              │ 4. Set draining│
                              │    key w/ TTL  │
                              └───────┬───────┘
                                 success?
                              ┌───┴───┐
                             yes      no → reAddToAvailable() (rollback)
                              │
                      ┌───────▼───────┐
                      │ 5. Return     │
                      │    result     │
                      └───────────────┘
```

---

### Step 1: Check Active Lease (drainer.go:49-53)

```go
leaseKey := redisclient.LeaseKey(podName)    // "voice:lease:{podName}"
hasActiveCall, err := client.Exists(ctx, leaseKey).Result()
```

Checks if the pod currently has an active call. This is **informational only** — the drain proceeds regardless. The response tells the caller whether there's an active call that needs to complete.

**Redis key READ:** `voice:lease:{podName}` (STRING, EXISTS)

---

### Step 2: Get Pod Tier (drainer.go:56-63)

```go
tierKey := redisclient.PodTierKey(podName)    // "voice:pod:tier:{podName}"
tier, err := client.Get(ctx, tierKey).Result()
```

The tier determines which pool to remove the pod from. Three possibilities:
- `"gold"`, `"standard"`, `"basic"` — regular tier
- `"merchant:9shines"` — merchant dedicated pool
- `redis.Nil` — pod not known to the system → error: "pod not found"

**Redis key READ:** `voice:pod:tier:{podName}` (STRING)

---

### Step 3: Remove from Available Pool — `removeFromAvailable()` (drainer.go:114-152)

Based on the tier type, the pod is removed from the correct available pool:

```go
poolKey, isMerchant, isShared := d.parseTier(tier)
```

**`parseTier()`** (drainer.go:184-191) parses the tier string:

| Tier Value | `poolKey` | `isMerchant` | `isShared` |
|-----------|-----------|:---:|:---:|
| `"merchant:9shines"` | `"9shines"` | true | false |
| `"gold"` | `"gold"` | false | false |
| `"basic"` | `"basic"` | false | true (from config) |

**Pool removal operations:**

| Pool Type | Redis Operation | Key |
|-----------|----------------|-----|
| Merchant | `SREM voice:merchant:{id}:pods {podName}` | SET |
| Shared | `ZREM voice:pool:{tier}:available {podName}` | ZSET |
| Exclusive | `SREM voice:pool:{tier}:available {podName}` | SET |

**After this step, the pod is invisible to the allocator** — it's not in any available pool. New allocation requests will skip it.

---

### Step 4: Set Draining Key (drainer.go:76-92)

```go
drainingKey := redisclient.DrainingKey(podName)    // "voice:pod:draining:{podName}"
drainingTTL := d.config.DrainingTTL               // Default: 6 minutes
client.Set(ctx, drainingKey, "true", drainingTTL)
```

The draining key serves multiple purposes:
1. **Allocator exclusion** — `tryAllocateExclusive()` and `tryAllocateShared()` both check `EXISTS voice:pod:draining:{pod}` and skip draining pods
2. **Release behavior** — `Release()` skips returning draining pods to the available pool
3. **Zombie cleanup exclusion** — `cleanupZombies()` skips draining pods (they're not zombies, they're intentionally removed)

**TTL: 6 minutes** — self-healing safety net. If the pod dies and the draining key persists, it would block zombie cleanup from recovering the pod slot. The 6-minute TTL ensures the key eventually expires even if cleanup doesn't happen.

**Why 6 minutes?** It should be longer than the Kubernetes termination grace period (60 seconds in the deployment manifest) to ensure the key outlasts the pod lifecycle. But not so long that it blocks recovery for too long.

### Rollback — `reAddToAvailable()` (drainer.go:157-174)

**Critical: If setting the draining key fails, we must rollback step 3.**

Without rollback, the pod would be removed from the available pool but not marked as draining. The allocator wouldn't find it (not available), and zombie cleanup wouldn't skip it (not draining) — but it also wouldn't recover it because it's still in the assigned pool with a valid lease.

```go
if err := client.Set(ctx, drainingKey, "true", drainingTTL).Err(); err != nil {
    d.reAddToAvailable(ctx, podName, tier)
    return nil, fmt.Errorf("failed to set draining status: %w", err)
}
```

**`reAddToAvailable()` logic:**

| Pool Type | Rollback Operation | Key |
|-----------|-------------------|-----|
| Merchant | `SADD voice:merchant:{id}:pods {podName}` | SET |
| Shared | `ZAddNX voice:pool:{tier}:available {podName}` (score=0) | ZSET |
| Exclusive | `SADD voice:pool:{tier}:available {podName}` | SET |

**Note for shared pools:** Uses `ZAddNX` (only adds if member doesn't exist) with score 0. If zombie cleanup already re-added the pod, this doesn't overwrite its score. Score 0 is a safe default — the Lua allocator uses ZINCRBY, so the score will be corrected on next allocation.

**If rollback also fails:** Pod is invisible until zombie cleanup recovers it (within 30 seconds). This is logged as an error.

---

### Step 5: Return Result (drainer.go:94-108)

```go
return &models.DrainResult{
    Success:       true,
    PodName:       podName,
    HasActiveCall: hasActiveCall > 0,
    Message:       "Pod voice-agent-0 is draining with active call in progress...",
}
```

---

## Complete Sequence Diagram

```
Kubernetes (preStop)    Smart Router (any replica)           Redis
       │                            │                           │
       │  POST /api/v1/drain        │                           │
       │  {pod_name:"voice-agent-0"}│                           │
       ├───────────────────────────►│                           │
       │                            │                           │
       │                            │  EXISTS voice:lease:      │
       │                            │         voice-agent-0     │
       │                            ├──────────────────────────►│
       │                            │  1 (has active call)      │
       │                            │◄──────────────────────────┤
       │                            │                           │
       │                            │  GET voice:pod:tier:      │
       │                            │      voice-agent-0        │
       │                            ├──────────────────────────►│
       │                            │  "gold"                   │
       │                            │◄──────────────────────────┤
       │                            │                           │
       │                            │  SREM voice:pool:gold:    │
       │                            │       available           │
       │                            │       "voice-agent-0"     │
       │                            ├──────────────────────────►│
       │                            │  1 (removed)              │
       │                            │◄──────────────────────────┤
       │                            │                           │
       │                            │  SET voice:pod:draining:  │
       │                            │      voice-agent-0        │
       │                            │      "true" EX 360        │
       │                            ├──────────────────────────►│
       │                            │  OK                       │
       │                            │◄──────────────────────────┤
       │                            │                           │
       │  200 OK                    │                           │
       │  {success, has_active_call}│                           │
       │◄───────────────────────────┤                           │
```

---

## What Happens After Drain

After the drain API returns, the pod's lifecycle continues:

```
┌─────────────┐     ┌──────────────┐     ┌───────────────┐     ┌──────────┐
│ preStop hook │     │ Active call   │     │ K8s sends     │     │ Pod dies │
│ calls drain  │────►│ finishes +    │────►│ SIGTERM       │────►│ Watcher/ │
│ API          │     │ release fires │     │ (grace=60s)   │     │ Reconciler│
└─────────────┘     └──────────────┘     └───────────────┘     │ removes   │
                                                                │ from Redis│
                                                                └──────────┘
```

1. **preStop hook** calls `/api/v1/drain` → pod removed from available pool + draining key set
2. **Active call continues** — pod still serves the in-progress call
3. **Call ends** → voice-agent calls `/api/v1/release` → pod info cleaned up, but NOT returned to pool (draining check)
4. **Kubernetes sends SIGTERM** → pod begins shutdown
5. **Pod dies** → K8s watch event fires → watcher/reconciler removes pod from assigned pool + cleans up all Redis keys
6. **Draining key TTL expires** (6min) — but this is usually after the pod is already fully cleaned up

---

## Redis Keys — Complete Reference

### Read

| Key | Type | When | Purpose |
|-----|------|------|---------|
| `voice:lease:{podName}` | STRING | Step 1 | Check if pod has active call (informational) |
| `voice:pod:tier:{podName}` | STRING | Step 2 | Determine pod's tier → which pool to remove from |

### Write

| Key | Type | TTL | When | Purpose |
|-----|------|-----|------|---------|
| `voice:pod:draining:{podName}` | STRING | 6min (DrainingTTL) | Step 4 | Mark pod as draining |

### Delete

| Key | Type | When | Purpose |
|-----|------|------|---------|
| `voice:pool:{tier}:available` | SET | Step 3 (SREM, exclusive) | Remove pod from exclusive available pool |
| `voice:pool:{tier}:available` | ZSET | Step 3 (ZREM, shared) | Remove pod from shared available pool |
| `voice:merchant:{id}:pods` | SET | Step 3 (SREM, merchant) | Remove pod from merchant pool |

---

## Edge Cases

1. **Pod not found (no tier key):** Returns error. No pool modifications. This can happen if drain is called for a pod that was never discovered or already fully cleaned up.

2. **Pod already drained:** The SREM/ZREM in step 3 returns 0 (already removed), and step 4 overwrites the draining key with a fresh TTL. This is safe — effectively a no-op refresh of the drain.

3. **Draining key SET fails (step 4):** Rollback via `reAddToAvailable()`. Pod goes back to available pool as if drain never happened. If rollback also fails, zombie cleanup recovers within 30 seconds.

4. **Pod has active call:** Drain proceeds normally. The call continues. When it ends, release will clean up but won't return the pod to the pool (draining check in release).

5. **Pod dies before call ends:** K8s watch event fires → `removePodFromPool()` cleans up the pod from assigned pools and all Redis keys. The active call is lost, but `ActiveCalls.Dec()` is called.

6. **Draining TTL expires before pod dies:** The draining key disappears. If the pod is still alive and in the assigned set, zombie cleanup would recover it to the available pool. This is the intended self-healing behavior — if something goes wrong with the shutdown, the pod becomes available again after 6 minutes.

7. **Concurrent drain requests for same pod:** Second drain sees the pod already removed from available (SREM returns 0). Draining key is re-set with fresh TTL. Both return success. Idempotent.

---

## Interaction with Other Flows

| Flow | Relationship |
|------|-------------|
| [07 - Allocation](./07-allocation.md) | Allocator checks `voice:pod:draining:{pod}` and skips draining pods |
| [08 - Release](./08-release.md) | Release checks draining status and skips pool return for draining pods |
| [10 - Zombie Cleanup](./10-zombie-cleanup.md) | Zombie cleanup skips draining pods (they're not zombies) |
| [04 - Pod Watcher](./04-pod-watcher.md) | Watcher's `removePodFromPool()` cleans up when the drained pod finally dies |
| [05 - Reconciler](./05-reconciler.md) | Reconciler's `removePodFromPool()` catches it if the watcher missed the deletion event |
| [12 - HTTP Layer](./12-http-layer.md) | `POST /api/v1/drain` handler feeds into `Drain()` |
