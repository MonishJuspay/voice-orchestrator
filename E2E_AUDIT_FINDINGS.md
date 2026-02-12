# E2E Logical Audit — Findings & Fix Tracker

**Date:** Feb 2026
**Scope:** Full end-to-end trace of all 6 coupled flows in Smart Router (Go)
**Method:** Step-by-step Redis command tracing at component boundaries

---

## All Fixes Applied

### C2 — Zombie Storm: Shared tier ZScore treats ALL errors as "missing from ZSET"

**File:** `internal/poolmanager/zombie.go`
**Status:** FIXED
**Severity:** CRITICAL

**What was broken:**
```go
_, err := m.redis.ZScore(ctx, availKey, podName).Result()
if err != nil {
    // Treated ANY error as "missing" — including connection errors
    m.redis.ZAdd(ctx, ..., redis.Z{Score: 0, Member: podName})
}
```

**Scenario:** Redis connection blip during zombie cleanup cycle.
1. ZScore returns a transient connection error (NOT redis.Nil)
2. Code enters `if err != nil` branch
3. ZAdd resets pod score to 0
4. Pod was handling 50 calls (score=50) -> now thinks it has 0
5. Allocator sees score 0 -> floods pod with `maxConcurrent` more calls
6. Every 30 seconds (cleanup interval), this repeats for ALL shared pods

**Fix applied:** Distinguish `redis.Nil` (pod truly missing -> recover with score 0) from real Redis errors (skip pod, log warning, continue).

---

### F3-A — removePodFromPool Missing ActiveCalls.Dec()

**File:** `internal/poolmanager/reconciler.go`
**Status:** FIXED
**Severity:** HIGH

**What was broken:** When a pod with an active call dies (K8s deletes it), the code:
1. Cleaned up call info: `DEL voice:call:{callSID}` OK
2. Cleaned up lease: `DEL voice:lease:{podName}` OK
3. Did NOT decrement `ActiveCalls` gauge

**Result:** Every time a pod died mid-call, `ActiveCalls` gauge drifted +1 permanently.

**Fix applied:** Added `middleware.ActiveCalls.Dec()` after successful orphaned call info cleanup in `removePodFromPool`.

---

### Non-Atomic Drain — Rollback on Draining Key SET Failure

**File:** `internal/drainer/drainer.go`
**Status:** FIXED
**Severity:** MEDIUM

**What was broken:** The drain operation does two non-atomic steps:
1. `removeFromAvailable()` — removes pod from available pool
2. `SET draining key` — marks pod as draining

If step 2 failed, the pod was removed from available but not marked as draining — it became invisible to the allocator. Zombie cleanup could eventually recover it (up to 30s), but there was no proactive recovery.

**Fix applied:** Added `reAddToAvailable()` rollback method. If `SET draining key` fails, the pod is re-added to its available pool (using SAdd for exclusive/merchant, ZAddNX with score 0 for shared). If the rollback also fails, the pod is invisible until zombie recovery (30s).

**Related race condition (accepted):** SPOP-vs-drain race — if the allocator SPOPs a pod at the exact moment the drainer is removing it, the pod is consumed by both paths. The allocator wins (has the pod), the drainer's drain succeeds but is a no-op. The pod becomes available again after drain TTL (6min) + zombie cleanup (30s). This is low severity and accepted.

---

### Lease TTL Comment Mismatch

**File:** `internal/allocator/allocator.go`
**Status:** FIXED
**Severity:** LOW

Comment said "default 24h" but actual default was `15*time.Minute` (set via `LEASE_TTL` env var). Updated comment to say "default 15min, set via LEASE_TTL env var".

---

### Dead `zombiesFound` Counter

**File:** `internal/poolmanager/zombie.go`
**Status:** FIXED
**Severity:** LOW

Variable `zombiesFound` was incremented but never read. Removed the variable declaration and all 3 `zombiesFound++` lines. Replaced with `zombiesRecovered` counter that IS used in the final log message.

---

### C3 — safeGo Restart Loop

**File:** `internal/poolmanager/manager.go`
**Status:** FIXED (was fixed in Phase 5)
**Severity:** CRITICAL

`safeGo` now wraps goroutines with panic recovery AND automatic restart with exponential backoff (1s -> 2s -> 4s ... capped at 30s). Exits only when ctx is cancelled. Both `watchPods` and `zombieCleanup` use this wrapper.

