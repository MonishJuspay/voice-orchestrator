# Flow 11: Redis Key Reference

**Source file:** `internal/redisclient/keys.go` (55 lines) + all other source files

## Overview

This document is a comprehensive reference for every Redis key used by the smart router. Each key is documented with its type, TTL, who writes/reads/deletes it, and what it means. This is the single source of truth for the Redis data model.

All keys use the `voice:` prefix (defined in `redisclient/keys.go:7`).

---

## Key Map — Quick Reference

| Key Pattern | Redis Type | TTL | Short Description |
|------------|:----------:|:---:|-------------------|
| `voice:tier:config` | STRING | None | Canonical tier config JSON (source of truth) |
| `voice:pool:{tier}:available` | SET (exclusive) or ZSET (shared) | None | Available pods for allocation |
| `voice:pool:{tier}:assigned` | SET | None | All pods assigned to this tier |
| `voice:merchant:{id}:pods` | SET | None | Merchant's available dedicated pods |
| `voice:merchant:{id}:assigned` | SET | None | All pods assigned to this merchant |
| `voice:pod:tier:{pod}` | STRING | None | Pod → tier mapping |
| `voice:pod:{pod}` | HASH | None | Pod status and metadata |
| `voice:pod:draining:{pod}` | STRING | 6min | Draining flag |
| `voice:pod:metadata` | HASH | None | Global pod metadata (JSON per pod) |
| `voice:lease:{pod}` | STRING | 15min | Active call lock |
| `voice:call:{sid}` | HASH | 30s (lock) → 1hr (real) | Call → pod mapping |
| `voice:merchant:config` | HASH | None | Merchant allocation config |

---

## Detailed Key Documentation

### 0. `voice:tier:config`

**Redis Type:** STRING

**TTL:** None (persistent, managed by operators)

**Value:** Structured envelope JSON containing the tier definitions and default fallback chain:

```json
{
  "tiers": {
    "gold":     {"type": "exclusive", "target": 1},
    "standard": {"type": "exclusive", "target": 1},
    "basic":    {"type": "shared", "target": 1, "max_concurrent": 3}
  },
  "default_chain": ["gold", "standard", "basic"]
}
```

**Helper:** `config.TierConfigRedisKey` (constant) → `"voice:tier:config"`

**Operations by flow:**

| Flow | Operation | Purpose |
|------|-----------|---------|
| [01 - Bootstrap](./01-bootstrap.md) | SETNX | Seed config on first deploy (no-op if key exists) |
| [01 - Bootstrap](./01-bootstrap.md) | GET (if key existed) | Load Redis config into memory, overriding env var |
| [15 - Tier Config Refresh](./15-tier-config-refresh.md) | GET (every 30s) | Refresh in-memory cache on all replicas |
| Operator (manual) | SET | Change tier config at runtime — propagates within 30s |

**Lifecycle:**

```
First deploy: env var → SETNX → key created in Redis
                                      │
Normal restart: SETNX (no-op) → GET → load into memory
                                      │
Every 30s (all replicas): GET → parse → swap in-memory cache
                                      │
Operator changes config: SET → within 30s all replicas pick it up
```

**Important:** This key is the **source of truth** for tier config after first deploy. The `TIER_CONFIG` env var is only used as the bootstrap/seed value. To change tier config at runtime, modify this Redis key — do NOT update the ConfigMap env var (that only affects the next bootstrap if the key is deleted).

---

### 1. `voice:pool:{tier}:available`

**Redis Type:** SET (for exclusive tiers like gold, standard) or ZSET (for shared tiers like basic)

**TTL:** None (persistent, managed by code)

**Members/Scores:**
- SET: Members are pod names (e.g. `"voice-agent-0"`)
- ZSET: Members are pod names, scores = number of active calls on that pod

**Helper:** `redisclient.PoolAvailableKey(tier)` → `"voice:pool:{tier}:available"`

**Operations by flow:**

