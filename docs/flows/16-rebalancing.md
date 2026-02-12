# Flow 16: Auto-Rebalancing

## Overview

When tier targets change in Redis config, existing pods stay in their current
tier. The `rebalanceTiers()` method detects imbalances and moves idle pods
to satisfy new targets.

**File:** `internal/poolmanager/reconciler.go`
**Called from:** `syncAllPods()` (end of every reconciler cycle, every 60s)
**Runs on:** Leader only

---

## When Does Rebalancing Trigger?

Every reconciler cycle (60s), after the normal K8s/Redis sync completes.

It does something only when BOTH conditions are true:
1. At least one tier has more pods than its target (over-target)
2. At least one tier has fewer pods than its target (under-target)

If only over-target tiers exist (you lowered targets without raising others),
nothing happens. There's nowhere to put the excess pods.

---

## The Algorithm

```
1. For each tier in default_chain:
     assigned = SCARD voice:pool:{tier}:assigned
     diff = assigned - target
     if diff > 0: add to overTiers list
     if diff < 0: add to underTiers list

2. If either list is empty: return (nothing to do)

3. For each over-target tier:
     Get all pods: SMEMBERS voice:pool:{tier}:assigned
     For each pod:
       - Skip if draining (voice:pod:draining:{name} exists)
       - Skip if has lease (voice:lease:{name} exists)
       - Check if idle:
           Exclusive: SREM from available SET (returns 0 if allocated)
           Shared:    Lua script (ZSCORE==0 check + ZREM, atomic)
       - If idle: move pod to first under-target tier
```

---

## How Pods Move

### Exclusive -> Exclusive (e.g. gold -> standard)

```
SREM voice:pool:gold:available   pod    # already done in idle check
SREM voice:pool:gold:assigned    pod
DEL  voice:pod:tier:pod
SET  voice:pod:tier:pod standard
SADD voice:pool:standard:assigned pod
SADD voice:pool:standard:available pod
```

### Shared -> Exclusive (e.g. basic -> gold)

```
ZREM voice:pool:basic:available  pod    # already done in Lua script
SREM voice:pool:basic:assigned   pod
DEL  voice:pod:tier:pod
SET  voice:pod:tier:pod gold
SADD voice:pool:gold:assigned    pod
SADD voice:pool:gold:available   pod
```

### Exclusive -> Shared (e.g. gold -> basic)

```
SREM voice:pool:gold:available   pod    # already done in idle check
SREM voice:pool:gold:assigned    pod
DEL  voice:pod:tier:pod
SET  voice:pod:tier:pod basic
SADD voice:pool:basic:assigned   pod
ZADD voice:pool:basic:available  0 pod  # score 0 = idle
```

### Shared -> Shared (e.g. basic -> economy)

```
ZREM voice:pool:basic:available    pod  # already done in Lua script
SREM voice:pool:basic:assigned     pod
DEL  voice:pod:tier:pod
SET  voice:pod:tier:pod economy
SADD voice:pool:economy:assigned   pod
ZADD voice:pool:economy:available  0 pod
```

---

## Race Condition: Shared Pod

The dangerous race:

```
T1 (rebalancer): ZSCORE voice:pool:basic:available agent-6 → 0 (idle)
T2 (allocator):  ZINCRBY voice:pool:basic:available 1 agent-6 → 1 (call allocated!)
T3 (rebalancer): ZREM voice:pool:basic:available agent-6 → removes pod with active call!
```

Result: agent-6 has an active call but is no longer in any available pool.
When the call finishes, the release does `ZINCRBY -1` on a non-existent member.
The pod is lost.

### Fix: Atomic Lua Script

```lua
local score = redis.call('ZSCORE', KEYS[1], ARGV[1])
if score and tonumber(score) == 0 then
    redis.call('ZREM', KEYS[1], ARGV[1])
    return 1  -- safe to move
end
return 0  -- not idle, skip
```

Redis executes Lua atomically. No allocation can happen between the ZSCORE
check and the ZREM. If the allocator got there first (score > 0), we return 0
and skip the pod.

### Exclusive Pod Race