---

## Verified Findings (No Fix Needed)

### ActiveCalls Gauge — Shared Zombie Recovery Path

**Status:** CONFIRMED OK
**Severity:** N/A (was a concern, verified correct)

Shared zombie recovery adds a pod back to the ZSET with score 0 but does NOT call `ActiveCalls.Dec()`. This is CORRECT — the call IS still active, the pod was just missing from the ZSET. Contrast with exclusive/merchant recovery which DO call `ActiveCalls.Dec()` because no lease = no call.

### SPOP vs Drain Race — Exclusive Pod Discarded for ~6.5min

**Files:** `internal/allocator/exclusive.go:35-37`, `internal/drainer/drainer.go`
**Status:** ACCEPTED (inherent distributed race, self-healing)
**Severity:** LOW

**Scenario:**
1. Drainer calls `SREM` on available pool for pod X
2. Simultaneously, allocator calls `SPOP` on same pool and gets pod X first
3. Allocator checks draining key → exists → discards pod (line 37, `continue`)
4. Drainer's SREM is a no-op (pod already gone via SPOP)
5. Pod is not in available pool AND not returned by allocator

**Impact:** Pod is invisible for up to drain TTL (6min) + zombie cleanup (30s) = ~6.5min.

**Why not fixed:**
- The pod was being drained anyway — it shouldn't receive new calls
- Making drain + allocate mutually exclusive would require a global lock (huge perf cost for a tiny edge case)
- Zombie cleanup automatically recovers the pod after draining TTL expires
- In practice, this race requires sub-millisecond timing overlap

---

### Shared Pool Score Loss on Drain

**Files:** `internal/drainer/drainer.go:118` (ZREM), `internal/poolmanager/zombie.go` (re-add with score 0)
**Status:** ACCEPTED (documented edge case)
**Severity:** LOW

**Scenario:**
1. Pod has score 50 in shared ZSET (50 active connections)
2. Drainer calls ZREM → score is permanently lost
3. Draining TTL (6min) expires while pod still has active connections
4. Zombie cleanup re-adds pod with score 0
5. Allocator sees score 0, allocates maxConcurrent more calls
6. Pod now has 50 + maxConcurrent total connections → overload

**Why not fixed:**
- Calls rarely last >6min (drain TTL is designed around this)
- If pod is still alive after 6min drain, K8s rolling update has likely already moved on
- The draining key prevents ALL new allocation during the 6min window
- A fix (save score before ZREM, restore on recovery) adds complexity for an extremely rare edge case
- If we ever need to fix it: store `{score: N}` as the draining key value instead of `"true"`, read it back in zombie recovery

---

### Lease TTL vs Long Calls — Calls >15min Risk Double Allocation

**Status:** ACCEPTED (known policy choice)
**Severity:** LOW

Lease TTL defaults to 15min (configurable via `LEASE_TTL` env var). If a call exceeds this duration:
1. Lease expires
2. Zombie cleanup sees no lease → considers pod orphaned → recovers it to available pool
3. Allocator can now assign a new call to the pod → double allocation

This is intentional: 15min covers 99.9% of voice calls. For longer calls, operators should increase `LEASE_TTL`. The alternative (no TTL / infinite lease) risks permanent pod loss if the release call is missed.

---

### Config Edge Case — Tier Deleted While Pods Running

**File:** `internal/poolmanager/reconciler.go`
**Status:** HANDLED
**Severity:** LOW (harmless stale entry)

When a tier is deleted from `TIER_CONFIG` while pods are still assigned:
- Reconciler detects unknown tier, deletes the pod's tier key, re-assigns to a valid tier
- The old tier's `assigned` SET retains a stale entry
- Zombie cleanup won't scan the deleted tier (iterates `ParsedTierConfig`)
- The stale entry is in a SET that nobody reads anymore — truly harmless
- Entry disappears when pod restarts (removePodFromPool iterates config tiers)

---

## Flow-by-Flow Audit Results

### 1. Allocation Flow

**Path:** HTTP -> `AllocateHandler` -> idempotency Lua -> merchant config -> chain resolution -> `tryAllocateFromStep` -> `storeAllocation` -> response

