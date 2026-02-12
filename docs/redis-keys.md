# Smart Router — Redis Keys Reference

All keys are prefixed with `voice:`.

---

## 1. Pool Keys (where pods live)

### 1a. Exclusive Pool — Available (`SET`)

```
voice:pool:{tier}:available
```

| Tier examples | `voice:pool:gold:available`, `voice:pool:standard:available`, `voice:pool:overflow:available` |
|---|---|
| **Type** | SET |
| **Members** | Pod names (e.g., `voice-agent-0`, `voice-agent-1`) |
| **Purpose** | Pods available for allocation. SPOP atomically grabs one. |
| **TTL** | None |

| Operation | Command | Who | File:Line |
|-----------|---------|-----|-----------|
| **Add pod** | `SADD` | Pool Manager (addPodToPool) | reconciler.go:155 |
| **Add pod** | `SADD` | Pool Manager (ensurePodInPool) | reconciler.go:258 |
| **Allocate (grab pod)** | `SPOP` | Allocator (tryAllocateExclusive) | exclusive.go:17 |
| **Return pod on failure** | `SADD` | Allocator (returnPodToPool) | allocator.go:319 |
| **Return pod on failure** | `SADD` | Exclusive allocator (draining check fail) | exclusive.go:31 |
| **Release pod** | `SADD` | Releaser (releaseToExclusivePool) | exclusive.go:27 |
| **Remove pod** | `SREM` | Pool Manager (removePodFromPool) | reconciler.go:185,194 |
| **Remove pod** | `SREM` | Drainer (removeFromAvailable) | drainer.go:127-133 |
| **Zombie recovery** | `SADD` | Zombie cleanup (cleanupZombies) | zombie.go:142 |
| **Check membership** | `SISMEMBER` | Zombie cleanup | zombie.go:130 |
| **Count** | `SCARD` | Status handler | status.go:79 |

---

### 1b. Shared Pool — Available (`ZSET`)

```
voice:pool:{tier}:available       (e.g., voice:pool:basic:available)
```

| **Type** | ZSET (Sorted Set) |
|---|---|
| **Members** | Pod names |
| **Score** | Number of active concurrent connections (0 = idle, higher = busier) |
| **Purpose** | Pods that can handle multiple calls. Score tracks load. Lua scripts ensure atomicity. |
| **TTL** | None |

| Operation | Command | Who | File:Line |
|-----------|---------|-----|-----------|
| **Add pod** | `ZADDNX` (score 0) | Pool Manager (addPodToPool) | reconciler.go:145-148 |
| **Add pod** | `ZADDNX` (score 0) | Pool Manager (ensurePodInPool) | reconciler.go:253-256 |
| **Allocate** | Lua: `ZRANGE` + `ZINCRBY +1` | Allocator (tryAllocateShared) | shared.go:18-51 (Lua), shared.go:63 (Eval) |
| **Release** | Lua: `ZSCORE` + `ZINCRBY -1` | Releaser (releaseToSharedPool) | shared.go:14-33 (Lua), shared.go:40 (Eval) |
| **Remove pod** | `ZREM` | Pool Manager (removePodFromPool) | reconciler.go:183 |
| **Remove pod** | `ZREM` | Drainer (removeFromAvailable) | drainer.go:118 |
| **Zombie recovery** | `ZSCORE` (check exists) | Zombie cleanup | zombie.go:97 |
| **Zombie recovery** | `ZADD` (score 0) | Zombie cleanup | zombie.go:100-103 |
| **Count** | `ZCARD` | Status handler | status.go:77 |

**Shared Allocate Lua Script** (atomic — single Redis round-trip):
```lua
-- Gets all pods sorted by score (least loaded first)
-- Iterates to find first non-draining pod with capacity < max_concurrent
-- Atomically ZINCRBY +1 on that pod
-- Returns pod name or nil
```

