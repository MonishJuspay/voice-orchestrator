# Flow 14 — Nginx Routing

## Overview

Nginx sits in front of both the Smart Router and the voice-agent pods, providing domain-based routing for `clairvoyance.breezelabs.app`. It serves four primary functions: (1) proxying Smart Router API calls, (2) routing WebSocket connections to specific voice-agent pods by name, (3) providing direct pod access for debugging, and (4) catch-all routing to any healthy voice-agent pod via the headless service.

**Active production config:** `k8s/nginx-config-simple.conf` (214 lines)

**Legacy config (NOT active):** `k8s/nginx-config-updated.conf` — uses nginx `auth_request` to have nginx itself call the Smart Router allocation endpoint before proxying. This approach was replaced by the simpler current design where the telephony provider calls `/api/v1/allocate` directly and receives a WSURL.

---

## Source Files

| File | Lines | Status | Purpose |
|------|-------|--------|---------|
| `k8s/nginx-config-simple.conf` | 214 | **ACTIVE** | Production nginx config |
| `k8s/nginx-config-updated.conf` | 161 | Legacy | Alternative auth_request approach (not deployed) |
| `k8s/nginx-deployment.yaml` | — | Active | 2-replica Deployment for nginx-router |

---

## Architecture: How Nginx Fits In

```
                     Internet
                        │
                        ▼
                ┌───────────────┐
                │  GKE Ingress  │
                │  (L7 LB)      │
                └───────┬───────┘
                        │
                        ▼
           clairvoyance.breezelabs.app:443 (TLS terminated at LB)
                        │
                        ▼
              ┌─────────────────┐
              │  nginx-router   │  ← 2 replicas, port 8080
              │  (this config)  │
              └────────┬────────┘
                       │
          ┌────────────┼────────────────┐
          │            │                │
          ▼            ▼                ▼
    ┌───────────┐ ┌──────────┐  ┌──────────────┐
    │ smart-    │ │ voice-   │  │ voice-agent  │
    │ router    │ │ agent-0  │  │ headless svc │
    │ :8080     │ │ :8000    │  │ (any pod)    │
    └───────────┘ └──────────┘  └──────────────┘
```

---

## Global Configuration

### Worker Settings

```
nginx-config-simple.conf:1-9
```

```nginx
worker_processes auto;        # One worker per CPU core
events {
    worker_connections 10240;  # High limit for concurrent connections
    use epoll;                 # Linux-optimized event model
    multi_accept on;           # Accept multiple connections per event loop
}
```

### K8s DNS Resolver

```
nginx-config-simple.conf:29-35
```

```nginx
resolver ${KUBE_DNS_IP} valid=10s ipv6=off;
```

**Critical detail:** `${KUBE_DNS_IP}` is substituted at container startup via `envsubst`. This is required because:

1. **Variable-based `proxy_pass`** (e.g. `proxy_pass http://$target_pod...`) forces nginx to resolve DNS at runtime, not at startup
2. Nginx's resolver does **NOT** read `/etc/resolv.conf` — it needs an explicit DNS server
3. The resolver does **NOT** use K8s search domains — all hostnames must be **FQDNs** (e.g. `voice-agent-2.voice-agent.voice-system.svc.cluster.local`)
4. `valid=10s` — DNS cache TTL. Pod IP changes (due to restarts) are picked up within 10 seconds
5. `ipv6=off` — K8s pods are IPv4-only

### Smart Router Upstream

```
nginx-config-simple.conf:44-47
```

```nginx
upstream smart_router {
    server smart-router:8080;
}
```

Resolved **once at startup** (not variable-based, so uses standard K8s DNS with search domains). Points to the `smart-router` ClusterIP Service which load-balances across 3 smart-router replicas.

### Non-Root Configuration

```
nginx-config-simple.conf:37-42
```

Nginx runs as non-root (uid 101), so all temp paths point to `/tmp/`:
```nginx
client_body_temp_path /tmp/client_temp;
proxy_temp_path /tmp/proxy_temp;
```

### Log Format

```
nginx-config-simple.conf:15-18
```

Custom log format includes upstream address and status for debugging routing:
```
rt=$request_time uaddr=$upstream_addr ustatus=$upstream_status
```

---

## Server Blocks

### Primary Server: `clairvoyance.breezelabs.app`

```
nginx-config-simple.conf:58-187
```

