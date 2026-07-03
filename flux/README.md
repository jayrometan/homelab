# FluxCD — Deep Dive & Deployment Notes

Deployed on **k3s cluster at 192.168.1.25 (jay1)**, bootstrapped against **local GitLab at 192.168.1.26 (jay2)**.

FluxCD version: **v2.9.0**

---

## What Is FluxCD

FluxCD is a **GitOps operator for Kubernetes**. It continuously watches a Git repository and reconciles the cluster state to match what's in Git. If you push a new manifest, Flux applies it. If you delete a manifest from Git, Flux removes it from the cluster. The cluster always converges toward Git.

The key principle: **Git is the single source of truth.** Nothing gets applied to the cluster by a human or CI pipeline directly — everything goes through Git first.

---

## GitOps — Pull vs Push

Most people's first instinct for CI/CD is push-based:

```
CI pipeline → kubectl apply → cluster
```

GitOps flips this:

```
CI pipeline → git commit → GitLab
                               ▲
                          Flux polls
                               │
                          Flux applies → cluster
```

**Why pull is better for a platform team:**
- CI pipeline doesn't need cluster credentials — huge security win
- If Flux is down, the cluster doesn't drift — it just doesn't update
- Git history IS the audit log — every cluster change is a commit
- Easy rollback: `git revert`, Flux reconciles the old state
- Multiple clusters can watch the same repo with different paths

**MacDonalds' model:** Platform team owns the GitOps repo. Dev teams open MRs. Flux on each cluster (dev/staging/prod) watches different paths in the same repo.

---

## Architecture

### Core Components

FluxCD installs four controllers into the `flux-system` namespace:

```
┌──────────────────────────────────────────────────────┐
│               flux-system namespace                  │
│                                                      │
│  source-controller        ← watches Git, Helm repos, │
│                             OCI registries           │
│                             fetches and stores       │
│                             artifacts locally        │
│                                                      │
│  kustomize-controller     ← applies Kustomizations   │
│                             (plain YAML or kustomize)│
│                                                      │
│  helm-controller          ← applies HelmReleases     │
│                             (installs/upgrades Helm  │
│                             charts declaratively)    │
│                                                      │
│  notification-controller  ← sends alerts to Slack,  │
│                             Teams, webhooks, etc.    │
└──────────────────────────────────────────────────────┘
```

### Key CRDs

**GitRepository** — defines a Git source Flux watches:
```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: homelab
  namespace: flux-system
spec:
  interval: 1m          # poll every 1 minute
  url: http://192.168.1.26/root/homelab.git
  ref:
    branch: main
  secretRef:
    name: flux-system   # contains git credentials
```

**Kustomization** — tells Flux which path in the repo to apply:
```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: apps
  namespace: flux-system
spec:
  interval: 5m
  path: ./manifests/apps        # apply everything under this path
  prune: true                   # delete resources removed from Git
  sourceRef:
    kind: GitRepository
    name: homelab
```

**HelmRelease** — declarative Helm chart installation:
```yaml
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: cilium
  namespace: kube-system
spec:
  chart:
    spec:
      chart: cilium
      version: "1.19.x"
      sourceRef:
        kind: HelmRepository
        name: cilium-charts
  values:
    kubeProxyReplacement: true
    k8sServiceHost: 192.168.1.25
```

### Reconciliation Loop

```
source-controller
  ├─ polls GitLab every 1 minute
  ├─ if new commit detected: fetches and stores artifact (tarball)
  └─ updates GitRepository status with new revision

kustomize-controller
  ├─ watches GitRepository for new artifacts
  ├─ if new artifact: renders manifests (kustomize or plain YAML)
  ├─ runs server-side apply against k8s API
  └─ if prune=true: garbage-collects removed resources
```

---

## Repository Structure

After bootstrap, Flux created this structure in the homelab repo:

```
homelab/
└── clusters/
    └── jay1/
        └── flux-system/
            ├── gotk-components.yaml    ← all Flux CRDs + controllers
            ├── gotk-sync.yaml          ← GitRepository + Kustomization for flux-system itself
            └── kustomization.yaml      ← kustomize entry point
```