**Shared Release Lua Script** (atomic):
```lua
-- ZSCORE to get current score
-- If score > 0: ZINCRBY -1 (floor at 0)
-- Returns new score
```

---

### 1c. Pool — Assigned (`SET`)

```
voice:pool:{tier}:assigned
```

| Tier examples | `voice:pool:gold:assigned`, `voice:pool:standard:assigned`, `voice:pool:overflow:assigned`, `voice:pool:basic:assigned` |
|---|---|
| **Type** | SET |
| **Members** | ALL pod names assigned to this tier (regardless of available/busy/draining) |
| **Purpose** | Tracks which pods belong to which tier. Used for reconciliation, zombie cleanup, and capacity checks. NOT used during allocation. |
| **TTL** | None |

| Operation | Command | Who | File:Line |
|-----------|---------|-----|-----------|
| **Add** | `SADD` | Pool Manager (addPodToPool) | reconciler.go:138 |
| **Add** | `SADD` | Pool Manager (ensurePodInPool) | reconciler.go:244 |
| **Remove** | `SREM` | Pool Manager (removePodFromPool) | reconciler.go:179,193 |
| **List all** | `SMEMBERS` | Pool Manager (getAllRedisPods) | reconciler.go:67 |
| **List all** | `SMEMBERS` | Zombie cleanup (cleanupZombies) | zombie.go:43 |
| **Count** | `SCARD` | Tier assigner (autoAssignTier) | tierassigner.go:55,66,82,92 |
| **Count** | `SCARD` | Tier assigner (getPoolCapacity) | tierassigner.go:111 |
| **Count** | `SCARD` | Status handler | status.go:70 |
| **Check** | `SISMEMBER` | Watcher (isPodRegistered) | watcher.go:130,137,146,153 |

---

### 1d. Merchant Pool — Pods (Available) (`SET`)

```
voice:merchant:{merchant_id}:pods
```

| Example | `voice:merchant:9shines:pods` |
|---|---|
| **Type** | SET |
| **Members** | Pod names available for this merchant |
| **Purpose** | Like exclusive available pool, but dedicated to a specific merchant. |
| **TTL** | None |

| Operation | Command | Who | File:Line |
|-----------|---------|-----|-----------|
| **Add** | `SADD` | Pool Manager (addPodToPool) | reconciler.go:129 |
| **Add** | `SADD` | Pool Manager (ensurePodInPool) | reconciler.go:241 |
| **Allocate** | `SPOP` | Allocator (tryAllocateExclusive via merchant pool key) | allocator.go:100, exclusive.go:17 |
| **Release** | `SADD` | Releaser (releaseToExclusivePool) | releaser.go:84 |
| **Return on failure** | `SADD` | Allocator (returnPodToPool) | allocator.go:324 |
| **Remove** | `SREM` | Pool Manager (removePodFromPool) | reconciler.go:186 |
| **Remove** | `SREM` | Drainer (removeFromAvailable) | drainer.go:108 |

---

### 1e. Merchant Pool — Assigned (`SET`)

```
voice:merchant:{merchant_id}:assigned
```

| Example | `voice:merchant:9shines:assigned` |
|---|---|
| **Type** | SET |
| **Members** | ALL pods assigned to this merchant (available + busy) |
| **Purpose** | Tracks total pods dedicated to a merchant. Used for capacity decisions. |
| **TTL** | None |

| Operation | Command | Who | File:Line |
|-----------|---------|-----|-----------|
| **Add** | `SADD` | Pool Manager (addPodToPool) | reconciler.go:125 |
| **Add** | `SADD` | Pool Manager (ensurePodInPool) | reconciler.go:239 |
| **Remove** | `SREM` | Pool Manager (removePodFromPool) | reconciler.go:180 |
| **Count** | `SCARD` | Tier assigner (autoAssignTier) | tierassigner.go:40 |
| **Count** | `SCARD` | Tier assigner (getMerchantPoolCapacity) | tierassigner.go:129 |
| **Check** | `SISMEMBER` | Watcher (isPodRegistered) | watcher.go:153 |
| **List** | `SMEMBERS` | Pool Manager (getAllRedisPods) | reconciler.go:91 |

