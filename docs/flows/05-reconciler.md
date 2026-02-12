# Flow 05: Pod Discovery — Reconciler

**Source file:** `internal/poolmanager/reconciler.go` (274 lines)

## Overview

The Reconciler performs **full state synchronization** between Kubernetes and Redis. Unlike the Watcher (which processes individual events), the Reconciler lists ALL pods from K8s and ALL pods from Redis, then fixes every discrepancy.

It runs:
1. **Once at leader startup** — ensures Redis is correct before accepting events
2. **Every 60 seconds** — catches drift from missed events, Redis data loss, etc.

The Reconciler also contains the `addPodToPool()` and `removePodFromPool()` functions that are shared with the Watcher.

## How It's Started

```
manager.go:120 → m.syncAllPods(ctx)        // Initial sync
manager.go:138 → syncTicker → m.syncAllPods(ctx)  // Every 60s
```

## syncAllPods() — Full Reconciliation (line 18-61)

### Step 1: List K8s Pods

```go
k8sPods, err := m.k8sClient.CoreV1().Pods(m.config.Namespace).List(ctx, metav1.ListOptions{
    LabelSelector: m.config.PodLabelSelector,   // "app=voice-agent"
})
```

Gets ALL pods matching the label selector from the K8s API (not a watch — a full LIST).

### Step 2: Build K8s Pod Map

```go
k8sPodMap := make(map[string]*corev1.Pod)
for i := range k8sPods.Items {
    pod := &k8sPods.Items[i]
    k8sPodMap[pod.Name] = pod
}
```

### Step 3: Get All Redis Pods

```go
allRedisPods := m.getAllRedisPods(ctx)   // returns map[string]bool
```

Scans every configured tier's `assigned` SET (both regular and merchant) to build a complete set of pod names that Redis knows about.

### Step 4: Reconcile K8s → Redis

```go
for name, pod := range k8sPodMap {
    if m.isPodReady(pod) && pod.Status.PodIP != "" {
        m.addPodToPool(ctx, pod)      // Ensure pod is in Redis
    } else {
        m.removePodFromPool(ctx, pod) // Pod not ready, remove from Redis
    }
    delete(allRedisPods, name)        // Mark as reconciled
}
```

For every pod in K8s:
- If ready + has IP → ensure it's in Redis (adds if missing, ensures correct pool if exists)
- If not ready → ensure it's NOT in Redis
- Remove from `allRedisPods` map (processed)

### Step 5: Remove Ghost Pods

```go
for ghostPodName := range allRedisPods {
    m.logger.Warn("Found ghost pod in Redis", zap.String("pod", ghostPodName))
    dummyPod := &corev1.Pod{
        ObjectMeta: metav1.ObjectMeta{Name: ghostPodName},
    }
    m.removePodFromPool(ctx, dummyPod)
}
```

Any pods remaining in `allRedisPods` after processing K8s pods are **ghost pods** — they exist in Redis but NOT in K8s. These are removed.

**When do ghosts appear?**
- Pod was deleted but watcher missed the event (network blip)
- Redis was restored from a backup with stale data
- Manual Redis key creation (debugging)

## getAllRedisPods() — Scan Redis State (line 64-91)

```go
func (m *Manager) getAllRedisPods(ctx context.Context) map[string]bool {
    pods := make(map[string]bool)

    for tier, cfg := range m.config.ParsedTierConfig {
        scanSet("voice:pool:" + tier + ":assigned")          // Regular tier assigned
        if cfg.Type != config.TierTypeShared {
            scanSet("voice:merchant:" + tier + ":assigned")  // Merchant assigned
        }
    }

    return pods
}
```

**Redis keys READ:** `voice:pool:{tier}:assigned` and `voice:merchant:{tier}:assigned` for every configured tier.

Uses `SMEMBERS` to get all members of each SET.

## addPodToPool() — Add/Ensure Pod in Redis (line 94-169)

This is the **most important function** in pool management. Called by both watcher and reconciler.

### Step 1: Check Existing Tier Assignment

