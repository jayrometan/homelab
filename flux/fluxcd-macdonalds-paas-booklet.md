## How to Use This Booklet

This is a working reference, not a tutorial. You already know Kubernetes, Helm, Kustomize, and CI/CD; nothing here re-explains those. What it does is build the specific mental model you need for Flux: what each controller actually owns, where the trust and blast-radius boundaries sit between platform and product teams, and how the whole thing changes shape once KubeVela and CUE sit on top of it — which is exactly the arrangement waiting for you.

A few conventions carry through every section:

- **Prose explains, tables summarize.** If you want the "just tell me the fields" version of something, look for the nearest table.
- A red-barred callout marks a **PROD** note — something that has genuinely burned teams in production, not a theoretical edge case.
- Every YAML example is labeled **platform-owned** or **app-team-owned** in its heading. That distinction is the organizing principle of this entire booklet, and it will be the organizing principle of your first month at Macdonalds.
- This booklet covers Flux at the version line current as of mid-2026 (Flux v2.8, with the Flux Operator and `FluxInstance` CRD as the modern install/lifecycle path). If Macdonalds' cluster predates that — plenty of production clusters still run classic `flux bootstrap` — the underlying controller mechanics are identical; only the installation and upgrade story differs, and that's called out explicitly wherever it matters.

---

## 1. FluxCD and the GitOps Model

### 1.1 What Flux actually is

Flux is not one program. It's a small federation of independent Kubernetes controllers — source-controller, kustomize-controller, helm-controller, notification-controller, and the image automation pair — each with its own CRDs, its own reconciliation loop, and its own single responsibility. There is no "Flux server" in the way there's a Consul server or a Vault server; there's a namespace (conventionally `flux-system`) full of ordinary Deployments, each running an ordinary controller-runtime reconciler, each watching its own slice of custom resources. If you've ever debugged a cert-manager or external-dns installation, you already know the shape of what you're dealing with — Flux is that pattern applied to the entire deployment pipeline instead of one concern.

The job of that federation, collectively, is to continuously pull declared state from an external source — Git, an OCI registry, a Helm repository, or an S3-compatible bucket — and reconcile the cluster toward it. "Reconcile" is doing a lot of work in that sentence, and it's worth being precise about it: this is not a one-shot deploy. Every controller runs a loop, on an interval you configure per-resource, that re-fetches the source, re-diffs it against live cluster state, and re-applies whatever has drifted — forever, until you tell it to stop. That's the entire idea of GitOps in one sentence: **the loop, not the deploy, is the product.**

### 1.2 Pull versus push, and why it's a security question before it's a workflow question

You've operated push-based delivery — CI pipelines that `kubectl apply` or `helm upgrade` directly against a cluster. It's worth being explicit about what changes when you flip the direction, because the interesting difference isn't ergonomics, it's blast radius.

In a push model, your CI runner needs live credentials with write access to the cluster's API server, sitting in CI variables, usable by anyone who can trigger a pipeline or, worse, anyone who compromises the runner. That credential is frequently broader than any single pipeline needs, because scoping CI credentials tightly per-repo, per-namespace is genuinely annoying to maintain, so in practice it rarely happens with the rigor it deserves. A compromised GitLab runner in a push shop is a compromised cluster.

In a pull model, nothing outside the cluster holds a credential that can touch the API server. CI's job stops at "produce an artifact and put it somewhere Flux can read it" — a registry push credential, not a cluster credential. The controller inside the cluster is the only thing with API server access, and that access is itself governed by Kubernetes RBAC and — this is the part that matters for a multi-tenant platform — can be scoped down per-tenant via impersonation, which Section 6 covers in depth. For a shop where blast radius containment is a stated design goal, this is not a stylistic preference. It's the actual reason to run GitOps over CI-push, ahead of drift correction, ahead of auditability, ahead of any of the other reasons usually listed first.

The second property — drift detection and self-healing — is the one everyone leads with, and it matters too: because the loop keeps re-diffing, a `kubectl edit` against a Flux-managed resource in production gets silently reverted on the next reconciliation. This is exactly the kind of thing that will bite you in week one if nobody warns you.

<div class="prodbox">
**PROD:** if you come from an environment where hotfixing prod with `kubectl edit` or `kubectl scale` under incident pressure was normal, that habit will actively fight you here. Flux will patiently undo your fix on the next reconcile — could be thirty seconds later — and you will not immediately understand why the pod count went back to 2. The correct move under incident pressure is `flux suspend kustomization <name>` first, edit second, fix-forward in Git third, `flux resume` last. Section 7 walks through this exact sequence and the discipline problem of the last step.
</div>

### 1.3 Contrast: ArgoCD, and plain CI `kubectl apply`

You'll hear ArgoCD mentioned constantly in interviews and in the wild, so it's worth having a crisp comparison rather than a vague "they're both GitOps tools" answer.

