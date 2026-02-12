# 🧪 Testing Progress & Issue Tracker

## 📊 Summary
| Category | Status | Count |
|----------|--------|-------|
| 🔴 Critical | ⚠️ Issues Found | 5/5 Tested |
| 🟡 High | ✅ Passed | 2/2 Tested |
| 🟢 Medium | ⏳ Pending | 0/5 Tested |

---

## 🐛 Issues & Bugs Found (Prioritized)

### 1. Rolling Update & Pod Failure: Orphaned Calls 🔴
- **Description**: Active calls are NOT terminated when their assigned pod is deleted/restarted.
- **Evidence**:
  - `rolling-test-001` pointed to `voice-agent-2`.
  - `voice-agent-2` was restarted (rolling update).
  - Upon return, `voice-agent-2` was added to `available` pool.
  - BUT `voice:call:rolling-test-001` remained in Redis, pointing to `voice-agent-2`.
- **Impact**: State conflict. Redis says pod is busy with old call, Pool says pod is free. New calls might be assigned to "busy" pod. Old calls hang until TTL (24h).
- **Fix Required**: Implement reconciliation logic. On "Pod Added", check if it has active calls in Redis and mark them as failed/released.

### 2. Race Condition in Allocation 🔴
- **Description**: Concurrent requests with the same `call_sid` result in multiple pods allocated.
- **Evidence**: `race-test-001` allocated BOTH `voice-agent-0` and `voice-agent-2`.
- **Impact**: Split-brain handling, wasted resources.
- **Fix Required**: Distributed lock (Redis SETNX on `call_sid`) at start of allocation.

### 3. Redis Data Loss Recovery 🔴
- **Description**: Smart Router does not automatically rebuild pools if Redis is flushed.
- **Evidence**: Flushed Redis -> Pools empty -> "No pods available" until manual restart.
- **Impact**: Downtime requires manual intervention.
- **Fix Required**: Periodic reconciliation or Redis key monitoring.

### 4. Redis Connection Handling (Runtime) 🟡
- **Description**:
  - **Startup**: Correctly crashes/backoffs (Safe).
  - **Runtime**: Code review suggests errors are often ignored (e.g., `exists, _ := ...`).
- **Impact**: Could lead to incorrect logic (assuming pod is not assigned when check fails).
- **Fix Required**: Proper error handling in `poolmanager` (don't assume false on error).

---

## ✅ Verified / Working

- **Basic Allocation**: Works.
- **Release**: Works.
- **Merchant Configuration**: Works (Redis-based).
- **Scale Up**: New pods (`voice-agent-3`, `4`) detected and assigned to tiers automatically.
- **Idempotency**: Sequential duplicate requests return same allocation safely.
- **Infrastructure**: Robust deployment (Smart Router, Nginx, Redis).

---

## 📝 Next Steps

1. **Fix Critical Bugs**:
   - Implement `lock(call_sid)` in allocation handler.
   - Implement `ReconcilePod(podName)` on startup/pod-add events to clean orphans.
2. **Fix Data Loss**:
   - Implement `RebuildPools()` function triggered if pools are found empty but pods exist.
3. **Continue Testing**:
   - Provider Webhooks (Plivo/Exotel).
   - Load Testing (after locking fix).

---

## 🛠️ Current Status
**Testing Complete**. Critical issues identified. Proceed to remediation.