To deploy your own apps via Flux, add manifests under a new path and create a Kustomization pointing at it:

```
homelab/
├── clusters/
│   └── jay1/
│       ├── flux-system/        ← Flux self-manages this
│       └── apps.yaml           ← your Kustomization pointing at manifests/
└── manifests/
    └── apps/
        ├── namespace.yaml
        ├── deployment.yaml
        └── service.yaml
```

---

## Deployment — What We Actually Did

### Step 1 — Install Flux CLI on jay1

```bash
curl -s https://fluxcd.io/install.sh | bash
flux --version
# flux version 2.9.0
```

### Step 2 — Verify prerequisites

```bash
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
flux check --pre
# ✔ Kubernetes 1.36.2+k3s1 >=1.33.0-0
# ✔ prerequisites checks passed
```

### Step 3 — Create the homelab project on local GitLab

The homelab repo existed on GitHub but not on the local GitLab instance. Created it via API:

```bash
curl --request POST 'http://192.168.1.26/api/v4/projects' \
  --header 'PRIVATE-TOKEN: <glpat-token>' \
  --form 'name=homelab' \
  --form 'visibility=private' \
  --form 'initialize_with_readme=false'
```

### Step 4 — Push existing content to local GitLab

```bash
cd /root/github/homelab
git remote add local http://root:<glpat-token>@192.168.1.26/root/homelab.git
git push local main
```

### Step 5 — Bootstrap FluxCD

```bash
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
flux bootstrap git \
  --url=http://192.168.1.26/root/homelab.git \
  --username=root \
  --password='<glpat-token>' \
  --branch=main \
  --path=clusters/jay1 \
  --allow-insecure-http=true \
  --token-auth=true
```

Bootstrap does several things automatically:
1. Clones the repo
2. Generates and commits `clusters/jay1/flux-system/` manifests
3. Pushes them to GitLab
4. Applies the Flux controllers to the cluster
5. Creates a `flux-system` secret with Git credentials
6. Flux immediately reconciles itself from the committed manifests

### Step 6 — Pull Flux-generated manifests back

Flux committed to local GitLab — sync those commits back to GitHub:

```bash
git pull local main
git push origin main
```

---

## Issues Encountered During Deployment

### Issue 1 — HTTP flagged as insecure

```
✗ failed to create authentication options: scheme http is insecure,
  pass --allow-insecure-http=true to allow it
```

**Fix:** Add `--allow-insecure-http=true`. This is expected for a local HTTP-only GitLab — in production you'd have TLS.

### Issue 2 — Password auth rejected, token required

```
✗ authentication required: HTTP Basic: Access denied.
  you're required to use a token instead of a password
```

**Cause:** GitLab 16+ does not accept the root user's login password for Git HTTP operations. A Personal Access Token (PAT) is required.

**Fix:** Use the `glpat-...` token as the `--password` value, not the GitLab UI password.

### Issue 3 — Flux tried to use SSH deploy keys instead of HTTP token

First bootstrap attempt without `--token-auth` generated an SSH key and asked:
```
? Please give the key access to your repository
```
Then aborted waiting for user input.

**Cause:** Flux's default for `git bootstrap` is to generate an SSH deploy key and use that for ongoing pulls. For HTTP-only setups, you must override this.

**Fix:** Add `--token-auth=true`. This tells Flux to use the username/password (PAT) for all Git operations, not SSH.

---

## Current State

```bash
flux get source git
# NAME        REVISION           READY  MESSAGE
# flux-system main@sha1:...      True   stored artifact...

flux get kustomizations
# NAME        REVISION           READY  MESSAGE
# flux-system main@sha1:...      True   Applied revision: main@sha1:...
```

Flux polls `http://192.168.1.26/root/homelab.git` every minute. Any commit to `clusters/jay1/` is automatically applied to k3s.

---

## How to Deploy Something via Flux (the GitOps loop)