For exclusive pods, the idle check is `SREM from available SET`:
- If `SREM` returns 1: we removed it, it was idle, safe to move
- If `SREM` returns 0: `SPOP` already took it (allocation), pod is busy, skip

No Lua needed because SREM is already atomic — either we remove it or it's
already gone.

---

## Example Walkthrough

### Starting State

```
Config: gold=3, standard=3, basic=3
gold:     {agent-0, agent-3, agent-8} assigned, all available
standard: {agent-1, agent-4, agent-5} assigned, all available
basic:    {agent-2:0, agent-6:0, agent-7:0} assigned, all idle (score 0)
```

### Config Change: gold=4, basic=2

```
Reconciler runs rebalanceTiers():
  gold:     3 assigned, target 4 → under by 1
  standard: 3 assigned, target 3 → OK
  basic:    3 assigned, target 2 → over by 1

  overTiers  = [{basic, 1}]
  underTiers = [{gold, 1}]

  Iterate basic's assigned pods:
    agent-2: isPodEligible? yes. Lua check ZSCORE=0? yes, ZREM done.
    Move agent-2: basic → gold

  Result:
    gold:     4 {agent-0, agent-3, agent-8, agent-2}
    standard: 3 {agent-1, agent-4, agent-5}
    basic:    2 {agent-6, agent-7}
```

### What If All Basic Pods Are Busy?

```
basic: {agent-2:2, agent-6:1, agent-7:3}  (all have active calls)

Lua script returns 0 for all → no pods moved → retry next cycle (60s)
```

---

## Scaling Down + Rebalancing

### The Right Workflow

1. Update tier config in Redis (set new targets that sum to new replica count)
2. Wait ~60s for rebalancer to move idle pods
3. Scale down the StatefulSet
4. K8s deletes highest ordinals, watcher cleans Redis
5. If distribution is still off, rebalancer fixes it next cycle

### Why It Converges

After any scale down, the system might be unbalanced:
- Some tiers over-target (had more pods than deleted)
- Some tiers under-target (lost pods to K8s deletion)

Rebalancer moves idle pods from over → under. If pods are busy, it waits.
Eventually calls finish, pods become idle, get moved. System reaches target
state.

**Only requirement:** `total replicas >= sum of all targets`. If you have
fewer pods than targets demand, some tiers will be permanently under-target.

---

## What Rebalancing Does NOT Do

| Scenario | Result |
|---|---|
| Create new pods | No — only moves existing pods between tiers |
| Delete pods | No — that's K8s StatefulSet scaling |
| Move busy pods | No — only idle pods (0 active calls) |
| Touch merchant pools | No — only rebalances default chain tiers |
| Move draining pods | No — skipped by isPodEligible() |
| Move leased pods | No — skipped by isPodEligible() |

---

## Logging

```
// When a pod is moved:
"Rebalanced pod" pod=voice-agent-2 from_tier=basic to_tier=gold

// When rebalancing completes with moves:
"Rebalancing complete" pods_moved=1

// Errors:
"rebalance: failed to get assigned count" tier=gold error=...
"rebalance: Lua script failed" pod=voice-agent-2 error=...
```

No log when rebalancing finds nothing to do (common case).

---

## Redis Commands Used

| Command | Key | Purpose |
|---|---|---|
| SCARD | voice:pool:{tier}:assigned | Count pods in tier |
| SMEMBERS | voice:pool:{tier}:assigned | List candidate pods |
| SREM | voice:pool:{tier}:available | Atomic idle check for exclusive |
| EVAL (Lua) | voice:pool:{tier}:available | Atomic idle check + remove for shared |
| SREM | voice:pool:{tier}:assigned | Remove from old tier |
| DEL | voice:pod:tier:{name} | Clear tier assignment |
| SET | voice:pod:tier:{name} | Assign new tier |
| SADD | voice:pool:{tier}:assigned | Add to new tier |
| SADD/ZADD | voice:pool:{tier}:available | Add to new available pool |
| EXISTS | voice:lease:{name} | Check active lease (via isPodEligible) |
| EXISTS | voice:pod:draining:{name} | Check draining (via isPodEligible) |
| HGET/HSET | voice:pod:metadata | Update pod metadata |