---

## 2. Per-Pod Keys

### 2a. Pod Tier (`STRING`)

```
voice:pod:tier:{pod_name}        (e.g., voice:pod:tier:voice-agent-0)
```

| **Type** | STRING |
|---|---|
| **Value** | Tier name: `"gold"`, `"standard"`, `"basic"`, `"merchant:9shines"` |
| **Purpose** | Maps a pod to its assigned tier. Used to know where to return a pod on release. |
| **TTL** | None (persistent) |

| Operation | Command | Who | File:Line |
|-----------|---------|-----|-----------|
| **Set** | `SET` | Pool Manager (addPodToPool) | reconciler.go:126 (merchant), 139 (pool) |
| **Get** | `GET` | Pool Manager (addPodToPool — check existing) | reconciler.go:103 |
| **Get** | `GET` | Pool Manager (getPodTier) | reconciler.go:267 |
| **Get** | `GET` | Tier assigner (autoAssignTier) | tierassigner.go:20 |
| **Get** | `GET` | Drainer (Drain) | drainer.go:57 |
| **Get** | `GET` | Pod Info handler | pod_info.go:53 |
| **Delete** | `DEL` | Pool Manager (removePodFromPool) | reconciler.go:219 |

---

### 2b. Pod Info (`HASH`)

```
voice:pod:{pod_name}             (e.g., voice:pod:voice-agent-0)
```

| **Type** | HASH |
|---|---|
| **Fields** | `status` ("available"/"allocated"/"draining"), `allocated_call_sid`, `allocated_at`, `released_at`, `source_pool` |
| **Purpose** | Detailed status of a pod. Updated during allocation and release. |
| **TTL** | None |

| Operation | Command | Who | File:Line |
|-----------|---------|-----|-----------|
| **Set (allocate)** | `HSET` | Allocator (storeAllocation) | allocator.go:287 |
| **Set (release)** | `HSET` | Releaser (Release) | releaser.go:146 |
| **Get active call** | `HGET allocated_call_sid` | Pool Manager (removePodFromPool) | reconciler.go:199 |
| **Delete** | `DEL` | Pool Manager (removePodFromPool) | reconciler.go:218 |

---

### 2c. Pod Draining Flag (`STRING`)

```
voice:pod:draining:{pod_name}    (e.g., voice:pod:draining:voice-agent-0)
```

| **Type** | STRING |
|---|---|
| **Value** | `"true"` |
| **Purpose** | Marks a pod as draining. Draining pods are skipped during allocation and not returned to available pool on release. |
| **TTL** | 6 minutes (configurable via `DRAINING_TTL`) |

| Operation | Command | Who | File:Line |
|-----------|---------|-----|-----------|
| **Set** | `SET` (with TTL) | Drainer (Drain) | drainer.go:76 |
| **Check** | `EXISTS` | Allocator (tryAllocateExclusive) | exclusive.go:28 |
| **Check** | `EXISTS` | Allocator Lua (shared — inside script) | shared.go:40-41 |
| **Check** | `EXISTS` | Releaser (Release) | releaser.go:68 |
| **Check** | `EXISTS` | Releaser (releaseToExclusivePool) | exclusive.go:16 |
| **Check** | `EXISTS` | Zombie cleanup | zombie.go:70 |
| **Check** | `EXISTS` | Watcher (isPodEligible) | watcher.go:178 |
| **Delete** | `DEL` | Pool Manager (removePodFromPool) | reconciler.go:221 |

---

### 2d. Pod Lease (`STRING`)

```
voice:lease:{pod_name}           (e.g., voice:lease:voice-agent-0)
```

