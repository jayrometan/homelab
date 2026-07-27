---
title: "KubeVela & OAM for the Macdonalds PaaS Stack"
subtitle: "Volume 2 — Operations & Platform-Owner Playbook"
date: "July 2026"
toc: true
toc-depth: 2
numbersections: true
geometry: "a4paper, margin=2.2cm"
fontsize: 10pt
mainfont: "DejaVu Serif"
sansfont: "DejaVu Sans"
monofont: "DejaVu Sans Mono"
linkcolor: blue
header-includes:
  - \usepackage{fvextra}
  - \setlength{\emergencystretch}{3em}
  - \DefineVerbatimEnvironment{Highlighting}{Verbatim}{breaklines,breakanywhere,commandchars=\\\{\}}
  - \usepackage{etoolbox}
  - \AtBeginEnvironment{longtable}{\small}
---

\newpage

# About This Volume

Volume 1 built the model: OAM's roles, vela-core's parse→render→apply→health loop, the X-Definition system, the workflow boundary, the two Flux architectures, tenancy, and the merge-to-pods anatomy (Chapter 9, referenced constantly below as "the chain"). This volume is the playbook: operating the controller, onboarding tenants and applications, developing and rolling out definitions, debugging the rendering pipeline, day-to-day command patterns, incident runbooks, and governance. Chapters are numbered continuing from Volume 1 (10–18). Assumptions as before: Architecture A (Flux applies Applications) unless stated; ownership tags **[PaaS team] / [App team] / [Shared]**; PROD callouts mark the places that end careers.

\newpage

# Chapter 10 — Operating vela-core

## 10.1 Install and upgrade paths

Two supported paths: the **Helm chart** (`kubevela/vela-core`) and the **vela CLI** (`vela install`, which drives the same chart). On a Flux platform the decision is already made for you by your own principles: vela-core is a platform workload, so it is installed by a **Flux HelmRelease** (or a rendered-manifest Kustomization if the platform bans Helm at runtime) in the platform's own GitOps repo, pinned to an exact chart+image version. Never `vela install` by hand against a real cluster; never let the CLI's version dictate the server's. Addons likewise vendored and applied via Git (Vol 1, 7.1).

Configuration that matters at install time, because changing it later is disruptive: webhook `failurePolicy` (Vol 1, 2.5 — decide eyes-open; `Fail` is safer against bad objects, but couples Flux applies to vela-core availability), controller concurrency (`--concurrent-reconciles` per controller), the ApplicationRevision retention limit, state-keep/GC intervals, and whether authentication/impersonation mode (Vol 1, 6.1) is enabled — the last one is near-impossible to retrofit once tenants depend on the permissive default.

## 10.2 Scaling and sharding

One vela-core replica does the reconciling; a second replica is a warm standby behind leader election, not extra throughput. Throughput levers, in order: reconcile concurrency (CPU-bound on CUE evaluation — vela-core is one of the few controllers where *CPU requests actually matter*), memory (definition cache + revision snapshots; large fleets with big ApplicationRevisions are memory-hungry), and finally **controller sharding** [KubeVela-impl]: vela-core supports splitting the Application controller across shard deployments, with Applications label-assigned (`controller.core.oam.dev/shard-id`) to shards — the master schedules unlabeled apps. Sharding is real but operationally heavy (per-shard health, per-shard upgrade); at the likely scale of this platform (hundreds of Applications, not tens of thousands) tuned concurrency on a single well-resourced deployment is the right call, and sharding is the documented escape hatch you probably never pull.

Watch the metrics from Vol 1, 8.2 to know when you're wrong: sustained workqueue depth with pegged CPU is the signal to add concurrency/CPU; if p99 reconcile time is dominated by a few giant Applications, fix the Applications (split them) before scaling the controller.

## 10.3 Upgrade blast radius

What a vela-core upgrade can change, ranked by nastiness:

1. **CUE evaluator/runtime version.** vela-core embeds a CUE version; upgrades can tighten or change evaluation behavior. A template that rendered on 1.x may error — or worse, render *differently* — on 1.y. Because rendering is continuous, the new behavior applies to the whole fleet at the moment of upgrade, no Git change anywhere. This is the single best argument for the pre-upgrade fleet dry-run in 10.4.
2. **CRD schema/conversion changes.** New vela-core ships new CRDs (Application, definitions, ResourceTracker). Conversion webhooks or storage-version bumps mean a rollback is *not* just re-deploying the old image — old controllers may not read objects written in the new storage version. Snapshot etcd-level state (Velero or a targeted `kubectl get -o yaml` export of vela CRs) before major upgrades.
3. **Default behavior changes** — GC semantics, SSA field-manager naming (a field-manager rename can turn every field Vela owns into a "conflict with foreign manager" on first apply), health-check defaults, webhook rule scope.
4. **Built-in definition updates.** Upgrades can update the stock definitions (`webservice` et al.) if you let them. You should not: your library pins or replaces stock definitions (Chapter 16), so an upgrade cannot silently re-render tenant workloads via a stock-definition bump.

## 10.4 Safe upgrade procedure

The shape, as a runbook skeleton — details per release notes each time:

1. Read release notes for: CUE version change, CRD storage version change, GC/SSA behavior notes, deprecated flags. **[PaaS team]**
2. In a kind/staging cluster with a representative Application corpus (your definition test fixtures from Chapter 12 double as this), upgrade and diff: `vela dry-run` every fixture against old and new, `diff -u` the rendered output. Any unexplained diff blocks the upgrade.
3. Back up vela CRs + note current chart/image versions (rollback target).
4. Upgrade staging via the normal Flux flow; soak; watch reconcile error rate, render duration p99, phase distribution (Vol 1, 8.2) and confirm a no-op reconcile of sample apps produces **zero** child diffs (state-keep should re-apply identical manifests — any diff means changed rendering).
5. Upgrade prod in a quiet window; webhook availability is the step to watch in real time (admission of Applications continues working throughout, or you are blocking Flux).
6. Rollback path: revert the HelmRelease pin. If CRD storage versions moved, rollback additionally requires the documented downgrade steps from the release notes — which is why step 1 exists. If none are documented, the honest rollback is roll-forward with a patched image; plan for that in the change ticket.

