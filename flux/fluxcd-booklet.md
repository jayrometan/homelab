# FluxCD — Platform Engineering Deep Dive

> A study booklet for platform and SRE engineers operating Flux as a shared delivery service.
> Covers mental models, production architecture, multi-tenancy, incident workflows, and best practices.
> Written for engineers familiar with Kubernetes, Helm, Kustomize, and CI/CD — not beginners.

---

## Table of Contents

1. [High-Level Overview — FluxCD and the GitOps Model](#1-high-level-overview)
2. [Architecture and Components](#2-architecture-and-components)
3. [How Companies Use Flux in Practice](#3-how-companies-use-flux-in-practice)
4. [Onboarding Product Teams](#4-onboarding-product-teams)
5. [Multi-Tenant and Multi-Cluster Patterns](#5-multi-tenant-and-multi-cluster-patterns)
6. [Access Control, RBAC, and Self-Service](#6-access-control-rbac-and-self-service)
7. [Delivery and Incident Workflows](#7-delivery-and-incident-workflows)
8. [Architecture Evolution Over Time](#8-architecture-evolution-over-time)
9. [Best Practices and Anti-Patterns](#9-best-practices-and-anti-patterns)
10. [Production Checklist](#10-production-checklist)

---

## 1. High-Level Overview

### What Flux Is

FluxCD is a set of Kubernetes controllers that continuously reconcile cluster state from declarative sources — Git repositories, OCI registries, Helm repositories, and S3-compatible buckets. It does not run jobs, it does not push kubectl commands; it watches sources and applies the desired state they describe, forever, on a loop.

The key insight: **Flux is not a deployment tool — it is a state synchronisation engine.** The question it asks every reconciliation cycle is not "what should I deploy?" but "does the cluster match what Git says it should look like? If not, make it so."

### The GitOps Operating Model

**Desired state in version control.** Every resource that should exist in the cluster is represented as YAML in a Git repository. Git becomes the single source of truth — not a CI server, not a person's laptop, not a shared bastion host.

**Pull-based reconciliation.** Flux controllers running inside the cluster pull from Git and apply changes. The cluster reaches out to Git; nothing external pushes into the cluster. This means:
- No inbound firewall rules needed for deployments
- The cluster can be air-gapped as long as controllers can reach the Git/OCI source
- A compromised CI pipeline cannot push arbitrary kubectl commands directly into production

**Drift detection and self-healing.** If someone manually `kubectl edit`s a resource, Flux detects the divergence at the next reconciliation interval and overwrites it with what Git says. The cluster enforces its own desired state continuously, not just at deploy time.

**Audit trail via Git history.** Every change to cluster state is a Git commit with an author, a timestamp, and a diff. The `git log` of your fleet repo IS your deployment history.

### Pull-Based vs Push-Based — The Architectural Difference

```
Push-based (classic CI/CD):
  Git push → CI pipeline triggered → CI runs kubectl/helm → changes applied

Pull-based (Flux / GitOps):
  Git push → CI pipeline builds artifact, commits manifest → Flux detects change → Flux pulls and applies
```

The shift is that the cluster becomes an agent of its own state, not a passive target. The CI pipeline's job ends at producing an artifact and updating a manifest. The cluster takes it from there.

**When push-based still wins:**
- Very simple setups with a single cluster and single team
- Environments where a human must approve between build and deploy (regulated environments with manual gates)
- One-off migration jobs that are not continuously running

**When Flux wins:**
- Multi-cluster, multi-team environments where you cannot give CI systems credentials to every cluster
- Anywhere you need continuous drift enforcement (compliance, security requirements)
- When cluster rebuild time must be minimised (everything re-applies from Git)

### Flux vs ArgoCD — Tradeoffs

| Dimension | Flux | ArgoCD |
|---|---|---|
| Architecture | Separate controllers, composable | Monolithic application server + UI |
| UI | None built-in (observability via metrics/CLI) | Rich web UI — central dashboard |
| Multi-tenancy | Controller-level flags, namespace isolation | Projects and AppProjects CRDs |
| OCI artifacts | First-class OCIRepository source | Supported but less native |
| Image automation | Built-in controllers | Requires external tooling |
| Extensibility | Composable — each controller is independent | Plugin system, custom plugins |
| Bootstrap | `flux bootstrap` installs itself from Git | `argocd install` pushed by CI |
| Helm drift | helm-controller detects and corrects | Compares manifests, optional sync |
| Who chooses it | Platform teams preferring GitOps purity and composability | Teams wanting a UI and centralised visibility |

Neither is universally better. Flux is more "gitops-native" and composable; ArgoCD gives you visibility out of the box. Many large orgs run both — Flux for infrastructure, ArgoCD for application delivery — though this creates operational overhead.

### Flux vs Plain `kubectl apply` in CI

A common starting point: GitLab CI pipeline that `kubectl apply -f manifests/` after every merge. Why move away from this?

- **Credentials sprawl.** Every CI pipeline needs a kubeconfig or service account token with apply permissions. As the number of clusters grows, rotating and auditing these becomes painful.
- **No drift protection.** Manual changes in the cluster go undetected until the next pipeline run.
- **No self-healing.** A deleted deployment stays deleted until someone re-runs the pipeline.
- **Hard to recover from.** If the cluster is wiped, you need to re-run all pipelines in the right order, with the right context.

Flux solves all four — at the cost of adding controller infrastructure to manage.

### Where Flux Fits in the Delivery Stack

```
Developer commits code
        │
        ▼
GitLab CI builds image, runs tests, pushes to registry
        │
        ▼
CI updates image tag in manifests repo (or pushes OCI artifact)
        │
        ▼
Flux source-controller detects change in Git/OCI
        │
        ▼
Flux kustomize-controller or helm-controller applies to cluster
        │
        ▼
Flux notification-controller sends result to Slack/PagerDuty
```

**Flux does not replace CI.** CI still builds, tests, and produces artifacts. Flux consumes those artifacts and applies them to clusters. The boundary: CI produces, Flux delivers.

In a mature platform with KubeVela or an internal platform layer on top of Flux, the developer may never see Flux at all — they interact with a higher-level abstraction (Application CRDs, a portal, CUE-rendered configs) and the platform layer generates the Flux resources underneath.

---

## 2. Architecture and Components

### The Five Controller Groups

Flux is not one binary — it is a set of independently deployable controllers. Each owns a distinct responsibility and exposes its own CRDs.

#### source-controller

Responsible for fetching, verifying, and packaging artifacts from external sources. It is the only controller that talks to the outside world (Git providers, OCI registries, Helm repos, S3 buckets).

**CRDs it owns:** `GitRepository`, `OCIRepository`, `HelmRepository`, `HelmChart`, `Bucket`

**What it does:**
- Polls or receives webhook triggers to check for new source revisions
- Downloads the source content and stores it as a local artifact (tarball) on a PVC
- Exposes the artifact via an in-cluster HTTP server for other controllers to consume
- Verifies GPG/cosign signatures if configured
- Handles authentication: SSH keys, tokens, TLS certs, OIDC

**Key mental model:** source-controller is the gatekeeper. No other controller touches external systems — they all consume artifacts that source-controller has already fetched, verified, and made available locally. If source-controller is down, no new changes apply, but what's already running continues running.

```yaml
# Platform-owned: watches the fleet repo
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: fleet-repo
  namespace: flux-system
spec:
  interval: 1m             # poll every minute; webhook-driven setups may use longer intervals
  url: https://gitlab.internal/platform/fleet.git
  ref:
    branch: main
  secretRef:
    name: fleet-repo-auth  # contains username/password or SSH key
```

```yaml
# App-team-owned: watches an OCI registry for pre-rendered manifests
apiVersion: source.toolkit.fluxcd.io/v1beta2
kind: OCIRepository
metadata:
  name: payments-app
  namespace: payments
spec:
  interval: 5m
  url: oci://registry.internal/platform/payments-manifests
  ref:
    tag: latest            # or semver: ">=1.0.0"
  verify:
    provider: cosign       # validate artifact signature before applying
```

#### kustomize-controller

Consumes artifacts from source-controller, renders them through Kustomize (or applies plain YAML), and applies the resulting manifests to the cluster. It is the reconciliation engine for GitOps workflows that do not use Helm.

**CRDs it owns:** `Kustomization` (note: FluxCD CRD, not the `kustomize.config.k8s.io/v1beta1` tool config)

**What it does:**
- Builds the Kustomize overlay (runs `kustomize build`) or applies plain YAML
- Validates manifests (dry-run against the API server)
- Applies with server-side apply
- Prunes resources that were removed from Git (garbage collection)
- Evaluates health checks on applied resources
- Respects `dependsOn` ordering — waits for dependencies to be Ready before applying

```yaml
# Platform-owned: applies infrastructure components from fleet repo
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: infrastructure
  namespace: flux-system
spec:
  interval: 10m
  sourceRef:
    kind: GitRepository
    name: fleet-repo
  path: ./infrastructure/controllers    # which path in the repo to apply
  prune: true                           # delete resources removed from Git
  wait: true                            # wait for all resources to become Ready
  timeout: 5m
  dependsOn:
    - name: crds                        # apply CRDs before controllers
```

```yaml
# App-team-owned: applies the payments service from the team's repo
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: payments
  namespace: payments                   # scoped to the team's namespace
spec:
  interval: 5m
  sourceRef:
    kind: GitRepository
    name: payments-repo
    namespace: payments
  path: ./deploy/prod
  prune: true
  serviceAccountName: payments-reconciler  # impersonate this SA — limits blast radius
  healthChecks:
    - apiVersion: apps/v1
      kind: Deployment
      name: payments-api
      namespace: payments
```

#### helm-controller

Manages Helm releases declaratively. It consumes `HelmChart` artifacts produced by source-controller and handles the full Helm lifecycle: install, upgrade, test, rollback, uninstall.

**CRDs it owns:** `HelmRelease`

**What it does:**
- Renders the Helm chart with the specified values
- Detects drift between the last-applied release and the live cluster state
- On drift: re-applies the chart (corrects manual changes)
- On upgrade failure: rolls back to last successful revision automatically (if configured)
- Manages the Helm release secret (stores chart history)

```yaml
# App-team-owned: deploys a service via Helm
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: payments-api
  namespace: payments
spec:
  interval: 15m
  chart:
    spec:
      chart: payments-api
      version: ">=2.0.0 <3.0.0"        # semver constraint
      sourceRef:
        kind: HelmRepository
        name: internal-charts
        namespace: flux-system
  values:
    replicaCount: 3
    image:
      tag: "v2.4.1"
  install:
    remediation:
      retries: 3
  upgrade:
    remediation:
      retries: 3
      remediateLastFailure: true        # roll back if upgrade fails
    cleanupOnFail: true
  rollback:
    timeout: 5m
    cleanupOnFail: true
```

**Key mental model:** helm-controller does not run `helm install` imperatively. It manages a desired release state — the HelmRelease spec IS the desired state. If you manually run `helm rollback`, helm-controller will undo it at the next reconciliation. All changes go through the HelmRelease spec in Git.

#### notification-controller

Handles all event routing: sending Flux events out to external systems and receiving webhooks from external systems to trigger reconciliation.

**CRDs it owns:** `Alert`, `Provider`, `Receiver`

**Outbound (Alerts + Providers):** Flux emits events when reconciliations succeed, fail, or are suspended. The notification-controller routes these to Slack, PagerDuty, Teams, GitHub commit statuses, or any webhook endpoint.

```yaml
# Platform-owned: Slack provider
apiVersion: notification.toolkit.fluxcd.io/v1beta3
kind: Provider
metadata:
  name: slack-platform
  namespace: flux-system
spec:
  type: slack
  channel: "#platform-alerts"
  secretRef:
    name: slack-webhook-url

---
# App-team-owned: alert for their namespace
apiVersion: notification.toolkit.fluxcd.io/v1beta3
kind: Alert
metadata:
  name: payments-alerts
  namespace: payments
spec:
  providerRef:
    name: slack-payments          # team's own Slack provider
  eventSeverity: error
  eventSources:
    - kind: Kustomization
      name: payments
    - kind: HelmRelease
      name: payments-api
```

**Inbound (Receivers):** Flux can expose a webhook endpoint that Git providers call on push events, triggering an immediate reconciliation rather than waiting for the poll interval. This gives push-latency with pull-security.

```yaml
# Platform-owned: GitLab webhook receiver
apiVersion: notification.toolkit.fluxcd.io/v1
kind: Receiver
metadata:
  name: gitlab-receiver
  namespace: flux-system
spec:
  type: gitlab
  secretRef:
    name: receiver-token          # shared secret with GitLab webhook config
  resources:
    - kind: GitRepository
      name: fleet-repo
```

#### image-reflector-controller and image-automation-controller

These two controllers work together to automate image tag updates in Git — the "continuous deployment" use case where a new image pushed to a registry automatically triggers a Git commit and reconciliation.

**CRDs:** `ImageRepository`, `ImagePolicy` (reflector) and `ImageUpdateAutomation` (automation)

**How it works:**
1. `ImageRepository` scans a container registry for available tags
2. `ImagePolicy` selects the latest tag matching a policy (semver, alphabetical, regex)
3. `ImageUpdateAutomation` commits the selected tag back to Git by updating marker comments in YAML files

```yaml
# Platform-owned: scan the registry
apiVersion: image.toolkit.fluxcd.io/v1beta2
kind: ImageRepository
metadata:
  name: payments-api
  namespace: payments
spec:
  image: registry.internal/payments/api
  interval: 5m

---
# App-team-owned: policy for which tag to use
apiVersion: image.toolkit.fluxcd.io/v1beta2
kind: ImagePolicy
metadata:
  name: payments-api
  namespace: payments
spec:
  imageRepositoryRef:
    name: payments-api
  policy:
    semver:
      range: ">=2.0.0 <3.0.0"    # only auto-promote patch/minor within major 2

---
# In the YAML file being updated — the marker comment:
# image: registry.internal/payments/api:v2.4.0 # {"$imagepolicy": "payments:payments-api"}
```

**When to use image automation:** Dev and staging environments where you want every merged image auto-deployed. Production environments typically use explicit tag promotion via MR instead — human approval over tag changes.

### The Reconciliation Loop — Mechanics

Every Flux controller runs an independent reconciliation loop for each resource it manages:

```
1. Fetch source artifact (if GitRepository/OCIRepository is Ready)
2. Render manifests (kustomize build or helm template)
3. Diff against cluster state (server-side apply --dry-run)
4. Apply changes (server-side apply)
5. Prune deleted resources (if prune: true)
6. Evaluate health checks
7. Update resource status conditions
8. Emit events (picked up by notification-controller)
9. Schedule next reconciliation at interval
```

**Key fields that control this loop:**

| Field | Effect |
|---|---|
| `interval` | How often to reconcile even if source has not changed |
| `timeout` | How long before the reconciliation is marked as failed |
| `retryInterval` | How long to wait before retrying after a failure |
| `prune: true` | Delete resources from the cluster when removed from Git |
| `wait: true` | Block completion until all applied resources are Ready |
| `dependsOn` | Do not start reconciling until named Kustomizations are Ready |
| `suspend: true` | Pause reconciliation entirely (manual intervention mode) |

**Force reconciliation without waiting for the interval:**
```bash
flux reconcile kustomization payments --with-source
```

**Suspend during a manual intervention:**
```bash
flux suspend kustomization payments
# ... do manual work ...
flux resume kustomization payments
```

### Bootstrap — The Trust Anchor

`flux bootstrap` is the command that installs Flux into a cluster and configures it to manage itself from a Git repository. After bootstrapping:

- Flux controllers run as Deployments in `flux-system` namespace
- A `GitRepository` named `flux-system` points to the bootstrap repo
- A root `Kustomization` named `flux-system` applies everything under `clusters/<cluster-name>/` — including the Flux controllers themselves

This means Flux upgrades are done by updating the Flux manifests in Git. The controllers update themselves. The cluster's Git repo is the single source of truth for everything, including the control plane.

**The root Kustomization is the trust anchor.** Everything else the cluster runs is, directly or indirectly, applied by a chain of Kustomizations rooted there. If you understand what the root Kustomization points to, you understand what the cluster's intended state is.

### Team Responsibility Mapping

```
┌─────────────────────────────────────────────────────────────┐
│  PLATFORM TEAM owns:                                        │
│                                                             │
│  • Flux controller deployment and upgrades                  │
│  • Bootstrap configuration and fleet repo                   │
│  • Cluster-level GitRepositories and HelmRepositories       │
│  • Tenancy model: namespaces, RBAC, service accounts        │
│  • SOPS/age secret decryption infrastructure                │
│  • Notification providers (Slack/PagerDuty endpoints)       │
│  • Root and infrastructure Kustomizations                   │
│  • Controller resource limits and monitoring                │
└─────────────────────────────────────────────────────────────┘
                          │ provides
                          ▼
┌─────────────────────────────────────────────────────────────┐
│  PRODUCT TEAMS own:                                         │
│                                                             │
│  • Their app repo (manifests, Helm values, overlays)        │
│  • Their Kustomization/HelmRelease specs in fleet repo      │
│  • Their image update policies                              │
│  • Their alert routing to their channels                    │
│  • Health check definitions for their workloads             │
└─────────────────────────────────────────────────────────────┘
```

---

## 3. How Companies Use Flux in Practice

### Repository Layout Patterns

#### Monorepo (Fleet Repo)

One repository contains everything: Flux bootstrap manifests, infrastructure components, and all app-team configurations. Teams open MRs to this repo to ship changes.

```
fleet-repo/
├── clusters/
│   ├── prod/
│   │   ├── flux-system/          # bootstrap manifests (auto-generated)
│   │   ├── infrastructure.yaml   # Kustomization pointing to infrastructure/
│   │   └── tenants.yaml          # Kustomization pointing to tenants/
│   └── staging/
│       └── ...
├── infrastructure/
│   ├── controllers/              # cert-manager, external-dns, etc.
│   ├── configs/                  # cluster-wide config (RBAC, storage classes)
│   └── monitoring/               # Prometheus, Grafana
└── tenants/
    ├── payments/
    │   ├── namespace.yaml
    │   ├── rbac.yaml
    │   └── kustomization.yaml    # points to payments team's app repo
    └── risk/
        └── ...
```

**Pros:** Single MR review process, visibility across all teams, easy to enforce standards via CI checks.
**Cons:** Repo becomes a bottleneck as teams grow; merge conflicts on shared files; one bad commit can affect everyone.

#### Polyrepo (Per-Team App Repos)

The fleet repo contains only platform config and Kustomizations that reference external app repos. Each team owns their app manifests independently.

```
fleet-repo/               (platform-owned, restricted)
├── clusters/prod/
│   ├── flux-system/
│   ├── infrastructure.yaml
│   └── tenants/
│       ├── payments.yaml   # GitRepository + Kustomization pointing to payments-repo
│       └── risk.yaml       # GitRepository + Kustomization pointing to risk-repo

payments-repo/            (payments team-owned)
├── deploy/
│   ├── base/
│   │   ├── deployment.yaml
│   │   └── kustomization.yaml
│   ├── staging/
│   │   └── kustomization.yaml   # patches for staging
│   └── prod/
│       └── kustomization.yaml   # patches for prod

risk-repo/                (risk team-owned)
└── ...
```

**Pros:** Teams are independent; fleet repo stays small; clear ownership boundaries.
**Cons:** Platform team must be involved any time a team needs a new source registered; harder to enforce cross-team standards.

#### Hybrid (Most Common at Scale)

Platform infrastructure is in a fleet repo. App teams have their own repos and register them via a tenant onboarding MR to the fleet repo. The fleet repo contains the "connector" (GitRepository + Kustomization) but not the app manifests themselves.

### Environment Promotion Strategies

**Directory-per-environment (recommended):**

```
payments-repo/
├── deploy/
│   ├── base/          # shared config
│   ├── staging/       # overlays for staging (lower replicas, staging secrets)
│   └── prod/          # overlays for prod
```

Flux has one Kustomization per environment, each pointing to the appropriate directory:
```yaml
# staging Kustomization points to deploy/staging
# prod Kustomization points to deploy/prod
```

Promotion = copy the image tag (or values change) from staging to prod via an MR. The change goes through code review before Flux applies it to prod.

**OCI artifact tags:**

CI renders the manifests (e.g. `cue export | kubectl kustomize`) and pushes the result to an OCI registry with a semver tag. Flux's `OCIRepository` watches for new tags. Promotion = updating the semver constraint or pinned tag in the Kustomization/HelmRelease.

```yaml
# staging: track latest
ref:
  semver: ">=0.0.0"

# prod: pin to a specific tag after human review
ref:
  tag: "v2.4.1"
```

**Branch-per-environment (anti-pattern):**

`staging` branch → staging cluster. `main` branch → prod. Promotion via cherry-pick or merge.

Avoid this. Branch divergence grows over time, cherry-picks are error-prone, and you lose the ability to compare environments as diffs against a common base.

### The CI/CD Split

The clearest mental model for what belongs where:

| Responsibility | CI (GitLab CI) | Flux |
|---|---|---|
| Run unit tests | ✓ | ✗ |
| Build container image | ✓ | ✗ |
| Push image to registry | ✓ | ✗ |
| Run linting and schema validation | ✓ | ✗ |
| Render manifests (CUE/Helm template) | ✓ (preferred) | Sometimes |
| Push rendered manifests or OCI artifact | ✓ | ✗ |
| Apply manifests to cluster | ✗ | ✓ |
| Monitor rollout health | ✗ | ✓ |
| Roll back on failure | ✗ | ✓ |
| Enforce desired state continuously | ✗ | ✓ |
| Send deployment notification | ✗ | ✓ |

CI ends when the artifact exists and the manifest is updated. Flux takes it from there.

**Why do rendering in CI rather than Flux-side?**

When using CUE (as in your target environment), the rendering is complex — it involves schema validation, constraint checking, and transformation that is not a simple `kustomize build`. Running this in CI means:
- Errors surface in the MR pipeline, not silently in the cluster
- The rendered output can be diffed and reviewed before merge
- The cluster gets pre-validated YAML, not raw CUE that Flux would have to understand

This is the OCI artifact pattern: CI renders CUE → YAML, packages as an OCI artifact, pushes to registry, Flux applies the artifact.

### Flux as a Substrate for KubeVela

When KubeVela (OAM) sits above Flux, the division of labour is:

```
Developer writes:           Application CRD (OAM)
KubeVela renders to:        Kubernetes manifests / HelmRelease / Kustomization
Flux applies:               The rendered resources to the cluster
```

**What the platform team still owns:**
- Flux controllers and bootstrap (KubeVela does not manage these)
- The GitRepository/OCIRepository sources that Flux watches
- Tenancy model: which namespaces Flux can write to, under which service accounts
- Notification routing and monitoring of reconciliation health
- SOPS/secrets infrastructure

**What "invisible Flux" means for app teams:** They never create GitRepository or Kustomization resources. KubeVela generates them. But the platform team's Flux RBAC model still controls the blast radius of what KubeVela-generated resources can do. Understanding Flux's behaviour is still essential for the platform team even if app teams never touch it.

### Real-World Scenarios

#### Scenario A: 10 Product Teams, 3 Clusters, One Fleet Repo

**Setup:**
- Fleet repo: `gitlab.internal/platform/fleet`
- Clusters: `prod`, `staging`, `dev`
- Each team has their own app repo
- Platform team owns fleet repo with protected `main` branch; teams open MRs to add their tenant entry

**Repo structure:**
```
fleet/
├── clusters/
│   ├── prod/
│   │   ├── flux-system/
│   │   ├── infrastructure.yaml
│   │   └── tenants/
│   │       ├── payments.yaml      # GitRepository + Kustomization for payments
│   │       ├── risk.yaml
│   │       └── ...10 teams...
│   ├── staging/
│   └── dev/
├── infrastructure/
│   ├── prod/
│   ├── staging/
│   └── dev/
└── tenants/
    └── _template/                  # template for new team onboarding
```

**How a new team ships:**
1. Team creates their app repo with base/staging/prod overlays
2. Team opens MR to fleet repo adding `tenants/<team>.yaml` (GitRepository + Kustomization)
3. Platform reviews and approves: checks RBAC, namespace, impersonation SA
4. Merge → Flux picks it up → team's namespace and reconciliation loop created
5. Team pushes to their app repo → Flux picks it up → deployed

**Responsibilities:**
- Platform: fleet repo review/merge, namespace provisioning, RBAC for the SA, secrets infra
- Team: their app repo, Helm values, overlays, image tags, alert routing for their services

#### Scenario B: Migrating from CI-Push kubectl to Flux Pull

**Current state:** GitLab CI pipelines with `kubectl apply -f manifests/` at the end. Multiple teams have kubeconfigs stored as CI variables.

**Migration path:**
1. Install Flux into each cluster without changing existing deployments
2. Migrate one low-risk service: move its manifests to a Git path, create a Kustomization pointing there — but mark the CI pipeline step as a no-op for that service
3. Verify Flux reconciles correctly for several days
4. Revoke the CI kubeconfig for that service
5. Repeat per service in order of risk (lowest first)
6. After all services migrated, revoke all CI kubeconfigs

**Common friction points:**
- Secrets: manifests in Git must have secrets encrypted (SOPS/age) or externalised (ESO). Teams may have been committing secrets to CI variables or mounted files.
- State drift: the first time Flux reconciles, it may prune resources that exist in the cluster but not in Git. Audit before enabling `prune: true`.
- Mindset: developers used to "deploy on merge" must now trust the reconciliation loop. Observability (flux CLI, metrics, alerts) is critical to build that trust.

---

## 4. Onboarding Product Teams

### What Teams Need to Know

A platform team running Flux as a shared service must be able to hand a new product team the following without that team needing to understand Flux deeply:

1. **Where to put their manifests** — repo, directory structure, naming conventions
2. **How to trigger a deploy** — push to their repo, open MR to fleet repo, or update an image tag
3. **How to observe a deploy** — what commands to run, what Slack channel to watch
4. **What to do when it goes wrong** — how to check status, how to roll back, when to escalate

### The Repo Layout Contract

Publish this as an internal standard. Every app team's repo must follow it:

```
<team>-repo/
├── deploy/
│   ├── base/
│   │   ├── kustomization.yaml    # lists all resources
│   │   ├── deployment.yaml
│   │   ├── service.yaml
│   │   └── configmap.yaml
│   ├── staging/
│   │   ├── kustomization.yaml    # extends base, patches for staging
│   │   └── patches/
│   │       └── replicas.yaml
│   └── prod/
│       ├── kustomization.yaml    # extends base, patches for prod
│       └── patches/
│           └── replicas.yaml
└── helmrelease/                  # alternative: just a HelmRelease values file
    ├── staging-values.yaml
    └── prod-values.yaml
```

**Naming conventions to enforce via CI:**
- Namespace must match team name: `payments`, not `payments-service-ns-v2`
- All resources must have `app.kubernetes.io/name` and `app.kubernetes.io/part-of` labels
- Health checks must be defined for all Deployments
- `prune: true` must be set on all Kustomizations (enforced by template)

### How Teams Get Access

**What the platform creates per tenant:**

```yaml
# 1. Namespace
apiVersion: v1
kind: Namespace
metadata:
  name: payments
  labels:
    flux-tenant: "true"
    team: payments

---
# 2. Reconciler service account (Flux impersonates this)
apiVersion: v1
kind: ServiceAccount
metadata:
  name: payments-reconciler
  namespace: payments

---
# 3. RBAC — team can only touch their namespace
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: payments-reconciler
  namespace: payments
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin              # within the namespace only
subjects:
  - kind: ServiceAccount
    name: payments-reconciler
    namespace: payments

---
# 4. GitRepository pointing to team's repo (platform creates, team provides URL)
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: payments-repo
  namespace: payments
spec:
  interval: 1m
  url: https://gitlab.internal/payments-team/payments-app.git
  ref:
    branch: main
  secretRef:
    name: payments-git-auth       # deploy key, created by platform

---
# 5. Kustomization — applies team's manifests under their SA
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: payments
  namespace: payments
spec:
  interval: 5m
  sourceRef:
    kind: GitRepository
    name: payments-repo
  path: ./deploy/prod
  prune: true
  serviceAccountName: payments-reconciler   # impersonation — key for multi-tenancy
  healthChecks:
    - apiVersion: apps/v1
      kind: Deployment
      name: payments-api
      namespace: payments
```

### The Onboarding MR Template

When a new team onboards, they (or the platform team) open an MR to the fleet repo with:

```
fleet-repo/tenants/payments/
├── namespace.yaml        # Namespace definition
├── rbac.yaml             # ServiceAccount + RoleBinding for reconciler SA
├── source.yaml           # GitRepository or OCIRepository
└── kustomization.yaml    # Kustomization referencing source
```

The MR is reviewed by the platform team for:
- [ ] Namespace name follows conventions
- [ ] `serviceAccountName` set to the impersonation SA (not omitted)
- [ ] No `ClusterRole` or `ClusterRoleBinding` requested (namespace-scoped only)
- [ ] `prune: true` set
- [ ] Health checks defined
- [ ] No cross-namespace `sourceRef` (if `--no-cross-namespace-refs` enforced)

### Onboarding Checklist

**Platform team actions:**
- [ ] Provision namespace and apply RBAC
- [ ] Generate Git deploy key, add to team's repo, store secret in cluster
- [ ] Add tenant entry to fleet repo (GitRepository + Kustomization)
- [ ] Provision SOPS/age key for team if they handle their own secrets
- [ ] Add team's Slack webhook to notification provider
- [ ] Create Alert resource for team's Kustomization/HelmRelease

**Product team actions:**
- [ ] Create deploy directory with base/staging/prod overlays
- [ ] Add health check annotations to Deployments
- [ ] Define `readinessProbe` and `livenessProbe` (prerequisite for health checks)
- [ ] Verify manifests render locally: `kustomize build deploy/prod`
- [ ] Confirm notification channel is configured and receiving events
- [ ] Test a deploy by pushing a change and watching `flux get kustomizations -n payments`

### Making the Paved Road Faster than kubectl

The platform team must make using Flux easier than `kubectl apply` from a laptop. Tactics:

- **Template repos:** Provide a Cookiecutter or repo template with the correct directory layout pre-built. New teams start from the template, not from scratch.
- **Onboarding CLI:** A wrapper script (`platform-onboard --team=payments --repo=https://...`) that creates the namespace, SA, deploy key, and opens the MR automatically.
- **Instant feedback:** Set up webhook receivers so reconciliation starts within seconds of a push, not minutes. Teams who wait 5 minutes for deploys revert to kubectl.
- **Observability from day one:** Every team gets a Grafana dashboard showing their Kustomization reconciliation history. If they can see it's working, they trust it.
- **Clear escalation path:** Documented runbook for "my deploy is stuck" — check status, check logs, escalate to #platform-support.

### Preventing GitOps Chaos

The most common ways GitOps discipline breaks down:

- **Unowned Kustomizations.** Flux applies things nobody remembers creating. Enforce: every Kustomization must have `team` and `owner` labels, audited by a weekly CI job.
- **Snowflake repos.** Team uses a directory structure nobody else uses, making cross-team PRs impossible to review. Enforce: repo layout CI check on every app repo.
- **Manual kubectl drift.** Someone applies a hotfix with kubectl and never puts it in Git. Enforce: `prune: true` everywhere, and alert on reconciliation overwrites. Treat overwrite events as incidents.
- **Suspended-forever resources.** A Kustomization was suspended during an incident and never resumed. Enforce: weekly audit of all suspended resources; alert if suspended for >24 hours.

---

## 5. Multi-Tenant and Multi-Cluster Patterns

### What "Tenant" Means

In Flux contexts, a tenant is the unit of isolation. It can be:
- A **team** (payments team, risk team)
- A **product** (trading platform, internal tooling)
- An **environment** (dev, staging, prod)
- A **cluster** (each cluster is its own tenant in a fleet)

The correct grain depends on your blast-radius requirements. Most orgs use "team" as the primary tenant grain.

### Flux's Multi-Tenancy Building Blocks

**`serviceAccountName` on Kustomization/HelmRelease — the primary isolation mechanism**

When a Kustomization has `serviceAccountName: payments-reconciler`, the kustomize-controller impersonates that SA when applying manifests. The SA's RBAC determines what can be applied. A tenant repo that tries to create a `ClusterRoleBinding` will be rejected if the SA only has namespace-level permissions.

**`--no-cross-namespace-refs` controller flag**

Prevents Kustomizations from referencing sources in other namespaces. Without this flag, a tenant Kustomization in `payments` could reference a `GitRepository` in `flux-system` (the platform namespace), effectively inheriting whatever that source pulls. This flag is **critical** in shared clusters.

**`--default-service-account` controller flag**

If a Kustomization does not specify `serviceAccountName`, uses this SA. Useful to ensure un-configured tenants still run with minimal permissions rather than the controller's own SA.

**Namespace-scoped sources**

Sources (GitRepository, OCIRepository) scoped to a tenant namespace are only accessible to Kustomizations in that namespace (when `--no-cross-namespace-refs` is enforced).

### Pattern 1 — Shared Cluster, Shared Flux, Per-Tenant Namespaces (Standard)

The most common production model.

```
Cluster: prod
├── flux-system/          (platform-owned, restricted)
│   ├── flux controllers
│   ├── fleet GitRepository
│   └── root Kustomization
├── payments/             (payments team's namespace)
│   ├── payments-repo GitRepository
│   ├── payments Kustomization (serviceAccountName: payments-reconciler)
│   └── app workloads
└── risk/
    ├── risk-repo GitRepository
    ├── risk Kustomization (serviceAccountName: risk-reconciler)
    └── app workloads
```

**Isolation level:**
- A compromised tenant repo can only create/modify resources in its namespace
- Cannot read other tenants' Secrets (namespace isolation)
- Cannot create ClusterRole, ClusterRoleBinding, or modify the Flux controllers
- Cannot reference other tenants' sources

**Blast radius of a bad tenant manifest:** Limited to that team's namespace. Other tenants and infrastructure unaffected.

**Blast radius of fleet repo compromise:** Full cluster access — everything is applied from there. Protect the fleet repo as if it were production infrastructure.

**Operational complexity:** Low to medium. Platform manages one set of controllers, multi-tenancy is enforced at the RBAC layer.

### Pattern 2 — Shared Cluster, Flux Per Tenant (Rare)

Each tenant runs their own copy of the Flux controllers in their own namespace.

**When this makes sense:**
- Tenants need to manage the Flux version they use independently
- Regulatory requirements that controllers for tenant A cannot be operated by the same team as tenant B
- Extreme isolation requirements

**Costs:** Much higher operational overhead — N copies of controllers to upgrade, monitor, and support. Resource overhead. Not recommended unless isolation requirements are extreme.

### Pattern 3 — Fleet of Clusters (Hub Repo)

One fleet repo defines the desired state for multiple clusters. Each cluster's bootstrap points to the same repo but a different path.

```
fleet-repo/
├── clusters/
│   ├── prod-us-east/
│   │   ├── flux-system/        # Flux manages itself here
│   │   ├── infrastructure.yaml
│   │   └── tenants/
│   ├── prod-eu-west/
│   ├── staging/
│   └── dev/
├── infrastructure/
│   ├── base/                   # common to all clusters
│   ├── prod/                   # prod-specific overlays
│   └── staging/                # staging-specific overlays
└── tenants/
    ├── base/                   # common tenant configs
    └── per-cluster/            # cluster-specific overrides
```

Each cluster's root Kustomization points to `clusters/<cluster-name>/` — Kustomize overlays handle per-cluster differences.

**Scaling the fleet:**
- Adding a new cluster: bootstrap it, point it at `clusters/<new-cluster>/` in the fleet repo, create that directory
- Adding a new app to all clusters: add to `tenants/base/`, it propagates everywhere
- Cluster-specific config: override in `tenants/per-cluster/<cluster-name>/`

**Blast radius in a fleet model:**
- A bad commit to `infrastructure/base/` affects all clusters simultaneously
- Use separate fleet repos for prod and non-prod to prevent a staging breakage from affecting prod bootstrap
- Or use a separate protected branch/tag for prod promotion

### Isolation Analysis — What Can a Compromised Tenant Do?

```
Tenant repo is compromised, attacker controls what Flux applies:

WITH impersonation SA (properly configured):
  ✓ Can deploy any resource in team's namespace
  ✓ Can create Services, ConfigMaps, Secrets (within namespace)
  ✗ Cannot create ClusterRole or ClusterRoleBinding
  ✗ Cannot modify other namespaces
  ✗ Cannot modify flux-system
  ✗ Cannot access other tenants' Secrets
  ✗ Cannot create PersistentVolumes (cluster-scoped)

WITHOUT impersonation SA (misconfigured — Flux uses its own SA):
  ✗ Can create any resource the kustomize-controller SA can create
  ✗ kustomize-controller often has cluster-admin — full cluster compromise
```

**This is why `serviceAccountName` is mandatory for every tenant Kustomization.**

### Failure Blast Radius Analysis

| Failure | What breaks | What keeps running |
|---|---|---|
| source-controller down | No new changes pulled. Reconciliation stalls. | Existing running pods continue. Last-applied state persists. |
| Fleet repo unreachable | No new fleet-level changes. | Existing pods and tenant apps continue. |
| One tenant repo unreachable | Only that tenant's Kustomization fails to reconcile. | All other tenants unaffected. |
| Bad commit to fleet infra | Could affect all cluster infrastructure on next reconcile. | Previous state runs until reconciled. |
| Bad commit to tenant repo | Only that tenant's resources are affected. | All other tenants unaffected. |
| kustomize-controller OOM crash | Reconciliation loops stop until pod restarts. | Running pods continue. |

**Key insight:** The running cluster state does not depend on Flux being healthy. Flux manages transitions, not ongoing operation. A Flux outage means you cannot deploy new changes, not that existing workloads stop.

---

## 6. Access Control, RBAC, and Self-Service

### Service Account Impersonation — The Core Mechanism

Every Kustomization and HelmRelease that applies workloads in a multi-tenant cluster should specify a `serviceAccountName`. The controller impersonates this SA when talking to the Kubernetes API.

```yaml
# Tenant's Kustomization
spec:
  serviceAccountName: payments-reconciler   # impersonate this
```

```yaml
# The SA's permissions — namespace-scoped
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: payments-reconciler
  namespace: payments
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin          # cluster-admin scoped to the namespace via RoleBinding
subjects:
  - kind: ServiceAccount
    name: payments-reconciler
    namespace: payments
```

**Practical RBAC design for tenants:**
- `cluster-admin` bound as a RoleBinding (namespace-scoped) — this gives full access within the namespace but nothing outside it
- No ClusterRoleBindings for tenants
- If a tenant needs a CRD applied, that CRD must be pre-installed by the platform team; the tenant cannot create CRDs (cluster-scoped)

**Platform team's Kustomization:**
```yaml
spec:
  serviceAccountName: platform-reconciler   # or no SA, using controller's own SA
```

The platform reconciler SA has broader permissions — it applies CRDs, ClusterRoles, and cluster-scoped infrastructure.

### Protecting the Fleet Repo

The fleet repo's access controls ARE your deployment controls. Anyone who can merge to the fleet repo's main branch can change what runs in your clusters.

**Required protections:**
- Protected `main` branch — no direct push, MR required
- Minimum 2 approvals for MRs (1 from platform team for infra-layer changes)
- CODEOWNERS file — platform team must approve changes to `infrastructure/` and `clusters/`; teams can self-approve changes to their own `tenants/<team>/` path
- CI checks on every MR: `kustomize build` validates, `kubeconform` validates schema, custom checks for naming conventions and required fields
- Signed commits — optional but increasingly standard for regulated environments

```
# CODEOWNERS in fleet repo
/infrastructure/         @platform-team
/clusters/               @platform-team
/tenants/payments/       @payments-team @platform-team   # team + platform co-own
/tenants/risk/           @risk-team @platform-team
```

### Secrets Management

**Problem:** Secrets cannot be committed to Git in plain text. But Flux needs to apply them to the cluster.

**Option 1: SOPS/age (most common with Flux)**

```
Workflow:
1. Platform generates an age keypair per cluster
2. Private key stored as a Kubernetes secret in flux-system
3. App teams encrypt their secrets locally: sops --encrypt --age <public-key> secret.yaml
4. Encrypted secret committed to Git (safe — only the cluster can decrypt)
5. Flux applies it — kustomize-controller decrypts using the private key before applying
```

```yaml
# In team's kustomization.yaml (the kustomize tool config, not Flux CRD)
generators:
- .|sops -d secret.enc.yaml      # or use kustomize-controller's native SOPS support

# In Flux Kustomization:
spec:
  decryption:
    provider: sops
    secretRef:
      name: sops-age-key          # the private key, in flux-system
```

**Team workflow with SOPS:**
- Platform shares the cluster's age public key (safe to share — encrypt only)
- Team encrypts: `sops --encrypt --age <pubkey> secret.yaml > secret.enc.yaml`
- Team commits `secret.enc.yaml` (never the unencrypted version)
- Team cannot decrypt on their laptops unless they also have the private key (most don't)
- Result: teams can add secrets without being able to read the existing ones in prod

**Option 2: External Secrets Operator**

Secrets live in Vault or AWS Secrets Manager. ESO syncs them into Kubernetes Secrets. Teams commit `ExternalSecret` CRDs (references to Vault paths) rather than encrypted values.

Better for environments with an existing secret store; adds an ESO dependency.

### OCI Artifact Signing (cosign)

For OCI-based delivery, CI signs the artifact after building and Flux verifies the signature before applying:

```yaml
# In OCIRepository:
spec:
  verify:
    provider: cosign
    secretRef:
      name: cosign-public-key
```

If the signature does not match, Flux refuses to apply. This ensures only CI-produced artifacts are ever applied — a compromised registry cannot inject unsigned artifacts.

### The flux CLI for Platform and Product Teams

**What platform teams use:**
```bash
# View reconciliation status across all namespaces
flux get all -A

# Check why something failed
flux logs --level=error -n payments

# Force a reconciliation (useful after fixing a source issue)
flux reconcile kustomization payments --with-source -n payments

# Suspend during an incident (prevents Flux from overwriting manual changes)
flux suspend kustomization payments -n payments

# Resume after incident is resolved
flux resume kustomization payments -n payments

# See what Flux would change without applying
flux diff kustomization payments -n payments

# Trace a resource back to its Flux source
flux trace deployment payments-api --namespace payments
```

**What product teams should know:**
```bash
# Check their deploy status
flux get kustomizations -n payments

# Check a HelmRelease
flux get helmreleases -n payments

# Watch events for their namespace
flux logs --namespace payments --tail 50

# See what's in their source
flux get sources git -n payments
```

---

## 7. Delivery and Incident Workflows

### A Normal Release

```
1. Developer pushes feature branch, opens MR in app repo
2. GitLab CI: runs tests, builds image, pushes image to registry
3. MR approved and merged to main
4. CI updates image tag in deploy/prod/kustomization.yaml (or commits to manifest repo)
5. Flux source-controller detects commit (via webhook or next poll)
6. Flux kustomize-controller reconciles: applies updated Deployment
7. Kubernetes rolls out new pods
8. Flux evaluates health check: waits for Deployment to be Ready
9. Flux notification-controller sends "Kustomization payments reconciled" to Slack
```

**Where it can stall and how each side observes:**

| Stage | Stall cause | How to detect |
|---|---|---|
| Step 5 | Webhook not configured, long poll interval | `flux get gitrepositories -n payments` shows old revision |
| Step 6 | Bad YAML, schema error | `flux logs -n payments`, Kustomization status has error condition |
| Step 7 | Image pull error, OOM, crashloop | `kubectl get events -n payments`, `kubectl describe pod` |
| Step 8 | Health check timeout | Kustomization status shows `HealthCheckFailed` condition |
| Step 9 | Slack provider misconfigured | Check Alert/Provider status in `flux get alerts -n payments` |

### Platform-Level Incidents

**source-controller is down:**
- New changes cannot be pulled
- All Kustomizations show `SourceNotFound` or stale conditions
- Running workloads: completely unaffected — pods keep running
- Platform action: check source-controller pod logs, check resource limits (OOM?), check Git provider reachability
- Recovery: once source-controller is back, it automatically catches up

**Git provider outage (GitLab down):**
- Same effect as source-controller down for Git-sourced repos
- OCI-sourced repos unaffected if registry is separate
- Mitigation: webhook-driven orgs may have been in the middle of a reconciliation — check Kustomization status for incomplete reconciles
- Recovery: automatic when GitLab recovers

**Controller OOM crash:**
- kustomize-controller or helm-controller can OOM if managing too many resources
- Symptom: controller pod in `OOMKilled` CrashLoopBackOff, reconciliation stops
- Mitigation: increase controller memory limits, enable leader-election with multiple replicas
- Platform action: `kubectl describe pod -n flux-system`, check metrics for memory trend

### Rollout Failure — Product Team's Playbook

**Step 1: Check the Kustomization or HelmRelease status**
```bash
kubectl describe kustomization payments -n payments
# Look for: Ready=False, Reason, Message
```

**Step 2: Check Flux logs**
```bash
flux logs --namespace payments --level error
```

**Step 3: Check the pod**
```bash
kubectl get events -n payments --sort-by='.lastTimestamp'
kubectl describe pod <failing-pod> -n payments
```

**Step 4: If the HelmRelease is stuck in `pending-upgrade`**

This happens when a previous upgrade failed midway. Helm's release secret is in a locked state.
```bash
# Check status
kubectl describe helmrelease payments-api -n payments
# Status will show: upgrade retries exhausted, last attempt failed

# Reset the release (Flux will re-run the upgrade)
flux suspend helmrelease payments-api -n payments
kubectl patch helmrelease payments-api -n payments \
  --type=json \
  -p='[{"op":"remove","path":"/status"}]'
flux resume helmrelease payments-api -n payments
```

**Step 5: Roll back via Git (preferred)**

If the bad version is already in prod:
```bash
# In the app repo — revert the image tag commit
git revert HEAD
git push
# Flux picks it up and reverts the deployment
```

Why Git revert beats `kubectl rollout undo`:
- The Git revert is the source of truth — Flux will re-apply the current tag if you kubectl rollout undo but don't update Git
- The Git history shows exactly what happened and when
- All teams see the revert in their notification channel

### Drift Scenarios

**Someone kubectl-edits a Deployment in prod:**
```
T+0:   kubectl edit deployment payments-api (increases replicas to 10)
T+5m:  Flux reconciles: overwrites with replicas: 3 from Git
T+5m:  Notification: "payments Kustomization reconciled"
T+5m:  Incident: "why did the replicas drop to 3?!"
```

**Resolution:**
1. If the change was intentional: update Git, open an MR
2. If the change was a mistake: Flux already corrected it — no action needed

**The break-glass pattern — making a deliberate manual change without fighting Flux:**
```bash
# Step 1: Suspend the Kustomization
flux suspend kustomization payments -n payments

# Step 2: Make the manual change
kubectl edit deployment payments-api -n payments

# Step 3: IMMEDIATELY update Git to match what you just did
# (otherwise Flux will revert it when resumed)
git commit -m "hotfix: increase replicas to 10 for traffic spike"
git push

# Step 4: Resume Flux
flux resume kustomization payments -n payments
```

**The discipline:** Never leave a Kustomization suspended without a matching Git commit. Suspended + no Git update = time bomb.

### Bad Image Tag Promoted to Production

**Detection:** Flux health check fails (pod crashloops, readiness probe fails). Alert fires to team Slack channel.

**Immediate response:**
```bash
# Platform confirms the issue
flux get kustomizations -n payments -A
# Shows: payments Kustomization Ready=False, HealthCheckFailed

# Check which image was applied
kubectl describe deployment payments-api -n payments | grep Image
```

**Resolution via Git revert:**
```bash
# In the app repo
git log --oneline deploy/prod/    # find the bad commit
git revert <bad-commit-hash>
git push
# Flux picks it up within seconds (webhook) or minutes (polling)
# Previous image tag is re-applied
```

**Why not use helm-controller rollback?** In a Git-based system, the cluster should mirror Git. A helm-controller rollback changes the cluster state without changing Git — Flux will re-apply the bad version at the next reconcile. Always revert in Git.

---

## 8. Architecture Evolution Over Time

### Stage 1 — Initial Setup (1 cluster, 1 team)

**Scale:** 1 cluster, 1-3 teams, 10-30 apps, ~5 reconciliations/minute

**Setup:**
- `flux bootstrap` into the cluster
- Fleet repo with a simple structure: `apps/` + `infrastructure/`
- No tenancy model yet — everyone has access to `flux-system`
- SOPS set up from the start (retrofitting secrets encryption is painful)
- Basic Slack notification

**Pain points that signal it's time to grow:**
- Teams are stepping on each other's Kustomizations
- Someone accidentally deletes another team's namespace with `prune: true`
- CI pipeline kubeconfigs are proliferating
- "I don't know why my service was restarted" — insufficient visibility

### Stage 2 — Growing (Multi-Tenant, Multi-Team)

**Scale:** 1-3 clusters, 5-15 teams, 50-200 apps, ~30 reconciliations/minute

**Changes:**
- Introduce namespace-per-team tenancy model
- Add `serviceAccountName` impersonation to all tenant Kustomizations
- Enable `--no-cross-namespace-refs` on controllers
- Each team gets their own app repo; fleet repo becomes the connector layer
- CODEOWNERS enforced on fleet repo
- Notification routing per team (each team has their own Alert)
- Grafana dashboard for Flux metrics per namespace
- Onboarding runbook published internally

**Operational pattern:** Platform team reviews and merges all fleet repo MRs. App teams self-serve within their app repos.

**Pain points that signal the next evolution:**
- Fleet repo MR volume is high — platform team is a bottleneck
- Reconciliation latency is too high for teams' deploy velocity (waiting 5+ minutes to see changes)
- Controller memory limits hit as resource count grows
- Desire to render manifests in CI rather than have Flux run kustomize (for CUE/complex rendering)

### Stage 3 — Mature Platform (OCI, Fleet, Platform Layer)

**Scale:** 5+ clusters, 20+ teams, 500+ apps, 100+ reconciliations/minute

**Changes:**
- Fleet of clusters from a hub repo (separate fleet repos for prod vs non-prod)
- OCI artifact delivery: CI renders manifests (CUE → YAML), pushes to OCI registry, Flux applies via OCIRepository — Flux never runs kustomize for app workloads
- Signed OCI artifacts (cosign) — Flux verifies before applying
- Controller resource tuning: separate controller deployments per concern, tuned concurrency
- Webhook-driven reconciliation (not polling) for sub-30-second deploy latency
- KubeVela or internal platform layer above Flux — app teams never create Flux resources directly
- Per-tenant SLAs: reconciliation latency alerting, drift detection alerting
- Flux as invisible substrate — teams interact with Application CRDs, platform converts to Flux resources

**Platform team responsibilities at this stage:**
- Controller upgrades and performance tuning
- Fleet repo governance
- OCI registry infrastructure
- Cosign key management
- Reconciliation SLA monitoring
- Platform layer (KubeVela) integration maintenance
- New cluster provisioning automation

---

## 9. Best Practices and Anti-Patterns

### Best Practices

**Repository and structure:**
- Enforce a standard repo layout via a template. Make the template the path of least resistance.
- Use a single fleet repo per cluster group (prod vs non-prod). Avoid one fleet repo for all environments — a bad prod-bound commit affecting staging first gives you a canary.
- Name Kustomizations after the team and environment: `payments-prod`, `payments-staging`. Never use generic names like `apps`.
- Every Kustomization and HelmRelease must have `team` and `owner` labels.

**Reconciliation mechanics:**
- Always set `dependsOn` for components with ordering requirements: CRDs before controllers, cert-manager before resources that use certificates.
- Set explicit `timeout` values. The default (infinite) means a hung reconciliation blocks the queue forever.
- Use `prune: true` deliberately — understand what resources it will delete. Do a dry-run before enabling on an existing cluster.
- Set `wait: true` so Flux confirms resources are healthy before marking the Kustomization as Ready. Without it, "reconciled" means "applied" not "healthy."

**Reconciliation performance:**
- Set up webhook receivers for low-latency reconciliation. Polling at 1m feels slow compared to sub-10-second webhook-triggered reconcile.
- Tune `interval` per criticality: infrastructure at 10m (rarely changes), app workloads at 5m, monitoring at 15m.
- Monitor controller goroutine count and queue depth — these signal when concurrency limits need tuning.

**Secrets:**
- Set up SOPS from day one. Retrofitting is painful.
- Share only the age public key with teams. Keep the private key only in the cluster.
- Run a CI check that no plain-text Kubernetes Secrets are committed to any team repo.
- Rotate decryption keys on a schedule; document the rotation procedure before you need it urgently.

**Monitoring Flux itself:**
- Alert on `gotk_reconcile_error_total` (reconciliation failures per controller)
- Alert on `gotk_reconcile_duration_seconds` (p95 reconcile latency) exceeding threshold
- Alert on any Kustomization or HelmRelease where `Ready=False` for >10 minutes
- Alert on suspended resources that have been suspended for >1 hour
- Run a weekly audit job: report all Kustomizations without `serviceAccountName`, without `prune`, or without `healthChecks`

**Disaster recovery:**
- Rehearse cluster rebuild from Git annually (or per quarter in regulated environments)
- The bootstrap command should be documented and tested — not just a note in a wiki
- Ensure all CRDs are installed before Flux tries to apply resources that use them (bootstrap order matters)
- Keep SOPS private keys in a secret store (Vault) with a documented recovery path

### Anti-Patterns

**Manual kubectl coexisting with reconciliation**
The first manual `kubectl apply` in a Flux-managed resource is the start of permanent drift. Flux will revert it at the next interval, causing confusion. Treat any manual change to a Flux-managed resource as an incident: suspend Flux, make the change, update Git, resume Flux — in that order.

**Long-lived environment branches**
`staging` branch and `main` branch diverge over months. Cherry-picking becomes the standard workflow. Eventually the branches are incompatible and nobody knows what's actually in each environment. Use directory-per-environment on a single branch.

**Cross-namespace references without `--no-cross-namespace-refs`**
Tenant A's Kustomization references a GitRepository in `flux-system`. If the platform's source is compromised, all tenants are affected. Enforce `--no-cross-namespace-refs` at controller startup.

**A single "god repo" that everyone pushes to**
No CODEOWNERS, no ownership, anyone can merge anything. Works for 1-3 people. At 10 teams it becomes a political and operational nightmare. Establish CODEOWNERS and MR approvals from the start.

**`prune: false` out of fear**
Platform team is scared to enable pruning because they don't know what's in the cluster. The correct fix is to audit the cluster and add missing resources to Git — not to disable pruning. A cluster with `prune: false` accumulates orphaned resources over time and eventually drifts beyond what anyone can understand.

**Suspended resources forgotten forever**
A Kustomization was suspended at 3am during an incident six months ago and nobody noticed. It has been running its last-known state since — but nobody has been applying updates to it. Enforce: alert on suspended resources >1 hour, weekly audit, required un-suspend in the incident postmortem.

**Secrets in Git (even accidentally)**
Once a secret is in Git history, it is compromised — removing it from the working tree does not purge it from history. Enforce: git-secrets or similar pre-commit hook on all repos, CI check that blocks MRs containing Kubernetes Secret resources without SOPS encryption.

**HelmRelease values that override image tags without pinning**
```yaml
values:
  image:
    tag: latest    # ← never do this in production
```
`latest` is not a version — it is a race condition. Always pin image tags in production HelmRelease values.

---

## 10. Production Checklist

### Technical Readiness

**Bootstrap and recoverability:**
- [ ] Cluster can be fully rebuilt from `flux bootstrap` + Git in under 30 minutes
- [ ] Bootstrap procedure is documented, tested, and stored outside the cluster itself
- [ ] Flux manifests (gotk-components) are pinned to a specific version (not `latest`)
- [ ] CRD installation is ordered before any controller that uses those CRDs

**Controller configuration:**
- [ ] Resource limits set on all Flux controllers (requests and limits)
- [ ] Controller memory monitored with alerts for >80% of limit
- [ ] `--no-cross-namespace-refs` enabled on kustomize-controller and helm-controller
- [ ] `--default-service-account` set to a minimal-permissions SA
- [ ] Leader election enabled if running multiple controller replicas

**Tenancy and RBAC:**
- [ ] Every tenant Kustomization has `serviceAccountName` set
- [ ] Tenant SAs are namespace-scoped (RoleBinding, not ClusterRoleBinding)
- [ ] Platform Kustomizations use a separate, audited SA
- [ ] No tenant can create ClusterRole, ClusterRoleBinding, or Namespace resources
- [ ] CODEOWNERS on fleet repo enforces platform approval on infrastructure paths

**Secrets:**
- [ ] SOPS or ESO configured for all clusters
- [ ] Age private key stored in Vault or equivalent, not only in the cluster
- [ ] Pre-commit hooks and CI checks block plain-text Secret commits
- [ ] Key rotation procedure documented and tested

**Notification and observability:**
- [ ] Slack/PagerDuty providers configured
- [ ] Alerts configured for every production Kustomization and HelmRelease
- [ ] `gotk_reconcile_error_total` alert firing in monitoring
- [ ] Grafana dashboard showing per-namespace reconciliation health
- [ ] Suspended resources audit scheduled (daily or weekly)

**Health checks and pruning:**
- [ ] `prune: true` on all Kustomizations (with initial audit completed)
- [ ] `wait: true` or explicit `healthChecks` on all Kustomizations
- [ ] `timeout` set on all Kustomizations and HelmReleases
- [ ] HelmRelease `upgrade.remediation` configured (retries + rollback)
- [ ] `dependsOn` set for all components with ordering requirements

**Disaster recovery rehearsal:**
- [ ] Full cluster wipe and rebuild from Git tested in staging
- [ ] Bootstrap time measured and within acceptable SLA
- [ ] SOPS key recovery procedure tested

### Organizational Readiness

**Repository and process:**
- [ ] Repo layout standard documented and enforced by CI
- [ ] Template repo available for new teams
- [ ] Onboarding runbook published and reviewed by at least two teams
- [ ] Ownership defined for every Kustomization and HelmRelease (label + CODEOWNERS)
- [ ] Promotion policy documented: how changes move from dev → staging → prod

**Team enablement:**
- [ ] Product teams know how to run `flux get`, `flux logs`, `flux reconcile`
- [ ] "My deploy is stuck" runbook accessible to all engineers
- [ ] Escalation path to platform team documented
- [ ] Break-glass procedure documented: suspend → manual change → Git commit → resume

**Change control:**
- [ ] Fleet repo has protected main branch requiring MR and approvals
- [ ] Infrastructure path changes require platform team approval
- [ ] CI pipeline validates all manifests before MR can merge (kustomize build, kubeconform, custom checks)
- [ ] All changes to production cluster state traceable to a Git commit with author and timestamp

### Multi-Tenant Strategy

- [ ] Tenant grain defined and documented (team / product / environment)
- [ ] Namespace naming convention enforced
- [ ] Impersonation SA per tenant provisioned and RBAC audited
- [ ] Blast radius per tenant documented and acceptable
- [ ] Fleet repo change control reviewed for multi-tenant safety (`--no-cross-namespace-refs` enabled)
- [ ] Tenant onboarding MR template exists and is tested with at least one team
- [ ] Per-tenant alerting routes to team channels (not only platform channel)
- [ ] Tenant offboarding procedure documented (namespace deletion, SA revocation, source cleanup)