Listens on port 8080 (TLS is terminated at the GKE load balancer). Only matches requests with `Host: clairvoyance.breezelabs.app`.

---

### Location 1: Health & Ready (Nginx's Own Probes)

```
nginx-config-simple.conf:65-75
```

```nginx
location /health { return 200 'healthy\n'; }
location /ready  { return 200 'ready\n'; }
```

These are nginx's **own** K8s liveness/readiness probes — they do NOT proxy to the smart-router or voice-agent. They always return 200 as long as nginx is running. `access_log off` keeps probe logs quiet.

**Note:** The smart-router has its own health/ready endpoints at `/api/v1/health` and `/api/v1/ready` which DO check Redis connectivity.

---

### Location 2: Smart Router API — `/api/v1/`

```
nginx-config-simple.conf:80-89
```

```nginx
location /api/v1/ {
    proxy_pass http://smart_router;
    ...
}
```

**Pass-through, no rewrite.** The request path is forwarded unchanged because `proxy_pass` has no trailing path. Example:

```
Client request:  POST /api/v1/allocate
Nginx forwards:  POST /api/v1/allocate → smart-router:8080
```

This handles ALL Smart Router endpoints:
- `POST /api/v1/allocate` — generic allocation
- `POST /api/v1/twilio/allocate` — Twilio-specific
- `POST /api/v1/plivo/allocate` — Plivo-specific
- `POST /api/v1/exotel/allocate` — Exotel-specific
- `POST /api/v1/release`
- `POST /api/v1/drain`
- `GET /api/v1/status`
- `GET /api/v1/pod/{pod_name}`
- `GET /api/v1/health`
- `GET /api/v1/ready`
- `GET /api/v1/metrics`

**Timeouts:** `connect=5s`, `read=30s`, `send=30s` — appropriate for short API calls.

---

### Location 3: WebSocket — `/ws/pod/{pod_name}/...`

```
nginx-config-simple.conf:103-122
```

This is the **core routing logic** — it connects telephony providers to specific voice-agent pods.

#### The Full Connection Flow

```
1. Telephony provider calls:
   POST https://clairvoyance.breezelabs.app/api/v1/twilio/allocate

2. Smart Router returns:
   {
     "ws_url": "wss://clairvoyance.breezelabs.app/ws/pod/voice-agent-2/agent/voice/breeze-buddy/twilio/callback/order-confirmation/v2",
     "pod_name": "voice-agent-2",
     "call_sid": "CA123..."
   }

3. Telephony provider opens WebSocket to ws_url:
   GET wss://clairvoyance.breezelabs.app/ws/pod/voice-agent-2/agent/voice/breeze-buddy/twilio/callback/order-confirmation/v2

4. Nginx regex captures:
   pod_name  = "voice-agent-2"
   rest_path = "/agent/voice/breeze-buddy/twilio/callback/order-confirmation/v2"

5. Nginx proxies to:
   http://voice-agent-2.voice-agent.voice-system.svc.cluster.local:8000/agent/voice/breeze-buddy/twilio/callback/order-confirmation/v2
```

#### Regex Breakdown

```nginx
location ~ ^/ws/pod/(?<pod_name>[^/]+)(?<rest_path>/.*)$ {
```

| Capture Group | Pattern | Example Value |
|--------------|---------|---------------|
| `pod_name` | `[^/]+` (one or more non-slash chars) | `voice-agent-2` |
| `rest_path` | `/.*` (slash + everything after) | `/agent/voice/breeze-buddy/twilio/callback/order-confirmation/v2` |

#### DNS Resolution

```nginx
set $target_pod "$pod_name.voice-agent.voice-system.svc.cluster.local:8000";
proxy_pass http://$target_pod$rest_path;
```

The variable in `proxy_pass` forces **runtime DNS resolution** using the configured `resolver`. The FQDN follows K8s StatefulSet DNS convention:

```
{pod_name}.{headless-service}.{namespace}.svc.cluster.local
```

Since voice-agents are a StatefulSet with a headless service named `voice-agent`, each pod gets a stable DNS name like `voice-agent-0.voice-agent.voice-system.svc.cluster.local`.

#### WebSocket Headers

```nginx
proxy_set_header Upgrade $http_upgrade;
proxy_set_header Connection "upgrade";
```