| Flow | Operation | Purpose |
|------|-----------|---------|
| [05 - Reconciler](./05-reconciler.md) | SADD / ZADD (score=0) | Add discovered pod to pool |
| [05 - Reconciler](./05-reconciler.md) | SREM / ZREM | Remove deleted pod from pool |
| [07 - Allocation](./07-allocation.md) | SPOP (exclusive) | Atomically remove + return random pod |
| [07 - Allocation](./07-allocation.md) | Lua: ZRANGE + ZINCRBY +1 (shared) | Find lowest-score non-draining pod, increment |
| [07 - Allocation](./07-allocation.md) | SADD / ZINCRBY -1 (rollback) | Return pod on storage failure |
| [08 - Release](./08-release.md) | SADD (exclusive) | Return pod after call ends |
| [08 - Release](./08-release.md) | Lua: ZINCRBY -1 (shared) | Decrement call count |
| [09 - Drain](./09-drain.md) | SREM / ZREM | Remove pod from available pool |
| [09 - Drain](./09-drain.md) | SADD / ZAddNX (rollback) | Re-add pod if draining key SET fails |
| [10 - Zombie Cleanup](./10-zombie-cleanup.md) | SISMEMBER (exclusive) | Check if pod is already available |
| [10 - Zombie Cleanup](./10-zombie-cleanup.md) | ZSCORE (shared) | Check if pod is in ZSET |
| [10 - Zombie Cleanup](./10-zombie-cleanup.md) | SADD / ZADD (recovery) | Return zombie pod to pool |
| [10 - Zombie Cleanup](./10-zombie-cleanup.md) | SCARD / ZCARD | Metrics: count available pods |

**Example values:**
```
# Exclusive (SET)
voice:pool:gold:available = {"voice-agent-0"}

# Shared (ZSET)
voice:pool:basic:available = {("voice-agent-2", 1.0)}   # 1 active call
```

---

### 2. `voice:pool:{tier}:assigned`

**Redis Type:** SET

**TTL:** None (persistent)

**Members:** Pod names assigned to this tier (regardless of available/allocated status)

**Helper:** `redisclient.PoolAssignedKey(tier)` → `"voice:pool:{tier}:assigned"`

**Operations by flow:**

| Flow | Operation | Purpose |
|------|-----------|---------|
| [05 - Reconciler](./05-reconciler.md) | SADD | Register pod as assigned to tier |
| [05 - Reconciler](./05-reconciler.md) | SREM | Remove pod from tier |
| [10 - Zombie Cleanup](./10-zombie-cleanup.md) | SMEMBERS | Scan all assigned pods for zombie check |
| [10 - Zombie Cleanup](./10-zombie-cleanup.md) | SCARD | Metrics: count assigned pods |

**Example:**
```
voice:pool:gold:assigned = {"voice-agent-0"}
```

**Note:** A pod in `assigned` but NOT in `available` and with no lease = zombie.

---

### 3. `voice:merchant:{id}:pods`

**Redis Type:** SET

**TTL:** None (persistent)

**Members:** Pod names available in this merchant's dedicated pool

**Helper:** `redisclient.MerchantPodsKey(merchant)` → `"voice:merchant:{merchant}:pods"`

**Operations by flow:**

| Flow | Operation | Purpose |
|------|-----------|---------|
| [05 - Reconciler](./05-reconciler.md) | SADD | Add pod to merchant pool |
| [05 - Reconciler](./05-reconciler.md) | SREM | Remove pod from merchant pool |
| [07 - Allocation](./07-allocation.md) | SPOP | Atomically allocate from merchant pool |
| [07 - Allocation](./07-allocation.md) | SADD (rollback) | Return pod on storage failure |
| [08 - Release](./08-release.md) | SADD (via releaseToExclusivePool) | Return pod after call ends |
| [09 - Drain](./09-drain.md) | SREM | Remove pod from merchant pool during drain |
| [10 - Zombie Cleanup](./10-zombie-cleanup.md) | SISMEMBER | Check if pod already in pool |
| [10 - Zombie Cleanup](./10-zombie-cleanup.md) | SADD (recovery) | Return zombie pod |

---

### 4. `voice:merchant:{id}:assigned`

**Redis Type:** SET

**TTL:** None (persistent)

**Members:** All pod names assigned to this merchant (regardless of available/allocated status)

**Operations by flow:**

