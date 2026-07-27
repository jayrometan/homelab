---
title: "KubeVela & OAM for the Macdonalds PaaS Stack"
subtitle: "Volume 1 — Architecture & Concepts: The Application Delivery Layer, Owner's Edition"
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

# How to Read This Volume

This is the first of two volumes on KubeVela, written for the engineer who *owns* the application-delivery layer of the platform — not a user's guide. Volume 1 covers the conceptual and architectural model: OAM, the vela-core controller, the X-Definition system, the workflow engine, the Flux boundary, multi-tenancy, and the complete merge-to-pods anatomy. Volume 2 is the operational playbook: running vela-core, onboarding tenants, developing definitions, debugging the rendering pipeline, and incident runbooks.

Throughout, three ownership tags appear: **[PaaS team]** (you), **[App team]** (the quant/trading teams consuming the platform), and **[Shared]** (contract surfaces where both sides touch the same object and the boundary must be policed socially and technically). Behaviors are tagged **[OAM-spec]** when they follow from the Open Application Model specification and **[KubeVela-impl]** when they are implementation choices of KubeVela that another OAM runtime could legitimately do differently. The distinction matters when you read upstream issues, because a surprising behavior that is [KubeVela-impl] is negotiable and version-dependent; one that is [OAM-spec] is load-bearing.

CUE fluency is assumed. Where this volume discusses CUE, it discusses *KubeVela's embedding of CUE* — the injected `context`, the `parameter`/`output` contract, the evaluation order, and the failure modes specific to CUE-inside-a-CRD-field — not the language.

The running example throughout both volumes is the platform's actual self-service flow: a quant team requests a Postgres cluster (StackGres underneath), a Redpanda topic, or an ordinary app deployment, by writing a small `Application` manifest against definitions your team publishes, merging it, and watching Flux and Vela carry it to running pods.

> **PROD.** Keep one number in mind for the entire booklet: the blast radius of this layer is *every application on the platform*. A bad Cilium agent takes out a node. A bad StackGres operator takes out databases. A bad ComponentDefinition revision, or a broken vela-core upgrade, changes what *every future reconcile of every Application* renders. The definitions library is the single highest-leverage, highest-risk artifact your team owns.

\newpage

# Chapter 1 — OAM From First Principles

## 1.1 The problem OAM claims to solve

The Open Application Model starts from an observation you already know intimately from the Helm era: raw Kubernetes manifests conflate *what the developer means* with *how the platform implements it*. A Deployment YAML contains the developer's intent (this image, this many replicas, these env vars) interleaved with platform policy (security context, topology spread, resource classes, label conventions, sidecar wiring, network policy hooks). Every mechanism the ecosystem invented to manage that conflation — Helm values, Kustomize overlays, Terraform modules wrapping manifests — is a *textual* separation: the developer edits one file, the platform edits another, and a template engine splices them. The splice point has no schema, no type checking, and no ownership enforcement beyond code review.

OAM's answer is to make the separation *structural* instead of textual. It defines a small vocabulary of API objects with explicit roles:

- An **Application** is what an app team writes. It is intent only: a list of components and the traits and policies applied to them. **[App team]**
- A **Component** is one deployable unit of the application — a service, a worker, a database claim. The app team picks a component *type* and supplies *parameters*; they never see the manifests the type expands into.
- A **Trait** is an operational capability attached to a component — expose it, scale it, add a sidecar, wire log shipping. Traits are the platform's menu of operational add-ons. **[Shared]** — app teams attach them, your team defines what attaching them does.
- A **Policy** is application-scoped behavior that isn't tied to one component: which clusters/namespaces to deploy into, override rules per environment, garbage-collection behavior.
- A **Workflow** is the ordered process by which the above becomes real: render, deploy group A, wait for health, deploy group B, notify.

The types behind the vocabulary — ComponentDefinition, TraitDefinition, PolicyDefinition, WorkflowStepDefinition, collectively "X-Definitions" — are authored by the platform team and are where all actual Kubernetes knowledge lives. **[PaaS team]**

The analogy that will serve you best is Terraform module ownership, and it is worth being precise about it. A ComponentDefinition is to an Application what a well-designed Terraform module is to a root module invocation: the module author (you) decides what is parameterized and what is fixed; the caller (app team) supplies `variables` and cannot reach inside. The differences are the interesting part. First, the "module language" is CUE, so the parameter surface is a real schema with types, defaults, and closedness — closer to a Terraform module with exhaustive `validation` blocks on every variable, enforced by the type system rather than by discipline. Second, evaluation is *continuous*: Terraform renders at plan time and the result is inert until the next apply, while a definition is re-evaluated by a controller on every reconcile, meaning a definition change retroactively changes the meaning of every existing Application that references it (unless you pin revisions — Chapter 3). That second difference is the source of most of the operational risk in this booklet.

## 1.2 The roles model, honestly

[OAM-spec] The specification describes three personas: application developer, application operator, and infrastructure operator. In practice, on a platform like this one, the middle persona does not exist as a separate human. The quant teams are the developers; your team is both operator personas. This collapse is normal and healthy — but it means the Trait concept, which the spec designed as the *application operator's* vocabulary, lands as **[Shared]** surface in real deployments, and shared surfaces are where abstractions leak. When a quant team attaches a `scaler` trait, they are exercising operator power through a hole your team drilled. Every trait you publish is a delegation decision.

## 1.3 Where the abstraction holds up

Three promises genuinely hold in production, and they are why this architecture is worth its complexity:

**Parameter surface control.** Because definitions are closed CUE, the app team literally cannot set a field you did not expose. There is no `--set securityContext.privileged=true` escape hatch as with Helm. For a platform serving trading teams with strict blast-radius requirements, this is the headline feature: the Application CRD is a *policy enforcement point with a schema*, not a convention.

**Uniform delivery mechanics.** Every workload — stateless service, StackGres cluster, Redpanda topic — arrives through the same pipeline: Application → render → apply → health. One status model (`vela status`), one revision history (ApplicationRevision), one garbage-collection model (ResourceTracker). When you build runbooks, you build them once.

**Legible intent.** An Application manifest is small enough to read in a merge request and states *what* without *how*. Reviews of app-team MRs become reviews of intent; reviews of your team's MRs (definition changes) are reviews of implementation. The Git history splits along the ownership boundary.

## 1.4 Where it leaks

Be equally honest about the failure modes of the model itself, because you will defend this architecture in design reviews:

**Debugging pierces the abstraction immediately.** The moment a rendered Deployment misbehaves, the app team's mental model ("I set replicas: 3") is useless and someone must reason about the rendered manifests, the CUE evaluation, and the server-side-apply field ownership. That someone is you. The abstraction reduces the app team's *authoring* burden but concentrates *debugging* burden on the platform team. Staff accordingly.

**Traits compose textually even though they look structural.** A patch trait modifying `spec.template.spec.containers` and a component that renders a StatefulSet instead of a Deployment can silently miss each other. [KubeVela-impl] Trait patches are CUE unification against the rendered output, and unification of a patch that matches nothing is not an error — it produces the unpatched output. Chapter 17's "CUE errors that produce empty output" gotcha family starts here.

**Day-2 of complex operands doesn't fit the model.** OAM models *delivery*. A StackGres cluster's interesting lifecycle — switchover, minor-version upgrade, PITR restore — is driven by the StackGres operator and by SGDbOps objects, not by re-rendering an Application. Wrapping SGCluster in a ComponentDefinition covers provisioning cleanly; the moment the DBA workflow starts, the app team is interacting with StackGres semantics through a Vela keyhole. Decide deliberately which day-2 verbs you surface as parameters/traits and which you route to a ticket or a direct SGDbOps flow. Do not pretend the keyhole is a door.

**Three orchestrators is one too many if you let it happen.** Vela has workflows, Flux has reconciliation, GitLab has pipelines. Chapter 4 draws the boundary; here it is enough to say the model does not draw it for you, and platforms that never decide end up with rollout logic smeared across all three.


\newpage

# Chapter 2 — vela-core: The Controller Architecture

## 2.1 What is actually running

KubeVela's runtime is a single primary controller deployment, `vela-core`, installed into `vela-system`. It is a controller-runtime manager hosting several reconcilers, of which the one that matters is the **Application controller**. Alongside it run:

- A **mutating/validating webhook** server (same pod by default) that defaults and validates Applications and X-Definitions at admission time.
- An optional **cluster-gateway** deployment for multi-cluster (hub pushes to spokes through it). If the platform is single-cluster-per-environment with Flux doing the cross-cluster distribution — the likely shape here — cluster-gateway may be absent entirely, and `topology` policies lose most of their purpose. Confirm on-site.
- Optional addon-installed controllers (VelaUX apiserver, workflow addon, etc.). Treat every one as an independent operand with its own blast radius.

The analogy: vela-core is to Applications what kustomize-controller is to Kustomizations — a reconciler that turns a declarative object into applied children — except its "build" step is a CUE evaluation of platform-authored templates rather than a kustomization of user-authored YAML, and it maintains a far richer ownership ledger (ResourceTracker) around the results.

## 2.2 The Application lifecycle: parse → render → apply → health

Every reconcile of an Application walks the same phases. Internalize this sequence; every debugging session in Volume 2 is a walk down it to find the stalled stage.

**Parse.** The controller reads the Application, resolves every referenced definition — for each component type, each trait, each policy, each workflow step — from the cluster. [KubeVela-impl] Definition resolution consults DefinitionRevisions when a revision is pinned (`type: webservice@v3` style or annotation-driven), otherwise the latest. A missing definition fails the Application into an error phase at this stage — before anything is rendered. This is the earliest and cheapest failure class.

**Render.** For each component, the controller assembles a CUE evaluation context: the definition's `template` block, the Application's `properties` for that component unified as `parameter`, and the injected `context` struct (`context.name`, `context.appName`, `context.namespace`, `context.appRevision`, `context.revision`, plus outputs of prior components where referenced). Evaluation must produce a concrete `output` (the primary workload) and optionally `outputs.<name>` (auxiliary resources). Trait templates are then evaluated against that result, either producing their own `outputs` or patching the workload via `patch`. The product of this stage is a fully concrete set of Kubernetes manifests — the *rendered revision*.

**Snapshot.** [KubeVela-impl] Before applying, the controller captures an **ApplicationRevision**: the Application spec *plus the full snapshot of every definition used, at the version used*. This is the property that makes revision pinning and honest rollback possible — an ApplicationRevision is self-contained and re-renderable even if the live definitions have since changed. It is also why ApplicationRevisions are large objects and why revision-limit trimming (Chapter 17) has teeth.

**Apply.** Manifests are applied with **server-side apply**, field manager identifying vela-core/the application. Ordering follows the workflow (Chapter 4); the implicit default workflow is essentially "deploy all components, then health-check." Every applied resource is recorded in the **ResourceTracker** for the Application.

**Health check.** Each component's definition may carry a `healthPolicy` (CUE over the observed resource) and `customStatus`. The controller polls until healthy or timeout, then sets the Application phase (`running`, `unhealthy`, `workflowSuspending`, etc.). Health here is *Vela's opinion computed from the child resources' status* — it adds no probes of its own.

## 2.3 ApplicationRevision, ResourceTracker, and garbage collection

These two auxiliary CRDs are the layer's memory, and most GC incidents are misunderstandings of them.

**ApplicationRevision** (`<app>-v1`, `-v2`, …) is created whenever the *effective* spec changes — application spec or a referenced definition's content. It is the unit of rollback and of audit ("what exactly did v7 render?"). A configurable limit (default 10) trims old revisions.

**ResourceTracker** is the ownership ledger. [KubeVela-impl] Vela maintains, per application, a *root* ResourceTracker (cluster-scoped object, holding the durable list of everything the app created) and *versioned* trackers per revision during transitions. Garbage collection is a set difference: after a successful apply of revision N, anything recorded in earlier trackers but absent from N's applied set is deleted, subject to `garbage-collect` policy rules (`keepLegacyResource`, per-resource rules by component or resource type, `sequential` deletion order). Deleting the Application deletes the trackers' contents via this ledger — *not* via ownerReferences in the usual sense for cross-namespace/cluster-scoped children, which is precisely why Vela can own resources a Deployment-style ownerReference model could not, and why a corrupted or hand-edited ResourceTracker is a genuinely dangerous object.

> **PROD.** The GC contract in one sentence: *removal from the rendered output is a deletion order.* If a definition change stops emitting a resource — even accidentally, even because a CUE conditional quietly evaluated false — the next reconcile of every referencing Application deletes that resource fleet-wide. Chapter 17 and runbook 15.3 exist because of this sentence.

## 2.4 Server-side apply, drift, and shared fields