```go
existingTier, err := m.redis.Get(ctx, "voice:pod:tier:"+podName).Result()
if err == nil && existingTier != "" {
    // Pod already has a tier
    if !config.IsMerchantTier(existingTier) && !m.config.IsKnownTier(existingTier) {
        // Tier was removed from config → re-assign
        m.redis.Del(ctx, "voice:pod:tier:"+podName)
    } else {
        m.ensurePodInPool(ctx, podName, existingTier)
        return   // Already assigned, just ensure it's in the right pool
    }
}
```

**Redis key READ:** `voice:pod:tier:{pod}`

Three outcomes:
1. Pod has valid tier → `ensurePodInPool()` and return
2. Pod has stale tier (removed from config) → delete tier key, fall through to re-assign
3. Pod has no tier → fall through to auto-assign

### Step 2: Auto-Assign Tier

```go
assignedTier, isMerchant := m.autoAssignTier(ctx, podName)
```

See [Flow 06: Tier Assignment](./06-tier-assignment.md) for full logic.

### Step 3: Store Metadata

```go
metadata := models.PodMetadata{Tier: assignedTier, Name: pod.Name}
metadataJSON, _ := json.Marshal(metadata)
m.redis.HSet(ctx, "voice:pod:metadata", podName, string(metadataJSON))
```

**Redis key WRITE:** `voice:pod:metadata` (HASH, field=podName, value=JSON)

### Step 4: Add to Pool (Merchant or Regular)

**Merchant path:**
```go
if isMerchant {
    m.redis.SAdd(ctx, "voice:merchant:"+assignedTier+":assigned", podName)
    m.redis.Set(ctx, "voice:pod:tier:"+podName, "merchant:"+assignedTier, 0)
    if m.isPodEligible(ctx, podName) {
        m.redis.SAdd(ctx, "voice:merchant:"+assignedTier+":pods", podName)
    }
}
```

Redis keys WRITTEN:
- `voice:merchant:{id}:assigned` — SADD pod to merchant assigned SET
- `voice:pod:tier:{pod}` — SET to `"merchant:{id}"`
- `voice:merchant:{id}:pods` — SADD pod to merchant available SET (if eligible)

**Regular pool path (exclusive):**
```go
m.redis.SAdd(ctx, "voice:pool:"+assignedTier+":assigned", podName)
m.redis.Set(ctx, "voice:pod:tier:"+podName, assignedTier, 0)
if m.isPodEligible(ctx, podName) {
    m.redis.SAdd(ctx, "voice:pool:"+assignedTier+":available", podName)
}
```

**Regular pool path (shared):**
```go
m.redis.SAdd(ctx, "voice:pool:"+assignedTier+":assigned", podName)
m.redis.Set(ctx, "voice:pod:tier:"+podName, assignedTier, 0)
if m.isPodEligible(ctx, podName) {
    m.redis.ZAddNX(ctx, "voice:pool:"+assignedTier+":available", redis.Z{
        Score:  0,
        Member: podName,
    })
}
```

**Key difference for shared:** Uses `ZAddNX` (add only if not exists) with score 0. This prevents resetting a pod's score if it's already in the ZSET with active calls.

## removePodFromPool() — Full Cleanup (line 172-220)

Called when a pod is deleted, becomes unready, or is a ghost.

### Step 1: Get Pod's Tier (for logging)

```go
tier := m.getPodTier(ctx, podName)
```

### Step 2: Remove from ALL Pools

```go
for t, cfg := range m.config.ParsedTierConfig {
    m.redis.SRem(ctx, "voice:pool:"+t+":assigned", podName)
    m.redis.SRem(ctx, "voice:merchant:"+t+":assigned", podName)

    if cfg.Type == config.TierTypeShared {
        m.redis.ZRem(ctx, "voice:pool:"+t+":available", podName)   // ZSET
    } else {
        m.redis.SRem(ctx, "voice:pool:"+t+":available", podName)   // SET
        m.redis.SRem(ctx, "voice:merchant:"+t+":pods", podName)    // SET
    }
}
```