| **Type** | STRING |
|---|---|
| **Value** | Call SID (e.g., `"CA09df12f5fefb31c82b6c8e7d42cc7c9d"`) |
| **Purpose** | Indicates pod is actively handling a call. Acts as a lock. Zombie cleanup uses absence of lease to recover pods. |
| **TTL** | 15 minutes (`leaseTTL` in allocator.go:35) |

| Operation | Command | Who | File:Line |
|-----------|---------|-----|-----------|
| **Set** | `SET` (with 15min TTL) | Allocator (storeAllocation) | allocator.go:293 |
| **Check** | `EXISTS` | Drainer (Drain) | drainer.go:50 |
| **Check** | `EXISTS` | Zombie cleanup (exclusive tier) | zombie.go:116 |
| **Check** | `EXISTS` | Watcher (isPodEligible) | watcher.go:168 |
| **Get** | `GET` | Pod Info handler | pod_info.go:73 |
| **Delete** | `DEL` | Releaser (Release) | releaser.go:125 |
| **Delete** | `DEL` | Pool Manager (removePodFromPool) | reconciler.go:220 |
| **Scan** | `SCAN voice:lease:*` | Status handler (count active calls) | status.go:53 |

---

### 2e. Pod Metadata (`HASH` — single global key)

```
voice:pod:metadata
```

| **Type** | HASH |
|---|---|
| **Fields** | Pod name → JSON `{"tier":"standard","name":"voice-agent-0"}` |
| **Purpose** | Global metadata store. Fallback for tier lookup if `voice:pod:tier:{name}` is missing. |
| **TTL** | None |

| Operation | Command | Who | File:Line |
|-----------|---------|-----|-----------|
| **Set** | `HSET` | Pool Manager (addPodToPool) | reconciler.go:115 |
| **Get** | `HGET` | Pool Manager (getPodTier — fallback) | reconciler.go:272 |
| **Delete field** | `HDEL` | Pool Manager (removePodFromPool) | reconciler.go:217 |

---

## 3. Per-Call Keys

### 3a. Call Info (`HASH`)

```
voice:call:{call_sid}            (e.g., voice:call:CA09df12f5fefb31c82b6c8e7d42cc7c9d)
```

| **Type** | HASH |
|---|---|
| **Fields** | `pod_name`, `source_pool`, `merchant_id`, `allocated_at`, `_lock` (temporary) |
| **Purpose** | Maps a call to its allocated pod. Source of truth for release — tells releaser which pool to return pod to. Also provides idempotency (same call_sid returns same pod). |
| **TTL** | 24 hours (`callInfoTTL`), or 30 seconds during lock phase |

| Operation | Command | Who | File:Line |
|-----------|---------|-----|-----------|
| **Lock (idempotency)** | Lua: `HGETALL` + `HSET _lock` + `EXPIRE 30s` | Allocator (CheckAndLockAllocation) | idempotency.go:20-33 (Lua), idempotency.go:43 |
| **Store** | `HSET` (4 fields, overwrites lock) | Allocator (storeAllocation) | allocator.go:266 |
| **Remove lock** | `HDEL _lock` | Allocator (storeAllocation) | allocator.go:271 |
| **Set TTL** | `EXPIRE` 24h | Allocator (storeAllocation) | allocator.go:274 |
| **Get all** | `HGETALL` | Releaser (Release) | releaser.go:48 |
| **Delete** | `DEL` | Releaser (Release) | releaser.go:152 |
| **Delete (orphan)** | `DEL` | Pool Manager (removePodFromPool) | reconciler.go:202 |

---

## 4. Merchant Config

### 4a. Merchant Config (`HASH` — single global key)

```
voice:merchant:config
```

| **Type** | HASH |
|---|---|
| **Fields** | Merchant ID → JSON `{"tier":"gold","pool":"9shines"}` |
| **Purpose** | Determines which tier/pool a merchant is assigned to. If merchant not found, defaults to `standard` tier. |
| **TTL** | None (manually managed) |

| Operation | Command | Who | File:Line |
|-----------|---------|-----|-----------|
| **Get** | `HGET` | Allocator (GetMerchantConfig) | merchant.go:25 |

