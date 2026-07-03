# Kustomize & Kustomization — What They Are and How They Work

> **Common confusion:** There are TWO different things called "kustomization" and they're related but distinct. This doc covers both.

---

## The Naming Problem — Clear It Up First

| Name | What it is |
|------|-----------|
| **Kustomize** | A standalone CLI tool (also built into `kubectl`) for customising Kubernetes YAML |
| **`kustomization.yaml`** | The config file that Kustomize reads to know what to do |
| **`Kustomization`** (capital K) | A FluxCD CRD — tells Flux what path to apply to the cluster |

They're related: FluxCD's `Kustomization` CRD runs Kustomize under the hood. But you don't have to use Kustomize features to use Flux's `Kustomization` CRD. Flux can apply plain YAML directories too.

---

## Do You Need Kustomize?

**Short answer: You need FluxCD's `Kustomization` CRD. You don't need to use Kustomize's overlay/patching features.**

When Flux applies a directory of plain YAML files, it still uses a `Kustomization` CRD to define what path to apply — but Kustomize itself just does a pass-through (equivalent to `kubectl apply -f ./directory`).

You only need Kustomize's features (patches, overlays, bases) when you want to:
- Apply the same base manifests to multiple clusters with small differences
- Override specific fields (image tags, replica counts, resource limits) per environment
- Add labels/annotations across a set of manifests without editing each file

If you're starting out, ignore the overlay features. Just use `Kustomization` CRDs to tell Flux what to apply. Add Kustomize patching later when you have a real multi-environment problem to solve.

---

## Part 1 — Kustomize (the tool)

### What Problem It Solves

You have a `Deployment` manifest for your app. You want to deploy it to dev (1 replica, small image) and prod (3 replicas, production image). Without Kustomize you have two options:

- **Copy the file twice** — now you maintain two copies that drift apart
- **Use Helm** — significant overhead for a simple difference

Kustomize's answer: one **base** manifest, multiple **overlays** that patch just the differences.

### Base + Overlay Pattern

```
app/
├── base/
│   ├── kustomization.yaml    ← lists what files to include
│   ├── deployment.yaml
│   └── service.yaml
└── overlays/
    ├── dev/
    │   ├── kustomization.yaml  ← references base, applies dev patches
    │   └── replica-patch.yaml
    └── prod/
        ├── kustomization.yaml  ← references base, applies prod patches
        └── replica-patch.yaml
```

### Base manifests

```yaml
# base/deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
spec:
  replicas: 1
  template:
    spec:
      containers:
      - name: app
        image: my-app:latest
        resources:
          requests:
            memory: "128Mi"
            cpu: "100m"
```

```yaml
# base/kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- deployment.yaml
- service.yaml
```

### Overlay — dev

```yaml
# overlays/dev/kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
bases:
- ../../base             # reference the base

# Add a prefix to all resource names
namePrefix: dev-

# Override the image
images:
- name: my-app
  newTag: dev-latest

# Apply a patch
patches:
- path: replica-patch.yaml
```

```yaml
# overlays/dev/replica-patch.yaml  (strategic merge patch)
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app          # must match the base resource name
spec:
  replicas: 1           # dev: 1 replica
```

### Overlay — prod

```yaml
# overlays/prod/kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
bases:
- ../../base

namePrefix: prod-

images:
- name: my-app
  newTag: v1.2.3        # pinned release tag in prod

patches:
- path: replica-patch.yaml
```

```yaml
# overlays/prod/replica-patch.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
spec:
  replicas: 3           # prod: 3 replicas
  template:
    spec:
      containers:
      - name: app
        resources:
          requests:
            memory: "512Mi"
            cpu: "500m"
          limits:
            memory: "1Gi"
            cpu: "1"
```

### Running Kustomize

```bash
# Preview what dev overlay produces (no apply)
kubectl kustomize overlays/dev

# Apply dev overlay to cluster
kubectl apply -k overlays/dev

# Apply prod overlay
kubectl apply -k overlays/prod

# Using standalone kustomize CLI
kustomize build overlays/prod | kubectl apply -f -
```

### Patch Types

**Strategic Merge Patch** (what we used above)
- Looks like a partial Kubernetes manifest
- Kustomize merges it into the base
- Lists are replaced by default (not merged) — be careful with containers array

**JSON 6902 Patch**
- More surgical — target a specific field by path
- Better for patching nested fields without repeating surrounding structure

```yaml
# JSON 6902 patch example — change replicas only
patches:
- target:
    kind: Deployment
    name: my-app
  patch: |
    - op: replace
      path: /spec/replicas
      value: 3
```