Required for HTTP → WebSocket upgrade. Without these, the connection stays HTTP and the WebSocket handshake fails.

#### Voice Call Timeouts

```nginx
proxy_connect_timeout 3s;
proxy_read_timeout 3600s;   # 1 hour
proxy_send_timeout 3600s;   # 1 hour
```

Voice calls can last up to 1 hour. The short 3s connect timeout ensures fast failure if the pod is unreachable.

#### Buffering Disabled

```nginx
proxy_buffering off;
proxy_cache off;
```

WebSocket frames must be forwarded immediately — buffering would add latency to real-time voice data.

#### Fallback on Error

```nginx
proxy_intercept_errors on;
error_page 502 504 = @ws_fallback;
```

If the target pod returns **502 Bad Gateway** or **504 Gateway Timeout** (pod is down/unreachable), nginx falls back to the `@ws_fallback` location.

---

### Location 4: WebSocket Fallback — `@ws_fallback`

```
nginx-config-simple.conf:124-138
```

```nginx
location @ws_fallback {
    set $fallback_target "voice-agent.voice-system.svc.cluster.local:8000";
    proxy_pass http://$fallback_target$rest_path;
    ...
}
```

Routes to the **headless service** (DNS round-robin to any healthy pod) instead of the specific allocated pod. This is a best-effort recovery — the call may land on a pod that doesn't have the call context, but it's better than a hard failure.

**When this triggers:** The allocated pod died between allocation and WebSocket connection (race condition during rolling updates). The zombie cleanup will eventually clean up the orphaned allocation.

Same WebSocket headers and 1-hour timeouts as the primary WebSocket location.

---

### Location 5: Direct Pod Access — `/{voice-agent-N}/...`

```
nginx-config-simple.conf:144-153
```

```nginx
location ~ ^/(?<pod_name>voice-agent-\d+)(?<rest_path>/.*)$ {
    set $target_pod "$pod_name.voice-agent.voice-system.svc.cluster.local:8000";
    proxy_pass http://$target_pod$rest_path;
    ...
}
```

**Debug/admin only.** Allows direct access to a specific pod without going through allocation. Examples:

```
GET /voice-agent-1/health    → voice-agent-1:8000/health
GET /voice-agent-0/metrics   → voice-agent-0:8000/metrics
```

**Regex:** `voice-agent-\d+` — only matches StatefulSet pod naming convention. Standard 30s timeouts (not WebSocket timeouts).

---

### Location 6: Nginx Stub Status

```
nginx-config-simple.conf:158-164
```

```nginx
location /nginx-status {
    stub_status on;
    allow 10.0.0.0/8;
    allow 127.0.0.1;
    deny all;
}
```

Exposes nginx connection stats (active connections, accepts, handled, requests). Restricted to cluster-internal IPs only (`10.0.0.0/8`).

---

### Location 7: Catch-All — `/`

```
nginx-config-simple.conf:175-186
```

```nginx
location / {
    set $voice_agent "voice-agent.voice-system.svc.cluster.local:8000";
    proxy_pass http://$voice_agent;
    ...
}
```

Everything that doesn't match the above locations goes to the **headless voice-agent service** (DNS round-robin). This includes:

- **Plivo answer webhooks** — Plivo sends the initial call to a general webhook URL (e.g. `/agent/voice/breeze-buddy/plivo/answer`) that doesn't include a pod name
- **Twilio/Plivo/Exotel status callbacks** — call status updates
- **Any other voice-agent endpoints**

Standard 30s timeouts — these are regular HTTP requests, not long-lived WebSocket connections.

---

### Default Server: Reject Unknown Domains

```
nginx-config-simple.conf:192-212
```

```nginx
server {
    listen 8080 default_server;
    server_name _;
    ...
    location / {
        return 404 '{"error":"unknown domain"}\n';
    }
}
```

Any request that doesn't match `clairvoyance.breezelabs.app` in the `Host` header gets a 404. Health/ready probes still work on the default server (K8s probes don't set a Host header).

---

## Location Match Priority

Nginx evaluates locations in this order:

| Priority | Type | Pattern | Matches |
|----------|------|---------|---------|
| 1 | Exact prefix | `/health`, `/ready` | K8s probes |
| 2 | Prefix | `/api/v1/` | Smart Router API |
| 3 | Prefix | `/nginx-status` | Stub status |
| 4 | Regex (order matters) | `^/ws/pod/...` | WebSocket to specific pod |
| 5 | Regex (order matters) | `^/(?:voice-agent-\d+)/...` | Direct pod access |
| 6 | Catch-all prefix | `/` | Everything else → headless service |

