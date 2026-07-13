# Kubernetes Gateway API + Cilium — Deep Dive

> **Your lab:** k3s on 192.168.1.25, Cilium 1.19.5. Gateway API is not yet installed — this doc covers concepts, internals, and how to deploy it when ready.

---

## Why Gateway API Exists — The Problem With Ingress

The original `Ingress` resource was designed in 2015 for a simple use case: HTTP/HTTPS routing to services. It aged poorly:

- **Too limited** — no TCP/UDP routing, no traffic splitting, no header manipulation in spec
- **Vendor extensions via annotations** — every ingress controller (nginx, traefik, haproxy) invented their own `nginx.ingress.kubernetes.io/...` annotations. Non-portable, undiscoverable, untypeable
- **Single resource owns everything** — no separation between "cluster operator sets up the listener" and "dev team defines routes"
- **No status feedback** — Ingress gives you no structured way to know if your config was accepted or why it failed

Gateway API was designed from scratch to fix all of this. It graduated to GA in Kubernetes 1.28.

---

## The Resource Hierarchy

```
GatewayClass          "Cilium is the implementation"
     │                 (created by Cilium automatically)
     │
     ▼
Gateway               "Listen on port 80/443, accept routes from namespace X"
     │                 (created by platform/ops team)
     │
     ├──► HTTPRoute   "Route /api → svc-a, /web → svc-b"
     ├──► TCPRoute    "Forward port 5432 → postgres-svc"
     ├──► TLSRoute    "Passthrough TLS to backend"
     └──► GRPCRoute   "Route gRPC service methods"
                       (created by application/dev teams)
```

Each layer has a clear owner — this is intentional and important.

---

## The Roles Model — Who Owns What

This is one of Gateway API's most important design decisions. Three distinct personas:

```
┌─────────────────────────────────────────────────────────────┐
│  Infrastructure Provider (Cilium / cloud vendor)            │
│  → Implements GatewayClass                                  │
│  → "We support this API, here's our implementation"         │
│  → You don't create this — Cilium creates it automatically  │
└─────────────────────────────────────────────────────────────┘
                          │
                          ▼
┌─────────────────────────────────────────────────────────────┐
│  Cluster Operator (Platform Team — MacDonalds PaaS team)    │
│  → Creates Gateway resources                                │
│  → Defines which namespaces can attach routes               │
│  → Controls listeners (ports, protocols, TLS certs)         │
│  → "Here's a gateway teams can attach their routes to"      │
└─────────────────────────────────────────────────────────────┘
                          │
                          ▼
┌─────────────────────────────────────────────────────────────┐
│  Application Developer (Dev team)                           │
│  → Creates HTTPRoute / TCPRoute / etc.                      │
│  → Attaches routes to the platform-owned Gateway            │
│  → "Route my service's traffic through the gateway"         │
│  → Cannot modify the Gateway itself                         │
└─────────────────────────────────────────────────────────────┘
```

At MacDonalds, the platform team would own the Gateways (defined via CUE + GitOps), and dev teams would open MRs to add their HTTPRoutes. Clean separation of concerns.

---

## GatewayClass — The Entry Point

`GatewayClass` is a cluster-scoped resource (not namespaced) that says "this controller handles gateways of this class."

When you enable `gatewayAPI.enabled=true` in Cilium, it automatically creates:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: cilium
spec:
  controllerName: io.cilium/gateway-controller
status:
  conditions:
  - type: Accepted
    status: "True"
    reason: Accepted
```

`controllerName` is the unique identifier Cilium uses to claim ownership of gateways that reference this class. If you had multiple implementations (e.g. Cilium + nginx), each would have its own GatewayClass and only manage gateways referencing their class.

---

## Gateway — The Listener

A `Gateway` defines a network endpoint — what ports to listen on, what protocols, TLS config, and which namespaces are allowed to attach routes.

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: main-gateway
  namespace: platform         # platform team owns this namespace
spec:
  gatewayClassName: cilium    # use Cilium's implementation

  listeners:
  - name: http
    port: 80
    protocol: HTTP
    allowedRoutes:
      namespaces:
        from: Selector
        selector:
          matchLabels:
            gateway-access: allowed   # only namespaces with this label can attach

  - name: https
    port: 443
    protocol: HTTPS
    tls:
      mode: Terminate           # Gateway terminates TLS (has the cert)
      certificateRefs:
      - name: tls-cert-secret
    allowedRoutes:
      namespaces:
        from: All               # any namespace can attach HTTPS routes
```