**Iterates ALL configured tiers** and removes from every possible pool. This is intentionally broad — even if we know the pod's tier, we clean everything to handle edge cases (tier changed, stale data).

**Type-aware commands:** Uses `ZRem` for shared available pools (ZSET) and `SRem` for exclusive available pools (SET) to avoid `WRONGTYPE` errors.

### Step 3: Clean Up Orphaned Call

```go
activeCallSID, err := m.redis.HGet(ctx, "voice:pod:"+podName, "allocated_call_sid").Result()
if err == nil && activeCallSID != "" {
    m.redis.Del(ctx, "voice:call:"+activeCallSID)
    middleware.ActiveCalls.Dec()
}
```

If the pod had an active call when it died:
- Deletes the `voice:call:{sid}` hash (call info)
- Decrements the `ActiveCalls` Prometheus gauge

**Redis keys READ:** `voice:pod:{pod}` (HASH, field `allocated_call_sid`)
**Redis keys DELETE:** `voice:call:{sid}`

### Step 4: Clean Up Metadata and Keys

```go
m.redis.HDel(ctx, "voice:pod:metadata", podName)
m.redis.Del(ctx, "voice:pod:"+podName)
m.redis.Del(ctx, "voice:pod:tier:"+podName)
m.redis.Del(ctx, "voice:lease:"+podName)
m.redis.Del(ctx, "voice:pod:draining:"+podName)
```

**Redis keys DELETED:**
- `voice:pod:metadata` — HDEL field for this pod
- `voice:pod:{pod}` — DELETE pod info hash
- `voice:pod:tier:{pod}` — DELETE tier mapping
- `voice:lease:{pod}` — DELETE active lease
- `voice:pod:draining:{pod}` — DELETE draining flag

## ensurePodInPool() — Idempotent Pool Maintenance (line 223-249)

Called when a pod already has a tier assignment. Ensures it's in the correct assigned and available pools.

```go
func (m *Manager) ensurePodInPool(ctx context.Context, podName, tier string) {
    if merchantID, ok := config.ParseMerchantTier(tier); ok {
        // Merchant: SADD to assigned + available (if eligible)
        m.redis.SAdd(ctx, "voice:merchant:"+merchantID+":assigned", podName)
        if m.isPodEligible(ctx, podName) {
            m.redis.SAdd(ctx, "voice:merchant:"+merchantID+":pods", podName)
        }
        return
    }

    // Regular tier
    m.redis.SAdd(ctx, "voice:pool:"+tier+":assigned", podName)
    if m.isPodEligible(ctx, podName) {
        if isShared {
            m.redis.ZAddNX(ctx, "voice:pool:"+tier+":available", redis.Z{Score: 0, Member: podName})
        } else {
            m.redis.SAdd(ctx, "voice:pool:"+tier+":available", podName)
        }
    }
}
```

`SADD` and `ZAddNX` are both idempotent — calling them on an already-existing member is a no-op.

## getPodTier() — Tier Lookup with Fallbacks (line 252-273)

```go
func (m *Manager) getPodTier(ctx context.Context, podName string) string {
    // 1. Direct tier key
    tier, err := m.redis.Get(ctx, "voice:pod:tier:"+podName).Result()
    if err == nil && tier != "" { return tier }

    // 2. Metadata hash
    metadataJSON, err := m.redis.HGet(ctx, "voice:pod:metadata", podName).Result()
    if err == nil {
        var metadata models.PodMetadata
        json.Unmarshal([]byte(metadataJSON), &metadata)
        if metadata.Tier != "" { return metadata.Tier }
    }

    // 3. Last resort: last tier in DefaultChain
    if len(m.config.DefaultChain) > 0 {
        return m.config.DefaultChain[len(m.config.DefaultChain)-1]
    }
    return "unknown"
}
```

Three-tier fallback for finding a pod's tier. Used mainly for logging in `removePodFromPool()`.

## Sequence Diagram — Full Reconciliation