| Flow | Operation | Purpose |
|------|-----------|---------|
| [05 - Reconciler](./05-reconciler.md) | SADD | Register pod as merchant-assigned |
| [05 - Reconciler](./05-reconciler.md) | SREM | Remove pod from merchant |
| [10 - Zombie Cleanup](./10-zombie-cleanup.md) | SMEMBERS | Scan for merchant zombies |

---

### 5. `voice:pod:tier:{pod}`

**Redis Type:** STRING

**TTL:** None (persistent, deleted on pod removal)

**Value:** Tier name or merchant identifier:
- `"gold"`, `"standard"`, `"basic"` — regular tier
- `"merchant:9shines"` — merchant dedicated pool

**Helper:** `redisclient.PodTierKey(podName)` → `"voice:pod:tier:{podName}"`

**Operations by flow:**

| Flow | Operation | Purpose |
|------|-----------|---------|
| [05 - Reconciler](./05-reconciler.md) | SET | Assign tier to newly discovered pod |
| [05 - Reconciler](./05-reconciler.md) | GET | Check existing tier before re-assignment |
| [05 - Reconciler](./05-reconciler.md) | DEL | Remove on pod deletion |
| [06 - Tier Assignment](./06-tier-assignment.md) | GET | Check existing tier (defense-in-depth) |
| [09 - Drain](./09-drain.md) | GET | Determine which pool to remove pod from |

**Example:**
```
voice:pod:tier:voice-agent-0 = "gold"
voice:pod:tier:voice-agent-5 = "merchant:9shines"
```

---

### 6. `voice:pod:{pod}`

**Redis Type:** HASH

**TTL:** None (persistent, deleted on pod removal)

**Fields:**

| Field | Set By | Description |
|-------|--------|-------------|
| `status` | Allocator, Releaser, Reconciler | `"available"`, `"allocated"`, `"draining"` |
| `allocated_call_sid` | Allocator (set), Releaser (clear) | Current call SID or empty |
| `allocated_at` | Allocator (set), Releaser (clear) | Unix timestamp or empty |
| `released_at` | Releaser | Unix timestamp of last release |
| `source_pool` | Allocator | Pool the pod was allocated from (e.g. `"pool:gold"`) |

**Helper:** `redisclient.PodInfoKey(podName)` → `"voice:pod:{podName}"`

**Operations by flow:**

| Flow | Operation | Purpose |
|------|-----------|---------|
| [07 - Allocation](./07-allocation.md) | HSET | Set status=allocated, call_sid, etc. |
| [08 - Release](./08-release.md) | HSET | Clear allocation fields, set status=available/draining |
| [05 - Reconciler](./05-reconciler.md) | DEL | Remove on pod deletion |

---

### 7. `voice:pod:draining:{pod}`

**Redis Type:** STRING

**TTL:** 6 minutes (`DRAINING_TTL` env var)

**Value:** `"true"`

**Helper:** `redisclient.DrainingKey(podName)` → `"voice:pod:draining:{podName}"`

**Purpose:** Marks a pod as draining. Every flow checks this key to decide how to handle the pod:

| Flow | Check | Behavior |
|------|-------|----------|
| [07 - Allocation](./07-allocation.md) | EXISTS | Skip pod during allocation (both exclusive and shared) |
| [08 - Release](./08-release.md) | EXISTS | Skip returning pod to available pool |
| [08 - Release (exclusive.go)](./08-release.md) | EXISTS | Double-check before SADD |
| [09 - Drain](./09-drain.md) | SET EX | Creates this key |
| [10 - Zombie Cleanup](./10-zombie-cleanup.md) | EXISTS | Skip draining pods (not zombies) |
| `isPodEligible()` | EXISTS | Return false for draining pods |

**TTL self-healing:** If a drain is set but the pod somehow doesn't die (stuck), the draining key expires after 6 minutes. Zombie cleanup then sees the pod as an orphan and recovers it to the available pool.

---

### 8. `voice:pod:metadata`

**Redis Type:** HASH

**TTL:** None (persistent)

**Fields:** Each field is a pod name, value is JSON metadata

**Helper:** `redisclient.PodMetadataKey()` → `"voice:pod:metadata"`

**Operations by flow:**