> **PROD.** The upgrade does not "deploy anything," yet it changes the renderer under every Application on the platform. Treat vela-core upgrades with the same ceremony as a Cilium agent upgrade: staged, diffed, soaked, with a written rollback and a quiet window. The absence of any visible change immediately after upgrade proves nothing — the changed behavior may only surface at each Application's next *real* reconcile.


\newpage

# Chapter 11 — Tenant and Application Onboarding

## 11.1 The two golden paths

Two distinct lifecycles that must never blur: onboarding a **team** (rare, platform-heavy, mostly automated but approval-gated) and onboarding an **application** (frequent, self-service, app-team-driven). Companies that get platform adoption right make the second path so smooth that teams stop asking for exceptions; the first path is allowed to have a ticket in it.

## 11.2 Onboarding a team

Everything a tenant *is*, as concrete artifacts, in creation order:

1. **Namespace(s)** — `quant-alpha` (or per-env: `quant-alpha-dev`, `-prod` on the respective clusters), with the platform's standard labels (tenant name, cost-center, PSS level).
2. **Quota + limits** — ResourceQuota and LimitRange per namespace. Remember quota violations will surface as Vela apply/health failures (Vol 1, 6.2) — the tenant docs must say so.
3. **Network baseline** — default-deny CiliumNetworkPolicy plus platform-service allowances (DNS, VictoriaMetrics scrape, gateway ingress path). Tenant-level, not per-app.
4. **Tenant ServiceAccount + RBAC** — the SA Flux impersonates for this tenant: write on `applications.core.oam.dev` in the tenant namespace(s), read on the objects tenants may inspect, nothing else.
5. **Flux wiring** — a GitRepository (tenant deploy repo, deploy key) and per-env Kustomizations with `spec.serviceAccountName` = tenant SA, path per environment, and the lockdown flags already global on the controllers.
6. **Repo scaffolding** — the tenant deploy repo created from a template: per-env directories, CODEOWNERS (platform team on `flux/`-adjacent files if any, tenant leads on apps), MR approval rules (prod dir requires review), CI includes for validation jobs (11.3).
7. **Docs handshake** — the tenant contract (11.5) linked in the repo README.

The pattern worth stealing: steps 1–5 are themselves a **`tenant` Vela Application** in the *platform's* namespace, built on a platform-internal ComponentDefinition (`tenant-baseline`) whose parameters are team name, environments, quota tier, and owner group. This gives tenant infrastructure the same revision history, GC, and status model as everything else — deleting the tenant Application decommissions the tenant, quota-tier changes are a one-line MR with an ApplicationRevision trail. Namespaces and other genuinely shared objects go under a `shared-resource` policy so two tenant Applications can't fight over cluster-scoped objects (Vol 1, 4.1). Step 6 stays in CI/Terraform-for-GitLab land — GitLab-side state is a different provider domain; don't teach Vela to manage GitLab. **[PaaS team]** owns this whole chapter; the only tenant action is naming their leads.

Self-service vs ticket-gated, drawn explicitly: new tenant = ticket/approval (it allocates quota — a capacity decision); new app within a tenant = pure self-service; quota *increase* = MR to the tenant Application parameters + platform approval via CODEOWNERS — the approval is a Git review, not a ticket system, which keeps the audit trail unified.

## 11.3 Onboarding an application

The app-team experience, end to end, as the docs you'll publish should describe it:

1. `vela def list` (or the platform catalog page rendered from definition annotations) to pick component/trait types.
2. Scaffold from the app template: a directory `apps/<app>/` per environment containing one `application.yaml` — nothing else. First manifest for the running example:

```yaml
apiVersion: core.oam.dev/v1beta1
kind: Application
metadata:
  name: pricer
  namespace: quant-alpha
spec:
  components:
    - name: pricer
      type: webservice          # platform definition, pinned: webservice@v3
      properties:
        image: artifactory.internal/quant/pricer@sha256:9f00...
        replicas: 2
        port: 8080
      traits:
        - type: gateway
          properties: { host: pricer.quant-alpha.internal, port: 8080 }
        - type: vm-scrape
          properties: { path: /metrics }
    - name: pricer-db
      type: postgres-cluster    # wraps SGCluster; Chapter 12's worked example
      properties: { instances: 2, storageGiB: 50, profile: small }
```

3. MR → CI validation (schema + `vela dry-run` against pinned definitions in the hybrid model) → review → merge → the Vol 1 Chapter 9 chain runs → `vela status pricer` green. First deploy should take minutes, and the docs should include the expected `vela status` output so the team knows what "done" looks like.

