# Flow 06: Tier Assignment

**Source file:** `internal/poolmanager/tierassigner.go` (95 lines)

## Overview

When a new pod is discovered (by the watcher or reconciler) and doesn't have an existing tier assignment, `autoAssignTier()` decides which tier it should join. This determines which pool the pod goes into and therefore which types of calls it can serve.

## How It's Called

```
reconciler.go:116 → assignedTier, isMerchant := m.autoAssignTier(ctx, podName)
```

Called from `addPodToPool()` when a pod has no existing `voice:pod:tier:{pod}` key in Redis (or the key was deleted because the tier was removed from config).

## Function Signature

```go
func (m *Manager) autoAssignTier(ctx context.Context, podName string) (string, bool)
```

Returns:
- `(tierName, false)` — assigned to a regular tier (e.g. "gold")
- `(merchantID, true)` — assigned to a merchant pool (e.g. "9shines")

## Assignment Priority Order

### Priority 1: Already Assigned (line 22-27)

```go
existingTier, err := m.redis.Get(ctx, "voice:pod:tier:"+podName).Result()
if err == nil && existingTier != "" {
    if merchantID, ok := config.ParseMerchantTier(existingTier); ok {
        return merchantID, true     // Already a merchant pod
    }
    return existingTier, false      // Already assigned to a regular tier
}
```

**Redis key READ:** `voice:pod:tier:{pod}`

If the pod already has a tier key, respect it. This is a redundant check since `addPodToPool()` already checks this before calling `autoAssignTier()`, but it provides defense-in-depth.

### Priority 2: Merchant Pools Under Capacity (line 29-54)

```go
// Build set of tiers in DefaultChain
chainSet := make(map[string]bool)
for _, t := range m.config.DefaultChain { chainSet[t] = true }

for tier, cfg := range m.config.ParsedTierConfig {
    if chainSet[tier] { continue }        // Skip regular chain tiers
    if cfg.Type == config.TierTypeShared { continue }  // Skip shared (not merchant)

    assignedKey := "voice:merchant:" + tier + ":assigned"
    currentAssigned, err := m.redis.SCard(ctx, assignedKey).Result()
    if err != nil { continue }            // Skip on error
    if currentAssigned < int64(cfg.Target) {
        return tier, true                 // Merchant pool has room
    }
}
```

**Logic:** Any tier in `ParsedTierConfig` that is NOT in `DefaultChain` and NOT shared is treated as a merchant pool. If that pool has fewer pods than its `Target`, the new pod is assigned there.

**Redis key READ:** `voice:merchant:{tier}:assigned` (SCARD)

**In practice with the current production config, this block does nothing** — all three tiers (gold, standard, basic) are in the DefaultChain. There are no merchant-specific tiers in the TIER_CONFIG. Merchant pools are created dynamically via Redis merchant config, not via TIER_CONFIG.

### Priority 3: DefaultChain Walk — First Tier Under Capacity (line 57-75)

```go
for _, tier := range m.config.DefaultChain {
    cfg, ok := m.config.ParsedTierConfig[tier]
    if !ok { continue }

    assignedKey := "voice:pool:" + tier + ":assigned"
    current, err := m.redis.SCard(ctx, assignedKey).Result()
    if err != nil { continue }

    if current < int64(cfg.Target) {
        return tier, false
    }
}
```

Walks `DefaultChain` in order (e.g. `["gold", "standard", "basic"]`). For each tier:
1. Get current assigned count from `voice:pool:{tier}:assigned`
2. If count < target → assign pod to this tier

**Redis key READ:** `voice:pool:{tier}:assigned` (SCARD) for each tier in chain

**With production config (3 pods, targets: gold=1, standard=1, basic=1):**
- First pod → gold (0 < 1)
- Second pod → standard (0 < 1)
- Third pod → basic (0 < 1)

### Priority 4: All Pools at Capacity — Fallback (line 77-91)

