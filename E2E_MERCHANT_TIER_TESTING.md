# E2E Merchant & Tier Testing — Comprehensive Test Plan & Results

**Date:** Feb 11, 2026
**Cluster:** `gke_breeze-automatic-prod_asia-south1_breeze-automatic-mum-01`
**Namespace:** `voice-system`
**Redis:** `10.100.0.4:6379`
**Domain:** `clairvoyance.breezelabs.app`

---

## Table of Contents

1. [Background: How Merchant Config Works](#1-background-how-merchant-config-works)
2. [Test Environment Setup](#2-test-environment-setup)
3. [Step-by-Step Execution Procedure](#3-step-by-step-execution-procedure)
4. [Test Scenarios](#4-test-scenarios)
5. [Test Results](#5-test-results)
6. [Cleanup](#6-cleanup)

---

## 1. Background: How Merchant Config Works

### Two Redis Keys Control Everything

**Key 1: `voice:tier:config`** (STRING) — Defines which tiers exist, their types, targets, and the default fallback chain. This is the TIER_CONFIG that was previously an env var, now lives in Redis.

**Key 2: `voice:merchant:config`** (HASH) — Each field is a merchant ID, value is a JSON object configuring how that merchant allocates pods.

### Merchant Config JSON Fields

```json
{
  "tier": "gold",                              // LEGACY — backward compat, mostly ignored
  "pool": "9shines",                           // CRITICAL — maps to a TIER NAME in voice:tier:config
  "fallback": ["gold", "standard", "basic"]    // What tiers to try if dedicated pool is empty
}
```

### How `pool` Connects to `voice:tier:config`

The `pool` field in merchant config must match a **tier name** in `voice:tier:config`. But that tier must NOT be in the `default_chain` — this is how the system knows it's a merchant-dedicated tier, not a regular tier.

**Example:**

```
voice:tier:config has tiers: gold, standard, basic, 9shines, acme
                default_chain: [gold, standard, basic]

→ "9shines" and "acme" are NOT in default_chain
→ The tier assigner treats them as merchant-dedicated pools
→ Pods are assigned to voice:merchant:9shines:assigned / voice:merchant:9shines:pods
```

When allocation happens for merchant "9shines" with `pool: "9shines"`:
1. The allocator prepends `"merchant:9shines"` to the fallback chain
2. Full chain becomes: `["merchant:9shines", "gold", "standard", "basic"]`
3. First tries SPOP from `voice:merchant:9shines:pods` (the dedicated pool)
4. If empty → falls through to gold → standard → basic

### Pod Assignment Logic (autoAssignTier)

The tier assigner in the reconciler assigns pods in this priority:
1. **Step 1:** Check existing assignment (already assigned? keep it)
2. **Step 2:** Fill merchant pools FIRST — tiers NOT in default_chain, NOT shared → treated as merchant pools. Fill until target is met.
3. **Step 3:** Walk default_chain in order — fill gold, then standard, then basic until targets are met.
4. **Step 4:** Overflow to last tier in default_chain if everything is at capacity.

### Redis Keys Created Per Merchant Pod

When a pod is assigned to merchant "9shines":
```
voice:pod:tier:voice-agent-X       = "merchant:9shines"     (STRING)
voice:merchant:9shines:assigned    ∋ "voice-agent-X"        (SET member)
voice:merchant:9shines:pods        ∋ "voice-agent-X"        (SET member — available pool)
voice:pod:voice-agent-X            = {status: "available"}  (HASH)
voice:pod:metadata[voice-agent-X]  = "{...}"                (HASH field)
```

### Allocation Endpoints & Query Params

| Endpoint | CallSID Source | merchant_id Source | Response Format |
|----------|---------------|-------------------|-----------------|
| `POST /api/v1/allocate` | JSON body `call_sid` | JSON body `merchant_id` | JSON |
| `POST /api/v1/twilio/allocate` | Form field `CallSid` | Query param `?merchant_id=` | TwiML XML |
| `POST /api/v1/plivo/allocate` | Form field `CallUUID` | Query param `?merchant_id=` | Plivo XML |
| `POST /api/v1/exotel/allocate` | JSON body `CallSid` | JSON body `merchant_id` | JSON |

### Release & Drain

| Endpoint | Params | Notes |
|----------|--------|-------|
| `POST /api/v1/release` | JSON body `{"call_sid": "..."}` | Looks up pod + source_pool from voice:call:{sid} |
| `POST /api/v1/drain` | JSON body `{"pod_name": "..."}` | Removes from pool, sets draining key with 6min TTL |

### Status

| Endpoint | Params | Notes |
|----------|--------|-------|
| `GET /api/v1/status` | None | Shows all tier pools (assigned/available counts), active calls, leader status. Does NOT show merchant pools explicitly. |
| `GET /api/v1/pod/{pod_name}` | Path param | Shows pod tier, draining status, lease info |

---

## 2. Test Environment Setup

### Current Production State (before test)

- **voice-agent pods:** 3 (StatefulSet, voice-agent-0/1/2)
- **smart-router pods:** 3 (Deployment)
- **Tier config (env var):** `{"gold":{"type":"exclusive","target":1},"standard":{"type":"exclusive","target":1},"basic":{"type":"shared","target":1,"max_concurrent":3}}`
- **No merchant configs** in Redis
- **No `voice:tier:config` key** in Redis (will be bootstrapped on first deploy of new code)

### Target Test State

**Scale:** 7 voice-agent pods (voice-agent-0 through voice-agent-6)

**Tier Config** (`voice:tier:config`):

```json
{
  "tiers": {
    "gold":     {"type": "exclusive", "target": 2},
    "standard": {"type": "exclusive", "target": 1},
    "basic":    {"type": "shared", "target": 1, "max_concurrent": 3},
    "9shines":  {"type": "exclusive", "target": 2},
    "acme":     {"type": "exclusive", "target": 1}
  },
  "default_chain": ["gold", "standard", "basic"]
}
```

**Expected pod assignment after reconciliation:**

| Tier | Type | Target | Pods | Pool Type |
|------|------|--------|------|-----------|
| 9shines | exclusive | 2 | 2 pods → `voice:merchant:9shines:pods` | Merchant dedicated |
| acme | exclusive | 1 | 1 pod → `voice:merchant:acme:pods` | Merchant dedicated |
| gold | exclusive | 2 | 2 pods → `voice:pool:gold:available` | Regular |
| standard | exclusive | 1 | 1 pod → `voice:pool:standard:available` | Regular |
| basic | shared | 1 | 1 pod → `voice:pool:basic:available` (ZSET, score=0) | Regular |
| **Total** | | **7** | **7 pods** | |

**Merchant Configs** (`voice:merchant:config`):

| Merchant | Pool | Fallback | Allocation Chain |
|----------|------|----------|-----------------|
| 9shines | `9shines` | `["gold", "standard", "basic"]` | `merchant:9shines → gold → standard → basic` |
| acme | `acme` | `["standard", "basic"]` | `merchant:acme → standard → basic` |

---

## 3. Step-by-Step Execution Procedure

### Step 1: Build, Push, Deploy Smart Router

```bash
cd /Users/harsh.tiwari/Documents/breeze-repos/orchestration-api-go

# Authenticate
gcloud auth print-access-token | podman login -u oauth2accesstoken --password-stdin asia-south1-docker.pkg.dev

# Build
podman build --platform=linux/amd64 -t asia-south1-docker.pkg.dev/breeze-automatic-prod/smart-router/smart-router:latest .

# Push
podman push asia-south1-docker.pkg.dev/breeze-automatic-prod/smart-router/smart-router:latest

# Deploy
kubectl rollout restart deployment/smart-router -n voice-system
kubectl rollout status deployment/smart-router -n voice-system --timeout=120s
```

**Verify:** All 3 smart-router pods running with new image.

### Step 2: Scale Voice Agent StatefulSet to 7

```bash
kubectl scale statefulset voice-agent --replicas=7 -n voice-system
kubectl rollout status statefulset/voice-agent -n voice-system --timeout=300s
```

**Verify:** `kubectl get pods -n voice-system -l app=voice-agent` shows 7 Running pods.

### Step 3: Set Tier Config in Redis

```bash
kubectl exec -n voice-system deploy/smart-router -- sh -c "
  apk add --no-cache redis > /dev/null 2>&1;
  redis-cli -h 10.100.0.4 SET voice:tier:config '{\"tiers\":{\"gold\":{\"type\":\"exclusive\",\"target\":2},\"standard\":{\"type\":\"exclusive\",\"target\":1},\"basic\":{\"type\":\"shared\",\"target\":1,\"max_concurrent\":3},\"9shines\":{\"type\":\"exclusive\",\"target\":2},\"acme\":{\"type\":\"exclusive\",\"target\":1}},\"default_chain\":[\"gold\",\"standard\",\"basic\"]}'
"
```

**Verify:**
```bash
kubectl exec -n voice-system deploy/smart-router -- sh -c "
  redis-cli -h 10.100.0.4 GET voice:tier:config
"
```

### Step 4: Set Merchant Configs in Redis

```bash
kubectl exec -n voice-system deploy/smart-router -- sh -c "
  redis-cli -h 10.100.0.4 HSET voice:merchant:config 9shines '{\"tier\":\"gold\",\"pool\":\"9shines\",\"fallback\":[\"gold\",\"standard\",\"basic\"]}';
  redis-cli -h 10.100.0.4 HSET voice:merchant:config acme '{\"tier\":\"gold\",\"pool\":\"acme\",\"fallback\":[\"standard\",\"basic\"]}'
"
```

**Verify:**
```bash
kubectl exec -n voice-system deploy/smart-router -- sh -c "
  redis-cli -h 10.100.0.4 HGETALL voice:merchant:config
"
```

### Step 5: Wait for Config Refresh + Reconciliation

The tier config refresh runs every 30s. The reconciler runs every 60s. Wait up to 90 seconds for everything to stabilize.

```bash
sleep 90
```

### Step 6: Verify Pool State

```bash
# Status endpoint
kubectl exec -n voice-system deploy/smart-router -- curl -s http://localhost:8080/api/v1/status | python3 -m json.tool

# Check merchant pools manually
kubectl exec -n voice-system deploy/smart-router -- sh -c "
  echo '=== 9shines merchant pool ===';
  redis-cli -h 10.100.0.4 SMEMBERS voice:merchant:9shines:pods;
  redis-cli -h 10.100.0.4 SMEMBERS voice:merchant:9shines:assigned;
  echo '=== acme merchant pool ===';
  redis-cli -h 10.100.0.4 SMEMBERS voice:merchant:acme:pods;
  redis-cli -h 10.100.0.4 SMEMBERS voice:merchant:acme:assigned;
  echo '=== gold pool ===';
  redis-cli -h 10.100.0.4 SMEMBERS voice:pool:gold:available;
  redis-cli -h 10.100.0.4 SMEMBERS voice:pool:gold:assigned;
  echo '=== standard pool ===';
  redis-cli -h 10.100.0.4 SMEMBERS voice:pool:standard:available;
  redis-cli -h 10.100.0.4 SMEMBERS voice:pool:standard:assigned;
  echo '=== basic pool (ZSET) ===';
  redis-cli -h 10.100.0.4 ZRANGE voice:pool:basic:available 0 -1 WITHSCORES;
  redis-cli -h 10.100.0.4 SMEMBERS voice:pool:basic:assigned;
  echo '=== all pod tiers ===';
  for i in 0 1 2 3 4 5 6; do
    echo -n \"voice-agent-\$i: \";
    redis-cli -h 10.100.0.4 GET voice:pod:tier:voice-agent-\$i;
  done
"
```

**Expected:** 2 pods in 9shines, 1 in acme, 2 in gold, 1 in standard, 1 in basic.

---

## 4. Test Scenarios

All tests use `curl` via `kubectl exec` against `localhost:8080` on a smart-router pod.

### Naming: Call SIDs

| Test # | Call SID | Purpose |
|--------|----------|---------|
| 1 | `CA_TEST_DEFAULT_01` | Default alloc (no merchant) — expect gold |
| 2 | `CA_TEST_DEFAULT_02` | Default alloc — expect second gold pod |
| 3 | `CA_TEST_DEFAULT_03` | Default alloc — gold exhausted → standard |
| 4 | `CA_TEST_DEFAULT_04` | Default alloc — standard exhausted → basic (shared, call 1) |
| 5 | `CA_TEST_DEFAULT_05` | Default alloc — basic shared, call 2 on same pod |
| 6 | Release `CA_TEST_DEFAULT_01` | Release first gold pod |
| 7 | `CA_TEST_DEFAULT_06` | Re-allocate default — should get released gold pod back |
| 8 | `CA_TEST_9SHINES_01` | 9shines merchant alloc — expect dedicated pod |
| 9 | `CA_TEST_9SHINES_02` | 9shines merchant alloc — expect second dedicated pod |
| 10 | `CA_TEST_9SHINES_03` | 9shines exhausted — falls to gold (from 9shines fallback) |
| 11 | `CA_TEST_ACME_01` | acme merchant alloc — expect dedicated pod |
| 12 | `CA_TEST_ACME_02` | acme exhausted — falls to standard (from acme fallback) |
| 13 | `CA_TEST_TWILIO_9S` | Twilio webhook with merchant_id query param |
| 14 | Re-send `CA_TEST_9SHINES_01` | Idempotency — should return same pod, was_existing=true |
| 15 | Release all active calls | Clean up |
| 16 | `GET /api/v1/status` | Verify all pools restored |
| 17 | Drain test | Drain a pod, then allocate — verify it's skipped |
| 18 | `CA_TEST_SHARED_01/02/03/04` | Shared pool concurrent limit — 3 succeed, 4th falls through |

### Test Commands

**Test 1: Default allocation (no merchant) — expect gold**
```bash
kubectl exec -n voice-system deploy/smart-router -- curl -s -X POST http://localhost:8080/api/v1/allocate \
  -H 'Content-Type: application/json' \
  -d '{"call_sid":"CA_TEST_DEFAULT_01"}'
```
Expected: `source_pool: "pool:gold"`, some gold pod.

**Test 2: Default allocation — second gold pod**
```bash
kubectl exec -n voice-system deploy/smart-router -- curl -s -X POST http://localhost:8080/api/v1/allocate \
  -H 'Content-Type: application/json' \
  -d '{"call_sid":"CA_TEST_DEFAULT_02"}'
```
Expected: `source_pool: "pool:gold"`, the OTHER gold pod.

**Test 3: Default allocation — gold exhausted → standard**
```bash
kubectl exec -n voice-system deploy/smart-router -- curl -s -X POST http://localhost:8080/api/v1/allocate \
  -H 'Content-Type: application/json' \
  -d '{"call_sid":"CA_TEST_DEFAULT_03"}'
```
Expected: `source_pool: "pool:standard"`.

**Test 4: Default allocation — standard exhausted → basic (shared)**
```bash
kubectl exec -n voice-system deploy/smart-router -- curl -s -X POST http://localhost:8080/api/v1/allocate \
  -H 'Content-Type: application/json' \
  -d '{"call_sid":"CA_TEST_DEFAULT_04"}'
```
Expected: `source_pool: "pool:basic"`, basic pod, shared.

**Test 5: Default allocation — basic shared, second call on same pod**
```bash
kubectl exec -n voice-system deploy/smart-router -- curl -s -X POST http://localhost:8080/api/v1/allocate \
  -H 'Content-Type: application/json' \
  -d '{"call_sid":"CA_TEST_DEFAULT_05"}'
```
Expected: `source_pool: "pool:basic"`, SAME basic pod (score goes from 1 → 2).

**Test 6: Release first default call**
```bash
kubectl exec -n voice-system deploy/smart-router -- curl -s -X POST http://localhost:8080/api/v1/release \
  -H 'Content-Type: application/json' \
  -d '{"call_sid":"CA_TEST_DEFAULT_01"}'
```
Expected: `released_to_pool: "pool:gold"`, gold pod returned to available.

**Test 7: Re-allocate default — released gold pod should come back**
```bash
kubectl exec -n voice-system deploy/smart-router -- curl -s -X POST http://localhost:8080/api/v1/allocate \
  -H 'Content-Type: application/json' \
  -d '{"call_sid":"CA_TEST_DEFAULT_06"}'
```
Expected: `source_pool: "pool:gold"`, the released gold pod.

**Test 8: 9shines merchant — first dedicated pod**
```bash
kubectl exec -n voice-system deploy/smart-router -- curl -s -X POST http://localhost:8080/api/v1/allocate \
  -H 'Content-Type: application/json' \
  -d '{"call_sid":"CA_TEST_9SHINES_01","merchant_id":"9shines"}'
```
Expected: `source_pool: "merchant:9shines"`, one of the 9shines dedicated pods.

**Test 9: 9shines merchant — second dedicated pod**
```bash
kubectl exec -n voice-system deploy/smart-router -- curl -s -X POST http://localhost:8080/api/v1/allocate \
  -H 'Content-Type: application/json' \
  -d '{"call_sid":"CA_TEST_9SHINES_02","merchant_id":"9shines"}'
```
Expected: `source_pool: "merchant:9shines"`, the OTHER 9shines dedicated pod.

**Test 10: 9shines exhausted — falls to gold fallback**
```bash
kubectl exec -n voice-system deploy/smart-router -- curl -s -X POST http://localhost:8080/api/v1/allocate \
  -H 'Content-Type: application/json' \
  -d '{"call_sid":"CA_TEST_9SHINES_03","merchant_id":"9shines"}'
```
Expected: `source_pool: "pool:gold"` (the one gold pod we released and re-allocated is now taken, so this might get that pod OR fall further to standard/basic depending on current gold availability).

**Test 11: acme merchant — dedicated pod**
```bash
kubectl exec -n voice-system deploy/smart-router -- curl -s -X POST http://localhost:8080/api/v1/allocate \
  -H 'Content-Type: application/json' \
  -d '{"call_sid":"CA_TEST_ACME_01","merchant_id":"acme"}'
```
Expected: `source_pool: "merchant:acme"`, the acme dedicated pod.

**Test 12: acme exhausted — falls to standard (acme fallback skips gold)**
```bash
kubectl exec -n voice-system deploy/smart-router -- curl -s -X POST http://localhost:8080/api/v1/allocate \
  -H 'Content-Type: application/json' \
  -d '{"call_sid":"CA_TEST_ACME_02","merchant_id":"acme"}'
```
Expected: `source_pool: "pool:standard"` or `"pool:basic"` (depends on what's available — standard may already be taken by test 3).

**Test 13: Twilio webhook with merchant_id query param**
```bash
kubectl exec -n voice-system deploy/smart-router -- curl -s -X POST \
  'http://localhost:8080/api/v1/twilio/allocate?merchant_id=9shines' \
  -d 'CallSid=CA_TEST_TWILIO_9S'
```
Expected: TwiML XML response with `<Stream>` URL. Since 9shines dedicated pool is exhausted, should fall to gold/standard/basic.

**Test 14: Idempotency — re-send same call_sid**
```bash
kubectl exec -n voice-system deploy/smart-router -- curl -s -X POST http://localhost:8080/api/v1/allocate \
  -H 'Content-Type: application/json' \
  -d '{"call_sid":"CA_TEST_9SHINES_01","merchant_id":"9shines"}'
```
Expected: Same pod as test 8, `was_existing: true`.

**Test 15: Release ALL active calls**
```bash
for sid in CA_TEST_DEFAULT_02 CA_TEST_DEFAULT_03 CA_TEST_DEFAULT_04 CA_TEST_DEFAULT_05 CA_TEST_DEFAULT_06 CA_TEST_9SHINES_01 CA_TEST_9SHINES_02 CA_TEST_9SHINES_03 CA_TEST_ACME_01 CA_TEST_ACME_02 CA_TEST_TWILIO_9S; do
  echo "--- Releasing $sid ---"
  kubectl exec -n voice-system deploy/smart-router -- curl -s -X POST http://localhost:8080/api/v1/release \
    -H 'Content-Type: application/json' \
    -d "{\"call_sid\":\"$sid\"}"
  echo ""
done
```

**Test 16: Status check after release**
```bash
kubectl exec -n voice-system deploy/smart-router -- curl -s http://localhost:8080/api/v1/status | python3 -m json.tool
```
Expected: All pools show available = assigned (everything returned to pools).

**Test 17: Drain test**
```bash
# Pick a gold pod (check which one from status)
# Drain it
kubectl exec -n voice-system deploy/smart-router -- curl -s -X POST http://localhost:8080/api/v1/drain \
  -H 'Content-Type: application/json' \
  -d '{"pod_name":"voice-agent-X"}'

# Now allocate — should skip the drained pod
kubectl exec -n voice-system deploy/smart-router -- curl -s -X POST http://localhost:8080/api/v1/allocate \
  -H 'Content-Type: application/json' \
  -d '{"call_sid":"CA_TEST_DRAIN_01"}'

# Release
kubectl exec -n voice-system deploy/smart-router -- curl -s -X POST http://localhost:8080/api/v1/release \
  -H 'Content-Type: application/json' \
  -d '{"call_sid":"CA_TEST_DRAIN_01"}'

# Wait 6min for draining TTL to expire, or just note the behavior
```

**Test 18: Shared pool concurrent limit**
```bash
# First release everything to start clean, then:
# Allocate 3 calls to basic (exhaust gold + standard first, or directly test after release)
# The 4th call should get 503 or fall to a different pool

# This test should be run with all exclusive pools occupied so allocation
# is forced to basic. With everything released, allocations will go to
# gold first, so we need to fill gold(2) + standard(1) first:
for i in 01 02 03; do
  kubectl exec -n voice-system deploy/smart-router -- curl -s -X POST http://localhost:8080/api/v1/allocate \
    -H 'Content-Type: application/json' \
    -d "{\"call_sid\":\"CA_TEST_FILL_$i\"}"
done

# Now basic is the only option. Send 3 calls:
for i in 01 02 03; do
  kubectl exec -n voice-system deploy/smart-router -- curl -s -X POST http://localhost:8080/api/v1/allocate \
    -H 'Content-Type: application/json' \
    -d "{\"call_sid\":\"CA_TEST_SHARED_$i\"}"
done

# 4th call — basic at max_concurrent=3, should get 503
kubectl exec -n voice-system deploy/smart-router -- curl -s -X POST http://localhost:8080/api/v1/allocate \
  -H 'Content-Type: application/json' \
  -d '{"call_sid":"CA_TEST_SHARED_04"}'

# Release all
for i in 01 02 03; do
  kubectl exec -n voice-system deploy/smart-router -- curl -s -X POST http://localhost:8080/api/v1/release \
    -H 'Content-Type: application/json' \
    -d "{\"call_sid\":\"CA_TEST_FILL_$i\"}"
  kubectl exec -n voice-system deploy/smart-router -- curl -s -X POST http://localhost:8080/api/v1/release \
    -H 'Content-Type: application/json' \
    -d "{\"call_sid\":\"CA_TEST_SHARED_$i\"}"
done
```
Expected: First 3 shared calls succeed (same basic pod, scores 1→2→3). 4th gets 503 (no pods available anywhere in default chain).

---

## 5. Test Results

> Results will be filled in during test execution. Each test records the full curl response.

### Pre-Test: Pool State After Setup

```
STATUS RESPONSE:
(to be filled)

MERCHANT POOLS:
(to be filled)

POD TIER ASSIGNMENTS:
(to be filled)
```

### Test 1: Default allocation → gold
```
REQUEST:  POST /api/v1/allocate {"call_sid":"CA_TEST_DEFAULT_01"}
RESPONSE: (to be filled)
RESULT:   ☐ PASS / ☐ FAIL
```

### Test 2: Default allocation → second gold
```
REQUEST:  POST /api/v1/allocate {"call_sid":"CA_TEST_DEFAULT_02"}
RESPONSE: (to be filled)
RESULT:   ☐ PASS / ☐ FAIL
```

### Test 3: Default allocation → standard (gold exhausted)
```
REQUEST:  POST /api/v1/allocate {"call_sid":"CA_TEST_DEFAULT_03"}
RESPONSE: (to be filled)
RESULT:   ☐ PASS / ☐ FAIL
```

### Test 4: Default allocation → basic shared (standard exhausted)
```
REQUEST:  POST /api/v1/allocate {"call_sid":"CA_TEST_DEFAULT_04"}
RESPONSE: (to be filled)
RESULT:   ☐ PASS / ☐ FAIL
```

### Test 5: Default allocation → basic shared (second call, same pod)
```
REQUEST:  POST /api/v1/allocate {"call_sid":"CA_TEST_DEFAULT_05"}
RESPONSE: (to be filled)
RESULT:   ☐ PASS / ☐ FAIL
```

### Test 6: Release first gold call
```
REQUEST:  POST /api/v1/release {"call_sid":"CA_TEST_DEFAULT_01"}
RESPONSE: (to be filled)
RESULT:   ☐ PASS / ☐ FAIL
```

### Test 7: Re-allocate default → released gold pod
```
REQUEST:  POST /api/v1/allocate {"call_sid":"CA_TEST_DEFAULT_06"}
RESPONSE: (to be filled)
RESULT:   ☐ PASS / ☐ FAIL
```

### Test 8: 9shines merchant → dedicated pod
```
REQUEST:  POST /api/v1/allocate {"call_sid":"CA_TEST_9SHINES_01","merchant_id":"9shines"}
RESPONSE: (to be filled)
RESULT:   ☐ PASS / ☐ FAIL
```

### Test 9: 9shines merchant → second dedicated pod
```
REQUEST:  POST /api/v1/allocate {"call_sid":"CA_TEST_9SHINES_02","merchant_id":"9shines"}
RESPONSE: (to be filled)
RESULT:   ☐ PASS / ☐ FAIL
```

### Test 10: 9shines exhausted → fallback to gold/standard/basic
```
REQUEST:  POST /api/v1/allocate {"call_sid":"CA_TEST_9SHINES_03","merchant_id":"9shines"}
RESPONSE: (to be filled)
RESULT:   ☐ PASS / ☐ FAIL
```

### Test 11: acme merchant → dedicated pod
```
REQUEST:  POST /api/v1/allocate {"call_sid":"CA_TEST_ACME_01","merchant_id":"acme"}
RESPONSE: (to be filled)
RESULT:   ☐ PASS / ☐ FAIL
```

### Test 12: acme exhausted → fallback to standard/basic
```
REQUEST:  POST /api/v1/allocate {"call_sid":"CA_TEST_ACME_02","merchant_id":"acme"}
RESPONSE: (to be filled)
RESULT:   ☐ PASS / ☐ FAIL
```

### Test 13: Twilio webhook with merchant_id query param
```
REQUEST:  POST /api/v1/twilio/allocate?merchant_id=9shines  FormData: CallSid=CA_TEST_TWILIO_9S
RESPONSE: (to be filled — expect TwiML XML)
RESULT:   ☐ PASS / ☐ FAIL
```

### Test 14: Idempotency — re-send same call_sid
```
REQUEST:  POST /api/v1/allocate {"call_sid":"CA_TEST_9SHINES_01","merchant_id":"9shines"}
RESPONSE: (to be filled — expect was_existing: true)
RESULT:   ☐ PASS / ☐ FAIL
```

### Test 15: Release ALL active calls
```
RESPONSES: (to be filled)
RESULT:    ☐ PASS / ☐ FAIL
```

### Test 16: Status after full release
```
REQUEST:  GET /api/v1/status
RESPONSE: (to be filled)
RESULT:   ☐ PASS / ☐ FAIL (all available = assigned)
```

### Test 17: Drain test
```
DRAIN REQUEST:    POST /api/v1/drain {"pod_name":"voice-agent-X"}
DRAIN RESPONSE:   (to be filled)
ALLOC REQUEST:    POST /api/v1/allocate {"call_sid":"CA_TEST_DRAIN_01"}
ALLOC RESPONSE:   (to be filled — should NOT get drained pod)
RELEASE RESPONSE: (to be filled)
RESULT:           ☐ PASS / ☐ FAIL
```

### Test 18: Shared pool concurrent limit (max_concurrent=3)
```
FILL RESPONSES (gold+standard): (to be filled)
SHARED CALL 1: (to be filled — basic pod, score=1)
SHARED CALL 2: (to be filled — same basic pod, score=2)
SHARED CALL 3: (to be filled — same basic pod, score=3)
SHARED CALL 4: (to be filled — expect 503)
RELEASE ALL:   (to be filled)
RESULT:        ☐ PASS / ☐ FAIL
```

---

## 6. Cleanup

After all tests are complete, restore production to original state:

```bash
# Scale back to 3 pods
kubectl scale statefulset voice-agent --replicas=3 -n voice-system

# Restore original tier config (3 pods: gold=1, standard=1, basic=1)
kubectl exec -n voice-system deploy/smart-router -- sh -c "
  redis-cli -h 10.100.0.4 SET voice:tier:config '{\"tiers\":{\"gold\":{\"type\":\"exclusive\",\"target\":1},\"standard\":{\"type\":\"exclusive\",\"target\":1},\"basic\":{\"type\":\"shared\",\"target\":1,\"max_concurrent\":3}},\"default_chain\":[\"gold\",\"standard\",\"basic\"]}'
"

# Remove merchant configs
kubectl exec -n voice-system deploy/smart-router -- sh -c "
  redis-cli -h 10.100.0.4 HDEL voice:merchant:config 9shines;
  redis-cli -h 10.100.0.4 HDEL voice:merchant:config acme
"

# Wait for reconciler to clean up (60-90s)
sleep 90

# Verify restored state
kubectl exec -n voice-system deploy/smart-router -- curl -s http://localhost:8080/api/v1/status | python3 -m json.tool
kubectl get pods -n voice-system -l app=voice-agent
```

**Note:** The reconciler will automatically clean up the merchant pool Redis keys when the extra pods are terminated and the tier config no longer references 9shines/acme tiers.
