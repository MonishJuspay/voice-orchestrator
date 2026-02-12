# Merchant & Tier Allocation Guide

Complete reference for configuring merchant allocation, tier pools, cost strategies, and scaling approaches in the Smart Router.

---

## Table of Contents

1. [Core Concepts](#core-concepts)
2. [Tier Configuration](#tier-configuration)
3. [Merchant Configuration](#merchant-configuration)
4. [Allocation Scenarios (Quick Reference)](#allocation-scenarios-quick-reference)
5. [Redis Key Reference](#redis-key-reference)
6. [API Reference](#api-reference)
7. [Cost Comparison](#cost-comparison)
8. [Dynamic Scaling Strategies](#dynamic-scaling-strategies)
9. [Operational Runbook](#operational-runbook)

---

## Core Concepts

### Pool Types

| Type | Redis Structure | Behavior | Use Case |
|------|----------------|----------|----------|
| **Exclusive** | SET (`SPOP`/`SADD`) | 1 call per pod. Pod removed from pool on allocate, returned on release. | Premium/guaranteed quality |
| **Shared** | ZSET (Lua `ZINCRBY`) | N calls per pod. Score = active call count. Allocator picks lowest-score non-draining pod. | Cost efficiency, high density |
| **Merchant** | SET (`SPOP`/`SADD`) | Dedicated pods for a specific merchant. Always exclusive. | SLA guarantees, isolation |

### Fallback Chain

Every allocation walks an ordered list of pools (the "chain") until a pod is found:

```
chain = ["merchant:shopify", "gold", "standard", "basic"]
         ──────┬──────────   ──┬──   ────┬────   ──┬──
         dedicated pool     excl.    excl.     shared
         (if merchant has   tier     tier      tier
          pool configured)
```

**Chain resolution logic** (`allocator.go:150-169`):

1. **Base chain**: merchant's `fallback` array if set, otherwise system `default_chain`
2. **Prepend**: if merchant has `pool` set, prepend `"merchant:{pool}"` to the base

### How Shared Pools Work

Shared pools use a sorted set (ZSET) where:
- **Member** = pod name
- **Score** = number of active calls on that pod

The Lua allocation script (`shared.go:18-51`):
1. Iterates pods sorted by score ascending (least loaded first)
2. Skips pods that are draining (`voice:pod:draining:{pod}` exists)
3. Skips pods at `max_concurrent` capacity
4. Atomically increments score via `ZINCRBY +1`

On release, the releaser does `ZINCRBY -1`. The lease is only deleted when score reaches 0.

**Pods in shared pools are NOT isolated per merchant.** Any merchant whose chain reaches the shared tier will land on the same pods. A single pod may handle calls from multiple merchants simultaneously.

### How Exclusive Pools Work

Exclusive pools use a regular SET:
- `SPOP` atomically removes a random pod on allocate
- `SADD` returns it on release
- Draining pods are skipped (up to 10 retries)

One call per pod. Pod is fully removed from the available set during the call.

### How Merchant Pools Work

Merchant pools are exclusive SETs at `voice:merchant:{id}:pods`:
- Pods assigned to a merchant pool are NOT in any tier pool
- On release, pod returns to the merchant's SET (not a tier pool)
- The reconciler auto-assigns pods to merchant pools based on tier config targets

**Current limitation:** Merchant pools are always exclusive. There is no "shared merchant pool" (dedicated pods for a merchant handling multiple concurrent calls). This would require code changes to use ZSETs for merchant pools.

---

## Tier Configuration

### Format

Stored in Redis key `voice:tier:config`. On first deploy, the `TIER_CONFIG` env var seeds this key (SETNX). After that, **Redis is the source of truth** — edit Redis directly. Changes are picked up within 30 seconds (background refresh).

```json
{
  "tiers": {
    "gold":     { "type": "exclusive", "target": 1 },
    "standard": { "type": "exclusive", "target": 1 },
    "basic":    { "type": "shared",    "target": 1, "max_concurrent": 3 }
  },
  "default_chain": ["gold", "standard", "basic"]
}
```

### Fields

| Field | Type | Description |
|-------|------|-------------|
| `tiers.{name}.type` | `"exclusive"` or `"shared"` | Pool type (default: `"exclusive"`) |
| `tiers.{name}.target` | int | Number of pods the reconciler should assign to this tier |
| `tiers.{name}.max_concurrent` | int | Max concurrent calls per pod (shared only, default: 5) |
| `default_chain` | string[] | Ordered list of tiers to try when merchant has no custom config |

### Updating Tier Config (Live)

```bash
# View current config
kubectl exec -n voice-system debug-tools -- redis-cli -h 10.100.0.4 \
  GET voice:tier:config

# Update config (takes effect within 30s)
kubectl exec -n voice-system debug-tools -- redis-cli -h 10.100.0.4 \
  SET voice:tier:config '{"tiers":{"gold":{"type":"exclusive","target":2},"standard":{"type":"exclusive","target":3},"basic":{"type":"shared","target":5,"max_concurrent":3}},"default_chain":["gold","standard","basic"]}'
```

### Tier Config Examples

**3 pods, all exclusive (current production):**
```json
{
  "tiers": {
    "gold":     { "type": "exclusive", "target": 1 },
    "standard": { "type": "exclusive", "target": 1 },
    "basic":    { "type": "shared",    "target": 1, "max_concurrent": 3 }
  },
  "default_chain": ["gold", "standard", "basic"]
}
```
Capacity: 2 exclusive + 3 shared = 5 concurrent calls on 3 pods.

**10 pods, maximize density:**
```json
{
  "tiers": {
    "basic": { "type": "shared", "target": 10, "max_concurrent": 3 }
  },
  "default_chain": ["basic"]
}
```
Capacity: 30 concurrent calls on 10 pods.

**10 pods, VIP + shared:**
```json
{
  "tiers": {
    "gold":  { "type": "exclusive", "target": 2 },
    "basic": { "type": "shared",    "target": 8, "max_concurrent": 3 }
  },
  "default_chain": ["gold", "basic"]
}
```
Capacity: 2 exclusive + 24 shared = 26 concurrent calls.

**50 pods, tiered with merchant pools:**
```json
{
  "tiers": {
    "gold":     { "type": "exclusive", "target": 5 },
    "standard": { "type": "exclusive", "target": 10 },
    "basic":    { "type": "shared",    "target": 35, "max_concurrent": 3 }
  },
  "default_chain": ["gold", "standard", "basic"]
}
```
Capacity: 5 + 10 + 105 = 120 concurrent calls on 50 pods.

---

## Merchant Configuration

### Format

Stored in Redis hash `voice:merchant:config`. Each field is a merchant ID, value is JSON.

```bash
# Set merchant config
redis-cli -h 10.100.0.4 HSET voice:merchant:config <merchant_id> '<json>'

# Get merchant config
redis-cli -h 10.100.0.4 HGET voice:merchant:config <merchant_id>

# List all merchants with config
redis-cli -h 10.100.0.4 HGETALL voice:merchant:config

# Remove merchant config (reverts to default chain)
redis-cli -h 10.100.0.4 HDEL voice:merchant:config <merchant_id>
```

### Fields

```json
{
  "tier": "gold",
  "pool": "shopify",
  "fallback": ["gold", "basic"]
}
```

| Field | Type | Description |
|-------|------|-------------|
| `tier` | string | **DEAD FIELD** — never read by the allocator. Vestigial. |
| `pool` | string | Dedicated merchant pool name. Auto-prepends `"merchant:{pool}"` to chain. |
| `fallback` | string[] | Custom fallback chain. Overrides `default_chain`. |

### Important Notes

- `tier` is **never used** by the allocator — do not rely on it
- `pool` and `fallback` are independent and can be combined
- Pods in a merchant `pool` are assigned by the reconciler from the configured tiers
- If `fallback` is set, it completely replaces `default_chain` for that merchant
- The dedicated `pool` step is always first (prepended automatically)

---

## Allocation Scenarios (Quick Reference)

### "I want merchant X to..."

| Goal | Redis Command | Resulting Chain | Notes |
|------|--------------|-----------------|-------|
| **Use default pools (no special treatment)** | No config needed (or `HDEL`) | `["gold", "standard", "basic"]` | Walks the system `default_chain` |
| **Skip exclusive tiers, go straight to shared** | `HSET voice:merchant:config X '{"fallback":["basic"]}'` | `["basic"]` | Shares pods with everyone on the shared tier |
| **Have dedicated exclusive pods** | `HSET voice:merchant:config X '{"pool":"X"}'` | `["merchant:X", "gold", "standard", "basic"]` | Tries dedicated pods first, falls back to default chain |
| **Have dedicated pods + shared fallback only** | `HSET voice:merchant:config X '{"pool":"X","fallback":["basic"]}'` | `["merchant:X", "basic"]` | Dedicated first, then shared (skips gold/standard) |
| **Use only gold tier (no fallback)** | `HSET voice:merchant:config X '{"fallback":["gold"]}'` | `["gold"]` | Fails if gold is empty — no fallback |
| **Use custom tier order** | `HSET voice:merchant:config X '{"fallback":["standard","gold"]}'` | `["standard", "gold"]` | Tries standard before gold |
| **Have dedicated pods with NO fallback** | `HSET voice:merchant:config X '{"pool":"X","fallback":[]}'` | `["merchant:X"]` | Only dedicated pods. If empty, allocation fails |

**Wait, `"fallback":[]` vs no `fallback` field:**
- `"fallback": []` → empty array → `len(mc.Fallback) > 0` is false → **uses default_chain** (same as no config)
- To have truly no fallback beyond the dedicated pool, set `"fallback": ["__nonexistent__"]` or implement a code change

### Full Scenario Walkthrough

#### Scenario 1: Default Merchant (No Config)

```
Call comes in for merchant "acme" → no entry in voice:merchant:config
Chain: ["gold", "standard", "basic"]  (from default_chain)

Step 1: SPOP voice:pool:gold:available → got "voice-agent-0" → allocated!
  OR empty → Step 2
Step 2: SPOP voice:pool:standard:available → got "voice-agent-1" → allocated!
  OR empty → Step 3
Step 3: Lua on voice:pool:basic:available ZSET → got "voice-agent-2" (score 0→1) → allocated!
  OR all at max_concurrent → 503 No Pods Available
```

#### Scenario 2: Merchant with Dedicated Pool

```bash
HSET voice:merchant:config shopify '{"pool":"shopify"}'
```

```
Call comes in for merchant "shopify"
Chain: ["merchant:shopify", "gold", "standard", "basic"]

Step 1: SPOP voice:merchant:shopify:pods → got "voice-agent-5" → allocated!
  source_pool = "merchant:shopify"
  OR empty → Step 2 (falls to gold tier)
...
```

On release, pod returns to `voice:merchant:shopify:pods` (not a tier pool).

#### Scenario 3: Shared-Only Merchant

```bash
HSET voice:merchant:config budget_co '{"fallback":["basic"]}'
```

```
Call comes in for merchant "budget_co"
Chain: ["basic"]

Step 1: Lua on voice:pool:basic:available → pod with lowest score < max_concurrent
  Got "voice-agent-2" (score 1→2) → allocated!
  source_pool = "pool:basic"
```

This merchant's calls share the same pods as all other merchants hitting the basic tier.

---

## Redis Key Reference

### Pool Keys

| Key Pattern | Type | Description |
|-------------|------|-------------|
| `voice:pool:{tier}:available` | SET (exclusive) or ZSET (shared) | Pods available for allocation |
| `voice:pool:{tier}:assigned` | SET | All pods assigned to this tier (available + in-use) |
| `voice:merchant:{id}:pods` | SET | Merchant's available dedicated pods |
| `voice:merchant:{id}:assigned` | SET | All pods assigned to this merchant |

### Pod Keys

| Key Pattern | Type | Description |
|-------------|------|-------------|
| `voice:pod:{name}` | HASH | Pod status info (`status`, `allocated_call_sid`, `allocated_at`, etc.) |
| `voice:pod:tier:{name}` | STRING | Tier assignment for the pod (e.g., `"gold"`, `"merchant:shopify"`) |
| `voice:pod:draining:{name}` | STRING | Exists if pod is draining (TTL = `DRAINING_TTL`, default 6min) |
| `voice:pod:metadata` | HASH | Field=pod name, value=JSON `{"tier":"gold","name":"voice-agent-0"}` |
| `voice:lease:{name}` | STRING | Lease key (value=call_sid, TTL = `LEASE_TTL`, default 15min) |

### Call Keys

| Key Pattern | Type | Description |
|-------------|------|-------------|
| `voice:call:{call_sid}` | HASH | Call→pod mapping (`pod_name`, `source_pool`, `merchant_id`, `allocated_at`) |

### Config Keys

| Key | Type | Description |
|-----|------|-------------|
| `voice:tier:config` | STRING | JSON tier configuration (source of truth) |
| `voice:merchant:config` | HASH | Merchant configs (field=merchant_id, value=JSON) |

### Inspection Commands

```bash
# Shorthand for all commands below
REDIS="kubectl exec -n voice-system debug-tools -- redis-cli -h 10.100.0.4"

# View tier config
$REDIS GET voice:tier:config

# View all merchant configs
$REDIS HGETALL voice:merchant:config

# View exclusive pool (SET)
$REDIS SMEMBERS voice:pool:gold:available

# View shared pool (ZSET with scores)
$REDIS ZRANGE voice:pool:basic:available 0 -1 WITHSCORES

# View merchant pool
$REDIS SMEMBERS voice:merchant:shopify:pods

# View assigned pods for a tier
$REDIS SMEMBERS voice:pool:gold:assigned

# Check pod's tier assignment
$REDIS GET voice:pod:tier:voice-agent-0

# Check pod status
$REDIS HGETALL voice:pod:voice-agent-0

# Check active call
$REDIS HGETALL voice:call:<call_sid>

# Check lease
$REDIS GET voice:lease:voice-agent-0
$REDIS TTL voice:lease:voice-agent-0

# Check if pod is draining
$REDIS EXISTS voice:pod:draining:voice-agent-0
```

---

## API Reference

### `POST /api/v1/allocate`

JSON body:
```json
{
  "call_sid": "CA123...",
  "merchant_id": "shopify",
  "provider": "twilio",
  "flow": "v2",
  "template": "order-confirmation"
}
```

Response (200):
```json
{
  "success": true,
  "pod_name": "voice-agent-0",
  "ws_url": "wss://clairvoyance.breezelabs.app/ws/pod/voice-agent-0/agent/voice/breeze-buddy/twilio/callback/order-confirmation/v2",
  "source_pool": "pool:gold",
  "was_existing": false
}
```

### `POST /api/v1/twilio/allocate`

Form-encoded (Twilio webhook). Reads `CallSid` from form, `merchant_id`/`flow`/`template` from query params.

Returns TwiML:
```xml
<Response>
  <Connect>
    <Stream url="wss://..."/>
  </Connect>
</Response>
```

### `POST /api/v1/plivo/allocate`

Form-encoded (Plivo webhook). Reads `CallUUID` from form, `merchant_id`/`flow`/`template` from query params.

Returns Plivo XML:
```xml
<Response>
  <Stream bidirectional="true" keepCallAlive="true" contentType="audio/x-mulaw;rate=8000">wss://...</Stream>
</Response>
```

### `POST /api/v1/exotel/allocate`

JSON body with `CallSid`, `merchant_id`, `flow`, `template`.

Returns JSON:
```json
{ "url": "wss://..." }
```

### `POST /api/v1/release`

```json
{ "call_sid": "CA123..." }
```

Response (200):
```json
{
  "success": true,
  "pod_name": "voice-agent-0",
  "released_to_pool": "pool:gold",
  "was_draining": false
}
```

### `POST /api/v1/drain`

```json
{ "pod_name": "voice-agent-0" }
```

### `GET /api/v1/status`

Returns pool sizes, active calls, leader status.

### `GET /api/v1/health`

Returns `{"status": "ok"}`.

---

## Cost Comparison

Based on GKE pricing (asia-south1), c2d-standard-4 nodes for voice-agent pods.

### Per-Pod Economics

| Item | Value |
|------|-------|
| vCPUs per pod | 4 (c2d-standard-4 node, 1 pod per node) |
| CPU per active call | ~1 core (pipecat v0.0.101 + audio pipeline) |
| Max calls per pod (exclusive) | 1 |
| Max calls per pod (shared, max_concurrent=3) | 3 |
| Node cost (c2d-standard-4, on-demand) | ~$148/month |
| Node cost (1-year CUD) | ~$104/month (-30%) |

### Scaling Scenarios at 50 Concurrent Calls

| Strategy | Pods | Nodes | Monthly Cost (compute) | Total w/ infra | Savings |
|----------|------|-------|----------------------|----------------|---------|
| Always-on, all exclusive | 50 | 50 | $7,400 | ~$7,700 | baseline |
| Always-on, all shared (3/pod) | 17 | 17 | $2,516 | ~$2,800 | -64% |
| Shared + day-only 12h | 17 peak / 5 off | 17/5 | ~$1,400 | ~$1,700 | -78% |
| **Dynamic scaling (shared)** | **scales to demand** | **scales** | **~$800** | **~$1,100** | **-86%** |
| Dynamic + 1yr CUD (base) | scales + CUD base | scales | ~$600 | ~$900 | -88% |

### Infrastructure Costs (Fixed, Regardless of Scale)

| Item | Monthly Cost |
|------|-------------|
| GKE management fee | $73 |
| Cloud SQL (db-custom-2-8192, HA) | $139 |
| Redis Memorystore (1GB, Standard HA) | $50 |
| Load Balancer + network | $28 |
| Persistent disks | $20 |
| **Total fixed** | **~$310** |

---

## Dynamic Scaling Strategies

### Option 1: Smart Auto-Scaler in Smart Router (Recommended)

Add a scaling loop to the reconciler that monitors pool utilization and patches the StatefulSet replica count.

**How it works:**
```
Every 30s:
  utilization = active_calls / total_capacity
  if utilization > 80% → scale up (add pods)
  if utilization < 30% for 5min → scale down (drain + remove pods)
  respect min_replicas and max_replicas bounds
```

**Implementation:** ~100-150 lines of Go in the reconciler. Smart Router already has K8s client access (used for pod watching). Adding `apps/v1` StatefulSet patching is straightforward.

**Pros:**
- Uses existing infrastructure (no new components)
- Reacts to actual allocation pressure
- Can be tuned per tier

**Cons:**
- Scale-up latency (pod startup time ~30-60s)
- Need to handle scale-down carefully (drain before remove)

### Option 2: Pre-Scaling for Outbound Calls

Add `/api/v1/prescale` endpoint. Before launching an outbound call batch, the application tells the smart router how many pods it needs.

```bash
POST /api/v1/prescale
{
  "required_capacity": 20,
  "merchant_id": "shopify",  // optional
  "ttl_minutes": 30          // auto scale-down after
}
```

Smart Router calculates current capacity, scales StatefulSet if needed, and returns when pods are ready.

**Pros:**
- Zero cold-start for outbound batches
- Application controls timing
- Can pre-warm pods before batch starts

**Cons:**
- Only works for outbound (controllable) calls
- Application must know call volume in advance

### Option 3: Hybrid (Recommended for Production)

Combine Options 1 and 2:
- **Inbound calls**: reactive auto-scaler monitors utilization
- **Outbound batches**: pre-scale before launching
- **Base capacity**: keep a minimum number of pods always warm (covered by CUD)

```
always-on base (CUD):  5 pods  → handles ~15 concurrent calls (shared)
auto-scaler:           scales 5→50 based on demand
pre-scale:             app requests capacity before outbound batches
scale-down:            drain → remove after 5min below 30% utilization
```

### Option 4: KEDA (External)

Use KEDA (Kubernetes Event-Driven Autoscaling) to watch Redis metrics and scale the StatefulSet.

**Pros:** Declarative, well-tested, supports complex triggers.
**Cons:** Additional component to deploy/maintain, less control over drain behavior.

### Option 5: CronJob-Based (Simplest)

Fixed schedule scale up/down via `kubectl scale` in a CronJob.

```yaml
# Scale up at 9am IST
- schedule: "30 3 * * 1-6"  # UTC
  command: kubectl scale statefulset voice-agent -n voice-system --replicas=20

# Scale down at 9pm IST
- schedule: "30 15 * * *"   # UTC
  command: kubectl scale statefulset voice-agent -n voice-system --replicas=3
```

**Pros:** Dead simple, no code changes.
**Cons:** Not demand-responsive, wastes resources during off-peak within the window.

---

## Operational Runbook

### Adding a New Tier

1. Update tier config in Redis:
```bash
# Read current config
$REDIS GET voice:tier:config

# Add new tier (include all existing tiers + new one)
$REDIS SET voice:tier:config '{"tiers":{"gold":{"type":"exclusive","target":1},"standard":{"type":"exclusive","target":1},"basic":{"type":"shared","target":1,"max_concurrent":3},"premium":{"type":"exclusive","target":2}},"default_chain":["premium","gold","standard","basic"]}'
```

2. Scale up StatefulSet if needed (new tier needs pods):
```bash
kubectl scale statefulset voice-agent -n voice-system --replicas=5
```

3. Wait for reconciler (60s) to assign new pods to the new tier.

### Assigning Pods to a Merchant Pool

The reconciler assigns pods to merchant pools based on the `target` count in tier config. To assign dedicated pods to a merchant:

1. Ensure enough total pods exist
2. Set merchant config with a `pool` name
3. The reconciler will notice unassigned pods and assign them

Currently, merchant pool pod assignment happens through the `autoAssignTier` function in the tier assigner. Pods are assigned to merchant pools when the merchant has a configured `pool` and the merchant pool hasn't reached its target.

### Removing a Merchant Config

```bash
$REDIS HDEL voice:merchant:config shopify
```

Next allocation for this merchant will use the default chain. Pods already in the merchant pool will be reassigned during the next reconciliation.

### Emergency: Force-Release a Stuck Call

```bash
# Find the call info
$REDIS HGETALL voice:call:<call_sid>

# Get the pod name from the response, then:
# Delete call mapping
$REDIS DEL voice:call:<call_sid>

# Delete lease
$REDIS DEL voice:lease:<pod_name>

# For exclusive pool: return pod to available
$REDIS SADD voice:pool:<tier>:available <pod_name>

# For shared pool: decrement score
$REDIS ZINCRBY voice:pool:<tier>:available -1 <pod_name>

# Update pod status
$REDIS HSET voice:pod:<pod_name> status available allocated_call_sid "" allocated_at ""
```

### Viewing System Status

```bash
# Via smart-router API
kubectl exec -n voice-system debug-tools -- \
  wget -qO- http://smart-router:8080/api/v1/status

# Direct Redis inspection
$REDIS KEYS "voice:*"
```

### Common Gotchas

1. **`tier` field in MerchantConfig is dead** — setting `"tier": "gold"` does nothing. Use `fallback` instead.

2. **`fallback: []` (empty array) uses default_chain** — `len([]) == 0`, so it falls through to `default_chain`. There's no way to configure "no fallback at all" through config alone.

3. **Merchant pool pods are exclusive only** — you can't have a shared merchant pool. A merchant pod handles 1 call at a time.

4. **Lease TTL must outlast longest call** — default is 15min (`LEASE_TTL`). If a call runs longer, zombie cleanup may reclaim the pod. Set `LEASE_TTL=2h` for long calls.

5. **Tier config changes take up to 30s** — the refresh interval for the in-memory cache. Critical changes should be verified after 30s.

6. **Reconciler runs every 60s** — pod tier assignments may take up to 60s after tier config changes.

7. **1 CPU core per active call** — pipecat v0.0.101 audio pipeline uses ~1 full core per call. c2d-standard-4 (4 vCPU) supports max 3 concurrent calls per pod.
