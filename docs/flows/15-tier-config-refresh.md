# Flow 15: Tier Config Refresh (Redis-Backed)

**Source files:**
- `internal/config/config.go` (lines 339-404) — Redis bootstrap, refresh, marshal
- `cmd/smart-router/main.go` (lines 69-75, 251-270) — bootstrap call + refresh goroutine

## Overview

Tier configuration (which tiers exist, their types, targets, and the default fallback chain) was originally a static env var (`TIER_CONFIG`) parsed once at startup. Changing it required updating the ConfigMap and restarting all smart-router pods.

Now tier config lives in **Redis** (`voice:tier:config`) with an **in-memory cache** on every replica. A background goroutine refreshes the cache every 30 seconds. Config changes propagate to all replicas without any pod restarts.

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                    Redis (source of truth)                       │
│                                                                  │
│  voice:tier:config = '{"tiers":{...},"default_chain":[...]}'    │
│                                                                  │
└──────────┬──────────────────┬──────────────────┬────────────────┘
           │ GET every 30s    │ GET every 30s    │ GET every 30s
           ▼                  ▼                  ▼
    ┌──────────────┐   ┌──────────────┐   ┌──────────────┐
    │ smart-router │   │ smart-router │   │ smart-router │
    │   replica 0  │   │   replica 1  │   │   replica 2  │
    │              │   │              │   │              │
    │ ┌──────────┐ │   │ ┌──────────┐ │   │ ┌──────────┐ │
    │ │ in-memory│ │   │ │ in-memory│ │   │ │ in-memory│ │
    │ │  cache   │ │   │ │  cache   │ │   │ │  cache   │ │
    │ │(RWMutex) │ │   │ │(RWMutex) │ │   │ │(RWMutex) │ │
    │ └──────────┘ │   │ └──────────┘ │   │ └──────────┘ │
    └──────────────┘   └──────────────┘   └──────────────┘
