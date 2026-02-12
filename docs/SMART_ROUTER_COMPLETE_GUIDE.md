# Smart Router - Complete System Guide

> Everything you need to know about the Smart Router, from high-level concepts to Redis keys to debugging commands. If you forgot everything, start here.

---

## Table of Contents

1. [What Is This? (The Big Picture)](#1-what-is-this-the-big-picture)
2. [Architecture Overview](#2-architecture-overview)
3. [The Two Repositories](#3-the-two-repositories)
4. [Directory Structure (Smart Router - Go)](#4-directory-structure-smart-router---go)
5. [Core Concepts (Broken Down Simply)](#5-core-concepts-broken-down-simply)
   - 5.1 [Pods and Tiers](#51-pods-and-tiers)
   - 5.2 [Exclusive vs Shared Pools](#52-exclusive-vs-shared-pools)
   - 5.3 [Merchant Configuration](#53-merchant-configuration)
   - 5.4 [Leader Election](#54-leader-election)
6. [The Complete Call Lifecycle](#6-the-complete-call-lifecycle)
   - 6.1 [Step 1: Call Comes In](#61-step-1-call-comes-in)
   - 6.2 [Step 2: Allocation](#62-step-2-allocation)
   - 6.3 [Step 3: WebSocket Connection](#63-step-3-websocket-connection)
   - 6.4 [Step 4: Release](#64-step-4-release)
7. [Redis Data Model (Every Single Key)](#7-redis-data-model-every-single-key)
   - 7.1 [Key Patterns Reference Table](#71-key-patterns-reference-table)
   - 7.2 [Concrete Example with 5 Pods](#72-concrete-example-with-5-pods)
   - 7.3 [Redis State During an Active Call](#73-redis-state-during-an-active-call)
   - 7.4 [Redis State After Release](#74-redis-state-after-release)
8. [Allocation Algorithm (Step by Step)](#8-allocation-algorithm-step-by-step)
   - 8.1 [Idempotency Check (Lua Script)](#81-idempotency-check-lua-script)
   - 8.2 [Merchant Config Lookup](#82-merchant-config-lookup)
   - 8.3 [The Fallback Chain](#83-the-fallback-chain)
   - 8.4 [Exclusive Pool Allocation (SPOP)](#84-exclusive-pool-allocation-spop)
   - 8.5 [Shared Pool Allocation (Lua + ZSET)](#85-shared-pool-allocation-lua--zset)
   - 8.6 [Storing the Allocation](#86-storing-the-allocation)
9. [Release Algorithm (Step by Step)](#9-release-algorithm-step-by-step)
10. [Pool Manager (Background Brain)](#10-pool-manager-background-brain)
    - 10.1 [What the Pool Manager Does](#101-what-the-pool-manager-does)
    - 10.2 [Kubernetes Watcher](#102-kubernetes-watcher)
    - 10.3 [Reconciler (Full Sync)](#103-reconciler-full-sync)
    - 10.4 [Tier Assigner (Auto-Assign)](#104-tier-assigner-auto-assign)
    - 10.5 [Zombie Cleanup](#105-zombie-cleanup)
    - 10.6 [Pod Removal and Orphan Cleanup](#106-pod-removal-and-orphan-cleanup)
11. [Draining a Pod](#11-draining-a-pod)
12. [Leader Election (How It Works)](#12-leader-election-how-it-works)
13. [API Endpoints (Complete Reference)](#13-api-endpoints-complete-reference)
14. [Voice Agent Integration (Python Side)](#14-voice-agent-integration-python-side)
    - 14.1 [Feature Flags and Rollout](#141-feature-flags-and-rollout)
    - 14.2 [SmartRouterClient](#142-smartrouterclient)
    - 14.3 [Circuit Breaker](#143-circuit-breaker)
    - 14.4 [Four Release Mechanisms (Defense in Depth)](#144-four-release-mechanisms-defense-in-depth)
    - 14.5 [Fallback When Smart Router Is Down](#145-fallback-when-smart-router-is-down)
15. [Webhook Flows (Twilio, Plivo, Exotel)](#15-webhook-flows-twilio-plivo-exotel)
16. [Environment Variables (Both Repos)](#16-environment-variables-both-repos)
17. [Tier Configuration](#17-tier-configuration)
18. [Lua Scripts (The Critical Ones)](#18-lua-scripts-the-critical-ones)
19. [Deployment & Build Commands](#19-deployment--build-commands)
20. [Debugging & Operations Cheat Sheet](#20-debugging--operations-cheat-sheet)
21. [Cluster State Reference](#21-cluster-state-reference)
22. [Bug Fixes Applied](#22-bug-fixes-applied)
23. [Known Caveats & Design Decisions](#23-known-caveats--design-decisions)

---

## 1. What Is This? (The Big Picture)

**Problem**: We run voice AI agents (Breeze Buddy) that handle phone calls. Each agent runs in a Kubernetes pod. Without isolation, multiple calls could land on the same pod, causing resource contention, audio quality issues, and crashes.

**Solution**: The **Smart Router** ensures **1 pod = 1 call** (for exclusive tiers). It acts as a traffic cop:
- When a call comes in → Smart Router picks a free pod and assigns the call to it
- When the call ends → Smart Router marks that pod as free again
- Background processes keep Redis (the "brain") in sync with Kubernetes (the "reality")

**Think of it like a parking lot attendant**: Cars (calls) come in, the attendant (Smart Router) assigns them to parking spots (pods). When a car leaves, the spot opens up. The attendant periodically walks the lot (reconciler) to check for abandoned cars (zombie cleanup).

---

## 2. Architecture Overview

```
                    ┌─────────────────────────────────────────────┐
                    │              TELEPHONY PROVIDERS             │
                    │         Twilio  /  Plivo  /  Exotel          │
                    └──────────────┬──────────────────────────────┘
                                   │ Webhook (call answered)
                                   ▼
                    ┌──────────────────────────────────┐
                    │          NGINX ROUTER             │
                    │   (2 replicas, WebSocket proxy)   │
                    │   Domain: clairvoyance.breezelabs │
                    └──────┬───────────────┬───────────┘
                           │               │
              ┌────────────▼──┐     ┌──────▼──────────────────┐
              │ SMART ROUTER  │     │   VOICE AGENT (Clair)   │
              │  (Go, 3 pods) │     │  (Python, StatefulSet)  │
              │  Port: 8080   │     │  5 pods: agent-0..4     │
              └───────┬───────┘     └──────▲──────────────────┘
                      │                    │
                      │  allocate/release  │ WebSocket (audio)
                      │                    │
                      └────────────────────┘
                               │
                      ┌────────▼────────┐
                      │     REDIS       │
                      │  10.100.0.4     │
                      │  (via PSC)      │
                      │  All pool data  │
                      └─────────────────┘
```

**Key domains**:
- `clairvoyance.breezelabs.app` → Smart Router API (allocation/release)
- `buddy.breezelabs.app` → Voice Agent WebSocket (audio streaming)

---

## 3. The Two Repositories

| Repo | Language | What It Does | Location |
|------|----------|-------------|----------|
| `orchestration-api-go` | Go | Smart Router — allocation, release, pool management, K8s watcher | `/Users/harsh.tiwari/Documents/breeze-repos/orchestration-api-go/` |
| `orchestration-clair` | Python/FastAPI | Voice Agent (Breeze Buddy) — handles actual phone calls, integrates with Smart Router | `/Users/harsh.tiwari/Documents/breeze-repos/orchestration-clair/` |

---

## 4. Directory Structure (Smart Router - Go)

```
orchestration-api-go/
├── cmd/smart-router/
│   └── main.go                    # Entry point: wires everything together
│
├── internal/
│   ├── allocator/                 # ALLOCATING pods to calls
│   │   ├── allocator.go           #   Core logic + fallback chain
│   │   ├── types.go               #   Interfaces + error types
│   │   ├── idempotency.go         #   Lua script: atomic check-and-lock
│   │   ├── exclusive.go           #   SPOP from SET (exclusive pools)
│   │   ├── shared.go              #   Lua script: ZSET allocation (shared pools)
│   │   └── merchant.go            #   Merchant config lookup
│   │
│   ├── releaser/                  # RELEASING pods after calls end
│   │   ├── releaser.go            #   Core logic
│   │   ├── types.go               #   Interfaces + error types
│   │   ├── exclusive.go           #   SADD back to SET
│   │   └── shared.go              #   Lua script: ZINCRBY -1 on ZSET
│   │
│   ├── poolmanager/               # BACKGROUND: keeps Redis in sync with K8s
│   │   ├── manager.go             #   Leader election + main loop
│   │   ├── watcher.go             #   K8s pod event handler (add/modify/delete)
│   │   ├── reconciler.go          #   Full sync every 60s + add/remove helpers
│   │   ├── tierassigner.go        #   Auto-assigns tiers to new pods
│   │   └── zombie.go              #   Recovers "lost" pods every 30s
│   │
│   ├── drainer/
│   │   └── drainer.go             # Gracefully drain a pod (no new calls)
│   │
│   ├── config/
│   │   └── config.go              # All env vars + tier config parsing
│   │
│   ├── models/
│   │   └── models.go              # Request/response structs
│   │
│   ├── redisclient/
│   │   ├── client.go              # Redis connection wrapper
│   │   └── keys.go                # Redis key pattern constants
│   │
│   └── api/
│       ├── router.go              # HTTP router (chi)
│       ├── handlers/
│       │   ├── allocate.go        # POST /allocate + webhook handlers
│       │   ├── release.go         # POST /release
│       │   ├── health.go          # GET /health, /ready
│       │   ├── status.go          # GET /status (pool overview)
│       │   ├── drain.go           # POST /drain
│       │   └── pod_info.go        # GET /pod/{name}
│       └── middleware/            # Logging, metrics, panic recovery
│
├── k8s/                           # Kubernetes manifests
├── scripts/                       # Reference Lua scripts
├── docs/                          # Documentation (you are here!)
├── Dockerfile                     # Multi-stage build
└── Makefile                       # Build/test/lint targets
```

---

## 5. Core Concepts (Broken Down Simply)

### 5.1 Pods and Tiers

Every voice-agent pod is assigned to exactly ONE tier. A tier is like a "lane" in the parking lot.

```
┌────────────────────────────────────────────────┐
│                  VOICE AGENTS                   │
│                                                 │
│  ┌─────────┐  ┌─────────┐  ┌─────────┐        │
│  │ agent-0 │  │ agent-1 │  │ agent-2 │        │
│  │  GOLD   │  │ STANDARD│  │ STANDARD│        │
│  └─────────┘  └─────────┘  └─────────┘        │
│                                                 │
│  ┌─────────┐  ┌─────────┐                      │
│  │ agent-3 │  │ agent-4 │                      │
│  │ STANDARD│  │  BASIC  │                      │
│  └─────────┘  │ (shared)│                      │
│               └─────────┘                      │
└────────────────────────────────────────────────┘
```

**Tier types**:
- **Gold** — Reserved for VIP merchants (exclusive: 1 call per pod)
- **Standard** — Default tier for all merchants (exclusive: 1 call per pod)
- **Basic** — Shared tier, allows multiple concurrent calls per pod (e.g., 3)
- **Overflow** — Extra capacity for spikes (exclusive)
- **Dedicated/Merchant** — A specific merchant gets their own reserved pods

### 5.2 Exclusive vs Shared Pools

This is the most important distinction in the system.

**Exclusive Pool** (Gold, Standard, Overflow):
- Data structure: Redis **SET**
- One pod handles ONE call at a time
- Allocation: `SPOP` (atomic pop a random member from the set)
- Release: `SADD` (add the pod back to the set)
- Think: "parking spot — one car per spot"

**Shared Pool** (Basic):
- Data structure: Redis **ZSET** (sorted set)
- One pod handles MULTIPLE calls (up to `max_concurrent`)
- The **score** = number of active connections on that pod
- Allocation: Lua script finds pod with lowest score under limit, `ZINCRBY +1`
- Release: Lua script does `ZINCRBY -1` (floor at 0)
- Think: "shared desk — up to 3 people can sit at the same desk"

```
EXCLUSIVE POOL (SET)                 SHARED POOL (ZSET)
┌──────────────────────┐            ┌────────────────────────────┐
│ voice:pool:standard: │            │ voice:pool:basic:available │
│ available            │            │                            │
│                      │            │  agent-4  score: 0  (idle) │
│  {agent-1, agent-2}  │            │  agent-5  score: 2  (busy) │
│                      │            │  agent-6  score: 3  (full) │
│  SPOP → agent-1      │            │                            │
│  SADD ← agent-1      │            │  Lua: pick agent-4 (lowest │
└──────────────────────┘            │        score under max)     │
                                    └────────────────────────────┘
```

### 5.3 Merchant Configuration

Merchants can be assigned to specific tiers. This is stored in a Redis hash:

```
voice:merchant:config = {
    "acme-corp": '{"tier": "gold"}',            # ACME gets gold tier
    "9shines":   '{"tier": "dedicated", "pool": "9shines"}',  # 9shines gets own pool
    "small-biz": '{"tier": "standard"}'         # explicit standard
}
```

- If a merchant has no config → defaults to `standard` tier
- Config changes are **live** — no restart needed, takes effect on next allocation

### 5.4 Leader Election

Only ONE Smart Router pod runs the Pool Manager (watcher + reconciler + zombie cleanup). This is the "leader."

Why? If all 3 replicas ran the Pool Manager, they'd step on each other — duplicate additions, conflicting removals, race conditions everywhere.

```
┌──────────────┐  ┌──────────────┐  ┌──────────────┐
│ smart-router │  │ smart-router │  │ smart-router │
│    pod-0     │  │    pod-1     │  │    pod-2     │
│              │  │              │  │              │
│  LEADER ★    │  │  follower    │  │  follower    │
│  Pool Mgr   │  │  API only    │  │  API only    │
│  Watcher    │  │              │  │              │
│  Reconciler │  │              │  │              │
│  Zombies    │  │              │  │              │
│  + API      │  │              │  │              │
└──────────────┘  └──────────────┘  └──────────────┘

All 3 pods handle API requests (allocate/release)
Only the leader runs background processes
```

---

## 6. The Complete Call Lifecycle

### 6.1 Step 1: Call Comes In

A telephony provider (Twilio/Plivo/Exotel) sends a webhook when a call is answered.

```
Customer picks up phone
        │
        ▼
Twilio/Plivo/Exotel sends webhook
        │
        ▼
Two paths:
  Option A: Webhook goes DIRECTLY to Smart Router
            POST /api/v1/twilio/allocate
  Option B: Webhook goes to Voice Agent (Clair), which
            then calls Smart Router internally
            POST http://smart-router:8080/api/v1/allocate
```

### 6.2 Step 2: Allocation

The Smart Router picks a free pod:

```
1. Check idempotency:  "Have I seen this call_sid before?"
   │                    YES → return the same pod (idempotent)
   │                    NO  → acquire a 30-second lock, continue
   ▼
2. Get merchant config: "What tier should this merchant use?"
   │                     e.g., "gold" or "standard"
   ▼
3. Try the configured tier first:
   │   SPOP voice:pool:gold:available → got agent-0? Done!
   │   Empty? ↓
   ▼
4. Fallback chain:  gold → standard → overflow → basic (shared)
   │                Try each pool in order until one works
   ▼
5. All pools empty? → Return 503 "no pods available"
   │
   ▼
6. Store allocation in Redis:
   - voice:call:{callSID}  = {pod_name, source_pool, merchant_id, allocated_at}
   - voice:pod:{podName}   = {status: "allocated", allocated_call_sid: ...}
   - voice:lease:{podName}  = callSID (24h TTL)
   ▼
7. Return: {pod_name: "voice-agent-0", ws_url: "wss://buddy.breezelabs.app/ws/pod/voice-agent-0/call123"}
```

### 6.3 Step 3: WebSocket Connection

The telephony provider connects to the pod-specific WebSocket URL:

```
wss://buddy.breezelabs.app/ws/pod/voice-agent-0/call123
     \_____________________/\___/\____________/\______/
           domain           path   pod name    call_sid

nginx routes /ws/pod/voice-agent-0/* → voice-agent-0:8080
The specific pod handles the audio stream
```

### 6.4 Step 4: Release

When the call ends, the pod is freed up:

```
Call ends (WebSocket closes)
        │
        ▼
Voice Agent calls: POST /api/v1/release {"call_sid": "call123"}
        │
        ▼
Smart Router:
1. Look up voice:call:call123 → {pod_name: "voice-agent-0", source_pool: "pool:gold"}
2. Check if pod is draining (voice:pod:draining:voice-agent-0)
3. If NOT draining:
   - Exclusive: SADD voice:pool:gold:available voice-agent-0  (return to pool)
   - Shared:    ZINCRBY voice:pool:basic:available voice-agent-0 -1  (decrement)
4. Delete voice:lease:voice-agent-0
5. Update voice:pod:voice-agent-0 → {status: "available"}
6. Delete voice:call:call123
        │
        ▼
Pod is available for the next call!
```

---

## 7. Redis Data Model (Every Single Key)

### 7.1 Key Patterns Reference Table

#### STRING Keys

| Key | Value | TTL | What It Does |
|-----|-------|-----|-------------|
| `voice:pod:tier:{podName}` | Tier name, e.g. `"gold"`, `"standard"`, `"merchant:acme"` | None | Remembers which tier this pod belongs to |
| `voice:lease:{podName}` | The call_sid using this pod | 24 hours | Marks pod as "busy with this call". If TTL expires, the call is considered dead |
| `voice:pod:draining:{podName}` | `"true"` | 6 minutes | Blocks new allocations to this pod. Auto-expires after 6 min |

#### HASH Keys

| Key | Fields | TTL | What It Does |
|-----|--------|-----|-------------|
| `voice:call:{callSID}` | `pod_name`, `source_pool`, `merchant_id`, `allocated_at` | 24 hours | The call's allocation record. This is the source of truth for "which pod is handling this call" |
| `voice:pod:{podName}` | `status`, `allocated_call_sid`, `allocated_at`, `source_pool`, `released_at` | None | Pod's current state (allocated/available/draining) |
| `voice:pod:metadata` | Field = podName, Value = JSON `{"tier":"...", "name":"..."}` | None | One big hash with metadata for ALL pods |
| `voice:merchant:config` | Field = merchantID, Value = JSON `{"tier":"...", "pool":"..."}` | None | Merchant-to-tier mapping. This is how you configure VIP merchants |

#### SET Keys (for Exclusive pools)

| Key | Members | What It Does |
|-----|---------|-------------|
| `voice:pool:{tier}:available` | Pod names | **Available pods** ready to take calls. `SPOP` removes one, `SADD` returns one |
| `voice:pool:{tier}:assigned` | Pod names | **All pods** in this tier (available + busy + draining). Used for reconciliation |
| `voice:merchant:{id}:pods` | Pod names | Available pods for a specific merchant's dedicated pool |
| `voice:merchant:{id}:assigned` | Pod names | All pods assigned to this merchant |

#### ZSET Keys (for Shared pools)

| Key | Members & Scores | What It Does |
|-----|-----------------|-------------|
| `voice:pool:{tier}:available` | Member = podName, Score = active connection count | Shared pool. Score 0 = idle, Score 3 = handling 3 calls. Lua script picks lowest-score pod under `max_concurrent` |

> **Important**: `voice:pool:{tier}:available` is a SET for exclusive tiers and a ZSET for shared tiers. The tier config determines which data structure is used.

### 7.2 Concrete Example with 5 Pods

Here's what Redis looks like with 5 voice-agent pods, all idle (no active calls):

```
# Tier assignments (which pod belongs to which tier)
voice:pod:tier:voice-agent-0  →  "gold"
voice:pod:tier:voice-agent-1  →  "standard"
voice:pod:tier:voice-agent-2  →  "standard"
voice:pod:tier:voice-agent-3  →  "standard"
voice:pod:tier:voice-agent-4  →  "basic"

# Gold pool (exclusive, SET)
voice:pool:gold:assigned   = {voice-agent-0}
voice:pool:gold:available  = {voice-agent-0}          ← 1 pod available

# Standard pool (exclusive, SET)
voice:pool:standard:assigned  = {voice-agent-1, voice-agent-2, voice-agent-3}
voice:pool:standard:available = {voice-agent-1, voice-agent-2, voice-agent-3}  ← 3 pods available

# Basic pool (shared, ZSET - note the scores!)
voice:pool:basic:assigned  = {voice-agent-4}           ← SET (tracking)
voice:pool:basic:available = {voice-agent-4: 0}        ← ZSET (score=0, idle)

# Metadata (one hash for all pods)
voice:pod:metadata = {
    "voice-agent-0": '{"tier":"gold","name":"voice-agent-0"}',
    "voice-agent-1": '{"tier":"standard","name":"voice-agent-1"}',
    "voice-agent-2": '{"tier":"standard","name":"voice-agent-2"}',
    "voice-agent-3": '{"tier":"standard","name":"voice-agent-3"}',
    "voice-agent-4": '{"tier":"basic","name":"voice-agent-4"}'
}

# No active calls, so these DON'T exist:
# voice:call:*     (no call records)
# voice:lease:*    (no leases)
# voice:pod:draining:*  (nothing draining)
```

### 7.3 Redis State During an Active Call

A call `call-abc123` comes in for merchant `acme-corp` (gold tier). After allocation:

```
# CHANGED: voice-agent-0 was REMOVED from the available set
voice:pool:gold:available  = {}                        ← EMPTY (pod was SPOP'd out)
voice:pool:gold:assigned   = {voice-agent-0}           ← Still here (tracking)

# NEW: Call record
voice:call:call-abc123 = {
    pod_name: "voice-agent-0",
    source_pool: "pool:gold",
    merchant_id: "acme-corp",
    allocated_at: "1738936800"
}    ← TTL: 24 hours

# NEW: Lease (marks pod as busy)
voice:lease:voice-agent-0 = "call-abc123"              ← TTL: 24 hours

# CHANGED: Pod status updated
voice:pod:voice-agent-0 = {
    status: "allocated",
    allocated_call_sid: "call-abc123",
    allocated_at: "1738936800",
    source_pool: "pool:gold"
}

# Everything else unchanged
voice:pool:standard:available = {voice-agent-1, voice-agent-2, voice-agent-3}
voice:pool:basic:available    = {voice-agent-4: 0}
```

### 7.4 Redis State After Release

Call `call-abc123` ends. After release:

```
# CHANGED: voice-agent-0 RETURNED to available set
voice:pool:gold:available  = {voice-agent-0}           ← Back!

# DELETED:
# voice:call:call-abc123      ← gone
# voice:lease:voice-agent-0   ← gone

# CHANGED: Pod status back to available
voice:pod:voice-agent-0 = {
    status: "available",
    released_at: "1738937100"
}

# Back to the initial state!
```

---

## 8. Allocation Algorithm (Step by Step)

File: `internal/allocator/allocator.go`

### 8.1 Idempotency Check (Lua Script)

**Why?** Telephony providers sometimes send the same webhook twice (retries). Without idempotency, the same call would get TWO pods.

**How it works (Lua script, runs atomically in Redis):**

```lua
-- File: internal/allocator/idempotency.go

local call_key = KEYS[1]  -- voice:call:{callSID}

-- Step 1: Check if this call already has an allocation
local existing = redis.call('HGETALL', call_key)
if #existing > 0 then
    return existing  -- Return the existing allocation (idempotent!)
end

-- Step 2: No existing allocation → acquire a 30-second lock
redis.call('HSET', call_key, '_lock', '1')
redis.call('EXPIRE', call_key, 30)
return {}  -- Empty means "lock acquired, proceed with allocation"
```

**The 30-second lock**: If the allocation crashes between getting the lock and storing the result, the lock auto-expires after 30s. This prevents a permanently "claimed but never allocated" state.

**Race condition handling**: If two requests for the same call_sid hit Redis simultaneously, the Lua script is atomic — only one will see the empty hash and acquire the lock. The other will see the `_lock` field and wait/retry.

### 8.2 Merchant Config Lookup

```
HGET voice:merchant:config {merchantID}
→ Returns JSON: {"tier": "gold", "pool": ""}
→ If not found or error: defaults to {"tier": "standard"}
```

### 8.3 The Fallback Chain

This is the core allocation logic. It tries pools in order:

```
Is the merchant configured for a specific tier?
│
├── tier = "dedicated" + pool = "acme"
│   └── Try: SPOP voice:merchant:acme:pods
│       └── Found? → Done!
│       └── Empty? → Fall through ↓
│
├── tier = "gold"
│   └── Try: SPOP voice:pool:gold:available
│       └── Found? → Done!
│       └── Empty? → Fall through ↓
│
├── (always tried)
│   └── Try: SPOP voice:pool:standard:available
│       └── Found? → Done!
│       └── Empty? → Fall through ↓
│
├── (always tried)
│   └── Try: SPOP voice:pool:overflow:available
│       └── Found? → Done!
│       └── Empty? → Fall through ↓
│
└── (always tried, last resort)
    └── Try: Lua script on voice:pool:basic:available (ZSET)
        └── Found? → Done!
        └── Empty? → 503 "No pods available"
```

**Key detail**: Gold tier merchants fall through to standard → overflow → basic. But merchants configured for `standard` tier do NOT try gold first (gold is reserved for VIPs).

### 8.4 Exclusive Pool Allocation (SPOP)

File: `internal/allocator/exclusive.go`

```go
// Simplified logic:
for attempt := 0; attempt < 10; attempt++ {
    podName = SPOP(poolKey)        // Atomic: removes + returns random member
    if poolEmpty {
        return ErrNoPodsAvailable
    }
    if EXISTS("voice:pod:draining:" + podName) {
        continue  // Skip draining pods (don't return to pool)
    }
    return podName  // Success!
}
```

**Why 10 attempts?** If many pods are draining, SPOP might keep pulling draining pods. After 10 tries, give up.

**Why not return draining pods to the pool?** They'll be cleaned up by the zombie cleanup process. Returning them would just cause them to be SPOP'd and skipped again.

### 8.5 Shared Pool Allocation (Lua + ZSET)

File: `internal/allocator/shared.go`

```lua
local pool_key = KEYS[1]              -- voice:pool:basic:available (ZSET)
local max_concurrent = tonumber(ARGV[1])  -- e.g., 3

-- Get ALL pods sorted by score ascending (least busy first)
local result = redis.call('ZRANGE', pool_key, 0, -1, 'WITHSCORES')
if #result == 0 then return nil end

for i = 1, #result, 2 do
    local pod_name = result[i]
    local current_score = tonumber(result[i + 1])

    -- If this pod is at capacity, all remaining are too (sorted ASC)
    if current_score >= max_concurrent then
        return nil
    end

    -- Skip draining pods
    if redis.call('EXISTS', 'voice:pod:draining:' .. pod_name) == 0 then
        redis.call('ZINCRBY', pool_key, 1, pod_name)  -- Increment connections
        return pod_name
    end
end
return nil  -- All pods draining or at capacity
```

**Visual example** (max_concurrent=3):
```
Before allocation:
  voice:pool:basic:available = {agent-4: 1, agent-5: 0}
  ↑ agent-5 has score 0 (least busy), picks agent-5

After allocation:
  voice:pool:basic:available = {agent-5: 1, agent-4: 1}
  ↑ agent-5 score incremented from 0 to 1
```

### 8.6 Storing the Allocation

After successfully finding a pod, three Redis writes happen:

```
# 1. Call record (HASH, 24h TTL)
HSET voice:call:{callSID}
    pod_name       = "voice-agent-0"
    source_pool    = "pool:gold"        # or "merchant:acme"
    merchant_id    = "acme-corp"
    allocated_at   = "1738936800"
HDEL voice:call:{callSID} _lock         # Remove the idempotency lock field
EXPIRE voice:call:{callSID} 86400       # 24h TTL

# 2. Pod state (HASH, no TTL)
HSET voice:pod:voice-agent-0
    status              = "allocated"
    allocated_call_sid  = "call-abc123"
    allocated_at        = "1738936800"
    source_pool         = "pool:gold"

# 3. Lease (STRING, 24h TTL)
SET voice:lease:voice-agent-0 "call-abc123" EX 86400
```

---

## 9. Release Algorithm (Step by Step)

File: `internal/releaser/releaser.go`

```
Input: call_sid = "call-abc123"

Step 1: Look up call
  HGETALL voice:call:call-abc123
  → {pod_name: "voice-agent-0", source_pool: "pool:gold", merchant_id: "acme-corp"}
  → If not found: return 404 "call not found"

Step 2: Check draining
  EXISTS voice:pod:draining:voice-agent-0
  → If draining: skip adding back to pool (pod is being removed)

Step 3: Return pod to pool (if not draining)
  Parse source_pool:
  ├── "pool:gold"       → exclusive → SADD voice:pool:gold:available voice-agent-0
  ├── "pool:standard"   → exclusive → SADD voice:pool:standard:available voice-agent-0
  ├── "pool:basic"      → shared    → Lua: ZINCRBY voice:pool:basic:available voice-agent-0 -1
  └── "merchant:acme"   → exclusive → SADD voice:merchant:acme:pods voice-agent-0

Step 4: Clean up lease
  DEL voice:lease:voice-agent-0
  (For shared pools: only delete if new score <= 0, meaning no more connections)

Step 5: Update pod state
  HSET voice:pod:voice-agent-0
      status      = "available"    (or "draining" if was draining)
      released_at = "1738937100"
  HDEL voice:pod:voice-agent-0 allocated_call_sid allocated_at

Step 6: Delete call record
  DEL voice:call:call-abc123

Return: {success: true, pod_name: "voice-agent-0", released_to_pool: "pool:gold"}
```

---

## 10. Pool Manager (Background Brain)

### 10.1 What the Pool Manager Does

The Pool Manager runs ONLY on the leader pod. It has three jobs:

```
Pool Manager
├── K8s Watcher     → Real-time: react to pod events instantly
├── Reconciler      → Periodic: full sync every 60 seconds
└── Zombie Cleanup  → Periodic: recover lost pods every 30 seconds
```

### 10.2 Kubernetes Watcher

File: `internal/poolmanager/watcher.go`

Watches for pod events in the `voice-system` namespace with label `app=voice-agent`.

| Event | Condition | Action |
|-------|-----------|--------|
| **ADDED** | Pod is Ready + has IP | `addPodToPool()` — assign tier, add to available pool |
| **MODIFIED** | Became ready (wasn't before) | `addPodToPool()` |
| **MODIFIED** | No longer ready (was before) | `removePodFromPool()` — clean everything |
| **DELETED** | Always | `removePodFromPool()` — clean everything |

**"Ready" means**: Pod phase is `Running` AND the `PodReady` condition is `True`.

**"Registered" means**: The pod exists in ANY `assigned` set in Redis.

### 10.3 Reconciler (Full Sync)

File: `internal/poolmanager/reconciler.go`

Runs on startup + every 60 seconds. Handles drift between K8s reality and Redis state.

```
Step 1: List all K8s pods matching label selector
Step 2: Get all pods from Redis (scan all assigned sets)
Step 3: For each K8s pod:
  ├── Ready + has IP? → addPodToPool() (idempotent — no-op if already there)
  └── Not ready?      → removePodFromPool()
Step 4: For each Redis pod NOT in K8s (ghost pods):
  └── removePodFromPool() — pod was deleted, clean up Redis
```

**`addPodToPool()` detailed flow:**

```
Is this pod already assigned a tier?
├── YES → ensurePodInPool() (make sure it's in the right available pool)
└── NO  → autoAssignTier() → store tier → add to assigned + available sets
```

**`removePodFromPool()` detailed flow:**

```
1. For EVERY tier in config:
   - SREM voice:pool:{tier}:assigned {podName}
   - SREM or ZREM voice:pool:{tier}:available {podName}
   - SREM voice:merchant:{tier}:assigned {podName}
   - SREM voice:merchant:{tier}:pods {podName}
2. Also clean hardcoded tiers: gold, standard, overflow
3. Check for orphaned call:
   - HGET voice:pod:{podName} allocated_call_sid
   - If found: DEL voice:call:{callSID}  ← crucial cleanup!
4. HDEL voice:pod:metadata {podName}
5. DEL voice:pod:{podName}
6. DEL voice:pod:tier:{podName}
7. DEL voice:lease:{podName}
```

### 10.4 Tier Assigner (Auto-Assign)

File: `internal/poolmanager/tierassigner.go`

When a new pod appears and has no tier, the system assigns one. Priority order:

```
1. Check voice:pod:tier:{podName} → already assigned? Use that
2. Merchant pools:    For each merchant tier, is SCARD < target? → assign
3. Gold:              Is SCARD voice:pool:gold:assigned < gold.target? → assign to gold
4. Shared pools:      For each shared tier, is SCARD < target? → assign
5. Standard:          Is SCARD voice:pool:standard:assigned < standard.target? → assign to standard
6. Overflow:          Is SCARD voice:pool:overflow:assigned < overflow.target? → assign to overflow
7. Default:           Fall back to standard
```

**With production config** `{"gold": {target:1}, "standard": {target:1}, "basic": {target:1, shared}}`:
- First pod → gold (needs 1, has 0)
- Second pod → basic (needs 1, has 0)
- Third pod → standard (needs 1, has 0)
- Fourth+ pods → standard (default fallback)

### 10.5 Zombie Cleanup

File: `internal/poolmanager/zombie.go`

Runs every 30 seconds. A "zombie" is a pod that is assigned to a tier but missing from the available pool.

```
For each tier:
  Get all pods in voice:pool:{tier}:assigned
  For each pod:
    ├── Is it draining? → skip
    ├── SHARED tier:
    │   └── ZSCORE voice:pool:{tier}:available {podName}
    │       └── Missing? → ZADD voice:pool:{tier}:available {podName} 0  (recover!)
    └── EXCLUSIVE tier:
        ├── Has active lease? → skip (legitimately busy)
        └── SISMEMBER voice:pool:{tier}:available {podName}
            └── Missing? + isPodEligible? → SADD voice:pool:{tier}:available {podName}  (recover!)
```

**When does this happen?**
- After a pod crash: the watcher removes the pod, K8s recreates it, the watcher adds it back — but if the timing is off, the pod might be in `assigned` but not `available`. Zombie cleanup catches this.
- After Redis data loss: if keys are partially lost, zombie cleanup rebuilds the available pools from the assigned sets.
- After a race condition: any scenario where a pod falls through the cracks.

### 10.6 Pod Removal and Orphan Cleanup

When a pod is removed (crash, scale-down, deletion), the system MUST clean up its active call:

```
Pod voice-agent-2 dies
        │
        ▼
removePodFromPool():
  1. Remove from all pools (assigned + available)
  2. Check: HGET voice:pod:voice-agent-2 allocated_call_sid
     └── Returns "call-xyz789"
  3. DEL voice:call:call-xyz789  ← orphaned call cleaned up!
  4. DEL voice:pod:voice-agent-2
  5. DEL voice:pod:tier:voice-agent-2
  6. DEL voice:lease:voice-agent-2
```

Without this, the call record would persist for 24 hours (its TTL), and the caller would try to reach a dead pod.

---

## 11. Draining a Pod

File: `internal/drainer/drainer.go`

Draining means "stop sending new calls to this pod, but let the current call finish."

```
POST /api/v1/drain {"pod_name": "voice-agent-2"}

Step 1: Check for active lease
  EXISTS voice:lease:voice-agent-2 → true/false

Step 2: Get pod's tier
  GET voice:pod:tier:voice-agent-2 → "standard"

Step 3: Remove from available pool
  SREM voice:pool:standard:available voice-agent-2
  (Pod stays in voice:pool:standard:assigned!)

Step 4: Set draining flag
  SET voice:pod:draining:voice-agent-2 "true" EX 360  (6 minutes)

Step 5: Response
  {success: true, has_active_call: true, message: "Pod drained..."}
```

**Auto-recovery**: The draining key has a 6-minute TTL. After it expires:
- The zombie cleanup (every 30s) will notice the pod is in `assigned` but not in `available` and not draining
- It will add the pod back to `available`
- The pod is "undrained" automatically

**Manual undrain**: Delete the draining key: `DEL voice:pod:draining:voice-agent-2`

---

## 12. Leader Election (How It Works)

File: `internal/poolmanager/manager.go`

Uses Kubernetes' built-in Lease API (coordination.k8s.io).

```
┌──────────────────────────────────────────────────┐
│ Kubernetes Lease: "smart-router-leader"           │
│ Namespace: voice-system                           │
│                                                   │
│ Holder: smart-router-pod-abc (the current leader) │
│ Lease Duration: 15 seconds                        │
│ Renew Deadline: 10 seconds                        │
│ Retry Period: 2 seconds                           │
└──────────────────────────────────────────────────┘
```

**How it works:**
1. All 3 Smart Router pods compete for the lease
2. One wins → becomes leader → runs Pool Manager
3. The leader must renew the lease every 10 seconds (within the 15s duration)
4. If the leader crashes or can't renew:
   - The lease expires after 15 seconds
   - Another pod acquires the lease → becomes the new leader
   - The new leader does a full sync (reconciler) first, then starts watching

**If the leader LOSES leadership** (e.g., network partition):
- It sends SIGTERM to itself
- This triggers graceful shutdown
- It stops handling API requests
- Kubernetes restarts the pod

**RBAC required:**
```yaml
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  verbs: ["get", "create", "update", "patch"]
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get", "list", "watch"]
```

---

## 13. API Endpoints (Complete Reference)

All endpoints are prefixed with `/api/v1/` except health/ready/metrics.

### Allocation Endpoints

#### `POST /api/v1/allocate`
**Use**: Generic allocation (called by Voice Agent Python code)
```json
// Request
{"call_sid": "call-abc123", "merchant_id": "acme-corp"}

// Success Response (200)
{
  "success": true,
  "allocation": {
    "pod_name": "voice-agent-0",
    "ws_url": "wss://buddy.breezelabs.app/ws/pod/voice-agent-0/call-abc123",
    "source_pool": "pool:gold",
    "allocated_at": "2026-02-08T10:00:00Z",
    "was_existing": false
  }
}

// No Pods (503)
{"success": false, "error": "no pods available"}

// Bad Request (400)
{"success": false, "error": "call_sid is required"}
```

#### `POST /api/v1/twilio/allocate`
**Use**: Twilio webhook (form-encoded)
```
// Request: Form data from Twilio
CallSid=CA123&From=+1234567890&To=+0987654321
?merchant_id=acme-corp  (query param)

// Success Response (200): TwiML XML
<Response>
  <Connect>
    <Stream url="wss://buddy.breezelabs.app/ws/pod/voice-agent-0/CA123"/>
  </Connect>
</Response>

// Error Response: TwiML XML
<Response>
  <Say>All agents are currently busy. Please try again later.</Say>
  <Hangup/>
</Response>
```

#### `POST /api/v1/plivo/allocate`
**Use**: Plivo webhook (form-encoded)
```
// Request: Form data from Plivo
CallUUID=plivo-uuid-123&From=+1234567890&To=+0987654321
?merchant_id=acme-corp  (query param)

// Success Response (200): Plivo XML
<Response>
  <Stream bidirectional="true" keepCallAlive="true"
         contentType="audio/x-mulaw;rate=8000">
    wss://buddy.breezelabs.app/ws/pod/voice-agent-0/plivo-uuid-123
  </Stream>
</Response>
```

#### `POST /api/v1/exotel/allocate`
**Use**: Exotel webhook (JSON)
```json
// Request
{"CallSid": "exo-sid-123", "merchant_id": "acme-corp"}

// Success Response (200)
{"url": "wss://buddy.breezelabs.app/ws/pod/voice-agent-0/exo-sid-123"}
```

### Release Endpoint

#### `POST /api/v1/release`
```json
// Request
{"call_sid": "call-abc123"}

// Success (200)
{
  "success": true,
  "pod_name": "voice-agent-0",
  "released_to_pool": "pool:gold",
  "was_draining": false
}

// Not Found (404)
{"success": false, "error": "call not found"}
```

### Management Endpoints

#### `POST /api/v1/drain`
```json
// Request
{"pod_name": "voice-agent-2"}

// Response (200)
{
  "success": true,
  "pod_name": "voice-agent-2",
  "has_active_call": true,
  "message": "Pod drained successfully"
}
```

#### `GET /api/v1/status`
```json
{
  "pools": {
    "gold:available": 1,
    "gold:assigned": 1,
    "standard:available": 3,
    "standard:assigned": 3,
    "basic:available": 1,
    "basic:assigned": 1
  },
  "active_calls": 0,
  "is_leader": true,
  "status": "up"
}
```

#### `GET /api/v1/pod/{pod_name}`
```json
{
  "pod_name": "voice-agent-0",
  "tier": "gold",
  "is_draining": false,
  "has_active_lease": false,
  "lease_call_sid": ""
}
```

### Health Endpoints

| Endpoint | Success | Failure | Used By |
|----------|---------|---------|---------|
| `GET /health` | `{"status": "ok"}` 200 | `{"status": "unhealthy"}` 503 | K8s liveness probe |
| `GET /ready` | `{"status": "ready"}` 200 | `{"status": "not ready"}` 503 | K8s readiness probe |
| `GET /metrics` | Prometheus text format | — | Prometheus scraping |

---

## 14. Voice Agent Integration (Python Side)

### 14.1 Feature Flags and Rollout

The Voice Agent has a multi-layered gate before using the Smart Router:

```
Should this call use Smart Router?
│
├── 1. ENABLE_VOICE_AGENT_POD_ISOLATION == false?
│      → NO (env var kill switch, must be "true")
│
├── 2. Circuit breaker is OPEN?
│      → NO (Smart Router is failing, back off)
│
├── 3. SMART_ROUTER_PERCENTAGE == 0?
│      → NO (feature disabled via DevCycle flag)
│
├── 4. SMART_ROUTER_PERCENTAGE < 100?
│      → Use deterministic hash: md5(merchant_id) % 100
│        If hash >= percentage → NO
│        This ensures same merchant always gets same decision
│
├── 5. SMART_ROUTER_MERCHANTS is non-empty AND
│      merchant_id NOT in list?
│      → NO (merchant not whitelisted)
│
└── All checks pass → YES, use Smart Router!
```

**Rollout strategy:**
1. Start with `SMART_ROUTER_PERCENTAGE=0` (off for everyone)
2. Set `SMART_ROUTER_MERCHANTS=acme-corp,test-merchant` (whitelist specific merchants)
3. Increase `SMART_ROUTER_PERCENTAGE=10` → 10% of traffic
4. Monitor, increase to 50%, then 100%

### 14.2 SmartRouterClient

File: `app/helpers/breeze_buddy/smart_router_client.py`

The Python class that talks to the Smart Router:

```python
class SmartRouterClient:
    base_url = "http://smart-router:8080"  # In-cluster URL
    timeout  = 3000ms
    retries  = 3

    async def allocate_pod(call_sid, merchant_id, provider):
        # POST /api/v1/allocate
        # Returns PodAllocation or None
        # Retries up to 3 times (except on 503)
        # 503 = no pods, returns None immediately

    async def release_pod(call_sid, reason):
        # POST /api/v1/release
        # 200 = success, 404 = already released (both return True)
        # Idempotent — safe to call multiple times

    async def drain_pod(pod_name):
        # POST /api/v1/drain

    async def get_pool_status():
        # GET /api/v1/status

    async def health_check():
        # GET /health → bool
```

### 14.3 Circuit Breaker

Protects the Voice Agent from cascading failures when Smart Router is down.

```
States:       CLOSED  ←→  OPEN  ←→  HALF_OPEN

CLOSED (normal):
  Every success → reset failure counter
  Every failure → increment failure counter
  5 consecutive failures → switch to OPEN

OPEN (backing off):
  All requests immediately return None (don't even try)
  After 30 seconds → switch to HALF_OPEN

HALF_OPEN (testing recovery):
  Allow up to 3 requests through
  3 consecutive successes → switch to CLOSED (recovered!)
  Any single failure → switch back to OPEN (not ready yet)
```

**Config:**
| Setting | Env Var | Default |
|---------|---------|---------|
| Failure threshold | `SMART_ROUTER_CB_FAILURE_THRESHOLD` | 5 |
| Recovery timeout | `SMART_ROUTER_CB_RECOVERY_TIMEOUT` | 30s |
| Half-open max calls | `SMART_ROUTER_CB_HALF_OPEN_MAX_CALLS` | 3 |

### 14.4 Four Release Mechanisms (Defense in Depth)

The system has FOUR different ways to release a pod. This redundancy ensures pods are never permanently stuck.

```
┌──────────────────────────────────────────────────────────────┐
│                     RELEASE MECHANISMS                        │
│                                                              │
│  1. WebSocket Close (PRIMARY)                                │
│     When: WebSocket disconnects (call ends, error, timeout)  │
│     Where: websocket.py finally block                        │
│     Reliability: Very high — fires on any disconnection      │
│                                                              │
│  2. Call Completion Callback                                 │
│     When: Voice agent finishes processing the call           │
│     Where: calls.py handle_call_completion()                 │
│     Reliability: High — fires after call logic completes     │
│                                                              │
│  3. Unanswered Call Handler                                  │
│     When: Outbound call not answered (WebSocket never opens) │
│     Where: calls.py handle_unanswered_calls()                │
│     Reliability: Critical for outbound — no WebSocket = no   │
│                  mechanism #1                                 │
│                                                              │
│  4. Status Callback (BACKUP)                                 │
│     When: Telephony provider reports call ended              │
│     Where: callbacks/handlers.py handle_callback_status()    │
│     Reliability: Last resort — depends on provider webhook   │
│                                                              │
│  All call: POST /api/v1/release {"call_sid": "..."}          │
│  All are IDEMPOTENT — duplicates are safe (404 = already     │
│  released, treated as success)                               │
└──────────────────────────────────────────────────────────────┘
```

### 14.5 Fallback When Smart Router Is Down

When Smart Router is unavailable, calls are NOT dropped. They fall back to non-isolated routing.

```
Smart Router down / circuit breaker open / allocation returns None
        │
        ▼
Is SMART_ROUTER_ENABLE_FALLBACK = true?
├── YES: Return a DEFAULT WebSocket URL (no pod name embedded)
│        e.g., wss://domain/agent/voice/breeze-buddy/twilio/callback/...
│        This routes through nginx to ANY available Clair pod
│        Call works normally, just without pod isolation
│
└── NO:  Return "busy" message
         Twilio: <Say>All agents are currently busy</Say><Hangup/>
         Exotel: HTTP 503
```

---

## 15. Webhook Flows (Twilio, Plivo, Exotel)

### Option A: Direct to Smart Router

```
TWILIO                      SMART ROUTER
  │  POST /api/v1/twilio/     │
  │  allocate?merchant_id=X   │
  │ ─────────────────────────>│
  │                           │ allocate pod
  │  TwiML <Stream url=       │
  │   "wss://pod-specific">   │
  │ <─────────────────────────│
  │                           │
  │  Connect WebSocket        │  VOICE AGENT POD
  │ ──────────────────────────────────>│
  │         Audio streaming           │
```

### Option B: Via Voice Agent (Clair)

```
TWILIO                 VOICE AGENT (Clair)              SMART ROUTER
  │  POST /allocate      │                                │
  │ ────────────────────>│                                │
  │                      │ POST /api/v1/allocate          │
  │                      │ ──────────────────────────────>│
  │                      │ {pod_name, ws_url}             │
  │                      │ <──────────────────────────────│
  │  TwiML <Stream       │                                │
  │   url="pod-specific">│                                │
  │ <────────────────────│                                │
  │                      │                                │
  │  Connect WebSocket   │  VOICE AGENT POD               │
  │ ─────────────────────────────>│                       │
```

### Exotel-Specific Flow

```
EXOTEL                 VOICE AGENT (Clair)              SMART ROUTER
  │  GET /voicebot-url   │                                │
  │ ────────────────────>│                                │
  │                      │ allocate_pod(call_sid, ...)     │
  │                      │ ──────────────────────────────>│
  │                      │ {ws_url: "wss://pod/agent-0"}  │
  │                      │ <──────────────────────────────│
  │  JSON {"url":        │                                │
  │   "wss://pod/agent-0│                                │
  │   ?template=..."}    │                                │
  │ <────────────────────│                                │
  │                      │                                │
  │  Connect WebSocket   │                                │
  │ ─────────────────────────────>│ (voice-agent-0)       │
```

---

## 16. Environment Variables (Both Repos)

### Smart Router (Go) - orchestration-api-go

| Variable | Default | Description |
|----------|---------|-------------|
| **Redis** | | |
| `REDIS_URL` | `redis://localhost:6379` | Redis connection string |
| `REDIS_POOL_SIZE` | `10` | Connection pool size |
| `REDIS_MIN_IDLE_CONN` | `5` | Min idle connections |
| `REDIS_MAX_RETRIES` | `3` | Max retries on failure |
| **HTTP** | | |
| `HTTP_PORT` | `8080` | API server port |
| `METRICS_PORT` | `9090` | Prometheus metrics port |
| `HTTP_READ_TIMEOUT` | `5s` | Request read timeout |
| `HTTP_WRITE_TIMEOUT` | `10s` | Response write timeout |
| `HTTP_SHUTDOWN_TIMEOUT` | `30s` | Graceful shutdown timeout |
| **Voice Agent** | | |
| `VOICE_AGENT_BASE_URL` | `wss://localhost:8081` | Base URL for pod WebSocket URLs |
| **Kubernetes** | | |
| `NAMESPACE` | `default` | K8s namespace to watch |
| `POD_LABEL_SELECTOR` | `app=voice-agent` | Label selector for voice-agent pods |
| `POD_NAME` | `smart-router-local` | This pod's identity (downward API) |
| **Timers** | | |
| `CLEANUP_INTERVAL` | `30s` | Zombie cleanup interval |
| `LEASE_TTL` | `24h` | Call lease expiry |
| `DRAINING_TTL` | `6m` | Draining flag auto-expiry |
| `RECONCILE_INTERVAL` | `60s` | Full K8s/Redis sync interval |
| **Tier Config** | | |
| `TIER_CONFIG` | `{}` (defaults) | JSON tier configuration |
| **Leader Election** | | |
| `LEADER_ELECTION_ENABLED` | `true` | Enable leader election |
| `LEADER_ELECTION_NAMESPACE` | (same as NAMESPACE) | Lease namespace |
| `LEADER_ELECTION_LOCK_NAME` | `smart-router-leader` | Lease resource name |
| `LEADER_ELECTION_DURATION` | `15s` | Lease duration |
| `LEADER_ELECTION_RENEW_DEADLINE` | `10s` | Renew deadline |
| `LEADER_ELECTION_RETRY_PERIOD` | `2s` | Retry period |
| **Logging** | | |
| `LOG_LEVEL` | `info` | Log level |
| `LOG_FORMAT` | `json` | Format: `json` or `console` |

### Voice Agent (Python) - orchestration-clair

| Variable | Default | Description |
|----------|---------|-------------|
| **Master Switch** | | |
| `ENABLE_VOICE_AGENT_POD_ISOLATION` | `false` | Enable Smart Router integration |
| **Smart Router Connection** | | |
| `SMART_ROUTER_BASE_URL` | `http://smart-router:8080` | Smart Router API URL |
| `SMART_ROUTER_API_KEY` | `""` | Bearer token |
| `SMART_ROUTER_TIMEOUT_MS` | `3000` | HTTP timeout (ms) |
| `SMART_ROUTER_RETRY_ATTEMPTS` | `3` | Allocation retry count |
| `SMART_ROUTER_ENABLE_FALLBACK` | `true` | Fall back to non-isolated on failure |
| **Circuit Breaker** | | |
| `SMART_ROUTER_CB_FAILURE_THRESHOLD` | `5` | Failures before circuit opens |
| `SMART_ROUTER_CB_RECOVERY_TIMEOUT` | `30` | Seconds before half-open |
| `SMART_ROUTER_CB_HALF_OPEN_MAX_CALLS` | `3` | Successes to close circuit |
| **Pod Identity** | | |
| `POD_NAME` | `""` | This pod's StatefulSet name |
| `POD_IP` | `""` | This pod's IP |
| `VOICE_AGENT_BASE_URL` | `""` | WebSocket base URL |
| `VOICE_AGENT_HEARTBEAT_INTERVAL` | `30` | Redis heartbeat interval (s) |
| `VOICE_AGENT_HEARTBEAT_TTL` | `90` | Heartbeat expiry (s) |
| `VOICE_AGENT_SHUTDOWN_TIMEOUT` | `300` | Shutdown wait for active calls (s) |

### Feature Flags (DevCycle/Redis, runtime-changeable)

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `SMART_ROUTER_PERCENTAGE` | int 0-100 | `0` | Traffic percentage through Smart Router |
| `SMART_ROUTER_MERCHANTS` | comma-separated | `""` | Merchant whitelist |
| `SMART_ROUTER_ENABLE_TWILIO_DIRECT` | bool | `false` | Twilio calls Smart Router directly |
| `SMART_ROUTER_ENABLE_PLIVO_DIRECT` | bool | `false` | Plivo calls Smart Router directly |

---

## 17. Tier Configuration

The tier config is set via `TIER_CONFIG` environment variable (JSON string) in the Smart Router's ConfigMap.

### Production Config (current)

```json
{
  "gold": {
    "type": "exclusive",
    "target": 1
  },
  "standard": {
    "type": "exclusive",
    "target": 1
  },
  "basic": {
    "type": "shared",
    "target": 1,
    "max_concurrent": 3
  }
}
```

**What this means with 5 pods:**
- 1 pod → gold (VIP exclusive)
- 1 pod → basic (shared, up to 3 concurrent calls)
- 3 pods → standard (general exclusive), because: 1 for target + 2 overflow into default
- Total capacity: 1 (gold) + 3 (standard) + 3 (basic shared) = **7 concurrent calls on 5 pods**

### Config Field Reference

| Field | Values | Description |
|-------|--------|-------------|
| `type` | `"exclusive"` or `"shared"` | Exclusive = 1 call/pod (SET), Shared = N calls/pod (ZSET) |
| `target` | integer | How many pods should be assigned to this tier |
| `max_concurrent` | integer | (Shared only) Max calls per pod. Default: 5 |

### Changing the Config

1. Edit the ConfigMap: `kubectl edit configmap smart-router-config -n voice-system`
2. Restart Smart Router: `kubectl rollout restart deployment/smart-router -n voice-system`
3. The leader will re-reconcile, potentially reassigning tiers

> **Note**: Changing the config may reshuffle tier assignments. The distribution is preserved, but specific pod-to-tier mappings may change.

---

## 18. Lua Scripts (The Critical Ones)

There are 3 Lua scripts embedded in the Go code. They run atomically on Redis (no race conditions).

### Script 1: Idempotency Check-and-Lock

**Purpose**: Prevent the same call from getting two pods.

```lua
-- Keys: voice:call:{callSID}
local call_key = KEYS[1]

-- Check if allocation already exists
local existing = redis.call('HGETALL', call_key)
if #existing > 0 then
    return existing  -- Return existing allocation
end

-- Acquire a 30-second lock (crash safety)
redis.call('HSET', call_key, '_lock', '1')
redis.call('EXPIRE', call_key, 30)
return {}  -- Lock acquired, proceed with allocation
```

### Script 2: Shared Pool Allocation

**Purpose**: Find the least-busy pod under the concurrency limit.

```lua
-- Keys: voice:pool:{tier}:available (ZSET)
-- Args: max_concurrent
local pool_key = KEYS[1]
local max_concurrent = tonumber(ARGV[1])

-- Get all pods sorted by score (least connections first)
local result = redis.call('ZRANGE', pool_key, 0, -1, 'WITHSCORES')
if #result == 0 then return nil end

for i = 1, #result, 2 do
    local pod_name = result[i]
    local current_score = tonumber(result[i + 1])

    if current_score >= max_concurrent then
        return nil  -- All pods at capacity
    end

    -- Skip draining pods
    if redis.call('EXISTS', 'voice:pod:draining:' .. pod_name) == 0 then
        redis.call('ZINCRBY', pool_key, 1, pod_name)  -- Claim a slot
        return pod_name
    end
end
return nil
```

### Script 3: Shared Pool Release

**Purpose**: Decrement the connection count, with a floor of 0.

```lua
-- Keys: voice:pool:{tier}:available (ZSET)
-- Args: pod_name
local pool_key = KEYS[1]
local pod_name = ARGV[1]

local current_score = redis.call('ZSCORE', pool_key, pod_name)
if current_score == false then
    return -1  -- Pod not in set
end

if tonumber(current_score) > 0 then
    return tonumber(redis.call('ZINCRBY', pool_key, -1, pod_name))
else
    return 0  -- Already at 0
end
```

---

## 19. Deployment & Build Commands

### Smart Router (Go)

```bash
# Build Docker image (we use podman, Go is not installed locally)
cd /Users/harsh.tiwari/Documents/breeze-repos/orchestration-api-go
podman build --platform=linux/amd64 \
  -t asia-south1-docker.pkg.dev/breeze-automatic-prod/smart-router/smart-router:latest .

# Authenticate with Google Artifact Registry
gcloud auth print-access-token | \
  podman login -u oauth2accesstoken --password-stdin asia-south1-docker.pkg.dev

# Push image
podman push asia-south1-docker.pkg.dev/breeze-automatic-prod/smart-router/smart-router:latest

# Deploy (restart pods to pull new image)
kubectl rollout restart deployment/smart-router -n voice-system

# Watch rollout
kubectl rollout status deployment/smart-router -n voice-system
```

### Voice Agent (Python)

```bash
# Build Docker image
cd /Users/harsh.tiwari/Documents/breeze-repos/orchestration-clair
podman build --platform linux/amd64 \
  -f Dockerfile.breeze-buddy \
  -t asia-south1-docker.pkg.dev/breeze-automatic-prod/orchestration-clair/voice-agent:latest .

# Push image
podman push asia-south1-docker.pkg.dev/breeze-automatic-prod/orchestration-clair/voice-agent:latest

# Deploy
kubectl rollout restart statefulset/voice-agent -n voice-system

# Watch rollout (StatefulSets roll one at a time)
kubectl rollout status statefulset/voice-agent -n voice-system
```

### Connecting to the Cluster

```bash
# Get credentials for GKE cluster
gcloud container clusters get-credentials \
  breeze-automatic-mum-01 \
  --region asia-south1 \
  --project breeze-automatic-prod

# Verify
kubectl get pods -n voice-system
```

---

## 20. Debugging & Operations Cheat Sheet

### Check System Health

```bash
# All pods running?
kubectl get pods -n voice-system

# Smart Router logs (look for errors)
kubectl logs -n voice-system -l app=smart-router --tail=50

# Leader pod (look for is_leader=true)
kubectl logs -n voice-system -l app=smart-router --tail=100 | grep "is_leader"

# Check Smart Router API
kubectl exec -n voice-system debug-tools -- \
  curl -s http://smart-router:8080/api/v1/status | python3 -m json.tool
```

### Redis Inspection

```bash
# Redis access (via debug pod)
REDIS_CMD="kubectl exec -n voice-system debug-tools -- redis-cli -h 10.100.0.4 -p 6379"

# List ALL Smart Router keys
$REDIS_CMD KEYS "voice:*"

# Check pool sizes
$REDIS_CMD SCARD voice:pool:gold:available
$REDIS_CMD SCARD voice:pool:standard:available
$REDIS_CMD ZCARD voice:pool:basic:available       # ZCARD for ZSET!

# Check pool members
$REDIS_CMD SMEMBERS voice:pool:gold:available
$REDIS_CMD SMEMBERS voice:pool:standard:available
$REDIS_CMD ZRANGE voice:pool:basic:available 0 -1 WITHSCORES

# Check assigned (all pods in tier, not just available)
$REDIS_CMD SMEMBERS voice:pool:gold:assigned
$REDIS_CMD SMEMBERS voice:pool:standard:assigned

# Check pod tier assignment
$REDIS_CMD GET voice:pod:tier:voice-agent-0

# Check active calls
$REDIS_CMD KEYS "voice:call:*"
$REDIS_CMD KEYS "voice:lease:*"

# Inspect a specific call
$REDIS_CMD HGETALL voice:call:call-abc123

# Inspect a specific pod
$REDIS_CMD HGETALL voice:pod:voice-agent-0

# Check draining pods
$REDIS_CMD KEYS "voice:pod:draining:*"

# Check merchant config
$REDIS_CMD HGETALL voice:merchant:config

# Check all metadata
$REDIS_CMD HGETALL voice:pod:metadata
```

### Allocation Testing

```bash
# Test allocation
kubectl exec -n voice-system debug-tools -- \
  curl -s -X POST http://smart-router:8080/api/v1/allocate \
  -H "Content-Type: application/json" \
  -d '{"call_sid": "test-call-001", "merchant_id": "test-merchant"}'

# Test release
kubectl exec -n voice-system debug-tools -- \
  curl -s -X POST http://smart-router:8080/api/v1/release \
  -H "Content-Type: application/json" \
  -d '{"call_sid": "test-call-001"}'
```

### Drain & Undrain a Pod

```bash
# Drain
kubectl exec -n voice-system debug-tools -- \
  curl -s -X POST http://smart-router:8080/api/v1/drain \
  -H "Content-Type: application/json" \
  -d '{"pod_name": "voice-agent-2"}'

# Manual undrain (delete the draining key)
$REDIS_CMD DEL voice:pod:draining:voice-agent-2
# Wait ~30s for zombie cleanup to re-add to pool
```

### Emergency: Force-Reset a Pod

```bash
# If a pod is stuck (has lease but call is dead):
$REDIS_CMD DEL voice:lease:voice-agent-0
$REDIS_CMD DEL voice:call:<the-stuck-call-sid>
$REDIS_CMD HSET voice:pod:voice-agent-0 status available
$REDIS_CMD HDEL voice:pod:voice-agent-0 allocated_call_sid allocated_at
# Zombie cleanup will re-add to pool within 30s
```

### Emergency: Full Redis Reset

```bash
# DANGER: Deletes ALL Smart Router state. Only in emergencies.
# The pool manager will rebuild everything from K8s within ~90 seconds.

$REDIS_CMD KEYS "voice:*" | xargs -n1 $REDIS_CMD DEL
# Or faster:
kubectl exec -n voice-system debug-tools -- \
  redis-cli -h 10.100.0.4 -p 6379 --scan --pattern "voice:*" | \
  xargs -n 100 kubectl exec -n voice-system debug-tools -- \
  redis-cli -h 10.100.0.4 -p 6379 DEL

# Wait ~90s for reconciler + zombie cleanup to rebuild
```

### Set Merchant Config

```bash
# Assign merchant "acme-corp" to gold tier
$REDIS_CMD HSET voice:merchant:config acme-corp '{"tier":"gold"}'

# Assign merchant "9shines" to dedicated pool
$REDIS_CMD HSET voice:merchant:config 9shines '{"tier":"dedicated","pool":"9shines"}'

# Remove merchant config (reverts to standard)
$REDIS_CMD HDEL voice:merchant:config acme-corp

# View all merchant configs
$REDIS_CMD HGETALL voice:merchant:config
```

### Scale Voice Agents

```bash
# Scale up
kubectl scale statefulset/voice-agent -n voice-system --replicas=7

# Scale down
kubectl scale statefulset/voice-agent -n voice-system --replicas=5

# Check pod status
kubectl get pods -n voice-system -l app=voice-agent
```

---

## 21. Cluster State Reference

```
Cluster:   gke_breeze-automatic-prod_asia-south1_breeze-automatic-mum-01
Namespace: voice-system
Redis:     10.100.0.4:6379 (external, accessed via Private Service Connect)
Domains:   clairvoyance.breezelabs.app (Smart Router API)
           buddy.breezelabs.app (Voice Agent WebSocket)

Components:
├── nginx-router     Deployment   2 replicas    Routes traffic, WebSocket proxy
├── smart-router     Deployment   3 replicas    Go, allocation/release/pool management
├── voice-agent      StatefulSet  5 replicas    Python/FastAPI, handles calls
├── debug-tools      Pod          1 replica     redis:7-alpine, for Redis access
└── Redis            External     PSC           10.100.0.4:6379

Images:
├── Smart Router:  asia-south1-docker.pkg.dev/breeze-automatic-prod/smart-router/smart-router:latest
└── Voice Agent:   asia-south1-docker.pkg.dev/breeze-automatic-prod/orchestration-clair/voice-agent:latest

Tier Config:
  gold:     exclusive, target=1
  standard: exclusive, target=1
  basic:    shared, target=1, max_concurrent=3
```

---

## 22. Bug Fixes Applied

These 7 bugs were found during code review, fixed, and deployed:

| # | Bug | File | Fix |
|---|-----|------|-----|
| 1 | Missing `time` import | `poolmanager/manager.go` | Added `"time"` to import block |
| 2 | `isPodEligible` swallows Redis errors | `poolmanager/watcher.go` | Returns `false` on error (fail-safe: don't add potentially busy pod) |
| 3 | Race condition: concurrent same call_sid | `allocator/idempotency.go`, `allocator/allocator.go` | Lua script atomically checks + acquires 30s lock. `storeAllocation` deletes `_lock` field. `data` map type fixed to `map[string]interface{}` |
| 4 | Shared Lua only checks first ZSET member | `allocator/shared.go` | Rewritten to iterate ALL members, skip draining, allocate first under-capacity |
| 5 | Zombie cleanup rejects shared pods | `poolmanager/zombie.go` | Removed `isPodEligible` check for shared tiers (draining check in outer loop suffices) |
| 6 | `removePodFromPool` hardcoded `SRem` breaks shared tiers | `poolmanager/reconciler.go` | Replaced with type-aware dynamic loop + fallback for gold/standard/overflow |
| 7 | `fmt.Printf` instead of structured logger | `releaser/releaser.go`, `main.go` | Added `*zap.Logger` to Releaser struct, replaced 3 `fmt.Printf` with `r.logger.Warn(...)` |

---

## 23. Known Caveats & Design Decisions

### Caveats

1. **Brief allocation failures during Smart Router rolling restart (~20s window)**: When the leader pod restarts, there's a gap where the new leader must acquire the lease and rebuild pool state. Allocations during this window may fail with 503. Non-leader pods continue serving API requests, but the pool state may be stale.

2. **Tier assignments shuffle after Redis data loss**: If all `voice:*` keys are deleted, the pool manager rebuilds everything. But `autoAssignTier` processes pods in K8s API order (not the original assignment order), so pod-to-tier mappings will differ. The tier DISTRIBUTION is preserved (correct number per tier).

3. **Circuit breaker is per-instance in Python**: Most call sites create a new `SmartRouterClient()` instead of using the singleton. This means the circuit breaker doesn't accumulate failures across requests — it only helps within one request's retry loop. The global singleton exists but is underutilized.

4. **Gold pool is reserved**: Standard-tier merchants do NOT fall through to gold. The fallback chain for standard is: standard → overflow → basic. Only gold-tier merchants get gold → standard → overflow → basic.

5. **Draining pods are consumed and discarded**: When SPOP pulls a draining pod from the exclusive pool, it's not returned. The zombie cleanup will handle it once the draining flag expires.

### Design Decisions

1. **Why Redis for state?** Fast atomic operations (Lua scripts), TTL-based auto-cleanup, and both repos already use Redis.

2. **Why leader election?** Without it, 3 Smart Router replicas would have 3 watchers + 3 reconcilers fighting each other. Only 1 should manage pool state.

3. **Why 4 release mechanisms?** Defense in depth. Any single mechanism could fail (WebSocket disconnect not detected, callback not received, etc.). The Smart Router's idempotent release makes multiple releases safe.

4. **Why Lua scripts instead of Redis transactions?** Lua runs atomically on the Redis server — no WATCH/MULTI/EXEC complexity, no retry loops on conflict, and can combine reads + writes in one round trip.

5. **Why SPOP for exclusive pools?** It's O(1) and atomic — removes and returns a random member in one operation. No race condition possible.

6. **Why ZSET for shared pools?** Scores represent connection counts, ZRANGE gives pods sorted by load, and ZINCRBY atomically increments/decrements. Perfect for load balancing across shared pods.

7. **Why 24h lease TTL?** Safety net for the longest possible call. If something goes catastrophically wrong and a call record is never cleaned up, the 24h TTL ensures it eventually expires.

8. **Why 6-minute draining TTL?** Long enough for most calls to finish (average call is 2-3 minutes), short enough that a manually-drained pod auto-recovers if you forget to undrain it.

---

## Quick Reference Card

```
ALLOCATE:  POST /api/v1/allocate      {"call_sid": "...", "merchant_id": "..."}
RELEASE:   POST /api/v1/release       {"call_sid": "..."}
DRAIN:     POST /api/v1/drain         {"pod_name": "..."}
STATUS:    GET  /api/v1/status
POD INFO:  GET  /api/v1/pod/{name}
HEALTH:    GET  /health
READY:     GET  /ready

WS URL FORMAT: wss://buddy.breezelabs.app/ws/pod/{pod-name}/{call-sid}

FALLBACK CHAIN: gold → standard → overflow → basic (shared)

KEY REDIS KEYS:
  voice:pool:{tier}:available    SET/ZSET  Available pods
  voice:pool:{tier}:assigned     SET       All pods in tier
  voice:call:{callSID}           HASH      Call → pod mapping
  voice:lease:{podName}          STRING    Pod is busy (24h TTL)
  voice:pod:draining:{podName}   STRING    Pod is draining (6m TTL)
  voice:pod:tier:{podName}       STRING    Pod's tier assignment
  voice:merchant:config          HASH      Merchant tier configs

BACKGROUND JOBS (leader only):
  Watcher:    Real-time K8s pod events
  Reconciler: Full K8s↔Redis sync every 60s
  Zombies:    Recover lost pods every 30s
```
