# Smart Router - Infrastructure & URLs Quick Reference

## 🚀 One-Line Summary
Smart Router is deployed and working. Main endpoint: `https://clairvoyance.breezelabs.app`

---

## 📍 Quick Access URLs

| Purpose | URL | Method |
|---------|-----|--------|
| **Health Check** | https://clairvoyance.breezelabs.app/health | GET |
| **Ready Check** | https://clairvoyance.breezelabs.app/ready | GET |
| **Allocate Pod** | https://clairvoyance.breezelabs.app/api/v1/allocate | POST |
| **Release Pod** | https://clairvoyance.breezelabs.app/api/v1/release | POST |
| **Status** | https://clairvoyance.breezelabs.app/api/v1/status | GET |
| **Twilio Webhook** | https://clairvoyance.breezelabs.app/agent/voice/breeze-buddy/twilio/callback/ | POST |
| **Plivo Webhook** | https://clairvoyance.breezelabs.app/agent/voice/breeze-buddy/plivo/callback/ | POST |
| **Exotel Webhook** | https://clairvoyance.breezelabs.app/agent/voice/breeze-buddy/exotel/callback/ | POST |
| **WebSocket Base** | wss://buddy.breezelabs.app | WebSocket |

---

## 🏗️ Infrastructure

### Cluster
- **Name**: breeze-automatic-mum-01
- **Project**: breeze-automatic-prod
- **Region**: asia-south1
- **Context**: `gke_breeze-automatic-prod_asia-south1_breeze-automatic-mum-01`

### Namespace
```bash
kubectl config use-context gke_breeze-automatic-prod_asia-south1_breeze-automatic-mum-01
kubectl config set-context --current --namespace=voice-system
```

### Services
```bash
# nginx-router: 34.118.225.185:80
# smart-router: 34.118.227.34:8080
# voice-agent: Headless service (no ClusterIP)
```

### Voice Agent Pods
```
voice-agent-0  → 10.196.7.6  (Tier: gold)
voice-agent-1  → 10.196.4.5  (Tier: standard)
voice-agent-2  → 10.196.5.5  (Tier: gold)
```

### Redis
```
URL: redis://10.100.0.4:6379
Prefix: voice:
```

---

## 🧪 Quick Test Commands

### Test Allocation
```bash
curl -X POST https://clairvoyance.breezelabs.app/api/v1/allocate \
  -H "Content-Type: application/json" \
  -d '{"call_sid":"test-001","merchant_id":"test"}'
```

**Expected Response:**
```json
{
  "pod_name": "voice-agent-1",
  "source_pool": "pool:standard",
  "success": true,
  "was_existing": false,
  "ws_url": "wss://buddy.breezelabs.app/ws/pod/voice-agent-1/test-001"
}
```

### Test Release
```bash
curl -X POST https://clairvoyance.breezelabs.app/api/v1/release \
  -H "Content-Type: application/json" \
  -d '{"call_sid":"test-001"}'
```

### Test Twilio Webhook
```bash
curl -X POST https://clairvoyance.breezelabs.app/agent/voice/breeze-buddy/twilio/callback/ \
  -d "CallSid=test-twilio-001"
```

**Expected Response:**
```xml
<?xml version="1.0" encoding="UTF-8"?>
<Response>
  <Connect>
    <Stream url="wss://buddy.breezelabs.app/ws/pod/voice-agent-1/test-twilio-001"/>
  </Connect>
</Response>
```

---

## 🔧 Common Operations

### View Logs
```bash
# Smart Router
kubectl logs -f deployment/smart-router

# Nginx Router
kubectl logs -f deployment/nginx-router

# Specific Voice Agent
kubectl logs -f voice-agent-0
```

### Restart Services
```bash
# Restart Smart Router
kubectl rollout restart deployment/smart-router

# Restart Nginx
kubectl rollout restart deployment/nginx-router

# Restart Voice Agents
kubectl rollout restart statefulset voice-agent
```

### Scale
```bash
# Scale Smart Router
kubectl scale deployment smart-router --replicas=5

# Scale Voice Agents
kubectl scale statefulset voice-agent --replicas=10
```

### Check Redis
```bash
# List all keys
kubectl run redis-cli --rm -i --restart=Never \
  --image=redis:7-alpine -- redis-cli -h 10.100.0.4 KEYS "voice:*"

# Check available pods in pool
kubectl run redis-cli --rm -i --restart=Never \
  --image=redis:7-alpine -- redis-cli -h 10.100.0.4 SMEMBERS voice:pool:standard:available
```

---

## 📊 Current Status

| Component | Status | Replicas |
|-----------|--------|----------|
| Smart Router | ✅ Running | 3/3 |
| Nginx Router | ✅ Running | 2/2 |
| Voice Agents | ✅ Running | 3/3 |
| Redis | ✅ Running | Connected |

---

## 🎯 What's Working

- ✅ Basic pod allocation
- ✅ Pod release
- ✅ Twilio webhook
- ✅ Health/readiness checks
- ✅ Redis connection
- ✅ Pod pool management
- ✅ Tier-based allocation (gold/standard)

---

## ⚠️ What's NOT Tested (See COMPREHENSIVE_TESTING_GUIDE.md)

- 🔴 Pod failure during active call
- 🔴 Redis connection failure
- 🔴 Rolling updates
- 🔴 Concurrent allocation race conditions
- 🔴 Plivo/Exotel webhooks
- 🔴 Scale up/down scenarios
- 🔴 Load testing
- 🔴 Full E2E call flow
- 🔴 WebSocket connection handling
- 🔴 Idempotency

**Total: 48 tests pending**

---

## 📚 Full Documentation

See: `docs/COMPREHENSIVE_TESTING_GUIDE.md`

This contains:
- Detailed test procedures
- Edge case scenarios
- Failure recovery tests
- Performance benchmarks
- Security validation
- Complete infrastructure details

---

## 🆘 Troubleshooting

### 503 No Pods Available
```bash
# Check if all pods are allocated
curl https://clairvoyance.breezelabs.app/api/v1/status

# Check Redis for stuck calls
kubectl run redis-cli --rm -i --restart=Never --image=redis:7-alpine \
  -- redis-cli -h 10.100.0.4 KEYS "voice:call:*"

# Release stuck calls manually if needed
```

### 404 Not Found
- Check URL path (should end with `/` for webhooks)
- Verify nginx config is updated
- Check Smart Router routes: `kubectl logs deployment/smart-router | grep 404`

### Redis Connection Issues
```bash
# Test Redis connectivity
kubectl run redis-test --rm -i --restart=Never --image=redis:7-alpine \
  -- redis-cli -h 10.100.0.4 PING

# Should return: PONG
```

---

## 📞 Need Help?

1. Check logs: `kubectl logs -f deployment/smart-router`
2. Check status: `curl https://clairvoyance.breezelabs.app/api/v1/status`
3. Review full guide: `docs/COMPREHENSIVE_TESTING_GUIDE.md`

---

*Last Updated: February 8, 2026*
*Deployment: COMPLETE ✅*
*Testing: IN PROGRESS ⚠️ (48 tests pending)*
