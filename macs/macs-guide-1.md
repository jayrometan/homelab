# Macdonalds PaaS — Predicted Platform Architecture & Workflows

> **Purpose:** A reasoned prediction of how Macdonalds' PaaS environment likely works, based on the known tool stack, HFT industry norms, and standard platform engineering patterns. Each section is marked with a confidence level:
> - 🟢 **High** — near-certain given the tools chosen
> - 🟡 **Medium** — likely, but multiple plausible designs exist
> - 🔴 **Low** — educated guess, verify early after joining
>
> **Known facts:** Firm-wide user base · fewer than 5 global clusters · everything is GitOps · stack as listed below.

---

## 1. The Big Picture

### 1.1 What the PaaS team owns 🟢

The team owns the **entire substrate between bare metal/VMs and the developer's application**:

```
┌─────────────────────────────────────────────────────────────┐
│  DEVELOPER EXPERIENCE LAYER                                  │
│  KubeVela Applications · CUE schemas · GitLab templates      │
├─────────────────────────────────────────────────────────────┤
│  DELIVERY LAYER                                              │
│  GitLab CI · FluxCD · KubeVela controller                    │
├─────────────────────────────────────────────────────────────┤
│  PLATFORM SERVICES                                           │
│  StackGres (Postgres) · Redpanda (streaming)                 │
│  VictoriaMetrics/Logs · OTel Collector · Fluent Bit          │
│  Quickwit/OpenSearch (search/log indexing)                   │
├─────────────────────────────────────────────────────────────┤
│  CLUSTER LAYER                                               │
│  Kubernetes (Kubespray) · etcd · Gateway API                 │
│  SPIFFE/SPIRE (identity) · Weka CSI (storage)                │
├─────────────────────────────────────────────────────────────┤
│  NETWORK LAYER                                               │
│  Cilium BGP mode (no encapsulation) · Gateway API impl       │
└─────────────────────────────────────────────────────────────┘
```

What the team almost certainly does **not** own: the physical network fabric (a network engineering team owns the routers/switches Cilium peers with), bare metal provisioning below the OS, and the applications themselves.

### 1.2 Cluster topology 🟡

With fewer than 5 clusters globally and offices in NYC, London, and Singapore, the most likely layout:

| Cluster | Location | Purpose |
|---|---|---|
| prod-us | NYC/NJ | Primary production |
| prod-eu | London | EU production |
| prod-apac | Singapore | APAC production |
| staging or dev | One region | Pre-production validation |

Possibly a small management/tooling cluster running shared services (Flux bootstrap, SPIRE root, GitLab runners). One cluster per region rather than per team means **shared multi-tenant clusters** — namespace-based tenancy with quotas is almost certain at this scale.

### 1.3 Why no encapsulation matters 🟢

Cilium in BGP mode with no encapsulation (no VXLAN/Geneve) means **pod IPs are directly routable on the datacenter network**. Each node advertises its pod CIDR to the top-of-rack routers via BGP. Consequences:

- Zero overlay overhead → lower, more predictable latency (the HFT reason)
- Pods are first-class network citizens — external systems can reach pod IPs directly
- The platform team must coordinate IP address management (IPAM) with the network team
- Troubleshooting requires understanding the physical routing domain, not just k8s

---

## 2. Developer Workflows (the user's perspective)

### 2.1 Deploying a new application 🟡 (high confidence on shape, medium on details)

The key insight: **the team chose KubeVela + CUE, which means they deliberately do NOT want developers writing raw Kubernetes YAML or Helm charts.** The predicted flow:

```
Developer                     Platform (automated)
─────────                     ────────────────────
1. Create repo from GitLab
   template ("golden path")
2. Write app code +
   a small app spec file  ──►  CI builds image, pushes to
   (KubeVela Application       internal registry
   or CUE-defined spec)
3. Merge to main          ──►  FluxCD detects change in
                               GitOps repo
                          ──►  KubeVela controller renders
                               the Application into raw k8s
                               resources (Deployment, Service,
                               HTTPRoute, ServiceMonitor...)
                          ──►  App is live
```

What the developer's spec file probably looks like (KubeVela Application, possibly generated from CUE):

```yaml
apiVersion: core.oam.dev/v1beta1
kind: Application
metadata:
  name: my-pricing-service
spec:
  components:
    - name: api
      type: webservice          # platform-defined ComponentDefinition
      properties:
        image: registry.macdonalds.internal/pricing/api:v1.2.3
        cpu: "2"
        memory: 4Gi
      traits:
        - type: http-route       # platform-defined TraitDefinition
          properties:
            host: pricing.apps.macdonalds.internal
        - type: metrics          # auto-wires VictoriaMetrics scraping
          properties:
            port: 9090
```