| Flow | Operation | Purpose |
|------|-----------|---------|
| [05 - Reconciler](./05-reconciler.md) | HSET | Store pod metadata JSON |
| [05 - Reconciler](./05-reconciler.md) | HDEL | Remove pod metadata on deletion |

---

### 9. `voice:lease:{pod}`

**Redis Type:** STRING

**TTL:** 15 minutes (`LEASE_TTL` env var, configurable)

**Value:** Call SID currently using this pod

**Helper:** `redisclient.LeaseKey(podName)` → `"voice:lease:{podName}"`

**The lease is the heartbeat of an active call.** It's the primary mechanism for zombie detection.

**Operations by flow:**

| Flow | Operation | Purpose |
|------|-----------|---------|
| [07 - Allocation](./07-allocation.md) | SET EX | Create lease when call starts |
| [08 - Release](./08-release.md) | DEL | Delete lease when call ends |
| [09 - Drain](./09-drain.md) | EXISTS | Check if pod has active call (informational) |
| [10 - Zombie Cleanup](./10-zombie-cleanup.md) | EXISTS | Check for active call — presence means not a zombie |
| `isPodEligible()` | EXISTS | Return false if lease exists |
| [05 - Reconciler](./05-reconciler.md) | DEL | Clean up on pod removal |

**Lifecycle:**
```
Allocation creates lease → (15min TTL counting down) → Release deletes lease
                                                     OR
                         → TTL expires → zombie cleanup recovers pod
```

**WARNING:** If a call lasts longer than `LEASE_TTL` (default 15 min), the lease expires and zombie cleanup will recover the pod to the available pool, potentially causing double allocation.

---

### 10. `voice:call:{sid}`

**Redis Type:** HASH

**TTL:** 30 seconds (lock phase) → 1 hour (after allocation, `CALL_INFO_TTL` env var)

**Fields:**

| Field | Set By | Description |
|-------|--------|-------------|
| `_lock` | Idempotency check | Temporary lock (removed after allocation) |
| `pod_name` | Allocator | Assigned pod name |
| `source_pool` | Allocator | Pool format: `"pool:gold"`, `"merchant:9shines"` |
| `merchant_id` | Allocator | Merchant identifier |
| `allocated_at` | Allocator | Unix timestamp |

**Helper:** `redisclient.CallInfoKey(callSID)` → `"voice:call:{callSID}"`

**Operations by flow:**

| Flow | Operation | Purpose |
|------|-----------|---------|
| [07 - Allocation](./07-allocation.md) | Lua: HGETALL | Idempotency check — return existing allocation |
| [07 - Allocation](./07-allocation.md) | Lua: HSET + EXPIRE 30s | Lock placeholder |
| [07 - Allocation](./07-allocation.md) | HSET + HDEL _lock + EXPIRE 1hr | Store real allocation data |
| [08 - Release](./08-release.md) | HGETALL | Look up pod + source pool for this call |
| [08 - Release](./08-release.md) | DEL | Remove call info after release |

**Lifecycle:**
```
Idempotency lock (30s TTL)
    │
    ▼
Real allocation stored (1hr TTL)
    │
    ▼
Release deletes key    OR    TTL expires (1hr)
```

---

### 11. `voice:merchant:config`

**Redis Type:** HASH

**TTL:** None (persistent, managed externally)

**Fields:** Each field is a merchant ID, value is JSON config

**Value format:**
```json
{
    "tier": "gold",
    "pool": "acme-corp",
    "fallback": ["standard", "basic"]
}
```

**Helper:** None (hardcoded as `"voice:merchant:config"` in `merchant.go:13`)

**Operations by flow:**

| Flow | Operation | Purpose |
|------|-----------|---------|
| [07 - Allocation](./07-allocation.md) | HGET | Get merchant's allocation config |

**Note:** This key is populated externally (admin API, manual Redis commands). The smart router only reads it.

---

## Key Relationships Diagram