Vela applies with SSA, so field ownership is tracked per-manager, and the interesting cases are fields *other* controllers also manage:

- **HPA vs `replicas`.** If a scaler trait renders an HPA, the definition must not also render a concrete `spec.replicas` on the workload, or the two managers will fight — the classic symptom being replicas snapping back on every Vela reconcile. The clean pattern is what you already know from Helm-with-HPA: emit `replicas` only when autoscaling is off. In CUE this is an `if parameter.autoscaling == _|_ { replicas: parameter.replicas }` guard in the definition — your job, invisible to the app team.
- **StackGres operator vs SGCluster fields.** The operator writes status and mutates some spec defaults through its own webhook. Your postgres-cluster definition must treat operator-defaulted fields as *not rendered* — render only the fields the app team's parameters determine, and let SSA merge the rest. Rendering a field you don't own converts every operator default change into a permanent diff fight.
- **Drift detection direction.** [KubeVela-impl] Vela's default posture is *state-keep*: periodic re-apply of the rendered manifests, reverting out-of-band edits to fields Vela owns. An `apply-once` policy relaxes this (apply on change, tolerate drift), configurable per path. Note the asymmetry with Flux: Flux detects drift of *what it applies* (Applications, if it applies Applications); Vela detects drift of *what it renders*. Two nested drift-correction loops, different objects. Keep the sentence "Flux owns the Application object; Vela owns the rendered children" as the invariant, and treat any manifest applied by both as a bug.

## 2.5 vela-core down vs degraded

**Down (deployment unavailable):** Everything already running keeps running — rendered children are ordinary Kubernetes objects reconciled by their own controllers (Deployment rollouts continue, StackGres keeps failing over, HPA keeps scaling). What stops: processing of new/changed Applications, GC, state-keep drift correction, status updates, and — critically — **admission of anything the webhook covers**, if the webhook `failurePolicy` is `Fail`: with vela-core's pods gone, *creating or updating any Application anywhere* is rejected at admission. Flux then can't apply Applications, and `flux get` shows apply errors that look like Flux problems. Know your webhook failurePolicy before the first incident, not during it. [KubeVela-impl]

**Degraded (up but sick — long work queue, definition cache stale, one shard hot):** Symptoms are latency and staleness rather than hard failure: Applications sit in `rendering`/`runningWorkflow` longer, `status.observedGeneration` lags `metadata.generation`, GC of superseded resources is deferred (old and new coexist longer than expected). Degradation is more dangerous than downtime because nothing pages on it by default — Chapter 8 defines the signals.


\newpage

# Chapter 3 — X-Definitions in Depth

This is the chapter you will live in. Definitions are the product your team ships; Applications are merely orders placed against them.

## 3.1 Anatomy of a definition

All four kinds share a shape. A ComponentDefinition, minimally:

```yaml
apiVersion: core.oam.dev/v1beta1
kind: ComponentDefinition
metadata:
  name: webservice
  namespace: vela-system          # cluster-visible; see 3.6
  annotations:
    definition.oam.dev/description: "Long-running service behind the platform gateway"
spec:
  workload:
    definition: { apiVersion: apps/v1, kind: Deployment }
  schematic:
    cue:
      template: |
        parameter: {
          image:    string
          replicas: *2 | int & >=1 & <=20
          port:     *8080 | int
          env?: [string]: string
        }
        output: {
          apiVersion: "apps/v1"
          kind:       "Deployment"
          metadata: labels: {
            "app.oam.dev/component": context.name
            "app.oam.dev/name":      context.appName
          }
          spec: {
            replicas: parameter.replicas
            selector: matchLabels: "app.oam.dev/component": context.name
            template: {
              metadata: labels: "app.oam.dev/component": context.name
              spec: containers: [{
                name:  context.name
                image: parameter.image
                ports: [{containerPort: parameter.port}]
                if parameter.env != _|_ {
                  env: [for k, v in parameter.env {name: k, value: v}]
                }
              }]
            }
          }
        }
        outputs: service: {
          apiVersion: "v1"
          kind:       "Service"
          metadata: name: context.name
          spec: {
            selector: "app.oam.dev/component": context.name
            ports: [{port: parameter.port, targetPort: parameter.port}]
          }
        }
```

The structural facts to internalize:

- `spec.workload.definition` declares the *primary* workload GVK. Health checks, traits that target "the workload," and rollout logic use this to know which of the rendered objects is the main one. A definition whose `output` GVK disagrees with its declared workload GVK is a lint error your CI should catch (Volume 2, Chapter 12).
- The template lives as a **string inside a CRD field**. Consequences: no syntax highlighting or type checking at authoring time unless you build it (Vol 2 covers extracting templates to `.cue` files and using `vela def` to round-trip); YAML escaping accidents are possible; and the API server validates the YAML, not the CUE — a definition with broken CUE **admits successfully** and fails only when an Application renders against it. The webhook does some template validation [KubeVela-impl], but treat admission as no guarantee of evaluability.
- `parameter` is the public API. Everything else is private implementation. Closedness of `parameter` is what makes unknown fields in an Application's `properties` an *error* rather than silently ignored — always write parameter as a closed struct (which struct literals in CUE are by default within a definition; be careful with open patterns like `[string]: _`).

## 3.2 The evaluation contract: context, parameter, output(s)

[KubeVela-impl] For each component, vela-core evaluates the template with these injected values:

- `parameter:` — the component's `properties` from the Application, unified against your declared `parameter` schema. Unification failure (wrong type, out-of-bounds, unknown field against a closed struct) fails the render for that component with a CUE error surfaced in Application status.
- `context:` — the injection point for everything the app team should never have to repeat. Guaranteed fields include `context.name` (component name), `context.appName`, `context.appRevision` / `context.appRevisionNum`, `context.namespace`, `context.revision` (component revision name when workload revisioning is on), `context.output` and `context.outputs` (inside *trait* templates: the already-rendered workload and auxiliaries — this is how traits read and patch), and `context.workflow`/step context inside workflow steps. Definitions should lean on `context` hard: naming conventions, standard labels, namespace — all derived, never parameters.
- The template must yield `output` (exactly one object, the declared workload) and may yield `outputs: <name>: {...}` for auxiliaries. Every rendered object gets the standard `app.oam.dev/*` labels stamped on it by the controller in addition to whatever you set — these labels are how you trace children back to Applications (Chapter 9) and how ResourceTracker cross-checks reality.