Architecturally, ArgoCD centers on a single `Application` CRD that bundles source, destination, and sync policy into one object, backed by a stateful API server, a repo-server, and (usually) a UI that a lot of orgs build real workflow around. Flux instead splits "where does the desired state come from" (a Source: `GitRepository`/`OCIRepository`/`HelmRepository`/`Bucket`) from "what do I do with it" (a Kustomization or HelmRelease) as separate, composable CRDs. That composability is not academic — it's the exact property that lets KubeVela sit on top of Flux without Flux needing to know KubeVela exists: KubeVela's controller can emit `Application` objects (its own CRD, unrelated to ArgoCD's) into a Git or OCI artifact, and a completely generic Kustomization applies them, the same way it'd apply anything else. Argo's tighter source-destination-policy coupling makes that kind of "invisible substrate" arrangement more awkward, though not impossible.

Operationally, Argo's culture leans UI-and-API-server-first — a lot of shops manage sync policy, RBAC, and even day-to-day operations through `argocd` the API/UI rather than pure Git. Flux is much more CLI-and-Git-native by default: no mandatory UI, no separate API server holding its own session state, fewer moving parts to secure and patch. That said, this gap has narrowed — the Flux Operator now ships an optional Web UI for cluster-wide visibility, so "Flux has no UI" is no longer strictly true, but it remains optional and read-oriented rather than the primary operating surface the way Argo's UI often becomes.

Plain CI `kubectl apply` — no reconciler at all — is the baseline everything else improves on. It's genuinely fine for a single small cluster with one team and low change volume. It breaks down for exactly the reasons above: no drift correction (a manual change just sits there, silently diverged, until someone notices), CI needs broad cluster credentials, and there's no clean "what's actually running versus what's declared" answer without bolting on extra tooling. Most orgs don't choose Flux over raw CI deploys for elegance — they choose it because the raw-CI failure mode (silent drift, broad CI credentials) becomes actively dangerous once you have more than a couple of teams touching the cluster.

### 1.4 Where this sits organizationally

The three-way split that recurs through this entire booklet: **platform team owns the controllers, the bootstrap, and the fleet repo's cluster-level paths. CI (GitLab CI, in your case) builds, tests, and publishes artifacts — images, and increasingly, fully-rendered configuration packaged as OCI artifacts. Flux's controllers pull those artifacts and reconcile them into the cluster. Product/feature teams own their own manifests, their own release cadence within the guardrails platform sets, and nothing else.**

CI never touches your cluster's API server directly. That single sentence is the load-bearing architectural decision underneath everything that follows, and it's worth having crisp in your head walking into week one, because it's the answer to "why does our pipeline look like this" for a large fraction of the questions you'll field from confused app teams.
## 2. Architecture and Components — With Team Boundaries

### 2.1 The controllers

Five controllers, each independently deployable, independently scalable, independently upgradable. Treat this table as the thing you memorize before anything else in this booklet — nearly every debugging conversation you'll have starts with "which controller owns this."

| Controller | Owns these kinds | Core job |
|---|---|---|
| source-controller | `GitRepository`, `OCIRepository`, `HelmRepository`, `Bucket` | Fetch, verify, and cache artifacts; serve them internally to other controllers |
| kustomize-controller | `Kustomization` | Build (via kustomize), apply via server-side apply, prune, health-check, order via `dependsOn` |
| helm-controller | `HelmRelease` | Render and install/upgrade/rollback Helm charts via the Helm SDK; drift detection on Helm-managed resources |
| notification-controller | `Alert`, `Provider`, `Receiver` | Outbound events to Slack, Teams, PagerDuty, or generic webhooks; inbound webhooks trigger immediate reconciliation |
| image-reflector / -automation | `ImageRepository`, `ImagePolicy`, `ImageUpdateAutomation` | Scan registries, evaluate tag policy, optionally commit tag bumps to Git |

source-controller is worth dwelling on because its role is easy to underrate. It doesn't just "fetch Git" — it fetches, verifies (checksum, optionally cosign or PGP signature), packages as a compressed tarball artifact, and exposes that artifact over an internal HTTP endpoint that every other controller consumes. Functionally it's a tiny internal pull-through cache sitting in front of your Git provider and your OCI registry, the same role Artifactory plays for you today — it decouples "how often do we hit the origin" from "how often do we act on what we fetched," which matters once you have dozens of Kustomizations that would otherwise each poll GitLab independently.

kustomize-controller is worth a second look too, because its name is slightly misleading in Macdonalds' context. The controller's job is "apply this directory of manifests, prune what's gone, health-check what's there, respect `dependsOn` ordering" — it does not require you to actually use Kustomize's overlay/patch machinery. If CI has already fully rendered CUE output into flat YAML and packaged it as an OCI artifact, the Kustomization object just applies that directory as-is; there's no `kustomization.yaml` overlay logic invoked at all. **CUE replaces Kustomize's templating role. kustomize-controller still provides the apply/prune/health/ordering machinery underneath it — the controller and the templating tool it's named after are decoupled once CI does the rendering.** This is the single most important architectural fact for understanding how Flux behaves under your specific stack, and it's worth having crisp before your first week, because "why is it called kustomize-controller if we don't write Kustomize overlays" is a question you'll otherwise waste time being confused about.

### 2.2 Reconciliation loop mechanics

Every Source and every Kustomization/HelmRelease has an `interval` — how often the controller re-checks, baseline safety net. Independently, a `Receiver` (notification-controller) can accept an inbound webhook from GitLab and trigger an out-of-cycle reconcile the moment something merges, so in practice you get near-immediate reconciliation on push with the interval as a fallback rather than the primary trigger — important, because the fallback path is the one that actually keeps you safe if the webhook breaks, so it has to be tested, not just configured and forgotten.

Controllers key off the **source revision** — a Git commit SHA or an OCI digest — not off wall-clock time. An unchanged revision on a reconcile tick is a no-op; nothing gets re-applied just because the interval elapsed. `dependsOn` on a Kustomization or HelmRelease builds a DAG: infrastructure that must exist first (a CRD, an operator, StackGres before something that needs a Postgres cluster) reconciles and passes its health check before anything depending on it is attempted — conceptually the same graph Terraform builds from resource references, just continuously re-evaluated instead of triggered once. Health checks themselves gate the `Ready` condition: `status.conditions[Ready]` doesn't mean "apply succeeded," it means "apply succeeded *and* the referenced resources (or a custom health check target) actually became healthy" — the same philosophy as an HAProxy backend not entering rotation until it passes its own health check, not just until the process started.

Pruning deletes anything previously applied by a Kustomization that's no longer present in the current source revision, tracked via a server-side-apply inventory rather than the older label-based approach. This is genuinely dangerous if you don't respect it: point a Kustomization's `path` at the wrong directory, or truncate a rendered artifact by mistake, and prune will happily delete everything it thinks is no longer wanted. Section 9 has the specific anti-pattern.

`flux suspend`/`flux resume` stop and restart reconciliation for an object without deleting it — the mechanism behind every deliberate break-glass intervention in Section 7, and also the source of the single most common operational mistake with Flux: forgetting to resume. A suspended Kustomization doesn't shout at you; it just quietly stops taking updates from Git, and unless you're specifically auditing for suspended resources, that can persist for weeks. This is common enough that it belongs in your monitoring from day one, and Section 9 says exactly how.

### 2.3 Installing and upgrading Flux itself: bootstrap versus the Flux Operator

Historically, `flux bootstrap github|gitlab|git ...` was the only installation path: the CLI installs the controllers into the cluster *and* commits their own manifests, plus a root `GitRepository` and root `Kustomization`, back into your target repo — meaning the cluster subsequently manages its own upgrade path from Git. That root Kustomization is the trust anchor for the entire cluster: whatever it watches, whoever can merge to that path controls the cluster, full stop. Protecting it with CODEOWNERS and branch protection isn't a nice-to-have, it's structurally equivalent to controlling who holds quorum on your Vault unseal keys.

As of Flux 2.8, the actively-developed installation and lifecycle path is the **Flux Operator**, via a `FluxInstance` custom resource:

```yaml
# platform-owned — clusters/<cluster-name>/flux-system/flux-instance.yaml
apiVersion: fluxcd.controlplane.io/v1
kind: FluxInstance
metadata:
  name: flux
  namespace: flux-system
  annotations:
    fluxcd.controlplane.io/reconcileEvery: "1h"
spec:
  distribution:
    version: "2.x"                # pinned minor line; operator manages patch upgrades
    registry: "ghcr.io/fluxcd"
  components:
    - source-controller
    - kustomize-controller
    - helm-controller
    - notification-controller
    - image-reflector-controller
    - image-automation-controller
  cluster:
    type: kubernetes
    multitenant: true               # first-class lockdown flag — see Section 5
    tenantDefaultServiceAccount: flux
    networkPolicy: true
  sync:
    kind: GitRepository
    url: "https://gitlab.tower.internal/platform/fleet.git"
    ref: "refs/heads/main"
    path: "clusters/prod-use1"
```

This is Flux-managing-Flux via the exact same declarative, Git-sourced model it applies to everything else — the FluxInstance is itself just another reconciled object, and the operator's `FluxInstanceReconciler` keeps the installed controllers, their versions, and cluster-wide lockdown flags converged to this spec. The practical upshot versus classic bootstrap: version pinning and upgrades become a one-line spec change instead of a CLI re-run, multi-tenancy lockdown becomes a boolean instead of a set of hand-maintained JSON6902 patches against the controller Deployments (the mechanism Section 5 shows for context, because plenty of production clusters — possibly Macdonalds' — were bootstrapped before the operator existed and still carry those patches forward), and large fleets get a `spec.sharding` option to split reconciliation load across multiple controller replicas by label selector, rather than one kustomize-controller instance quietly becoming the bottleneck as tenant count grows.

**Confirm on day one which of these Macdonalds runs.** The controller-level mechanics in the rest of this booklet are identical either way; only "how do we upgrade Flux" and "how is multi-tenancy lockdown configured" change.

### 2.4 Team boundary map

| Owned by platform | Owned by product/app team |
|---|---|
| Bootstrap / FluxInstance, controller versions, HA and resource sizing | Their namespace's Kustomization(s) / HelmRelease(s) |
| Cluster-scoped Sources (fleet repo root, shared infra sources) | Their own image policies (if image automation is in use) |
| Tenancy/RBAC scaffolding, SOPS/age key infrastructure | Their own Alerts, routed to their own channel |
| Notification Providers (the actual webhook URLs/credentials) | Their app repo or path within the fleet repo |
| Fleet repo CODEOWNERS and branch protection | — |

```yaml
# platform-owned — infrastructure/stackgres/kustomization.yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: stackgres-operator
  namespace: flux-system
spec:
  interval: 30m
  sourceRef:
    kind: GitRepository
    name: fleet
  path: "./infrastructure/stackgres"
  prune: true
  wait: true
  timeout: 5m
  # No serviceAccountName set: this reconciles with kustomize-controller's
  # own (elevated) identity, because installing an operator is cluster-scoped
  # platform work, not tenant work. That's a deliberate exception to the
  # impersonation rule in Section 6, not an oversight.
```

