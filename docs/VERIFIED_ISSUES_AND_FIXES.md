# Smart Router - Verified Issues & Fix Plan

> **Date**: February 8, 2026  
> **Status**: 4 Critical Issues VERIFIED ✅  
> **Action Required**: Code fixes needed

---

## 🔴 CRITICAL ISSUES (All Verified)

### Issue #1: Orphaned Calls During Pod Restart
**Status**: ✅ VERIFIED - Bug exists

**Problem**:
When a voice-agent pod restarts (rolling update, crash, etc.):
1. The pod's **lease expires** (24h TTL) during restart
2. Pod returns to "available" pool via `isPodEligible()` check
3. But the **call info remains in Redis** (voice:call:{call_sid})
4. Result: Pod appears available but has orphaned call assignment

**Evidence**:
```bash
# Check assigned vs available - voice-agent-0 is in BOTH!
Gold Assigned: voice-agent-0
Gold Available: voice-agent-0  ← WRONG! Should not be in both

Standard Assigned: voice-agent-3
Standard Available: voice-agent-2
```

**Root Cause** (in `internal/poolmanager/watcher.go:151-165`):
```go
func (m *Manager) isPodEligible(ctx context.Context, podName string) bool {
    // Only checks lease and draining
    hasLease, _ := m.redis.Exists(ctx, "voice:lease:"+podName).Result()
    isDraining, _ := m.redis.Exists(ctx, "voice:pod:draining:"+podName).Result()
    
    // ❌ MISSING: Check if pod has active call in Redis!
    // Should query: voice:pod:{podName} for "allocated_call_sid"
}
```

**Impact**:
- New calls may be assigned to "busy" pods
- Orphaned calls hang until TTL (24h)
- Pool state inconsistency

**Fix Required**:
```go
func (m *Manager) isPodEligible(ctx context.Context, podName string) bool {
    // Check existing conditions
    hasLease, _ := m.redis.Exists(ctx, "voice:lease:"+podName).Result()
    if hasLease > 0 {
        return false
    }
    
    isDraining, _ := m.redis.Exists(ctx, "voice:pod:draining:"+podName).Result()
    if isDraining > 0 {
        return false
    }
    
    // ✅ ADD: Check if pod has active call
    podInfo, err := m.redis.HGetAll(ctx, "voice:pod:"+podName).Result()
    if err == nil && podInfo["allocated_call_sid"] != "" {
        // Pod has active call - verify if call still exists
        callExists, _ := m.redis.Exists(ctx, "voice:call:"+podInfo["allocated_call_sid"]).Result()
        if callExists > 0 {
            return false // Pod is busy with active call
        }
    }
    
    return true
}
```

**Also Needed**:
- Reconciliation logic on pod startup to clean orphaned calls
- Background job to periodically verify call<->pod consistency

---

### Issue #2: Race Condition in Allocation
**Status**: ✅ VERIFIED - Bug exists

**Problem**:
Concurrent requests with same `call_sid` can allocate multiple different pods.

**Root Cause** (in `internal/allocator/allocator.go:57-77`):
```go
func (a *Allocator) Allocate(ctx context.Context, callSID, merchantID string) (*models.AllocationResult, error) {
    // 1. Check idempotency
    existing, err := CheckExistingAllocation(ctx, a.redis, callSID)
    if existing != nil {
        return existing, nil // Return existing
    }
    
    // 2. Try to allocate from pool ← RACE WINDOW HERE
    // Multiple goroutines can pass step 1 simultaneously
    // All will try to allocate different pods
    podName, err = tryAllocateExclusive(...)
    
    // 3. Store allocation
    a.storeAllocation(ctx, callSID, podName, ...) // Last one wins
}
```

**Impact**:
- Split-brain: Same call assigned to multiple pods
- Resource waste
- Incorrect routing