```
┌─────────────────── TIER CONFIGURATION ─────────────────┐
│                                                         │
│  voice:tier:config                                      │
│  (canonical tier config JSON — source of truth)         │
│       │                                                 │
│       │ GET every 30s (all replicas)                    │
│       ▼                                                 │
│  In-memory cache (sync.RWMutex)                         │
│  → drives tier assignment, allocation, pool management  │
│                                                         │
└─────────────────────────────────────────────────────────┘

                    voice:merchant:config
                    (merchant allocation preferences)
                            │
                            │ HGET during allocation
                            ▼
┌─────────────────── ALLOCATION ───────────────────────┐
│                                                       │
│  voice:call:{sid}         voice:lease:{pod}           │
│  (call→pod mapping)       (active call indicator)     │
│       │                        │                      │
│       │ HGETALL               │ EXISTS                │
│       ▼                        ▼                      │
│  voice:pod:{pod}         ZOMBIE CLEANUP               │
│  (pod status hash)       checks lease presence        │
│                                                       │
└───────────────────────────────────────────────────────┘

┌─────────────────── POOL MANAGEMENT ──────────────────┐
│                                                       │
│  voice:pod:tier:{pod}                                 │
│  (pod → tier mapping)                                 │
│       │                                               │
│       │ determines pool                               │
│       ▼                                               │
│  voice:pool:{tier}:available     voice:pool:{tier}:assigned
│  (pods ready for allocation)     (all pods in tier)   │
│       │                               │               │
│       │ SPOP/ZINCRBY (allocate)      │ SMEMBERS      │
│       │ SADD/ZINCRBY (release)       │ (zombie scan) │
│       │                               │               │
│  voice:merchant:{id}:pods    voice:merchant:{id}:assigned
│  (merchant available pods)   (all merchant pods)      │
│                                                       │
└───────────────────────────────────────────────────────┘

┌─────────────────── LIFECYCLE FLAGS ──────────────────┐
│                                                       │
│  voice:pod:draining:{pod}                             │
│  (6min TTL — blocks allocation, release-to-pool,      │
│   and zombie recovery)                                │
│                                                       │
│  voice:pod:metadata                                   │
│  (global hash — pod JSON metadata)                    │
│                                                       │
└───────────────────────────────────────────────────────┘
```

---

## Key Count Per Pod (Steady State)

When a pod is idle and available:
```
voice:pod:tier:voice-agent-0     = "gold"           # 1 STRING
voice:pod:voice-agent-0          = {status: "available", ...}  # 1 HASH
voice:pool:gold:available        ∋ "voice-agent-0"  # member of 1 SET
voice:pool:gold:assigned         ∋ "voice-agent-0"  # member of 1 SET
voice:pod:metadata[voice-agent-0] = "{...}"         # field in 1 HASH
```
**= 3 keys + membership in 2 sets + 1 hash field = ~5 Redis entries**

When a pod is handling a call:
```
(all of the above minus available pool membership, PLUS:)
voice:lease:voice-agent-0       = "CA123"           # 1 STRING (15min TTL)
voice:call:CA123                = {pod_name: ..., ...}  # 1 HASH (1hr TTL)
```
**= 4 keys + membership in 1 set + 1 hash field + 1 call hash = ~7 Redis entries**

---

## Redis Memory Estimation

For a production cluster with 3 voice-agent pods and low concurrent calls:
- 1 tier config key (global)
- ~15 keys for pod state (3 pods x 5 entries each)
- ~3 call info hashes during concurrent calls
- ~3 leases during concurrent calls
- 1 merchant config hash (global)
- 1 pod metadata hash (global)
- **Total: ~26 Redis keys** — negligible memory usage

---

## Interaction with Other Flows

Every flow interacts with Redis keys. See individual flow docs for details:
- [02 - Configuration](./02-configuration.md) — tier config bootstrap and Redis-backed lifecycle
- [05 - Reconciler](./05-reconciler.md) — manages pool membership and tier assignments
- [07 - Allocation](./07-allocation.md) — reads/writes call info, lease, pod info, pool membership
- [08 - Release](./08-release.md) — reverses allocation: returns to pool, deletes lease/call info
- [09 - Drain](./09-drain.md) — removes from pool, sets draining key
- [10 - Zombie Cleanup](./10-zombie-cleanup.md) — scans assigned sets, checks leases, recovers orphans
- [15 - Tier Config Refresh](./15-tier-config-refresh.md) — reads voice:tier:config every 30s
