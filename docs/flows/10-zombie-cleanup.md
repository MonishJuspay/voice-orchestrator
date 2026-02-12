# Flow 10: Zombie Cleanup

**Source file:** `internal/poolmanager/zombie.go` (253 lines)

## Overview

Zombie cleanup is the third and final recovery layer in the smart router. It runs periodically on the **leader replica only** and scans all assigned pods in Redis to find "zombies" — pods that are in the assigned set but not in the available pool and don't have an active lease. These pods were likely allocated for a call that ended without a proper release (crash, timeout, network partition).

The three recovery layers operate at different timescales:
1. **Watcher** (instant) — K8s watch events for pod add/delete
2. **Reconciler** (every 1 min) — full K8s-vs-Redis diff for missed events
3. **Zombie Cleanup** (every 30s) — Redis-only scan for orphaned pods

Zombie cleanup is unique because it operates **entirely within Redis** — it doesn't query Kubernetes. It detects pods that are in the wrong Redis state (assigned but not available, with no lease to justify the assignment).

## How It's Started

```
manager.go → runLeaderWorkload() → safeGo("zombie-cleanup", m.runZombieCleanup)
```

Only the leader smart-router replica runs zombie cleanup. Started as a goroutine via `safeGo()` (with exponential backoff restart on panic). Stopped when leadership is lost (context cancelled).

## Loop Structure — `runZombieCleanup()` (zombie.go:16-31)

```go
ticker := time.NewTicker(m.config.CleanupInterval)    // Default: 30 seconds
defer ticker.Stop()

for {
    select {
    case <-ctx.Done():
        return
    case <-ticker.C:
        m.cleanupZombies(ctx)
    }
}
```

Simple ticker loop. Every `CleanupInterval` (configurable via `CLEANUP_INTERVAL` env var, default 30s), runs `cleanupZombies()`.

## Core Logic — `cleanupZombies()` (zombie.go:45-223)

### Phase 1: Scan All Assigned Pods (zombie.go:52-76)

First, builds a complete map of all pods in all assigned sets:

```go
type podEntry struct {
    tier       string    // tier name or merchant ID
    isMerchant bool
}
allAssignedPods := make(map[string]podEntry)
```

Scans two types of assigned sets:

```go
for tier, cfg := range m.config.ParsedTierConfig {
    // Regular tier pools
    scanSet("voice:pool:"+tier+":assigned", podEntry{tier: tier})

    // Merchant pools (only for non-shared tiers)
    if cfg.Type != config.TierTypeShared {
        scanSet("voice:merchant:"+tier+":assigned", podEntry{tier: tier, isMerchant: true})
    }
}
```

**`scanSet()`** (zombie.go:54-66): `SMEMBERS` on the assigned key, adds each member to the map.

**With production config (gold, standard, basic):**
- `SMEMBERS voice:pool:gold:assigned` — e.g. `{"voice-agent-0"}`
- `SMEMBERS voice:pool:standard:assigned` — e.g. `{"voice-agent-1"}`
- `SMEMBERS voice:pool:basic:assigned` — e.g. `{"voice-agent-2"}`
- `SMEMBERS voice:merchant:gold:assigned` — merchant pods in gold tier
- `SMEMBERS voice:merchant:standard:assigned` — merchant pods in standard tier

**Redis keys READ:** `voice:pool:{tier}:assigned` (SET), `voice:merchant:{tier}:assigned` (SET)

---

### Phase 1.5: Update Pool Metrics (zombie.go:79)

```go
m.updatePoolMetrics(ctx)
```

Piggybacks on the zombie cycle to update Prometheus gauges for every tier:

```go
for tier, cfg := range m.config.ParsedTierConfig {
    assigned = SCARD voice:pool:{tier}:assigned
    middleware.PoolAssignedPods.WithLabelValues(tier).Set(assigned)

    available = SCARD/ZCARD voice:pool:{tier}:available
    middleware.PoolAvailablePods.WithLabelValues(tier).Set(available)
}
```

---

### Phase 2: Check Each Pod (zombie.go:83-215)

For each pod in the assigned map, runs the appropriate zombie check:

