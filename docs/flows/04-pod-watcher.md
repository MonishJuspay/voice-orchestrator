# Flow 04: Pod Discovery — Watcher

**Source file:** `internal/poolmanager/watcher.go` (185 lines)

## Overview

The Watcher is a **leader-only** goroutine that maintains a real-time K8s Watch on voice-agent pods. When a pod is added, becomes ready, becomes unready, or is deleted, the watcher immediately updates Redis pools.

This is the **fastest** discovery mechanism — events arrive within milliseconds of pod state changes. The Reconciler (Flow 05) and Zombie Cleanup (Flow 10) are backup mechanisms that catch anything the watcher misses.

## How It's Started

```
manager.go:130 → go m.safeGo(ctx, "watchPods", func() { m.watchPods(ctx) })
```

Wrapped in `safeGo` for automatic restart on panic (see [Flow 03](./03-leader-election.md)).

## watchPods() — Outer Loop (line 15-43)

```go
func (m *Manager) watchPods(ctx context.Context) {
    for {
        // Check if context cancelled
        select {
        case <-ctx.Done(): return
        default:
        }

        // Start K8s watch
        watcher, err := m.k8sClient.CoreV1().Pods(m.config.Namespace).Watch(ctx, metav1.ListOptions{
            LabelSelector: m.config.PodLabelSelector,   // "app=voice-agent"
        })
        if err != nil {
            m.logger.Error("Failed to start watch", zap.Error(err))
            time.Sleep(5 * time.Second)
            continue
        }

        m.handleWatchEvents(ctx, watcher)
        watcher.Stop()

        // Brief pause before reconnecting
        select {
        case <-ctx.Done(): return
        case <-time.After(time.Second):
        }
    }
}
```

**The outer loop is an infinite reconnection loop.** K8s watches are not permanent — they close after the API server's timeout (usually 5-10 minutes) or on network errors. The watcher reconnects automatically.

### Watch Parameters

| Parameter | Value | Source |
|-----------|-------|--------|
| Namespace | `voice-system` | `cfg.Namespace` |
| Label Selector | `app=voice-agent` | `cfg.PodLabelSelector` |

This watches **only** pods with `app=voice-agent` label in the `voice-system` namespace — NOT smart-router pods or other workloads.

## handleWatchEvents() — Event Processing (line 46-81)

```go
for {
    select {
    case <-ctx.Done(): return
    case event, ok := <-watcher.ResultChan():
        if !ok {
            return   // Channel closed, reconnect
        }

        if event.Type == watch.Error {
            // Log and continue — don't crash on error events
            continue
        }

        pod, ok := event.Object.(*corev1.Pod)
        if !ok { continue }

        switch event.Type {
        case watch.Added:    m.handlePodAdded(ctx, pod)
        case watch.Modified: m.handlePodModified(ctx, pod)
        case watch.Deleted:  m.handlePodDeleted(ctx, pod)
        }
    }
}
```

**Error event handling (line 59-64):** K8s watch can send `watch.Error` events with `*metav1.Status` objects (not `*corev1.Pod`). These are logged but not type-asserted to Pod — that would panic.

## Event Handlers

### handlePodAdded (line 84-95)

```go
func (m *Manager) handlePodAdded(ctx context.Context, pod *corev1.Pod) {
    if !m.isPodReady(pod) || pod.Status.PodIP == "" {
        return   // Skip — pod not ready yet
    }
    m.addPodToPool(ctx, pod)
}
```

**Gate conditions:**
1. Pod must be in `Running` phase with `Ready` condition = True
2. Pod must have a PodIP assigned

If either fails, the pod is skipped. It will be picked up later by a `Modified` event when it becomes ready.

### handlePodModified (line 98-113)

```go
func (m *Manager) handlePodModified(ctx context.Context, pod *corev1.Pod) {
    if pod.Name == "" || pod.Status.PodIP == "" { return }

    isReady := m.isPodReady(pod)
    isRegistered := m.isPodRegistered(ctx, pod.Name)

    if isReady && !isRegistered {
        m.addPodToPool(ctx, pod)         // Pod became ready → add
    } else if !isReady && isRegistered {
        m.removePodFromPool(ctx, pod)    // Pod became unready → remove
    }
}
```

This handles **state transitions:**

| Previous State | New State | Action |
|---------------|-----------|--------|
| Not ready | Ready | `addPodToPool()` |
| Ready | Not ready | `removePodFromPool()` |
| Ready | Ready | No action (already registered) |
| Not ready | Not ready | No action (not registered) |

### handlePodDeleted (line 116-119)

```go
func (m *Manager) handlePodDeleted(ctx context.Context, pod *corev1.Pod) {
    m.removePodFromPool(ctx, pod)
}
```

Unconditionally removes the pod from all pools. No readiness check needed — pod is gone.

## Helper: isPodReady (line 122-133)

```go
func (m *Manager) isPodReady(pod *corev1.Pod) bool {
    if pod.Status.Phase != corev1.PodRunning { return false }
    for _, condition := range pod.Status.Conditions {
        if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
            return true
        }
    }
    return false
}
```

Checks two things:
1. `pod.Status.Phase == "Running"` (container is started)
2. At least one condition with `Type=Ready, Status=True` (readiness probe passed)

Both must be true. A pod that is `Running` but not `Ready` (e.g. readiness probe failing) is NOT added to pools.

## Helper: isPodRegistered (line 136-158)