```yaml
# app-team-owned — tenants/quant-desk-a/overlays/prod/kustomization.yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: quant-desk-a-checkout
  namespace: flux-system
spec:
  interval: 5m
  sourceRef:
    kind: OCIRepository
    name: quant-desk-a-checkout   # CI-published, CUE-rendered artifact
  path: "./"
  prune: true
  wait: true
  timeout: 3m
  serviceAccountName: quant-desk-a   # impersonation — this Kustomization can
                                      # only do what the quant-desk-a SA is
                                      # allowed to do in its own namespace
  targetNamespace: quant-desk-a
  healthChecks:
    - apiVersion: apps/v1
      kind: Deployment
      name: checkout-api
      namespace: quant-desk-a
```

The `serviceAccountName` line is doing the real work in the second example, and it's the exact mechanism Section 6 goes into in depth — everything about Flux's multi-tenancy story reduces to that one field being set correctly, everywhere it needs to be.
## 3. How Companies Actually Use Flux

### 3.1 Fleet repo layout

The dominant pattern at any org past "one team, one cluster" is a single fleet repo with a directory convention, not a repo-per-team free-for-all. A layout you'll recognize immediately from Kustomize base/overlay conventions you already use:

```
fleet-repo/
├── clusters/
│   ├── prod-use1/flux-system/        # FluxInstance or gotk-*.yaml, root Kustomization
│   └── prod-eu1/flux-system/
├── infrastructure/                    # platform-owned: operators, CNI config, CRDs
│   ├── cilium/
│   ├── spire/
│   ├── stackgres-operator/
│   ├── redpanda-operator/
│   └── victoriametrics/
└── tenants/                           # app-team-owned, one directory per tenant
    ├── quant-desk-a/
    │   ├── base/
    │   └── overlays/{dev,staging,prod}/
    └── quant-desk-b/
```

Polyrepo (each product team owns a separate repo, referenced into the fleet via its own Source) is the other common shape, and it's not strictly worse — it gives teams real repo-level access control and independent CI without touching a shared repo's MR queue. The tradeoff is that "what's actually different between environments" becomes harder to audit at a glance across many repos versus one fleet repo with visible per-environment directories. Most orgs land on a hybrid: a fleet repo owning `clusters/` and `infrastructure/`, with `tenants/*` either as directories in the same repo (simpler at small-to-mid scale) or as thin pointer manifests referencing each team's own repo (better isolation at larger scale, more moving parts to keep straight).

### 3.2 Environment promotion — and why directories beat branches

Three ways to express "this change is now in staging, this other change is now in prod," and they are not equally good:

**Branch-per-environment** (a `staging` branch, a `prod` branch, changes flow via merge or cherry-pick) is the pattern people reach for first because it maps onto familiar Git-flow instincts, and it's also the pattern that causes the most pain at scale. It conflates two different lifecycles — code branching and environment promotion — that don't actually move at the same cadence, and cherry-picking a fix into three long-lived branches while they've each drifted independently is a recurring, entirely avoidable source of incidents.

**Directory-per-environment** (single `main` branch, `overlays/dev`, `overlays/staging`, `overlays/prod`, promotion is an MR that changes a value — usually an image tag or artifact digest — from one overlay to the next) is the standard modern answer. Every environment's state is visible in the same commit; promotion is a small, auditable, reviewable diff; there's exactly one source of truth per point in time.

**OCI-artifact-tag promotion** is the sharpest version of the same idea, and it's the one that matters most for your specific stack: CI renders once, publishes an immutable OCI artifact (content-addressed by digest), and "promoting to prod" means updating which digest a given environment's `OCIRepository` references — not rebuilding, not re-rendering, just re-pointing. Once CI is rendering CUE output and packaging it as an OCI artifact — which is exactly Macdonalds' model — this stops being optional. You get byte-for-byte identical artifacts across environments, which is the entire point: what passed staging is *provably* what reaches prod, not a re-render that happens to usually produce the same thing.

### 3.3 The CI/CD split

The clean division: **GitLab CI builds, tests, scans, renders, and publishes. Flux pulls, applies, health-checks, and notifies.** Nothing crosses that line in the other direction — CI does not `kubectl apply`, and Flux's controllers do not run arbitrary build steps.

```yaml
# .gitlab-ci.yml — app-team-owned, illustrative
stages: [build, render, publish]

build-image:
  stage: build
  script:
    - docker build -t "$CI_REGISTRY_IMAGE:$CI_COMMIT_SHORT_SHA" .
    - docker push "$CI_REGISTRY_IMAGE:$CI_COMMIT_SHORT_SHA"

render-cue:
  stage: render
  script:
    - cue vet ./app/...
    - cue export ./app --out yaml > rendered/
  artifacts:
    paths: [rendered/]

publish-oci-artifact:
  stage: publish
  needs: [render-cue]
  script:
    - flux push artifact
      "oci://registry.tower.internal/quant-desk-a/checkout:$CI_COMMIT_SHORT_SHA"
      --path=./rendered
      --source="$CI_PROJECT_URL"
      --revision="$CI_COMMIT_SHA"
    - flux tag artifact
      "oci://registry.tower.internal/quant-desk-a/checkout:$CI_COMMIT_SHORT_SHA"
      --tag="staging-latest"
  rules:
    - if: '$CI_COMMIT_BRANCH == "main"'
```

Why render in CI instead of letting Flux template on the cluster side? Three reasons, in the order they actually matter in production: **determinism** — what CI tested is byte-identical to what gets applied, no skew between a local `cue eval` and whatever's running cluster-side; **reduced blast radius on the controller itself** — kustomize-controller isn't executing arbitrary templating logic with cluster credentials at reconcile time, it's applying static YAML; and **testability** — you can render, diff, and policy-check the artifact in CI before it's ever pulled, instead of discovering a rendering bug live on cluster.

### 3.4 Flux underneath KubeVela: what's still Flux's job

This is the part of the CUE deep-dive worth reconnecting here: CUE authors KubeVela's `ComponentDefinition`/`TraitDefinition`/`WorkflowStepDefinition` templates *and* app-level configuration values; KubeVela's controller consumes those to render an `Application` object into real Deployments, Services, HTTPRoutes, and so on, via its own workflow engine — and critically, **KubeVela typically applies those resources itself once its own controller reconciles**, rather than emitting them for something else to apply.

The layered-controller pattern that results is one you already understand from any operator-backed CRD: Flux's job stops at "get the platform-authored `ComponentDefinition`/`TraitDefinition` set and the tenant's `Application` object into the cluster and keep them in sync with Git." KubeVela's own controller takes it from there — "given this `Application`, are the actual workloads present and correct" is KubeVela's reconciliation loop, not Flux's, the same relationship Flux itself has with a `Certificate` object it applies and cert-manager's controller then actually issues. Two independent reconcilers, each responsible for one layer, neither aware of the other's internals.

