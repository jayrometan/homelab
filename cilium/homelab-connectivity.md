# Homelab Network Connectivity — How Traffic Reaches Your Pods

> Lab: jay1 at 192.168.1.25, k3s v1.36.2, Cilium 1.19.5, VXLAN mode
> Written to explain how a request from your MacBook or any home network device reaches a pod running inside Kubernetes.

---

## The Four Address Spaces You Need to Know

This is the root of all confusion. Kubernetes runs multiple overlapping networks, and knowing which one you are in tells you what you can reach.

```
Your Home Network (LAN):     192.168.1.0/24
  MacBook:                   192.168.1.x
  jay1 (node):               192.168.1.25
  Router/gateway:            192.168.1.254
  LB-IPAM pool:              192.168.1.240–255   ← addresses Cilium hands out to LoadBalancer Services

Kubernetes Pod Network:      10.42.0.0/24        ← every pod gets an IP here
  podinfo pod 1:             10.42.0.77
  podinfo pod 2:             10.42.0.12

Kubernetes Service Network:  10.43.0.0/16        ← virtual IPs, only exist in eBPF tables
  podinfo ClusterIP:         10.43.239.5
```

These three networks are completely separate. Nothing on your home LAN can route to `10.42.x.x` or `10.43.x.x` directly — those IPs only exist inside the node, managed by Cilium's eBPF programs. The **only** IPs reachable from the outside are `192.168.1.x` addresses — including the LB-IPAM-assigned ones.

---

## The Three Service Types and Where They're Reachable

Kubernetes gives you three ways to expose a workload. Each has a different reach:

```
┌─────────────────┬─────────────────────┬───────────────────────────────────────┐
│ Service Type    │ Address             │ Reachable from                        │
├─────────────────┼─────────────────────┼───────────────────────────────────────┤
│ ClusterIP       │ 10.43.239.5:9898    │ Only inside the cluster               │
│                 │                     │ (pods, and the node itself via Cilium) │
├─────────────────┼─────────────────────┼───────────────────────────────────────┤
│ NodePort        │ 192.168.1.25:31120  │ Any machine on the home network       │
│                 │                     │ Uses the node's own IP + a high port   │
├─────────────────┼─────────────────────┼───────────────────────────────────────┤
│ LoadBalancer    │ 192.168.1.240:9898  │ Any machine on the home network       │
│                 │                     │ Clean IP from LB-IPAM pool            │
└─────────────────┴─────────────────────┴───────────────────────────────────────┘
```

Your podinfo service is `type: LoadBalancer`, so it exposes all three simultaneously:
- ClusterIP `10.43.239.5:9898` — pods inside the cluster use this
- NodePort `192.168.1.25:31120` — accessible from outside, uses node's IP
- LoadBalancer `192.168.1.240:9898` — accessible from outside, clean dedicated IP

**From your MacBook on the home network, you can use either:**
```bash
curl http://192.168.1.240:9898   # LoadBalancer IP (cleanest)
curl http://192.168.1.25:31120   # NodePort (same thing, different address)
```

**You cannot use from your MacBook:**
```bash
curl http://10.43.239.5:9898     # ClusterIP — doesn't exist on the home network
curl http://10.42.0.77:9898      # Pod IP — doesn't exist on the home network
```

---

## How a Packet Gets from Your MacBook to the Pod

Here is the complete journey, step by step, for `curl http://192.168.1.240:9898` from a device on your home network.