**The platform team's real product is the library of ComponentDefinitions and TraitDefinitions** (written in CUE — this is why CUE is on the stack). Developers pick from a menu: `webservice`, `worker`, `cronjob`, `stateful-service`. Each definition encodes the team's opinions: resource limits, security contexts, SPIFFE identity injection, observability wiring, topology spread.

**Important nuance on "how they deploy a new Helm chart":** they most likely *don't*, at least not directly. Two possibilities:
- KubeVela has a `helm` component type that can wrap charts — possibly allowed for third-party software
- Or third-party charts are onboarded *by the platform team* via Flux `HelmRelease`, and developers only deploy first-party apps via KubeVela

Ask this exact question in week one — it reveals a lot about how locked-down the abstraction is.

### 2.2 GitOps repo structure 🟡

With Flux and <5 clusters, expect a monorepo or small set of repos like:

```
platform-gitops/
├── clusters/
│   ├── prod-us/
│   │   ├── flux-system/
│   │   ├── infrastructure/     # Cilium, SPIRE, StackGres operator...
│   │   └── tenants/            # namespace + RBAC + quota per team
│   ├── prod-eu/
│   ├── prod-apac/
│   └── staging/
├── definitions/                # KubeVela ComponentDefinitions (CUE)
└── tenants/
    ├── team-pricing/
    │   └── apps/               # KubeVela Applications per app
    └── team-risk/
```

Developers get merge rights only to their tenant directory. CODEOWNERS + GitLab approval rules enforce the boundary. Platform team owns `infrastructure/` and `definitions/`.

### 2.3 DNS and service exposure 🟡

The predicted flow for "how do I get a hostname for my service":

1. Developer declares a hostname in their app spec (the `http-route` trait above)
2. The trait renders a **Gateway API `HTTPRoute`** attached to a shared, platform-managed **`Gateway`** (implemented by Cilium)
3. **external-dns** (or an in-house equivalent — Macdonalds is old enough to have in-house DNS tooling) watches HTTPRoutes/Gateways and creates DNS records in the corporate DNS (likely Infoblox, BIND, or similar enterprise DNS, *not* a cloud provider)
4. The Gateway's IP is a **BGP-advertised LoadBalancer IP** — Cilium announces it to the network fabric, no MetalLB needed

Likely conventions:
- Wildcard domain per cluster: `*.apps.us.macdonalds.internal`, `*.apps.sg.macdonalds.internal`
- Internal-only; TLS possibly terminated at the Gateway with certs from an internal CA (and pod-to-pod mTLS handled separately by SPIFFE/SPIRE)

### 2.4 East-west traffic and identity 🟢 (that SPIRE is central) / 🟡 (exact mechanism)

SPIFFE/SPIRE on the stack means **workload identity, not network location, is the security boundary**:

- Every pod gets an X.509 SVID with an identity like `spiffe://macdonalds.internal/ns/pricing/sa/api`
- SPIRE agent runs as DaemonSet, attests workloads via k8s node/pod attestation
- Services do mTLS with SVIDs — either natively (Go services using go-spiffe, since Golang is the stack language) or via a proxy/Cilium mutual auth integration
- Notably **no Istio/Linkerd on the list** — they likely avoided a service mesh for latency reasons and do identity-based mTLS in-process. This is a very HFT choice.
- Cilium NetworkPolicies provide L3/L4 segmentation on top

### 2.5 Observability onboarding 🟡

The `metrics` trait pattern again — observability is probably near-zero-effort by design:

**Metrics:**
1. Developer exposes `/metrics` on a port and declares it in the app spec
2. The trait renders a `VMServiceScrape` (VictoriaMetrics operator CRD) or the vmagent scrape config picks it up by convention/annotation
3. Metrics flow: app → vmagent → VictoriaMetrics (likely cluster-local, with global query federation via Promxify/vmselect for cross-region views)
4. Dashboards: Grafana, almost certainly, with per-team folders

**Logs:**
1. Developer just writes to stdout in a structured format (JSON — likely mandated by convention)
2. Fluent Bit DaemonSet ships everything → VictoriaLogs (and/or Quickwit for full-text search)
3. Zero per-app onboarding; enrichment (namespace, pod, team labels) is automatic

**Traces:**
1. Apps instrument with OpenTelemetry SDKs (Go SDK, given the stack)
2. Send OTLP → OTel Collector (DaemonSet or Deployment gateway)
3. Backend uncertain — possibly Quickwit (it has trace support), Grafana Tempo, or an in-house store 🔴

