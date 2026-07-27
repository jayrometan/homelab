---
title: "StackGres on the PaaS Stack — Volume 1: Architecture & Concepts"
subtitle: "Patroni from first principles, the StackGres operator, CRDs, pooling, Cilium BGP exposure, backups, Weka storage, and security"
author: "Platform Engineering Reference Series"
date: "July 2026"
toc: true
toc-depth: 2
numbersections: true
geometry: margin=2.2cm
fontsize: 10pt
---

# How to Read This Volume

This volume is the conceptual half of a two-volume set. It builds the mental model: what Patroni actually is, what StackGres adds on top of it, and how the whole thing behaves on a Cilium BGP, no-encapsulation, Weka-backed Kubernetes platform. Volume 2 is the standalone operations playbook — self-service workflow, runbooks, upgrade procedure, and incident walkthroughs — written to be pulled up mid-incident.

A convention used throughout, because it matters constantly when debugging: every behavior is tagged as either **[Patroni]** — vanilla Patroni behavior you would see in any Patroni deployment, StackGres or not — or **[StackGres]** — something the operator adds, wraps, or changes. When something breaks at 3am, the single most valuable piece of knowledge is which layer owns the behavior you are staring at, because it determines whether the answer is in `patronictl`, in the operator logs, or in a CRD.

Version note: this material is written against the StackGres 1.18 line (Patroni 4 underneath, supported since StackGres 1.15). Where behavior changed meaningfully across recent versions — the Envoy sidecar, SGDistributedLogs, the restart/rollout machinery — it is called out, because the deployed version at the firm is one of the first things to confirm.

> **PROD:** This is a stateful layer with real blast radius. Almost everything else on the platform can be deleted and re-reconciled by Flux. A Postgres cluster cannot. Internalize the asymmetry before touching anything: GitOps makes stateless mistakes cheap and stateful mistakes exactly as expensive as they have always been.

# Patroni From First Principles

## The problem Patroni solves

Start from what Postgres gives you natively, because the gap is the whole point. Postgres has excellent built-in *replication*: a primary streams its write-ahead log (WAL) to replicas, which replay it. What Postgres does **not** have built in is *cluster membership and failover orchestration*. Nothing in core Postgres answers: who is the primary right now? If the primary dies, which replica should take over, who decides, and how do we guarantee two nodes never both believe they are the primary? Postgres replication is a data plane with no control plane.

Patroni is that control plane. It is a Python daemon that runs *next to* every Postgres instance — one Patroni per Postgres — and takes over the lifecycle of that instance entirely. Patroni starts Postgres, stops it, promotes it, demotes it, rewrites its configuration, and continuously reports and reads cluster state. You stop thinking of "a Postgres server" and start thinking of "a Patroni member that happens to run Postgres as its payload."

If you want an anchor from your existing stack: Patroni is to Postgres roughly what a Consul agent + a leader-election lock is to a service that needs exactly one active instance. You already know the shape of this problem from Consul leader election — sessions, locks, TTLs, and the rule that whoever holds the lock is the leader and everyone else watches. Patroni is that pattern, purpose-built for Postgres, with the promotion/demotion mechanics baked in.

## The DCS and leader election

Patroni's correctness rests entirely on a **DCS — distributed configuration store**. This is any strongly-consistent store supporting atomic compare-and-swap and TTLs: etcd, Consul, ZooKeeper, or — critically for us — **the Kubernetes API itself**. The DCS holds a small set of keys per cluster, and the one that matters most is the **leader key**.

The protocol is simple and worth knowing cold, because every failover behavior derives from it:

1. The leader key holds the name of the current primary and carries a TTL (default 30s).
2. The current primary's Patroni refreshes ("touches") the leader key every `loop_wait` seconds (default 10s), each refresh being an atomic update that succeeds only if the key still names *this* member.
3. Every replica's Patroni watches the key. As long as it exists and is fresh, replicas do nothing but replicate.
4. If the primary fails to refresh the key for TTL seconds — process dead, node dead, network partitioned from the DCS — the key expires. Now, and only now, an election happens: each healthy replica checks its own WAL position, checks it can see the other members, and attempts an atomic create of the leader key. Exactly one create succeeds, because the DCS is consistent. The winner promotes its Postgres (`pg_promote`); the losers re-point their replication at the new primary.

The inverse rule is the split-brain guard and the most important sentence in this chapter: **[Patroni]** *a primary that cannot reach the DCS demotes itself.* If the primary's Patroni cannot refresh the leader key — even though its Postgres is perfectly healthy and clients can reach it — it must assume the key will expire and someone else will be elected. So it stops Postgres or demotes it to read-only within the TTL window. This is exactly the Consul lock-holder discipline: losing your session means you must stop acting as leader *before* anyone else can be elected, and the TTL math guarantees the old leader steps down before a new one can step up. Two writable primaries — the split-brain that corrupts data irrecoverably via divergent WAL timelines — is prevented by time-based fencing, not by any network mechanism.

Three tunables govern this dance and appear constantly in incident analysis: `ttl` (30s), `loop_wait` (10s), and `retry_timeout` (10s). The practical consequence: **[Patroni]** worst-case unplanned failover is roughly TTL + election + promotion — think 30–60 seconds of write unavailability with defaults. You can shrink TTL to fail over faster, but every second you remove increases the odds that a transient DCS slowdown (an apiserver hiccup, on our platform) triggers a *spurious* demotion of a healthy primary. This is the same tradeoff as aggressive Consul session TTLs, and the same advice applies: tune it down only with evidence, never speculatively.

**Kubernetes as the DCS.** On Kubernetes, Patroni does not need etcd or Consul as a separate dependency — it uses the Kubernetes API as its DCS, storing state in annotations on Endpoints (or ConfigMaps) and using the apiserver's optimistic concurrency (resourceVersion compare-and-swap) as its atomic primitive. **[StackGres]** StackGres always deploys Patroni in this mode. Two consequences you must internalize on this platform:

- The **kube-apiserver (and therefore the platform etcd behind it) is in the availability path of every Postgres failover**. An apiserver outage doesn't take down running primaries' data plane, but a *sufficiently long* apiserver outage means primaries cannot refresh their leader key and will self-demote. Postgres availability is now coupled to control-plane availability. This is the single most important architectural fact for an HFT reliability review, and it is why etcd ops is on your Tier 3 list — it deserves promotion in your mental model.
- Patroni traffic to the DCS is HTTPS to the apiserver. Any CiliumNetworkPolicy applied to database namespaces must allow `toEntities: kube-apiserver` or Patroni is instantly and catastrophically broken (see the networking chapter).

> **PROD:** Never firewall, throttle, or webhook-intercept database-namespace traffic to the apiserver without understanding this coupling. A misapplied network policy or a misbehaving admission webhook on Endpoints updates can demote every primary on the platform simultaneously. That is a platform-wide database outage caused by a "networking" or "security" change.

## Failover vs switchover

The words are precise and the distinction drives real operational decisions:

- **Failover** is *unplanned*: the leader key expired because the primary genuinely failed. Election happens, a replica promotes, and — this is the part people forget — any transactions committed on the old primary but not yet received by the promoted replica are **lost** under asynchronous replication. The cluster's WAL history forks: the new primary starts a new *timeline*, and the old primary, if it comes back, is on a divergent timeline and cannot simply rejoin.
- **Switchover** is *planned and coordinated*: an operator (human or SGDbOps) tells Patroni "move the primary to member X." Patroni checkpoints the primary, cleanly shuts it down *after* confirming the target replica has received all WAL, then promotes the target. **Zero data loss by construction**, and the old primary rejoins as a replica automatically. Downtime is a few seconds of connection disruption rather than a TTL-length outage.

The judgment call you will face: a primary is sick but not dead — degraded I/O, memory pressure, slow queries. Do you wait for Patroni to failover, or do you trigger a switchover? Almost always switchover, because it is the lossless path and it works while the primary is still limping. Failover is what happens when you no longer have the choice. Volume 2's runbooks return to this.

**Rejoining after failover: pg_rewind.** The old primary's timeline diverged the moment the new primary was promoted. To rejoin, its data directory must be rewound to the fork point. **[Patroni]** Patroni automates this with `pg_rewind` when enabled (`use_pg_rewind: true`, which **[StackGres]** StackGres sets by default). pg_rewind copies back only changed blocks — fast. When it fails (it sometimes does, e.g. required WAL already recycled), the fallback is a full **reinitialization**: wipe the member's data directory and re-clone from the current primary (`patronictl reinit`). On multi-TB clusters over Weka, know your reclone time *before* you need it.

## Replication: async, sync, and quorum

**[Patroni]** Underneath, this is stock Postgres streaming replication: the primary ships WAL records over a replication connection; replicas apply them continuously. Patroni's contribution is *managing* it — creating replication slots, setting `primary_conninfo` on replicas, repointing everyone after topology changes.

**Asynchronous (default):** the primary commits and acknowledges the client *without waiting* for any replica. Fastest commits; the cost is the data-loss window on failover: whatever WAL hadn't reached the promoted replica is gone. Patroni bounds the blast radius with `maximum_lag_on_failover` (default 1MB): a replica lagging beyond this is ineligible for promotion, so you lose at most that much WAL rather than promoting a badly stale replica.