```
MacBook (192.168.1.x)
        │
        │  Step 1: ARP
        │  "Who has 192.168.1.240? Tell 192.168.1.x"
        ▼
Home network switch/router
        │
        │  Step 2: jay1 responds
        │  "192.168.1.240 is at bc:24:11:85:54:a3" (jay1's MAC)
        ▼
jay1 NIC (ens18) receives the packet
  Ethernet frame: src=MacBook MAC, dst=bc:24:11:85:54:a3
  IP packet:      src=192.168.1.x, dst=192.168.1.240, dport=9898
        │
        │  Step 3: Cilium eBPF tc hook fires
        │  Before the Linux kernel even processes the packet,
        │  the BPF program attached to ens18 intercepts it
        ▼
Cilium eBPF LB lookup (BPF map)
  Lookup key:   192.168.1.240:9898
  Lookup result: [10.42.0.77:9898, 10.42.0.12:9898]  ← two pods
  Pick one:      10.42.0.77:9898  (consistent hash load balancing)
        │
        │  Step 4: DNAT (Destination Network Address Translation)
        │  Rewrite the destination in the packet header:
        │  dst: 192.168.1.240:9898  →  10.42.0.77:9898
        ▼
Packet now destined for 10.42.0.77 (pod IP)
        │
        │  Step 5: Deliver to pod
        │  Pod is on this same node (single-node cluster)
        │  Cilium routes via cilium_host virtual interface
        ▼
podinfo pod (10.42.0.77) receives the packet
        │
        │  Step 6: Response
        │  Pod sends reply: src=10.42.0.77, dst=192.168.1.x
        │  eBPF SNAT rewrites source: 10.42.0.77 → 192.168.1.240
        │  (so the reply comes back from the expected IP)
        ▼
MacBook receives response from 192.168.1.240
```

The critical insight: **192.168.1.240 is not a real interface on jay1.** There is no `ip addr` entry for it. It only exists in Cilium's eBPF BPF maps. Cilium answers ARP for it (making your router think jay1 owns that IP) and handles all packet rewriting in the kernel via eBPF before the Linux network stack even sees them.

---

## The Real eBPF LB Table From Your Cluster

This is the actual output of `cilium bpf lb list` from your cluster for the podinfo service:

```
192.168.1.240:9898/TCP (1)   10.42.0.12:9898/TCP  (backend 1 — pod 2)
192.168.1.240:9898/TCP (2)   10.42.0.77:9898/TCP  (backend 2 — pod 1)
192.168.1.240:9898/TCP (0)   0.0.0.0:0            [LoadBalancer]

10.43.239.5:9898/TCP (1)     10.42.0.12:9898/TCP  (backend 1)
10.43.239.5:9898/TCP (2)     10.42.0.77:9898/TCP  (backend 2)
10.43.239.5:9898/TCP (0)     0.0.0.0:0            [ClusterIP, non-routable]

0.0.0.0:31120/TCP (1)        10.42.0.12:9898/TCP  (backend 1 — NodePort)
0.0.0.0:31120/TCP (2)        10.42.0.77:9898/TCP  (backend 2 — NodePort)
```

What you are reading:
- Each entry maps a virtual address (LB IP, ClusterIP, or NodePort) to a real pod IP
- The `(0)`, `(1)`, `(2)` are slot indices — slot 0 is the service entry, slots 1+ are backends
- All three frontend addresses (LB, ClusterIP, NodePort) point to the same two backends
- `[non-routable]` on ClusterIP means Cilium marks it as internal-only (eBPF still handles it, but no ARP response is sent for those IPs on the LAN)

**This replaces iptables entirely.** Before Cilium, kube-proxy would have created dozens of iptables DNAT rules to implement this. With Cilium in kube-proxy replacement mode (your setup), none of that exists — it is all in these BPF maps. Lookup is O(1) regardless of how many services exist.

---

## How LB-IPAM Works — Who Owns 192.168.1.240?

On a cloud provider (AWS, GCP), when you create a `type: LoadBalancer` service, the cloud assigns an IP from its pool and configures a real load balancer. On your bare-metal homelab there is no cloud. Cilium's LB-IPAM fills that gap.

**Step 1 — Define a pool:**
```yaml
apiVersion: cilium.io/v2
kind: CiliumLoadBalancerIPPool
metadata:
  name: homelab-pool
spec:
  blocks:
    - cidr: 192.168.1.240/28    # IPs .240 through .255 available
```

**Step 2 — Service requests an IP:**
When Kubernetes creates a `LoadBalancer` Service, it sets `status.loadBalancer.ingress` to the assigned IP. Cilium LB-IPAM watches for new LoadBalancer Services and assigns the next available IP from the pool.

