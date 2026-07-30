---
title: "VictoriaMetrics & VictoriaLogs"
subtitle: "A Platform Engineer's Field Guide to Running Observability as a Shared Service"
author: "Study Booklet"
date: "July 2026"
toc: true
toc-depth: 2
numbersections: true
geometry: "margin=2.2cm"
fontsize: 10pt
colorlinks: true
linkcolor: NavyBlue
urlcolor: NavyBlue
---

\newpage

# High-Level Overview

## What VictoriaMetrics Is

VictoriaMetrics (VM) is a high-performance time-series database that speaks the Prometheus ecosystem's protocols. The fastest way to build a mental model: **VM is to Prometheus what a central Consul cluster is to local agents** — the local scrapers stay lightweight and disposable, while the durable, queryable state lives in one shared, well-operated backend.

Key properties that matter in production:

- **Prometheus-compatible surface area.** Accepts Prometheus `remote_write`, exposes `/api/v1/query` and friends, and runs PromQL (technically **MetricsQL**, a superset with extra functions like `histogram_quantiles`, `rate` over irregular intervals, and label manipulation helpers). Grafana points at it exactly as it would at Prometheus.
- **Two deployment modes.** *Single-node* — one binary (`victoria-metrics`) that ingests, stores, and queries. *Cluster* — three separately scalable roles (`vminsert`, `vmstorage`, `vmselect`). The single-node binary routinely handles 1M+ samples/sec on modest hardware; many orgs never need cluster mode.
- **Long-term storage.** Retention is a flag (`-retentionPeriod=12`), not an architecture project. Compression is significantly better than vanilla Prometheus TSDB, typically <1 byte per sample after compression.
- **No external dependencies.** No object storage requirement (unlike Thanos/Mimir/Cortex), no ZooKeeper, no consensus protocol between storage nodes. Data lives on local block storage.

The honest positioning against alternatives: Thanos and Mimir buy you object-storage economics and "infinite" retention at the cost of many moving parts (compactors, store gateways, queriers, caches). VM buys you **operational simplicity and raw efficiency** at the cost of being tied to block storage and (in the open-source version) a coarser multi-tenancy model. Platform teams who value a small blast radius and a short 3 a.m. debugging path tend to pick VM.

## What VictoriaLogs Is

VictoriaLogs (VL) is the same philosophy applied to logs: a single, efficient, purpose-built log database with its own query language, **LogsQL**. Mental model: **VL is to the ELK stack what HAProxy is to a full service-mesh — deliberately smaller, deliberately simpler, and much cheaper to run** for the 90% use case of "ingest a firehose of structured logs, filter and aggregate them fast."

Key properties:

- **LogsQL** — a pipe-based query language (`_time:5m error | stats by (service) count()`), closer to Splunk/Kusto ergonomics than to Lucene.
- **Stream-oriented storage.** Every log entry belongs to a *log stream* identified by a small set of stream fields (e.g. `{namespace, pod, container}`). Streams are VL's equivalent of a Prometheus time series — this analogy matters enormously for capacity planning and sharp edges (see §9).
- **Wide ingestion surface.** Accepts Elasticsearch bulk API, Loki push API, OTLP logs, JSON-lines, and syslog — so Fluent Bit, Vector, Filebeat, and OpenTelemetry Collector all work as shippers without a custom protocol.
- **Single-node first, cluster mode available.** Single-node VL handles tens of TB. Cluster mode (`vlinsert` / `vlselect` / `vlstorage`) mirrors the VM cluster shape for horizontal scale.
- **Full-text search without heavy indexes.** VL indexes stream fields and word tokens cheaply rather than building Elasticsearch-style inverted indexes for every field, which is why its disk and RAM footprint is typically 10–30x smaller than an equivalent ELK deployment.

## Where They Sit in an Organization

The standard operating model in multi-team companies is **hub-and-spoke, platform-owned center**:

```
  Product team clusters / VMs (spokes)          Platform-owned core (hub)
  ─────────────────────────────────────         ───────────────────────────────
  apps ──> vmagent  (scrape + relabel) ──────┐
  apps ──> vmagent  (another cluster)  ──────┼──> vmauth ──> VictoriaMetrics
  Prometheus (legacy, remote_write)    ──────┘        │        cluster
                                                      │
  apps ──> Fluent Bit / OTel Collector ────────> vmauth/LB ──> VictoriaLogs
                                                      │
  Grafana / vmalert / on-call tooling  <──── reads ───┘
```

The division of labor that this booklet keeps returning to:

- **Platform team owns the hub**: the VM/VL clusters themselves, storage, retention, backups, authentication (`vmauth`), capacity, upgrades, and the *paved road* (templates, label standards, onboarding docs).
- **Product teams own the spokes and the content**: instrumentation in their code, their `vmagent`/collector configs (usually from a platform template), their dashboards, their alert rules, and their query hygiene.

This is the same ownership split you already know from running shared Vault or Consul: platform owns availability and guardrails of the service; tenants own what they put into it. Everything else in this booklet — tenancy, RBAC, onboarding, incident workflows — is a refinement of that one boundary.

\newpage
# Architecture & Components (with Team Boundaries)

## VictoriaMetrics: Single-Node Mode

One statically-linked binary. It scrapes (optionally), ingests remote_write, stores to a local data directory, and answers queries. There is no coordination, no sidecar, no schema.

```yaml
# Platform-owned: single-node VM via the VictoriaMetrics operator
apiVersion: operator.victoriametrics.com/v1beta1
kind: VMSingle
metadata:
  name: vm-central
  namespace: observability          # platform-owned namespace
spec:
  retentionPeriod: "12"             # months; the entire retention "architecture"
  storage:
    accessModes: ["ReadWriteOnce"]
    resources:
      requests:
        storage: 500Gi              # local block storage (Weka CSI works fine)
  resources:
    requests: { cpu: "2", memory: 8Gi }
    limits:   { memory: 12Gi }      # VM is memory-elastic; give headroom
  extraArgs:
    maxLabelsPerTimeseries: "30"    # guardrail against label abuse (platform sets)
    search.maxUniqueTimeseries: "300000"  # per-query blast-radius limit
```