```
┌─────────────────────┐
│ For each assigned pod│
└──────────┬──────────┘
           │
    ┌──────▼──────────┐
    │ EXISTS draining? │── YES ──► skip (not a zombie)
    └──────┬──────────┘
           │ NO
           │
    ┌──────▼──────────┐
    │ Is merchant?     │── YES ──► Merchant zombie check
    └──────┬──────────┘
           │ NO
           │
    ┌──────▼──────────┐
    │ Is shared?       │── YES ──► Shared zombie check
    └──────┬──────────┘
           │ NO
           │
           └──► Exclusive zombie check
```

#### Common: Skip Draining Pods (zombie.go:84-95)

```go
isDraining, err := m.redis.Exists(ctx, "voice:pod:draining:"+podName).Result()
if isDraining > 0 {
    continue    // Draining pods are expected to be out of the available pool
}
```

Draining pods are intentionally removed from the available pool by the drain flow. They're not zombies.

#### Path A: Exclusive Tier Zombie Check (zombie.go:178-213)

For regular exclusive tiers (e.g. gold, standard):

```
1. EXISTS voice:lease:{pod}
   └─ YES → active call, not a zombie → skip

2. SISMEMBER voice:pool:{tier}:available {pod}
   └─ YES → already in available pool → skip

3. isPodEligible(pod)?
   └─ NO → has lease or draining → skip
   └─ YES → ZOMBIE! → SADD voice:pool:{tier}:available {pod}
                     → ActiveCalls.Dec()
                     → ZombiesRecoveredTotal.Inc()
```

