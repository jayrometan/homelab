# k3s + Cilium Homelab Setup

Single-node Kubernetes cluster on Rocky Linux 9.3 with Cilium in full kube-proxy replacement mode.

- **Node:** 192.168.1.25 (jay1) — 14 vCPU, 19GB RAM
- **k3s:** v1.36.2
- **Cilium:** v1.19.5
- **Pod CIDR:** 10.42.0.0/16
- **Service CIDR:** 10.43.0.0/16

---

## Why This Setup

Standard k3s ships with flannel (CNI) and kube-proxy (service routing via iptables). We replaced both with Cilium:

- **No flannel** — Cilium handles all pod networking
- **No kube-proxy** — Cilium's eBPF programs handle all service routing in-kernel
- **No iptables rules** — ClusterIP lookups are O(1) eBPF map lookups, not O(n) iptables chain scans

This mirrors production HFT platform setups (e.g. Tower Research Capital) where latency and scale matter.

---

## Installation Steps

### 1. Install k3s (no flannel, no kube-proxy)

```bash
curl -sfL https://get.k3s.io | sh -s -   --flannel-backend=none   --disable-network-policy   --disable=traefik   --disable=servicelb   --disable-kube-proxy
```

After this, node is `NotReady` — expected, no CNI yet.

### 2. Install Helm

```bash
curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
```

### 3. Install Cilium

```bash
helm repo add cilium https://helm.cilium.io/ && helm repo update

helm install cilium cilium/cilium   --namespace kube-system   --set kubeProxyReplacement=true   --set k8sServiceHost=192.168.1.25   --set k8sServicePort=6443   --set ipam.mode=kubernetes   --set operator.replicas=1
```

**Key flags:**
- `kubeProxyReplacement=true` — Cilium loads eBPF programs to handle all service traffic
- `k8sServiceHost/Port` — Cilium needs to reach the API server directly since kube-proxy is gone
- `operator.replicas=1` — single node, no need for HA operator

### 4. Disable firewalld

Rocky 9 runs firewalld by default. It blocks traffic between pod network namespaces and ClusterIPs.

```bash
systemctl disable --now firewalld
```

Then restart any pods that failed due to this:

```bash
kubectl rollout restart deployment/metrics-server deployment/local-path-provisioner -n kube-system
```

---

## What Goes Wrong Without Step 4

Pods trying to reach the Kubernetes API via ClusterIP (`10.43.0.1:443`) get:

```
dial tcp 10.43.0.1:443: connect: no route to host
```

**Why:** Cilium DNATs `10.43.0.1:443` → `192.168.1.25:6443`. The return traffic from the API server back to the pod IP (`10.42.0.x`) gets dropped by firewalld — it has no rules for pod/service CIDRs.

This only affects pods, not the node itself. From the node's root network namespace, the same curl succeeds because firewalld allows it. Pods live in their own network namespaces, and return traffic from `192.168.1.25:6443 → 10.42.0.x` hits firewalld and gets dropped.

In production Kubespray setups, this is handled by either disabling firewalld or adding explicit zone rules for pod and service CIDRs.

---

## How Service Routing Works (No kube-proxy)

```
Pod A → connect to 10.43.0.1:443 (kubernetes ClusterIP)
         │
         ├─ Cilium eBPF hook intercepts at socket level (before packet is formed)
         ├─ Looks up service in eBPF map: 10.43.0.1:443 → 192.168.1.25:6443
         ├─ Rewrites destination in-kernel (DNAT)
         ├─ Packet goes to 192.168.1.25:6443 (actual API server)
         ├─ Response returns, Cilium reverses the DNAT
         └─ Pod A sees response as coming from 10.43.0.1:443 ✓
```

No iptables. No kube-proxy process watching for changes. Cilium operator updates eBPF maps atomically whenever a Service or Endpoint changes.

---

## k3s as a systemd Service

k3s runs as a systemd unit:

```bash
systemctl status k3s
systemctl restart k3s
journalctl -u k3s -f          # follow logs
```

Config and state:
- Kubeconfig: `/etc/rancher/k3s/k3s.yaml`
- Data dir: `/var/lib/rancher/k3s/`
- Service file: `/etc/systemd/system/k3s.service`

```bash
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
```

---

## Useful Commands

```bash
# Cluster state
kubectl get nodes -o wide
kubectl get pods -A

# Cilium health
kubectl -n kube-system exec ds/cilium -- cilium status
kubectl -n kube-system exec ds/cilium -- cilium status --brief

# See all ClusterIP → endpoint mappings (replaces iptables -L)
kubectl -n kube-system exec ds/cilium -- cilium service list

# Live packet trace (great for debugging)
kubectl -n kube-system exec ds/cilium -- cilium monitor

# Confirm kube-proxy replacement is active
kubectl -n kube-system exec ds/cilium -- cilium status | grep KubeProxy
```