**Fix Required**:
Add distributed lock using Redis SETNX:
```go
func (a *Allocator) Allocate(ctx context.Context, callSID, merchantID string) (*models.AllocationResult, error) {
    // Acquire distributed lock for this call_sid
    lockKey := "voice:allocation_lock:" + callSID
    lockValue := fmt.Sprintf("%d", time.Now().UnixNano())
    
    // Try to acquire lock with 5s timeout
    locked, err := a.redis.SetNX(ctx, lockKey, lockValue, 5*time.Second).Result()
    if err != nil || !locked {
        // Wait and check if another allocation succeeded
        time.Sleep(100 * time.Millisecond)
        existing, _ := CheckExistingAllocation(ctx, a.redis, callSID)
        if existing != nil {
            return existing, nil
        }
        return nil, ErrAllocationInProgress
    }
    
    // Ensure lock is released
    defer a.redis.Del(ctx, lockKey)
    
    // Now safe to proceed with allocation
    // ... rest of allocation logic
}
```

---

### Issue #3: Redis Data Loss Recovery
**Status**: ✅ VERIFIED - Issue exists

**Problem**:
If Redis is flushed or loses data, Smart Router doesn't auto-recover pools.

**Evidence**:
```bash
# Flush Redis
redis-cli FLUSHDB

# Smart Router returns:
{"pools":{},"active_calls":0,"is_leader":true,"status":"up"}

# Allocation fails:
{"error":"no pods available"}

# Requires manual restart to recover
```

**Root Cause**:
No automatic reconciliation trigger when pools are empty but K8s pods exist.

**Fix Required** (in `internal/poolmanager/manager.go`):
```go
// Add periodic reconciliation or empty-pool detection
func (m *Manager) runPeriodicReconciliation(ctx context.Context) {
    ticker := time.NewTicker(30 * time.Second)
    defer ticker.Stop()
    
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            // Check if all pools are empty
            if m.areAllPoolsEmpty(ctx) {
                m.logger.Warn("All pools empty, triggering reconciliation")
                if err := m.syncAllPods(ctx); err != nil {
                    m.logger.Error("Reconciliation failed", zap.Error(err))
                }
            }
        }
    }
}

func (m *Manager) areAllPoolsEmpty(ctx context.Context) bool {
    // Check if any pool has available pods
    for tier := range m.config.ParsedTierConfig {
        count, _ := m.redis.SCard(ctx, "voice:pool:"+tier+":available").Result()
        if count > 0 {
            return false
        }
    }
    return true
}
```

**Alternative**: Trigger reconciliation in allocator when no pods found:
```go
// In allocator.go
if podName == "" {
    // Trigger reconciliation before giving up
    a.triggerPoolReconciliation(ctx)
    
    // Retry once
    podName, err = tryAllocateExclusive(ctx, a.redis, poolKey, drainingPrefix)
    if podName == "" {
        return nil, ErrNoPodsAvailable
    }
}
```

---

### Issue #4: Redis Error Handling (Ignored Errors)
**Status**: ✅ VERIFIED - Issue exists

**Problem**:
Multiple places ignore Redis errors, assuming `false` on error.

**Locations**:
1. `internal/poolmanager/watcher.go:130-148`:
```go
func (m *Manager) isPodRegistered(ctx context.Context, podName string) bool {
    // Errors ignored!
    if exists, _ := m.redis.SIsMember(ctx, "voice:pool:gold:assigned", podName).Result(); exists {
        return true
    }
    if exists, _ := m.redis.SIsMember(ctx, "voice:pool:standard:assigned", podName).Result(); exists {
        return true
    }
    // ... more ignored errors
    return false
}
```

2. `internal/poolmanager/reconciler.go:67-71`:
```go
scanSet := func(key string) {
    members, _ := m.redis.SMembers(ctx, key).Result() // Error ignored
    for _, m := range members {
        pods[m] = true
    }
}
```

**Impact**:
- Redis down = all checks return false (incorrect)
- Silent failures
- Inconsistent state