Two evaluation-order facts that bite:

1. **Concreteness is checked at the end.** A template can pass through wildly incomplete states as long as the final unified value is concrete. The flip side: a *conditionally emitted* resource (`if parameter.metrics { outputs.podMonitor: ... }`) that evaluates false is simply absent — correct when intended, a fleet-wide GC event when a refactor changes the truthiness of the guard (Chapter 2.3's warning, mechanism now visible).
2. **`_|_` (bottom) inside an unexercised branch is invisible.** CUE only errors on bottom values that end up demanded by the output. Your test matrix, not the evaluator, is what exercises branches. This is why Vol 2 Chapter 12 insists on rendering tests across the full parameter matrix.

## 3.3 TraitDefinitions and the patch pipeline

Traits come in two functional styles, sometimes combined:

**Generators** emit `outputs` — new objects alongside the workload. A `gateway` trait emitting an HTTPRoute (bound to the Cilium Gateway API implementation), a `victoria-metrics-scrape` trait emitting a VMPodScrape: pure addition, low risk.

**Patchers** modify the rendered workload via a `patch:` block, which is unified into the component's output. The `+patchKey=name` comment directive controls list-merge semantics: with it, patching `containers` merges by element name (inject a sidecar, modify the main container by matching its name); without it, CUE list unification is positional — the single largest source of "my patch trait scrambled the pod spec" bugs. `patchStrategy` options (`retainKeys`, `jsonPatch`, `jsonMergePatch` [KubeVela-impl, version-dependent]) exist for cases unification can't express, such as *removing* a field.

> **PROD.** A patch that matches nothing is silent. A trait whose patch selects a container named `"app"` under `spec.template.spec.containers` on a component whose main container is named `context.name` (not "app") unifies successfully and changes nothing. Trait/component compatibility is declared via `appliesToWorkloads` in the TraitDefinition — set it strictly, and treat `appliesToWorkloads: ["*"]` in review the way you'd treat a Helm chart that templates arbitrary user YAML: as a smell. `conflictsWith` similarly prevents nonsensical trait pairs (two ingress traits) at admission rather than at debug time.

Trait evaluation order is deterministic ([KubeVela-impl]: ordered by the definition's declared stage — pre/post workload output — then by declaration order in the Application). Two patch traits touching the same field resolve by CUE unification, meaning *conflict is an error only if the values differ*; identical writes merge silently. Document per-trait what fields it claims; you are the registrar of a namespace of patches.

## 3.4 PolicyDefinitions and WorkflowStepDefinitions

Policies render no workload; their parameters configure application-scope behavior consumed by the controller (`garbage-collect`, `apply-once`, `override`, `topology`, `shared-resource`) or by workflow steps (`env-binding` era patterns). You will author custom policies rarely; you will *set* the built-ins constantly, and Chapter 4 covers the deploy-relevant trio. WorkflowStepDefinitions wrap imperative-ish actions (apply a group, wait, HTTP call, notify, suspend) in the same CUE-templated clothing; the `context` inside them includes workflow state, and their `op.#Apply`/provider-call vocabulary is [KubeVela-impl] entirely — nothing OAM about it.

## 3.5 Definition versioning and revision pinning

Every edit to a definition produces a **DefinitionRevision** (`webservice-v1`, `-v2`, …). Unpinned Applications resolve *latest at render time* — which, combined with "renders are re-run continuously," yields the central operational hazard: **publishing a definition edit immediately re-renders the entire fleet that references it.** There is no gradual rollout unless you build one.

Pinning mechanisms, from blunt to sharp: (a) reference `type: webservice@v2` in the Application — explicit, visible in Git, but pushes version management onto app teams; (b) publish under versioned *names* (`webservice-v2` as a distinct definition) — crude, litters the namespace, but makes migration an explicit app-team action; (c) leave Applications unpinned and instead make your *rollout process* staged: apply the new revision, then migrate cohorts by pinning them forward (or by namespace-scoped shadow definitions, 3.6). Volume 2 Chapter 12/16 turns this into a procedure; the architectural takeaway here is that *the definition reference is a dependency edge with no default version constraint*, exactly like a Terraform module sourced from `main` instead of a tag. You would never ship the latter; don't ship the former for Tier-1 definitions like `postgres-cluster`.

## 3.6 Scope: where definitions live

Definitions in `vela-system` are platform-global; definitions in a tenant namespace are visible to Applications in that namespace, and namespace-local definitions shadow global ones of the same name [KubeVela-impl]. The shadowing rule is a gift and a threat: a gift, because it gives you a canary mechanism (deploy the candidate revision as a namespace-local definition in a staging tenant); a threat, because *anyone who can create definitions in their own namespace can override your platform definitions for their namespace* — see Chapter 6's RBAC treatment. The default posture for this platform should be: all definitions global, definition-write RBAC restricted to the platform team and its CI, namespace-local definitions denied by policy except in designated canary namespaces.


\newpage

# Chapter 4 — The Workflow Engine, and Why You Should Use Almost None of It

## 4.1 What it is

Every Application execution runs a workflow. If you declare none, [KubeVela-impl] an implicit one is synthesized: deploy all components (respecting inter-component `dependsOn`/output references), then evaluate health. Declared workflows are ordered lists of steps — `deploy` (apply a set of components, optionally filtered by policies), `suspend` (halt until a human or API call resumes), `step-group` (parallelism), plus utility steps (`notification`, `read-object`, `export2config`, HTTP requests). Steps run in a state machine persisted in Application status; `suspend` is durable across controller restarts; failed steps retry with backoff until the workflow is marked failed.

The deploy-relevant policy trio:

- **`topology`** — *where*: which clusters/namespaces a deploy step targets. In a hub-spoke multi-cluster Vela this is the distribution mechanism. If this platform distributes per-environment via Flux (separate cluster, separate Git path), topology policies may be nearly unused — one of the two boundary architectures in Chapter 5 makes them redundant. Confirm on-site; their presence or absence in real Applications will tell you instantly which architecture the firm runs.
- **`override`** — *with what changes*: patch components/parameters per target (staging gets 1 replica, prod gets 5). Directly competes with "per-environment directories in Git" as the environment-specialization mechanism. A platform should pick exactly one of these; running both means answering "what's different in prod?" requires reading two systems.
- **`shared-resource`** — declares specific rendered resources (namespaces, CRDs, shared ConfigMaps) as co-ownable by multiple Applications, exempting them from exclusive-ownership conflict errors and from GC until the *last* referencing Application departs. You will need this for tenant-infrastructure patterns (Vol 2, Chapter 11).

`suspend` deserves a special note because it is the workflow feature most likely to earn its keep here: a prod deploy step preceded by `suspend` gives you a human approval gate *inside* the GitOps flow — merge lands, staging deploys, workflow suspends, an operator runs `vela workflow resume` after checks. Whether you want that gate in Vela versus in GitLab (MR approval to the prod branch/directory) is a boundary decision — see 4.2.

## 4.2 Three orchestrators: the opinionated boundary

The platform has GitLab CI, Flux, and Vela workflows. Left undesigned, rollout logic accretes in all three and incidents require reading all three. Draw the boundary like this and defend it:

**GitLab CI owns everything before Git state.** Build, test, scan, publish image, *write the desired state* (bump the tag in the Application file or let Flux image automation do it). CI must end at a commit. CI should never `kubectl apply`, never `vela up`, never talk to the cluster at all. The moment CI applies things, you have two sources of truth and your Git history stops being the audit log.

**Flux owns Git-to-cluster transport and nothing else.** Fetch, verify, apply, detect drift of what it applied, notify. No environment logic beyond "this Kustomization points at this path/branch with this interval." Flux is deliberately boring here.

**Vela owns intra-application orchestration only:** render, inter-component ordering (app before its HTTPRoute, StackGres cluster before the app that needs its secret), health gating, and — if you adopt them — canary/approval steps for the application's own rollout. Vela workflows should *never* orchestrate across Applications (that's dependency hell reinvented) and never reach outside the cluster (no "notify Slack" steps that duplicate Flux's notification-controller; one alerting pipeline, not two).

The test for any proposed workflow step: *does this step's failure mode make sense to debug with `vela status`?* Health gates, yes. "Call the trading-calendar API to decide if we can deploy," absolutely not — that belongs in CI, before the commit, where a failure is a red pipeline and not a fleet of suspended workflows.

> **PROD.** Multi-stage canary via Vela workflows (deploy 10% → wait → 100%) is real and works, but it holds Applications in `runningWorkflow` for the duration and turns every deploy into a stateful process with resumption semantics. For an HFT platform, prefer boring: full rollout gated by health checks, with pre-prod environments doing the confidence work. Adopt canary steps only for the handful of services that genuinely need them, not as the paved-road default.

\newpage

# Chapter 5 — The Flux/KubeVela Boundary: Two Architectures

The standing question: does Flux apply Application objects and vela-core expands them in-cluster, or does something render manifests upstream so Flux applies raw output? Both are legitimate; they distribute failure modes very differently. Learn both cold, then read the firm's repos on day one — the answer is visible in about thirty seconds once you know what to look for (5.4).

## 5.1 Architecture A — Flux applies Applications; Vela renders in-cluster

```
GitLab MR merge
   |
   v
Git repo (apps/<tenant>/<app>/application.yaml)
   |            source-controller: fetch on interval, package artifact
   v
kustomize-controller: SSA-applies Application CR (impersonating tenant SA)
   |
   v
vela-core: parse -> render (CUE) -> ApplicationRevision -> workflow
   |         -> SSA-apply children -> ResourceTracker -> health
   v
Deployments / SGClusters / HTTPRoutes / ...  -> their controllers -> pods
```

Git contains *intent* (small Application files). Flux's job ends at the Application object; Vela's begins there. Two nested reconciliation loops: Flux corrects drift of Applications; Vela corrects drift of children.

**Where a rendering bug surfaces:** in-cluster, *after* merge, in Application status — `flux get ks` is green (it applied the Application successfully; its job is done), while `vela status` shows the render error. The failure is invisible to anyone watching only Flux. **Drift:** hand-edits to children are reverted by Vela state-keep; hand-edits to Applications are reverted by Flux. **Unique failure modes:** everything in vela-core's runtime path is in the critical path of *deployment* (though not of running workloads) — vela-core down means merges stop landing; definition changes act at render time, fleet-wide, decoupled from any Git change to Applications (the Git log of the app repo shows *nothing* on the day everything re-rendered); and the webhook coupling from 2.5 means vela-core outage can block Flux applies.

**Debugging implication:** the audit chain is commit SHA → Flux revision → Application generation → ApplicationRevision → rendered children. Four ledgers, all queryable, walked in Chapter 9.

## 5.2 Architecture B — Rendering upstream; Flux applies raw manifests

Rendering (`vela dry-run` or a pure CUE pipeline in CI, or a pre-commit tool) happens *before* apply: CI takes the Application + pinned definitions, renders concrete manifests, commits them to a deploy repo/branch; Flux applies the raw Deployments/SGClusters directly. vela-core may not run in the cluster at all, or runs only for a subset.

**What you gain:** the diff reviewed in the deploy repo is the *actual* change to the cluster — no "what will this render as?" uncertainty at review time; no vela-core in the runtime path (deployment keeps working when Vela tooling is broken); definition upgrades become visible, reviewable diffs in the deploy repo instead of silent fleet re-renders — arguably the strongest blast-radius story of all, and why rigor-minded shops are drawn to it. **What you lose:** the entire day-2 value of the controller — no ResourceTracker GC (Flux prune takes over, with its own semantics), no health/status model (`vela status` doesn't exist; you're back to per-workload status), no workflows (ordering falls back to Flux dependsOn between Kustomizations, much coarser), no state-keep beyond Flux's drift correction of exactly-what's-in-Git, and *two* Git repos whose relationship (intent repo → rendered repo) is maintained by CI glue that is now itself Tier-1 platform code. GC edge: a component removed from an Application only gets cleaned up if the render step *also removes files* and Flux prune is on — the deletion contract moves from ResourceTracker semantics to "did CI delete the file."

**Where a rendering bug surfaces:** in CI, *before* merge to the deploy repo — earlier and safer. But an *evaluation-environment* mismatch class appears that A doesn't have: CI renders with the CUE/vela version and definition set present in the pipeline image, which can drift from what anyone else believes is current.

## 5.3 Judgement

A is the architecture KubeVela is designed for and the one that justifies running vela-core at all; B uses Vela as a template engine and Flux as the delivery system. For this platform's stated values, the honest trade is: **A concentrates risk in the definitions library and vela-core's operational health; B concentrates risk in CI glue and forfeits the unified status/GC/health model.** A shop that owns Vela seriously (your mandate) usually runs A, and mitigates A's fleet-render hazard with the pinning/staged-rollout discipline of Vol 2 — because it keeps one ownership ledger, one status model, and one place where "what did this app create" is answerable. If you find B on-site, your role tilts from "controller owner" toward "render-pipeline owner," and Vol 2 Chapters 12–13 apply to the CI incarnation of the same pipeline.

There is also a hybrid worth recognizing on sight: Flux applies Applications (A), *and* CI runs `vela dry-run` against pinned definitions as a merge check — B's review-time visibility bolted onto A's runtime model. This is the shape I'd argue for if the choice is open.

## 5.4 Determining which one the firm runs, in thirty seconds

Look at the repo Flux points at: Application YAMLs → A; rendered Deployments/StatefulSets → B. Or `kubectl get applications -A`: populated → A (then check `kubectl get applicationrevisions` is advancing). Or read a Flux Kustomization's `spec.path` and look at what lives there. Then confirm the hybrid check by looking for `vela dry-run`/`vela def vet` in `.gitlab-ci.yml`.

## 5.5 Vela's own GitOps-adjacent features, and why a Flux shop ignores them

KubeVela has grown GitOps capabilities of its own over time [KubeVela-impl]: the FluxCD *addon* (which installs Flux controllers and exposes `helm`/`kustomize` ComponentDefinitions so an Application can say "deploy this Helm chart from this repo" — Vela *consuming* Flux source/helm controllers as its engine), and VelaUX-driven pipeline features. On a platform that already runs Flux as the transport, adopting Vela-pulls-from-Git inverts the ownership: suddenly some workloads' source of truth is referenced *from inside* an Application rather than from a Flux Kustomization, and your "one transport, one audit chain" invariant dies. The defensible uses are narrow: the `helm` component type can be a pragmatic bridge for migrating an existing Helm-deployed app into an Application without immediately rewriting it as a native definition (Vol 2, Chapter 11.4). Treat it as scaffolding with a demolition date, not architecture.


\newpage

# Chapter 6 — Multi-Tenancy and the Real Privilege Boundary

## 6.1 The chain of authority

Trace who can make the cluster do what, end to end:

```
Developer --(MR + review)--> Git --(deploy key)--> Flux source-controller
  --> kustomize-controller --(impersonates tenant SA)--> Application object
  --> vela-core --(its own ServiceAccount)--> rendered children
```

Two impersonation boundaries matter, and they are *different systems*:

**Flux's boundary** you already know: `--default-service-account` forces kustomize-controller to impersonate a per-tenant ServiceAccount, so a tenant's Kustomization can only apply what that SA's RBAC allows. On this platform, the tenant SA should be allowed to write `applications.core.oam.dev` in the tenant namespace and essentially nothing else. That is the whole point of Architecture A's tenancy story: **the tenant's Git-mediated power is "create Applications," full stop.**

**Vela's boundary** is where it gets interesting. When vela-core renders and applies children, whose authority does it use? [KubeVela-impl] By default, its *own* controller ServiceAccount — which is cluster-admin-ish, because it must be able to create anything any definition can render. This means the Application object is a *privilege escalation gateway*: a tenant restricted by Flux to "create Applications" nonetheless causes cluster-admin-powered creation of whatever the referenced definitions render. The system is safe **exactly to the degree the definitions library constrains what can be rendered.** Vela supports authentication/impersonation modes (propagating the applying user's identity, or an annotated ServiceAccount, so children are applied with tenant authority) — if enabled, RBAC re-enters at render time and definitions can be looser; if not, your definitions *are* the security policy. Find out which mode is on. In the default mode, the following sentence is the platform's core security invariant:

> **PROD.** *Definition write access is arbitrary-workload-creation access.* Anyone who can create or modify an X-Definition (or a namespace-local shadow of one, 3.6) can make vela-core render anything — privileged pods, mounts of any secret, cluster-role bindings — on behalf of any Application that references it. This is the exact analogue of StackGres's SGScript warning: an innocent-looking "configuration" object that is actually code executing with operator privileges. Definition CRUD must be locked to the platform team and its CI pipeline identity; RBAC on `componentdefinitions`, `traitdefinitions`, `policydefinitions`, `workflowstepdefinitions`, and `definitionrevisions` audited like you'd audit `clusterrolebindings`.

## 6.2 Namespace scoping in practice

The workable tenancy layout for this platform: one namespace per team (or per team-environment), Applications namespaced with the tenant, all definitions global in `vela-system` (3.6), tenant SAs writable on Applications only, ResourceQuota/LimitRange per namespace enforced on the *rendered children* (quotas don't know about Vela; they act at pod/object creation — which means quota exhaustion surfaces as a *Vela apply/health failure*, a mapping your runbooks must include). Cross-namespace rendering — a definition emitting resources into another namespace — is possible for the controller and must be treated as a definition-review red flag except for blessed patterns (the tenant-bootstrap definition of Vol 2 Ch. 11, which legitimately creates namespaces). Baseline CiliumNetworkPolicies per tenant namespace belong in the tenant bootstrap, not in per-app definitions, so that a rendering bug in an app definition can never widen network reach.

## 6.3 Where the real boundary sits

Composing it all: the *authoring* boundary is Git review (an MR approver on the tenant repo); the *transport* boundary is Flux impersonation (tenant SA, Applications only); the *expansion* boundary is the definitions library (what renders); the *runtime* boundary is namespace isolation (quota, NetworkPolicy, Pod Security admission on rendered pods). The first and third are yours; note that the third is the only one enforced by *code you write* rather than by RBAC objects, which is why Volume 2 spends three chapters on definition lifecycle discipline.

\newpage

# Chapter 7 — Addons and VelaUX

## 7.1 The addon system

Addons are packaged bundles — CRDs, controller Deployments, and definitions — installed via `vela addon enable` from catalogs, rendered (naturally) as Vela Applications in `vela-system`. Architecturally each addon is: some operators + some definitions your tenants may start referencing. That second half is the trap: enabling an addon can silently grow your public API surface with definitions you don't maintain and didn't review to your standards. Platform-relevant here, at most: the observability-adjacent pieces if their definitions suit VictoriaMetrics scraping (usually you'll write your own thin traits instead), and possibly `fluxcd` if a migration bridge is wanted (5.5). The rest of the catalog — rollouts integrations, dex, terraform providers, cloud-vendor addons — is cruft for this stack. Policy: addons are platform changes, installed via your GitOps flow with vendored/pinned content, never `vela addon enable` by hand against prod, and *every definition an addon ships is either explicitly adopted into your library standards or RBAC-hidden from tenants*.

## 7.2 VelaUX, honestly

VelaUX is the dashboard/apiserver addon: UI-driven application creation, environment management, pipelines — its own data model layered *above* Application CRs. On a GitOps platform its write path is poison: applications created through the UI have their source of truth in VelaUX's database, not Git, and you now run two authoring systems that each believe they own the object. As a *read-only* status surface it's defensible but weak — you will get more from wiring `vela-core` metrics and Application phase data into VictoriaMetrics/Grafana (Chapter 8), where the rest of the platform's observability already lives. Recommendation: don't deploy it; state the "Git is the only write path" contract in the tenant docs instead; give tenants `vela status`/`vela top` and Grafana dashboards.

\newpage

# Chapter 8 — Observability of the Abstraction Layer Itself

The layered question every platform owner must answer instantly during an incident: *is the abstraction layer sick, or is a workload under it sick?* Different pagers, different runbooks.

## 8.1 Health and status the layer computes

Definitions can carry `healthPolicy` and `customStatus` — CUE evaluated against the *observed* child resources (via `context.output`/`context.outputs` post-apply):

```cue
healthPolicy: |
  isHealth: context.output.status.readyReplicas == context.output.spec.replicas
customStatus: |
  message: "ready \(context.output.status.readyReplicas)/\(context.output.spec.replicas)"
```

For your `postgres-cluster` definition, health should key off SGCluster status conditions (pods ready + cluster bootstrapped), so `vela status` answers "is my database up" without the app team learning StackGres. Component healths roll up into the Application **phase state machine**; the states worth memorizing: `rendering` → `runningWorkflow` → `running` (healthy steady state), with `workflowSuspending` (gate or failure-suspend), `unhealthy` (applied but health checks failing), `deleting` (GC in progress — where stuck-finalizer incidents live). A fleet-wide transition `running` → `unhealthy` minutes after a definitions MR merges is the signature of a bad definition rollout; a single app flapping is a workload problem. Phase is your triage fork.

## 8.2 What vela-core exposes to VictoriaMetrics

vela-core exports Prometheus metrics; scrape it with a VMPodScrape like any controller. The signals that indicate *layer* sickness, in the order you should alert on them:

- **Reconcile queue depth and workqueue latency** (controller-runtime standard metrics, per-controller). Rising queue + flat CPU means stuck reconciles; rising queue + pegged CPU means under-scaled or a render storm (someone changed a hot definition).
- **Reconcile error rate and duration histograms** for the Application controller. A step-change in p99 render duration after a definitions release is a CUE performance regression.
- **Application phase counts** (gauge by phase). Alert on `unhealthy` + `workflowSuspending` fleet-wide deltas, and on any Application whose `observedGeneration` lags `generation` beyond a few reconcile intervals — the universal "controller has not caught up" signal, worth a recording rule of its own.
- **Webhook latency/error rate** — remembering 2.5: webhook failure blocks *Flux*, so this alert should page the same rotation that owns Flux applies.
- **GC/apply failure counts** — apply errors here are quota, RBAC, webhook-of-child-CRD, or SSA-conflict problems surfacing through Vela.

Log side: vela-core logs to stdout, shipped by the platform's Fluent Bit into VictoriaLogs; the high-value log lines are CUE evaluation errors (which include component and definition names — index them) and SSA conflict reports naming the competing field manager. Layer-vs-workload triage in one line: **if `flux get` is green, Application phase is `running`, and the service is broken — it's the workload or its config values; anything else, walk Chapter 9's chain to the stalled stage.**


\newpage

# Chapter 9 — The Deployment Anatomy: "I Merged, Now What"

This chapter is the spine of the booklet. Everything in Volume 2 — every runbook, every debugging session — is a walk down this chain to find the stalled or lying stage. It is written for Architecture A (Flux applies Applications); where Architecture B differs, the divergence is exactly that stages 4–6 move into CI and the chain shortens.

## 9.1 The actors and their clocks

Every actor below is a polling or event-driven loop with its own interval. Deployment latency is the *sum of the waits you land in*, and "nothing happened yet" is usually "you are inside somebody's interval."

| # | Actor | Watches / trigger | Acts by | Typical clock |
|---|-------|-------------------|---------|---------------|
| 1 | GitLab CI | push/MR events | builds image, pushes to Artifactory, updates Git state | 2–10 min pipeline |
| 2 | Git (GitLab) | — | source of truth | instant on merge |
| 3 | flux source-controller | GitRepository interval (or webhook via Receiver) | fetches, packages artifact, bumps revision | interval 1–5 min; webhook ≈ seconds |
| 4 | flux kustomize-controller | new artifact revision / Kustomization interval | SSA-applies Application CRs, impersonating tenant SA | on artifact change; drift re-check at interval (often 10 min) |
| 5 | vela-core (Application controller) | watch on Applications (event-driven) | parse → resolve definitions → CUE render → new ApplicationRevision → workflow → SSA-apply children → record in ResourceTracker | seconds after apply; periodic re-reconcile for state-keep |
| 6 | workload controllers (Deployment, StackGres, Gateway API/Cilium) | watch on their CRs | rollout: new RS, surge pods, readiness; SGCluster orchestration; route programming | rollout time: image pull + probes; SGCluster: minutes |
| 7 | vela-core (health) | observed child status | evaluates healthPolicy, advances workflow, sets phase `running` | polls until healthy/timeout |

Interval-tuning note before the walkthroughs: the platform lever that changes perceived deploy latency most is a Flux **Receiver** (GitLab webhook → source-controller), collapsing stage 3 from minutes to seconds. Vela adds almost no waiting of its own on the happy path — it is event-driven off the Application watch.

## 9.2 Shape (a): app config change

A developer edits their Application — bumps `replicas`, changes an env var, adds a trait — in `apps/quant-alpha/pricer/application.yaml`, opens an MR, gets review, merges.

1. **Merge → Git.** New commit SHA `c0ffee1` on the deployed branch. (CI may run validation — lint, `vela dry-run` in the hybrid model — but builds nothing.)
2. **source-controller** (webhook or interval) fetches, stores artifact, sets the GitRepository's `status.artifact.revision` to `main@sha1:c0ffee1`. First ledger entry: **Flux revision = commit SHA.**
3. **kustomize-controller** sees the new revision, builds, SSA-applies the changed Application. The Application's `metadata.generation` increments; its `app.kubernetes.io/...`/Flux labels tie it to the Kustomization. `flux get ks quant-alpha` now shows `Applied revision: main@sha1:c0ffee1`. Second ledger entry: **Kustomization lastAppliedRevision.**
4. **vela-core** watch fires. Parse; definitions resolved; CUE render with the new parameters; the effective spec differs from the last ApplicationRevision, so **`pricer-v8`** is created — snapshotting spec *and* the definitions used. Third ledger entry: **ApplicationRevision.** Workflow starts; children SSA-applied; ResourceTracker updated. Phase: `runningWorkflow`.
5. **Deployment controller** sees the changed pod template (or just scales, if only replicas changed — no new RS in that case). New ReplicaSet hash if the template changed; rolling update per surge settings; kubelet pulls from Artifactory; readiness probes pass. Fourth ledger entry: **ReplicaSet revision annotation** (`deployment.kubernetes.io/revision`).
6. **vela-core health** sees readyReplicas == replicas via healthPolicy; workflow completes; phase `running`. `vela status pricer` shows the healthy component tree.

Elapsed, typical: seconds (webhook) or up to one source interval, + seconds of Flux apply, + seconds of render, + rollout time. The dominant term is almost always stage 3's interval or stage 5/6's rollout — *not* Vela.

## 9.3 Shape (b): new image release

The interesting question is the one the prompt asks: **how does the new tag reach the Application?** Three honest options; the platform must pick one and document it:

**(i) CI writes the tag back to Git** — the deploy job in the *app* pipeline commits a one-line change to the Application's `image` parameter (same repo or a separate deploy repo), typically via a bot account + MR or direct push to the env branch. Pros: dead simple, fully auditable, promotion = the same mechanism pointed at the next environment's file. Cons: CI needs write credentials to the deploy repo (secure them like deploy keys); commit loops must be guarded (`[skip ci]`); parallel releases race on the file.

**(ii) Flux Image Automation** — image-reflector-controller scans Artifactory for new tags matching a policy (semver range, regex); image-automation-controller commits the bump to Git itself, driven by markers in the YAML. Pros: no CI credentials into the deploy repo; consistent machinery. Cons: the marker comments must sit *inside the Application's properties* (they work on YAML fields regardless of CRD type, but reviewers must understand them); tag-policy mistakes auto-deploy the wrong thing — with `latest`-style policies being the classic self-inflicted outage; and *scanning Artifactory* requires reflector credentials and network reach to it. For an HFT shop, the loss of a human/MR gate on prod image bumps is usually disqualifying for prod paths and acceptable for dev.
**(iii) Digest pinning** — either flavor, but writing `image@sha256:...` instead of a tag. This is the only option that makes "what is running" cryptographically exact and immune to tag mutation in Artifactory. Strong recommendation regardless of (i) vs (ii): CI resolves tag→digest at build time and the committed state carries the digest; humans read the adjacent tag comment.

The pipeline then proceeds *identically to shape (a)* from the moment the commit exists — that is the entire elegance of the model: an image release *is* a config change. Full sequence with option (i):

```
dev pushes code ──> GitLab CI: test -> build -> push image
                    registry: artifactory/.../pricer@sha256:ab12...
                    CI job "release": commit to deploy repo
                    apps/quant-alpha/pricer/application.yaml
                      image: ...@sha256:ab12   (was @sha256:9f00)
                         │
   [stage 3] source-controller: revision main@sha1:d00d42
   [stage 4] kustomize-controller: apply Application (gen 15)
   [stage 5] vela-core: render -> pricer-v9 -> apply Deployment
   [stage 6] Deployment ctrl: new RS pricer-7c9f...  rollout
   [stage 7] health ok -> phase running
```

## 9.4 The timeline, with real latency contributors

```
t=0      merge (config) or CI release-commit (image)
t+0..3m  ── source interval (or +5s with Receiver webhook) ──────────
t+ε      kustomize build+apply            (seconds; +retry backoff on error)
t+ε      vela watch fires; render         (sub-second to seconds;
                                           big CUE or many components: more)
t+ε      SSA apply children               (seconds; webhook-of-child latency)
t+X      workload rollout                 (image pull + readiness: the bulk)
t+X+δ    healthPolicy true -> running
```

The platform-owned knobs, in impact order: Receiver webhooks (kills the biggest fixed wait); Kustomization retry/backoff on transient apply errors (a failed apply waits out a full retry interval — this is the classic "merged 20 minutes ago, nothing happened" when a *different* object in the same Kustomization is broken and the whole apply fails — remember Flux Kustomizations fail as a unit); image pull time (Artifactory proximity, pre-pull daemonsets for fat images); probe initial delays.

## 9.5 Tracing a pod backwards to a commit

The chain in reverse, resources-only, no tribal knowledge — this is the "who deployed this and when" audit walk:

1. **Pod → ReplicaSet → Deployment** via ownerReferences (`kubectl get pod X -o jsonpath='{.metadata.ownerReferences}'`, walk up).
2. **Deployment → Application**: read labels `app.oam.dev/name` (application) and `app.oam.dev/component`; namespace gives tenant. The ResourceTracker for that app cross-confirms: this Deployment is in its ledger.
3. **Application → ApplicationRevision**: `status.latestRevision.name` (e.g. `pricer-v9`) — or, if you suspect you're looking at a pod from an older revision mid-rollout, match the RS pod-template-hash against what each revision renders.
4. **ApplicationRevision → definitions**: the revision object *contains* the definition snapshots — you can prove exactly which `webservice` template version produced this Deployment without trusting the live definitions at all.
5. **Application → Flux → commit**: the label `kustomize.toolkit.fluxcd.io/name` on the Application (with its namespace sibling) names the owning Kustomization; its `lastAppliedRevision` status field gives branch@sha — but note that's the Kustomization's *current* state. For the historical question ("which commit produced *this* revision?"), the honest ledger is Git itself: `git log -S` the image digest or parameter value in the deploy repo, which is why digest pinning (9.3-iii) pays for itself in audits. Shops that want this mapping airtight annotate Applications with the commit SHA in CI (a one-line `metadata.annotations` addition to the release commit), making step 5 a label read like the others. Recommend it.

Total: four object reads and a git log. When you can do this walk in under two minutes from memory, you own the layer.