### Kustomize Built-in Transformers

```yaml
# kustomization.yaml features

# Add prefix/suffix to all resource names
namePrefix: prod-
nameSuffix: -v2

# Add labels to ALL resources (great for cost tracking, ownership)
commonLabels:
  team: platform
  environment: prod
  managed-by: flux

# Add annotations to all resources
commonAnnotations:
  contact: platform-team@company.internal

# Override image tags (used by Flux image automation)
images:
- name: my-app
  newName: registry.internal/my-app
  newTag: v1.2.3

# Set namespace for all resources
namespace: production
```

---

## Part 2 — FluxCD's `Kustomization` CRD

This is separate from the Kustomize tool, but uses it under the hood.

### What It Does

A FluxCD `Kustomization` tells Flux:
- Which `GitRepository` (or other source) to read from
- Which **path** in that source to apply
- How often to reconcile
- Whether to **prune** (delete resources removed from Git)
- What other Kustomizations must be ready first (`dependsOn`)

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: apps
  namespace: flux-system
spec:
  interval: 5m               # reconcile every 5 minutes
  path: ./manifests/apps     # apply everything under this path
  prune: true                # delete resources removed from Git
  sourceRef:
    kind: GitRepository
    name: flux-system        # which git source to use
  dependsOn:
    - name: infrastructure   # wait for infrastructure Kustomization first
  timeout: 2m                # fail if apply takes longer than this
  retryInterval: 30s         # retry on failure after this duration
```

### Plain YAML vs Kustomize Overlays With Flux

**Plain YAML directory (no kustomize features):**

```
manifests/
└── apps/
    ├── namespace.yaml
    ├── deployment.yaml
    └── service.yaml
```

Flux applies all three files. No `kustomization.yaml` needed — Flux generates one implicitly.

**With Kustomize overlays:**

```
manifests/
└── apps/
    ├── base/
    │   ├── kustomization.yaml
    │   ├── deployment.yaml
    │   └── service.yaml
    └── overlays/
        ├── dev/
        │   └── kustomization.yaml
        └── prod/
            └── kustomization.yaml
```

Point different cluster Kustomizations at different overlays:

```yaml
# clusters/prod/apps.yaml
spec:
  path: ./manifests/apps/overlays/prod

# clusters/dev/apps.yaml
spec:
  path: ./manifests/apps/overlays/dev
```

Same Git repo, same base manifests, different rendered output per cluster.

### `prune: true` — Understand This Before Enabling

When `prune: true`:
- Flux tracks every resource it has ever applied
- If you remove a file from Git, Flux deletes the corresponding resource from the cluster
- This is usually what you want — Git is the truth

When `prune: false`:
- Removed resources are left in place
- Manual cleanup required
- Safer when migrating existing resources into Flux management

**Warning:** If you rename a Kubernetes resource (change `metadata.name`), Flux with `prune: true` will create the new one AND delete the old one. That's correct behaviour but can cause downtime if not sequenced properly.

### `dependsOn` — Ordering Kustomizations

Flux applies Kustomizations in parallel by default. Use `dependsOn` to enforce order:

```yaml
# infrastructure must be ready before apps
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: infrastructure
spec:
  path: ./infrastructure

---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: apps
spec:
  path: ./apps
  dependsOn:
    - name: infrastructure   # wait for CRDs, namespaces, etc.
```

Critical for:
- Apps that depend on CRDs installed by infrastructure (StackGres CRDs must exist before SGCluster)
- Namespaces that must exist before resources in them
- Secrets that must exist before Deployments that mount them

---

## Part 3 — Practical Lab Guide

### Lab 1 — Plain YAML via Flux (no Kustomize features)

The simplest possible Flux-managed resource.

**Step 1: Create the manifest**

```bash
mkdir -p /root/github/homelab/manifests/apps

cat > /root/github/homelab/manifests/apps/test-namespace.yaml << 'EOF'
apiVersion: v1
kind: Namespace
metadata:
  name: flux-demo
  labels:
    managed-by: flux
EOF
```

**Step 2: Create a Kustomization pointing at it**

```bash
cat > /root/github/homelab/clusters/jay1/apps.yaml << 'EOF'
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: apps
  namespace: flux-system
spec:
  interval: 2m
  path: ./manifests/apps
  prune: true
  sourceRef:
    kind: GitRepository
    name: flux-system