The presence of both OpenSearch **and** Quickwit (with a "?") suggests an in-flight migration — probably OpenSearch is legacy and Quickwit is the target (much cheaper, object-storage-native). Expect migration work; this could be one of your early projects.

### 2.6 Self-service Postgres via StackGres 🟡

Your manager flagged StackGres as important, which strongly suggests **databases-as-a-service is a flagship platform offering**. Predicted flow:

1. Developer adds a database claim to their tenant directory — either a raw `SGCluster` CR or (more likely, given the abstraction philosophy) a KubeVela component like:

```yaml
    - name: db
      type: postgres            # platform-defined component
      properties:
        size: medium            # t-shirt sizes mapping to CPU/mem/storage
        version: "16"
        backups: standard
```

2. Merge → Flux applies → StackGres operator provisions the cluster: Patroni-managed HA Postgres, built-in connection pooling (PgBouncer), automated backups (to object storage or Weka), monitoring pre-wired into VictoriaMetrics
3. Credentials delivered as a k8s Secret in the team's namespace (possibly with rotation)
4. Storage rides on **Weka CSI** — high-performance parallel filesystem, which makes running serious databases on k8s actually viable (this pairing is deliberate: StackGres on Weka is a strong signal they run real production databases on the platform, not toys)

Platform team's admin side: operator lifecycle/upgrades, defining the `SGInstanceProfile`/`SGPostgresConfig` catalog (the t-shirt sizes), backup policy, major-version upgrade orchestration, and being the escalation point for database incidents.

### 2.7 Streaming via Redpanda 🟡

Similar self-service shape, though possibly less abstracted:

- Likely a small number of **shared Redpanda clusters** (per region) rather than per-team clusters — streaming platforms are usually centralized
- Self-service topic creation via GitOps: a topic manifest in the tenant directory, applied by Flux (Redpanda has a k8s operator with `Topic` CRDs)
- Access control via SASL/mTLS — plausibly integrated with SPIFFE identities
- No ZooKeeper (Raft-native) and written in C++ with thread-per-core architecture — again, the latency-conscious choice over Kafka

---

## 3. Admin/Platform-Team Workflows (your side)

### 3.1 Cluster lifecycle 🟢

- Clusters built with **Kubespray** (Ansible) against Rocky/RHEL hosts — inventory and group_vars in Git
- Upgrades: Kubespray's rolling upgrade playbooks, staged staging → prod-region-by-region. With <5 clusters this is manual-ish, careful, scheduled work
- etcd: owned directly — backups (snapshot cron → object storage/Weka), defrag, monitoring quorum health. Expect strict runbooks
- Node OS patching coordinated with cluster drains — likely Ansible-driven

### 3.2 Platform component delivery 🟢

The platform team eats its own GitOps dogfood:

- All infrastructure components (Cilium, SPIRE, StackGres operator, VictoriaMetrics, OTel, Redpanda operator...) deployed as Flux `HelmRelease`/`Kustomization` from the `infrastructure/` directory
- Changes flow: MR → review → merge → Flux reconciles staging → verify → promote to prod clusters (probably via directory promotion or Flux dependencies)
- Renovate or similar bot probably automates upstream version bump MRs

### 3.3 Tenant onboarding 🟡

New team wants on the platform:

1. Platform team (or a self-service template) creates a tenant definition: namespace(s), ResourceQuota, LimitRange, RBAC bindings (mapped to LDAP/AD groups — a firm like Macdonalds runs LDAP), NetworkPolicies, GitLab group wiring
2. All of it is CUE-generated boilerplate in the `tenants/` directory — one MR, review, merge
3. Team gets: a namespace per environment, a GitLab template repo, docs

### 3.4 The CUE layer 🟢 (that it's load-bearing) / 🟡 (exact usage)

CUE likely serves three roles:

1. **Authoring KubeVela definitions** — KubeVela's ComponentDefinitions/TraitDefinitions are natively written in CUE. This is the single strongest explanation for CUE on the stack
2. **Schema validation** — validating tenant configs and app specs in GitLab CI before Flux ever sees them ("shift-left" config errors)
3. **Generating repetitive config** — per-cluster/per-tenant manifests generated from a single source of truth instead of copy-paste YAML

Your CUE prep should focus on: definitions/schemas, unification semantics, and the `cue vet`/`cue export` workflow in CI.

### 3.5 Incident response 🟡