> **Note:** This key is NOT written by Smart Router code. It must be set manually or by an external admin tool.

---

## 5. Complete Key Inventory

| # | Key Pattern | Type | Purpose | Created By | TTL |
|---|------------|------|---------|-----------|-----|
| 1 | `voice:pool:{tier}:available` | SET or ZSET | Available pods for allocation | Pool Manager | None |
| 2 | `voice:pool:{tier}:assigned` | SET | All pods in tier (tracking) | Pool Manager | None |
| 3 | `voice:merchant:{id}:pods` | SET | Merchant's available pods | Pool Manager | None |
| 4 | `voice:merchant:{id}:assigned` | SET | All pods in merchant tier | Pool Manager | None |
| 5 | `voice:pod:tier:{pod}` | STRING | Pod → tier mapping | Pool Manager | None |
| 6 | `voice:pod:{pod}` | HASH | Pod status details | Allocator / Releaser | None |
| 7 | `voice:pod:draining:{pod}` | STRING | Draining flag | Drainer | 6 min |
| 8 | `voice:lease:{pod}` | STRING | Active call lock | Allocator | 15 min |
| 9 | `voice:pod:metadata` | HASH | Global pod metadata | Pool Manager | None |
| 10 | `voice:call:{sid}` | HASH | Call → pod mapping | Allocator | 24 hr |
| 11 | `voice:merchant:config` | HASH | Merchant tier config | External/Admin | None |

---

## 6. Allocation Flow — Redis Ops by Tier

### Standard Tier (most common — your test case)

```
Allocate (9 ops):
  1. EVAL checkAndLockScript         → idempotency check + lock         [Lua: HGETALL + HSET + EXPIRE]
  2. HGET voice:merchant:config      → get merchant tier                [returns nil → default "standard"]
  3. SPOP voice:pool:standard:avail  → grab a pod atomically
  4. EXISTS voice:pod:draining:{pod} → ensure not draining
  5. HSET voice:call:{sid}           → store call info (4 fields)
  6. HDEL voice:call:{sid} _lock     → remove lock placeholder
  7. EXPIRE voice:call:{sid}         → set 24h TTL
  8. HSET voice:pod:{pod}            → update pod status (4 fields)
  9. SET voice:lease:{pod}           → create lease (15min TTL)

Release (7 ops):
  1. HGETALL voice:call:{sid}        → get pod_name + source_pool
  2. EXISTS voice:pod:draining:{pod} → check draining
  3. EXISTS voice:pod:draining:{pod} → re-check in releaseToExclusive  (*)
  4. SADD voice:pool:standard:avail  → return pod to pool
  5. DEL voice:lease:{pod}           → remove lease
  6. HSET voice:pod:{pod}            → update status to "available"
  7. DEL voice:call:{sid}            → delete call info
```

(*) The draining check happens twice — once in Release() and once in releaseToExclusivePool(). Could be optimized.

### Gold Tier (merchant has gold config)

```
Allocate (9 ops — same count, different pool):
  1. EVAL checkAndLockScript
  2. HGET voice:merchant:config      → returns {"tier":"gold"}
  3. SPOP voice:pool:gold:available  → grab from gold pool
  4. EXISTS voice:pod:draining:{pod}
  5-9. Same store operations
```

### Dedicated Merchant Tier

```
Allocate (9 ops):
  1. EVAL checkAndLockScript
  2. HGET voice:merchant:config      → returns {"tier":"dedicated","pool":"9shines"}
  3. SPOP voice:merchant:9shines:pods → grab from merchant pool
  4. EXISTS voice:pod:draining:{pod}
  5-9. Same store operations
```

### Shared Tier (basic — last resort fallback)