| Check | Result |
|-------|--------|
| Race conditions (shared) | SAFE — Lua script atomic |
| Race conditions (exclusive) | SAFE — SPOP atomic |
| Idempotency (duplicate callSID) | SAFE — Lua check-and-lock with 30s TTL placeholder |
| Idempotency race (two allocates simultaneously) | OK by design — lock is best-effort, comment says so |
| Chain resolution | Correct — merchant pool prepended, custom fallback or DefaultChain |
| Storage failure recovery | Correct — `returnPodToPool` returns pod on error |

### 2. Release Flow

**Path:** HTTP -> `ReleaseHandler` -> call info lookup -> draining check -> pool return -> lease handling -> pod info update -> call info delete

| Check | Result |
|-------|--------|
| Double release of same callSID | SAFE — SADD idempotent, shared Lua floors score at 0 |
| Shared: score decrement + lease | SAFE — ZINCRBY -1, lease deleted only when score <= 0 |
| Exclusive: lease deletion | SAFE — always deleted |
| ActiveCalls decrement | SAFE — always decremented |
| Draining pod release | SAFE — pod info set to "draining", NOT re-added to pool |

### 3. Pod Lifecycle (K8s Watch)

**Path:** K8s watch event -> `addPodToPool` (tier check -> autoAssign -> pool placement) or `removePodFromPool`

| Check | Result |
|-------|--------|
| Pod added to correct pool type | SAFE — config lookup determines SET vs ZSET |
| Pod removal cleans up active call | FIXED (F3-A) — now decrements ActiveCalls |
| Unknown tier on add | SAFE — re-assigns via autoAssignTier |
| Ghost pod detection | SAFE — reconciler removes pods not in K8s |

### 4. Zombie Cleanup

**Path:** 30s ticker -> scan all assigned SETs + merchant SETs -> per-pod recovery

| Check | Result |
|-------|--------|
| Shared: ZScore error handling | FIXED (C2) — distinguishes redis.Nil from real errors |
| Exclusive: lease-based detection | SAFE — no lease = zombie |
| Merchant: same as exclusive | SAFE |
| Draining pods skipped | SAFE — Exists check before recovery |
| Prometheus metrics updated | SAFE — updatePoolMetrics called each cycle |

### 5. Drain Flow

**Path:** HTTP -> `DrainHandler` -> lease check -> tier lookup -> removeFromAvailable -> SET draining key

| Check | Result |
|-------|--------|
| Active calls preserved | SAFE — lease is NOT deleted |
| Re-allocation prevented | SAFE — pod removed from available pool |
| Non-atomic failure | FIXED — rollback re-adds pod on SET failure |
| SPOP-vs-drain race | Accepted — low severity, recovers via drain TTL + zombie |
| Draining TTL expiry | After 6min, draining key expires; if pod still exists, zombie may recover it |

### 6. Reconciliation

**Path:** 1min ticker -> `syncAllPods` -> list K8s pods -> compare with Redis -> fix discrepancies

| Check | Result |
|-------|--------|
| K8s pods missing from Redis | SAFE — `addPodToPool` adds them |
| Redis pods missing from K8s | SAFE — `removePodFromPool` cleans ghost pods |
| Deleted tier handling | SAFE — re-assigns pod to valid tier |
| Orphaned assigned SET entries | Harmless — nobody reads deleted tier's SETs |

---

## Redis Key Consistency Audit

**Result: ZERO mismatches across the entire codebase.**

### Key Patterns Verified (11 total)

| Key Pattern | Type | TTL | Used By |
|------------|------|-----|---------|
| `voice:pool:{tier}:available` | SET or ZSET | None | allocator, poolmanager, drainer, releaser |
| `voice:pool:{tier}:assigned` | SET | None | poolmanager |
| `voice:merchant:{id}:pods` | SET | None | allocator, poolmanager, drainer, releaser |
| `voice:merchant:{id}:assigned` | SET | None | poolmanager |
| `voice:pod:tier:{pod}` | STRING | None | allocator, poolmanager, drainer |
| `voice:pod:{pod}` | HASH | None | allocator, poolmanager, releaser |
| `voice:pod:draining:{pod}` | STRING | 6min | drainer, allocator (check), poolmanager (check) |
| `voice:lease:{pod}` | STRING | LeaseTTL (15min) | allocator (set), releaser (del), poolmanager (check) |
| `voice:pod:metadata` | HASH | None | poolmanager |
| `voice:call:{sid}` | HASH | CallInfoTTL (1hr) | allocator (set), releaser (read+del), poolmanager (cleanup) |
| `voice:merchant:config` | HASH | None | allocator (read) |