**Synchronous:** the primary waits for at least one designated standby to confirm receipt (`synchronous_standby_names`, confirmation level per `synchronous_commit` — `on` waits for flush to the standby's disk). Zero data loss on failover of a sync standby; the cost is commit latency now includes a network round-trip and the standby's fsync, and — the sharp edge — with `synchronous_mode_strict`, **if all sync standbys die, the primary blocks all writes** rather than silently degrading to async. Plain `synchronous_mode: true` in Patroni degrades gracefully (drops to async when no standby is available) at the cost of reopening the loss window exactly when things are already going wrong.

**Quorum-based (Patroni 4, so StackGres ≥ 1.15):** instead of naming specific sync standbys, require acknowledgment from *any k of n* replicas (Postgres `ANY k (...)` syntax). This decouples durability from the identity of individual replicas — one slow or dead replica no longer stalls commits as long as the quorum holds — and Patroni 4 can use quorum state during failover to identify which replicas are guaranteed to have every committed transaction. For a trading platform this is usually the right shape when sync replication is required at all: durability without a single-standby latency hostage.

The HFT framing: this is a **durability-vs-latency dial**, and it is per-cluster, which means it is a *product decision the platform exposes to tenant teams*, not a global default. A cluster storing research metadata wants async. A cluster recording anything where "we lost the last 200ms of commits" is unacceptable wants quorum sync and accepts the commit latency. Expect this to be one of the knobs the PaaS deliberately surfaces (or deliberately doesn't) in the CUE abstraction.

## WAL: streaming, archiving, and why backups care

Two WAL transport mechanisms coexist and serve different purposes; conflating them causes real confusion:

1. **Streaming replication** — the live TCP connection described above. Serves *replicas*, in real time.
2. **WAL archiving** — `archive_command` runs on every WAL segment (16MB) as it completes, copying it to durable external storage. Serves *backups and PITR*. A base backup plus a continuous archive of every subsequent WAL segment lets you reconstruct the database as of any moment: restore the base, replay archived WAL to the target timestamp. This is the entire theory of point-in-time recovery, and it is what WAL-G does for StackGres (chapter on backups below).

Also relevant: **replication slots**. A slot makes the primary retain WAL that a specific replica hasn't consumed yet, so a briefly-disconnected replica can resume without a full reclone. The dark side: a replica that goes away *forever* while its slot remains pins WAL retention and **fills the primary's disk**. Patroni manages slots for its own members (creating and cleaning them as membership changes), which removes most of this risk — but any *manually* created slot (a CDC consumer, a Debezium/SGStream pipeline someone added) is yours to watch. Disk-full-from-orphaned-slot is a classic Postgres incident and it appears in Volume 2's runbooks.

## What Patroni looks like when you touch it

Every Patroni exposes a REST API on **port 8008** — health checks (`/primary`, `/replica`, `/health`), metrics, and the control surface `patronictl` talks to. The commands to know now, before the tooling chapters:

```bash
patronictl list          # topology: members, roles, state, timeline, lag
patronictl switchover    # planned primary move
patronictl restart       # controlled restart of members
patronictl reinit        # wipe & reclone a broken member
patronictl history       # failover/timeline history
patronictl show-config   # the DCS-level dynamic configuration
patronictl edit-config   # edit it (StackGres: don't — see below)
```

**[Patroni]** Configuration is layered: local YAML < DCS dynamic config < environment overrides, with some Postgres parameters (`max_connections`, `max_wal_senders`, etc.) forced through the DCS layer so they stay consistent cluster-wide. **[StackGres]** StackGres *owns* the local YAML and the DCS config, generating both from CRDs. `patronictl edit-config` on a StackGres cluster is fighting the reconciler — your edit works until the operator reconciles it away. All configuration goes through `SGPostgresConfig`/`SGCluster`; `patronictl` is for *state operations* (list, switchover, reinit), not configuration. This split — state via patronictl, config via CRDs — is the operating discipline for the whole platform.

# The StackGres Operator

## What StackGres actually is

Hand-rolling Patroni on Kubernetes is entirely possible — Zalando's Spilo images, a StatefulSet, careful RBAC — and understanding that hand-rolled shape is the best way to see what StackGres adds. A hand-rolled deployment gives you Patroni's HA core and nothing else: no pooling, no backups, no metrics, no admission-time config validation, no day-2 operations. Every one of those becomes a bespoke sidecar-and-cronjob project.

StackGres is an opinionated packaging of the *entire* stack: Patroni for HA, **PgBouncer** for pooling, **WAL-G** for backups/PITR, **postgres_exporter** for metrics, log shipping, plus a **Java (Quarkus) operator** that turns a family of CRDs into all of the above, a REST API, and a web console. The honest framing: **[StackGres]** StackGres does not replace or modify Patroni's HA logic *at all*. Leader election, failover, replication management — every word of the previous chapter is stock Patroni running inside StackGres pods. StackGres generates Patroni's configuration, wraps it in a production-grade pod, and adds the operational machinery around it. When you debug HA behavior, you are debugging Patroni; when you debug provisioning, config application, backups, or day-2 ops, you are debugging StackGres.

## Operator architecture and reconciliation

The moving parts:

- **Operator deployment** (`stackgres-operator`, in the operator namespace): watches StackGres CRDs, generates and reconciles Kubernetes resources (StatefulSets, Services, Secrets, ConfigMaps, Roles), and hosts the **admission webhooks**. Since 1.18 it also drives cluster rollouts directly rather than delegating restarts to Jobs.
- **REST API + web console** (`stackgres-restapi`): a UI/API layer over the CRDs. Useful for exploration; on a GitOps platform it should be read-only in practice, because anything mutated through the console is drift that Flux/KubeVela will fight. Expect the firm to either disable it or treat it as a dashboard.
- **Admission webhooks**: *validating* webhooks reject malformed resources at apply time — the flagship example being SGPostgresConfig validation of `postgresql.conf` parameters against the actual Postgres version (unknown parameter names and out-of-range values are rejected by the apiserver, not discovered at 2am). *Mutating* webhooks fill defaults, which is why a minimal SGCluster round-trips from the apiserver fully populated.
- **Per-pod local controller** (`cluster-controller` container): a StackGres agent inside each database pod that manages the pod-local reconciliation — rendering config files for Patroni/PgBouncer/exporters, handling managed-SQL execution, reloading sidecars. Since 1.18, a pod whose cluster-controller crashes keeps serving Postgres if it already bootstrapped — an availability-over-management tradeoff worth knowing during operator-outage incidents.

> **PROD:** Webhooks are an availability dependency for *CRD writes*, not for running databases. If the operator is down: existing clusters keep running and keep failing over (Patroni needs only the apiserver), but you cannot create/modify StackGres resources — applies fail with webhook connection errors — and no reconciliation happens. Operator-down is therefore a "management plane frozen" incident, not a data-plane incident. Know this cold; it changes incident severity classification.

## What gets generated for one SGCluster

Apply a three-instance SGCluster named `trades` and the operator produces, in the cluster's namespace:

- **StatefulSet `trades`** → pods `trades-0`, `trades-1`, `trades-2`, each with (per current versions): the **patroni** container (Patroni + Postgres — one container, Patroni is PID 1 and Postgres its child, exactly the Tini/PID1 supervision shape you know from container internals), **pgbouncer**, **prometheus-postgres-exporter**, **cluster-controller**, **fluent-bit** (log shipping), and optionally **postgres-util** (a psql-equipped toolbox container). Historic note to verify against the deployed version: older StackGres versions also injected an **Envoy** sidecar as the traffic entrypoint (with Postgres-protocol-aware filters and metrics); recent versions removed it and route Services to PgBouncer/Postgres directly. Confirm which port chain your version uses before you first trace a connection — it changes what `Service targetPort` points at.
- **Services**: `trades` (read-write, always resolving to the primary), `trades-replicas` (read-only, load-balanced across replicas), plus config/REST endpoints. The `trades` primary service is the interesting one: **[Patroni]** it is a Service *without a selector* whose Endpoints object Patroni itself rewrites on every leadership change. Failover re-points the service the instant the new leader writes the DCS — there is no controller in the loop, no readiness-probe propagation delay. This is materially better than label-selector-based primary services and is a detail worth repeating in design discussions.
- **Secret `trades`**: generated credentials — `superuser` (postgres), `replication`, and `authenticator` (used by PgBouncer's auth_query) passwords.
- **PVCs** via the StatefulSet's volumeClaimTemplate — one data volume per pod, from the storage class named in the SGCluster (Weka, for us).
- **ConfigMaps** carrying rendered Patroni/PgBouncer/exporter configuration, plus RBAC for Patroni's DCS access (Patroni needs get/update on Endpoints etc. in its namespace — this is the Role you'll find when auditing).

The vanilla-vs-StackGres tags for this chapter, condensed: pod lifecycle, leader election, the selectorless primary service endpoint dance — **[Patroni]**. The StatefulSet shape, sidecar set, credential Secrets, config rendering, webhooks, day-2 SGDbOps machinery — **[StackGres]**.

# The Core CRDs

The CRD family splits cleanly into *profile/config objects* (reusable, referenced by name) and *the cluster object* that composes them. This referential design is deliberate and platform-friendly: the PaaS team can own a small library of blessed profiles and configs, and tenant SGClusters simply point at them — the same ownership split you know from platform-owned Terraform modules with team-owned instantiations, and it maps directly onto closed CUE definitions later.

## SGInstanceProfile — sizing

```yaml
apiVersion: stackgres.io/v1
kind: SGInstanceProfile
metadata:
  name: size-m            # platform-owned: a blessed sizing tier
  namespace: trading-alpha
spec:
  cpu: "4"                # becomes both request and limit (Guaranteed QoS)
  memory: 16Gi
  # optional per-container/initContainer overrides exist for the sidecars
```

Requests equal limits by design: Guaranteed QoS class, so database pods are last in line for eviction and immune to CPU throttling surprises from overcommit. On an HFT platform this is exactly right — do not "optimize" it away. **PROD:** memory limits interact with Postgres memory config (`shared_buffers`, `work_mem × connections`): an SGPostgresConfig tuned for a bigger profile than the cluster actually uses is an OOMKill generator. The two objects are validated independently; *you* own their coherence.

## SGPostgresConfig — postgresql.conf

```yaml
apiVersion: stackgres.io/v1
kind: SGPostgresConfig
metadata:
  name: pg17-trading-base
  namespace: trading-alpha
spec:
  postgresVersion: "17"        # major version this config is validated against
  postgresql.conf:
    shared_buffers: "4GB"
    max_connections: "400"
    wal_compression: "on"
    checkpoint_timeout: "15min"
    max_wal_size: "8GB"
    random_page_cost: "1.1"     # flash-backed storage (Weka)
    log_min_duration_statement: "250ms"
```

The webhook validates parameter names and value ranges against `postgresVersion` — typos die at apply time. Two sharp edges now so they're not surprises later: (1) some parameters require a Postgres **restart** to take effect; StackGres applies what it can via reload and the rest sit *pending* until a restart — a cluster can run indefinitely with your "applied" config not actually in force (`patronictl list` shows a pending-restart flag; Volume 2 §15). (2) **[Patroni]** Patroni force-manages a small parameter set through the DCS (`max_connections`, `max_wal_senders`, `max_prepared_transactions`, ...) with its own consistency rules — changing `max_connections` in particular is restart-required and ordering-sensitive across members.

## SGPoolingConfig — pgbouncer.ini

```yaml
apiVersion: stackgres.io/v1
kind: SGPoolingConfig
metadata:
  name: pool-transaction-default
  namespace: trading-alpha
spec:
  pgBouncer:
    pgbouncer.ini:
      pgbouncer:
        pool_mode: transaction
        max_client_conn: "1000"
        default_pool_size: "40"
```

Full treatment in the pooling chapter; the CRD itself is a thin, validated wrapper over pgbouncer.ini.

## SGObjectStorage — backup target

```yaml
apiVersion: stackgres.io/v1beta1
kind: SGObjectStorage
metadata:
  name: backups-trading-alpha
  namespace: trading-alpha
spec:
  type: s3Compatible          # s3 | s3Compatible | gcs | azureBlob
  s3Compatible:
    bucket: pg-backups-trading-alpha
    endpoint: https://objectstore.internal:9000
    enablePathStyleAddressing: true
    awsCredentials:
      secretKeySelectors:
        accessKeyId:     { name: backup-creds, key: accessKeyId }
        secretAccessKey: { name: backup-creds, key: secretAccessKey }
```

Note the shape: credentials come from a *referenced Secret*, never inline. On-prem this is almost certainly an S3-compatible endpoint (MinIO or similar — possibly Weka's own S3 front, worth confirming day one).

## Backups: SGBackupConfig is dead, long live spec.configurations.backups

**SGBackupConfig is deprecated.** Modern StackGres configures backups inline in the SGCluster, referencing an SGObjectStorage:

```yaml
# fragment of SGCluster.spec
configurations:
  backups:
  - sgObjectStorage: backups-trading-alpha
    path: /pg/trades          # unique per cluster — see PROD note
    cronSchedule: "30 1 * * *"
    retention: 7              # count of full backups kept
    compression: lz4
    performance:
      uploadDiskConcurrency: 2
```

If the firm's manifests still use SGBackupConfig, that's a version signal and a migration item. **PROD:** the `path` inside the bucket must be unique per cluster *forever* — reusing a path from a deleted cluster interleaves two clusters' WAL histories in one archive and can silently poison PITR. Convention: embed namespace + cluster name + a generation marker in the path, and never recycle.

Individual backups materialize as **SGBackup** objects (one per base backup, holding status, size, LSNs, timestamps) — the thing you list to answer "when did this cluster last back up successfully."

## SGScript — managed SQL

```yaml
apiVersion: stackgres.io/v1
kind: SGScript
metadata:
  name: trades-bootstrap
  namespace: trading-alpha
spec:
  managedVersions: true
  continueOnError: false
  scripts:
  - name: create-app-role
    version: 1                 # bump to re-run a changed script
    script: |
      CREATE ROLE trades_app LOGIN PASSWORD NULL;
  - name: create-schema
    version: 1
    scriptFrom:
      secretKeyRef: { name: trades-bootstrap-sql, key: schema.sql }
```

SGScripts referenced from `SGCluster.spec.managedSql` run against the cluster in order, tracked by (name, version) in the cluster status. The semantics that bite: a script entry re-executes **only when its `version` increments**. Editing SQL without bumping the version does nothing; bumping it re-runs the whole entry, so scripts must be written idempotently (`CREATE ... IF NOT EXISTS`, `DO $$ ... $$` guards). This is the backbone of self-service database/role bootstrapping — Volume 2 §10 builds on it.

## SGDbOps — day-2 operations as resources

The most operationally interesting CRD: one-shot operations expressed declaratively.

```yaml
apiVersion: stackgres.io/v1
kind: SGDbOps
metadata:
  name: trades-minor-upgrade-20260801
  namespace: trading-alpha
spec:
  sgCluster: trades
  op: minorVersionUpgrade      # restart | minorVersionUpgrade | majorVersionUpgrade
                               # | securityUpgrade | repack | vacuum | benchmark
  runAt: "2026-08-01T02:00:00Z"        # optional: schedule for the window
  maxRetries: 1
  minorVersionUpgrade:
    postgresVersion: "17.6"
    method: InPlace            # InPlace | ReducedImpact
```

An SGDbOps is *consumed*: the operator executes it (historically via a Job; in 1.18 restarts/rollouts are operator-driven), records progress and outcome in `.status`, and the resource remains as an audit record. `ReducedImpact` temporarily adds an extra instance so read capacity never drops below n during the rolling restart; `InPlace` restarts within the existing member count. Major version upgrades get their own dedicated section in Volume 2 — they are the highest-blast-radius operation in the entire StackGres surface.

## SGCluster — the composition

Everything converges here. An annotated, realistic example:

```yaml
apiVersion: stackgres.io/v1
kind: SGCluster
metadata:
  name: trades
  namespace: trading-alpha
spec:
  postgres:
    version: "17.6"
    # ssl: { enabled: true, certificateSecretKeySelector: ..., privateKeySecretKeySelector: ... }
  instances: 3                       # 1 primary + 2 replicas
  sgInstanceProfile: size-m
  configurations:
    sgPostgresConfig: pg17-trading-base
    sgPoolingConfig: pool-transaction-default
    backups:
    - sgObjectStorage: backups-trading-alpha
      path: /pg/trading-alpha/trades/v1
      cronSchedule: "30 1 * * *"
      retention: 7
  pods:
    persistentVolume:
      size: 500Gi
      storageClass: weka-fs          # our CSI layer
    resources: { }                   # per-pod tweaks; prefer the profile
    # disableConnectionPooling: false
    # disableMetricsExporter: false
  replication:
    mode: sync-all-async-others      # illustrative; async | sync | strict-sync
                                     # + syncInstances for quorum shapes
  managedSql:
    scripts:
    - sgScript: trades-bootstrap
  prometheusAutobind: true           # wire ServiceMonitor/PodMonitor discovery
  nonProductionOptions: {}           # empty == production defaults enforced
```

`replication.mode` is where the async/sync/quorum discussion from the Patroni chapter becomes one field — this is precisely the knob a platform CUE definition either exposes as a tenant tier or pins. `nonProductionOptions` is a list of safety features you can disable (cluster pod anti-affinity being the famous one — see the Weka chapter); in production it should be empty, and its emptiness is worth enforcing in CUE.

## Awareness level: SGShardedCluster and SGDistributedLogs

**SGShardedCluster** provisions a Citus-based sharded topology — a coordinator SGCluster plus N shard SGClusters, with `SGShardedDbOps` for day-2. Know it exists; do not reach for it until a single vertically-scaled cluster with read replicas has actually run out of road, because it multiplies every operational concern in this booklet by the shard count.

**SGDistributedLogs** is StackGres's optional centralized store for *database* logs (Postgres + Patroni logs shipped to a dedicated Postgres with timescaledb, queryable via the console). Since 1.15 it is itself backed by a generated SGCluster, so it upgrades/restarts via SGDbOps like everything else. On this platform it competes directly with the existing Fluent Bit → VictoriaLogs pipeline; Volume 2 §12 argues you probably want exactly one of these paths, and it's probably the platform-standard one.

# Connection Pooling: PgBouncer

## Why a pooler is not optional

Postgres spawns a full OS process per connection. Hundreds of idle connections burn memory (`work_mem` is *per sort/hash per query*, but each backend has real fixed overhead) and connection *establishment* is expensive (fork + auth + catalog warmup). Microservice-era workloads — many pods, each with its own app-level pool of 10–20 — trivially produce thousands of client connections against a database that is happiest with low hundreds of backends. PgBouncer sits between, multiplexing many client connections onto few server connections.

The HAProxy analogy is genuinely load-bearing here, with one crucial twist. Like HAProxy in TCP mode, PgBouncer terminates client connections and maintains a backend pool. Unlike HAProxy, it speaks the Postgres wire protocol and multiplexes at *protocol granularity*: with `pool_mode: transaction`, a server connection is assigned to a client only for the duration of one transaction, then returned to the pool. That is like HAProxy reusing a backend keep-alive connection across requests — except the "request" is a transaction, and the protocol is stateful enough that this reuse has semantic consequences, which is where the sharp edges live.

**Pool modes:** `session` (server connection held for the client's whole session — safe, weak multiplexing), `transaction` (per-transaction — the useful one, and StackGres's practical default), `statement` (per-statement — breaks multi-statement transactions; ignore). **The transaction-mode contract:** any session-scoped state does not survive between transactions, because the next transaction may run on a different server connection. Concretely broken: `SET` (non-local), session advisory locks, `LISTEN/NOTIFY`, named prepared statements at the protocol level (modern PgBouncer ≥ 1.21 tracks and replays these — `max_prepared_statements` — which removed the single biggest historical pain, notably for JDBC; verify it's enabled in the platform's SGPoolingConfig). Tenant teams *will* hit this contract; it belongs in the platform's self-service docs on day one.

**[StackGres]** PgBouncer runs as a **sidecar in every database pod**, fronting its local Postgres — not as a separate pooler tier. The Services target the pooler port, so the standard client path is client → primary Service → primary pod's PgBouncer → local Postgres. Consequences: the pooler scales with the cluster and shares its fate (no separate pooler deployment to operate — a genuine simplification), pooler→Postgres latency is loopback-negligible, but pool configuration is *per-pod* — `default_pool_size` etc. apply per instance, and *all* write-path pooling funnels through the single primary pod's PgBouncer. Its `max_client_conn` is a platform-wide-visible ceiling per cluster. Auth uses the `authenticator` role and `auth_query` — PgBouncer verifies client credentials against Postgres itself, so database users work transparently without maintaining a separate userlist.

## What a failover looks like from the pooler's perspective

This is the question that matters for every client team, so walk it precisely. Steady state: clients hold TCP connections to the primary pod's PgBouncer; PgBouncer holds a smaller set of connections to the local Postgres.

**Unplanned failover of the primary pod:**

1. The primary pod dies. Every client TCP connection into its PgBouncer dies with it — pooler and database share the pod. In-flight transactions are gone; under async replication, recently *committed* transactions may be gone too (the Patroni chapter's loss window).
2. For up to TTL (~30s), the leader key hasn't expired: the `trades` Service still points at the dead pod. New connection attempts fail fast (Cilium's eBPF datapath will RST — no backend) or hang, depending on failure shape.
3. Patroni elects and promotes a replica; the new leader rewrites the `trades` Endpoints. From this instant, new connections land on the *new* primary pod's PgBouncer — which was already running, warm, with its own pool — and succeed.
4. Client recovery is therefore entirely a *client-side* property: applications must treat connection death as normal, reconnect with backoff, and retry idempotently. The platform's honest SLO statement to tenants: **writes pause for roughly TTL + promotion (tens of seconds) on unplanned failover; connections are severed, not migrated.** No pooler configuration changes that. Anyone who claims transparent failover is selling something.

**Planned switchover** is far gentler: Patroni demotes cleanly and promotes the target with WAL fully caught up; the endpoint flips in seconds; connections to the old primary are terminated but there is no election window and no data loss. This asymmetry — seconds and lossless vs tens-of-seconds and lossy — is *the* argument for doing maintenance via switchover instead of letting failover "handle it," and it recurs throughout Volume 2.

One more subtlety worth having ready for architecture discussions: because the read Service (`trades-replicas`) spreads across replica pods' poolers, replica pod restarts during rolling operations sever only the clients on that pod — this is what SGDbOps `ReducedImpact` is protecting.

# Networking and Exposure on Cilium BGP

## Where the IPs come from

Ground truth first, because "Cilium BGP mode, no encapsulation" changes the usual assumptions. In native-routing mode, pod IPs are *real* routed addresses: each node owns a PodCIDR, Cilium's BGP control plane advertises those CIDRs to the top-of-rack peers, and the fabric routes to pods directly — no VXLAN, no NAT inside the fabric. Services are a separate layer: ClusterIPs are virtual addresses realized entirely by Cilium's eBPF kube-proxy replacement, which translates ClusterIP→backend-pod-IP at the *source node's* socket/veth layer. A ClusterIP never appears as a packet on the wire in socket-LB mode — the connection is rewritten to the pod IP before it leaves the node.

Now apply that to the generated Services for cluster `trades`:

- `trades` (primary, selectorless, Patroni-managed Endpoints) — ClusterIP.
- `trades-replicas` (read, selector-based) — ClusterIP.

**Default posture: ClusterIP-only, in-cluster access only.** For a database serving workloads that all live on the same Kubernetes platform, this is the correct and probably intended design: no BGP advertisement of anything database-related, no Gateway API involvement (Gateway API on Cilium is an HTTP/TLS/TCP *ingress* construct — putting the Postgres wire protocol through it buys nothing over a ClusterIP and adds a hop and a failure domain). Databases are not "exposed" at all in the ingress sense; they are reachable, in-cluster, subject to network policy.

**Failover routability in this posture is a non-event at the routing layer.** When Patroni rewrites the `trades` Endpoints, Cilium agents on every node observe the change and update their eBPF service maps; new connections translate to the new pod IP. Nothing about BGP changes — the fabric only ever routed to pod CIDRs, and both old and new primary pods' IPs were always routable. There is no route to withdraw, no convergence to wait for. The failover window is purely {leader-key TTL + promotion + apiserver→Cilium propagation}, and that last hop is sub-second. This is a genuinely elegant property of the design and worth being able to articulate: *the routing fabric is topology-stable across database failover; only endpoint state moves.*

## The exceptional case: clients outside the cluster

HFT reality: some consumers of a database may be bare-metal trading hosts or legacy systems that are *not* pods. Then the database needs an address routable from outside, and the clean mechanism on this stack is: **Service type LoadBalancer + Cilium LB-IPAM + BGP service advertisement.** Cilium assigns a stable VIP from a `CiliumLoadBalancerIPPool`, and the BGP control plane advertises that /32 to the ToR peers. StackGres supports shaping its generated services (types/annotations) via the SGCluster's postgres-services configuration, so this is expressible declaratively.

Here the failover analysis genuinely depends on one Kubernetes field, and this is the sharp edge to own:

- **`externalTrafficPolicy: Cluster`** — every node advertises the VIP; a packet arriving at any node is eBPF-forwarded (SNAT'd) to the current primary pod. On failover, *routes do not change at all*; only the eBPF backend selection changes, propagating in sub-second time. Seamless from the fabric's perspective. Cost: an extra intra-cluster hop sometimes, and client source IPs are SNAT'd — which matters if anyone wants `pg_hba` rules or audit logs keyed on client IPs.
- **`externalTrafficPolicy: Local`** — only nodes actually hosting a backend advertise the VIP; source IPs preserved; no extra hop. But now **failover is a BGP event**: the old primary's node must *withdraw* its /32 and the new primary's node must *announce* it, and until the fabric converges, traffic can blackhole at the old node. Convergence is typically fast (BGP UPDATE propagation, not a timer), but "typically fast" now includes the ToR's behavior in the failure domain — and if the *node* died rather than the pod, you are waiting on BGP holddown/BFD to even notice. With BFD, milliseconds-to-subsecond; without, potentially the BGP hold timer (default 90s!).

> **PROD:** If any database VIP runs `externalTrafficPolicy: Local` over BGP, BFD to the ToRs stops being optional. A 90-second hold-timer blackhole on top of a 30-second Patroni failover is how a "one minute" database incident becomes a five-minute trading incident. Confirm the firm's BGP timer/BFD posture against this exact scenario in week one — it's a sharp, concrete question that shows you understand both layers.

For most internal-platform databases, the honest recommendation is: stay ClusterIP; where external exposure is unavoidable, prefer `Cluster` policy for topology-stability unless source-IP preservation is a hard requirement, and document the convergence math either way.

## Network policy for databases

With no encapsulation and eBPF policy enforcement at each endpoint, CiliumNetworkPolicy is the database access-control layer, and default-deny in database namespaces is the right posture. The policy for a database namespace must admit, or everything breaks in instructive ways:

1. **Client ingress** — port 5432 (or the pooler port chain, per your version) from labeled client workloads/namespaces only. This is your tenant isolation at L3/4.
2. **Patroni REST, 8008, pod↔pod within the cluster** — members health-check each other; SGDbOps and the operator also probe it.
3. **Replication, 5432, pod↔pod within the cluster** — replicas stream WAL from the primary.
4. **Egress to `kube-apiserver` entity** — the DCS. Blocking this **demotes primaries** (the coupling from the Patroni chapter, now as a firewall rule). Use `toEntities: [kube-apiserver]`; on a Kubespray on-prem topology, verify the entity actually matches your apiserver endpoints (Cilium derives it, but confirm — this is a classic on-prem gotcha where apiserver traffic goes via a VIP that Cilium classifies differently).
5. **Operator webhook/agent traffic** — operator namespace → database pods (cluster-controller/Patroni endpoints) and apiserver → operator webhooks.
6. **Egress to object storage** — WAL-G's backup pushes and WAL archiving to the S3 endpoint. Forgetting this rule produces the quiet failure mode: cluster healthy, **backups silently failing** — see Volume 2's runbooks.
7. **Egress DNS** (kube-dns) and metrics ingress from the VictoriaMetrics scrapers' identity.

The instructive failure taxonomy: block (1) and clients complain immediately; block (4) and you cause a platform-wide primary demotion; block (6) and nothing complains until restore day. Policy changes on database namespaces deserve the same review rigor as schema migrations.

# Backups and PITR: WAL-G Under the Hood

The architecture is the classic base-backup-plus-WAL-archive design from the Patroni chapter, implemented by **WAL-G** inside the patroni container:

- **Continuous WAL archiving**: Postgres `archive_command` invokes `wal-g wal-push` for every completed 16MB segment, compressed and encrypted (if configured) into the SGObjectStorage bucket under the cluster's `path`. This runs *always*, independent of backup schedules — the archive is a continuous tail of every committed byte.
- **Scheduled base backups**: on `cronSchedule`, `wal-g backup-push` streams a full base backup (WAL-G also supports delta backups) from a cluster member. Each produces an SGBackup resource with LSN bounds and status.
- **Retention**: `retention: 7` keeps the last 7 *full* backups and prunes WAL older than needed by the oldest kept backup. Your real PITR horizon is therefore "oldest retained base backup → now," continuously.

**Restore/PITR is cluster creation, not an in-place operation**: you create a *new* SGCluster whose `spec.initialData.restore` points at an SGBackup (by name/UID) with an optional `pointInTimeRecovery.restoreToTimestamp`. WAL-G pulls the base backup, Postgres replays archived WAL to the target, the new cluster goes live, and you cut clients over. This immutable-restore shape is operationally *good* — the damaged cluster stays available for forensics, and restore never races the thing it's restoring — but it means restore time includes full data transfer + WAL replay, and **it means you must know that number**. An untested backup is a hypothesis.

> **PROD:** Institutionalize restore drills: quarterly, restore a production-scale cluster into a scratch namespace, measure wall-clock time to accepting queries, and record it next to the cluster's RTO expectation. WAL replay speed after a busy trading day is the term people forget to measure. Also verify the *deleted-cluster* story: retention of SGBackups and bucket contents after `kubectl delete sgcluster` decides whether deletion is recoverable or final — confirm the configured behavior on day one, before someone tests it in production.

Two design notes to carry into reviews. First, backup traffic (base backups especially) competes with production I/O and network; `performance.uploadDiskConcurrency` and scheduling backups off trading-critical hours are real levers. Consider running base backups from a replica where supported to keep the primary's I/O clean. Second, recent StackGres can alternatively use **CSI volume snapshots** for backups — whether Weka's CSI supports the snapshot capability well enough to rely on is an open platform question; the WAL-G object-storage path is the conservative default and keeps restore independent of the storage layer's health.

# Storage: StackGres on Weka CSI

StackGres itself is storage-agnostic — it asks for PVCs from a named storage class and expects POSIX filesystem semantics with honest `fsync`. The interesting analysis is where cloud-CSI assumptions meet a parallel filesystem:

**What holds.** Weka CSI provisions RWO filesystem volumes dynamically; StatefulSet volumeClaimTemplates work normally; Postgres runs fine on a POSIX FS. Weka's latency profile (NVMe-backed, microsecond-class) is excellent for WAL fsync, which is the latency-critical path for commit performance — likely better than most cloud block storage.

**What to verify rather than assume:**

1. **Durability semantics of fsync** on the Weka client under node failure — Postgres correctness assumes fsync-acknowledged data survives a crash. Weka's client is a kernel driver with its own caching; get the firm's Weka configuration confirmed as write-durable-on-fsync (it should be, but this is a "confirm, don't assume" item for a database platform).
2. **Volume expansion**: `allowVolumeExpansion` on the storage class and the CSI's online-expansion support. StackGres PVC resize flows through the StatefulSet/PVC machinery and is a known operational rough spot generally (Volume 2 §15) — knowing whether Weka expands online, offline, or awkwardly determines your resize runbook.
3. **CSI snapshots** — as above, relevant only if the snapshot backup path is ever considered.
4. **Failure-domain honesty of anti-affinity.** StackGres enforces pod anti-affinity by default (one cluster member per node — disabling it lives in `nonProductionOptions`, which is why that field should be empty in prod). But with Weka, *storage* is a shared distributed system across the same hardware estate: node-level anti-affinity protects against node loss, while a Weka-cluster-level incident is a *correlated* failure across all members — every replica's volume degrades together. Your real independence story for that failure class is the object-storage backup archive (different system) and, for the truly critical, cross-cluster replication. This is precisely the kind of correlated-failure-domain reasoning an HFT reliability review will expect you to volunteer, and it echoes your Patroni-on-shared-SAN instincts from Patroni's own documentation lore: HA replicas on shared storage are less independent than they look.
5. **The apiserver coupling compounds this**: platform etcd, Weka, and the database fleet are three "shared fate" systems. Map which failure of each degrades which guarantee (etcd → failover ability; Weka → all replicas' I/O; object store → backup/PITR) and you have the platform's actual database dependency diagram — worth literally drawing in week one.

**PROD:** One pragmatic Weka-specific note: parallel filesystems sometimes exhibit different small-sync-write vs large-sequential profiles than block devices; `full_page_writes`, `wal_compression`, and checkpoint tuning interact with this. Benchmark with `SGDbOps op: benchmark` (pgbench) on the real storage class before blessing instance profiles — StackGres gives you the tool; use it to replace folklore with numbers.

# Security

## Credentials

**[StackGres]** Cluster creation generates Secret `<cluster>` containing the `superuser`, `replication`, and `authenticator` passwords. The replication and authenticator roles are internal plumbing (Patroni replication; PgBouncer auth_query). The superuser credential is the crown jewel: RBAC on *reading Secrets in database namespaces* is effectively RBAC on the databases themselves. The platform stance should be that humans don't read that Secret in normal operation — application access uses per-team roles created via SGScript/self-service (Volume 2 §10), and break-glass superuser access is audited. The Vault comparison is deliberately deferred to Volume 2's credential-lifecycle section, where it does real work; here, note only that a Secret-object password is static-at-rest in etcd — everything Vault taught you about rotation, leasing, and audit is *absent by default* and must be added deliberately if required.

## TLS between components

Three distinct TLS surfaces; keep them separate in your head because they have separate answers:

1. **Operator webhooks/REST** — certificates for apiserver→webhook trust, self-managed by the operator or via cert-manager. Platform-internal machinery.
2. **Client↔Postgres/PgBouncer** — off by default; enabled via `SGCluster.spec.postgres.ssl` with certificate/key from referenced Secrets. Whether the firm requires wire TLS *inside* the cluster is a policy question intertwined with the next point.
3. **Replication traffic** between members — Postgres-level TLS, configurable, same certificate machinery.

## The SPIFFE/SPIRE question — flagged as open

**Open question to confirm on day one:** StackGres has no native SPIFFE/SPIRE integration. Its identities are Postgres roles and passwords plus optional Postgres TLS with statically-provisioned certificates; nothing in the operator issues SVIDs, consumes the Workload API, or maps SPIFFE IDs to Postgres identities. So the platform's zero-trust identity layer and the database layer are, out of the box, *disjoint* systems. What to find out: (a) does the firm terminate/authorize database access at a SPIFFE-aware layer in front (which Cilium's mutual-auth beta could relate to — remembering the sharp caveat from the Cilium material that Cilium mutual auth authenticates agent identity pairs and does *not* provide payload encryption or end-to-end application mTLS, so it cannot substitute for Postgres TLS); (b) is there tooling that renders SPIRE-issued X.509 into the Secrets StackGres consumes for Postgres SSL, giving rotating certs with SPIFFE lineage but opaque to StackGres; or (c) is database auth simply accepted as password/role-based inside a network-policy perimeter, with zero-trust scoped to service↔service traffic? All three are defensible; which one is *true here* changes your runbooks (cert rotation incidents look completely different in each) and is exactly the kind of precise architectural question worth arriving with.

**RBAC over the CRDs** closes the chapter: whoever can write SGCluster/SGDbOps/SGScript objects can restart databases, run arbitrary SQL (SGScript!) as superuser, and trigger major upgrades. **PROD:** treat SGScript write access as equivalent to superuser SQL access — because it is. On the GitOps platform this concentrates into: who can merge to the tenant repos, and which ServiceAccount Flux impersonates when applying (the `--default-service-account` lockdown from the Flux material becomes, here, the actual database privilege boundary). The layers connect: Git permissions → Flux impersonation → CRD RBAC → operator → superuser SQL. Volume 2 opens by walking that exact chain forward as the self-service workflow.