**Important:** The WebSocket regex (`/ws/pod/...`) is listed before the direct pod access regex (`/voice-agent-\d+/...`) in the config file. Since nginx evaluates regex locations in config order, WebSocket paths are matched first.

---

## WSURL Construction: End-to-End

This is how the pieces connect across Smart Router and Nginx:

### Smart Router Side (allocator.go — `buildWSURL`)

```
base_url  = "wss://clairvoyance.breezelabs.app"  (from VOICE_AGENT_BASE_URL env var)
pod_name  = "voice-agent-2"                        (allocated pod)
path      = "/agent/voice/breeze-buddy/twilio/callback/order-confirmation/v2"  (from request)

ws_url    = "wss://clairvoyance.breezelabs.app/ws/pod/voice-agent-2/agent/voice/..."
```

The Smart Router **inserts `/ws/pod/{pod_name}`** between the base URL and the original path.

### Nginx Side (this config)

```
Receives:  /ws/pod/voice-agent-2/agent/voice/breeze-buddy/twilio/callback/order-confirmation/v2
Captures:  pod_name=voice-agent-2, rest_path=/agent/voice/breeze-buddy/twilio/callback/order-confirmation/v2
Forwards:  http://voice-agent-2.voice-agent.voice-system.svc.cluster.local:8000/agent/voice/...
```

Nginx **strips `/ws/pod/{pod_name}`** and forwards `rest_path` to the specific pod.

### Voice Agent Side

The voice-agent pod receives a clean path (`/agent/voice/...`) — it has no idea nginx routing happened.

---

## Legacy Config: auth_request Approach

```
k8s/nginx-config-updated.conf (NOT ACTIVE)
```

The legacy config used a different architecture:

```
1. Client hits /agent/voice/breeze-buddy/ on nginx
2. Nginx internally calls /_allocate (auth_request subrequest)
3. /_allocate proxies to smart-router /api/v1/allocate
4. Smart Router returns X-Allocated-Pod header
5. Nginx captures the header and proxies to that pod
```

**Why it was replaced:** The `auth_request` approach tied allocation to nginx's request lifecycle, making it harder to support different telephony providers with different request formats. The current approach lets each provider call the appropriate allocation endpoint directly and receive a WSURL.

---

## Edge Cases

1. **Pod dies between allocation and WebSocket connect:** The `@ws_fallback` catches the 502/504 and routes to any healthy pod. The call may fail gracefully on the fallback pod, but the connection isn't dropped at the nginx level.

2. **DNS resolution failure:** If the pod FQDN doesn't resolve (pod doesn't exist), nginx returns 502. The `@ws_fallback` is triggered for WebSocket locations.

3. **DNS cache staleness:** With `valid=10s`, a pod IP change takes up to 10 seconds to propagate. During this window, connections to the old IP may fail and trigger fallback.

4. **Long-running WebSocket connections:** The 1-hour timeout applies to idle connections. Active WebSocket connections with regular frame exchange (voice data) are never timed out.

5. **envsubst failure:** If `${KUBE_DNS_IP}` isn't set, the resolver directive will have an empty value and nginx will fail to start. This is caught by K8s readiness probe.

---

## Interaction with Other Flows

| Flow | Nginx's Role | Doc |
|------|-------------|-----|
| Allocation | Proxies `POST /api/v1/allocate` to Smart Router; returns WSURL containing `/ws/pod/{name}` | [07-allocation.md](07-allocation.md) |
| WebSocket Connection | Routes WSURL to specific pod via regex capture + K8s FQDN DNS | This doc |
| Release | Proxies `POST /api/v1/release` to Smart Router | [08-release.md](08-release.md) |
| Drain | Proxies `POST /api/v1/drain` to Smart Router | [09-drain.md](09-drain.md) |
| Health Probes | Own `/health` + `/ready` for nginx K8s probes; proxies `/api/v1/health` for Smart Router probes | [12-http-layer.md](12-http-layer.md) |
| Metrics | Proxies `GET /api/v1/metrics` to Smart Router for Prometheus scraping | [13-metrics.md](13-metrics.md) |