Per-environment promotion — the structural decision: **directories, not branches.** Branch-per-env forces merge-order gymnastics and diverging histories; directory-per-env (`apps/pricer/dev/`, `apps/pricer/prod/`, each targeted by that env's Flux Kustomization) makes promotion an explicit, diffable MR that copies/edits the prod file — usually just the image digest line. Overlay-style sharing (one base + per-env patches) is tempting but reintroduces Kustomize-style indirection above Vela's abstraction; with Applications this small, plain per-env files with a little duplication read better in review, and review legibility is the currency here. Vela's `override` policy as the env mechanism (Vol 1, 4.1) is the one to argue *against* in this shop: it moves env differences out of the env's Git path into the Application body, and the "what is different in prod" question should be answerable with `diff dev/ prod/`.

## 11.4 Migrating an existing app without downtime

Three cases, ordered by how often you'll meet them:

**Raw manifests today.** Write/choose the target definition, `vela dry-run` the candidate Application, and diff rendered output against live manifests until the delta is only labels/annotations. Then the adoption question: creating the Application while the old Deployment exists means Vela SSA-applies over it — SSA merges by field, so a rendered spec that matches live spec is a no-op rollout (names must match exactly; if the definition derives names from `context.name`, choose the component name to hit the existing object). Vela takes field ownership on apply; the resource is now in the ResourceTracker; retire the old apply pipeline in the same MR so nothing else fights for the fields. If names *can't* match, it's a blue-green: deploy the Vela copy alongside, shift the Gateway route, delete the old. [KubeVela-impl] Vela's resource-adoption tooling (`vela adopt`) can generate the wrapping Application from live resources — useful as a starting point; review its output rather than trusting it.

**Helm today.** Two-step: bridge, then rewrite. Bridge = the `helm` component type via the fluxcd addon (Vol 1, 5.5) so the app enters Application-shaped management with its chart intact — acceptable *temporarily* because it preserves the status/GC umbrella while you work. Rewrite = a native definition capturing what the chart actually did, migrated as above. Put the demolition date for the bridge in the migration MR.

**The zero-downtime invariant** for all cases: at every step, exactly one controller believes it owns the pod-template fields. The outage-shaped mistake is the overlap period where old CD and Vela alternate applying different specs — a rollout ping-pong that looks like flapping. Sequence ownership transfer, never let it be concurrent.

## 11.5 The tenant contract

Publish a short document — one page, versioned in the docs repo — stating: what the platform guarantees (definitions are semver'd and breaking changes follow Chapter 16's process; deploy latency expectations; support channels and paging boundaries); what tenants must do (all changes via Git, no kubectl writes; pin definition versions in prod; keep Applications under review rules); and the escalation split (**app `unhealthy` with green platform dashboards → app team first; anything involving phase stuck, rendering errors, or `flux get` red → platform team**). The contract is what makes "whose move is it" answerable at 3am; every runbook in Chapter 15 ends by pointing at it.


\newpage

# Chapter 12 — The Definition Development Lifecycle

The worked example throughout: `postgres-cluster`, a ComponentDefinition wrapping StackGres's SGCluster — the cross-product artifact you own both sides of.

## 12.1 Authoring: postgres-cluster wrapping SGCluster

Design decisions before any CUE is written, because parameter surface is API and API is forever:

- **Expose profiles, not knobs.** App teams pick `profile: small|medium|large`; the definition maps profiles to SGInstanceProfile references, resource shapes, and pgconfig baselines your team maintains. Exposing raw CPU/memory/postgresql.conf invites both quota fights and untested configurations.
- **Own the operational fields absolutely.** Backups (SGObjectStorage/SGBackupConfig wiring), pod anti-affinity, distributed-logs settings, the SGCluster version pin — rendered from platform constants, not parameters. A parameter for `postgresVersion` constrained to the two versions you actually support is the compromise position.
- **Decide the day-2 keyhole** (Vol 1, 1.4): expose `instances` (safe: operator handles scale-out), expose restore-from-backup as a creation-time parameter (a new cluster from a backup is provisioning), and do **not** expose switchover/upgrade/vacuum verbs — those are SGDbOps objects with their own workflow, routed through the DBA path. Write this split into the definition's description annotation so `vela def show` teaches it.

Skeleton (abridged to the structural points):

```cue
// postgres-cluster.cue  — source of truth lives as .cue in Git, not YAML
"postgres-cluster": {
  type: "component"
  attributes: workload: definition: {
    apiVersion: "stackgres.io/v1", kind: "SGCluster"
  }
}
template: {
  parameter: {
    instances:  *2 | int & >=1 & <=5
    storageGiB: *20 | int & >=10 & <=500
    profile:    *"small" | "small" | "medium" | "large"
    pgVersion:  *"16" | "16" | "15"
    restoreFromBackup?: string        // SGBackup name, creation-time only
  }
  _profileMap: { small: "size-s", medium: "size-m", large: "size-l" }
  output: {
    apiVersion: "stackgres.io/v1"
    kind:       "SGCluster"
    metadata: name: context.name
    spec: {
      instances: parameter.instances
      postgres: version: parameter.pgVersion
      sgInstanceProfile: _profileMap[parameter.profile]
      pods: persistentVolume: size: "\(parameter.storageGiB)Gi"
      configurations: sgBackupConfig: "platform-backup"   // platform-owned
      if parameter.restoreFromBackup != _|_ {
        initialData: restore: fromBackup: name: parameter.restoreFromBackup
      }
    }
  }
  outputs: conn: {   // normalize the connection secret location for app teams
    apiVersion: "v1", kind: "Secret"
    // ... projecting StackGres's generated secret into the app's expected
    // name, per platform convention
  }
}
```

The `healthPolicy` keys off SGCluster conditions + `status.podStatuses` ready counts so `vela status` answers "is my database up" (Vol 1, 8.1). SSA discipline per Vol 1, 2.4: render *only* parameter-determined fields; the StackGres webhook defaults the rest, and rendering defaulted fields buys you a permanent drift fight with the operator.

## 12.2 Local development loop

Templates live as `.cue` files in the definitions repo — never authored as YAML string blobs — with `vela def` doing the round-trip:

- `vela def init postgres-cluster -t component --template-yaml` scaffolds; you won't use it twice.
- `vela def vet postgres-cluster.cue` — syntax/structure lint.
- `vela def render` / **`vela dry-run -f test-app.yaml -d postgres-cluster.cue`** — the core loop: a fixture Application in, concrete YAML out, eyeball or diff. Dry-run evaluates the same code path as the controller [KubeVela-impl], which is what makes it trustworthy.
- `vela def apply postgres-cluster.cue --dry-run` shows the CRD-wrapped form that will be committed; `vela def apply` against a kind cluster for live testing; `vela def get -o cue` to confirm round-trip fidelity.

Local kind + the StackGres operator + a stub SGInstanceProfile set is a complete test rig for this definition; your existing kind/FRR lab habits carry over directly.

## 12.3 Testing definitions properly

Three layers, all in CI on the definitions repo:

1. **CUE-level unit tests.** Pure `cue vet`/`cue eval` against the template package: for each fixture parameter set, unify and assert on rendered fields — no cluster, milliseconds fast. This is where you exercise every conditional branch (`restoreFromBackup` present/absent), because the evaluator only errors on demanded bottoms (Vol 1, 3.2) and only your matrix demands them.
2. **Render tests.** `vela dry-run` each fixture Application against the definition; `diff -u` against **golden files** committed in the repo. A definition MR whose golden diffs are unexplained is rejected — this single gate catches the "conditional quietly became false, resource vanished, fleet-wide GC" class before merge (Vol 1, 2.3). Fixtures must include: minimal parameters, maximal parameters, each profile, each version, restore path, and one *invalid* fixture per constraint asserting that rendering *fails* (negative tests keep your error surface honest).
3. **Cluster tests** (nightly / pre-release, kind): apply definition, deploy fixture apps, assert phase reaches `running`, mutate parameters, assert rollout + GC behavior, delete, assert clean GC. Slow, so not per-MR — but it is the only layer that tests ResourceTracker/GC semantics at all.

## 12.4 Staged rollout of a definition change

The procedure that turns "publishing an edit re-renders the fleet" (Vol 1, 3.5) into a non-event:

1. MR to definitions repo → CI layers 1–2 → review by a second platform engineer (golden diffs are the review artifact).
2. Merge → Flux applies the definition to **staging** cluster first (definitions flow through GitOps like everything else; per-cluster paths give you the stage). Cluster tests + soak.
3. Prod apply. If Applications pin (`postgres-cluster@v4`), applying v5 changes *nothing* yet — the fleet is inert. Migration is then per-cohort: canary tenant MRs bump the pin, soak, then the rest. If the fleet is unpinned, the prod apply *is* the fleet re-render — acceptable only for changes whose golden diffs were empty (pure-additive parameters, comment/description changes); anything that changes rendered output for existing parameter sets **must** ride the pinned path.
4. Watch during rollout: render error rate, phase distribution deltas, and — the specific tell for GC accidents — deletion events on resources labeled with the definition's component type (`kubectl get events -A --field-selector reason=...` filtered on the type, or the VictoriaLogs equivalent).

## 12.5 Deprecation and removal

Definitions still referenced cannot be deleted safely — a missing definition fails every referencing Application at parse (Vol 1, 2.2). Sequence: mark deprecated (description annotation + docs + a CI warning when new Applications reference it) → migrate references (tenant MRs, tracked to zero via a `kubectl get applications -A -o json | jq` scan for the type) → only at zero references, delete — and even then keep DefinitionRevisions until no ApplicationRevision within the retention window snapshots it, so rollbacks stay renderable. "Reference count → zero → delete" is the entire rule; the work is the migration campaign, which is Chapter 16's subject.


\newpage

# Chapter 13 — Debugging the Rendering Pipeline

The owner's core skill: given a symptom, name the stalled or lying layer in the Vol 1 Chapter 9 chain, then prove it with one command. Two master symptoms cover most tickets.

## 13.1 "I merged and nothing happened"

Walk the chain forward; each step either passes (move on) or is the answer:

1. **Is the commit on the deployed branch/path?** `git log` — wrong branch, unmerged MR, or a path the Kustomization doesn't include. Embarrassingly common; check first.
2. **Did source-controller see it?** `flux get sources git` — revision should show the SHA. Stale: webhook Receiver broken (fell back to interval — wait or `flux reconcile source git <name>`), or fetch/auth errors in the source-controller status.
3. **Did kustomize-controller apply?** `flux get ks <tenant>` — lastAppliedRevision vs the SHA. Failing: the *whole* Kustomization fails as a unit, so a broken sibling object blocks your Application (Vol 1, 9.4); the error names it. Also remember webhook-of-Application failures surface *here* as apply errors — if vela-core is down with `failurePolicy: Fail`, this is where the chain visibly breaks (Vol 1, 2.5).
4. **Did the Application object change?** `kubectl get app pricer -o jsonpath='{.metadata.generation} {.status.observedGeneration}'` — generation unchanged means Flux applied a no-op (the merged change didn't actually alter the object: wrong file, wrong field, YAML anchor mistake). Generation bumped but observedGeneration lagging means vela-core hasn't processed it: controller down/degraded — check its pods and workqueue metrics (Vol 1, 8.2).
5. **Did it render?** `vela status pricer` / phase. `rendering` with an error message: parse or CUE failure — go to 13.2. New ApplicationRevision exists (`kubectl get apprev -l app.oam.dev/name=pricer`)? If yes, rendering succeeded.
6. **Did the workflow run / apply succeed?** `vela status --detail`: step statuses; suspended (approval gate someone forgot? failed step auto-suspend?); apply errors (quota, RBAC, child-CRD webhook down — e.g. the StackGres webhook rejecting an SGCluster surfaces *here*, as a Vela apply error, and the message contains the operator's rejection).
7. **Did the workload roll out?** Standard Kubernetes from here: `kubectl rollout status`, events, image pull, probes. Vela shows `runningWorkflow`/`unhealthy` while waiting; the cause is beneath the abstraction now.

Ten minutes, seven checks, and every one distinguishes "platform's move" from "app team's move" — steps 1 and 7 are theirs, 2–6 are yours.

## 13.2 "It rendered wrong" — CUE failure archaeology

How CUE errors surface, from loud to silent [KubeVela-impl]:

- **Loud:** unification conflicts and unsatisfied constraints (`properties.replicas: invalid value 50 (out of bound <=20)`) land in Application status conditions and `vela status` — with the definition name and path. These are the good ones.
- **Quiet:** incomplete values demanded at output (`field X: incomplete value string`) — usually a parameter the definition demands but doesn't default, or `context` field assumed present that isn't in this evaluation mode. The message names the path but not *why* it's incomplete; reproduce with `vela dry-run` locally and `cue eval` the template with the fixture to see the partial value.
- **Silent (the dangerous class):** conditionals that evaluate false and *omit* output (no error, resource missing → GC), patches that match nothing (no error, trait ineffective — Vol 1, 3.3), and defaults masking a typo'd parameter *when the parameter struct is accidentally open* (the field lands nowhere, the default wins, closedness would have caught it — this is why parameter closedness is a review gate).

The toolkit, in escalation order: **`vela dry-run -f app.yaml`** (render locally against live or file definitions — first stop always); **`vela status --detail` / `--tree`** (what the controller believes, component by component); **`vela debug`** (interactive per-component rendered-output inspection); **ApplicationRevision diffing** — `kubectl get apprev pricer-v8 -o yaml` vs `-v9`, or `vela live-diff` to compare a candidate against the running revision: the revisions contain the definition snapshots, so this diff distinguishes "app changed" from "definition changed under the app" with certainty — the single most valuable forensic property of the whole system; **ResourceTracker inspection** — `kubectl get resourcetracker -l app.oam.dev/name=pricer -o yaml` for what Vela believes it owns vs `kubectl get` reality.

## 13.3 Stuck states

- **Stuck `deleting`:** the Application's finalizer won't release until ResourceTracker contents are gone; something in the ledger won't delete — a child with its own stuck finalizer (SGCluster waiting on the StackGres operator, PVC protection) or a child in a namespace/cluster no longer reachable. Find it: the ResourceTracker lists everything; `kubectl get` each until you find the survivor; fix *its* deletion. Removing the Application's finalizer by hand is the last resort and orphans everything still listed — runbook 15.3 covers when that's acceptable.
- **Workflow suspended and nobody knows why:** `vela workflow status` — distinguishes explicit suspend steps from failure-induced suspension (`suspending` after max retries). Resume with `vela workflow resume` only after reading *why* it suspended; `vela workflow restart` re-runs from the top (idempotency of your steps suddenly matters).
- **GC pending / old+new coexisting:** normal briefly during transitions; persisting means apply of the new revision hasn't completed (health gate failing holds GC of the old, by design — steady-state guarantee) or GC hit an error (deletion RBAC, finalizers). `vela status` + tracker comparison shows which.

\newpage

# Chapter 14 — Day-to-Day Command Patterns

Kept deliberately terse — this is the page to print.

## 14.1 vela CLI worth memorizing

```bash
vela status <app> [-n ns] [--tree|--detail|--endpoint]   # first command, always
vela dry-run -f app.yaml [-d dir/]     # render locally; -d = candidate defs
vela live-diff -f app.yaml             # candidate vs running revision
vela debug <app>                       # interactive rendered-output browser
vela logs <app> [--component c]        # child pod logs without label hunting
vela exec <app> -- sh                  # ditto, exec
vela port-forward <app>                # ditto, port-forward
vela revision list <app>               # ApplicationRevision history
vela workflow status|suspend|resume|restart <app>
vela def list|show <type>|vet f.cue|apply f.cue [--dry-run]
vela top                               # fleet overview TUI
```

## 14.2 kubectl patterns against the Vela CRs

```bash
# Fleet phase survey (triage during incidents):
kubectl get applications -A -o custom-columns=\
  NS:.metadata.namespace,APP:.metadata.name,\
  PHASE:.status.status,\
  GEN:.metadata.generation,OBS:.status.observedGeneration

# Who references definition X (pre-change impact set):
kubectl get applications -A -o json | jq -r '.items[] |
  select(.spec.components[].type|startswith("postgres-cluster")) |
  "\(.metadata.namespace)/\(.metadata.name)"'

# Revision history + what definition versions v9 snapshotted:
kubectl get apprev -n quant-alpha -l app.oam.dev/name=pricer
kubectl get apprev pricer-v9 -n quant-alpha -o jsonpath='{.spec.componentDefinitions}' | jq 'keys'

# The ownership ledger:
kubectl get resourcetracker -l app.oam.dev/name=pricer -o yaml
```

## 14.3 "What exactly did this Application create, and why," from resources alone

The interview-grade drill, no CLI sugar: (1) ResourceTracker for the app = the *claimed* set. (2) `kubectl get <each>` = the *actual* set; discrepancies are GC-pending or drift. (3) For any child, `metadata.labels['app.oam.dev/component']` names the component; the current ApplicationRevision's snapshot of that component's definition + the Application's properties for it are the complete *why* — you can re-derive the manifest by hand with `cue eval` from those two inputs and nothing else. That closed loop (ledger → objects → revision → re-derivable render) is the property that makes this layer auditable in a way Helm never was; being able to demonstrate it end-to-end is how you'll know you own the system.


\newpage

# Chapter 15 — Incident Runbooks

Format per runbook: symptom → blast radius → diagnosis → resolution → **whose move** → escalation. All assume Architecture A. "The chain" = Vol 1 Chapter 9.

## 15.1 vela-core down or crash-looping

**Symptom.** Pods CrashLoopBackOff / deployment unavailable; fleet `observedGeneration` frozen; possibly Flux Kustomizations erroring on Application applies (webhook, `failurePolicy: Fail`).
**Blast radius.** Running workloads: **unaffected** — say this first on the incident call. Stopped: new deploys, GC, drift correction, status/health updates, and possibly all Application admission (→ Flux applies fail platform-wide).
**Diagnosis.** Pod logs: panic on a malformed CR (which one — it's named), OOM (memory limits vs fleet size), CRD/schema mismatch after an upgrade (10.3), leader-election/API-server issues.
**Resolution.** OOM → raise limits (GitOps MR; `kubectl edit` only under incident exception with follow-up MR). Panic-on-object → quarantine the object (annotate/pause or delete the offending Application if the tenant agrees) then fix root cause. Post-upgrade → roll back per 10.4 step 6. If admission is blocking Flux fleet-wide and restoration is not quick: consciously flip webhook to `failurePolicy: Ignore` (documented break-glass, restore after) — unvalidated Applications for an hour beat a frozen platform.
**Whose move.** [PaaS team] entirely. Tenants informed: "deploys paused, running services unaffected."
**Escalation.** If CRD state is corrupted (conversion errors), stop, snapshot, involve whoever owns etcd backups before any further writes.

## 15.2 A definition change broke rendering fleet-wide

**Symptom.** Minutes after a definitions merge: render-error rate spike, many Applications `rendering`/error phase, tenants report frozen deploys. (If the change *rendered successfully but wrongly*, see 15.3/15.4 instead — this runbook is the loud version.)
**Blast radius.** Every unpinned Application referencing the definition, on its next reconcile. Already-running pods unaffected *until* something triggers re-apply of bad output.
**Diagnosis.** `vela status` on one victim: error names definition and CUE path. Correlate merge time vs error onset. Confirm impact set with the 14.2 reference query.
**Resolution.** **Revert the definitions MR** — this is why definitions ship via GitOps: the revert flows through the same chain and the fleet re-renders back. Do not hand-edit the live definition (Flux reverts you). After recovery, victims re-render automatically; spot-check phases return `running` and that no GC occurred during the window (14.3 drill on a sample). Post-incident: why did CI golden tests not catch it — a fixture gap to close, every time.
**Whose move.** [PaaS team]. This is your outage even though tenants see it.
**Escalation.** If the bad revision *deleted* resources before revert (rendered-output shrank), escalate to 15.3 immediately — reverting restores rendering, not deleted state.

## 15.3 GC removed something it shouldn't have / refuses to remove something

**Removed wrongly — symptom.** A resource vanished fleet- or app-wide after a definition or Application change; users report missing Services/Secrets/monitors; deletion events at the change time.
**Blast radius.** Whatever the resource did — potentially traffic-affecting (a Service, an HTTPRoute).
**Diagnosis.** The GC contract (Vol 1, 2.3): removal from rendered output *is* a deletion order. Diff the ApplicationRevisions around the event (`vela live-diff` / apprev diff) — you will find the conditional or removed block that stopped emitting the resource.
**Resolution.** Restore emission (revert the change) → next reconcile recreates. Data-bearing casualties are the hard case: an SGCluster GC'd means you are in StackGres restore territory (from SGBackups) — which is why the `postgres-cluster` fixtures include a "resource present in every profile's golden file" assertion, and why a `garbage-collect` policy with `keepLegacyResource`/rules protecting data-bearing kinds belongs in the paved-road Application template for anything stateful. Add it retroactively now if it wasn't there.
**Refuses to remove — symptom.** Application stuck `deleting`, or superseded children lingering. Walk 13.3: find the survivor via the ResourceTracker, fix its finalizer/RBAC. Hand-removing the *Application's* finalizer is acceptable only when every tracked child is confirmed handled or intentionally orphaned — record the orphan list in the incident notes, because the ledger is about to be deleted with the app.
**Whose move.** [PaaS team] mechanics; [App team]/[Shared] decisions on restoring data.

## 15.4 Drift fight: Vela vs another controller over the same fields

**Symptom.** A value flaps on a cadence (replicas snapping back — HPA vs rendered `replicas`; an SGCluster field ping-ponging — operator default vs rendered field). Rollout churn without user changes; SSA conflict messages naming field managers in vela-core logs.
**Blast radius.** The affected workloads — churn can be traffic-affecting (repeated rollouts) even when values are "just" flapping.
**Diagnosis.** `kubectl get <obj> -o yaml --show-managed-fields`: two managers claiming the path, timestamps alternating. Identify the pair; the fix differs by pair.
**Resolution.** The invariant: **exactly one owner per field** (Vol 1, 2.4). HPA-vs-replicas → definition must omit `replicas` when autoscaling (definition fix, staged per 12.4). Operator-default-vs-render → stop rendering the field. Flux-vs-Vela on the same manifest → an architecture violation: something is applied by both paths (usually a migration leftover, 11.4) — remove it from one. Short-term mitigation while the fix lands: `apply-once` policy scoped to the contested path stops Vela's re-assertion without disabling state-keep wholesale.
**Whose move.** [PaaS team] — field-ownership design is definition design.

## 15.5 Workflow suspended / stuck mid-rollout

**Symptom.** Application `workflowSuspending` or `runningWorkflow` beyond expectation; deploy half-landed (canary at 10%, or component group A live and B absent).
**Blast radius.** That Application; but a half-rolled-out state can itself be the incident (old+new serving simultaneously).
**Diagnosis.** `vela workflow status`: explicit gate awaiting approval vs failure-suspend (step name + error: health timeout, apply failure) vs a step retry-looping.
**Resolution.** Gate → confirm the human process, resume. Failure-suspend → fix the underlying cause (it is one of 13.1's steps 6–7), then `resume`; `restart` only if steps are idempotent — and your paved-road workflows should only ever contain idempotent steps precisely so this decision is trivial at 3am. If the half-state is harming traffic and the fix isn't quick: roll back via Git (revert the app MR) — the reverted spec renders, workflow restarts toward the old state; this beats imperative surgery every time it's available.
**Whose move.** [Shared]: platform diagnoses the layer, app team owns the go/no-go on resume vs revert.

## 15.6 CRD / webhook issues after an upgrade

**Symptom.** Post-upgrade: Application applies rejected (admission errors), conversion webhook errors in API server logs, `vela` CLI schema complaints, or controllers logging unknown-field errors.
**Blast radius.** Potentially all Application writes (→ Flux fleet-wide, again), or specific CRs unreadable.
**Diagnosis.** Which webhook (mutating/validating/conversion) via the API server error text; `kubectl get crd applications.core.oam.dev -o yaml` storage/served versions vs what the running controller expects; cert expiry on webhook TLS (a classic that looks like an upgrade issue but is really cert rotation).
**Resolution.** Version skew → complete the upgrade or roll back *coherently* (controller and CRDs as a set, 10.3/10.4). Conversion failures on stored objects → the release notes' migration steps; do not hand-edit stored CRs before snapshotting. Cert expiry → re-issue (cert-manager or the chart's cert job), no version action needed.
**Whose move.** [PaaS team]; freeze definition merges and tenant deploy expectations until admission is stable.


\newpage

# Chapter 16 — Governance of the Definitions Library

## 16.1 Repo layout

One repo, platform-owned, structured so the blast radius of a directory is legible from its path:

```
platform-definitions/
├── components/
│   ├── webservice/          # webservice.cue, fixtures/, golden/, README.md
│   ├── postgres-cluster/
│   └── redpanda-topic/
├── traits/
│   ├── gateway/
│   └── vm-scrape/
├── policies/
├── workflowsteps/
├── lib/                     # shared CUE packages: labels, profiles, naming
├── tests/                   # cross-definition fixtures (whole Applications)
└── ci/                      # vet -> unit -> dry-run/golden -> (nightly) kind
```

`lib/` deserves emphasis: shared constants (label schema, profile maps, registry prefixes) imported by every definition, so conventions change in one place — with the corollary that **a `lib/` change re-renders everything**, and CI must therefore run the golden suite for *all* definitions on any `lib/` diff, not just the touched directory.

## 16.2 Review gates and versioning policy

Every MR: `vela def vet` + CUE unit tests + full dry-run/golden diff, with golden diffs pasted into the MR as the review artifact; two-platform-engineer review for anything touching `parameter` schemas or `lib/`; CODEOWNERS enforcing it. Versioning is semver *communicated through the definition description and changelog*, mapped onto DefinitionRevisions: **patch** = rendering-identical for all existing parameter sets (empty golden diff) — may ship unpinned; **minor** = additive parameters, existing sets render identically — may ship unpinned; **major** = anything that changes rendered output for an existing valid parameter set, removes/renames a parameter, or tightens a constraint that could invalidate existing Applications — pinned rollout only (12.4), migration campaign required. The golden-diff-empty test *is* the mechanical definition of "non-breaking"; you never argue about it in review, you read the diff.

## 16.3 The testing matrix

Maintain, per definition, a fixtures directory that spans: every enum value (profiles, versions), every optional parameter present and absent, minimum and maximum of every bounded numeric, each supported trait combination the definition claims (`appliesToWorkloads` honesty check), and the negative fixtures (12.3). The matrix is versioned with the definition; a new parameter without new fixtures fails CI by policy. This matrix is also your upgrade corpus (10.4) — one artifact, three duties.

## 16.4 Running a breaking change without a flag day

The campaign pattern for "postgres-cluster v5 changes the backup wiring" across forty Applications and six tenant teams:

1. Ship v5 alongside v4 (pinned world: both exist; nothing moves). Publish the migration note: what changes, what tenants must edit (often nothing but the pin), the deadline, the rollback (re-pin v4).
2. Migrate the canary cohort yourself via MRs to their repos (platform-authored, tenant-approved — you write the diff, they click approve; this respects the Git boundary while removing tenant toil). Soak with the 12.4 watch list.
3. Tranche the remainder — by tenant, worst-case-first or least-critical-first per the change's risk shape. Automation: a script generating the pin-bump MRs across repos beats asking six teams to do homework, and its MR descriptions carry the migration note.
4. Track stragglers with the 14.2 reference query as a dashboard number ("Applications on postgres-cluster@v4: 3"). At zero, start the 12.5 deprecation clock on v4.

The social contract that makes this work is in the tenant docs (11.5): the platform may require pin bumps with N weeks' notice; in exchange, unannounced re-renders never happen. Hold both sides of it.

\newpage

# Chapter 17 — Sharp Edges

The collected gotchas, each one paragraph: mechanism, symptom, defense. Several were introduced in context earlier; this is the consolidated reference to re-read before any definitions release.

**Silent GC via vanished output.** A CUE conditional evaluating false, a refactor dropping an `outputs` block, or a typo'd guard removes a resource from rendered output — and removal is a deletion order (Vol 1, 2.3). Symptom: resources deleted fleet-wide with no error anywhere. Defense: golden-file diffs as a merge gate (12.3), `garbage-collect` protection rules on data-bearing kinds, deletion-event alerting on protected types.

**Revision-limit trimming eats your rollback target.** ApplicationRevision retention (default ~10) trims oldest; a busy Application can trim past the last-known-good during an extended incident, and with it the definition snapshots you'd roll back to. Symptom: "roll back to v3" — v3 no longer exists. Defense: raise the limit for Tier-1 apps; remember Git holds the app spec but only the revision held the *definition* snapshot — your definitions repo history is the recovery path then, so definitions repo tags per release matter.

**SSA field-manager conflicts after renames.** Controller upgrades or migration leftovers change the field manager string; the same logical owner now looks foreign, and applies either conflict loudly or silently co-own. Symptom: apply errors naming a manager that "is us," or `managedFields` bloat with alternating timestamps (15.4). Defense: check release notes for manager changes; `--show-managed-fields` is the oracle; force-apply (`--force-conflicts` semantics via controller config) only as a deliberate, logged decision.

**Definitions silently resolving to older revisions.** A pinned reference (`type: x@v3`) outlives the intent to pin, or a namespace-local shadow definition (Vol 1, 3.6) intercepts resolution — the Application renders against something other than "latest in vela-system" and nobody remembers why. Symptom: a fixed bug that "came back," or one namespace behaving differently. Defense: the ApplicationRevision snapshot tells you *exactly* what was used — check it before trusting any assumption; CI lint flagging shadow definitions outside canary namespaces; a fleet report of pin distribution (14.2 query variant).

**CUE that produces empty output instead of failure.** The family: unmatched patches (Vol 1, 3.3), false conditionals, open-struct parameter typos absorbing user input while defaults win (13.2). Common shape: *the system is working perfectly and doing nothing you intended.* Defense: closed parameter structs (non-negotiable review rule), `appliesToWorkloads` strictness, negative fixtures, and the habit of reading `vela dry-run` output rather than assuming.

**Ordering assumptions in multi-component apps.** Components render and apply per the workflow's grouping; "the Secret exists before the pod that mounts it" holds only if expressed — via `dependsOn`, output references (component B consuming `outputs` of A creates an edge), or workflow step ordering. Symptom: first deploy of a multi-component app flaps (pod CrashLoops until the SGCluster secret materializes) but self-heals — then someday doesn't. Defense: paved-road templates encode the dependency explicitly; treat "it converges eventually" as a bug in review.

**The webhook coupling.** Worth its third mention because it *will* be misdiagnosed as a Flux incident: vela-core unavailability + `failurePolicy: Fail` = fleet-wide Flux apply failures (Vol 1, 2.5; runbooks 15.1/15.6). Defense: decide the policy deliberately, alert on webhook latency, and put the coupling in the Flux runbooks too — the on-call who sees it first will be looking at Flux.

**Where the abstraction officially leaks.** Maintain the short list of situations where everyone drops to rendered manifests without guilt: SSA/managedFields forensics, StackGres day-2 (SGDbOps), NetworkPolicy debugging (Cilium tooling operates on pods/identities, not Applications), and performance tuning of rendered workloads. Publishing the list beats pretending the keyhole is a door (Vol 1, 1.4) — tenants who know when to look underneath file better tickets.

\newpage

# Chapter 18 — Walked Scenarios

## 18.1 Happy path A: shipping a new version of pricer

Operationalizing the chain end to end, with what each actor shows. Precondition: pricer running at `@sha256:9f00`, per 11.3's manifest.

1. Dev merges code MR → GitLab CI: test → build → push `pricer@sha256:ab12` to Artifactory → release job opens/auto-merges the deploy-repo MR bumping the digest in `apps/pricer/prod/application.yaml` (option 9.3-i with digest pinning). *Visible:* green pipeline; deploy-repo commit `d00d42`.
2. Receiver webhook fires → `flux get sources git tenant-quant-alpha` shows `main@sha1:d00d42` within seconds.
3. `flux get ks quant-alpha-prod` → `Applied revision: main@sha1:d00d42`. *Cluster:* Application `pricer` generation 15.
4. vela-core renders → `vela revision list pricer` shows `pricer-v9`; `vela status pricer` → `runningWorkflow`, component pricer `healthy: false, message: ready 2/2 of old RS...` transitioning.
5. `kubectl get rs -n quant-alpha -l app.oam.dev/component=pricer` → new RS surging; rollout proceeds; Gateway route untouched (no trait change → HTTPRoute manifest identical → SSA no-op — worth *knowing*, not assuming: it's in the dry-run diff).
6. HealthPolicy true → `vela status pricer` → `running`, message `ready 2/2`; workflow complete. Dev's verification command is exactly that one status call; the audit walk (9.5) can reproduce commit `d00d42` from any new pod.

## 18.2 Happy path B: onboarding team quant-beta and their first app

1. Approved ticket → platform MR to the *platform* repo: new `tenant` Application `quant-beta` (11.2) with `environments: [dev, prod], quotaTier: standard, owners: [...]`. Merge → chain runs in the platform namespace → `vela status tenant-quant-beta` green: namespaces, quota, netpol baseline, tenant SA, Flux GitRepository+Kustomizations all exist. *One MR, whole tenant.*
2. Platform Terraform/CI creates the GitLab deploy repo from template (CODEOWNERS, approval rules, CI includes), registers the deploy key and Receiver webhook.
3. quant-beta writes their first `application.yaml` in `apps/hello/dev/` from the scaffold, MR, CI dry-run validation passes, merge.
4. Chain runs; first deploy pulls the image, `vela status hello -n quant-beta-dev` → `running`. Elapsed from ticket approval: under an hour, of which most is human review — the number to actually measure and publish, because onboarding latency is the platform's first impression.

## 18.3 Happy path C: adding a StackGres database to pricer

1. App MR: add the `pricer-db` component (`postgres-cluster`, per 11.3) *and* the env wiring on `pricer` (secretRef to the normalized connection secret, `dependsOn: [pricer-db]` — 17's ordering edge, encoded in the scaffold snippet the docs provide).
2. Review: platform CODEOWNERS auto-added because a data-bearing type appeared (repo rule) — reviewer checks profile choice and the GC-protection policy presence.
3. Merge → chain → workflow deploys `pricer-db` first (dependency edge), SGCluster orchestration takes minutes (`vela status` shows the component progressing on StackGres conditions), then `pricer` rolls with the secret mounted. `vela status pricer --tree` shows both components; the SGCluster's day-2 (upgrades, switchover) is now the platform DBA path, which the MR template's description links — the keyhole boundary (12.1) stated at the moment the team first meets it.

## 18.4 Incident A (rendering layer): "deploys stopped fleet-wide at 14:07"

*You see:* pages on render-error rate; `kubectl get applications -A` (14.2 survey) shows dozens in error phase across tenants. *Walk:* one victim's `vela status` → CUE error naming `lib/labels` path → definitions repo log shows a `lib/` merge at 14:05 → 15.2: revert MR, chain flows, errors drain. *Root cause:* golden suite ran only for the touched component dir, `lib/` blast radius rule (16.1) missing from CI — add it. Duration: fifteen minutes if the on-call walks it in order; hours if they start at Flux.

## 18.5 Incident B (GC/ownership): "our VMPodScrapes disappeared"

*You see:* observability team reports scrape targets dropping at 11:30 across many apps; no tenant merged anything. *Walk:* deletion events on VMPodScrape at 11:30 → objects were labeled `app.oam.dev/*` → apprev diff on a victim (13.2 forensics): v12→v13 created 11:29, trigger = definition change (snapshot diff shows `vm-scrape` trait template) → the trait's emission guard now keys on a parameter no existing app sets → 15.3: revert, resources recreate on re-render. *The lesson to file:* the fixture matrix lacked "trait with default parameters" — the exact 16.3 row that would have caught it.

## 18.6 Incident C (stalled chain): "I merged an hour ago and nothing deployed"

*You see:* one tenant, one app, no fleet signal. *Walk 13.1 in order:* commit on branch ✓; source revision current ✓; `flux get ks` → **failing**: sibling YAML in the same tenant path is invalid since 13:40, whole Kustomization failing as a unit (9.4) — the merging dev's change is innocent and hostage. *Fix:* revert the sibling commit (tenant's move, you supply the diagnosis and the exact file); Kustomization recovers; the hostage change applies on the same reconcile. *Publish afterward:* per-app Kustomizations vs per-tenant is a real design dial — hostage-taking argues for finer granularity, Flux object sprawl argues against; know where your platform sits and why.

\newpage

# Closing Note

You now hold the full loop: the model (Vol 1), and the operation of the model (Vol 2). The three sentences to carry into week one: **removal from rendered output is a deletion order; definition write access is arbitrary-workload-creation access; and every incident is a walk down the Chapter 9 chain to the stalled or lying stage.** Day one, resolve the standing question with the thirty-second test (Vol 1, 5.4), read the definitions repo before any Application repo, and find the webhook `failurePolicy` before your first on-call shift.