```
        syncAllPods()
             │
             ├── K8s API: List Pods ──► [pod-A (Ready), pod-B (Ready), pod-C (Pending)]
             │
             ├── Redis: getAllRedisPods() ──► {pod-A, pod-B, pod-X}
             │
             ├── Process K8s pods:
             │   ├── pod-A: Ready + in Redis → ensurePodInPool() ✓
             │   ├── pod-B: Ready + in Redis → ensurePodInPool() ✓
             │   └── pod-C: Pending → removePodFromPool() (no-op if not in Redis)
             │
             ├── Ghost pods remaining: {pod-X}
             │   └── pod-X: In Redis, NOT in K8s → removePodFromPool() ← REMOVED
             │
             └── Log: "Reconciliation complete, k8s_pods=3, ghost_pods_removed=1"
```

## All Redis Keys Touched (Summary)

### Read

| Key | Type | Where |
|-----|------|-------|
| `voice:pool:{tier}:assigned` | SET | `getAllRedisPods`, `isPodRegistered` |
| `voice:merchant:{tier}:assigned` | SET | `getAllRedisPods`, `isPodRegistered` |
| `voice:pod:tier:{pod}` | STRING | `addPodToPool`, `getPodTier` |
| `voice:pod:metadata` | HASH | `getPodTier` |
| `voice:lease:{pod}` | STRING | `isPodEligible` |
| `voice:pod:draining:{pod}` | STRING | `isPodEligible` |
| `voice:pod:{pod}` | HASH | `removePodFromPool` (check active call) |

### Write

| Key | Type | Operation | Where |
|-----|------|-----------|-------|
| `voice:pool:{tier}:assigned` | SET | SADD | `addPodToPool`, `ensurePodInPool` |
| `voice:pool:{tier}:available` | SET/ZSET | SADD/ZAddNX | `addPodToPool`, `ensurePodInPool` |
| `voice:merchant:{id}:assigned` | SET | SADD | `addPodToPool`, `ensurePodInPool` |
| `voice:merchant:{id}:pods` | SET | SADD | `addPodToPool`, `ensurePodInPool` |
| `voice:pod:tier:{pod}` | STRING | SET | `addPodToPool` |
| `voice:pod:metadata` | HASH | HSET | `addPodToPool` |

### Delete

| Key | Type | Where |
|-----|------|-------|
| `voice:pool:{tier}:assigned` | SET | SREM in `removePodFromPool` |
| `voice:pool:{tier}:available` | SET/ZSET | SREM/ZRem in `removePodFromPool` |
| `voice:merchant:{tier}:assigned` | SET | SREM in `removePodFromPool` |
| `voice:merchant:{tier}:pods` | SET | SREM in `removePodFromPool` |
| `voice:pod:metadata` | HASH | HDEL in `removePodFromPool` |
| `voice:pod:{pod}` | HASH | DEL in `removePodFromPool` |
| `voice:pod:tier:{pod}` | STRING | DEL in `removePodFromPool` |
| `voice:lease:{pod}` | STRING | DEL in `removePodFromPool` |
| `voice:pod:draining:{pod}` | STRING | DEL in `removePodFromPool` |
| `voice:call:{sid}` | HASH | DEL in `removePodFromPool` (orphaned call cleanup) |

## Interaction with Other Flows

| Flow | Relationship |
|------|-------------|
| [03 - Leader Election](./03-leader-election.md) | `syncAllPods()` called from `runLeaderWorkload()` |
| [04 - Watcher](./04-pod-watcher.md) | Shares `addPodToPool()`, `removePodFromPool()`, `isPodReady()`, `isPodRegistered()`, `isPodEligible()` |
| [06 - Tier Assignment](./06-tier-assignment.md) | `addPodToPool()` calls `autoAssignTier()` for new pods |
| [07 - Allocation](./07-allocation.md) | Reconciler populates the pools that allocation reads |
| [10 - Zombie Cleanup](./10-zombie-cleanup.md) | Zombie cleanup is the third safety net after watcher and reconciler |