Key fields in plain language: `retentionPeriod` is how long samples live before background deletion; `extraArgs` is where platform guardrails go — limits on label counts, query cost, and concurrent selects. Everything under `spec` is platform-owned; app teams never touch this object.

**Opinion:** start single-node and stay there longer than you think. A single-node VM on a 16-core box with fast NVMe outperforms many three-tier "scalable" stacks, and its failure modes fit in your head. Move to cluster mode for one of three reasons only: you exceed single-machine capacity, you need `replicationFactor > 1`, or you need URL-path multi-tenancy (cluster-only feature).

## VictoriaMetrics: Cluster Mode

Three roles, deliberately boring:

- **`vminsert`** — stateless. Receives writes, consistent-hashes each series across `vmstorage` nodes, forwards. Scale horizontally behind a load balancer.
- **`vmstorage`** — stateful. Owns a shard of the data on local disk. Nodes are *independent*: no replication protocol between them, no leader election, no gossip. If `replicationFactor=2`, `vminsert` simply writes each sample to 2 storage nodes.
- **`vmselect`** — stateless. Fans a query out to every `vmstorage`, merges results, dedupes replicas. Scale horizontally; give it RAM for merging.

Contrast with what you know: **etcd/Consul-style consensus does not exist here.** There is no quorum. Losing a `vmstorage` node with `replicationFactor=1` means that shard's data is unavailable (and lost if the disk is gone) — but ingestion continues, because `vminsert` reroutes new writes to surviving nodes. This is a deliberate availability-over-consistency trade, and it drives two production behaviors you must internalize:

1. **Rerouting can cascade.** When a storage node dies or slows down, its write share reroutes to the others, raising their load. If the cluster was running hot, this can tip the next node over. Capacity-plan so the cluster survives N-1 nodes at peak ingest, exactly as you would for an HAProxy backend pool.
2. **Replication is not backup.** `replicationFactor=2` protects against a dead node, not against `DELETE` mishaps, corruption, or fat-fingered retention changes. `vmbackup` to object storage is still mandatory.

```yaml
# Platform-owned: cluster mode via operator
apiVersion: operator.victoriametrics.com/v1beta1
kind: VMCluster
metadata:
  name: vm-prod
  namespace: observability
spec:
  retentionPeriod: "12"
  replicationFactor: 2              # vminsert writes each sample to 2 nodes
  vmstorage:
    replicaCount: 4                 # survives 1 node loss at RF=2
    storage:
      volumeClaimTemplate:
        spec:
          resources: { requests: { storage: 2Ti } }
    extraArgs:
      dedup.minScrapeInterval: "15s"   # collapse replicated/HA duplicate samples
  vminsert:
    replicaCount: 3                 # stateless; scale on ingest volume
  vmselect:
    replicaCount: 3                 # stateless; scale on query load
    cacheMountPath: /select-cache
    extraArgs:
      dedup.minScrapeInterval: "15s"   # must match vmstorage
      search.maxConcurrentRequests: "8"
```

Plain-language notes: `dedup.minScrapeInterval` tells VM "if two samples for the same series land within 15s, keep one" — required when you run replicated `vmagent`s or HA Prometheus pairs, and when `replicationFactor > 1` so `vmselect` returns one copy. Set it on **both** select and storage or you'll chase phantom double-counting.

## The Satellite Components

- **`vmagent`** — the workhorse at the edge. Scrapes Prometheus targets, applies relabeling, and pushes via remote_write with **on-disk buffering** (`-remoteWrite.tmpDataPath`). If the central cluster is down or the WAN link flaps, vmagent buffers locally and drains later. Analogy: a Consul agent's local state + HAProxy's retry queue in one. Also does *stream aggregation* — pre-aggregating high-cardinality metrics at the edge before they ever hit central storage.
- **`vmalert`** — evaluates recording and alerting rules against a datasource (VM), writes recording-rule results back via remote_write, sends alerts to Alertmanager. It's stateless; state (silences, grouping) lives in Alertmanager as usual.
- **`vmauth`** — a small auth/routing proxy in front of the cluster. Maps bearer tokens or basic-auth users to backend URLs, tenant paths, rate limits, and enforced label filters. This is the platform team's policy enforcement point — the same architectural role Keycloak-fronting-HAProxy plays for HTTP apps.
- **`vmgateway`** (enterprise) — vmauth plus JWT validation, per-tenant rate limiting and accounting.
- **`vmbackup` / `vmrestore`** — snapshot-based backups to S3/GCS. Snapshots are instant (hard-link based), incremental uploads. Backups are per-`vmstorage`-node in cluster mode.
- **VictoriaMetrics operator** — Kubernetes CRDs (`VMSingle`, `VMCluster`, `VMAgent`, `VMAlert`, `VMAuth`, `VMUser`, `VMRule`, `VMServiceScrape`, ...). The operator is what makes the team boundary *mechanically real*: platform owns `VMCluster`/`VMAuth`; app teams own `VMServiceScrape` and `VMRule` objects in their own namespaces, and the operator aggregates them.

## VictoriaLogs: Modes and Components

- **Single-node `victoria-logs`** — one binary, local storage, all ingestion APIs, LogsQL query endpoint. Sizing rule of thumb: it comfortably ingests 100k+ log lines/sec/node and stores compressed logs at roughly 5–15x smaller than raw.
- **Cluster mode** — same shape as VM: `vlinsert` (stateless ingestion/routing), `vlstorage` (stateful shards), `vlselect` (stateless query/merge). Same no-consensus, reroute-on-failure semantics; same "replication is not backup" caveat.
- **Collectors** — VL deliberately has no mandatory agent. Fluent Bit (output: `http` in jsonline or the native VL output), Vector, Filebeat, and OTel Collector all ship to it. `vlagent` exists as a purpose-built shipper with vmagent-style disk buffering, but most orgs keep the collector they already run.