When Cilium sees this Gateway:
1. Creates a `LoadBalancer` Service for it → gets an external IP
2. Programs eBPF to forward traffic on that IP:port
3. For HTTP/HTTPS → hands off to Envoy for L7 routing
4. Updates `Gateway.status` with the assigned IP

---

## Routes — The Routing Rules

### HTTPRoute

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: api-routes
  namespace: quant-team         # dev team's namespace
spec:
  parentRefs:
  - name: main-gateway
    namespace: platform          # attach to the platform-owned gateway
    sectionName: https           # specifically the HTTPS listener

  hostnames:
  - "api.internal.company.com"

  rules:
  # Route /api/v1 to service-a
  - matches:
    - path:
        type: PathPrefix
        value: /api/v1
    backendRefs:
    - name: service-a
      port: 8080
      weight: 100

  # Traffic splitting — canary deploy (90% stable, 10% canary)
  - matches:
    - path:
        type: PathPrefix
        value: /api/v2
    backendRefs:
    - name: service-a-stable
      port: 8080
      weight: 90
    - name: service-a-canary
      port: 8080
      weight: 10

  # Header-based routing
  - matches:
    - headers:
      - name: X-Beta-User
        value: "true"
    backendRefs:
    - name: service-a-beta
      port: 8080
```

### TCPRoute (experimental channel)

For non-HTTP TCP traffic — databases, custom protocols:

```yaml
apiVersion: gateway.networking.k8s.io/v1alpha2
kind: TCPRoute
metadata:
  name: postgres-route
  namespace: platform
spec:
  parentRefs:
  - name: db-gateway
    sectionName: postgres

  rules:
  - backendRefs:
    - name: stackgres-cluster-pooler   # route to PgBouncer
      port: 5432
```

### TLSRoute — Passthrough (experimental channel)

TLS terminated at the backend, not the gateway. Gateway just forwards encrypted traffic:

```yaml
apiVersion: gateway.networking.k8s.io/v1alpha2
kind: TLSRoute
metadata:
  name: secure-backend
spec:
  parentRefs:
  - name: main-gateway
    sectionName: tls-passthrough

  rules:
  - backendRefs:
    - name: backend-with-mtls
      port: 8443
```

---

## CRD Channels — Standard vs Experimental

Gateway API ships in two channels:

| Channel | CRDs included | Stability |
|---------|--------------|-----------|
| **Standard** | GatewayClass, Gateway, HTTPRoute, GRPCRoute | GA / stable |
| **Experimental** | + TCPRoute, TLSRoute, UDPRoute, BackendLBPolicy | Alpha / may change |

Install standard for production, experimental if you need TCP/UDP routing.

```bash
# Standard channel (HTTPRoute, GRPCRoute)
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.0/standard-install.yaml

# Experimental channel (adds TCPRoute, TLSRoute, UDPRoute)
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.0/experimental-install.yaml
```

---

## How Cilium Implements Gateway API Internally

This is where it gets interesting. Cilium doesn't use a separate ingress controller process — it integrates Gateway API directly into its eBPF + Envoy architecture.

### The Two Planes

```
┌────────────────────────────────────────────────────────────┐
│                    Control Plane                           │
│                                                            │
│  cilium-operator                                           │
│  ├─ Watches GatewayClass, Gateway, HTTPRoute CRDs         │
│  ├─ Creates LoadBalancer Services for each Gateway         │
│  ├─ Generates Envoy config (xDS) from HTTPRoutes          │
│  └─ Updates status on Gateway/Route resources             │
└────────────────────────────────────────────────────────────┘
                          │
                          │ programs
                          ▼
┌────────────────────────────────────────────────────────────┐
│                     Data Plane                             │
│                                                            │
│  eBPF programs (in kernel)                                 │
│  └─ Handle L3/L4: initial packet interception,            │
│     load balancing, service IP translation                 │
│     For L4 TCPRoute: eBPF handles end-to-end             │
│                                                            │
│  Envoy (cilium-envoy DaemonSet)                           │
│  └─ Handle L7: HTTP routing, header manipulation,         │
│     traffic splitting, TLS termination                    │
│     Receives xDS config from cilium-operator              │
└────────────────────────────────────────────────────────────┘
```

### L4 vs L7 Traffic Path

**TCPRoute (L4) — eBPF only:**
```
Client packet arrives at node
        │
        ▼