**Step 3 — Cilium announces the IP via ARP:**
Cilium sends a gratuitous ARP on `ens18`:
```
"I am 192.168.1.240 and my MAC is bc:24:11:85:54:a3"
```
Your router and any other device on the LAN update their ARP table. From that point, any packet destined for `192.168.1.240` is sent to jay1's MAC address.

**Step 4 — eBPF intercepts and DNATs:**
As described above. The IP never appears in `ip addr` on the node — it is a virtual address that only exists in eBPF.

**Your current pool state:**
- Pool: `192.168.1.240/28` — 16 IPs available (.240 through .255)
- Allocated: `192.168.1.240` (podinfo)
- Remaining: `192.168.1.241` through `.255` — available for future services or Gateways

---

## Your Cluster's Actual Network Interfaces

Running `ip addr` + `ip route` on jay1 reveals the full picture:

```
ens18            192.168.1.25/24   ← Physical NIC. Home network. This is the "real" interface.
cilium_host      10.42.0.100       ← Cilium's virtual interface into the pod network.
                                      Acts as the gateway for pod traffic.
cilium_vxlan     (no IP assigned)  ← VXLAN tunnel interface for pod-to-pod traffic
                                      between nodes. Used when you have multiple nodes.
lo               127.0.0.1/8       ← Loopback. Standard.
```

**Route table:**
```
default via 192.168.1.254 dev ens18          ← Internet traffic goes out via home router
10.42.0.0/24 via 10.42.0.100 dev cilium_host ← Pod traffic goes via Cilium's virtual gateway
192.168.1.0/24 dev ens18                     ← LAN traffic goes out the physical NIC
```

Pod traffic (`10.42.0.x`) is routed through `cilium_host`, which is Cilium's internal bridge into the pod network. The Linux kernel never routes it out through `ens18` — it stays inside the node.

---

## VXLAN — Why It Exists and What It Does

Your cluster is running in `tunnel: vxlan` mode (check: `cilium config view | grep routing-mode`).

VXLAN is an **encapsulation protocol**. When pod traffic needs to cross from one node to another, Cilium wraps the pod packet inside a UDP packet with a VXLAN header, sends it across the physical network, and the receiving node's Cilium unwraps it.

```
Pod A (10.42.0.77 on jay1)  →  Pod B (10.42.1.5 on jay2)

Without VXLAN:   Your home router has no idea what 10.42.x.x is → packet dropped
With VXLAN:      Cilium wraps packet in UDP 192.168.1.25 → 192.168.1.26
                 Router knows how to forward that → jay2 receives it → unwraps → pod B gets it
```

**In your current single-node setup, VXLAN does nothing.** Both pods are on jay1, so there is no inter-node traffic. The `cilium_vxlan` interface exists but carries no traffic. If you add a second node (jay2 at 192.168.1.26), inter-pod traffic would automatically use VXLAN without any configuration change.

**VXLAN vs native routing (for future reference):**

In VXLAN mode (your setup): encapsulates all pod traffic. Works on any network — even if the router doesn't know about pod CIDRs.

In native routing mode: pod packets are sent as-is. The router must have routes to each node's pod CIDR (`10.42.0.0/24 via 192.168.1.25`, `10.42.1.0/24 via 192.168.1.26`, etc.). Faster (no encapsulation overhead) but requires router config. Most production clusters use native routing or BGP.

---

## Connectivity Summary — What You Can Reach From Where

```
From your MacBook (192.168.1.x):
  ✓ http://192.168.1.240:9898     LoadBalancer IP — works, Cilium answers ARP
  ✓ http://192.168.1.25:31120     NodePort — works, traffic to node's real IP
  ✗ http://10.43.239.5:9898       ClusterIP — does not exist on home LAN
  ✗ http://10.42.0.77:9898        Pod IP — does not exist on home LAN

From inside jay1 (the node itself):
  ✓ http://192.168.1.240:9898     Works — eBPF intercepts, DNATs to pod
  ✓ http://10.43.239.5:9898       Works — ClusterIP, eBPF handles it
  ✓ http://10.42.0.77:9898        Works — direct pod IP, routed via cilium_host

From inside a pod (e.g., exec into another pod):
  ✓ http://podinfo.podinfo.svc.cluster.local:9898   DNS resolves ClusterIP, eBPF handles it
  ✓ http://10.43.239.5:9898       Works — ClusterIP
  ✓ http://10.42.0.77:9898        Works — direct pod IP
  ✓ http://192.168.1.240:9898     Works — goes out via default route, comes back in
```