```bash
# 1. Create a manifest
cat > /tmp/test-namespace.yaml << EOF
apiVersion: v1
kind: Namespace
metadata:
  name: gitops-test
  labels:
    managed-by: flux
EOF

# 2. Put it somewhere Flux watches
mkdir -p /root/github/homelab/manifests/apps
cp /tmp/test-namespace.yaml /root/github/homelab/manifests/apps/

# 3. Create a Kustomization pointing at it
cat > /root/github/homelab/clusters/jay1/apps.yaml << EOF
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: apps
  namespace: flux-system
spec:
  interval: 5m
  path: ./manifests/apps
  prune: true
  sourceRef:
    kind: GitRepository
    name: flux-system
EOF

# 4. Commit and push to local GitLab (Flux watches this)
cd /root/github/homelab
git add .
git commit -m "Add gitops-test namespace"
git push local main

# 5. Watch Flux pick it up (within 1 minute)
flux get kustomizations -w

# 6. Verify
kubectl get ns gitops-test
```

---

## Integration with the CUE + GitLab CI Workflow

In the full GitOps loop (MacDonalds-style):

```
Developer writes CUE config
          │
          ▼
git push → GitLab repo (feature branch)
          │
          ▼
GitLab CI pipeline (jay2 shell runner):
  stage 1: cue vet ./...            ← validate schema
  stage 2: cue export --out yaml    ← render manifests
  stage 3: git commit manifests/ to main
          │
          ▼ (Flux polls every 1 min)
FluxCD source-controller detects new commit
          │
          ▼
FluxCD kustomize-controller applies manifests
          │
          ▼
k3s cluster reaches desired state
```

The shell runner on jay2 **never runs kubectl**. It only validates, renders, and commits. The cluster drives itself.

---

## Useful Commands

```bash
# Overall health check
flux check

# Watch all sources
flux get source git -A

# Watch all kustomizations
flux get kustomizations -A

# Force immediate reconciliation (don't wait for poll interval)
flux reconcile source git flux-system
flux reconcile kustomization flux-system

# Suspend reconciliation (e.g. during manual emergency fix)
flux suspend kustomization apps

# Resume
flux resume kustomization apps

# See what Flux would apply (dry run)
flux diff kustomization flux-system

# Full event log for debugging
kubectl get events -n flux-system --sort-by='.lastTimestamp'

# Tail Flux controller logs
kubectl logs -n flux-system deploy/source-controller -f
kubectl logs -n flux-system deploy/kustomize-controller -f

# Check a specific resource reconciliation status
kubectl describe gitrepository flux-system -n flux-system
kubectl describe kustomization flux-system -n flux-system
```

---

## Production Sharp Edges

**1. `prune: true` deletes things**
If you set `prune: true` on a Kustomization and remove a file from Git, Flux will delete the corresponding resource from the cluster. This is usually what you want — but be careful when first enabling it on existing resources. It can surprise you if you rename a file.

**2. Flux reconciles in the order you define Kustomizations**
If app A depends on a CRD that app B installs, order matters. Use `dependsOn` in your Kustomization:
```yaml
spec:
  dependsOn:
    - name: crds
```

**3. HTTP in production is a non-starter**
`--allow-insecure-http=true` is lab-only. In production, GitLab must serve HTTPS. Credentials in the `flux-system` secret would be exposed over the wire otherwise.

**4. Bootstrap is idempotent**
You can re-run `flux bootstrap` safely. It checks what's already there and only updates what changed. Useful if you want to add components or change the path.

**5. Flux doesn't manage secrets**
Flux applies whatever is in Git. If you commit a Secret manifest, its value is in Git in plaintext — bad. Use a secrets management solution alongside Flux:
- **SOPS** — encrypts secrets in Git, Flux decrypts at apply time
- **External Secrets Operator** — pulls secrets from Vault/AWS SM at runtime
- **SPIFFE/SPIRE** — eliminates static secrets entirely (MacDonalds' approach for workload auth)

**6. Drift detection vs correction**
Flux detects and corrects drift by re-applying on its interval. But if a human does `kubectl edit` directly, Flux will revert it on the next reconciliation. This is intentional — Git is the truth. Document this clearly for your team.