**Fix Required**:
Proper error handling:
```go
func (m *Manager) isPodRegistered(ctx context.Context, podName string) (bool, error) {
    exists, err := m.redis.SIsMember(ctx, "voice:pool:gold:assigned", podName).Result()
    if err != nil {
        return false, fmt.Errorf("redis error checking gold pool: %w", err)
    }
    if exists {
        return true, nil
    }
    
    exists, err = m.redis.SIsMember(ctx, "voice:pool:standard:assigned", podName).Result()
    if err != nil {
        return false, fmt.Errorf("redis error checking standard pool: %w", err)
    }
    if exists {
        return true, nil
    }
    
    return false, nil
}

// Callers should handle error:
isRegistered, err := m.isPodRegistered(ctx, podName)
if err != nil {
    m.logger.Error("Failed to check pod registration", zap.Error(err))
    // Conservative: assume registered to avoid double-adding
    return
}
```

---

## ✅ VERIFIED WORKING

### Voice Agent Integration
**Status**: ✅ Already integrated

The voice agents (orchestration-clair) already have Smart Router integration:
- `websocket.py` calls `SmartRouterClient().release_pod()` on WebSocket close
- Pod registry marks pod busy/available
- Graceful release implemented

**No code changes needed** to voice agents.

---

## 📋 IMPLEMENTATION PRIORITY

### Phase 1: Critical Fixes (Do First)
1. **Fix Issue #1** (Orphaned calls) - Add active call check in `isPodEligible()`
2. **Fix Issue #2** (Race condition) - Add distributed locking
3. **Fix Issue #4** (Error handling) - Proper error propagation

### Phase 2: Recovery Mechanisms
4. **Fix Issue #3** (Data loss recovery) - Auto-reconciliation trigger
5. **Add reconciliation endpoint** - Manual trigger via API

### Phase 3: Monitoring
6. **Add metrics** for orphaned calls, race conditions, Redis errors
7. **Add alerts** for pool inconsistency

---

## 🔧 TESTING AFTER FIXES

### Test Issue #1 Fix
```bash
# 1. Allocate a call
curl -X POST https://clairvoyance.breezelabs.app/api/v1/allocate \
  -d '{"call_sid":"orphan-test","merchant_id":"test"}'

# 2. Note the pod (e.g., voice-agent-1)

# 3. Restart the pod
kubectl delete pod voice-agent-1 -n voice-system

# 4. Wait for pod to restart
kubectl wait --for=condition=ready pod/voice-agent-1 -n voice-system --timeout=60s

# 5. Check pod NOT in available pool (should be reserved for orphaned call)
kubectl run redis-cli --rm -i --restart=Never -n voice-system --image=redis:7-alpine \
  -- redis-cli -h 10.100.0.4 SMEMBERS voice:pool:standard:available
# Should NOT include voice-agent-1
```

### Test Issue #2 Fix
```bash
# Concurrent allocations with same call_sid
for i in {1..10}; do
  curl -s -X POST https://clairvoyance.breezelabs.app/api/v1/allocate \
    -d '{"call_sid":"race-test","merchant_id":"test"}' &
done
wait

# Check Redis - should only have ONE call entry
kubectl run redis-cli --rm -i --restart=Never -n voice-system --image=redis:7-alpine \
  -- redis-cli -h 10.100.0.4 HGETALL voice:call:race-test

# All pods should be the same
```

### Test Issue #3 Fix
```bash
# Flush Redis
kubectl run redis-cli --rm -i --restart=Never -n voice-system --image=redis:7-alpine \
  -- redis-cli -h 10.100.0.4 FLUSHDB

# Wait 30s for auto-reconciliation

# Try allocation - should work after reconciliation
curl -X POST https://clairvoyance.breezelabs.app/api/v1/allocate \
  -d '{"call_sid":"recovery-test","merchant_id":"test"}'
```

---

## 📝 NOTES

- All issues are **confirmed real bugs**
- Fixes are **straightforward** but require careful testing
- **No changes needed** to voice agent code
- Smart Router **architecture is sound**, just needs hardening
- Redis is **shared resource** - error handling is critical

---

**Ready to implement fixes** ✅