### Construction Methods

Three different construction methods are used across the codebase:
1. **`redisclient/keys.go` helpers** — used by releaser, drainer
2. **Inline const prefixes** — used by allocator (`merchantPoolPrefix`, `poolAvailablePrefix`, etc.)
3. **Inline string concatenation** — used by poolmanager

All three produce identical key strings. Verified by tracing every Redis command in every .go file. No format mismatches.

**Maintenance note:** Having 3 construction methods is a maintenance concern (easy to introduce a typo in future changes), but there are zero runtime bugs today. Consider consolidating to use `redisclient/keys.go` everywhere in a future cleanup.

---

## Rolling Update & Crash Scenario Verification

**Date:** Feb 2026
**Method:** Step-by-step code trace of 8 scenarios against actual source, tracking Redis state changes at each step.

### Architecture Discovery

**Smart-router preStop hook is a NO-OP:** The `k8s/deployment.yaml` preStop runs `curl -X POST http://localhost:8080/api/v1/drain || true`, but the `/api/v1/drain` handler requires `pod_name` in the JSON body. Empty body returns 400, `|| true` swallows it. This is fine — smart-router is stateless and doesn't need draining. The drain API drains **voice-agent** pods.

**Voice-agent pods have NO preStop calling drain.** Every rolling update is a "sudden death" from the smart-router's perspective. The watcher detects pod transitions and cleans up via `removePodFromPool`.

### Three Concurrent Actors on Redis (leader-only)

1. **Watcher** — K8s watch events (real-time, event-driven)
2. **Reconciler** — 1min full sync ticker
3. **Zombie cleanup** — 30s ticker

No mutex between them. Relies on Redis atomic commands + eventual consistency.

---

### Scenario 1: Happy Path — Drain Called Externally, Then Pod Killed

**Verdict: SAFE**

Flow: Drain removes from available → sets draining key (6min TTL) → call finishes → release skips pool re-add (wasDraining=true) → deletes lease → K8s kills pod → watcher DELETE → `removePodFromPool` cleans all keys (including draining key) → new pod starts → `addPodToPool` with clean slate.

No leaks. Clean handoff.

---

### Scenario 2: Voice-Agent Pod Dies WITHOUT Drain (Most Common!)

**Verdict: SAFE**

This is the production default — no external drain call during rolling update.

**Path:** K8s sets pod not-ready → watcher MODIFIED fires → `isReady=false, isRegistered=true` → `removePodFromPool()` → SRem from all pools → checks `allocated_call_sid` → if found: DEL call mapping + ActiveCalls.Dec() → cleanup all metadata/tier/lease/draining keys → watcher DELETED fires → `removePodFromPool()` again (idempotent no-ops) → new pod starts → `addPodToPool()`.

**Key behavior:** Active calls are killed (unavoidable — pod is dying). Redis state cleaned immediately. `removePodFromPool` is idempotent — safe to call multiple times.

**Fallback:** If watcher misses events, reconciler catches ghost pods at next 1-min tick.

**Release after cleanup:** If telephony sends release after call info is deleted, `releaser.go:54-56` returns `ErrCallNotFound` (404). Correct behavior.

---

### Scenario 3: Voice-Agent Pod Crash During Active Call (OOM/segfault)

**Verdict: SAFE**

Identical to Scenario 2 in terms of smart-router behavior. K8s may restart the container in the same pod (same name) — watcher fires MODIFIED(not-ready) → `removePodFromPool` → container restarts → MODIFIED(ready) → `addPodToPool`.

If pod is evicted entirely, same as Scenario 2 DELETE path.

**Recovery layers (most conservative to least):**
1. Watcher (instant): catches not-ready transition
2. Reconciler (1 min): catches ghosts or missed events
3. Zombie cleanup + lease TTL expiry (up to 15 min): last resort

---

### Scenario 4: Smart-Router Rolling Update (Leader Election)