- On-call rotation within the platform team, paged via VictoriaMetrics alerting (vmalert) → likely PagerDuty/Opsgenie or in-house
- The platform is in the critical path of trading support systems — expect strict change windows aligned to market hours, freeze periods, and a culture of "no changes during trading hours" for anything risky
- Blameless postmortems, runbooks in GitLab wiki or similar

---

## 4. How the Tools Interlock — One Flow, End to End

**Scenario: developer ships a new version of a service with a database.**

1. Dev pushes code → **GitLab CI** builds, tests, pushes image to internal registry, and (likely) auto-bumps the image tag in the GitOps repo via MR or direct commit
2. **FluxCD** on the target cluster detects the Git change
3. Flux applies the **KubeVela Application**; the KubeVela controller renders it — using **CUE**-based definitions — into Deployment, Service, HTTPRoute, VMServiceScrape, NetworkPolicy
4. Pods schedule; **Weka CSI** mounts any persistent volumes; **SPIRE** agent attests the new pods and issues SVIDs
5. **Cilium** (BGP mode) makes pod IPs routable; the **Gateway API** HTTPRoute wires the hostname through the shared Cilium Gateway; DNS record exists via the DNS automation
6. The service does mTLS to its **StackGres** Postgres using its SPIFFE identity / delivered credentials
7. It emits events to **Redpanda**, metrics scraped into **VictoriaMetrics**, stdout logs shipped by **Fluent Bit** into **VictoriaLogs**/**Quickwit**, traces via **OTel Collector**
8. vmalert watches the SLOs; if the deploy misbehaves, on-call gets paged and the rollback is a Git revert (GitOps!)

If you can narrate this flow on day one and ask sharp questions about where reality diverges from it, you'll make an immediate impression.

---

## 5. Open Questions to Resolve in Week One

Ranked by how much the answer changes your mental model:

1. **Do developers write KubeVela Applications directly, or is there another layer on top** (internal portal/CLI, or CUE-generated specs)?
2. **How are third-party Helm charts onboarded** — dev-accessible KubeVela helm components, or platform-team-only Flux HelmReleases?
3. **How is multi-cluster/multi-region handled in the GitOps repo** — directory-per-cluster promotion, Flux image automation, something in-house?
4. **What is the SPIFFE mTLS enforcement mechanism** — native go-spiffe in apps, Cilium mutual auth, or a proxy?
5. **What's the OpenSearch → Quickwit story** — migration in flight? Who owns search UX?
6. **How does IPAM/BGP peering coordination with the network team work** — who owns the ASN plan, per-rack peering, LB IP pools?
7. **What are the trading-hours change windows and freeze rules?**
8. **Is there a global VictoriaMetrics view across regions**, or per-cluster silos?
9. **Do any workloads bypass the platform** (latency-critical trading systems almost certainly run on bare metal outside k8s — where's the boundary?)
10. **What does the StackGres service catalog look like** — sizes, HA tiers, who handles major version upgrades?

---

## 6. Confidence Summary

| Prediction | Confidence |
|---|---|
| Team owns everything from OS up to app abstraction | 🟢 High |
| Shared multi-tenant clusters, namespace tenancy | 🟢 High |
| KubeVela definitions in CUE are the "product" devs consume | 🟢 High |
| No service mesh; SPIFFE-based identity instead | 🟢 High |
| Pod IPs directly routable, LB IPs via Cilium BGP | 🟢 High |
| Flux monorepo with cluster/tenant directory split | 🟡 Medium |
| DNS automation from Gateway API resources | 🟡 Medium |
| StackGres as flagship self-service DBaaS with t-shirt sizing | 🟡 Medium |
| OpenSearch→Quickwit migration in flight | 🟡 Medium |
| Trace backend choice | 🔴 Low |
| Exact promotion/rollout mechanics across regions | 🔴 Low |

---

*Generated as a preparation aid — treat every section as a hypothesis to verify, not a fact. The fastest way to learn a platform is to arrive with a wrong-but-specific model and let reality correct it.*
-e 

---

# PART 2 — GitLab Repo Layout & Detailed Workflows


> Companion to the architecture doc. This goes deep on the *mechanics*: what the repos probably look like, exactly how things land on clusters, and what BAU/project work looks like for the platform team. Same confidence philosophy — these are specific, falsifiable predictions to verify in week one. Names, paths, and conventions are invented but realistic.

---

## 1. The Repository Landscape

With everything GitOps and <5 clusters, expect a small number of load-bearing repos rather than repo sprawl:

```
gitlab.macdonalds.internal/
├── platform/
│   ├── gitops/                  # ⭐ THE repo — Flux watches this
│   ├── definitions/             # KubeVela ComponentDefinitions/Traits (CUE)
│   ├── kubespray-inventory/     # cluster provisioning (Ansible inventory + group_vars)
│   ├── ci-templates/            # shared GitLab CI includes for the whole firm
│   ├── docs/                    # platform handbook / user docs
│   └── tools/                   # Go CLIs, operators, admission webhooks (in-house)
│
├── pricing/                     # example tenant group (a dev team)
│   ├── pricing-api/             # app source code
│   └── pricing-batch/
└── risk/
    └── risk-engine/
```

Two plausible variants for where *tenant app manifests* live:

- **Variant A (central):** all deployment specs live inside `platform/gitops/tenants/` — devs MR into the platform repo. Simpler for the platform team, one Flux source.
- **Variant B (federated):** each team has its own `*-deploy` repo, registered as an additional Flux `GitRepository`. More autonomy, more moving parts.

With a firm-wide user base I'd bet on **Variant A with strict CODEOWNERS**, because it gives the platform team one place to run validation CI and enforce schemas. Examples below assume Variant A.

---

## 2. The `platform/gitops` Repo in Detail

```
platform/gitops/
├── clusters/                          # entrypoint per cluster (Flux bootstrap points here)
│   ├── prod-us/
│   │   ├── flux-system/               # Flux's own manifests (bootstrap-generated)
│   │   ├── infrastructure.yaml        # Kustomization -> ../../infrastructure, US overlay
│   │   ├── platform-services.yaml     # Kustomization -> ../../platform-services
│   │   └── tenants.yaml               # Kustomization -> ../../tenants, US overlay
│   ├── prod-eu/
│   ├── prod-apac/
│   └── staging/
│
├── infrastructure/                    # cluster-critical, platform-team only
│   ├── base/
│   │   ├── cilium/                    # HelmRelease + BGP peering policies + LB IP pools
│   │   ├── spire/
│   │   ├── weka-csi/
│   │   ├── gateway/                   # shared Gateway objects, cert wiring
│   │   └── external-dns/
│   └── overlays/
│       ├── prod-us/                   # e.g. per-DC BGP ASNs, peer IPs, CIDR pools
│       ├── prod-eu/
│       └── ...
│
├── platform-services/                 # the "products": operators + shared instances
│   ├── stackgres/
│   │   ├── operator/                  # HelmRelease, pinned version
│   │   └── profiles/                  # SGInstanceProfile catalog (t-shirt sizes)
│   ├── redpanda/
│   │   ├── operator/
│   │   └── clusters/                  # shared Redpanda cluster CRs per region
│   ├── victoriametrics/
│   │   ├── vmcluster.yaml             # storage/select/insert topology
│   │   ├── vmagent.yaml
│   │   └── vmalert/
│   │       └── rules/                 # platform-level alert rules
│   ├── victorialogs/
│   ├── otel-collector/
│   ├── fluent-bit/
│   └── quickwit/
│
├── tenants/                           # 👥 the only place devs can merge to
│   ├── _template/                     # scaffolding for new tenants
│   ├── pricing/
│   │   ├── tenant.yaml                # generated: ns, quota, RBAC, netpol, limits
│   │   ├── apps/
│   │   │   ├── pricing-api.yaml       # KubeVela Application
│   │   │   └── pricing-batch.yaml
│   │   ├── databases/
│   │   │   └── pricing-db.yaml        # postgres claim
│   │   ├── streams/
│   │   │   └── topics.yaml            # Redpanda Topic CRs
│   │   └── observability/
│   │       ├── alerts.yaml            # team-owned VMRule alerts
│   │       └── dashboards/            # Grafana dashboards as code (json/jsonnet)
│   └── risk/
│       └── ...
│
├── cue/                               # schemas + generation
│   ├── schemas/                       # what a valid tenant/app/topic looks like
│   └── gen/                           # cue export templates for tenant.yaml etc
│
└── .gitlab-ci.yml                     # validation pipeline (see §3.2)
```

Key structural ideas to notice:

- **`clusters/<name>/` is just pointers.** Each cluster's directory contains Flux `Kustomization` objects referencing shared bases + a cluster overlay. Promotion between environments = changing what a pointer references (or bumping a version in an overlay).
- **Three trust zones:** `infrastructure/` (platform only, high blast radius), `platform-services/` (platform only, the product), `tenants/` (developer-writable, schema-policed).
- **Flux dependency ordering:** `infrastructure` Kustomization has no dependencies; `platform-services` `dependsOn: infrastructure`; `tenants` `dependsOn: platform-services`. This is how a fresh cluster bootstraps in the right order.

---

## 3. Workflow 1 — Onboarding a Brand-New Application (end to end)

**Cast:** Priya, a developer on the `pricing` team, needs a new Go service `quote-gateway` in production.

### 3.1 Scaffolding

Priya creates the source repo from a GitLab project template (`platform/ci-templates` provides the CI):

```yaml
# pricing/quote-gateway/.gitlab-ci.yml
include:
  - project: platform/ci-templates
    file: /go-service.yml    # lint, test, build, scan, push image, bump manifest
```

The shared template gives her: Go build + unit tests, container build (probably Kaniko/buildah — no Docker daemon on runners), image scan, push to `registry.macdonalds.internal/pricing/quote-gateway`, and a final `deploy-bump` job.

### 3.2 The app spec MR

She adds one file to the gitops repo:

```yaml
# platform/gitops/tenants/pricing/apps/quote-gateway.yaml
apiVersion: core.oam.dev/v1beta1
kind: Application
metadata:
  name: quote-gateway
  namespace: pricing
spec:
  components:
    - name: quote-gateway
      type: webservice                      # platform ComponentDefinition
      properties:
        image: registry.macdonalds.internal/pricing/quote-gateway:0.1.0   # tag pinned, not :latest
        replicas: 3
        cpu: "2"
        memory: 2Gi
        port: 8080
      traits:
        - type: http-route
          properties:
            host: quote-gateway.pricing.apps.us.macdonalds.internal
        - type: metrics
          properties:
            port: 9090
            path: /metrics
        - type: autoscale
          properties:
            min: 3
            max: 10
            cpuTarget: 70
```

She opens an MR. The gitops repo's pipeline runs:

```yaml
# platform/gitops/.gitlab-ci.yml (conceptual)
validate:
  script:
    - cue vet ./cue/schemas/... ./tenants/...    # schema validation — bad specs fail here
    - vela dry-run tenants/pricing/apps/quote-gateway.yaml   # render check
    - kubeconform --strict rendered/             # k8s API validation
    - conftest test rendered/                     # policy (no :latest, limits set, netpol sane)
diff:
  script:
    - flux diff kustomization tenants --path ./tenants   # posted as MR comment
```

**This CI stage is the platform team's front door** — most of the "how do we stop people breaking things" energy lives here, not in cluster-side admission (though an admission webhook likely exists as backstop). CODEOWNERS requires one approval from `@pricing/maintainers`; touching anything outside `tenants/pricing/` would require platform-team approval.

### 3.3 What happens on merge

1. Flux's `GitRepository` source polls (or gets a GitLab webhook → Flux notification-controller receiver) — sees new commit on `main`
2. The `tenants` `Kustomization` for `prod-us` reconciles, applies the `Application` CR
3. **KubeVela controller** picks it up, runs the CUE in the `webservice` ComponentDefinition, and renders: `Deployment` (with security context, topology spread, SPIRE-compatible labels, resource limits baked in), `Service`, `HTTPRoute` (attached to the shared Gateway), `VMServiceScrape`, `HPA`
4. Pods schedule → SPIRE agent attests them (k8s workload attestor: namespace `pricing`, serviceaccount `quote-gateway`) → issues SVID `spiffe://macdonalds.internal/ns/pricing/sa/quote-gateway`
5. Cilium: pod IPs routable via BGP; the HTTPRoute programs the Cilium Gateway; external-dns sees the route and creates `quote-gateway.pricing.apps.us.macdonalds.internal` → Gateway LB IP (itself BGP-advertised from a `CiliumLoadBalancerIPPool`)
6. vmagent starts scraping `:9090/metrics` within a minute; Fluent Bit is already shipping stdout
7. Flux notification-controller posts success/failure to the team's chat channel and back to the GitLab commit status

Total elapsed time from merge: a couple of minutes. **Nobody with kubectl was involved.**

### 3.4 Day-2: shipping version 0.2.0

Priya merges code to `quote-gateway` main. CI builds `0.2.0`, then the `deploy-bump` job automates the gitops change. Two common mechanisms — Macdonalds could use either:

- **CI-driven bump (most likely):** the job clones `platform/gitops`, runs `yq -i '.spec.components[0].properties.image = "...:0.2.0"' tenants/pricing/apps/quote-gateway.yaml`, and opens an MR (auto-merge for staging; human approval for prod). Explicit, auditable, promotion = MR.
- **Flux Image Automation:** `ImageRepository` + `ImagePolicy` CRs watch the registry and Flux commits the bump itself. Less common in shops that want strict prod change control — I'd bet against it for prod here. 🟡

Rollback either way = `git revert` + merge. GitOps means the cluster follows Git, always.

---

## 4. Workflow 2 — Redpanda: Topics, ACLs, and Platform Work

### 4.1 Developer side: creating a topic

The Redpanda operator supports a `Topic` CRD, so self-service is another file in the tenant directory:

```yaml
# platform/gitops/tenants/pricing/streams/topics.yaml
apiVersion: cluster.redpanda.com/v1alpha2
kind: Topic
metadata:
  name: pricing-quotes-v1
  namespace: pricing
spec:
  cluster:
    clusterRef:
      name: redpanda-shared-us          # the platform's shared regional cluster
  partitions: 12
  replicationFactor: 3
  additionalConfig:
    retention.ms: "86400000"            # 1 day
    cleanup.policy: delete
```

CI validation likely enforces platform policy in CUE/conftest: naming convention (`<team>-<stream>-v<N>`), partition ceilings without platform approval, replication factor exactly 3, retention caps (storage is money). Merge → Flux applies → operator creates the topic on the shared cluster. 

**Access control** 🟡: an ACL/User CR alongside the topic, or — the more elegant possibility given the stack — authorization derived from SPIFFE identities via mTLS principals. Either way, the *consumer* team's identity gets read access via an MR that the *producing* team approves (data ownership as code). Ask how this actually works; cross-team topic access is a classic policy question.

### 4.2 Platform side: Redpanda BAU

What "owning Redpanda" means day to day:

- **Capacity & partition hygiene:** watching per-broker partition counts, disk (local NVMe, likely — Redpanda wants fast local disk; possibly Weka), producer/consumer throughput. Fielding "we need 200 partitions" requests and pushing back with math.
- **Version upgrades:** operator-orchestrated rolling broker restarts. Because it's shared multi-tenant infra, this is change-window work with comms to every consuming team. Staging soak first.
- **Consumer-lag firefighting:** teams page you saying "the platform is slow" when their consumer is lagging; you prove it with metrics (Redpanda exports rich Prometheus metrics → VictoriaMetrics dashboards you own).
- **Tiered storage project work** 🟡: Redpanda supports offloading segments to object storage — if not enabled yet, enabling it (retention economics) is exactly the kind of quarter-long project a new joiner could pick up.

### 4.3 Example project you might get: "Topic self-service v2"

Realistic project shape: today, ACL requests need a platform MR approval; goal, make cross-team access fully self-service with producing-team approval only. Work involves: CUE schema for an `AccessGrant` abstraction, a small Go controller or CI job translating grants to Redpanda ACLs, CODEOWNERS restructure, docs. This is the archetypal PaaS project — **turning a ticket into a merge request.**

---

## 5. Workflow 3 — VictoriaMetrics: Onboarding, Alerts, and Platform Work

### 5.1 Developer side: metrics + alerts + dashboards

Metrics onboarding, as covered, is the `metrics` trait → `VMServiceScrape` rendered automatically. The interesting self-service parts are alerts and dashboards:

```yaml
# platform/gitops/tenants/pricing/observability/alerts.yaml
apiVersion: operator.victoriametrics.com/v1beta1
kind: VMRule
metadata:
  name: pricing-alerts
  namespace: pricing
spec:
  groups:
    - name: quote-gateway
      rules:
        - alert: QuoteGatewayHighErrorRate
          expr: |
            sum(rate(http_requests_total{namespace="pricing",app="quote-gateway",code=~"5.."}[5m]))
              /
            sum(rate(http_requests_total{namespace="pricing",app="quote-gateway"}[5m])) > 0.02
          for: 5m
          labels:
            severity: page
            team: pricing            # routing key — alertmanager routes on this
          annotations:
            summary: "quote-gateway 5xx > 2%"
            runbook: "https://gitlab.macdonalds.internal/pricing/quote-gateway/-/wikis/runbook"
```

CI validates: `promtool`/vmalert rule syntax check, `team` label present (so pages route to *their* on-call, not yours), maybe a lint that every `severity: page` rule has a runbook link. Alertmanager routing tree (platform-owned) fans out by team label to the right escalation.

Dashboards: JSON (or jsonnet/grafonnet) files in `dashboards/`, picked up by a Grafana sidecar watching ConfigMaps that Flux creates. Dashboards-as-code, per-team Grafana folders.

### 5.2 Platform side: VictoriaMetrics BAU

- **The VMCluster topology is yours:** vminsert/vmselect/vmstorage sizing, retention (e.g. 30d high-res local, longer downsampled), storage on Weka or local NVMe. Scaling vmstorage is real operational work (it's stateful; adding nodes changes the shard map).
- **Cardinality policing** — the classic. Some team ships a metric labeled by order-ID and vmstorage memory climbs. You own the cardinality dashboards (`vm_cache_*`, top-N series by metric name), the admonishment, and probably CI-side guardrails (scrape config `metric_relabel_configs` dropping known offenders). Expect this to be a recurring theme; it's every metrics platform's groundhog day.
- **Global view** 🟡: per-region VMClusters with either vmselect multi-tenant federation or a global query layer for cross-region dashboards. If it doesn't exist, that's a project.
- **vmagent fleet:** scrape interval policy, remote-write queue tuning, sharding vmagent if scrape targets grow.
- **Alertmanager routing tree stewardship:** teams constantly want routing tweaks; keeping the tree sane is janitorial but important.

### 5.3 Logs/traces equivalents (brief)

- **VictoriaLogs/Quickwit BAU:** ingestion pipeline health (Fluent Bit buffer/backpressure), retention & index lifecycle, the probable OpenSearch→Quickwit migration (dual-write, query parity checks, cutover — a strong candidate for project work).
- **OTel Collector BAU:** pipeline config is platform-owned (processors for k8s metadata enrichment, tail sampling policy); teams just point SDKs at the collector endpoint. Sampling policy debates ("why is my trace missing") land on you.

---

## 6. Workflow 4 — StackGres Database, Concretely

Developer claim (raw CR shown; a KubeVela `postgres` component may wrap this):

```yaml
# platform/gitops/tenants/pricing/databases/pricing-db.yaml
apiVersion: stackgres.io/v1
kind: SGCluster
metadata:
  name: pricing-db
  namespace: pricing
spec:
  postgres:
    version: "16"
  instances: 2                          # HA pair, Patroni-managed
  sgInstanceProfile: size-medium        # from the platform catalog
  configurations:
    sgPostgresConfig: pg16-default      # platform-tuned postgresql.conf
    sgPoolingConfig: pgbouncer-default
    backups:
      - sgObjectStorage: backups-us     # platform-owned backup target
        cronSchedule: "0 2 * * *"
        retention: 7
  pods:
    persistentVolume:
      size: 200Gi
      storageClass: weka-fast
```

On merge: operator spins up Patroni pods, PgBouncer sidecar, backup CronJobs, and pre-wired Postgres exporter metrics into VictoriaMetrics. Credentials appear as a Secret in the `pricing` namespace; the app references it. 

Platform BAU: catalog maintenance (profiles/configs are versioned files in `platform-services/stackgres/profiles/`), operator upgrades (careful — it touches every DB), **major version upgrade campaigns** (SGDbOps CRs, coordinated per-team), backup restore drills (if you don't test restores, you don't have backups), and being the "is it the database or your query" escalation point.

---

## 7. Cross-Cutting: How Promotion Across Clusters Likely Works 🟡

The cleanest pattern consistent with everything above:

- `staging` cluster reconciles from the same directories, but overlays pin **channel: latest**; prod overlays pin explicit versions
- Promotion = MR that updates the prod overlay (an image tag, a HelmRelease chart version, a Kustomize ref) — often automated as a "promotion MR" generated by CI after staging soak
- Region rollout for platform changes: staging → one prod region (probably the smallest) → remaining regions, with explicit merges gating each step, respecting market-hours freeze windows per region (which are *different* per region — APAC/EU/US markets — a genuinely interesting scheduling constraint your team lives with)

---

## 8. What This Means for Your Prep

Highest-leverage things to practice in the homelab, in order:

1. **Recreate the repo skeleton** (§2) in your own GitLab/Gitea and bootstrap Flux against it with the three-Kustomization dependency chain. Feeling the `dependsOn` ordering and overlay pattern is 80% of "understanding how stuff gets onboarded."
2. **Write one ComponentDefinition in CUE** (a simple `webservice`) and deploy an Application through it via Flux — the full §3 loop, minus GitLab CI.
3. **Deploy the VictoriaMetrics operator and get a VMServiceScrape + VMRule working** — then deliberately create a cardinality explosion and watch it, so you recognize the failure mode.
4. **StackGres:** one SGCluster, take a backup, **do a restore**, and do a minor version SGDbOps upgrade.
5. **Redpanda operator + one Topic CR**, produce/consume with a Go client (rpk makes this fast).
6. Simulate a promotion: staging overlay auto-bumps, prod overlay bumps via MR you "approve."

If you've physically done those six loops, the real environment will feel like a bigger, stricter version of something you already know.

---

*Same caveat as before: specific to be useful, wrong in details by design. Bring it to week one as a hypothesis sheet.*