```

**Key design properties:**
- **ALL replicas** run the refresh goroutine (not just the leader)
- **Hot-path reads** (allocate, release, drain) read memory only — **zero Redis calls** for config
- **30-second stale window** after a Redis key change is the only tradeoff — acceptable, self-healing
- **On Redis failure:** current in-memory config is kept, logged as warning. System continues serving.

## Startup Sequence

### Step 1: Parse env var (config.Load)

```go
cfg.parseTierConfig()  // calls applyTierConfigJSON(cfg.TierConfigJSON)
```

The `TIER_CONFIG` env var is parsed into the in-memory cache. This is the initial seed value.

### Step 2: Bootstrap to Redis (main.go:69-72)

```go
cfg.BootstrapTierConfigToRedis(ctx, redisClient.GetRedis(), logger)
```

**Implementation** (`config.go:349-366`):

1. Serialise current in-memory config to structured envelope JSON via `marshalTierConfig()`
2. `SETNX voice:tier:config <json>` — write only if key doesn't exist
3. **If SETNX returned true** (key was created):
   - First deploy or key was manually deleted
   - Env var config is now in Redis
   - Log: `"Tier config bootstrapped to Redis"`
4. **If SETNX returned false** (key already existed):
   - Redis already has a config (from a previous deploy or manual update)
   - Call `RefreshTierConfigFromRedis()` to load it into memory
   - **Redis wins over env var** — this is intentional
   - Log: `"Tier config already exists in Redis, loading from Redis"`
5. **On Redis error:**
   - Log warning, keep env var config
   - System operates in degraded mode (env var only) until next refresh succeeds

### Step 3: Start refresh goroutine (main.go:74-75)

```go
go runTierConfigRefresh(ctx, cfg, redisClient, logger)
```

**Implementation** (`main.go:254-270`):

```go
func runTierConfigRefresh(ctx context.Context, cfg *config.Config,
    redisClient *redisclient.Client, logger *zap.Logger) {
    ticker := time.NewTicker(config.TierConfigRefreshInterval)  // 30s
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            cfg.RefreshTierConfigFromRedis(ctx, redisClient.GetRedis(), logger)
        }
    }
}
```

## Refresh Cycle (every 30s)

**Implementation** (`config.go:371-391`):

```
RefreshTierConfigFromRedis():
    │
    ├── GET voice:tier:config
    │     │
    │     ├── redis.Nil (key missing):
    │     │     → log debug "key not found, keeping current config"
    │     │     → return (no change)
    │     │
    │     ├── Redis error:
    │     │     → log warn "failed to read, keeping current config"
    │     │     → return (no change)
    │     │
    │     └── Success (got raw JSON string):
    │           │
    │           ├── applyTierConfigJSON(raw)
    │           │     ├── Parse JSON (supports all 3 formats)
    │           │     ├── normalizeTierConfigs()
    │           │     ├── ensureDefaultChain()
    │           │     ├── tierMu.Lock()
    │           │     ├── swap parsedTierConfig + defaultChain
    │           │     └── tierMu.Unlock()
    │           │
    │           ├── Parse error:
    │           │     → log error "failed to parse, keeping current config"
    │           │     → return (no change — bad JSON doesn't crash anything)
    │           │
    │           └── Success:
    │                 → log debug "tier config refreshed from Redis"
    │
    └── Done
```

## Thread Safety

The in-memory cache is protected by a `sync.RWMutex` (`tierMu`) on the `Config` struct:

| Operation | Lock Type | Duration |
|-----------|-----------|----------|
| `GetParsedTierConfig()` | RLock | ~100ns (copy map) |
| `GetDefaultChain()` | RLock | ~100ns (copy slice) |
| `IsSharedTier(tier)` | RLock | ~50ns (map lookup) |
| `IsKnownTier(tier)` | RLock | ~50ns (map lookup) |
| `GetTierConfig(tier)` | RLock | ~50ns (map lookup) |
| `TierNames()` | RLock | ~100ns (build slice) |
| `applyTierConfigJSON()` (refresh) | Lock (write) | ~1μs (parse + swap) |

**Readers never block each other.** Multiple goroutines handling concurrent allocate/release/drain requests all acquire RLock simultaneously. The write lock (refresh) only blocks for the duration of the pointer swap — microseconds.

**Snapshot pattern:** Components that need multiple tier config reads in a single operation (e.g. `tierassigner.go`) snapshot once at the top:

```go
defaultChain := m.config.GetDefaultChain()     // copy
tierConfig := m.config.GetParsedTierConfig()    // copy
// ... use local variables for the rest of the function
```

This ensures consistent reads even if a refresh happens mid-operation.

## Redis Key Format

The `voice:tier:config` key stores the structured envelope format:

```json
{
  "tiers": {
    "gold":     {"type": "exclusive", "target": 1},
    "standard": {"type": "exclusive", "target": 1},
    "basic":    {"type": "shared", "target": 1, "max_concurrent": 3}
  },
  "default_chain": ["gold", "standard", "basic"]
}
```

All three parsing formats are supported when reading from Redis (structured, flat, simple int), but the **bootstrap always writes the structured format** via `marshalTierConfig()`. This means even if the env var uses the legacy flat format, Redis gets the normalised structured version.

## Operator Runbook

### View current tier config

```bash
redis-cli -h 10.100.0.4 GET voice:tier:config | python3 -m json.tool
```

### Change tier config at runtime

```bash
redis-cli -h 10.100.0.4 SET voice:tier:config '{"tiers":{"gold":{"type":"exclusive","target":2},"standard":{"type":"exclusive","target":1},"basic":{"type":"shared","target":1,"max_concurrent":5}},"default_chain":["gold","standard","basic"]}'
```

Within 30 seconds, all replicas will pick up the change. The reconciler (leader only) will then adjust pool sizes based on the new targets on its next cycle.

### Force immediate refresh (restart one replica)

If you can't wait 30 seconds, restart any single replica. It will bootstrap from Redis (SETNX is a no-op since the key exists) and load the current Redis config immediately.

### Reset to env var config

```bash
redis-cli -h 10.100.0.4 DEL voice:tier:config
# Then restart all replicas — they will re-seed from the TIER_CONFIG env var
```

### Check if replicas have the latest config

Hit `/api/v1/status` on any replica — the response includes the current pool structure which reflects the in-memory tier config.

## Error Scenarios

| Scenario | Behavior |
|----------|----------|
| Redis down at startup | Bootstrap logs warning, keeps env var config. Refresh goroutine retries every 30s. |
| Redis down during refresh | Keeps current in-memory config, logs warning. Retries next cycle. |
| Redis key deleted while running | Refresh sees `redis.Nil`, keeps current config. Next restart re-seeds from env var. |
| Invalid JSON written to Redis key | `applyTierConfigJSON` returns error, current config kept, error logged with the raw value. |
| Redis key has unknown tier names | Parsed normally — new tiers become available. Old tier names not in Redis are gone after refresh. |
| Partial config (missing fields) | `normalizeTierConfigs()` fills defaults (type→exclusive, max_concurrent→5 for shared). |

## Interaction with Other Flows

| Flow | Relationship |
|------|-------------|
| [01 - Bootstrap](./01-bootstrap.md) | Bootstrap + refresh goroutine started in Steps 3b/3c |
| [02 - Configuration](./02-configuration.md) | Tier config parsing, normalisation, and env var reference |
| [05 - Reconciler](./05-reconciler.md) | Uses `GetParsedTierConfig()` to check tier membership and targets |
| [06 - Tier Assignment](./06-tier-assignment.md) | Uses `GetDefaultChain()` + `GetParsedTierConfig()` to assign tiers |
| [07 - Allocation](./07-allocation.md) | Uses `GetDefaultChain()` for fallback chain, `IsSharedTier()` for pool type |
| [08 - Release](./08-release.md) | Uses `IsSharedTier()` for exclusive vs shared release path |
| [10 - Zombie Cleanup](./10-zombie-cleanup.md) | Uses `GetParsedTierConfig()` to iterate all known tiers |
| [11 - Redis Keys](./11-redis-keys.md) | `voice:tier:config` documented in the key reference |
