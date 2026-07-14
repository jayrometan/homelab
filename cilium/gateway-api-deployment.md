# Gateway API Deployment Notes

> Deployed: 2026-07-14 on jay1 (192.168.1.25), Cilium 1.19.5, k3s v1.36.2

---

## What Was Done

### Step 1 — Install Gateway API CRDs

Standard channel v1.2.1 (includes GatewayClass, Gateway, HTTPRoute, GRPCRoute, ReferenceGrant):

```bash
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.1/standard-install.yaml
```

Output:
```
customresourcedefinition.apiextensions.k8s.io/gatewayclasses.gateway.networking.k8s.io created
customresourcedefinition.apiextensions.k8s.io/gateways.gateway.networking.k8s.io created
customresourcedefinition.apiextensions.k8s.io/grpcroutes.gateway.networking.k8s.io created
customresourcedefinition.apiextensions.k8s.io/httproutes.gateway.networking.k8s.io created
customresourcedefinition.apiextensions.k8s.io/referencegrants.gateway.networking.k8s.io created
```

### Step 2 — Enable Gateway API in Cilium

Existing Cilium values before upgrade:
```yaml
ipam:
  mode: kubernetes
k8sServiceHost: 192.168.1.25
k8sServicePort: 6443
kubeProxyReplacement: true
operator:
  replicas: 1
```

Helm upgrade (kept all existing values, added `gatewayAPI.enabled=true`):
```bash
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml

helm upgrade cilium cilium/cilium \
  --version 1.19.5 \
  --namespace kube-system \
  --reuse-values \
  --set gatewayAPI.enabled=true
```

### Step 3 — Restart cilium-operator

The operator needs a restart to pick up the new config and create the GatewayClass:

```bash
kubectl rollout restart deployment/cilium-operator -n kube-system
```

### Step 4 — Verify GatewayClass

```bash
kubectl get gatewayclass
```

Output:
```
NAME     CONTROLLER                     ACCEPTED   AGE
cilium   io.cilium/gateway-controller   True       19s
```

`ACCEPTED: True` means Cilium has claimed the GatewayClass and is ready to reconcile Gateways.

### Step 5 — Create LB-IPAM Pool

Since jay1 is bare metal (no cloud load balancer), Cilium needs a pool of IPs to assign to Gateways.
Using `192.168.1.240/28` (IPs .240–.255) — chosen to avoid the router's DHCP range.

> **Note:** Use `cilium.io/v2` — the `v2alpha1` API is deprecated as of Cilium 1.19.

```bash
kubectl apply -f - <<EOF
apiVersion: cilium.io/v2
kind: CiliumLoadBalancerIPPool
metadata:
  name: homelab-pool
spec:
  blocks:
    - cidr: 192.168.1.240/28
EOF
```

---

## Current State

```bash
# Cilium Helm values (revision 2)
helm get values cilium -n kube-system
# gatewayAPI:
#   enabled: true
# ipam:
#   mode: kubernetes
# k8sServiceHost: 192.168.1.25
# k8sServicePort: 6443
# kubeProxyReplacement: true
# operator:
#   replicas: 1

# GatewayClass
kubectl get gatewayclass
# NAME     CONTROLLER                     ACCEPTED   AGE
# cilium   io.cilium/gateway-controller   True       ...

# LB-IPAM pool
kubectl get ciliumloadbalancerippool
# NAME           DISABLED   CONFLICTING   IPS AVAILABLE   AGE
# homelab-pool   false      False         16              ...
```

---

## Next: Test Gateway + HTTPRoute

Deploy a test workload to validate end-to-end routing (from `gateway-api.md` Step 5):

```bash
# Namespace
kubectl create namespace gateway-test

# Echo server
kubectl -n gateway-test create deployment echo \
  --image=hashicorp/http-echo -- -text="hello from gateway api"
kubectl -n gateway-test expose deployment echo --port=5678

# Gateway
kubectl apply -f - <<EOF
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

# HTTPRoute
kubectl apply -f - <<EOF
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

# Check assigned IP (will be in 192.168.1.240/28 range)
kubectl get gateway test-gateway -n gateway-test

# Test
curl http://<EXTERNAL-IP>/
# hello from gateway api
```

---

## If You Need TCPRoute (e.g. for StackGres/PostgreSQL)

TCPRoute is in the experimental channel — install additional CRDs:

```bash
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.1/experimental-install.yaml
```

This adds TCPRoute, TLSRoute, UDPRoute on top of the standard set.