```
Allocate (8 ops — Lua combines SPOP+draining check):
  1. EVAL checkAndLockScript
  2. HGET voice:merchant:config
  3-6. SPOP gold (miss) + EXISTS draining, SPOP standard (miss) + EXISTS draining, SPOP overflow (miss) + EXISTS draining
       → 6 ops for misses (worst case)
  7. EVAL allocateSharedScript        → Lua: ZRANGE + EXISTS draining + ZINCRBY
  8-12. Same 5 store operations

  Worst case total: 1 + 1 + 6 + 1 + 5 = 14 ops
```

### Fallback Chain (worst case — all exclusive pools empty)

```
Standard merchant hitting all pools:
  1. EVAL checkAndLockScript                              → 1 Lua
  2. HGET voice:merchant:config                           → 1
  3. SPOP voice:pool:standard:available → empty (redis.Nil) → 1
  4. SPOP voice:pool:overflow:available → empty            → 1
  5. EVAL allocateSharedScript (basic)  → finds pod        → 1 Lua
  6-10. Store operations                                   → 5
  Total: 10 ops

Gold merchant hitting all pools:
  1. EVAL checkAndLockScript                              → 1 Lua
  2. HGET voice:merchant:config                           → 1
  3. SPOP voice:pool:gold:available → empty               → 1
  4. SPOP voice:pool:standard:available → empty            → 1
  5. SPOP voice:pool:overflow:available → empty            → 1
  6. EVAL allocateSharedScript (basic) → finds pod         → 1 Lua
  7-11. Store operations                                   → 5
  Total: 11 ops

Dedicated merchant hitting all pools:
  1. EVAL checkAndLockScript                              → 1 Lua
  2. HGET voice:merchant:config                           → 1
  3. SPOP voice:merchant:{id}:pods → empty                → 1
  4. SPOP voice:pool:gold:available → empty               → 1
  5. SPOP voice:pool:standard:available → empty            → 1
  6. SPOP voice:pool:overflow:available → empty            → 1
  7. EVAL allocateSharedScript (basic) → finds pod         → 1 Lua
  8-12. Store operations                                   → 5
  Total: 12 ops
```

---

## 7. Background Processes (Leader Only)

### Pod Watcher (K8s Watch → Redis)

| Event | Action | Redis Ops |
|-------|--------|-----------|
| Pod Added/Ready | `addPodToPool()` | GET tier, HSET metadata, SADD assigned, SET tier, EXISTS lease, EXISTS draining, SADD/ZADDNX available |
| Pod Not Ready | `removePodFromPool()` | GET tier, SREM from all pools, HGET active call, DEL call, HDEL metadata, DEL pod info, DEL tier, DEL lease, DEL draining |
| Pod Deleted | `removePodFromPool()` | Same as above |

### Zombie Cleanup (every 30s)

For each assigned pod:
1. `EXISTS voice:pod:draining:{pod}` — skip if draining
2. **Exclusive:** `EXISTS voice:lease:{pod}` — skip if leased (active call)
3. **Exclusive:** `SISMEMBER voice:pool:{tier}:available` — check if in pool
4. **Exclusive:** `SADD` if missing → recovered zombie
5. **Shared:** `ZSCORE voice:pool:{tier}:available` — check if in ZSET
6. **Shared:** `ZADD` (score 0) if missing → recovered zombie

### Full Sync (every 1 minute)

Lists all K8s pods, compares with `getAllRedisPods()`, adds missing, removes ghosts.

---

## 8. Visual: Pool Data Structure

