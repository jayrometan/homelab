# Homelab — Platform Engineering Lab

Personal homelab built to mirror the stack at **MacDonalds** (PaaS / Platform Engineering team). Used for hands-on learning before joining the team.

All infrastructure runs on a **Beelink mini-PC** hosting two Rocky Linux 9.3 VMs.

---

## Infrastructure Overview

```
Beelink (192.168.1.24)
├── jay1 (192.168.1.25) — 14 vCPU, 19GB RAM
│   ├── k3s v1.36.2          Kubernetes cluster
│   ├── Cilium v1.19.5        CNI + kube-proxy replacement (eBPF)
│   └── FluxCD v2.9.0         GitOps operator
│
└── jay2 (192.168.1.26) — 4 vCPU, 3.6GB RAM
    ├── GitLab CE 19.1.1      Self-hosted Git + CI/CD
    └── GitLab Runner          Shell executor, instance runner
```

---

## Repository Structure

```
homelab/
├── README.md                   ← this file
│
├── clusters/
│   └── jay1/
│       └── flux-system/        ← FluxCD bootstrap manifests (auto-generated)
│
├── manifests/                  ← Kubernetes manifests managed by FluxCD
│   └── apps/                   ← drop files here, Flux applies them to k3s
│
├── k3s/                        ← k3s + Cilium setup docs
├── gitlab-ce/                  ← GitLab CE setup docs + runner
├── flux/                       ← FluxCD setup docs
├── stackgres/                  ← StackGres (Postgres operator) deep dive
└── cue/                        ← CUE lang lesson plan + deep dive
```

---

## The GitOps Loop

FluxCD on jay1 watches the **local GitLab** (`http://192.168.1.26/root/homelab.git`) on branch `main`, path `clusters/jay1`. Anything committed under `manifests/` and referenced by a Kustomization is automatically reconciled onto k3s.

```
Write config (CUE or YAML)
        │
        ▼
git push local main  →  GitLab (192.168.1.26)
                               │
                        FluxCD polls every 1 min
                               │
                               ▼
                    k3s cluster (192.168.1.25) reconciles
```

**The shell runner on jay2 never touches the cluster.** Its job is validate + render + commit. The cluster drives itself from Git.

---

## Repo Remotes (on jay1)

```bash
git remote -v
# local   http://192.168.1.26/root/homelab.git  ← FluxCD watches this
# origin  git@github.com:jayrometan/homelab.git ← public backup / notes
```

Push to `local` to trigger GitOps. Push to `origin` to sync notes to GitHub.

---

## Components Installed

### k3s + Cilium (jay1)
- k3s installed **without flannel and without kube-proxy**
- Cilium handles all pod networking (CNI) and all service routing (eBPF kube-proxy replacement)
- No iptables rules — ClusterIP lookups are O(1) eBPF map lookups
- See `k3s/` for full setup notes

### GitLab CE (jay2)
- Omnibus RPM install on Rocky Linux 9.3
- Tuned for single-user low-memory: Puma single mode, Sidekiq concurrency=5, monitoring stack disabled
- Shell runner registered as instance runner (shared across all projects)
- See `gitlab-ce/` for full setup notes and deployment journal

### FluxCD (on k3s)
- Bootstrapped against local GitLab with HTTP token auth
- Watches `clusters/jay1/` path in homelab repo
- Installs: source-controller, kustomize-controller, helm-controller, notification-controller
- See `flux/` for full setup notes

---

## Learning Roadmap (MacDonalds Stack)

| Priority | Topic | Status | Notes |
|----------|-------|--------|-------|
| Tier 1 | CUE lang | In progress | Lesson plan in `cue/` |
| Tier 1 | KubeVela | Not started | Builds on FluxCD |
| Tier 1 | Cilium BGP mode | Not started | Need BGP router |
| Tier 1 | SPIFFE/SPIRE | Not started | |
| Tier 1 | StackGres | Not started | Deep dive in `stackgres/` |
| Tier 2 | FluxCD | Deployed | See `flux/` |
| Tier 2 | Gateway API | Not started | Via Cilium |
| Tier 2 | VictoriaMetrics | Not started | |
| Tier 2 | Redpanda | Not started | |
| Tier 3 | Fluent Bit | Not started | |
| Tier 3 | etcd ops | Not started | |

---

## Quick Reference

```bash
# k3s — on jay1
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
kubectl get nodes
kubectl get pods -A

# Cilium — on jay1
kubectl -n kube-system exec ds/cilium -- cilium status
kubectl -n kube-system exec ds/cilium -- cilium service list

# FluxCD — on jay1
flux get all
flux reconcile source git flux-system
flux logs --follow

# GitLab — on jay2
gitlab-ctl status
gitlab-ctl tail
gitlab-runner status

# Git remotes — on jay1
cd /root/github/homelab
git push local main    # triggers FluxCD
git push origin main   # syncs to GitHub
```
