# Flow 03: Leader Election & Pool Manager Lifecycle

**Source file:** `internal/poolmanager/manager.go` (183 lines)

## Overview

The Pool Manager is the **leader-only** component. In a 3-replica Smart Router deployment, exactly ONE pod runs the pool management workload (K8s watcher, reconciler, zombie cleanup) at any time. The other 2 replicas participate in leader election but remain idle followers.

Leader election uses the standard **Kubernetes Lease-based** leader election library (`k8s.io/client-go/tools/leaderelection`).

## Why Leader Election?

Without leader election, all 3 smart-router replicas would:
- Watch K8s pods simultaneously (3x the K8s API load)
- Run zombie cleanup simultaneously (race conditions on pod recovery)
- Run reconciliation simultaneously (duplicate writes, inconsistent state)

With leader election, **only the leader touches Redis pool state**. All replicas can still serve API requests (allocate, release, drain) — those are stateless Redis operations.

## Manager Struct

```go
type Manager struct {
    k8sClient *kubernetes.Clientset
    redis     *redis.Client
    config    *config.Config
    logger    *zap.Logger
    isLeader  atomic.Bool          // Thread-safe leader status flag
}
```

`isLeader` is an `atomic.Bool` because:
- It's **read** by every HTTP request to `/api/v1/status` (any replica, any goroutine)
- It's **written** only by the leader election callbacks (single goroutine)

## Run() — Entry Point (line 53-113)

```
main.go:137 → poolManager.Run(ctx)   // called in a goroutine
```

### Path A: Leader Election Disabled

```go
if !m.config.LeaderElectionEnabled {
    m.isLeader.Store(true)
    middleware.LeaderStatus.Set(1)
    return m.runLeaderWorkload(ctx)   // run directly, blocks
}
```

Used for local development. The single instance acts as leader immediately.

### Path B: Leader Election Enabled (Production)

```go
lock := &resourcelock.LeaseLock{
    LeaseMeta: metav1.ObjectMeta{
        Name:      "smart-router-leader",     // LEADER_ELECTION_LOCK_NAME
        Namespace: "voice-system",             // LEADER_ELECTION_NAMESPACE
    },
    Client: m.k8sClient.CoordinationV1(),
    LockConfig: resourcelock.ResourceLockConfig{
        Identity: m.config.PodName,            // e.g. "smart-router-0"
    },
}
```

This creates a K8s Lease resource named `smart-router-leader` in the `voice-system` namespace. Each smart-router pod uses its own pod name as identity.

```go
leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
    Lock:            lock,
    ReleaseOnCancel: true,
    LeaseDuration:   15s,   // LEADER_ELECTION_DURATION
    RenewDeadline:   10s,   // LEADER_ELECTION_RENEW_DEADLINE
    RetryPeriod:     2s,    // LEADER_ELECTION_RETRY_PERIOD
    Callbacks:       ...,
})
```

### Leader Election Timing

```
├── LeaseDuration: 15s ──── How long a lease is valid
├── RenewDeadline: 10s ──── Leader must renew within this window
└── RetryPeriod:   2s  ──── Non-leaders check every 2s
```

**Worst-case leadership transfer time:** ~15s (lease must fully expire before a new leader can acquire it).

## Callbacks

### OnStartedLeading (line 85-91)

```go
func(ctx context.Context) {
    m.isLeader.Store(true)
    middleware.LeaderStatus.Set(1)        // Prometheus gauge → 1
    m.runLeaderWorkload(ctx)
}
```

Called when **this pod wins** the leader election. `ctx` is cancelled when leadership is lost.

### OnStoppedLeading (line 93-101)

```go
func() {
    m.isLeader.Store(false)
    middleware.LeaderStatus.Set(0)        // Prometheus gauge → 0
    process.Signal(syscall.SIGTERM)       // Trigger graceful shutdown
}
```