---

## Quick Test Commands

Run these from your MacBook (or any machine on the home network):

```bash
# Hit the podinfo API directly
curl http://192.168.1.240:9898

# Hit via NodePort (same backend, different frontend)
curl http://192.168.1.25:31120

# Watch which pod responds (hostname rotates between the two pods with repeated requests)
for i in $(seq 1 6); do curl -s http://192.168.1.240:9898 | grep hostname; done
```

Run these from jay1 (the node) via SSH:

```bash
# From the node — all of these should work
curl http://192.168.1.240:9898     # LB IP
curl http://10.43.239.5:9898       # ClusterIP (only works from node/pod)
curl http://10.42.0.77:9898        # Pod IP directly

# Check what Cilium has in its LB table for podinfo
kubectl -n kube-system exec ds/cilium -- cilium bpf lb list | grep 192.168.1.240

# Check which IPs are allocated from the LB-IPAM pool
kubectl get ciliumloadbalancerippool homelab-pool -o yaml

# Check all services with their external IPs
kubectl get svc -A
```

---

## The Mental Model in One Diagram

```
                        HOME NETWORK (192.168.1.0/24)
  ┌──────────┐         ┌─────────────────────────────────────────────────────┐
  │ MacBook  │         │                    jay1 (192.168.1.25)              │
  │          │──ARP──▶│                                                     │
  │          │◀──MAC──│  Cilium: "I own 192.168.1.240"                      │
  │          │         │                                                     │
  │  curl    │         │  ┌─────────────────────────────────────────────┐   │
  │  .240    │─packet─▶│  │ ens18 NIC (192.168.1.25)                   │   │
  │  :9898   │         │  │           │                                 │   │
  └──────────┘         │  │     eBPF tc hook fires                      │   │
                       │  │           │                                 │   │
                       │  │  BPF LB lookup: .240:9898 → 10.42.0.77     │   │
                       │  │           │                                 │   │
                       │  │  DNAT: dst rewritten to 10.42.0.77:9898    │   │
                       │  │           │                                 │   │
                       │  │     cilium_host (10.42.0.100)               │   │
                       │  │           │                                 │   │
                       │  │  ┌────────▼────────┐  ┌─────────────────┐  │   │
                       │  │  │ podinfo pod 1   │  │ podinfo pod 2   │  │   │
                       │  │  │ 10.42.0.77:9898 │  │ 10.42.0.12:9898 │  │   │
                       │  │  └─────────────────┘  └─────────────────┘  │   │
                       │  └─────────────────────────────────────────────┘   │
                       └─────────────────────────────────────────────────────┘
```

**The packet never leaves jay1.** It arrives on `ens18`, eBPF rewrites the destination, and it is delivered internally to the pod. The reply is reverse-NATed back to `192.168.1.240` before it goes back out.

---

## Why You Don't Need to Configure Your Router

On a traditional bare-metal setup without Cilium, exposing a service externally requires either:
- Configuring your router with static routes to the pod CIDR
- Running MetalLB which sends ARP/BGP
- Using NodePort and accepting the port-sharing ugliness

With Cilium LB-IPAM:
- No router config needed — Cilium answers ARP directly on the LAN
- No MetalLB — Cilium does IPAM natively
- Clean IPs from your chosen range
- Works on any flat L2 network (your home LAN qualifies)

The only requirement: the LB-IPAM CIDR (`192.168.1.240/28`) must not overlap with your router's DHCP range. If your router hands out IPs up to `.239`, you are safe.
