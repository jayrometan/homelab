# Cilium L2 Announcement — Making LoadBalancer IPs Reachable on the LAN

> Date: 2026-07-29  
> Cluster: jay1 (192.168.1.25) + jay2 (192.168.1.26), Cilium 1.19.5  
> LB pool: 192.168.1.240/28

---

## The Problem

Cilium's LB-IPAM assigns IPs from the `homelab-pool` (`192.168.1.240/28`) to
LoadBalancer Services automatically. After assignment:

| Service | IP |
|---|---|
| podinfo | 192.168.1.240 |
| stackgres-restapi | 192.168.1.241 |
| postgres primary | 192.168.1.242 |

These IPs are **reachable from jay1 and jay2** because Cilium's eBPF dataplane
intercepts packets destined for them in-kernel — no ARP needed on the node.

They are **not reachable from the MacBook or any other LAN host** because:

1. The MacBook sees `192.168.1.241` as a host on its local subnet (`/24`)
2. It sends an ARP broadcast: "who has 192.168.1.241?"
3. No node responds — Cilium never claimed that IP on the wire
4. ARP times out → connection fails

The IPs exist logically inside the cluster's eBPF tables but are invisible to
the broader Ethernet segment.

---

## How L2 Announcement Fixes It

`CiliumL2AnnouncementPolicy` tells Cilium to respond to ARP requests for LB IPs
on the physical LAN interface. When active:

```
MacBook → ARP "who has 192.168.1.241?"
jay1    ← ARP reply "192.168.1.241 is at bc:24:11:85:54:a3" (jay1's MAC)
MacBook → sends TCP SYN to bc:24:11:85:54:a3
jay1 eBPF → intercepts, forwards to stackgres-restapi pod
```

From the MacBook's perspective, `192.168.1.241` is just a normal host on the
LAN. The fact that it's running inside a Kubernetes pod behind eBPF is invisible.

### Leader election — no VRRP, no keepalived

When multiple nodes match `nodeSelector` (both jay1 and jay2 here), Cilium
runs an internal **leader election** per IP using Kubernetes lease objects.
Only the elected node sends ARP replies for that IP. If the leader node goes
down, another node wins the election and takes over the ARP lease within
seconds. No floating IP config or external HA daemon required.

This is fundamentally different from traditional keepalived/VRRP setups:
- keepalived: you configure which IPs float between specific nodes
- Cilium L2: Cilium manages the election automatically; you just describe *which
  services* to announce and *which interfaces* to use

### Why not BGP?

BGP is the production-grade answer for multi-subnet environments — the router
learns routes and forwards traffic across subnets. For a homelab where every
device (MacBook, servers, NAS) is on the same `192.168.1.0/24` flat network,
BGP is unnecessary overhead. L2 announcement (ARP) is simpler and achieves the
same result when everything is on one Ethernet segment.

BGP makes sense when you have:
- Multiple VLANs / routed subnets
- A BGP-capable switch or router (Arista, Cumulus, FRR)
- Need to advertise specific prefixes, not just /32 host routes

---

## What Was Deployed

### `infrastructure/cilium/ippool.yaml` — CiliumLoadBalancerIPPool

```yaml
apiVersion: cilium.io/v2
kind: CiliumLoadBalancerIPPool
metadata:
  name: homelab-pool
spec:
  blocks:
  - cidr: "192.168.1.240/28"
```

This was previously applied with `kubectl apply` directly. It is now tracked in
Git and managed by Flux. **No change to cluster state** — Flux adopts the
existing object via SSA.

### `infrastructure/cilium/l2announcement.yaml` — CiliumL2AnnouncementPolicy

```yaml
apiVersion: cilium.io/v2alpha1
kind: CiliumL2AnnouncementPolicy
metadata:
  name: homelab-l2-policy
spec:
  serviceSelector:
    matchLabels: {}     # all LoadBalancer services
  nodeSelector:
    matchLabels: {}     # all nodes participate in leader election
  loadBalancerIPs: true
  externalIPs: false
  interfaces:
  - "^ens[0-9]+"       # matches ens18 on jay1 and jay2
```

**Key fields explained:**

| Field | Value | Why |
|---|---|---|
| `serviceSelector: {}` | selects all services | Simpler than labelling each service; all LB services in this cluster should be reachable |
| `nodeSelector: {}` | all nodes | jay1 and jay2 both participate; Cilium picks the leader per IP |
| `loadBalancerIPs: true` | enabled | Announce IPs assigned by LB-IPAM |
| `externalIPs: false` | disabled | We don't use manually-set `spec.externalIPs` on any service |
| `interfaces: ^ens[0-9]+` | regex | Matches `ens18` (physical NIC) on both nodes; explicitly excludes `lxc*`, `cilium_*`, `tailscale0` |

### `clusters/jay1/cilium.yaml` — Flux Kustomization

Registers the `infrastructure/cilium/` directory with Flux. The root
`flux-system` Kustomization watches `./clusters/jay1/` and picks this up
automatically on the next reconcile.

No `dependsOn` is needed because the Cilium CRDs (`CiliumLoadBalancerIPPool`,
`CiliumL2AnnouncementPolicy`) are installed when Cilium starts — before Flux
even runs. Flux just applies objects into pre-existing CRD schemas.

---

## Verification

```bash
# 1. Check the policy was applied
kubectl get ciliuml2announcementpolicy homelab-l2-policy -o wide

# 2. Check which node holds the ARP lease for each IP
kubectl get ciliuml2announcemententry -A

# 3. From MacBook — ARP lookup
arp -n 192.168.1.241
# Expected: 192.168.1.241 (192.168.1.241) at bc:24:11:85:54:a3 on en0

# 4. Curl the StackGres console
curl -k https://192.168.1.241
# Expected: HTML response from StackGres Web Console

# 5. Curl podinfo
curl http://192.168.1.240:9898
# Expected: {"hostname": "podinfo-...", "version": "6.7.0", ...}
```

---

## Day-2 Operations

### Adding a new LoadBalancer Service

No policy change needed. Any new Service of `type: LoadBalancer` automatically
gets an IP from the pool and is announced via L2 — the `serviceSelector: {}`
covers it.

To request a specific IP:
```yaml
metadata:
  annotations:
    lbipam.cilium.io/ips: "192.168.1.243"
```

### Restricting which services are announced

Replace `serviceSelector: matchLabels: {}` with a label filter:
```yaml
serviceSelector:
  matchLabels:
    announce-via-l2: "true"
```

Then label only the services you want announced:
```bash
kubectl label svc my-service -n my-ns announce-via-l2=true
```

### Restricting to specific nodes (edge nodes)

```yaml
nodeSelector:
  matchLabels:
    node-role.kubernetes.io/edge: "true"
```

Useful if you add nodes that are not on the same L2 segment as the LAN
(e.g. a node on a different VLAN or a remote site).

### Checking ARP lease holder

```bash
kubectl get ciliuml2announcemententry -A
# Shows which node currently holds the ARP lease for each IP
```

If a node goes down, the lease moves to another node within the
`leaseDuration` (default ~15s). During that window, ARP cache on LAN hosts
still points to the old MAC — connections may stall briefly until ARP cache
expires or is refreshed.

---

## Why Not Just Add a Static Route on the MacBook?

A static route (`sudo route add 192.168.1.240/28 192.168.1.25`) would work but
has problems:
- Routes are lost on reboot
- Only works on the MacBook, not other LAN devices
- Ties you to jay1 as the gateway — if jay1 goes down, the route breaks
- Doesn't scale as you add more devices

L2 announcement is transparent — any LAN host reaches the IPs without any
client-side config.
