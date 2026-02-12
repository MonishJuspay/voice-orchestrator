# Flow 02: Configuration & Tier System

**Source file:** `internal/config/config.go` (507 lines)

## Overview

All Smart Router configuration comes from **environment variables** — there are no config files, no CLI flags, no database lookups. The `config.Load()` function reads every env var, applies defaults, and parses the `TIER_CONFIG` JSON into structured data.

**Tier config is special:** after initial parsing from the env var, the tier config is stored in **Redis** (`voice:tier:config`) and refreshed from Redis every 30 seconds. This means tier configuration changes can be made at runtime by writing to the Redis key — no pod restarts needed. See [Flow 15: Tier Config Refresh](./15-tier-config-refresh.md) for the full lifecycle.

The tier system is the foundation of pool management. It defines:
- **Which tiers exist** (e.g. gold, standard, basic)
- **What type each tier is** (exclusive = 1-pod-1-call, or shared = multi-call per pod)
- **How many pods each tier should have** (target count)
- **The default fallback order** for allocation (DefaultChain)

## Complete Environment Variable Reference

### Redis

| Env Var | Type | Default | Description |
|---------|------|---------|-------------|
| `REDIS_URL` | string | `redis://localhost:6379` | Redis connection URL (parsed by go-redis) |
| `REDIS_POOL_SIZE` | int | `10` | Max connections in pool |
| `REDIS_MIN_IDLE_CONN` | int | `5` | Minimum idle connections maintained |
| `REDIS_MAX_RETRIES` | int | `3` | Retries on transient failure |
| `REDIS_DIAL_TIMEOUT` | duration | `5s` | TCP dial timeout |

### HTTP Server

| Env Var | Type | Default | Description |
|---------|------|---------|-------------|
| `HTTP_PORT` | string | `8080` | Main API server port |
| `METRICS_PORT` | string | `9090` | Prometheus metrics port |
| `HTTP_READ_TIMEOUT` | duration | `5s` | Max time to read request |
| `HTTP_WRITE_TIMEOUT` | duration | `10s` | Max time to write response |
| `HTTP_IDLE_TIMEOUT` | duration | `60s` | Idle connection timeout |
| `HTTP_SHUTDOWN_TIMEOUT` | duration | `30s` | Graceful shutdown timeout |

### Voice Agent

| Env Var | Type | Default | Description |
|---------|------|---------|-------------|
| `VOICE_AGENT_BASE_URL` | string | `wss://localhost:8081` | Base URL for WebSocket connections to voice agents. In production: `wss://clairvoyance.breezelabs.app` |

### Kubernetes

| Env Var | Type | Default | Description |
|---------|------|---------|-------------|
| `NAMESPACE` | string | `default` | K8s namespace to watch pods in. Production: `voice-system` |
| `POD_LABEL_SELECTOR` | string | `app=voice-agent` | Label selector for voice agent pods |
| `POD_NAME` | string | `smart-router-local` | This pod's name (injected by K8s downward API) |

### Pool Manager

| Env Var | Type | Default | Description |
|---------|------|---------|-------------|
| `CLEANUP_INTERVAL` | duration | `30s` | Zombie cleanup cycle interval |
| `LEASE_TTL` | duration | `15m` | Active call lease expiry. **Must outlast longest expected call.** |
| `DRAINING_TTL` | duration | `6m` | Draining key expiry. Self-heals if pod never dies. |
| `RECONCILE_INTERVAL` | duration | `60s` | Full K8s-vs-Redis sync interval |
| `CALL_INFO_TTL` | duration | `1h` | Call info hash TTL. Cleanup safety net. |

### Tier Configuration

| Env Var | Type | Default | Description |
|---------|------|---------|-------------|
| `TIER_CONFIG` | string (JSON) | `""` (empty → hardcoded defaults) | The tier system configuration. See parsing section below. |

### Leader Election

| Env Var | Type | Default | Description |
|---------|------|---------|-------------|
| `LEADER_ELECTION_ENABLED` | bool | `true` | Enable/disable leader election |
| `LEADER_ELECTION_NAMESPACE` | string | (same as `NAMESPACE`) | Namespace for the Lease resource |
| `LEADER_ELECTION_LOCK_NAME` | string | `smart-router-leader` | Name of the K8s Lease resource |
| `LEADER_ELECTION_DURATION` | duration | `15s` | Lease duration |
| `LEADER_ELECTION_RENEW_DEADLINE` | duration | `10s` | Renew deadline |
| `LEADER_ELECTION_RETRY_PERIOD` | duration | `2s` | Retry period for non-leaders |

### Logging