**What makes a pod a zombie?** It's in the assigned set (meaning it was given to a tier) but:
- No active lease (call ended or lease expired)
- Not in the available pool (wasn't returned after the call)
- Not draining (not being shut down)
- Passes `isPodEligible()` (no lease + not draining — double-checks)

**`isPodEligible()`** (watcher.go:162-184):
```go
func (m *Manager) isPodEligible(ctx context.Context, podName string) bool {
    // No active lease?
    hasLease := EXISTS voice:lease:{pod}
    if hasLease > 0 { return false }

    // Not draining?
    isDraining := EXISTS voice:pod:draining:{pod}
    if isDraining > 0 { return false }

    return true
}
```

This is a defense-in-depth check — lease and draining were already checked, but `isPodEligible()` re-verifies atomically at the moment of recovery.

**Recovery action:** `SADD voice:pool:{tier}:available {podName}` — puts the pod back in the available pool.

**`ActiveCalls.Dec()`** — the zombie pod was counted as having an active call when it was allocated. Since the call is gone (no lease), we decrement the gauge.

#### Path B: Shared Tier Zombie Check (zombie.go:144-176)

For shared tiers (e.g. basic):

```
1. ZSCORE voice:pool:{tier}:available {pod}
   ├─ Real Redis error → skip (don't treat as missing!)
   ├─ Score exists → pod is in ZSET, not a zombie → skip
   └─ redis.Nil → pod MISSING from ZSET → ZOMBIE!
       → ZADD voice:pool:{tier}:available {pod} score=0
       → ZombiesRecoveredTotal.Inc()
```

**Key difference from exclusive:** Shared pods don't get a lease check. Why?

Shared pods legitimately have leases while handling calls — they can handle multiple concurrent calls. A lease doesn't mean the pod is "out of pool" — shared pods stay in the ZSET the entire time (their score tracks call count). The only zombie condition is being **missing from the ZSET entirely** while being in the assigned set.

**Critical: `redis.Nil` vs real Redis error** (zombie.go:152-163):

```go
_, err := m.redis.ZScore(ctx, availKey, podName).Result()
if err != nil && err != redis.Nil {
    // REAL Redis error — skip this pod!
    continue
}
if err == redis.Nil {
    // Truly missing — recover
    m.redis.ZAdd(ctx, availKey, redis.Z{Score: 0, Member: podName})
}
```

This distinction was a critical bug fix (Phase 7, C2 — "Zombie Storm"). If a Redis connection blip returns an error, we must NOT treat the pod as missing — resetting its score to 0 while it has active calls would cause the allocator to over-allocate that pod, creating a "zombie storm."

**No `ActiveCalls.Dec()`** for shared zombies — because shared pods may still have active calls. The score reset to 0 is approximate; the next allocation will increment it correctly.

#### Path C: Merchant Pool Zombie Check (zombie.go:97-136)

For merchant dedicated pools — same logic as exclusive:

```
1. EXISTS voice:lease:{pod}
   └─ YES → active call, not a zombie → skip

2. SISMEMBER voice:merchant:{id}:pods {pod}
   └─ YES → already in available pool → skip

3. isPodEligible(pod)?
   └─ YES → ZOMBIE! → SADD voice:merchant:{id}:pods {pod}
                     → ActiveCalls.Dec()
                     → ZombiesRecoveredTotal.Inc()
```

Identical to exclusive tier check, just uses the merchant pool key.

---

## Complete Sequence Diagram — One Zombie Recovery Cycle

```
Leader Smart Router                                          Redis
       │                                                       │
       │  ── Every 30 seconds ──                               │
       │                                                       │
       │  SMEMBERS voice:pool:gold:assigned                    │
       ├──────────────────────────────────────────────────────►│
       │  ["voice-agent-0"]                                    │
       │◄──────────────────────────────────────────────────────┤
       │                                                       │
       │  SMEMBERS voice:pool:standard:assigned                │
       ├──────────────────────────────────────────────────────►│
       │  ["voice-agent-1"]                                    │
       │◄──────────────────────────────────────────────────────┤
       │                                                       │
       │  SMEMBERS voice:pool:basic:assigned                   │
       ├──────────────────────────────────────────────────────►│
       │  ["voice-agent-2"]                                    │
       │◄──────────────────────────────────────────────────────┤
       │                                                       │
       │  updatePoolMetrics() — SCARD/ZCARD for each tier      │
       ├──────────────────────────────────────────────────────►│
       │◄──────────────────────────────────────────────────────┤
       │                                                       │
       │  ── Check voice-agent-0 (gold, exclusive) ──          │
       │                                                       │
       │  EXISTS voice:pod:draining:voice-agent-0              │
       ├──────────────────────────────────────────────────────►│
       │  0 (not draining)                                     │
       │◄──────────────────────────────────────────────────────┤
       │                                                       │
       │  EXISTS voice:lease:voice-agent-0                     │
       ├──────────────────────────────────────────────────────►│
       │  0 (no lease — potential zombie!)                     │
       │◄──────────────────────────────────────────────────────┤
       │                                                       │
       │  SISMEMBER voice:pool:gold:available voice-agent-0    │
       ├──────────────────────────────────────────────────────►│
       │  0 (NOT in available pool — ZOMBIE!)                  │
       │◄──────────────────────────────────────────────────────┤
       │                                                       │
       │  isPodEligible() — double check lease + draining      │
       ├──────────────────────────────────────────────────────►│
       │  true                                                 │
       │◄──────────────────────────────────────────────────────┤
       │                                                       │
       │  SADD voice:pool:gold:available "voice-agent-0"       │
       ├──────────────────────────────────────────────────────►│
       │  1 (recovered!)                                       │
       │◄──────────────────────────────────────────────────────┤
       │                                                       │
       │  [ActiveCalls.Dec() + ZombiesRecoveredTotal.Inc()]    │
```

---

## Redis Keys — Complete Reference

### Read

| Key | Type | When | Purpose |
|-----|------|------|---------|
| `voice:pool:{tier}:assigned` | SET | Phase 1 | SMEMBERS — get all assigned pods per tier |
| `voice:merchant:{tier}:assigned` | SET | Phase 1 | SMEMBERS — get all merchant-assigned pods |
| `voice:pod:draining:{pod}` | STRING | Phase 2 | EXISTS — skip draining pods |
| `voice:lease:{pod}` | STRING | Phase 2 (exclusive/merchant) | EXISTS — check for active call |
| `voice:pool:{tier}:available` | SET | Phase 2 (exclusive) | SISMEMBER — check if already available |
| `voice:pool:{tier}:available` | ZSET | Phase 2 (shared) | ZSCORE — check if in ZSET |
| `voice:merchant:{id}:pods` | SET | Phase 2 (merchant) | SISMEMBER — check if already available |
| `voice:pool:{tier}:assigned` | SET | Metrics | SCARD — count for gauge |
| `voice:pool:{tier}:available` | SET/ZSET | Metrics | SCARD/ZCARD — count for gauge |

### Write

| Key | Type | When | Purpose |
|-----|------|------|---------|
| `voice:pool:{tier}:available` | SET | Exclusive recovery | SADD — return pod to available pool |
| `voice:pool:{tier}:available` | ZSET | Shared recovery | ZADD score=0 — return pod to ZSET |
| `voice:merchant:{id}:pods` | SET | Merchant recovery | SADD — return pod to merchant pool |

---

## When Are Zombies Created?

Zombies appear when the normal allocate→release lifecycle is interrupted:

| Scenario | What Goes Wrong | How Zombie Cleanup Fixes It |
|----------|----------------|---------------------------|
| Voice-agent crashes mid-call | Release never fires. Lease eventually expires (15min). | Zombie sees: assigned + no lease + not available → recovers |
| Smart-router crashes during allocation | Pod SPOP'd from pool, but `storeAllocation()` failed and `returnPodToPool()` also failed | Zombie sees: assigned + no lease + not available → recovers |
| Network partition between voice-agent and smart-router | Release HTTP call fails/times out | Zombie sees: assigned + no lease (after 15min TTL) + not available → recovers |
| Bug in release code | Release doesn't return pod to pool | Same detection pattern |
| Redis blip during release | SADD/ZINCRBY fails on pool return | Same detection pattern |

---

## Prometheus Metrics Updated

| Metric | Type | When |
|--------|------|------|
| `zombies_recovered_total` | Counter | Each zombie recovered |
| `active_calls` | Gauge | Dec'd for exclusive/merchant zombies (not shared) |
| `pool_available_pods{tier}` | Gauge | Updated every cycle |
| `pool_assigned_pods{tier}` | Gauge | Updated every cycle |

---

## Edge Cases

1. **Redis error on SMEMBERS:** That tier's assigned set is skipped entirely. Logged as warning. Other tiers still checked.

2. **Redis error on EXISTS (draining/lease):** Pod is skipped (continue to next). Conservative: don't recover a pod we can't fully verify.

3. **Redis error on ZSCORE (shared pool):** Pod is skipped. This is the "zombie storm" prevention — we do NOT treat a Redis error as "pod missing from ZSET."

4. **Pod recovered but dies immediately after:** Next reconciler cycle removes it. The SADD/ZADD is harmless if the pod is about to be removed.

5. **Concurrent zombie cleanup and allocation:** SADD/ZADD and SPOP/ZINCRBY are atomic operations. If zombie recovery adds a pod to the available pool at the same moment an allocator SPOP's from it, the operations are serialized by Redis.

6. **Shared pod with score reset to 0:** If a shared pod is recovered with score 0 but actually has active calls, the score will be wrong (too low). This means the allocator might allocate more calls to it than intended. However, this only happens if the pod was somehow removed from the ZSET (a rare condition) — and having slightly wrong scores is better than the pod being permanently invisible.

7. **Leader failover during zombie cleanup:** Context is cancelled → cleanup stops mid-cycle. New leader starts a fresh cycle. No harm: all operations are idempotent (SADD to a set that already has the member is a no-op).

---

## Interaction with Other Flows

| Flow | Relationship |
|------|-------------|
| [03 - Leader Election](./03-leader-election.md) | Zombie cleanup only runs on the leader |
| [07 - Allocation](./07-allocation.md) | Creates the lease that zombie cleanup checks for active calls |
| [08 - Release](./08-release.md) | Normal release path that, if it works, prevents zombies from appearing |
| [09 - Drain](./09-drain.md) | Sets draining key → zombie cleanup skips draining pods |
| [05 - Reconciler](./05-reconciler.md) | Complementary: reconciler handles K8s-vs-Redis sync, zombie handles Redis-internal orphans |
| [13 - Metrics](./13-metrics.md) | Updates pool gauges and zombie counter every cycle |