eBPF tc (traffic control) hook intercepts
        │
        ▼
eBPF looks up TCPRoute backend in BPF map
        │
        ▼
DNAT directly to backend pod IP:port
        │
        ▼
Backend pod        (Envoy never involved)
```

**HTTPRoute (L7) — eBPF + Envoy:**
```
Client packet arrives at node
        │
        ▼
eBPF tc hook intercepts
        │
        ▼
eBPF sees: this is a Gateway IP, L7 routing needed
        │
        ▼
eBPF redirects to local Envoy process (cilium-envoy)
        │
        ▼
Envoy applies HTTPRoute rules:
  - path matching
  - header matching
  - traffic weights
  - TLS termination
        │
        ▼
Envoy forwards to backend pod
        │
        ▼
Return traffic: eBPF handles SNAT on the way back
```

### Envoy Config Generation

When you apply an HTTPRoute, the cilium-operator translates it into Envoy's xDS (eXtensible Discovery Service) config and pushes it to Envoy via the xDS API:

```
HTTPRoute YAML
      │
      ▼
cilium-operator translates to:
  - Envoy Listener (port + protocol)
  - Envoy Route Configuration (path/header rules)
  - Envoy Cluster (backend service + endpoints)
      │
      ▼
Pushed to cilium-envoy via xDS gRPC stream
      │
      ▼
Envoy hot-reloads config (zero downtime)
```

You never write Envoy config manually — the Gateway API resources ARE the config.

### LB-IPAM — How Gateways Get IPs on Bare Metal

On cloud providers, `LoadBalancer` Services get IPs from the cloud. On bare metal (your homelab, Tower's on-prem), there's no cloud. Cilium solves this with **LB-IPAM** (Load Balancer IP Address Management).

You define a pool of IPs Cilium can assign:

```yaml
apiVersion: "cilium.io/v2alpha1"
kind: CiliumLoadBalancerIPPool
metadata:
  name: homelab-pool
spec:
  cidrs:
  - cidr: "192.168.1.100/28"   # IPs .100 → .115 available for LB services
```

When a Gateway creates a LoadBalancer Service:
1. Cilium LB-IPAM assigns an IP from the pool (e.g. `192.168.1.100`)
2. Cilium programs eBPF to respond to ARP for that IP on the node
3. Traffic to `192.168.1.100` arrives at the node and gets forwarded

This is how Tower's bare metal clusters get stable IPs for their Gateways without a cloud provider.

---

## Installation on Your k3s Cluster

When you're ready (not doing it now):

### Step 1 — Install Gateway API CRDs

```bash
# Standard channel (sufficient for HTTP/HTTPS/gRPC)
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.0/standard-install.yaml

# Verify CRDs exist
kubectl get crd | grep gateway.networking.k8s.io
# gatewayclasses.gateway.networking.k8s.io
# gateways.gateway.networking.k8s.io
# httproutes.gateway.networking.k8s.io
# grpcroutes.gateway.networking.k8s.io
```

### Step 2 — Upgrade Cilium via Helm

```bash
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml

helm upgrade cilium cilium/cilium \
  --namespace kube-system \
  --reuse-values \
  --set gatewayAPI.enabled=true
```

`--reuse-values` keeps all our existing config (kubeProxyReplacement, k8sServiceHost, etc.) and just adds the Gateway API flag.

### Step 3 — Verify GatewayClass was created

```bash
kubectl get gatewayclass
# NAME     CONTROLLER                     ACCEPTED   AGE
# cilium   io.cilium/gateway-controller   True       30s
```

### Step 4 — (Homelab only) Configure LB-IPAM

Since jay1 has no cloud load balancer:

```bash
cat <<EOF | kubectl apply -f -
apiVersion: "cilium.io/v2alpha1"
kind: CiliumLoadBalancerIPPool
metadata:
  name: homelab-pool
spec:
  cidrs:
  - cidr: "192.168.1.100/28"