EOF
```

**Step 3: Commit and push to local GitLab (FluxCD watches this)**

```bash
cd /root/github/homelab
git add .
git commit -m "Add flux-demo namespace via GitOps"
git push local main
```

**Step 4: Watch Flux apply it**

```bash
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml

# Force immediate reconciliation instead of waiting up to 1 minute
flux reconcile source git flux-system

# Watch the Kustomization status
flux get kustomizations -w

# Verify the namespace was created
kubectl get ns flux-demo
```

**Step 5: Test prune — delete the file and watch Flux remove the resource**

```bash
rm /root/github/homelab/manifests/apps/test-namespace.yaml
git add . && git commit -m "Remove flux-demo namespace"
git push local main

flux reconcile source git flux-system
kubectl get ns flux-demo  # should be gone within 2 minutes
```

---

### Lab 2 — Kustomize Overlay for Multi-Environment Config

Simulate deploying the same app with different replica counts to "dev" and "prod" paths.

**Directory structure to create:**

```bash
mkdir -p /root/github/homelab/manifests/demo-app/{base,overlays/dev,overlays/prod}
```

**Base manifests:**

```bash
cat > /root/github/homelab/manifests/demo-app/base/deployment.yaml << 'EOF'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: demo-app
  namespace: flux-demo
spec:
  replicas: 1
  selector:
    matchLabels:
      app: demo-app
  template:
    metadata:
      labels:
        app: demo-app
    spec:
      containers:
      - name: app
        image: nginx:alpine
        ports:
        - containerPort: 80
EOF

cat > /root/github/homelab/manifests/demo-app/base/kustomization.yaml << 'EOF'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- deployment.yaml
EOF
```

**Dev overlay (1 replica):**

```bash
cat > /root/github/homelab/manifests/demo-app/overlays/dev/kustomization.yaml << 'EOF'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- ../../base
patches:
- patch: |
    - op: replace
      path: /spec/replicas
      value: 1
  target:
    kind: Deployment
    name: demo-app
commonLabels:
  environment: dev
EOF
```

**Prod overlay (3 replicas):**

```bash
cat > /root/github/homelab/manifests/demo-app/overlays/prod/kustomization.yaml << 'EOF'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- ../../base
patches:
- patch: |
    - op: replace
      path: /spec/replicas
      value: 3
  target:
    kind: Deployment
    name: demo-app
commonLabels:
  environment: prod
EOF
```

**Preview locally before pushing:**

```bash
# See what dev renders to
kubectl kustomize /root/github/homelab/manifests/demo-app/overlays/dev

# See what prod renders to
kubectl kustomize /root/github/homelab/manifests/demo-app/overlays/prod
```

**Point Flux at the dev overlay (since we only have one cluster):**

```bash
cat > /root/github/homelab/clusters/jay1/demo-app.yaml << 'EOF'
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: demo-app
  namespace: flux-system
spec:
  interval: 2m
  path: ./manifests/demo-app/overlays/dev
  prune: true
  sourceRef:
    kind: GitRepository
    name: flux-system
  dependsOn:
    - name: apps    # namespace must exist first
EOF
```

**Push and verify:**

```bash
cd /root/github/homelab
git add . && git commit -m "Add demo-app with kustomize overlays"
git push local main

flux reconcile source git flux-system
kubectl get deploy demo-app -n flux-demo
# Should show 1/1 (dev overlay)
```

---

## How This Fits the CUE Workflow

In Tower's stack, CUE **generates** the Kustomize base manifests. The workflow is:

```
Developer writes CUE config
        │
        ▼
GitLab CI: cue vet (validate)
        │
        ▼
GitLab CI: cue export --out yaml → writes to manifests/apps/base/
        │
        ▼
GitLab CI: git commit manifests/
        │
        ▼
FluxCD picks up new commit
        │
        ▼
Kustomization applies the overlay for this cluster
        │
        ▼
Cluster reaches desired state
```

CUE owns the source of truth. Kustomize handles environment-specific differences. Flux reconciles. Each layer has one job.

---

## Quick Reference

```bash
# Preview what Kustomize produces (no apply)
kubectl kustomize ./overlays/prod
kustomize build ./overlays/prod        # same thing, standalone CLI

# Check Flux Kustomization status
flux get kustomizations -A

# Force reconcile
flux reconcile kustomization apps

# Suspend a Kustomization (stop reconciling temporarily)
flux suspend kustomization apps
flux resume kustomization apps

# See what Flux would change (dry run diff)
flux diff kustomization apps

# Debug: see all resources managed by a Kustomization
kubectl get all -n flux-demo -l "kustomize.toolkit.fluxcd.io/name=apps"
```