| Env Var | Type | Default | Description |
|---------|------|---------|-------------|
| `LOG_LEVEL` | string | `info` | Log level: debug, info, warn, error |
| `LOG_FORMAT` | string | `json` | Output format: `json` or `console` |

## TIER_CONFIG Parsing — Three Formats

`config.go:258-337` — `parseTierConfig()` calls `applyTierConfigJSON()` which supports three JSON formats, tried in order:

### Format 1: Structured Envelope (Preferred)

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

- **`tiers`**: Map of tier name → `TierConfig` struct
- **`default_chain`**: Explicit fallback order for allocation

This is the most explicit format and the one used in production (via the configmap, which uses the flat format — see Format 2).

### Format 2: Legacy Flat Map

```json
{
  "gold":     {"type": "exclusive", "target": 1},
  "standard": {"type": "exclusive", "target": 1},
  "basic":    {"type": "shared", "target": 1, "max_concurrent": 3}
}
```

- No `tiers` wrapper, no explicit `default_chain`
- `default_chain` is auto-built: exclusive tiers sorted alphabetically, then shared tiers sorted alphabetically
- **This is what the production configmap uses** (see `k8s/configmap.yaml:19`)

With the production config, auto-built chain = `["gold", "standard", "basic"]` (gold < standard alphabetically, then basic as shared).

### Format 3: Simple Integer Map

```json
{
  "gold": 10,
  "standard": 20,
  "overflow": 5
}
```

- Just tier name → target count
- All tiers default to `exclusive` type
- `default_chain` auto-built same as Format 2

### Empty `TIER_CONFIG` (Hardcoded Fallback)

If `TIER_CONFIG` is empty/unset, defaults to:
```go
ParsedTierConfig = {
    "gold":     {Type: "exclusive", Target: 10},
    "standard": {Type: "exclusive", Target: 20},
    "overflow": {Type: "exclusive", Target: 5},
}
DefaultChain = ["gold", "standard", "overflow"]
```

## Normalization — `normalizeTierConfigs()` (line 411-421)

After parsing, fills in defaults for incomplete configs:
- If `Type` is empty → defaults to `"exclusive"`
- If `Type` is `"shared"` and `MaxConcurrent` is 0 → defaults to `5`

## DefaultChain Construction — `ensureDefaultChain()` (line 425-452)

If `default_chain` was explicitly provided in the JSON:
1. Validate every entry against `ParsedTierConfig` (remove unknown tiers)
2. If any valid entries remain, use them

If not provided (or all entries were invalid):
1. Split tiers into exclusive and shared buckets
2. Sort each bucket alphabetically (insertion sort — `sortStrings()` at line 275)
3. Concatenate: `exclusive_sorted + shared_sorted`

**Why exclusive first?** Exclusive pools give dedicated resources — preferred for allocation. Shared pools are the last resort.

## TierConfig Struct

```go
type TierConfig struct {
    Type          TierType `json:"type"`                     // "exclusive" or "shared"
    Target        int      `json:"target"`                   // Desired number of pods in this tier
    MaxConcurrent int      `json:"max_concurrent,omitempty"` // Max calls per pod (shared only)
}
```

- `Target` is used by `autoAssignTier()` to decide which tier a new pod goes into
- `MaxConcurrent` is used by the shared allocation Lua script to cap concurrent calls per pod

## Helper Methods on Config

All tier-related methods acquire a `sync.RWMutex` read lock internally, making them safe for concurrent use from any goroutine.

| Method | Line | Description |
|--------|------|-------------|
| `GetParsedTierConfig()` | 122 | Returns a **shallow copy** of the full tier config map (safe to iterate) |
| `GetDefaultChain()` | 133 | Returns a **copy** of the default fallback chain slice |
| `IsSharedTier(tier)` | 142 | Returns true if tier exists and is shared type |
| `IsKnownTier(tier)` | 150 | Returns true if tier exists in parsedTierConfig |
| `GetTierConfig(tier)` | 158 | Returns (TierConfig, bool) for a tier |
| `TierNames()` | 166 | Returns all tier names (unordered) |
| `IsMerchantTier(tier)` | 181 | Returns true if tier has "merchant:" prefix (standalone function, no mutex) |
| `ParseMerchantTier(tier)` | 187 | Extracts merchant ID from "merchant:xxx" string (standalone function, no mutex) |

**Thread-safety note:** The getter methods return copies so that callers can freely iterate without holding any lock. The hot-path cost is ~100ns per call (RLock + map/slice copy + RUnlock), which is negligible compared to the Redis calls they avoid.

## Merchant Tier Convention