```
                          ┌─────────────────────────────────────┐
                          │         voice:merchant:config        │
                          │              (HASH)                  │
                          │  "9shines" → {"tier":"dedicated"...} │
                          │  "acme"    → {"tier":"gold"}         │
                          └───────────────┬─────────────────────┘
                                          │ HGET (during allocate)
                                          ▼
┌──────────────────────────────────────────────────────────────────────────────┐
│                        EXCLUSIVE POOLS (SET)                                 │
│                                                                              │
│  voice:pool:gold:assigned     = {agent-0}           ← tracking              │
│  voice:pool:gold:available    = {agent-0}           ← SPOP for allocate     │
│                                                                              │
│  voice:pool:standard:assigned = {agent-1}           ← tracking              │
│  voice:pool:standard:available= {agent-1}           ← SPOP for allocate     │
│                                                                              │
│  voice:pool:overflow:assigned = {}                  ← tracking              │
│  voice:pool:overflow:available= {}                  ← SPOP for allocate     │
│                                                                              │
│  voice:merchant:9shines:assigned = {agent-X}        ← tracking              │
│  voice:merchant:9shines:pods     = {agent-X}        ← SPOP for allocate     │
└──────────────────────────────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────────────────────────────┐
│                        SHARED POOLS (ZSET)                                   │
│                                                                              │
│  voice:pool:basic:assigned    = {agent-2}           ← tracking (SET)        │
│  voice:pool:basic:available   = {agent-2: score=0}  ← Lua ZINCRBY allocate  │
│                                                       score = active calls   │
└──────────────────────────────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────────────────────────────┐
│                        PER-POD KEYS                                          │
│                                                                              │
│  voice:pod:tier:agent-0       = "gold"              (STRING, no TTL)        │
│  voice:pod:tier:agent-1       = "standard"          (STRING, no TTL)        │
│  voice:pod:tier:agent-2       = "basic"             (STRING, no TTL)        │
│                                                                              │
│  voice:pod:agent-0            = {status, call_sid, allocated_at, ...}       │
│                                                      (HASH, no TTL)         │
│                                                                              │
│  voice:pod:draining:agent-0   = "true"              (STRING, TTL=6min)      │
│                                                                              │
│  voice:lease:agent-0          = "CA09df12..."        (STRING, TTL=15min)    │
│                                                                              │
│  voice:pod:metadata           = {agent-0: "{...}", agent-1: "{...}"}        │
│                                                      (HASH, no TTL)         │
└──────────────────────────────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────────────────────────────┐
│                        PER-CALL KEYS                                         │
│                                                                              │
│  voice:call:CA09df12...       = {pod_name, source_pool, merchant_id, ...}   │
│                                                      (HASH, TTL=24hr)       │
└──────────────────────────────────────────────────────────────────────────────┘
```

---

## 9. Key Lifecycle Diagram

```
Pod comes up (K8s Ready):
  Pool Manager → SET voice:pod:tier:{pod}
               → HSET voice:pod:metadata
               → SADD voice:pool:{tier}:assigned
               → SADD voice:pool:{tier}:available  (or ZADDNX for shared)

Call comes in (Allocate):
  Allocator → EVAL idempotency (voice:call:{sid})
            → HGET voice:merchant:config
            → SPOP voice:pool:{tier}:available     (or EVAL Lua for shared)
            → EXISTS voice:pod:draining:{pod}
            → HSET voice:call:{sid}
            → HSET voice:pod:{pod}
            → SET voice:lease:{pod}

Call ends (Release):
  Releaser → HGETALL voice:call:{sid}
           → EXISTS voice:pod:draining:{pod}
           → SADD voice:pool:{tier}:available      (or EVAL Lua for shared)
           → DEL voice:lease:{pod}
           → HSET voice:pod:{pod}
           → DEL voice:call:{sid}

Pod draining (Rolling Update):
  Drainer → EXISTS voice:lease:{pod}
          → GET voice:pod:tier:{pod}
          → SREM/ZREM voice:pool:{tier}:available
          → SET voice:pod:draining:{pod} (TTL=6min)

Pod removed (K8s Delete):
  Pool Manager → SREM from all pools (assigned + available)
               → HGET voice:pod:{pod} allocated_call_sid
               → DEL voice:call:{orphaned_sid}
               → HDEL voice:pod:metadata
               → DEL voice:pod:{pod}
               → DEL voice:pod:tier:{pod}
               → DEL voice:lease:{pod}
               → DEL voice:pod:draining:{pod}
```