**Confirm this exact wiring with your team on day one rather than assuming it** — this is the standard way these two tools compose, but the specific division of labor (does Flux apply the `Application` object directly via a Kustomization, or does something in the CI pipeline talk to KubeVela's API instead) is a real design choice a platform team makes, not something inherent to the tools, and it's the single highest-value architecture question you can ask in your first week.

### 3.5 Two scenarios worked through

**Ten product teams, three clusters, one fleet repo, per-tenant directories.** Platform owns `clusters/*/flux-system` and `infrastructure/`; each of the ten teams owns `tenants/<team>/`. Repo-wide conventions (required labels, naming, directory shape) are enforced two ways: CODEOWNERS routes review of `tenants/<team>/**` to that team and routes `clusters/**`/`infrastructure/**` to platform, and a CI lint job runs schema validation (`kubeconform`, a `kustomize build --dry-run`, and an OPA/conftest policy pass) against any MR touching a Kustomization or HelmRelease, so violations get caught before merge rather than surfacing as a reconciliation failure after. Onboarding a new team is "copy the template directory, open an MR against your own path, get platform sign-off once on the tenant scaffolding (namespace, RBAC, service account) — after that, every future change is self-service within your own directory."

**Migrating from CI-push `kubectl`/Helm to Flux pull.** The trap here is assuming you can just point Flux at what's already running and walk away. In practice: stand up Flux with reconciliation on the target Kustomizations/HelmReleases *suspended* first, `flux diff kustomization <name> --path <local>` (or the HelmRelease equivalent) against live cluster state to see exactly what Flux would change if it took over — this surfaces every place hand-applied state has quietly drifted from whatever's declared in Git — reconcile those diffs deliberately (either by fixing Git to match reality, or accepting Flux's version and letting it correct reality), and only then resume reconciliation and decommission the old CI-push jobs. Skipping the diff-first step is how a migration turns into an incident: Flux takes over, immediately "corrects" months of undocumented manual drift in one shot, and something that was quietly relying on that drift breaks.
## 4. Onboarding Product Teams

### 4.1 What "the paved road" needs to contain

The platform team's real competitive advantage over "just let people kubectl whatever they want" is that the sanctioned path has to be *faster* than going around it, not just safer on paper. If shipping through Flux takes longer than a rogue `kubectl apply`, people will route around it under deadline pressure, and you'll spend your time on drift-cleanup instead of platform work. Concretely, that means a documentation set that answers, in order: how do I get a tenant namespace, what does my repo/directory need to look like, what labels and annotations are required, what counts as "healthy" for my Kustomization/HelmRelease, and how do I get paged when it isn't.

A "Shipping your first app" doc should be short enough to actually get read, and should point at a real, working template — not prose describing what a Kustomization should look like, an actual directory the team copies. The template should already have `dependsOn`, a `healthChecks` block, and a `serviceAccountName` filled in correctly, because "what does correct look like" is far more effective taught by example than by policy document.

### 4.2 Access provisioning

Platform provisions the tenant's namespace, ResourceQuota, ServiceAccount, and its scoped Role/RoleBinding — this is platform-owned, cluster-scoped work, applied the same way the StackGres operator install is (Section 2.4's first example: no impersonation, because creating a namespace and RBAC for a tenant is inherently a platform-level action a tenant's own limited SA couldn't perform on itself). Once that scaffolding exists, the team is genuinely self-service: their own repo/path permissions in GitLab, an OCI registry push credential scoped to their own artifact path in Artifactory, and from that point on, every change they make is an MR against their own directory.

### 4.3 Onboarding checklist

- [ ] Tenant namespace, ResourceQuota, and ServiceAccount created (platform MR, cluster-scoped)
- [ ] RoleBinding scoped to that namespace only, least-privilege verbs for the resource kinds the team actually needs
- [ ] Template Kustomization or HelmRelease copied into `tenants/<team>/`, with `serviceAccountName` and `targetNamespace` already correct
- [ ] `healthChecks` defined against at least the primary Deployment/StatefulSet
- [ ] `dependsOn` set if the app needs infra (a StackGres cluster, a Redpanda topic) that isn't already guaranteed present
- [ ] Team's own `Alert` object created, routed to their channel via a platform-provided `Provider`
- [ ] Rollback plan documented: what does this team do if a release fails health checks — who suspends, who reverts, who resumes
- [ ] MR opened against the fleet repo registering the tenant's source and Kustomization/HelmRelease; platform reviews once via CODEOWNERS

```yaml
# app-team-owned — tenants/quant-desk-b/ocirepository.yaml
# (Templated onboarding MR: this plus the Kustomization from Section 2.4
#  is the entire "shipping your first app" contract for a CUE/OCI tenant.)
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: quant-desk-b-pricer
  namespace: flux-system
spec:
  interval: 5m
  url: oci://registry.tower.internal/quant-desk-b/pricer
  ref:
    tag: staging-latest
  serviceAccountName: quant-desk-b   # pull credential scoped to this tenant's path
```

### 4.4 Avoiding GitOps chaos

The failure mode that shows up as tenant count grows past what one platform engineer can eyeball is not usually a big dramatic incident — it's slow accumulation of **unowned Kustomizations**: objects nobody's CODEOWNERS entry covers, created during some one-off migration or a departed engineer's experiment, that nobody gets paged for when they go `Ready=False`, because nobody's `Alert` routes to a human for them. The fix is boring and structural rather than clever: every Kustomization and HelmRelease's `namespace` and path must map to exactly one CODEOWNERS entry, enforced by a CI check that fails an MR if it introduces a path with no owner, not by hoping people remember. Snowflake repos — a team that got a one-time exception to skip the template and hand-roll their own layout — are the same problem wearing a different hat, and the fix is the same: no exceptions to the template without an expiry date and a follow-up ticket to bring it back in line.
## 5. Tenant-Based and Multi-Cluster Architecture

### 5.1 What "tenant" means here

"Tenant" is overloaded across the industry — team, product, environment, and cluster all get called tenants depending on who's talking. For Flux specifically, the trust boundary a tenant maps to is the **Kubernetes namespace**: Flux's own multi-tenancy documentation is explicit that sharing a namespace between tenants isn't supported, because it breaks the isolation guarantees the whole model rests on. Whatever "tenant" means organizationally at Macdonalds — a desk, a strategy team, an environment — it needs to resolve to one-or-more namespaces per tenant, not a shared one. Given the colo/low-latency topology considerations you'd expect at an HFT shop, don't be surprised if the cluster boundary itself also tracks something physical (site, exchange proximity) independently of the tenant boundary — that's a reasonable guess, not confirmed; ask on day one rather than assume.

### 5.2 The building blocks

Namespace-scoped Sources and Kustomizations/HelmReleases are the default — nothing here requires a separate mechanism, just discipline about where objects live. The mechanism that actually enforces isolation is **impersonation**: set `spec.serviceAccountName` on a Kustomization or HelmRelease, and the controller uses the Kubernetes Impersonation API to act as that ServiceAccount — under the hood, the controller's own (typically cluster-admin-bound) identity impersonates the named SA, and every apply is then subject to *that* SA's RBAC, not the controller's own. A tenant whose SA only has a Role granting `deployments`, `services`, and `configmaps` in their own namespace cannot create a ClusterRole no matter what their Kustomization's source contains — the apply simply fails with a permission-denied, surfaced in the object's status conditions.

Three flags close the gaps around that core mechanism, and they're worth knowing by name because they show up in exactly this form in Flux's own reference multi-tenancy repo (`github.com/fluxcd/flux2-multi-tenancy`, still the canonical example to clone and read):

- **`--default-service-account=<name>`**, set on kustomize-controller and helm-controller. Without it, any Kustomization or HelmRelease anywhere that simply omits `serviceAccountName` reconciles using the *controller's own* elevated identity — an easy, easy way to accidentally hand a tenant path cluster-admin-equivalent apply rights just because someone forgot one field. With it, a missing `serviceAccountName` falls back to a named account that should hold zero privileges, so the failure mode flips from fail-open to fail-closed.
- **`--no-cross-namespace-refs=true`**. Blocks a Kustomization or HelmRelease in namespace A from referencing a Source object that lives in namespace B. Without this, a tenant could point their (correctly impersonated, low-privilege) Kustomization at a *platform-owned* Source in another namespace and pull its artifact content — a confidentiality boundary as much as an RBAC one, since source-controller serves fetched content to anything that references it, independent of the requester's own Git credentials.
- **`--no-remote-bases=true`**, on kustomize-controller specifically. Denies Kustomize remote bases, so every resource a Kustomization applies has to come from the fetched Source's own content — closing the possibility of a tenant's `kustomization.yaml` quietly pulling in a base from some arbitrary external Git URL that platform never reviewed.

With the Flux Operator, this entire trio collapses into one declarative field on the `FluxInstance`: `spec.cluster.multitenant: true` plus `spec.cluster.tenantDefaultServiceAccount`, which the operator translates into the equivalent lockdown automatically. Classic-bootstrap clusters apply the same effect by hand, via a `kustomization.yaml` patch against the controller Deployments during bootstrap — functionally identical, just declared differently:

```yaml
# platform-owned — the manual (pre-operator) equivalent, for context
# clusters/prod-use1/flux-system/kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - gotk-components.yaml
  - gotk-sync.yaml
patches:
  - target:
      kind: Deployment
      name: "(kustomize-controller|helm-controller|notification-controller|image-reflector-controller|image-automation-controller)"
    patch: |
      - op: add
        path: /spec/template/spec/containers/0/args/-
        value: --no-cross-namespace-refs=true
  - target:
      kind: Deployment
      name: "kustomize-controller"
    patch: |
      - op: add
        path: /spec/template/spec/containers/0/args/-
        value: --no-remote-bases=true
  - target:
      kind: Deployment
      name: "(kustomize-controller|helm-controller)"
    patch: |
      - op: add
        path: /spec/template/spec/containers/0/args/-
        value: --default-service-account=default
```

### 5.3 Three patterns, compared

| Pattern | Isolation level | Platform operational load | Typical use |
|---|---|---|---|
| Shared cluster, shared Flux, per-tenant namespace + impersonation | RBAC-enforced per namespace; artifact content isolated via `--no-cross-namespace-refs` | Low — one set of controllers to run and upgrade | The default; what most orgs run at Macdonalds' likely scale |
| Shared cluster, Flux-per-tenant (multiple independent instances) | Near-total, including controller-layer isolation | High — N times the controllers, versions, upgrades to track | Regulatory separation, or tenants needing genuinely independent Flux upgrade cadences |
| Fleet-of-clusters, hub repo | Cluster-level, the strongest boundary available | Bootstrap and template discipline must scale with cluster count | Mature orgs; often layered on top of one of the two rows above, per cluster |

The fleet-of-clusters row deserves a specific mental model: a hub repo defines `clusters/<name>/` per cluster, each with its own `flux-system` bootstrap referencing the *same* shared `infrastructure/` and `tenants/` trees where that makes sense, or cluster-specific overrides where it doesn't. Scaling bootstrap across a growing cluster count is a templating problem — a new cluster should be "copy a cluster directory template, fill in three variables," not a bespoke one-off each time.

### 5.4 Blast radius, stated plainly

This is the framing an HFT platform team should default to, because it's the one that actually answers "what breaks for whom":

A **tenant repo having a bad day** — broken YAML, a failed `cue vet`, a bad artifact push — blocks reconciliation for *that tenant's own* Kustomization or HelmRelease only. It goes `Ready=False`, an alert fires to that team's channel, and every other tenant on the same cluster is completely unaffected. This is the blast-radius containment property doing exactly what it's supposed to do, and it's worth pointing out explicitly in design discussions, because it's the concrete answer to "why do we bother with per-tenant impersonation" that resonates in a room full of people who think in terms of blast radius by default.

A **fleet repo (or a shared platform-owned Source) having a bad day** is the scenario that actually deserves the tight CODEOWNERS and branch protection — a broken change to `infrastructure/` or to the root Kustomization can affect every tenant on the cluster, or every cluster in the fleet if it's hub-repo-shared content.

**source-controller (or any controller) being down** is a different, much less scary failure mode than it sounds: Kubernetes doesn't un-apply anything just because Flux stopped reconciling. Existing workloads keep running exactly as they were. What you lose is *new* changes landing and drift correction — you're flying without autopilot corrections until the controller's back, not falling out of the sky. That distinction is worth having ready for an interview question, because it's the thing people who've never operated Flux tend to get wrong in exactly the scary direction.
## 6. Access Control, RBAC, and Self-Service

### 6.1 Git permissions are your deployment permissions

This is the single sentence to internalize before anything else in this section: in a GitOps model, the question "who can deploy to prod" is no longer answered by cluster RBAC alone — it's answered by **who can merge to the path the production Kustomization watches.** Protecting the fleet repo is therefore your actual change-control gate, not a formality layered on top of one. CODEOWNERS maps `clusters/**` and `infrastructure/**` to the platform team and `tenants/<team>/**` to that team; branch protection on the default branch requires review and forbids force-push and direct pushes; and — the same discipline you already apply to who holds Terraform apply access — the review requirement on cluster-scoped paths should be un-bypassable even by admins, or it isn't actually a control.

Cluster-side RBAC still matters, but it's the second gate, not the first: even a merged, reviewed change lands inside whatever the target Kustomization's impersonated ServiceAccount is allowed to do, per Section 5.

### 6.2 Secrets: SOPS + age

The mechanism you'll almost certainly meet, given Vault is already in your toolkit: **SOPS with an age keypair**, where platform holds the private key — plausibly itself stored in Vault and synced into the cluster as the Secret kustomize-controller reads for decryption at reconcile time — and app teams hold only the **public** key, letting them `sops --encrypt --age <pubkey>` locally and commit ciphertext without ever being able to decrypt it themselves. This is precisely the encrypt-only pattern you'd design with a Vault transit engine and an encrypt-only policy: the team producing the secret never needs, and never gets, the ability to read it back.

Sealed-secrets (Bitnami) is the other common answer — a controller-side asymmetric keypair and a `kubeseal` CLI — and it's a reasonable choice, but it's less portable across clusters than SOPS/age, because a sealed value is bound to the specific sealing key of one controller instance rather than an age key you can provision flexibly per team or per environment. Given SPIFFE/SPIRE and Vault are already load-bearing at Macdonalds, SOPS+age with the age key itself vaulted is the way to bet — confirm rather than assume.

### 6.3 Verification

Two independent trust questions, both worth asking explicitly: "did this come from someone authorized to merge" and "is the artifact I'm about to apply actually the thing that got reviewed." Git commit signing (enforced via GitLab push rules) answers the first. **cosign-based OCI artifact signing, verified by `OCIRepository.spec.verify`, answers the second** — and it's the one people forget, because CODEOWNERS review feels like it should be enough. It isn't: CODEOWNERS controls who can merge source; it says nothing about whether the artifact your CI pipeline built and pushed to Artifactory is actually what got built from that reviewed source, or whether the registry entry got swapped afterward. In a shop running SPIFFE/SPIRE for workload identity, this gap matters more than usual — strong identity for the workload consuming an artifact buys you nothing if the artifact itself was tampered with upstream of the identity check.

### 6.4 Self-service, from the CLI

The `flux` CLI is the entire self-service surface for a team operating within their own guardrails — no UI required, though the Flux Operator's Web UI is available for read-oriented cluster visibility if your team ends up using it.

| Command | What it's for |
|---|---|
| `flux get kustomizations -A` / `flux get helmreleases -A` | Fleet-wide reconciliation status at a glance |
| `flux get kustomizations --status-selector ready=false` | Jump straight to what's broken |
| `flux logs --follow --tail=50` | Controller logs, filterable by kind/namespace |
| `flux reconcile kustomization <name> --with-source` | Force an off-cycle reconcile, re-fetching the source first |
| `flux suspend kustomization <name>` / `flux resume kustomization <name>` | Break-glass pause/resume — see Section 7 for the discipline this requires |
| `flux trace <kind> <name> -n <ns>` | Walk the full dependency chain: Source → Kustomization → object, and *why* something is or isn't reconciling |
| `flux diff kustomization <name> --path <local-dir>` | Dry-run diff against live cluster state — the `terraform plan` of Flux |

`flux trace` and `flux diff` are the two worth genuine muscle memory over the other five — they're the difference between "I read the docs once" and "I can debug a live incident calmly," and both map directly onto reasoning you already do with `terraform plan`.

### 6.5 Conceptual example: what a scoped tenant can and can't do

```yaml
# platform-owned — tenants/quant-desk-a/platform/rbac.yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: quant-desk-a
  namespace: quant-desk-a
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: quant-desk-a-deployer
  namespace: quant-desk-a
rules:
  - apiGroups: ["apps"]
    resources: ["deployments"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: [""]
    resources: ["services", "configmaps"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  # deliberately absent: clusterroles, clusterrolebindings, namespaces, CRDs
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: quant-desk-a-deployer
  namespace: quant-desk-a
subjects:
  - kind: ServiceAccount
    name: quant-desk-a
    namespace: quant-desk-a
roleRef:
  kind: Role
  name: quant-desk-a-deployer
  apiGroup: rbac.authorization.k8s.io
```

If quant-desk-a's rendered artifact somehow contains a ClusterRole — a CUE templating mistake, a copy-pasted example that slipped past review — the Kustomization's apply fails outright with a permission-denied surfaced in its status conditions, and nothing cluster-scoped is created. That failure is the system working correctly, not a bug to route around; the fix is removing the offending resource from source, never widening the tenant's Role to make it pass.
## 7. Delivery and Incident Workflows

### 7.1 A normal release, and where it stalls

Commit merges → GitLab CI builds, tests, scans, renders CUE, packages and pushes an OCI artifact → source-controller notices the new digest (webhook-triggered, poll-interval as fallback) → kustomize-controller reconciles: build, apply, prune → health check evaluates against the target Deployment(s) → notification-controller fires an event to the owning team's channel. Five places this stalls, roughly in order of how often they actually happen in practice:

CI fails silently or the pipeline never runs — the most common "why isn't my change live" ticket, and it has nothing to do with Flux at all; check GitLab first. The artifact pushes successfully but source-controller can't pull it — usually a registry auth issue or a network policy blocking egress from `flux-system` to Artifactory; `flux get sources oci` shows the fetch error directly. Reconciliation applies successfully but the health check never goes green — a pod that applied cleanly but never became Ready, which is the single most common source of "it says Ready=False but nothing looks wrong" confusion for people new to Flux, because the apply genuinely did succeed; it's the workload, not the GitOps layer, that's unhealthy. Notification routing is misconfigured, so success or failure happens silently and nobody finds out until someone goes looking — a genuinely dangerous failure mode because it looks like nothing's wrong. And, less often: `dependsOn` ordering is wrong, so something reconciles before an infra dependency it actually needs is ready, and fails in a way that looks unrelated to the real cause.

### 7.2 Platform-level outages: what still works

The framing from Section 5.4 is worth restating here because it's exactly the calm-under-pressure fact worth having automatic: **source-controller down, kustomize-controller down, or a full GitLab outage does not take your workloads down.** Kubernetes doesn't un-apply anything because the reconciler stopped. What breaks is *new changes landing* and *drift correction* — you're flying without autopilot, not falling. The triage sequence is exactly that framing in order: confirm nothing currently running is actually affected (it almost never is), check controller pod status and logs, check the webhook receiver's reachability if that's the specific symptom (a broken receiver degrades you back to poll-interval, not down entirely — worth confirming the interval is actually a sane safety net and not something someone set to `24h` "temporarily" and forgot about), check GitLab's own status if pulls are failing fleet-wide, and escalate.

The scenario worth war-gaming explicitly, because it's the one people don't think about until they're in it: **an urgent prod fix is needed while GitLab itself is down.** There is no clean GitOps answer to this — by design, the source of truth is unavailable. The answer has to be a documented break-glass procedure: `flux suspend` the affected Kustomization, apply the fix directly, and — critically — get that same fix into Git the moment GitLab's back, then `flux resume` only once the two match, so the reconciler doesn't fight your hotfix on the next tick. This needs to be written down and rehearsed before you need it, not improvised during the outage.

### 7.3 Debugging a failed product-team rollout

`kubectl get kustomization <name> -o yaml` (or the HelmRelease equivalent) and read `status.conditions` — `Ready=False` always comes with a `reason` and a `message` that's usually specific enough to act on directly. `flux get kustomizations --status-selector ready=false` jumps straight there across a namespace or the whole fleet. `flux trace <kind> <name>` walks the full chain — which Source, which revision, which Kustomization, why it is or isn't converging — and is the fastest path to "oh, it's still on the old revision because the webhook never fired" type answers. `flux diff` shows exactly what would change if reconciliation ran right now, without actually applying anything.

For a HelmRelease specifically, the remediation behavior is configurable and worth knowing cold, because the defaults are not "retry forever":

```yaml
# app-team-owned — excerpt
spec:
  install:
    remediation:
      retries: 3
      remediateLastFailure: true
  upgrade:
    remediation:
      retries: 3
      remediateLastFailure: true
      strategy: rollback     # or: uninstall
  rollback:
    cleanupOnFail: true
    timeout: 5m
  driftDetection:
    mode: enabled
    ignore:
      - paths: ["/spec/replicas"]   # e.g. HPA-managed field
        target: {kind: Deployment}
```

With `strategy: rollback`, an upgrade that exhausts its retries rolls back to the last known-good release — the safe default for production. `strategy: uninstall` removes the release entirely and lets the next reconciliation reinstall from scratch, which is the right call when the release state is corrupted beyond what a rollback can fix, but is a heavier hammer and worth reserving for exactly that case. `driftDetection.mode: enabled` is a newer addition worth knowing existed as a genuine gap once — it means helm-controller now also corrects manual `helm upgrade`/`kubectl edit` drift against Helm-managed resources, not just plain-applied ones, closing a real historical asymmetry between how Kustomizations and HelmReleases handled drift.

Suspending during manual triage (`flux suspend hr <name>`) buys you room to investigate without the reconciler fighting you — and Section 2.2's warning about forgetting to resume applies with extra force here, because a suspended HelmRelease during an incident is exactly the kind of thing that's easy to forget once the immediate fire is out.

### 7.4 Drift, deliberately and accidentally

Someone `kubectl edit`s a Deployment in prod under pressure, without suspending first. Flux reverts it on the next tick — could be under a minute, could be up to the configured interval. This is the single most common onboarding surprise for engineers coming from a "we hotfix live and file a ticket after" culture, and it's worth having said out loud before it happens to someone at 2am: **the revert is not a bug. It is Flux doing exactly its job.** The deliberate version of the same action — suspend, edit, fix Git in parallel, confirm they match, resume — is the correct pattern, and fighting the reconciler by re-editing every time it reverts your change is the anti-pattern people fall into under pressure when they don't know the suspend step exists.

<div class="prodbox">
**PROD:** a bad image tag reaching prod is caught by the health check never going green (crashloop) or, if a KubeVela rollout trait with canary/smoke-test steps is in play, by that trait failing before full rollout. The remediation is **`git revert` the offending commit, not a manual in-cluster rollback.** helm-controller's own auto-remediation may already have self-healed the running state if configured, but that's not the same as fixing the *source of truth* — if Git still declares the bad version, the next unrelated change that touches that Kustomization can silently re-apply it. Revert-in-Git is the only fix that's actually durable, because it's the only fix the reconciler itself will also converge to.
</div>

### 7.5 The stuck HelmRelease

A Helm operation interrupted mid-flight — a controller restart during an upgrade being the classic trigger — leaves the underlying Helm release Secret in a `pending-upgrade` or `pending-install` state, which blocks all further Helm operations on that release until it's cleared. This is a Helm behavior, not a Flux one, but Flux inherits it because helm-controller uses the Helm SDK underneath. Recovery: first check whether `upgrade.remediation` is actually configured with reasonable retries and a `strategy` — if it is, modern Flux's own remediation logic often clears this automatically within the configured retry count, which is the whole point of that field existing. If it isn't configured, or retries are exhausted, `helm history <release> -n <ns>` shows the stuck revision, and either a targeted `helm rollback` to the last good revision or — if the state is corrupted beyond that — switching `upgrade.remediation.strategy` to `uninstall` for one cycle so Flux tears down and reinstalls clean from the current Git state, is the standard sequence. Whichever path, do it with reconciliation suspended, confirm the release is healthy again, then resume.
## 8. Architecture Evolution

Three rough stages, worth knowing less as a checklist and more as a way to recognize which stage a given piece of Macdonalds' setup is actually in when you meet it — because different parts of a real platform frequently sit at different stages simultaneously.

| Stage | Rough shape | What forces the next stage |
|---|---|---|
| Initial | One cluster, one repo, `flux bootstrap`, everything platform-managed, a handful of apps | Team count grows past what one CODEOWNERS file and one platform engineer can track by hand |
| Growing | Tenant onboarding model, impersonation RBAC live, SOPS in place, per-team notification routing, monorepo discipline (CODEOWNERS, templates, CI lint) | Repo contention (MR queue pressure across many teams), reconcile latency as tenant count climbs, RBAC sprawl needing periodic review |
| Mature | Fleet-of-clusters from a hub repo, OCI-artifact CI-rendered delivery, signed artifacts, Flux effectively invisible beneath KubeVela/OAM for app teams | Controller resource limits at very high tenant/object counts — this is what Flux's `spec.sharding` on `FluxInstance` exists to address, splitting reconciliation load across multiple controller replicas by label selector rather than one kustomize-controller instance becoming a silent bottleneck |

The specific pain point worth flagging because it's easy to miss until it's already hurting: reconcile-loop starvation. A single kustomize-controller replica handling a fast interval across hundreds of Kustomizations can start queuing, and the symptom looks like "reconciliation is just slow now" rather than anything that points at its actual cause. Sharding, or simply widening base intervals and leaning more on webhook-triggered reconciliation instead of tight polling, are the two levers — and it's worth knowing both exist before you're debugging degraded reconcile times under pressure and reaching for neither.

## 9. Best Practices and Anti-Patterns

### 9.1 Best practices

- Enforce repo layout and naming through templates plus a CI lint gate (`kubeconform`, `kustomize build --dry-run`, an OPA/conftest policy check) — catch violations before merge, not after a reconciliation failure.
- `dependsOn` and health checks on everything that has a real dependency or a meaningful readiness signal; treat a Kustomization or HelmRelease without a health check as incomplete, not optional.
- Enable pruning deliberately, with the team that owns a given Kustomization understanding exactly what it will delete if the source path changes unexpectedly — see the anti-pattern below for what happens when this understanding is missing.
- Tune reconciliation intervals and webhook receivers together: webhooks give you near-immediate reconciliation on push, the interval is the safety net if the webhook breaks, and both need to actually be verified working, not just configured once and assumed.
- Monitor Flux itself, not just what it deploys — reconciliation duration and failure rate, and specifically a scheduled audit for suspended resources, since nothing else will tell you a Kustomization has been silently suspended for three weeks. (VictoriaMetrics scraping the controllers' own `/metrics` endpoints gets you `gotk_reconcile_duration_seconds` and `gotk_reconcile_condition` for free — worth wiring into the same dashboards you're already building for everything else.)
- Rehearse disaster recovery for real: actually rebuild a cluster from Git alone in a game day, not just believe it would work.

### 9.2 Anti-patterns

- Manual `kubectl` changes coexisting with reconciliation, without suspending first — this is a permanent, self-inflicted drift war, not a one-time inconvenience.
- Long-lived environment branches with cherry-pick promotion — see Section 3.2 for why directory or OCI-tag promotion is simply the better version of the same idea.
- Cross-namespace Source references and shared "god repos" with no clear owner — exactly what `--no-cross-namespace-refs` and per-tenant CODEOWNERS exist to prevent.
- Suspended resources forgotten indefinitely — the single most common real-world Flux footgun, and the reason a suspended-resource audit belongs in your monitoring from day one, not added after the first incident it causes.
- Prune disabled out of fear of it deleting the wrong thing — this doesn't remove the risk, it just trades it for orphaned-resource sprawl that's arguably worse, since now nothing is actually tracking what's supposed to exist. The fix for prune-anxiety is a correct `path` and a tested `dependsOn` graph, not turning prune off.

<div class="prodbox">
**PROD:** the real-world version of the prune anti-pattern: someone fat-fingers a Kustomization's `spec.path` during a refactor, pointing it at an empty or near-empty directory. Prune faithfully does its job — everything it previously applied that's no longer in source gets deleted, correctly, exactly as designed, potentially wiping an entire tenant namespace's workloads in one reconcile cycle. This is not a Flux bug. It's why `path` changes on any Kustomization with `prune: true` deserve the same review scrutiny as a Terraform resource deletion, and why `flux diff` before merging a path change is not optional theater.
</div>
## 10. GitLab Repository Layouts, Controller by Controller

You asked specifically to see what the repo actually looks like under each controller — reasonable, because "which controller owns this directory" is exactly the question you'll be answering constantly in week one. Four concrete shapes below: pure Kustomize, pure Helm, the realistic mixed pattern most platform teams actually run, and the CUE/OCI/KubeVela pattern your specific stack points toward.

### 10.1 kustomize-controller only

The shape you'd recognize immediately from any Kustomize base/overlay setup you've already run — the only new element is the `Kustomization` *custom resource* (Flux's CRD) that points at a `path`, which is a different thing from the `kustomization.yaml` *file* Kustomize itself reads at that path. They share a name and it trips people up initially.

```
fleet-repo/
├── clusters/prod-use1/flux-system/
│   ├── flux-instance.yaml              # or gotk-components.yaml + gotk-sync.yaml
│   ├── infrastructure.yaml             # Flux Kustomization -> infrastructure/
│   └── tenants.yaml                    # Flux Kustomization -> tenants/
├── infrastructure/
│   └── cilium/
│       ├── kustomization.yaml          # kustomize CLI file
│       └── bgp-cluster-config.yaml
└── tenants/
    └── quant-desk-a/
        ├── base/
        │   ├── kustomization.yaml
        │   ├── deployment.yaml
        │   └── service.yaml
        └── overlays/
            ├── staging/kustomization.yaml   # patches base, image tag
            └── prod/kustomization.yaml      # patches base, image tag
```

```yaml
# platform-owned — clusters/prod-use1/flux-system/tenants.yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: tenants
  namespace: flux-system
spec:
  interval: 10m
  sourceRef: {kind: GitRepository, name: fleet}
  path: "./tenants"
  prune: true
  dependsOn:
    - name: infrastructure
```

### 10.2 helm-controller only

The shape you'd use for anything that ships as a vendor Helm chart — which, in practice, is most third-party operators, including several in your own Tier-2/3 list.

```
fleet-repo/
├── infrastructure/
│   ├── sources/
│   │   ├── helmrepository-victoriametrics.yaml
│   │   └── ocirepository-internal-charts.yaml
│   └── releases/
│       ├── stackgres/
│       │   ├── helmrelease.yaml
│       │   └── values-prod.yaml
│       └── redpanda/
│           ├── helmrelease.yaml
│           └── values-prod.yaml
```

```yaml
# platform-owned — infrastructure/releases/stackgres/helmrelease.yaml
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: stackgres-operator
  namespace: flux-system
spec:
  interval: 30m
  chart:
    spec:
      chart: stackgres-operator
      version: "1.x"
      sourceRef: {kind: HelmRepository, name: stackgres}
  install:
    remediation: {retries: 3, remediateLastFailure: true}
  upgrade:
    remediation: {retries: 3, remediateLastFailure: true, strategy: rollback}
  valuesFrom:
    - kind: ConfigMap
      name: stackgres-values-prod
```

### 10.3 The mixed pattern — what platform teams actually run

Fighting the ecosystem is wasted effort: if StackGres, Redpanda, and cert-manager-style components ship as Helm charts upstream, hand-converting every one of them to raw manifests just to keep a single-controller purity is work with no payoff. The realistic split is **helm-controller for anything whose upstream distribution format is a Helm chart, kustomize-controller for everything platform or app teams author themselves** — which in your environment means kustomize-controller ends up applying fully CUE-rendered, already-flat YAML with no Kustomize overlay logic actually invoked, per the note in Section 2.1.

```
fleet-repo/
├── infrastructure/
│   ├── releases/            # helm-controller: StackGres, Redpanda, VictoriaMetrics stack
│   └── platform-defs/       # kustomize-controller: CUE-authored ComponentDefinitions etc.
└── tenants/
    └── quant-desk-a/        # kustomize-controller: CI-rendered, OCI-delivered
```

### 10.4 The pattern your stack actually points toward: CUE → OCI → KubeVela, under Flux

```
fleet-repo/
├── clusters/prod-use1/flux-system/
├── infrastructure/
│   └── vela-core/                      # KubeVela's own controller — via Helm or Kustomize
├── platform-defs/                      # ComponentDefinition/TraitDefinition, CUE-authored
│   └── kustomization.yaml              # Flux Kustomization applying these CRD instances
└── tenants/
    └── quant-desk-a/
        ├── ocirepository.yaml          # points at CI-published artifact
        └── kustomization.yaml          # applies the rendered Application + supporting objects
```

```yaml
# app-team-owned — tenants/quant-desk-a/ocirepository.yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: quant-desk-a-checkout
  namespace: flux-system
spec:
  interval: 5m
  url: oci://registry.tower.internal/quant-desk-a/checkout
  ref: {tag: prod-latest}
```

Here, Flux's job is entirely "get the KubeVela `Application` object (and anything else CI rendered) into the cluster and keep it in sync with Git" — it stops there. KubeVela's own controller reconciles from that point: `Application` in, real Deployments/Services/HTTPRoutes out, via its own workflow engine. Two independent reconcilers stacked on top of each other, exactly as described in Section 3.4 — worth re-reading that section alongside this repo layout, since the directory structure only makes sense once the two-controller relationship is clear.

### 10.5 A note on what to actually verify

All four layouts above are standard, defensible shapes — but which one(s) Macdonalds actually runs, and where the Flux/KubeVela boundary specifically sits, is worth confirming directly rather than assuming from this booklet. Section 11 has the exact questions worth asking in week one.
## 11. Mentor's Field Notes

A few things beyond the spec you gave me — where I'd spend the month if I were you.

### 11.1 Clone the reference repo and actually break it

`github.com/fluxcd/flux2-multi-tenancy` is the Flux team's own canonical multi-tenancy example, and it's the single highest-value hands-on artifact for this whole booklet — reading Section 5 gets you the mental model, running this repo in a kind cluster gets you the muscle memory. Don't just read it: clone it, bootstrap it locally, then deliberately try to break tenant isolation — point a tenant Kustomization at another tenant's namespace, omit `serviceAccountName` and see the default account deny everything, try a cross-namespace Source reference and watch it get rejected. Breaking something you built yourself and watching *why* it fails teaches the RBAC boundary faster than any amount of reading.

### 11.2 A suggested five-lab track

Mirroring the structure of your Cilium lab track, scoped to what actually needs hands-on time rather than reading time:

1. **Bootstrap and break drift.** Kind cluster, `flux bootstrap` (or a `FluxInstance`) against a scratch repo. Apply a change via `kubectl edit`, time how long it takes Flux to revert it, then do the same thing correctly with suspend/resume.
2. **Multi-tenancy lockdown.** The `flux2-multi-tenancy` exercise above, end to end.
3. **Break a HelmRelease on purpose.** Deploy something trivial (podinfo is the standard target), set an invalid image tag, watch retries exhaust and remediation kick in under both `strategy: rollback` and `strategy: uninstall`, then recover it cleanly.
4. **Prune a namespace by accident, on purpose.** Point a Kustomization's `path` at an empty directory with `prune: true` set, and watch it happen — this is the fastest way to make Section 9's PROD callout visceral instead of theoretical.
5. **OCI artifact round-trip.** `flux push artifact`, `flux tag artifact`, an `OCIRepository` pulling it, a webhook `Receiver` triggering reconciliation on push instead of waiting for the interval. This is the exercise closest to what your actual day-to-day will look like under the CUE/KubeVela pattern.

Happy to write this out as a full standalone lab booklet, the same way the Cilium track got one, if that's useful.

### 11.3 Two commands to drill until they're automatic

Everything else in the CLI table in Section 6 you'll pick up naturally through use. `flux trace` and `flux diff` are worth deliberately drilling before day one, because they're the two that separate "I've read about Flux" from "I can stay calm during a live incident" — and both map directly onto reasoning you already do constantly with `terraform plan`.

### 11.4 Questions worth asking in week one, specifically

Better to walk in with sharp questions than assumptions dressed up as knowledge. In rough priority order:

- Where exactly does Flux's responsibility end and KubeVela's begin — does a Kustomization apply the `Application` object directly, or does something else in the pipeline talk to KubeVela's API?
- Classic `flux bootstrap`, or the Flux Operator with `FluxInstance`? (Changes how you'd approach an upgrade or a multi-tenancy config change, per Section 2.3.)
- Is image-automation-controller used anywhere in the production path, or only in dev/staging? Worth knowing the house opinion before you form your own out loud.
- SOPS+age, sealed-secrets, or something else — and if SOPS+age, is the age key itself in Vault?
- What's the actual tenant unit in practice — desk, strategy, cluster-per-site — and does cluster topology track physical/colo concerns independently of the tenant boundary?

### 11.5 Further reading, verified current as of this booklet

- `fluxcd.io/flux/installation/configuration/multitenancy/` — the multi-tenancy mechanics in Section 5, from source.
- `github.com/fluxcd/flux2-multi-tenancy` — clone this one, don't just read it.
- `fluxcd.io/flux/components/helm/helmreleases/` — full HelmRelease API reference, including the remediation and drift-detection fields from Section 7.
- `fluxcd.io/flux/cheatsheets/oci-artifacts/` — the `flux push`/`tag artifact` CLI surface behind Section 10.4's CI example.
- `fluxoperator.dev` — Flux Operator and `FluxInstance` docs, for whichever install path Macdonalds turns out to run.
## 12. Production Checklist

### Technical readiness

- [ ] Bootstrap (or `FluxInstance`) is recoverable from Git alone — rehearsed, not assumed
- [ ] Controller resource requests/limits set deliberately; reconciliation duration and failure rate monitored (VictoriaMetrics scraping `gotk_*` metrics)
- [ ] Multi-tenancy lockdown flags active: `--no-cross-namespace-refs`, `--no-remote-bases`, `--default-service-account` (or `FluxInstance.spec.cluster.multitenant: true`)
- [ ] Per-tenant RBAC (Role/RoleBinding/ServiceAccount) exists and is least-privilege, reviewed periodically as tenant count grows
- [ ] Secrets decryption infrastructure live (SOPS/age or sealed-secrets), decryption key custody documented and owned
- [ ] Notification routing verified per tenant — not just configured, actually tested to fire
- [ ] `prune` and `healthChecks` coverage audited across all Kustomizations/HelmReleases, not just the ones someone remembered
- [ ] Suspended-resource audit running on a schedule, alerting if anything's been suspended past a defined threshold
- [ ] Disaster recovery rehearsed: an actual cluster rebuild from Git, not a tabletop exercise

### Organizational readiness

- [ ] Repo layout contract documented and enforced by CI lint, not just convention
- [ ] Onboarding process exists, is fast enough to beat "just let me kubectl it," and points at a working template
- [ ] Every Kustomization and HelmRelease maps to exactly one CODEOWNERS entry — no unowned objects
- [ ] Promotion policy defined and consistently applied (directory or OCI-tag based, per Section 3.2 — not branch-per-env)
- [ ] Break-glass procedure written down and rehearsed, including the discipline step of resuming afterward

### Multi-tenant strategy

- [ ] Tenant unit explicitly defined (team, desk, environment, cluster) and consistently applied
- [ ] Impersonation model in place everywhere — no Kustomization or HelmRelease relying on the controller's own elevated identity outside genuinely platform-owned, cluster-scoped work
- [ ] Fleet-repo change control (CODEOWNERS + branch protection) treated as equivalent in rigor to production deployment permissions, because it is
- [ ] Blast-radius review done per pattern: what a compromised tenant repo can and can't reach, stated explicitly rather than assumed