```go
func (m *Manager) isPodRegistered(ctx context.Context, podName string) bool {
    for tier, cfg := range m.config.ParsedTierConfig {
        // Check voice:pool:{tier}:assigned SET
        if exists, err := m.redis.SIsMember(ctx, "voice:pool:"+tier+":assigned", podName).Result(); ... {
            return true
        }
        // For non-shared tiers, also check voice:merchant:{tier}:assigned SET
        if cfg.Type != config.TierTypeShared {
            if exists, err := m.redis.SIsMember(ctx, "voice:merchant:"+tier+":assigned", podName).Result(); ... {
                return true
            }
        }
    }
    return false
}
```

Scans **every configured tier's** assigned SET to check if a pod is already registered anywhere. 

**Fail-safe:** On Redis errors, returns `true` (assume registered) to avoid adding duplicate entries.

**Redis keys READ:** `voice:pool:{tier}:assigned`, `voice:merchant:{tier}:assigned` (for each tier)

## Helper: isPodEligible (line 162-184)

```go
func (m *Manager) isPodEligible(ctx context.Context, podName string) bool {
    // Check if pod has an active lease
    hasLease, err := m.redis.Exists(ctx, "voice:lease:"+podName).Result()
    if err != nil { return false }   // Fail safe
    if hasLease > 0 { return false }

    // Check if pod is draining
    isDraining, err := m.redis.Exists(ctx, "voice:pod:draining:"+podName).Result()
    if err != nil { return false }   // Fail safe
    if isDraining > 0 { return false }

    return true
}
```

A pod is eligible for the **available** pool only if:
1. No active lease (`voice:lease:{pod}` doesn't exist)
2. Not draining (`voice:pod:draining:{pod}` doesn't exist)

**Fail-safe:** On Redis errors, returns `false` (don't add potentially busy pod to available pool).

**Redis keys READ:** `voice:lease:{pod}`, `voice:pod:draining:{pod}`

## When isPodEligible Matters

`isPodEligible` is called by `addPodToPool()` and `ensurePodInPool()` (in reconciler.go). It determines whether a pod goes into the **available** pool (can receive new calls) vs only the **assigned** pool (tracked but not available for allocation).

Example: A pod has an active call (lease exists). The reconciler runs and sees this pod in K8s but not in Redis. It calls `addPodToPool()` → the pod gets added to the `assigned` SET but NOT the `available` SET/ZSET, because `isPodEligible` returns false.

## Sequence Diagram — Pod Lifecycle via Watcher

```
K8s API Server              Watcher                    Redis
      │                       │                          │
      ├── Pod Created ───────►│                          │
      │   (Phase=Pending)     │  isPodReady=false        │
      │                       │  → skip                  │
      │                       │                          │
      ├── Pod Modified ──────►│                          │
      │   (Phase=Running,     │  isPodReady=true         │
      │    Ready=True)        │  isPodRegistered=false   │
      │                       │                          │
      │                       ├── addPodToPool() ───────►│
      │                       │   autoAssignTier()       │ SADD assigned
      │                       │   isPodEligible()        │ SADD/ZADD available
      │                       │   Store metadata         │ HSET metadata
      │                       │   Store tier             │ SET tier
      │                       │                          │
      ├── Pod Modified ──────►│                          │
      │   (Ready=False,       │  isPodReady=false        │
      │    e.g. failing       │  isPodRegistered=true    │
      │    readiness probe)   │                          │
      │                       ├── removePodFromPool() ──►│
      │                       │   (cleanup all keys)     │ SREM/ZREM all pools
      │                       │                          │ DEL metadata, tier,
      │                       │                          │   lease, draining
      │                       │                          │
      ├── Pod Deleted ───────►│                          │
      │                       ├── removePodFromPool() ──►│
      │                       │   (same cleanup)         │
```

## Redis Keys Touched

### Read

| Key | Type | Purpose |
|-----|------|---------|
| `voice:pool:{tier}:assigned` | SET | `isPodRegistered` — check if pod in any tier |
| `voice:merchant:{tier}:assigned` | SET | `isPodRegistered` — check merchant pools too |
| `voice:lease:{pod}` | STRING | `isPodEligible` — check active call |
| `voice:pod:draining:{pod}` | STRING | `isPodEligible` — check draining status |

### Write (via addPodToPool / removePodFromPool)

See [Flow 05: Reconciler](./05-reconciler.md) for the full `addPodToPool()` and `removePodFromPool()` Redis operations — those functions live in `reconciler.go` and are shared between watcher and reconciler.

## Edge Cases

1. **Watch channel closes:** The outer loop reconnects after 1s. During the gap, events are buffered by K8s (the resourceVersion is maintained).

2. **Watch error event:** Logged and skipped. Does not crash the watcher.

3. **Pod IP not assigned yet:** `handlePodAdded` skips the pod. The subsequent `Modified` event (when IP is assigned) will pick it up.

4. **Redis error in isPodRegistered:** Returns `true` (assume registered) to avoid duplicates.

5. **Redis error in isPodEligible:** Returns `false` (assume not eligible) to avoid adding busy pods to available pool.

## Interaction with Other Flows

| Flow | Relationship |
|------|-------------|
| [03 - Leader Election](./03-leader-election.md) | Watcher only runs on the leader, started by `runLeaderWorkload()` |
| [05 - Reconciler](./05-reconciler.md) | Shares `addPodToPool()` and `removePodFromPool()` functions |
| [06 - Tier Assignment](./06-tier-assignment.md) | `addPodToPool()` calls `autoAssignTier()` for new pods |
| [07 - Allocation](./07-allocation.md) | Watcher populates the pools that allocation reads from |