```yaml
# App-team-owned (from a platform template): Fluent Bit output to VL
[OUTPUT]
    Name        http
    Match       kube.*
    Host        vlogs.observability.svc
    Port        9428
    URI         /insert/jsonline?_stream_fields=namespace,app,container
    Format      json_lines
    Json_date_key   _time
    Json_date_format iso8601
    Header      AccountID 12         # tenant ID issued by platform team
```

The single most important line is `_stream_fields`. It declares which fields identify a *log stream*. Get this wrong (e.g., include `pod_name` with high churn, or worse `trace_id`) and you recreate Prometheus cardinality explosions in your log store. Platform teams should own and template this line, not app teams.

## Component-to-Team Responsibility Map

| Component | Owner | The other team's touchpoint |
|---|---|---|
| VMCluster / VMSingle / VLStorage | Platform | None. Consumed via URLs only. |
| vmauth / vmgateway, tokens, tenants | Platform | Teams receive credentials + endpoint URLs |
| vmbackup, retention, capacity | Platform | Teams request retention via tenant contract |
| vmagent (per-cluster infra instance) | Platform | Teams add `VMServiceScrape` / annotations |
| Fluent Bit / OTel DaemonSets | Platform (template + rollout) | Teams add parsers/labels via their namespace config |
| Instrumentation, /metrics endpoints | Product teams | Platform publishes naming standards |
| VMRule (alerts, recording rules) | Product teams (their services) | Platform reviews via CI lint; owns meta-alerts |
| Dashboards, queries | Product teams | Platform provides golden templates |

\newpage

# How Companies Use VictoriaMetrics in Practice

## The Dominant Pattern: Central Remote Storage, Edge Agents

Across companies of very different sizes, the converged pattern looks like this:

1. **One VM cluster (or HA pair of single-nodes) per environment tier** — commonly `prod` and `nonprod`, sometimes per-region for latency or data-locality reasons. Not per team, not per app.
2. **One `vmagent` deployment per Kubernetes cluster / datacenter / network zone.** It scrapes everything locally (kubelet, cAdvisor, node-exporter, app pods) and remote_writes to the central cluster. Local scraping keeps scrape traffic off the WAN; only compressed remote_write crosses zones.
3. **Legacy Prometheus instances kept temporarily as dumb scrapers**, with `remote_write` pointed at VM, then decommissioned once teams trust the central store.
4. **Grafana, vmalert, and on-call tooling read from the central cluster only.** Nobody queries edge agents; edge agents have no meaningful query surface anyway.

```yaml
# Platform-owned: per-cluster vmagent via operator
apiVersion: operator.victoriametrics.com/v1beta1
kind: VMAgent
metadata:
  name: vmagent-cluster-a
  namespace: observability
spec:
  replicaCount: 2                       # HA pair; dedup at central handles doubles
  externalLabels:
    cluster: "a"                        # every sample stamped with origin cluster
    env: "prod"
  remoteWrite:
    - url: "https://vm.example.internal/insert/0/prometheus/api/v1/write"
      # tmpDataPath below is the on-disk buffer that survives central outages
  extraArgs:
    remoteWrite.tmpDataPath: /vmagent-buffer
    remoteWrite.maxDiskUsagePerURL: "10GiB"
  selectAllByDefault: true              # discover VMServiceScrape objects everywhere
```

Plain language: `externalLabels` is how the platform guarantees every series can be traced to a source cluster/environment without trusting app teams to label correctly. `maxDiskUsagePerURL` bounds how much a central outage can fill edge disks — after that, oldest buffered data drops (a conscious platform decision to prefer bounded disk over unbounded backlog).

## Scenario A — 10 Product Teams, 3 Kubernetes Clusters, One VM Cluster

**Setup.** A mid-size org: clusters `a`, `b`, `c`; teams `payments`, `pricing`, `execution`, etc. One `VMCluster` (RF=2, 4 storage nodes) in a dedicated observability cluster. Logical tenancy: each team gets a tenant ID (see §5), issued and mapped by platform in `vmauth`.

**Responsibilities.**

- Platform runs the cluster, `vmauth`, the three per-cluster `vmagent`s, backups, and *meta-monitoring* (VM monitoring itself — always on a separate small VM single-node, never self-hosted in the thing it monitors).
- Teams own `VMServiceScrape` objects in their namespaces (the operator picks them up automatically via `selectAllByDefault`), their `VMRule` alert definitions, and their dashboards.

**How label standards are enforced.** Three mechanical layers, because documentation alone never works:

1. `externalLabels` on vmagent stamp `cluster` and `env` — teams cannot forge or omit them.
2. Relabeling on vmagent injects `namespace` and `team` (from a namespace annotation) onto every scraped series.
3. A CI lint job (platform-owned, runs in every team's repo via a shared GitLab CI include) validates metric names against the standard (`<domain>_<noun>_<unit>_<suffix>`, e.g. `orders_settlement_latency_seconds_bucket`) and rejects rules querying non-existent labels.

**Onboarding a new team** is: create namespace with `team=` annotation → platform issues tenant + token → team copies the scrape/rules templates → first dashboard lives within a day. The heavy lifting was done once, in the paved road.

## Scenario B — Migrating from Per-Team Prometheus Sprawl

**Starting state.** 14 Prometheus instances of wildly different versions, each with 15-day retention, owned (nominally) by teams, half of them unmonitored themselves. Queries across services are impossible; on-call engineers hop between 14 Grafana datasources.

**The migration that works in practice:**

1. **Phase 0 — stand up VM, change nothing else.** Platform deploys the VM cluster + vmauth + meta-monitoring. No team is asked to do anything yet.
2. **Phase 1 — remote_write fan-in.** Each Prometheus gets one config block added (by platform, via MR to each team's repo):

   ```yaml
   remote_write:
     - url: https://vm.example.internal/insert/0/prometheus/api/v1/write
       basic_auth:
         username: team-payments
         password_file: /etc/prom/vm-token
   ```

   Prometheus keeps scraping and alerting exactly as before. VM is a shadow copy. Grafana gets a second datasource pointed at VM; teams verify their dashboards render identically (MetricsQL is PromQL-compatible, so this almost always just works).
3. **Phase 2 — move reads.** Dashboards and vmalert switch to the VM datasource. Prometheus local retention drops to 2 days (it's now just a scraper with a safety buffer).
4. **Phase 3 — replace the scraper.** Prometheus instances are swapped for `vmagent` (lighter, disk-buffered, centrally templated), or simply deleted where the platform vmagent already covers the targets.

**Why this ordering matters:** every phase is independently reversible, and the risky step (moving alert evaluation) happens only after weeks of passive data verification. This is the same "shadow, verify, cut over" discipline you'd use for a load-balancer migration — never move writes and reads in the same step.

**Sharp edge during migrations:** duplicate series from Prometheus HA pairs. If both replicas remote_write, you must either set distinct `replica` external labels and let VM's deduplication (`-dedup.minScrapeInterval`) collapse them, or you'll see sawtooth artifacts in counters. Budget a week of "why does this graph look wrong" tickets regardless.

\newpage

# Onboarding Tech Teams: Workflows & Processes

## The Paved Road Principle

Onboarding is a product the platform team ships. Its quality is measured the same way you'd measure any internal service: time-to-first-metric, time-to-first-alert, and how few Slack questions the docs generate. The platform team that answers the same question three times has a documentation bug.

## Documentation Pattern That Works

A short, opinionated **"Getting Started with Metrics & Logs"** page containing, in order:

1. **The contract in one paragraph.** "Platform runs the stores, guarantees N-month retention and X ingest rate per tenant; you own instrumentation, alerts, and dashboards. Here's the escalation path each way."
2. **The standard label set** (non-negotiable, injected where possible):

   | Label | Source | Example |
   |---|---|---|
   | `cluster`, `env` | vmagent externalLabels (platform-injected) | `a`, `prod` |
   | `namespace` | scrape relabeling (platform-injected) | `payments` |
   | `team` | namespace annotation (platform-injected) | `payments` |
   | `service` | app-provided, mandatory | `settlement-api` |
   | `version` | app-provided, recommended | `1.14.2` |

   The same five labels appear as log fields in VL, with `namespace`,`service` doubling as stream fields. **One label taxonomy across metrics and logs** is what makes cross-signal debugging (§7) a join instead of a guessing game.
3. **Copy-paste templates** (below).
4. **Three worked examples**: a counter with a rate query, a histogram with a p99 query, and one alert rule — in the team's actual naming convention.
5. **The guardrails, stated plainly**: cardinality budget per tenant, forbidden label values (no user IDs, order IDs, or raw IPs as label values), query cost limits, and what happens when you exceed them (throttled, then paged, in that order).

## What a New Team Receives

- A **tenant ID** and a **write token** + **read token** (distinct — read tokens leak into dashboards and notebooks; write tokens live only in cluster secrets).
- Endpoint URLs: one write URL, one query URL, one logs-ingest URL — all pointing at `vmauth`, never at cluster internals. Platform can reshard, upgrade, or even swap the backend without teams noticing, exactly like keeping clients pointed at a VIP instead of at backend IPs.
- The template blocks:

```yaml
# App-team-owned, from platform template: scrape my service
apiVersion: operator.victoriametrics.com/v1beta1
kind: VMServiceScrape
metadata:
  name: settlement-api
  namespace: payments               # team's own namespace
spec:
  selector:
    matchLabels: { app: settlement-api }
  endpoints:
    - port: metrics
      interval: 15s
```

```yaml
# App-team-owned: first alert, reviewed by platform CI lint
apiVersion: operator.victoriametrics.com/v1beta1
kind: VMRule
metadata:
  name: settlement-api-alerts
  namespace: payments
spec:
  groups:
    - name: settlement-api.availability
      rules:
        - alert: SettlementApiHighErrorRate
          expr: |
            sum(rate(http_requests_total{service="settlement-api",code=~"5.."}[5m]))
              /
            sum(rate(http_requests_total{service="settlement-api"}[5m])) > 0.02
          for: 5m
          labels: { team: payments, severity: page }
          annotations:
            summary: ">2% 5xx on settlement-api for 5m"
```

```ini
# App-team-owned Fluent Bit fragment (platform ships the DaemonSet;
# teams only control parsing/enrichment for their namespace)
[FILTER]
    Name     modify
    Match    kube.payments.*
    Add      team payments
```

## The Onboarding Checklist (Printable)

**Team side:**

- [ ] Instrument service with client library; expose `/metrics`; include `service` label
- [ ] Apply `VMServiceScrape` from template; confirm series arrive (`up{service="..."}`)
- [ ] Confirm logs arrive with correct stream fields (`_stream:{namespace="...",service="..."}`)
- [ ] Define the minimal alert set: error rate, latency SLO, saturation, absence (`up == 0`)
- [ ] Build the team dashboard from the golden template; delete panels you don't understand
- [ ] Record who owns each alert (route in Alertmanager by `team` label)

**Platform side:**

- [ ] Create tenant, issue tokens, register in vmauth
- [ ] Verify namespace annotations (`team=`) so injected labels are correct
- [ ] Add tenant to capacity dashboard and per-tenant limit config
- [ ] 30-minute enablement session; record it once, link it forever

## Avoiding Observability Chaos: the Organizational Levers

- **Advertise internally like a product.** A launch announcement, a demo at engineering all-hands, office hours for the first month of any team's onboarding. Adoption of shared platforms is a marketing problem as much as a technical one.
- **Enforce in CI, not in review comments.** Metric-name linting, `VMRule` validation (`vmalert -dryRun` equivalent, promtool-style checks), and label-standard checks run in the shared pipeline. Humans review intent; machines review conventions.
- **Make the paved road genuinely faster than the dirt path.** If copying the template takes 10 minutes and hand-rolling takes a day, you will never need to *mandate* the standard. Guardrails that rely on mandate alone are already failing.

\newpage

# Tenant-Based / Multi-Tenant Architecture Patterns

## What "Tenant" Means (Decide This First)

A tenant is whatever boundary you need isolation, accounting, or access control around. In practice one of:

- **Team** — the most common internal choice; maps to on-call ownership and cost attribution.
- **Product / system** — when several teams share one large system (e.g. "the trading platform" vs "corporate IT").
- **Environment** — `prod` vs `staging`; many orgs make this the *strongest* boundary (separate clusters) and use logical tenancy only within an environment.
- **External customer** — SaaS vendors reselling observability; needs the strongest isolation and usually the enterprise feature set.

Pick one primary meaning and encode it consistently. Orgs that let "tenant" mean team in metrics and environment in logs pay for it in every cross-signal query and every access-control review.

## VictoriaMetrics Tenancy Mechanics

**Cluster mode has native URL-path tenancy.** Every tenant is an `AccountID` or `AccountID:ProjectID` pair baked into the write and read paths:

```text
write:  /insert/<accountID>[:<projectID>]/prometheus/api/v1/write
read:   /select/<accountID>[:<projectID>]/prometheus/api/v1/query
```

Facts that shape your design:

- Tenants are **created implicitly** on first write — there is no registry, no CRUD API. Your tenant registry is therefore your vmauth config plus your own documentation. Treat vmauth config as the source of truth, in Git.
- Data is **physically interleaved** on the same vmstorage disks; isolation is logical. One tenant cannot *query* another's data (different URL path), but they share disk, RAM, and CPU — noisy-neighbor protection comes from limits, not from tenancy itself.
- **Cross-tenant reads** exist for the platform team: multitenant endpoints (`/select/multitenant/...`) allow querying across tenants, with tenant identity exposed via labels. Guard this path tightly in vmauth — it is the "break glass" view.
- **Single-node VM has no URL tenancy.** Single-node multi-tenancy is done with an injected label (e.g. `tenant=payments`) plus vmauth-enforced label filters on read. Workable, but enforcement is only as strong as the proxy in front.

Two implementation styles:

```yaml
# Style 1: tenant in the URL path (cluster mode) — vmagent per team or
# per-tenant remoteWrite entries with team-scoped tokens
remoteWrite:
  - url: "https://vm.internal/insert/12/prometheus/api/v1/write"   # tenant 12
```

```yaml
# Style 2: tenant as a label (works everywhere; needed for single-node)
remoteWrite:
  - url: "https://vm.internal/api/v1/write"
    # vmagent stamps the label so apps can't forge it
extraArgs: {}
# in VMAgent spec:
externalLabels:
  tenant: "payments"
```

**Opinion:** in cluster mode, prefer URL-path tenancy issued by vmauth, because the tenant assignment then lives in platform-controlled credentials rather than in labels an app could emit incorrectly. Use the `tenant` label *in addition* when you want easy cross-tenant platform dashboards.

## VictoriaLogs Tenancy Mechanics

VL mirrors the model: tenants are `(AccountID, ProjectID)` pairs supplied as **HTTP headers** on ingest and query:

```ini
# Fluent Bit — platform-templated headers pin the tenant
[OUTPUT]
    Name    http
    Match   kube.payments.*
    URI     /insert/jsonline?_stream_fields=namespace,service
    Header  AccountID 12
    Header  ProjectID 0
```

Same properties: implicit creation, logical isolation on shared storage, header-based selection at query time. The same vmauth instance can front VL and inject/enforce the tenant headers per credential, giving you one policy point for both signals.

## The Isolation Spectrum

| Pattern | Isolation | Platform ops load | Cost profile | When |
|---|---|---|---|---|
| Shared cluster, labels only | Weak (proxy-enforced) | Lowest | Best consolidation | Small org, high trust, single-node |
| Shared cluster, URL/header tenancy | Medium (credential-enforced) | Low | Best consolidation | **Default for internal multi-team** |
| Shared cluster + per-tenant limits/quotas | Medium + bounded blast radius | Medium | Slight overhead | The above, grown up |
| Cluster per environment (prod/nonprod) | Strong between envs | Medium | Duplicate control planes | Nearly everyone should do this split |
| Cluster (or VMSingle) per tenant | Strongest | Highest (N of everything) | Worst; idle headroom × N | Compliance walls, external customers, or one pathological tenant |

**Noisy neighbors** are the force pushing you rightward in that table. The three noisy-neighbor vectors and their mitigations:

1. **Ingest floods** (deploy bug emits 10M new series): per-tenant ingest rate limits at vmauth/vmgateway, `-maxLabelsPerTimeseries`, cardinality alerts per tenant (`vm_new_timeseries_created_total` by tenant, from multitenant meta-queries).
2. **Query bombs** (someone runs a 30-day `group by (pod)` over everything): `-search.maxUniqueTimeseries`, `-search.maxQueryDuration`, `-search.maxConcurrentRequests`, and per-user concurrency in vmauth. These flags are your circuit breakers; ship them from day one, not after the first incident.
3. **Retention/disk creep**: per-tenant retention (enterprise feature) or — the OSS workaround — separate clusters for the tenants with genuinely different retention needs.

**Blast-radius framing:** logical tenancy contains *access*; only limits contain *load*; only separate clusters contain *failure*. State this sentence in your design docs and most tenancy debates resolve themselves.

\newpage

# Access Control, RBAC, and Self-Service

## vmauth as the Policy Enforcement Point

All reads and writes flow through vmauth. Its config is a Git-versioned, platform-owned file — reviewable, diffable, auditable, and the de facto tenant registry:

```yaml
# Platform-owned vmauth config (rendered from VMAuth/VMUser CRDs or plain file)
users:
  # Team write path: token maps to their tenant's insert URL. The team
  # cannot write anywhere else no matter what URL they configure.
  - bearer_token: "<payments-write-token>"
    url_prefix: "https://vminsert.observability.svc:8480/insert/12/prometheus/"
    max_concurrent_requests: 4

  # Team read path: scoped to their tenant's select URL; optionally with
  # enforced extra filters for sub-tenant restrictions.
  - bearer_token: "<payments-read-token>"
    url_prefix: "https://vmselect.observability.svc:8481/select/12/prometheus/"
    max_concurrent_requests: 6

  # Read-only, label-filtered access (single-node or intra-tenant scoping):
  - bearer_token: "<pricing-dashboards-token>"
    url_map:
      - src_paths: ["/api/v1/query.*", "/api/v1/series", "/api/v1/label.*"]
        url_prefix: "https://vm.observability.svc:8428/"
    # every query is rewritten to include this matcher — the team can only
    # ever see series carrying their label:
    extra_label_filters: ['{team="pricing"}']

  # Platform break-glass: cross-tenant read for incident response.
  - bearer_token: "<platform-oncall-token>"
    url_prefix: "https://vmselect.observability.svc:8481/select/multitenant/prometheus/"
```

Key fields in plain language: `url_prefix` is the routing decision — the credential *is* the tenant; `extra_label_filters` appends a matcher to every query server-side, which is how you grant "read your own data only" without trusting the client; `max_concurrent_requests` is per-credential noisy-neighbor damping.

With the operator, the same policy is expressed as `VMUser` objects, which platform can even let teams *propose* via MR to a policy repo — the review step is your change-control gate.

## The Standard Access Tiers

- **Team write** — bearer token, insert-only, own tenant. Lives in the team's cluster secrets; injected into vmagent/Fluent Bit by platform templates.
- **Team read** — bearer token or SSO-fronted Grafana datasource, select-only, own tenant (or enforced label filter). This is what dashboards and ad-hoc queries use.
- **Platform read (global)** — multitenant select path, short-lived credentials if you can (Vault dynamic secrets work nicely here), used for troubleshooting and meta-dashboards.
- **Platform admin** — direct access to component admin endpoints (`/snapshot/*`, force-merge, flags pages). Network-restricted to the observability namespace/jump hosts; never exposed through vmauth at all.

Same model for VL: write tokens pin `AccountID` headers on ingest; read tokens pin them on query; LogsQL `extra_filters` scoping is available where finer-than-tenant read restriction is needed.

## Self-Service Within Guardrails

The goal state: a product team never files a ticket for day-to-day observability work.

- **Alerts and recording rules**: teams commit `VMRule` objects to their own repos/namespaces; the operator aggregates them into vmalert. Platform's control is a CI lint (syntax, naming standards, mandatory `team` label, query-cost sanity) plus a meta-alert on rule-evaluation failures.
- **Ingestion config**: teams edit only their template-derived fragments (scrape objects, Fluent Bit filters). The transport (agents, endpoints, credentials) is platform-injected and not team-editable.
- **Queries**: unrestricted within their tenant, bounded by the vmauth concurrency and vmselect cost flags. Teach `query stats` / slow-query logs early so teams can self-diagnose expensive queries before platform throttles them.
- **The one ticketed operation**: new tenant creation and limit changes. Keep it a 1-line MR to the vmauth policy repo with platform approval — self-service in mechanics, controlled in authority.

\newpage

# Observability Workflows Across Multiple Teams

## Workflow 1 — Platform-Level Alarm: Ingestion Lag / Disk Pressure

Meta-monitoring (the small, separate VM instance watching the big one) fires `VMStorageDiskSpaceLow` or remote-write lag alerts. This page goes to *platform* on-call, never to product teams.

Step-by-step triage, in the order that finds the cause fastest:

1. **Is it supply or demand?** Check ingest rate per tenant (multitenant query on `vm_rows_inserted_total` / `vlinsert` equivalents). A step change in one tenant = demand problem (their deploy started emitting garbage); flat ingest with rising lag = supply problem (a slow/dying storage node, disk saturation, compaction backlog).
2. **Demand problem path:** identify the tenant → check their new-series creation rate (`vm_new_timeseries_created_total`) and top label values (cardinality explorer / `/api/v1/status/tsdb`) → apply the pre-agreed guardrail: rate-limit the tenant at vmauth, or push a relabel drop rule on their vmagent for the offending metric. **Then** message the team with evidence. The contract (§4) means this is a defined procedure, not a negotiation during an incident.
3. **Supply problem path:** per-node checks — disk latency and utilization, `vm_slow_row_inserts_total` (RAM pressure indicator: series index not fitting in memory), merge/compaction backlog. Short-term: add disk or a storage node (vminsert reroutes automatically; expect temporarily reduced query completeness for recent data on the new topology). Long-term: revisit the capacity model.
4. **Communicate blast radius precisely.** "Writes are buffered at edge agents; no data loss expected; queries over the last 20 minutes may be incomplete" — the edge-buffering design (§3) is what lets you say this calmly.

Notice the roles: platform diagnoses and stabilizes the *service*; the offending team fixes the *content*; the limits and contracts negotiated in peacetime are what keep the incident short.

## Workflow 2 — Product Team Debugs an SLO Violation

A latency SLO burn-rate alert fires for a trading-adjacent service, `execution-gw`. The team's on-call (not platform) drives; every step is self-service within their tenant.

1. **Scope with metrics.** `histogram_quantile(0.99, sum by (le, version, pod) (rate(execution_gw_request_duration_seconds_bucket[5m])))` — is the spike global, per-version (bad deploy), or per-pod (bad node/neighbor)?
2. **Correlate with the standard labels.** Because `service`, `version`, `namespace`, `cluster` are guaranteed (§4), the team pivots: CPU throttling (`container_cpu_cfs_throttled_periods_total`), GC time from runtime metrics, connection-pool saturation — all joined on the same labels.
3. **Cross to logs with the same identity.** In VL: `_time:30m {namespace="execution", service="execution-gw"} (error OR timeout) | stats by (upstream) count()` — the shared label taxonomy makes this a mechanical translation of the metrics query, not a new investigation. Suppose logs show timeouts concentrated on one upstream: the team now pages *that* upstream's owners with evidence, or rolls back if step 1 implicated a version.
4. **Platform's only involvement:** none, unless the team's queries hit cost limits or the data itself looks wrong — at which point platform's global view (multitenant read) lets them confirm whether the anomaly is in the data or in the pipeline.

The example generalizes: metrics answer *where and how much*; logs answer *what exactly*; shared labels are the join key; tenancy means each team moves fast inside its own lane.

## Workflow 3 — One Tenant's Data Looks Anomalous

Symptom: platform's per-tenant capacity dashboard shows tenant `pricing` at 6x normal log volume and rising series churn; every other tenant is flat. This shape — one tenant anomalous, the rest healthy — is diagnostic gold: it almost always means *that tenant shipped something*, not that the platform broke.

1. Platform confirms isolation is holding: other tenants' ingest, query latency, and error rates are unaffected (this is the per-tenant limits doing their job — the incident is contained before anyone is paged urgently).
2. Platform identifies the change: VL query `_time:2h {tenant="pricing"} | stats by (service, level) count()` typically fingers one service logging debug-level output, or a crash-loop spamming stack traces.
3. Handoff with evidence: "your `quote-cache` service went from 200 to 40k lines/sec at 14:32, correlating with deploy X; you're at 80% of your ingest quota." The team rolls back or fixes the log level; platform tightens nothing, because the guardrails already worked.
4. If limits were *not* in place and the tenant degraded shared storage — that is a platform failure, not a team failure. The retro action item is quotas, not blame.

\newpage

# Architecture Variants & Evolution Over Time

## Stage 1 — Small Org: Single-Node Everything

**Scale:** ≤ ~50 engineers, a handful of services, one or two clusters; ≤ ~1–2M active series; ≤ ~100 GB/day logs.

- One `VMSingle` (HA pair at most, dedup enabled) + one single-node VL. One vmagent per cluster, one Fluent Bit DaemonSet, one Grafana, one vmalert.
- Tenancy: a `team` label and nothing more. Auth: one vmauth with a write token and a read token per team, `extra_label_filters` if needed.
- Platform "team" is often one or two people; the paved-road docs still pay for themselves immediately.

**Pain that triggers evolution:** RAM pressure from series churn on the single node; the first noisy-neighbor incident; retention demands exceeding one machine's disk; the first "we can't have staging load-tests polluting prod metrics" complaint (→ split environments first, before anything else).

## Stage 2 — Growing: Cluster Mode, Real Tenancy, Real Auth

**Scale:** 50–500 engineers, 5–15 teams, several clusters; 5–30M active series; 0.5–3 TB/day logs.

- `VMCluster` (RF=2) per environment; VL moves to cluster mode or a small fleet of sharded single-nodes; vmagent per cluster with disk buffering sized deliberately.
- URL/header tenancy per team; vmauth policy repo becomes the tenant registry; per-tenant rate limits and query circuit breakers turned on *before* they're needed.
- The operator becomes load-bearing: teams self-serve `VMServiceScrape`/`VMRule`; CI linting enforces standards; onboarding is a checklist, not a meeting.
- Meta-monitoring is now non-negotiable and physically separate.

**Pain that triggers evolution:** per-tenant SLA asks ("trading needs 13-month retention, batch analytics needs 1"), chargeback/showback pressure, audit requirements on who can read what, and query load from ad-hoc analytics competing with on-call dashboards.

## Stage 3 — Mature: Observability as a Product

**Scale:** 500+ engineers, dozens of tenants, multi-region; 50M+ active series; 5+ TB/day logs.

- Formal service tiers: retention, ingest quota, query concurrency, and support SLA per tenant class — written down as a contract, priced via showback.
- Read/write separation hardened: dedicated vmselect pools for interactive vs. batch/analytics queries (same data, different circuit-breaker profiles), so a research notebook can never brown-out on-call dashboards.
- Possibly per-region clusters with global-view federation at the Grafana/vmauth layer; possibly a dedicated cluster for the one tenant whose requirements genuinely differ (compliance retention, extreme cardinality).
- Platform ships SDKs/config generators rather than templates; enablement is a rotating internal-education duty; deprecations follow a published policy.

**Pain at this stage** is organizational, not technical: keeping the label taxonomy coherent across dozens of teams, funding the platform team as headcount grows sublinearly to tenants, and resisting the gravitational pull toward "just give every team their own cluster" (which trades one platform team's clarity for N teams' fractional, inconsistent ops burden).

\newpage

# Best Practices & Anti-Patterns for Platform/SRE Teams

## Best Practices

**Data standards**

- One label taxonomy across metrics *and* logs; inject the infrastructure labels (`cluster`, `env`, `namespace`, `team`) mechanically so they cannot be wrong.
- Metric naming standard with units in the name (`_seconds`, `_bytes`, `_total`); lint it in CI.
- Cardinality budgets per tenant, monitored continuously (`vm_new_timeseries_created_total`, TSDB status top-lists), alerting on *churn rate* (new series/sec) — churn, not total series count, is what kills VM performance, because every new series costs index writes and RAM.
- In VL, platform owns the `_stream_fields` choice. Stream fields should be low-cardinality, long-lived identities (`namespace`, `service`, `container`). Never `pod_name` alone in high-churn environments, never request-scoped IDs.

**Separation and safety**

- Meta-monitoring on separate infrastructure, always. The monitoring system must be able to report its own death.
- Prod and nonprod on separate clusters — the cheapest strong isolation you will ever buy.
- Query circuit breakers (`search.maxUniqueTimeseries`, `maxQueryDuration`, `maxConcurrentRequests`, vmauth per-user concurrency) configured from day one.
- Backups (`vmbackup` to object storage) tested by *restoring*, on a schedule. Replication factor is not backup; snapshots you've never restored are hope, not backups.
- Edge buffering sized consciously: `remoteWrite.maxDiskUsagePerURL` bounds outage backlog; know your drain time after a 1-hour central outage (backlog drains at spare bandwidth, not instantly — a 2x-provisioned link drains 1 hour of backlog in roughly 1 hour).

**Operating discipline**

- Capacity model that survives N-1 storage nodes at peak ingest (rerouting cascades are VM cluster mode's characteristic failure).
- Per-tenant everything on the platform dashboard: ingest rate, series count, churn, log volume, query cost, quota headroom. You cannot have a calm noisy-neighbor conversation without this chart.
- Upgrade discipline: components are backward-compatible within documented windows; upgrade vmselect → vmstorage → vminsert (reads first), one node at a time, watching reroute load.
- Retention changes are near-irreversible (deleted data is gone); gate them behind MR review like any destructive migration.

## Anti-Patterns

- **No label standards.** Every team invents `env` vs `environment` vs `stage`; cross-service queries become archaeology; the platform team becomes a human query-translation service.
- **No per-tenant limits.** The first cardinality explosion takes the shared cluster down for everyone, and the retro concludes "shared platforms don't work" when the actual failure was missing quotas.
- **Unclear alert ownership.** Rules without a `team` label route to a shared channel that everyone mutes. Every alert must page a team that can act on it; platform owns only meta-alerts.
- **High-cardinality labels as a debugging habit.** User IDs, order IDs, full URLs as label values. The fix is cultural + mechanical: docs say "IDs go in logs/traces, not metric labels," and `maxLabelsPerTimeseries`/cardinality alerts enforce it.
- **Letting teams query storage components directly.** Bypassing vmauth means no limits, no audit, no ability to reshape the backend. Everything goes through the proxy, the way everything goes through the VIP.
- **Self-hosted meta-monitoring.** The VM cluster monitoring itself will report "all healthy" right up until it can't report anything at all.
- **Treating logs as free.** Debug-level logging in prod, stack traces per request, no volume quotas — VL is cheap per GB, which quietly enables 10x the GBs. Quota it like metrics.
- **Per-team clusters as the default.** Strong isolation has its place (§5), but defaulting to it converts one well-run service into N poorly-run ones and forfeits the entire point of a platform team.

\newpage

# Production Checklist

## Technical Readiness

- [ ] Deployment mode chosen deliberately (single-node vs cluster) with a written trigger for revisiting
- [ ] `replicationFactor` ≥ 2 in cluster mode; dedup interval set consistently on select **and** storage
- [ ] Retention set per environment; change process gated (destructive)
- [ ] Storage headroom ≥ 30%; disk-latency and free-space alerts on every storage node
- [ ] Capacity survives loss of one vmstorage/vlstorage node at peak ingest
- [ ] vmbackup/vlogs backups to object storage; restore rehearsed and timed
- [ ] vmauth in front of all read/write paths; no direct component access from tenant networks
- [ ] Query circuit breakers configured (max unique series, duration, concurrency; per-user limits)
- [ ] Ingest guardrails configured (`maxLabelsPerTimeseries`, per-tenant rate limits)
- [ ] Edge agents (vmagent/collectors) HA where needed, with bounded disk buffers; drain time after 1h outage known
- [ ] Meta-monitoring on separate infrastructure, paging platform on-call
- [ ] TLS everywhere tenants cross a network boundary; tokens in secret stores, not in Git
- [ ] Upgrade runbook (order, pace, rollback) written and followed once in nonprod

## Organizational Readiness

- [ ] One-page contract published: what platform guarantees, what tenants own, escalation both ways
- [ ] Label taxonomy documented; infrastructure labels injected mechanically; CI lint live in the shared pipeline
- [ ] "Getting started" doc with copy-paste templates (scrape, rules, log shipping); time-to-first-metric measured
- [ ] Onboarding checklist (both sides) in use; enablement session recorded
- [ ] Alert-ownership routing by `team` label; no unowned alerts
- [ ] Golden dashboards published; per-tenant capacity dashboard live
- [ ] Office hours / support channel with a stated response expectation

## Multi-Tenant Strategy

- [ ] "Tenant" defined (team / product / env / customer) and applied identically to metrics and logs
- [ ] Tenant registry = vmauth policy repo in Git, MR-gated
- [ ] Per-tenant quotas: ingest rate, series/cardinality budget, log volume, query concurrency
- [ ] Per-tenant retention needs mapped to architecture (shared vs dedicated) with rationale written down
- [ ] Break-glass cross-tenant read path exists, is audited, and is boring to use
- [ ] Noisy-neighbor runbook: detect (per-tenant dashboard) → contain (limits) → notify (evidence) → retro (quota gaps)
- [ ] Escalation paths and SLAs per tenant tier published
- [ ] Showback (at minimum) per tenant, so growth conversations are about numbers, not feelings

---

*Use this booklet as the skeleton for your own platform's runbook: every section that says "platform-owned" should eventually point at a repo, and every checklist line at a dashboard or a document. When that's true, the platform is real.*