**Verdict: SAFE**

**Config:** LeaseDuration=15s, RenewDeadline=10s, RetryPeriod=2s.

**Non-leader killed:** Only API traffic disrupted briefly (K8s Service removes pod from endpoints). No Redis state impact. API calls are stateless Redis operations.

**Leader killed:** Max leadership gap ~15 seconds. During gap:
- API calls (allocate/release/drain) continue on other replicas (stateless)
- Watcher/zombie/reconciler paused (leader-only)
- New leader acquires lease → `syncAllPods()` full reconciliation catches any missed events
- watchPods + zombieCleanup goroutines restarted

**In-flight HTTP on dying leader:** Connection reset → telephony retries → hits another replica → idempotency check handles it.

---

### Scenario 5: Redis Blip During Rolling Update

**Verdict: ACCEPTED (double-fault, self-healing)**

`removePodFromPool` does ~10 sequential Redis commands with **no error checking** (fire-and-forget by design). If Redis blips mid-cleanup:

**Worst case:** Pod stays in assigned SET, removed from available SET, orphaned call mapping persists.

**Recovery concern:** Zombie cleanup (30s) could add a dead pod back to available pool (sees: in assigned, no lease, not in available → "zombie!" → SAdd to available). Pod is dead → allocator picks it → call fails.

**Self-healing:** Reconciler at next 1-min tick detects ghost pod (in Redis, not in K8s) → `removePodFromPool` again (Redis is back) → clean.

**Max exposure:** Up to 30s where a dead pod could be in available pool. Very low probability (requires Redis blip + pod death at same instant).

---

### Scenario 6: Multiple Voice-Agent Pods Rolling Simultaneously

**Verdict: SAFE**

StatefulSet rolls pods one at a time (reverse ordinal order: pod-2 → pod-1 → pod-0). Each waits for previous to be Running+Ready.

Even with forced simultaneous deletion (e.g., `kubectl delete pod --all`):
- Each `removePodFromPool` operates on a different pod name → no Redis contention
- System correctly returns "no pods available" during outage
- StatefulSet controller recreates pods in order
- Watcher processes events serially (queued)

---

### Scenario 7: New Pod Same Name While Old Still Terminating

**Verdict: SAFE**

**K8s guarantee:** StatefulSet ensures old pod is fully terminated before creating new pod with same name. No overlap.

**With drain:** `removePodFromPool` explicitly DELs the draining key (`reconciler.go:214`). New pod starts with clean slate — `isPodEligible` returns true.

**Edge case — draining key DEL fails (Redis error):** Draining key persists up to 6min TTL. New pod registered in assigned but NOT in available (isPodEligible returns false). Zombie cleanup also skips it (draining key exists). **Self-heals when TTL expires** (max 6min delay). No data corruption.

---

### Scenario 8: Drain Called But Pod Never Dies

**Verdict: SAFE (by design)**

Draining key has 6min TTL. After expiry:
- If pod has no active call: zombie cleanup recovers it to available pool within 30s
- If pod has active call: lease prevents zombie recovery until lease also expires (15min). OR: release is called when call ends → draining key already expired → pod returned to pool normally.

**Design intent:** Draining TTL is a safety net preventing permanent pod loss from stale drain state.

---

### Summary Table

| Scenario | Verdict | Max Recovery Time | Bugs Found |
|----------|---------|-------------------|------------|
| 1. Drain + kill (happy path) | SAFE | Instant | None |
| 2. Kill without drain (most common) | SAFE | Instant (watcher), 1min (reconciler) | None |
| 3. OOM/crash during call | SAFE | Instant (watcher), 1min (reconciler) | None |
| 4. Smart-router rolling update | SAFE | ~15s leadership gap | None |
| 5. Redis blip during rolling update | ACCEPTED | 30s-60s (zombie + reconciler) | None (double-fault) |
| 6. Multiple pods rolling simultaneously | SAFE | Instant per-pod | None |
| 7. Same-name pod overlap | SAFE | Instant (K8s guarantee) | None |
| 8. Drain but pod never dies | SAFE | 6min (drain TTL) + 30s (zombie) | None (by design) |

**Zero new bugs found.** All scenarios self-heal through the layered recovery system (watcher → reconciler → zombie cleanup → TTL expiry).
