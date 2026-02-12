# Nginx → Envoy/Istio Migration Guide

## Voice System — Pod-Specific WebSocket Routing

This document covers migrating the `nginx-router` in `voice-system` to Envoy, either as a standalone proxy or with the full Istio service mesh. It maps every current nginx feature to its Envoy/Istio equivalent, provides full resource YAMLs, and is honest about what's easy, what's hard, and what's worse.

---

## Table of Contents

1. [Prerequisites](#1-prerequisites)
2. [Concepts Mapping: Nginx → Envoy/Istio](#2-concepts-mapping)
3. [Architecture Diagram](#3-architecture-diagram)
4. [Kubernetes Resource YAMLs](#4-kubernetes-resource-yamls)
5. [The Hard Part: Pod-Specific WebSocket Routing](#5-pod-specific-websocket-routing)
6. [WebSocket Fallback Implementation](#6-websocket-fallback)
7. [Migration Plan](#7-migration-plan)
8. [Honest Pros and Cons](#8-pros-and-cons)
9. [Alternative: Standalone Envoy (No Istio)](#9-standalone-envoy)

---

## 1. Prerequisites

### What You Need

| Component | Version | Purpose |
|---|---|---|
| Istio | 1.22+ (latest stable) | Service mesh control plane |
| Envoy | Bundled with Istio (1.30+) | Data plane proxy |
| `istioctl` | Matches Istio version | CLI for install/debug |
| GKE Standard cluster | Already have | Autopilot has Istio limitations |

### Install Istio on GKE

```bash
# Download istioctl
curl -L https://istio.io/downloadIstio | ISTIO_VERSION=1.22.0 sh -
export PATH=$PWD/istio-1.22.0/bin:$PATH

# Install with "default" profile (includes istiod + ingress gateway)
istioctl install --set profile=default -y

# Verify
kubectl get pods -n istio-system
# Should see: istiod-xxx (Running), istio-ingressgateway-xxx (Running)
```

**GKE-specific notes:**
- **Standard cluster** (what we have): Full control, no restrictions. Install Istio yourself.
- **Autopilot cluster**: Use GKE's managed Istio (Anthos Service Mesh / ASM) instead. ASM is a Google-managed control plane — different install process, some feature restrictions.
- Our cluster is Standard, so we use upstream Istio directly.

### Enable Sidecar Injection

```bash
# Label the namespace for automatic sidecar injection
kubectl label namespace voice-system istio-injection=enabled

# After this, every new pod in voice-system gets an Envoy sidecar container.
# Existing pods need a restart to pick up the sidecar:
kubectl rollout restart statefulset/voice-agent -n voice-system
kubectl rollout restart deployment/smart-router -n voice-system
```

**What sidecar injection does:** Adds an `istio-proxy` container to every pod. All traffic to/from the pod goes through this Envoy sidecar. The sidecar is configured automatically by Istio's control plane (istiod) based on VirtualService/DestinationRule resources.

### RBAC

Istio creates its own ClusterRoles during install. No extra RBAC needed unless you're restricting who can create VirtualService/Gateway resources:

```bash
# Verify CRDs are installed
kubectl get crd | grep istio
# Should see: virtualservices.networking.istio.io, gateways.networking.istio.io,
# destinationrules.networking.istio.io, envoyfilters.networking.istio.io, etc.
```

---

## 2. Concepts Mapping

### Nginx → Envoy/Istio Feature Map

| Nginx Feature | Nginx Config | Envoy/Istio Equivalent | Notes |
|---|---|---|---|
| **Static upstream** | `upstream smart_router { server smart-router:8080; }` | Just use K8s Service directly in VirtualService `route.destination.host` | Envoy discovers endpoints via EDS (Endpoint Discovery Service) from Istio. No manual DNS config. |
| **Prefix location** | `location /api/v1/ { proxy_pass ...; }` | `VirtualService.http[].match[].uri.prefix: "/api/v1/"` | Same concept, declarative YAML instead of imperative config. |
| **Regex location** | `location ~ ^/ws/pod/(?<pod_name>[^/]+)(.*)$ { ... }` | `VirtualService.http[].match[].uri.regex` | Istio supports RE2 regex. BUT: you can't capture groups and use them in routing decisions. This is the hard part — see Section 5. |
| **Path rewrite** | `proxy_pass http://smart_router/api/v1/twilio/allocate;` (implicit rewrite) | `VirtualService.http[].rewrite.uri: "/api/v1/twilio/allocate"` | Explicit rewrite field. |
| **Variable-based proxy_pass** | `set $target "pod.svc:8000"; proxy_pass http://$target;` | Not directly possible in VirtualService. Need EnvoyFilter or static per-pod routes. | Nginx resolves DNS at request time for variables. Envoy uses EDS which is real-time but routes to services, not arbitrary DNS names computed at request time. |
| **DNS resolver** | `resolver 34.118.224.10 valid=10s;` | Envoy EDS — real-time endpoint discovery | Envoy doesn't poll DNS on a TTL. Istio pushes endpoint changes to Envoy immediately when pods come/go. Much better than 10s DNS TTL. |
| **WebSocket upgrade** | `proxy_set_header Upgrade $http_upgrade; proxy_set_header Connection "upgrade";` | Automatic. Envoy upgrades WebSocket connections natively. | Envoy handles `Upgrade: websocket` and `Connection: Upgrade` transparently. You just set `upgradeConfigs` in the route or it auto-detects. No explicit header forwarding needed. |
| **Timeouts** | `proxy_connect_timeout 3s; proxy_read_timeout 3600s;` | `VirtualService.http[].timeout: "3600s"` + `DestinationRule.trafficPolicy.connectionPool.tcp.connectTimeout: "3s"` | Split across two resources. |
| **Error page fallback** | `proxy_intercept_errors on; error_page 502 504 = @ws_fallback;` | Envoy retry policy with `retryOn: "connect-failure,refused-stream,unavailable"` | Can retry to a different endpoint, but can't retry to a completely different service (like our headless fallback). Need EnvoyFilter for that. See Section 6. |
| **proxy_buffering off** | `proxy_buffering off;` | Default for WebSocket/streaming in Envoy. No config needed. | Envoy streams by default for upgraded connections. |
| **proxy_http_version 1.1** | `proxy_http_version 1.1;` | Envoy uses HTTP/2 to upstream by default (within mesh), HTTP/1.1 for WebSocket upgrade automatically. | No config needed. Envoy auto-downgrades to HTTP/1.1 for WebSocket. |
| **Host header** | `proxy_set_header Host $host;` | Envoy forwards `Host` (or `:authority` in HTTP/2) by default. | No config needed. |
| **Stub status** | `location /nginx-status { stub_status; }` | Envoy admin interface at `:15000` on sidecar, or Prometheus metrics via `/stats/prometheus` | Much richer metrics out of the box. |
| **Non-root execution** | `runAsUser: 101` | Istio sidecar runs as uid 1337 by default. Configure via `securityContext` in sidecar injection template. | Different UID but same principle. |

### Key Insight

Most of the nginx config maps cleanly to VirtualService + DestinationRule. The ONE thing that doesn't map well is the **dynamic pod-specific routing** — extracting a pod name from the URL regex and routing to that specific pod's DNS name. This is trivial in nginx (variable-based proxy_pass) but has no direct equivalent in Istio VirtualService.

---

## 3. Architecture Diagram

### Current (Nginx)

```
                    ┌──────────────────────────────────────────┐
                    │          GKE Load Balancer (TLS)         │
                    └─────────────────┬────────────────────────┘
                                      │
                    ┌─────────────────▼────────────────────────┐
                    │     nginx-router (2 replicas)            │
                    │     ClusterIP: 34.118.225.185:80         │
                    │                                          │
                    │  /api/v1/* ──────► smart-router:8080     │
                    │  /agent/voice/* ──► smart-router:8080    │
                    │  /ws/pod/{name}/ ─► {name}.voice-agent   │
                    │  /{name}/* ───────► {name}.voice-agent   │
                    └──────────────────────────────────────────┘
                           │                      │
              ┌────────────▼──────┐    ┌──────────▼──────────────┐
              │  smart-router     │    │  voice-agent StatefulSet │
              │  (3 pods, Go)     │    │  (5 pods, Python)        │
              │  ClusterIP:8080   │    │  Headless svc:8000       │
              └───────────────────┘    └──────────────────────────┘
```

### With Istio (Option A: Istio Ingress Gateway)

```
                    ┌──────────────────────────────────────────┐
                    │          GKE Load Balancer (TLS)         │
                    └─────────────────┬────────────────────────┘
                                      │
                    ┌─────────────────▼────────────────────────┐
                    │   Istio Ingress Gateway (Envoy)          │
                    │   (replaces nginx-router entirely)       │
                    │                                          │
                    │   Routing defined by:                    │
                    │   - Gateway resource (ports, TLS, hosts) │
                    │   - VirtualService (route rules)         │
                    └──────────────────────────────────────────┘
                           │                      │
              ┌────────────▼──────┐    ┌──────────▼──────────────┐
              │  smart-router     │    │  voice-agent StatefulSet │
              │  + envoy sidecar  │    │  + envoy sidecar         │
              │  (3 pods)         │    │  (5 pods)                │
              └───────────────────┘    └──────────────────────────┘

    istiod (control plane) pushes config to all Envoy instances
```

### With Istio (Option B: Sidecar Only, Keep nginx)

```
                    ┌──────────────────────────────────────────┐
                    │   nginx-router (keep as-is)              │
                    │   + envoy sidecar (mTLS, observability)  │
                    └──────────────────────────────────────────┘
                           │                      │
              ┌────────────▼──────┐    ┌──────────▼──────────────┐
              │  smart-router     │    │  voice-agent StatefulSet │
              │  + envoy sidecar  │    │  + envoy sidecar         │
              └───────────────────┘    └──────────────────────────┘

    Benefit: mTLS between services, metrics, tracing
    Downside: Still running nginx, just adding sidecars
```

### Standalone Envoy (No Istio)

```
    Same as nginx diagram, but replace nginx-router with
    envoy-router Deployment running Envoy with a static
    config file (envoy.yaml). No istiod, no sidecars.
```

---

## 4. Kubernetes Resource YAMLs

### 4a. Gateway

The Gateway resource tells Istio's ingress gateway which ports/hosts to listen on. This replaces the nginx `server { listen 8080; }` block.

```yaml
apiVersion: networking.istio.io/v1beta1
kind: Gateway
metadata:
  name: voice-system-gateway
  namespace: voice-system
spec:
  # Use Istio's default ingress gateway (deployed in istio-system)
  selector:
    istio: ingressgateway
  servers:
    - port:
        number: 80
        name: http
        protocol: HTTP
      hosts:
        - "clairvoyance.breezelabs.app"
        - "buddy.breezelabs.app"
    # If TLS is terminated at GKE LB (current setup), keep HTTP here.
    # If you want Istio to terminate TLS:
    # - port:
    #     number: 443
    #     name: https
    #     protocol: HTTPS
    #   tls:
    #     mode: SIMPLE
    #     credentialName: voice-system-tls  # K8s Secret with cert
    #   hosts:
    #     - "clairvoyance.breezelabs.app"
    #     - "buddy.breezelabs.app"
```

### 4b. VirtualService

This is the big one — all routing rules. Replaces every `location` block in nginx.

```yaml
apiVersion: networking.istio.io/v1beta1
kind: VirtualService
metadata:
  name: voice-system-routes
  namespace: voice-system
spec:
  hosts:
    - "clairvoyance.breezelabs.app"
    - "buddy.breezelabs.app"
  gateways:
    - voice-system-gateway     # External traffic via Gateway
    - mesh                     # Internal traffic within the mesh
  http:
    # ---------------------------------------------------------------
    # Health / Readiness
    # Istio Gateway itself exposes health on :15021/healthz/ready
    # But if we want /health and /ready to return 200 directly,
    # we need a "direct response" — which VirtualService does NOT support.
    #
    # Options:
    # a) Route to a simple health service (like nginx did)
    # b) Use EnvoyFilter for direct_response
    # c) Let the GKE LB health check hit the gateway's built-in health port
    #
    # Simplest: route /health to smart-router (it has a /health endpoint)
    # ---------------------------------------------------------------
    - match:
        - uri:
            exact: "/health"
      route:
        - destination:
            host: smart-router   # smart-router has /health
            port:
              number: 8080

    - match:
        - uri:
            exact: "/ready"
      route:
        - destination:
            host: smart-router
            port:
              number: 8080

    # ---------------------------------------------------------------
    # Smart Router API pass-through
    # Equivalent of: location /api/v1/ { proxy_pass http://smart_router/api/v1/; }
    # ---------------------------------------------------------------
    - match:
        - uri:
            prefix: "/api/v1/"
      route:
        - destination:
            host: smart-router
            port:
              number: 8080
      timeout: 30s

    # ---------------------------------------------------------------
    # Provider webhooks → Smart Router (with path rewrite)
    #
    # In nginx these were:
    #   /agent/voice/breeze-buddy/twilio/callback/ → /api/v1/twilio/allocate
    #
    # In VirtualService, we match the prefix and rewrite the URI.
    # ---------------------------------------------------------------
    - match:
        - uri:
            prefix: "/agent/voice/breeze-buddy/twilio/callback/"
      rewrite:
        uri: "/api/v1/twilio/allocate"
      route:
        - destination:
            host: smart-router
            port:
              number: 8080
      timeout: 30s

    - match:
        - uri:
            prefix: "/agent/voice/breeze-buddy/plivo/callback/"
      rewrite:
        uri: "/api/v1/plivo/allocate"
      route:
        - destination:
            host: smart-router
            port:
              number: 8080
      timeout: 30s

    - match:
        - uri:
            prefix: "/agent/voice/breeze-buddy/exotel/callback/"
      rewrite:
        uri: "/api/v1/exotel/allocate"
      route:
        - destination:
            host: smart-router
            port:
              number: 8080
      timeout: 30s

    - match:
        - uri:
            prefix: "/agent/voice/breeze-buddy/allocate"
      rewrite:
        uri: "/api/v1/allocate"
      route:
        - destination:
            host: smart-router
            port:
              number: 8080
      timeout: 30s

    # ---------------------------------------------------------------
    # WebSocket: Pod-specific routing
    #
    # THIS IS THE HARD PART. See Section 5 for detailed discussion.
    #
    # VirtualService CANNOT do: "extract pod name from URL regex,
    # route to that specific pod". It can only route to a Service.
    #
    # WORKAROUND: Static routes per pod. Ugly but works.
    # For 5 pods, we need 5 route entries.
    # If you scale to N pods, you need N entries (or use EnvoyFilter).
    # ---------------------------------------------------------------
    - match:
        - uri:
            prefix: "/ws/pod/voice-agent-0/"
      rewrite:
        uri: "/"   # Strip /ws/pod/voice-agent-0, keep the rest
      route:
        - destination:
            host: voice-agent-0.voice-agent.voice-system.svc.cluster.local
            port:
              number: 8000
      timeout: 3600s  # 1 hour for voice calls

    - match:
        - uri:
            prefix: "/ws/pod/voice-agent-1/"
      rewrite:
        uri: "/"
      route:
        - destination:
            host: voice-agent-1.voice-agent.voice-system.svc.cluster.local
            port:
              number: 8000
      timeout: 3600s

    - match:
        - uri:
            prefix: "/ws/pod/voice-agent-2/"
      rewrite:
        uri: "/"
      route:
        - destination:
            host: voice-agent-2.voice-agent.voice-system.svc.cluster.local
            port:
              number: 8000
      timeout: 3600s

    - match:
        - uri:
            prefix: "/ws/pod/voice-agent-3/"
      rewrite:
        uri: "/"
      route:
        - destination:
            host: voice-agent-3.voice-agent.voice-system.svc.cluster.local
            port:
              number: 8000
      timeout: 3600s

    - match:
        - uri:
            prefix: "/ws/pod/voice-agent-4/"
      rewrite:
        uri: "/"
      route:
        - destination:
            host: voice-agent-4.voice-agent.voice-system.svc.cluster.local
            port:
              number: 8000
      timeout: 3600s

    # ---------------------------------------------------------------
    # Direct pod access (same problem — static per-pod routes)
    # ---------------------------------------------------------------
    - match:
        - uri:
            prefix: "/voice-agent-0/"
      rewrite:
        uri: "/"
      route:
        - destination:
            host: voice-agent-0.voice-agent.voice-system.svc.cluster.local
            port:
              number: 8000
      timeout: 30s

    - match:
        - uri:
            prefix: "/voice-agent-1/"
      rewrite:
        uri: "/"
      route:
        - destination:
            host: voice-agent-1.voice-agent.voice-system.svc.cluster.local
            port:
              number: 8000
      timeout: 30s

    - match:
        - uri:
            prefix: "/voice-agent-2/"
      rewrite:
        uri: "/"
      route:
        - destination:
            host: voice-agent-2.voice-agent.voice-system.svc.cluster.local
            port:
              number: 8000
      timeout: 30s

    - match:
        - uri:
            prefix: "/voice-agent-3/"
      rewrite:
        uri: "/"
      route:
        - destination:
            host: voice-agent-3.voice-agent.voice-system.svc.cluster.local
            port:
              number: 8000
      timeout: 30s

    - match:
        - uri:
            prefix: "/voice-agent-4/"
      rewrite:
        uri: "/"
      route:
        - destination:
            host: voice-agent-4.voice-agent.voice-system.svc.cluster.local
            port:
              number: 8000
      timeout: 30s
```

**IMPORTANT: The `rewrite.uri: "/"` with prefix match has a problem.** Istio rewrites the entire matched prefix to `/`, so `/ws/pod/voice-agent-0/twilio/callback/order-confirmation/v2` becomes `/twilio/callback/order-confirmation/v2`. This actually works correctly because Istio preserves the path suffix after the matched prefix. But verify this behavior with your Istio version.

### 4c. ServiceEntry (for pod-specific DNS)

Istio needs to know about the individual pod DNS names since they're not regular K8s Services:

```yaml
# One ServiceEntry per pod, OR one with all pods listed.
# These tell Istio "these DNS names are valid endpoints you can route to".
apiVersion: networking.istio.io/v1beta1
kind: ServiceEntry
metadata:
  name: voice-agent-pods
  namespace: voice-system
spec:
  hosts:
    - voice-agent-0.voice-agent.voice-system.svc.cluster.local
    - voice-agent-1.voice-agent.voice-system.svc.cluster.local
    - voice-agent-2.voice-agent.voice-system.svc.cluster.local
    - voice-agent-3.voice-agent.voice-system.svc.cluster.local
    - voice-agent-4.voice-agent.voice-system.svc.cluster.local
  location: MESH_INTERNAL
  ports:
    - number: 8000
      name: http
      protocol: HTTP
  resolution: DNS
  # resolution: DNS means Envoy resolves these via kube-dns
  # For headless services with StatefulSet pods, this works because
  # each pod has a stable DNS name: {pod}.{service}.{ns}.svc.cluster.local
```

### 4d. DestinationRule

```yaml
apiVersion: networking.istio.io/v1beta1
kind: DestinationRule
metadata:
  name: smart-router
  namespace: voice-system
spec:
  host: smart-router
  trafficPolicy:
    connectionPool:
      tcp:
        connectTimeout: 5s
      http:
        h2UpgradePolicy: DO_NOT_UPGRADE  # Keep HTTP/1.1 for smart-router
---
apiVersion: networking.istio.io/v1beta1
kind: DestinationRule
metadata:
  name: voice-agent
  namespace: voice-system
spec:
  host: voice-agent  # Applies to the headless service
  trafficPolicy:
    connectionPool:
      tcp:
        connectTimeout: 3s
      http:
        h2UpgradePolicy: DO_NOT_UPGRADE  # WebSocket needs HTTP/1.1
    # Outlier detection: eject unhealthy pods
    outlierDetection:
      consecutive5xxErrors: 2
      interval: 10s
      baseEjectionTime: 30s
      maxEjectionPercent: 50
```

---

## 5. The Hard Part: Pod-Specific WebSocket Routing

### The Problem

In nginx:
```nginx
location ~ ^/ws/pod/(?<pod_name>[^/]+)(?<rest_path>/.*)$ {
    set $target_pod "$pod_name.voice-agent.voice-system.svc.cluster.local:8000";
    proxy_pass http://$target_pod$rest_path;
}
```

This is 3 lines. It handles any number of pods. When you scale from 5 to 50, nothing changes.

In Istio VirtualService, **there is no equivalent**. Here's why:

1. **VirtualService regex match captures are NOT usable in routing.** You can match `uri.regex: "^/ws/pod/([^/]+)(/.*)$"` but you cannot use the captured group `$1` in the `destination.host` field. The destination must be a static string.

2. **VirtualService routes to Services, not arbitrary DNS names.** The `destination.host` must be a known service (K8s Service or ServiceEntry host). You can't compute it dynamically from the request.

3. **No variable interpolation.** There's no `set $var` equivalent. Routing decisions are purely declarative pattern matching.

### Possible Approaches

#### Approach A: Static Per-Pod Routes (shown in Section 4b)

```yaml
- match:
    - uri:
        prefix: "/ws/pod/voice-agent-0/"
  route:
    - destination:
        host: voice-agent-0.voice-agent.voice-system.svc.cluster.local
```

**Pros:**
- Simple, no custom code
- Easy to understand

**Cons:**
- Must update VirtualService YAML every time you scale
- 5 pods = 5 routes. 50 pods = 50 routes. 200 pods = 200 routes.
- Manual process, error-prone at scale

**Verdict:** Fine for our 5 pods. Bad if you plan to scale significantly.

#### Approach B: EnvoyFilter with Lua

EnvoyFilter lets you inject raw Envoy configuration, including Lua scripts that run per-request.

```yaml
apiVersion: networking.istio.io/v1alpha3
kind: EnvoyFilter
metadata:
  name: ws-pod-routing
  namespace: voice-system
spec:
  workloadSelector:
    labels:
      istio: ingressgateway  # Apply to the ingress gateway only
  configPatches:
    - applyTo: HTTP_FILTER
      match:
        context: GATEWAY
        listener:
          filterChain:
            filter:
              name: "envoy.filters.network.http_connection_manager"
              subFilter:
                name: "envoy.filters.http.router"
      patch:
        operation: INSERT_BEFORE
        value:
          name: envoy.filters.http.lua
          typed_config:
            "@type": type.googleapis.com/envoy.extensions.filters.http.lua.v3.Lua
            inline_code: |
              function envoy_on_request(request_handle)
                local path = request_handle:headers():get(":path")
                -- Match /ws/pod/{pod_name}/{rest}
                local pod_name, rest = string.match(path, "^/ws/pod/([^/]+)(/.*)$")
                if pod_name then
                  -- Rewrite path: strip /ws/pod/{pod_name}
                  request_handle:headers():replace(":path", rest)
                  -- Set the target cluster/host header
                  local target = pod_name .. ".voice-agent.voice-system.svc.cluster.local"
                  request_handle:headers():replace(":authority", target)
                  -- Route to the dynamic forward proxy cluster
                  request_handle:headers():add("x-envoy-dynamic-target", target .. ":8000")
                end
              end
    # Also need a dynamic_forward_proxy cluster configured
    # This is getting complex...
```

**Pros:**
- Dynamic — handles any number of pods without config changes
- Same flexibility as nginx variable-based routing

**Cons:**
- Lua in EnvoyFilter is fragile and hard to debug
- EnvoyFilter is considered "escape hatch" — Istio team discourages it
- EnvoyFilter YAML is tightly coupled to Envoy's internal config structure
- Envoy version upgrades can break EnvoyFilters
- Needs additional cluster configuration (dynamic_forward_proxy or per-pod static clusters)
- Much harder to review/maintain than the nginx config

**Verdict:** Possible but significantly more complex than nginx. The Lua approach also requires configuring Envoy's dynamic forward proxy cluster, which adds more EnvoyFilter patches.

#### Approach C: EnvoyFilter with WASM

Similar to Lua but using a compiled WebAssembly module.

**Pros:**
- Better performance than Lua
- Can be written in Go/Rust
- More maintainable than inline Lua strings

**Cons:**
- Even more complex than Lua (requires building/deploying WASM binaries)
- Debugging is harder
- Overkill for simple URL parsing + routing

**Verdict:** Over-engineered for this use case.

#### Approach D: Envoy Dynamic Forward Proxy

Envoy has a `dynamic_forward_proxy` cluster type that resolves DNS at request time based on the `Host` header or a specific header value.

```yaml
# Would need EnvoyFilter to:
# 1. Parse pod name from URL
# 2. Set Host header to pod FQDN
# 3. Route to a dynamic_forward_proxy cluster
```

**Pros:**
- True dynamic routing like nginx variables
- No per-pod static routes

**Cons:**
- Still requires EnvoyFilter to set up the cluster and parse the URL
- Complex Envoy config that's hard to maintain via EnvoyFilter patches
- Not well-supported in Istio (Istio prefers EDS-based routing)

**Verdict:** The most "correct" Envoy approach, but requires deep Envoy knowledge and is fragile in an Istio context.

#### Approach E: Move Pod Selection to Smart Router Response

Instead of encoding the pod name in the URL and having the proxy route to it, have Smart Router return the pod's direct IP in a header, and have the proxy use that.

This requires rethinking the architecture — not a drop-in replacement.

**Verdict:** Out of scope for a migration doc, but worth considering for a v2 architecture.

### Recommendation

**For 5 pods: Use Approach A (static per-pod routes).** It's simple, readable, and works. The VirtualService is verbose but straightforward.

**If scaling beyond ~20 pods: Stay with nginx.** The dynamic pod routing is a 3-line nginx config that "just works". Replicating it in Istio requires fragile EnvoyFilters or WASM. The complexity isn't worth it unless you need other Istio features (mTLS, tracing, traffic splitting).

---

## 6. WebSocket Fallback

### Current Nginx Behavior

```nginx
# In WebSocket location:
proxy_intercept_errors on;
error_page 502 504 = @ws_fallback;

# Fallback named location:
location @ws_fallback {
    set $fallback_target "voice-agent.voice-system.svc.cluster.local:8000";
    proxy_pass http://$fallback_target$rest_path;
}
```

When the target pod is unreachable (502 Bad Gateway or 504 Gateway Timeout), nginx internally redirects to `@ws_fallback` which sends the request to the headless service (round-robin any healthy pod).

### Envoy/Istio Equivalent

#### Option 1: Retry Policy (partial)

```yaml
# In VirtualService route:
- match:
    - uri:
        prefix: "/ws/pod/voice-agent-0/"
  route:
    - destination:
        host: voice-agent-0.voice-agent.voice-system.svc.cluster.local
        port:
          number: 8000
  retries:
    attempts: 1
    retryOn: "connect-failure,refused-stream,unavailable,503"
    perTryTimeout: 5s
  timeout: 3600s
```

**Problem:** Envoy retries to the SAME destination. It doesn't retry to a different service. So if voice-agent-0 is down, it retries voice-agent-0 again — which fails again.

#### Option 2: Weighted Routing with Failover (not quite right)

```yaml
  route:
    - destination:
        host: voice-agent-0.voice-agent.voice-system.svc.cluster.local
        port:
          number: 8000
      weight: 100
    - destination:
        host: voice-agent    # Headless service, all pods
        port:
          number: 8000
      weight: 0
```

**Problem:** `weight: 0` means this destination is never used. Envoy doesn't fall back to weight-0 destinations on failure.

#### Option 3: DestinationRule Outlier Detection

```yaml
apiVersion: networking.istio.io/v1beta1
kind: DestinationRule
metadata:
  name: voice-agent-pods-outlier
  namespace: voice-system
spec:
  host: voice-agent
  trafficPolicy:
    outlierDetection:
      consecutive5xxErrors: 1
      interval: 5s
      baseEjectionTime: 30s
```

This ejects unhealthy pods from the load balancing pool, but it works at the SERVICE level. Since each pod-specific route targets a single-endpoint "service" (the individual pod DNS name), outlier detection doesn't help — there's only one endpoint to eject.

#### Option 4: EnvoyFilter with Custom Retry Logic

You could write a Lua filter that catches the 502/504 from the primary route and re-issues the request to the headless service. This is the only way to replicate the nginx `@ws_fallback` behavior exactly.

```yaml
# Pseudocode for the Lua filter:
# 1. Forward request to target pod
# 2. If response is 502/504:
#    a. Change :authority header to voice-agent.voice-system.svc.cluster.local
#    b. Re-issue the request
```

**Problem:** Lua filters in Envoy run in the request path. Re-issuing a request from within a Lua filter is not straightforward — you'd need to use `httpCall()` which doesn't support WebSocket upgrade.

### Honest Assessment

**The nginx fallback pattern (`error_page = @named_location`) has no clean Istio equivalent.** The closest you can get is:

1. Use outlier detection on the headless service to eject dead pods
2. Route ALL WebSocket traffic to the headless service (not pod-specific) and let Envoy/K8s pick a healthy pod
3. Accept that pod-specific routing + fallback-to-any-pod is an nginx-specific pattern

**If you use static per-pod routes (Approach A), you lose the fallback entirely** unless you add complex EnvoyFilter logic.

---

## 7. Migration Plan

### Phase 1: Install Istio (Non-disruptive)

```bash
# Install Istio control plane — does not affect existing workloads
istioctl install --set profile=default -y

# Do NOT label the namespace yet (no sidecar injection)
# Verify istiod is healthy
kubectl get pods -n istio-system
```

### Phase 2: Deploy Istio Resources (No Traffic Yet)

```bash
# Apply Gateway, VirtualService, DestinationRule, ServiceEntry
# These have no effect until traffic enters through the Istio gateway
kubectl apply -f gateway.yaml
kubectl apply -f virtualservice.yaml
kubectl apply -f destinationrule.yaml
kubectl apply -f serviceentry.yaml
```

### Phase 3: Parallel Testing

```bash
# Istio's ingress gateway gets its own LoadBalancer IP
kubectl get svc -n istio-system istio-ingressgateway -o jsonpath='{.status.loadBalancer.ingress[0].ip}'
# → e.g., 34.100.x.x

# Test through Istio gateway (using IP directly or a test DNS entry):
curl -H "Host: clairvoyance.breezelabs.app" http://34.100.x.x/health
curl -H "Host: clairvoyance.breezelabs.app" http://34.100.x.x/api/v1/status
curl -H "Host: clairvoyance.breezelabs.app" http://34.100.x.x/voice-agent-0/health

# Test WebSocket:
wscat -c "ws://34.100.x.x/ws/pod/voice-agent-0/twilio/callback/test/v2" \
  -H "Host: buddy.breezelabs.app"
```

Both nginx and Istio gateway serve traffic simultaneously. Production DNS still points to nginx.

### Phase 4: Switch DNS

```bash
# Point clairvoyance.breezelabs.app and buddy.breezelabs.app
# to the Istio ingress gateway IP
# Wait for DNS propagation
# Monitor for errors
```

### Phase 5: Enable Sidecar Injection (Optional)

```bash
# If you want full mesh (mTLS, tracing):
kubectl label namespace voice-system istio-injection=enabled
kubectl rollout restart statefulset/voice-agent -n voice-system
kubectl rollout restart deployment/smart-router -n voice-system
```

### Phase 6: Decommission Nginx

```bash
kubectl delete deployment nginx-router -n voice-system
kubectl delete service nginx-router -n voice-system
kubectl delete configmap nginx-router-config -n voice-system
```

### Rollback

At any point before Phase 6:
- Switch DNS back to the nginx-router service IP
- Traffic flows through nginx again
- Istio resources can stay deployed (they don't interfere)

---

## 8. Honest Pros and Cons

### What Envoy/Istio Does Better

| Feature | Nginx | Envoy/Istio |
|---|---|---|
| **Endpoint discovery** | DNS polling with TTL (10s stale) | Real-time via EDS — instant pod detection |
| **WebSocket handling** | Requires explicit `Upgrade` + `Connection` header forwarding | Automatic, transparent |
| **Observability** | Access logs only, manual Prometheus exporter | Built-in metrics (request count, latency, errors per service), distributed tracing (Jaeger/Zipkin), Kiali dashboard |
| **mTLS** | Not supported (plain HTTP between services) | Automatic mTLS between all services, zero config |
| **Traffic splitting** | Not practical | Trivial (canary deployments, A/B testing via weights) |
| **Circuit breaking** | None | Built-in outlier detection + connection pool limits |
| **Health checking** | None (relies on DNS removing dead pods) | Active health checking + passive outlier detection |
| **Header-based routing** | Possible but verbose | First-class support in VirtualService |

### What's Harder/Worse with Envoy/Istio

| Feature | Nginx | Envoy/Istio |
|---|---|---|
| **Dynamic pod routing** | 3 lines (regex + variable + proxy_pass) | Static per-pod routes or fragile EnvoyFilter. The single biggest pain point. |
| **Fallback on error** | `error_page = @named_location` (simple) | No clean equivalent. Need complex EnvoyFilter or accept no fallback. |
| **Direct response** | `return 200 'healthy\n'` | VirtualService can't return direct responses. Need EnvoyFilter or a backend service. |
| **Config complexity** | 1 file, ~200 lines, readable | Gateway + VirtualService + DestinationRule + ServiceEntry + maybe EnvoyFilter = 5 resources, 300+ lines of YAML |
| **Debugging** | `nginx -t` validates config, `error_log` is clear | `istioctl analyze`, `istioctl proxy-config`, Envoy debug logs are verbose and hard to read |
| **Resource overhead** | 2 pods, ~128MB each = 256MB total | istiod (500MB+), ingress gateway (128MB), sidecar per pod (~50MB each × 8 pods = 400MB) = ~1GB+ total |apiVersion: networking.istio.io/v1alpha3
kind: VirtualService
metadata:
  name: voice-agent-routing
spec:
  hosts:
    - voice-agent
  http:
    - match:
        - uri:
            regex: "^/voice-agent-[0-9]+/.*"   # Match any path for voice-agent-<number>
      route:
        - destination:
            host: voice-agent
            subset: voice-agent-${1}  # This allows dynamic routing to the correct pod

| **Operational knowledge** | Nginx is well-understood by most infra teams | Istio has a steep learning curve, debugging mesh issues is notoriously difficult |
| **Startup time** | nginx starts in <1s | Sidecar injection adds 2-5s to pod startup. istiod takes 30s+. |
| **Path rewrite** | Implicit (proxy_pass replaces matched prefix) | Explicit `rewrite.uri` field — sometimes surprising behavior with suffix preservation |

### Resource Overhead Comparison

| Component | Nginx Setup | Istio Setup |
|---|---|---|
| nginx-router | 2 × 128MB = 256MB | 0 (removed) |
| istiod | 0 | 1 × 512MB = 512MB |
| Istio ingress gateway | 0 | 2 × 128MB = 256MB |
| Sidecars (8 pods) | 0 | 8 × 50MB = 400MB |
| **Total** | **~256MB** | **~1.2GB** |

### Bottom Line

For THIS specific use case (pod-specific WebSocket routing with fallback), **nginx is objectively the better tool**. The dynamic variable-based routing is nginx's strength, and Istio has no clean equivalent.

Istio makes sense if you need: mTLS between services, distributed tracing, canary deployments, circuit breaking, or Kiali-style service mesh observability. If you don't need those, Istio adds significant complexity for zero benefit over the current 200-line nginx config.

---

## 9. Alternative: Standalone Envoy (No Istio)

If you want Envoy's features without Istio's complexity, you can run Envoy as a standalone proxy — a direct replacement for nginx.

### How It Works

- Deploy an `envoy-router` Deployment (same as nginx-router)
- Mount an `envoy.yaml` config file via ConfigMap
- No istiod, no sidecars, no CRDs
- You manage the Envoy config directly (like you manage nginx.conf today)

### Equivalent Envoy Config

```yaml
# envoy.yaml — standalone Envoy config equivalent to nginx-config-simple.conf
admin:
  address:
    socket_address:
      address: 127.0.0.1
      port_value: 9901

static_resources:
  listeners:
    - name: main
      address:
        socket_address:
          address: 0.0.0.0
          port_value: 8080
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: ingress
                codec_type: AUTO
                # WebSocket support is automatic with upgrade_configs
                upgrade_configs:
                  - upgrade_type: websocket
                route_config:
                  name: local_route
                  virtual_hosts:
                    - name: voice_system
                      domains: ["*"]
                      routes:
                        # Health
                        - match:
                            path: "/health"
                          direct_response:
                            status: 200
                            body:
                              inline_string: "healthy\n"

                        - match:
                            path: "/ready"
                          direct_response:
                            status: 200
                            body:
                              inline_string: "ready\n"

                        # Smart Router API
                        - match:
                            prefix: "/api/v1/"
                          route:
                            cluster: smart_router
                            timeout: 30s

                        # Twilio webhook → Smart Router
                        - match:
                            prefix: "/agent/voice/breeze-buddy/twilio/callback/"
                          route:
                            cluster: smart_router
                            prefix_rewrite: "/api/v1/twilio/allocate"
                            timeout: 30s

                        # Plivo webhook → Smart Router
                        - match:
                            prefix: "/agent/voice/breeze-buddy/plivo/callback/"
                          route:
                            cluster: smart_router
                            prefix_rewrite: "/api/v1/plivo/allocate"
                            timeout: 30s

                        # Exotel webhook → Smart Router
                        - match:
                            prefix: "/agent/voice/breeze-buddy/exotel/callback/"
                          route:
                            cluster: smart_router
                            prefix_rewrite: "/api/v1/exotel/allocate"
                            timeout: 30s

                        # Generic allocate → Smart Router
                        - match:
                            prefix: "/agent/voice/breeze-buddy/allocate"
                          route:
                            cluster: smart_router
                            prefix_rewrite: "/api/v1/allocate"
                            timeout: 30s

                        # WebSocket per-pod routes (same static problem as Istio)
                        - match:
                            prefix: "/ws/pod/voice-agent-0/"
                          route:
                            cluster: voice_agent_0
                            prefix_rewrite: "/"
                            timeout: 3600s
                        - match:
                            prefix: "/ws/pod/voice-agent-1/"
                          route:
                            cluster: voice_agent_1
                            prefix_rewrite: "/"
                            timeout: 3600s
                        - match:
                            prefix: "/ws/pod/voice-agent-2/"
                          route:
                            cluster: voice_agent_2
                            prefix_rewrite: "/"
                            timeout: 3600s
                        - match:
                            prefix: "/ws/pod/voice-agent-3/"
                          route:
                            cluster: voice_agent_3
                            prefix_rewrite: "/"
                            timeout: 3600s
                        - match:
                            prefix: "/ws/pod/voice-agent-4/"
                          route:
                            cluster: voice_agent_4
                            prefix_rewrite: "/"
                            timeout: 3600s

                        # Direct pod access (same static pattern)
                        - match:
                            prefix: "/voice-agent-0/"
                          route:
                            cluster: voice_agent_0
                            prefix_rewrite: "/"
                            timeout: 30s
                        - match:
                            prefix: "/voice-agent-1/"
                          route:
                            cluster: voice_agent_1
                            prefix_rewrite: "/"
                            timeout: 30s
                        - match:
                            prefix: "/voice-agent-2/"
                          route:
                            cluster: voice_agent_2
                            prefix_rewrite: "/"
                            timeout: 30s
                        - match:
                            prefix: "/voice-agent-3/"
                          route:
                            cluster: voice_agent_3
                            prefix_rewrite: "/"
                            timeout: 30s
                        - match:
                            prefix: "/voice-agent-4/"
                          route:
                            cluster: voice_agent_4
                            prefix_rewrite: "/"
                            timeout: 30s

                http_filters:
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router

  clusters:
    - name: smart_router
      type: STRICT_DNS
      connect_timeout: 5s
      dns_lookup_family: V4_ONLY
      load_assignment:
        cluster_name: smart_router
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address:
                      address: smart-router.voice-system.svc.cluster.local
                      port_value: 8080

    # One cluster per voice-agent pod
    - name: voice_agent_0
      type: STRICT_DNS
      connect_timeout: 3s
      dns_lookup_family: V4_ONLY
      load_assignment:
        cluster_name: voice_agent_0
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address:
                      address: voice-agent-0.voice-agent.voice-system.svc.cluster.local
                      port_value: 8000
    - name: voice_agent_1
      type: STRICT_DNS
      connect_timeout: 3s
      dns_lookup_family: V4_ONLY
      load_assignment:
        cluster_name: voice_agent_1
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address:
                      address: voice-agent-1.voice-agent.voice-system.svc.cluster.local
                      port_value: 8000
    - name: voice_agent_2
      type: STRICT_DNS
      connect_timeout: 3s
      dns_lookup_family: V4_ONLY
      load_assignment:
        cluster_name: voice_agent_2
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address:
                      address: voice-agent-2.voice-agent.voice-system.svc.cluster.local
                      port_value: 8000
    - name: voice_agent_3
      type: STRICT_DNS
      connect_timeout: 3s
      dns_lookup_family: V4_ONLY
      load_assignment:
        cluster_name: voice_agent_3
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address:
                      address: voice-agent-3.voice-agent.voice-system.svc.cluster.local
                      port_value: 8000
    - name: voice_agent_4
      type: STRICT_DNS
      connect_timeout: 3s
      dns_lookup_family: V4_ONLY
      load_assignment:
        cluster_name: voice_agent_4
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address:
                      address: voice-agent-4.voice-agent.voice-system.svc.cluster.local
                      port_value: 8000

    # Fallback cluster — all pods via headless service
    - name: voice_agent_fallback
      type: STRICT_DNS
      connect_timeout: 5s
      dns_lookup_family: V4_ONLY
      dns_refresh_rate: 10s
      load_assignment:
        cluster_name: voice_agent_fallback
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address:
                      address: voice-agent.voice-system.svc.cluster.local
                      port_value: 8000
```

**Note:** Standalone Envoy has the same static-per-pod-route limitation as Istio VirtualService. You could use Envoy's `dynamic_forward_proxy` to get dynamic routing, but the config is significantly more complex.

### Standalone Envoy: Pros and Cons

| Aspect | Standalone Envoy | Nginx | Istio |
|---|---|---|---|
| Dynamic pod routing | Static per-pod (or complex dynamic_forward_proxy) | Trivial (3 lines) | Static per-pod (or EnvoyFilter) |
| Direct response | Native (`direct_response`) | Native (`return 200`) | Not supported |
| WebSocket | Automatic | Requires explicit headers | Automatic |
| Config format | YAML (~200 lines for static, more verbose) | nginx.conf (~200 lines, concise) | Multiple K8s resources |
| Observability | Built-in stats + Prometheus | Minimal (stub_status) | Full mesh observability |
| Hot reload | Via xDS API or SIGHUP | `nginx -s reload` | Automatic via istiod |
| Resource usage | ~same as nginx | ~same | 5x more |
| Learning curve | Moderate (Envoy config is verbose) | Low (everyone knows nginx) | High |

### Verdict

Standalone Envoy is a reasonable alternative IF you want Envoy-native features (better stats, HTTP/2, gRPC support, circuit breaking) without the Istio overhead. But for this specific use case, **it's more config for the same result**. The nginx config is more concise and the dynamic pod routing works out of the box.

---

## Final Recommendation

**Stay with nginx** for now. Here's the decision matrix:

| If you need... | Use... |
|---|---|
| Just routing (current use case) | **Nginx** — simplest, most concise, dynamic pod routing works natively |
| mTLS + observability + tracing | **Istio** — but accept the static pod routing limitation and complexity |
| Better stats without mesh overhead | **Standalone Envoy** — moderate complexity, better observability than nginx |
| Scaling to 50+ pods | **Nginx** — only option where pod routing stays at 3 lines regardless of scale |

The 200-line nginx config handles everything cleanly. Migrating to Envoy/Istio adds complexity (especially for pod-specific routing) without solving any current problem. Revisit this decision when you need mTLS, distributed tracing, or canary deployments.
