# Adding a k3s Worker Node — GitLab Host (192.168.1.26)

> Date: 2026-07-29  
> Cluster: jay1 (192.168.1.25), k3s v1.36.2+k3s1, Rocky Linux 9.3  
> New worker: 192.168.1.26 (GitLab host), 4 vCPU, 3.6Gi RAM, Rocky Linux 9.3

---

## Why this matters

Adding 192.168.1.26 as a worker node gives the cluster a second schedulable
node. Immediate benefit: StackGres `instances: 2` (Patroni primary + 1 replica)
becomes viable — Patroni's hard pod anti-affinity can now place `primary-0` on
jay1 and `primary-1` on the new node, giving real HA. Without a second node,
`instances: 2` leaves a pod permanently Pending.

### Trade-off

192.168.1.26 runs GitLab. Adding it to k3s means the OS now hosts two
demanding services. With 3.6Gi RAM total and GitLab already using ~3Gi, the
node will be memory-constrained. Keep k3s workloads light here (Postgres
replica, not primary). Use resource limits and node selectors / taints to
prevent accidental scheduling of heavy workloads.

---

## Prerequisites

### Firewall / network (verify before joining)

k3s worker-to-server communication needs these ports open from 192.168.1.26
to 192.168.1.25:

| Port | Protocol | Purpose |
|------|----------|---------|
| 6443 | TCP | k8s API server (worker ↔ control plane) |
| 10250 | TCP | kubelet API (metrics, exec, logs) |
| 4240 | TCP | Cilium health checks |
| 8472 | UDP | VXLAN encapsulation (Cilium overlay) |
| 51871 | UDP | WireGuard (if Cilium encryption enabled) |

Check with:
```bash
# From 192.168.1.26 → jay1
nc -zv 192.168.1.25 6443
```

---

## Step 1 — Verify the join token (on jay1)

The token is at `/var/lib/rancher/k3s/server/node-token` on the control-plane
node. Do NOT print it into shared terminals. Pipe it directly:

```bash
# On jay1 — verify token exists
ls -la /var/lib/rancher/k3s/server/node-token
```

---

## Step 2 — Join the worker (on 192.168.1.26)

Single command — pipes the token directly without exposing it:

```bash
# Run ON 192.168.1.26 (the new worker)
curl -sfL https://get.k3s.io | \
  K3S_URL="https://192.168.1.25:6443" \
  K3S_TOKEN="$(ssh root@192.168.1.25 cat /var/lib/rancher/k3s/server/node-token)" \
  INSTALL_K3S_VERSION="v1.36.2+k3s1" \
  sh -s - agent
```

**Why pin the version?**  
`INSTALL_K3S_VERSION` must match the control-plane exactly (`v1.36.2+k3s1`).
A version mismatch causes the kubelet to refuse to join.

**What the installer does:**
1. Downloads the k3s binary for the pinned version
2. Creates the `k3s-agent` systemd unit
3. Starts the agent — it connects to `K3S_URL`, authenticates with `K3S_TOKEN`,
   registers itself as a node, and begins the CNI handshake with Cilium

---

## Step 3 — Verify the node joined (on jay1)

```bash
# Should show two nodes within ~60 seconds
kubectl get nodes -o wide

# NAME    STATUS   ROLES           AGE   VERSION
# jay1    Ready    control-plane   25d   v1.36.2+k3s1   192.168.1.25
# jay2    Ready    <none>          30s   v1.36.2+k3s1   192.168.1.26
#
# Note: worker nodes have no ROLES label by default. Add one if desired:
kubectl label node <new-node-name> node-role.kubernetes.io/worker=worker
```

Check Cilium picked up the node:
```bash
cilium status
# Should show N nodes in the Cilium network
```

---

## Step 4 — Taint the node (optional but recommended)

Because 192.168.1.26 already runs GitLab, prevent arbitrary workloads from
landing on it. Only allow pods that explicitly tolerate the taint:

```bash
# Apply taint — only pods with matching toleration can schedule here
kubectl taint node <node-name> workload=gitlab-host:NoSchedule
```