EOF
```

This gives Cilium IPs `192.168.1.100–115` to assign to Gateways.

### Step 5 — Deploy a test Gateway + HTTPRoute

```bash
# Create a namespace
kubectl create namespace gateway-test

# Deploy a simple echo server
kubectl -n gateway-test create deployment echo \
  --image=hashicorp/http-echo -- -text="hello from gateway api"
kubectl -n gateway-test expose deployment echo --port=5678

# Create a Gateway
cat <<EOF | kubectl apply -f -
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: test-gateway
  namespace: gateway-test
spec:
  gatewayClassName: cilium
  listeners:
  - name: http
    port: 80
    protocol: HTTP
    allowedRoutes:
      namespaces:
        from: Same
EOF

# Create an HTTPRoute
cat <<EOF | kubectl apply -f -
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: echo-route
  namespace: gateway-test
spec:
  parentRefs:
  - name: test-gateway
  rules:
  - matches:
    - path:
        type: PathPrefix
        value: /
    backendRefs:
    - name: echo
      port: 5678
EOF

# Get the assigned IP
kubectl get gateway test-gateway -n gateway-test
# EXTERNAL-IP will be from your LB-IPAM pool

# Test it
curl http://192.168.1.100/
# hello from gateway api
```

---

## Gateway API vs Ingress — Side by Side

```yaml
# Old way — Ingress
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  annotations:
    nginx.ingress.kubernetes.io/rewrite-target: /    # vendor annotation
    nginx.ingress.kubernetes.io/canary: "true"        # more vendor annotations
    nginx.ingress.kubernetes.io/canary-weight: "10"   # not portable at all
spec:
  rules:
  - host: api.example.com
    http:
      paths:
      - path: /api
        backend:
          service:
            name: my-service
            port:
              number: 8080
```

```yaml
# New way — HTTPRoute
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: api-route              # no annotations needed
spec:
  parentRefs:
  - name: main-gateway
  hostnames: ["api.example.com"]
  rules:
  - matches:
    - path:
        type: PathPrefix
        value: /api
    backendRefs:
    - name: my-service-stable   # traffic splitting is first-class
      port: 8080
      weight: 90
    - name: my-service-canary
      port: 8080
      weight: 10                # portable across all Gateway API implementations
```

---

## Sharp Edges

**1. CRDs must be installed before enabling Cilium Gateway API**
If you enable `gatewayAPI.enabled=true` in Cilium before the CRDs exist, the operator crashes on startup. Always install CRDs first.

**2. TCPRoute and TLSRoute require the experimental channel**
Standard channel only gives you HTTP/HTTPS/gRPC. If you want TCP routing (e.g. for StackGres/PostgreSQL), you need experimental CRDs.

**3. `--reuse-values` is important on Helm upgrade**
Without it, `helm upgrade` resets all your existing Cilium values (kubeProxyReplacement, k8sServiceHost, etc.) to defaults. Always use `--reuse-values` when adding features to an existing Cilium install.

**4. GatewayClass is cluster-scoped, Gateway is namespaced**
GatewayClass exists once per cluster. Gateways live in namespaces — typically a platform-owned namespace. Routes live in app namespaces and reference the Gateway by name + namespace.

**5. `allowedRoutes` is your security boundary**
Without restricting `allowedRoutes`, any namespace can attach routes to your Gateway. In a multi-tenant cluster, always set `from: Selector` with a label restriction so only approved namespaces can attach.

**6. LB-IPAM pool must not overlap with DHCP range**
If your home router's DHCP assigns IPs from `.100` onwards, there'll be conflicts. Check your router's DHCP range first and pick a non-overlapping block for the Cilium pool.

---

## How This Fits Tower's Stack

Tower uses Cilium in BGP mode — their Gateways get real routable IPs announced via BGP to their datacenter switches, not LB-IPAM. The Gateway API resources are the same, but the IP assignment mechanism is BGP instead of ARP.

```
Dev team creates HTTPRoute → attaches to platform Gateway
        │
        ▼
Cilium programs Envoy with the route
        │
        ▼
Gateway gets IP via BGP announcement to ToR switch
        │
        ▼
Traffic from internal clients hits the switch
Switch routes to the node hosting the Gateway
eBPF + Envoy handles the rest
```

This is why understanding Cilium BGP mode is Tier 1 on your learning plan — it's the mechanism that makes Gateway IPs reachable across Tower's datacenter network.