```go
if len(m.config.DefaultChain) > 0 {
    fallback := m.config.DefaultChain[len(m.config.DefaultChain)-1]
    return fallback, false
}
// Absolute fallback (shouldn't happen)
return "standard", false
```

If all pools are at or above their target counts, the pod is assigned to the **last tier in DefaultChain** (e.g. "basic"). This is the overflow behavior — shared pools can absorb extra pods since they support concurrent calls.

## Decision Flowchart

```
autoAssignTier(pod)
    │
    ├── Has existing tier key? ──► YES → return existing tier
    │                              │
    │                              NO
    │                              │
    ├── Any non-chain tier ──────► YES → has capacity? ──► YES → return (tier, merchant)
    │   under capacity?            │                       │
    │                              NO                      NO
    │                              │                       │
    ├── Walk DefaultChain: ────────┤                       │
    │   gold under capacity? ──────┼──► YES → return "gold"
    │   standard under capacity? ──┼──► YES → return "standard"
    │   basic under capacity? ─────┼──► YES → return "basic"
    │                              │
    │                              NO (all at capacity)
    │                              │
    └── Return last in chain ──────┴──► return "basic" (overflow)
```

## Production Example — 3 Voice Agent Pods

Given production config:
```
DefaultChain: ["gold", "standard", "basic"]
gold:     target=1
standard: target=1
basic:    target=1 (shared, max_concurrent=3)
```

When 3 voice-agent pods start up (StatefulSet: voice-agent-0, voice-agent-1, voice-agent-2):

| Pod | Gold Assigned | Standard Assigned | Basic Assigned | Decision |
|-----|:---:|:---:|:---:|----------|
| voice-agent-0 | 0 | 0 | 0 | 0 < 1 → **gold** |
| voice-agent-1 | 1 | 0 | 0 | gold full, 0 < 1 → **standard** |
| voice-agent-2 | 1 | 1 | 0 | gold/standard full, 0 < 1 → **basic** |

If a 4th pod were added:
| Pod | Gold Assigned | Standard Assigned | Basic Assigned | Decision |
|-----|:---:|:---:|:---:|----------|
| voice-agent-3 | 1 | 1 | 1 | all at capacity → **basic** (overflow to last) |

## Redis Keys Touched

### Read

| Key | Type | Purpose |
|-----|------|---------|
| `voice:pod:tier:{pod}` | STRING | Check existing assignment |
| `voice:merchant:{tier}:assigned` | SET | SCARD to check merchant pool capacity |
| `voice:pool:{tier}:assigned` | SET | SCARD to check regular pool capacity |

### Write

None — `autoAssignTier()` only returns a decision. The caller (`addPodToPool()`) writes the tier key and pool entries.

## Edge Cases

1. **Redis error on SCARD:** The tier is skipped (continues to next). Pod eventually lands somewhere.

2. **No tiers configured:** Returns `"standard"` as absolute fallback (line 91). This should never happen in production.

3. **All tiers at/above target:** Pod goes to last tier in DefaultChain. For shared tiers, this is fine — more pods means more concurrent capacity. For exclusive tiers, this means more pods than intended in that tier, but no functional issue.

4. **Pod already has tier (redundant check):** Returns immediately. This is a defense-in-depth check — `addPodToPool()` already verifies before calling.

## Interaction with Other Flows

| Flow | Relationship |
|------|-------------|
| [02 - Configuration](./02-configuration.md) | `DefaultChain` and `ParsedTierConfig` drive assignment decisions |
| [05 - Reconciler](./05-reconciler.md) | `addPodToPool()` calls `autoAssignTier()` |
| [04 - Watcher](./04-pod-watcher.md) | `addPodToPool()` calls `autoAssignTier()` (via watcher events) |
| [07 - Allocation](./07-allocation.md) | Tier assignment determines which pool a pod is in → affects allocation chain |