Merchant tiers are **not defined in TIER_CONFIG**. They are dynamic — created when:
1. A merchant config in Redis specifies a `pool` field
2. The pool manager assigns a pod to a merchant pool

The tier string in Redis for merchant pods is stored as `"merchant:{merchantID}"` (e.g. `"merchant:9shines"`). The `IsMerchantTier()` and `ParseMerchantTier()` functions parse this prefix.

## Production Configuration

From `k8s/configmap.yaml`:

```yaml
TIER_CONFIG: '{"gold":{"type":"exclusive","target":1},"standard":{"type":"exclusive","target":1},"basic":{"type":"shared","target":1,"max_concurrent":3}}'
```

This results in:

```
ParsedTierConfig:
  gold     → {Type: "exclusive", Target: 1, MaxConcurrent: 0}
  standard → {Type: "exclusive", Target: 1, MaxConcurrent: 0}
  basic    → {Type: "shared",    Target: 1, MaxConcurrent: 3}

DefaultChain: ["gold", "standard", "basic"]
```

Meaning:
- 1 pod assigned to gold (exclusive, 1 call at a time)
- 1 pod assigned to standard (exclusive, 1 call at a time)
- 1 pod assigned to basic (shared, up to 3 concurrent calls)
- Allocation tries gold → standard → basic

## Data Flow

### Startup

```
Environment Variables
        │
        ▼
  config.Load()
        │
        ├── Read all env vars with defaults
        │
        ├── parseTierConfig() → applyTierConfigJSON()
        │     ├── Try Format 1 (structured envelope)
        │     ├── Try Format 2 (flat TierConfig map)
        │     ├── Try Format 3 (simple int map)
        │     └── Error if all fail
        │
        ├── normalizeTierConfigs()
        │     └── Fill in Type defaults, MaxConcurrent defaults
        │
        └── ensureDefaultChain()
              └── Build or validate DefaultChain
        │
        ▼
  *config.Config (in-memory, from env var)
        │
        ▼
  BootstrapTierConfigToRedis()
        │
        ├── SETNX voice:tier:config (serialised envelope JSON)
        │     ├── Key didn't exist → env var config seeded to Redis ✓
        │     └── Key already existed → GET → applyTierConfigJSON() → swap cache
        │
        ▼
  In-memory cache now reflects Redis (source of truth)
```

### Runtime (every 30 seconds, all replicas)

```
  runTierConfigRefresh() goroutine
        │
        ├── GET voice:tier:config
        │     ├── Key exists → applyTierConfigJSON() → swap cache under write lock
        │     ├── Key missing (redis.Nil) → keep current config, log debug
        │     └── Redis error → keep current config, log warning
        │
        ▼
  Hot-path reads always use the in-memory cache (no Redis calls):
    config.GetDefaultChain()      → RLock → copy → RUnlock (~100ns)
    config.GetTierConfig(tier)    → RLock → copy → RUnlock (~100ns)
    config.IsSharedTier(tier)     → RLock → read → RUnlock (~50ns)
```

### Changing Tier Config at Runtime

```bash
# Change config directly in Redis — no pod restart needed
redis-cli SET voice:tier:config '{"tiers":{"gold":{"type":"exclusive","target":2},"standard":{"type":"exclusive","target":1},"basic":{"type":"shared","target":1,"max_concurrent":5}},"default_chain":["gold","standard","basic"]}'

# All replicas pick up the change within 30 seconds
# The reconciler (leader only) will then adjust pool sizes based on new targets
```

## Interaction with Other Flows

| Flow | How Config is Used |
|------|--------------------|
| [01 - Bootstrap](./01-bootstrap.md) | `config.Load()` is the first thing called; bootstrap + refresh goroutine started |
| [03 - Leader Election](./03-leader-election.md) | Leader election params (duration, deadline, lock name) |
| [06 - Tier Assignment](./06-tier-assignment.md) | `GetParsedTierConfig()` + `GetDefaultChain()` determine pod assignment |
| [07 - Allocation](./07-allocation.md) | `GetDefaultChain()` is the fallback chain, `IsSharedTier()` selects pool type |
| [08 - Release](./08-release.md) | `IsSharedTier()` determines exclusive vs shared release path |
| [09 - Drain](./09-drain.md) | `DrainingTTL` sets the self-healing TTL |
| [10 - Zombie Cleanup](./10-zombie-cleanup.md) | `CleanupInterval`, `GetParsedTierConfig()` drive the cleanup loop |
| [15 - Tier Config Refresh](./15-tier-config-refresh.md) | Redis-backed tier config lifecycle (bootstrap, refresh, runtime changes) |