Then add a toleration to the SGCluster's pod scheduling spec so the Postgres
replica can still land there:

```yaml
# In sgcluster-jay.yaml, under spec.pods.scheduling:
scheduling:
  tolerations:
  - key: workload
    operator: Equal
    value: gitlab-host
    effect: NoSchedule
```

---

## Step 5 — Enable StackGres replica on second node

With two schedulable nodes, bump SGCluster back to `instances: 2`:

```yaml
# infrastructure/stackgres/cluster/sgcluster-jay.yaml
spec:
  instances: 2    # primary-0 on jay1, primary-1 on new worker
  ...
  postgresServices:
    replicas:
      enabled: true   # re-enable replica service
```

Patroni's hard anti-affinity will place `primary-0` and `primary-1` on
separate nodes automatically. No extra scheduling config needed.

Verify Patroni sees both members:
```bash
kubectl exec -n postgres primary-0 -c patroni -- patronictl list
# should show: primary-0 (Leader) + primary-1 (Replica, streaming)
```

---

## Troubleshooting

### Node stays NotReady
```bash
# On the new worker
journalctl -u k3s-agent -f

# Common causes:
# - Token mismatch: "failed to verify token"
# - Network: can't reach 6443
# - Version mismatch: kubelet version != server version
```

### Cilium doesn't pick up the node
```bash
kubectl -n kube-system get pods -l k8s-app=cilium
# A new Cilium agent pod should appear for the new node
# If not: kubectl -n kube-system describe pod cilium-<new-node>
```

### GitLab performance degrades after joining
The k3s agent (containerd + kubelet) adds ~200-300MB RAM overhead. If GitLab
becomes slow, consider:
- Reducing GitLab's Puma worker count (`/etc/gitlab/gitlab.rb`: `puma['worker_processes'] = 1`)
- Running `gitlab-ctl reconfigure` after
- Or reverting the worker join and keeping instances: 1

---

## Reverting (remove node from cluster)

```bash
# On jay1 (drain first so Patroni can failover)
kubectl drain <node-name> --ignore-daemonsets --delete-emptydir-data
kubectl delete node <node-name>

# On 192.168.1.26
/usr/local/bin/k3s-agent-uninstall.sh
```


---

## Cilium connectivity test results (2026-07-29)

Run after jay2 joined the cluster. 42/45 tests passed.

```bash
cilium connectivity test
```

### Passed (42/45)
All core scenarios: pod-to-pod (same node and cross-node), pod-to-service,
DNS resolution, NodePort, health checks, L4 policy enforcement.

### Failed (3/45)

| Test | Cause |
|---|---|
| `pod-to-pod-no-frag` | **MTU**: VXLAN adds ~50B overhead; `ping -M do -s 1422` with DF bit set exceeds MTU and drops. Normal TCP traffic with PMTUD is unaffected. Fix: set `--mtu 1450` in Cilium Helm values. |
| `echo-ingress-l7` | **L7 policy not enabled**: requires Cilium's Envoy proxy for HTTP-aware NetworkPolicy. Not configured in this homelab. |
| `echo-ingress-l7-named-port` | Same as above. |

### Cross-node manual verification

```bash
kubectl run test-jay1 --image=busybox:latest --restart=Never \
  --overrides='{"spec":{"nodeSelector":{"kubernetes.io/hostname":"jay1"}}}' -- sleep 120
kubectl run test-jay2 --image=busybox:latest --restart=Never \
  --overrides='{"spec":{"nodeSelector":{"kubernetes.io/hostname":"jay2"}}}' -- sleep 120

JAY2_IP=$(kubectl get pod test-jay2 -o jsonpath='{.status.podIP}')
kubectl exec test-jay1 -- ping -c 3 $JAY2_IP
# 0% loss, ~0.57ms RTT
# 10.42.0.x (jay1 pod CIDR) → 10.42.1.x (jay2 pod CIDR) via Cilium VXLAN overlay
```