Called when **this pod loses** leadership. **Sends SIGTERM to itself** — this causes the entire process to shut down gracefully (via main.go's signal handler).

**Why self-SIGTERM?** A pod that lost leadership should not continue serving API requests with stale assumptions. It's safer to restart and let K8s schedule a fresh instance.

### OnNewLeader (line 103-108)

```go
func(identity string) {
    if identity == m.config.PodName { return }
    m.logger.Info("Leader elected", zap.String("leader", identity))
}
```

Informational callback on all non-leader pods when a new leader is elected.

## runLeaderWorkload() — The Core Loop (line 116-144)

This is what the leader actually runs:

```
runLeaderWorkload(ctx)
    │
    ├── 1. syncAllPods(ctx)          ← Initial full reconciliation
    │      (errors logged, continues anyway)
    │
    ├── 2. go safeGo(ctx, "watchPods", ...)       ← K8s watch events
    │
    ├── 3. go safeGo(ctx, "zombieCleanup", ...)   ← Periodic zombie recovery
    │
    └── 4. Main loop: every 60s → syncAllPods(ctx)  ← Periodic reconciliation
           (blocks until ctx.Done)
```

### Startup Order Matters

1. **syncAllPods first** — ensures Redis state matches K8s reality before accepting events
2. **watchPods next** — handles real-time changes
3. **zombieCleanup next** — recovers any pods missed by watcher
4. **Periodic sync** — catches any drift (Redis flush, network partition recovery)

## safeGo() — Crash Recovery Wrapper (line 150-182)

Both `watchPods` and `zombieCleanup` run inside `safeGo()`, which provides:

1. **Panic recovery** — catches panics, logs them, doesn't crash the process
2. **Automatic restart** — retries the function after a backoff delay
3. **Exponential backoff** — 1s → 2s → 4s → 8s → 16s → 30s (capped)

```go
func (m *Manager) safeGo(ctx context.Context, name string, fn func()) {
    backoff := time.Second
    for {
        func() {
            defer recover()
            fn()                    // Run the actual function
        }()

        select {
        case <-ctx.Done(): return   // Leader lost / shutdown
        case <-time.After(backoff): // Wait before restart
        }
        backoff *= 2
        if backoff > 30*time.Second { backoff = 30*time.Second }
    }
}
```

**Why is this important?** If `watchPods` panics (e.g. unexpected K8s API response), the entire pool management doesn't stop. It restarts after a brief delay.

**Note:** The backoff is per-goroutine and never resets. If `watchPods` panics 5 times in a row, the 6th restart waits 30s. In practice, a panic usually means a transient issue that resolves quickly.

## Sequence Diagram — Leader Election + Startup

```
smart-router-0        smart-router-1        smart-router-2        K8s API (Lease)
      │                     │                     │                     │
      ├── RunOrDie ─────────┼─────────────────────┼── Create/Acquire ──►│
      │                     ├── RunOrDie ──────────┼── Try Acquire ────►│
      │                     │                     ├── RunOrDie ────────►│
      │                     │                     │                     │
      │◄──── OnStartedLeading (winner!) ──────────┤                     │
      │                     │◄── OnNewLeader("smart-router-0") ────────┤
      │                     │                     │◄── OnNewLeader ─────┤
      │                     │                     │                     │
      ├── syncAllPods() ────┤                     │                     │
      ├── watchPods() ──────┤                     │                     │
      ├── zombieCleanup() ──┤                     │                     │
      │                     │                     │                     │
      │ ◄──── every 2s renew ───────────────────────── Renew Lease ────►│
      │                     │                     │                     │
```

## What Happens When Leader Dies?

```
Time 0s:   Leader (smart-router-0) crashes / gets OOM-killed
Time 0-15s: Lease still valid (15s duration), no new leader
Time ~15s:  Lease expires
Time ~17s:  smart-router-1 or -2 acquires lease (retry every 2s)
Time ~17s:  New leader calls syncAllPods() → full reconciliation
```

**Gap:** Up to ~15 seconds where no leader is running. During this gap:
- API requests (allocate, release, drain) continue working on all replicas
- K8s pod events are buffered (the watcher reconnects on the new leader)
- Zombie cleanup is paused (30s interval anyway, so 15s gap is negligible)

## Redis Keys Touched

**The Manager struct itself touches no Redis keys.** It delegates to:
- `syncAllPods()` → Reconciler (see [Flow 05](./05-reconciler.md))
- `watchPods()` → Watcher (see [Flow 04](./04-pod-watcher.md))
- `runZombieCleanup()` → Zombie Cleanup (see [Flow 10](./10-zombie-cleanup.md))

The only Redis-adjacent operation is `middleware.LeaderStatus.Set()` (Prometheus gauge, not Redis).

## Interaction with Other Flows

| Flow | Relationship |
|------|-------------|
| [01 - Bootstrap](./01-bootstrap.md) | `poolManager.Run(ctx)` launched as goroutine from main |
| [04 - Watcher](./04-pod-watcher.md) | `watchPods()` started by `runLeaderWorkload()` inside `safeGo` |
| [05 - Reconciler](./05-reconciler.md) | `syncAllPods()` called at startup + every 60s |
| [10 - Zombie Cleanup](./10-zombie-cleanup.md) | `runZombieCleanup()` started by `runLeaderWorkload()` inside `safeGo` |
| [13 - Metrics](./13-metrics.md) | `LeaderStatus` gauge updated on leadership changes |
