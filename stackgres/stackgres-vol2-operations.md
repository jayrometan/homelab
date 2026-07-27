---
title: "StackGres on the PaaS Stack — Volume 2: Operations & Self-Service Playbook"
subtitle: "Provisioning workflow, credential lifecycle, daily operations, monitoring, incident runbooks, upgrades, and sharp edges"
author: "Platform Engineering Reference Series"
date: "July 2026"
toc: true
toc-depth: 2
numbersections: true
geometry: margin=2.2cm
fontsize: 10pt
---

# How to Use This Volume

This is the standalone operations playbook: it assumes Volume 1's mental model (Patroni's leader-key protocol, the StackGres CRD family, the sidecar pod shape, the Cilium exposure analysis) and gets straight to *doing*. Sections 13 and 16 are written to be readable mid-incident: symptom-first, with explicit escalation criteria. Ownership boundaries are stated everywhere in the form **[App team]** / **[PaaS team]** / **[Shared]**, because on a self-service platform the second question in every incident, right after "what's broken," is "whose move is it."

One caveat honestly stated: the self-service sections describe the *canonical* shape for this stack (Git → Flux → KubeVela → CUE-rendered SGCluster) with the decision points a real implementation must have chosen. The firm's actual choices at each decision point are flagged as day-one confirmations, not assumed.

# Self-Service Provisioning, End to End

## The path a database request travels

The whole platform design compresses into one sentence: *a quant team merges a small CUE-shaped request into Git, and several reconciliation layers later a production Postgres cluster exists, sized, backed up, monitored, and network-isolated, without a human on the PaaS team touching kubectl.* Walking the path layer by layer — because when provisioning stalls, you debug it in exactly this order:

1. **[App team] The request.** The team edits their application definition in their tenant Git repository — on this stack, plausibly a KubeVela `Application` with a `postgres-cluster` component, or a CUE file that unifies against a platform-published `#PostgresCluster` definition. What the team writes is deliberately small: a name, a size tier (`size-s|m|l` mapping to SGInstanceProfiles), a storage amount, maybe a replication tier (`async`/`quorum-sync`) and an extensions list. Everything else is platform-owned.

2. **[Shared] Review gate.** The merge request is the *only* human gate in the happy path. Who must approve is a policy choice with real teeth: a `CODEOWNERS` rule can require PaaS review for new SGCluster-shaped components while letting teams self-merge changes to their own app workloads. **Day-one confirmation:** where exactly this gate sits, and whether size/storage *changes* (not just creation) re-trigger it — a storage resize is not an innocent edit (§15).

3. **[PaaS team] Flux reconciles.** The source-controller pulls the merged commit; kustomize-controller (or helm-controller, but on this stack kustomize) applies the manifests under the tenant's Kustomization — critically, impersonating the tenant's ServiceAccount per the `--default-service-account` lockdown. That ServiceAccount's RBAC is the *real* privilege boundary: it must be allowed to write `Application` (KubeVela) objects in the tenant namespace, and *its* effective reach determines whether a tenant can smuggle in resources the platform didn't intend. This is where the Flux multi-tenancy triad stops being theory and becomes database security.

4. **The Flux↔KubeVela handoff.** The open architectural question from the Flux/KubeVela material lands here with a concrete instance: does Flux apply the KubeVela `Application` (KubeVela's controller then expands it), or does Flux apply *rendered* SGCluster manifests directly (CUE evaluation happening in CI or in a Vela workflow)? The difference is operationally material: in the first shape, a rendering bug surfaces in *vela/application status*; in the second, in *CI or Flux*. **Confirm on day one which layer owns the expansion** — it determines where you look when "I merged and nothing happened."

5. **[PaaS team] KubeVela/CUE renders.** The platform-owned ComponentDefinition's CUE template unifies the team's parameters with platform policy and emits the SGCluster plus its satellite objects (SGInstanceProfile references, backup configuration pointing at the team's SGObjectStorage, an SGScript for bootstrap, CiliumNetworkPolicy for the namespace, ServiceMonitor wiring). Remember the CUE mental model: unification is intersection, not override — the platform definition *closes* the fields it owns. A team cannot set `nonProductionOptions`, cannot point backups at a nonexistent bucket, cannot choose a storage class, not because a policy engine rejects it but because the definition offers no such degree of freedom. This is the deepest difference from a Helm-values world and the platform's strongest safety property: **the invalid states are unrepresentable in the tenant-facing schema.**

6. **StackGres operator reconciles** the SGCluster: webhooks validate, StatefulSet and Services and Secrets appear, Patroni bootstraps the primary, replicas clone, managed SQL runs, the first backup fires on schedule. Watch it converge via `kubectl get sgcluster -n <ns> <name> -o yaml` status conditions and pod readiness.

**Where the self-service boundary sits (the summary to keep):** [App team] owns: requesting clusters within blessed tiers, their own databases/roles/schemas (§10), their application's connection handling, their data. [PaaS team] owns: the CUE definitions and every field they close, SGInstanceProfile/SGPostgresConfig/SGPoolingConfig libraries, storage classes, backup infrastructure and restore execution, SGDbOps (all of them — teams request operations, platform runs them), version lifecycle, and everything in §13's escalation columns. [Shared]: capacity planning conversations and the review gate. **Anything requiring the superuser credential is PaaS-side by definition.**

> **PROD:** The provisioning path has five queues (Git, Flux, Vela, operator, Patroni bootstrap) and therefore five distinct places to stall. Learn the status-checking command for each *before* the first "my database isn't appearing" ticket: `flux get kustomizations`, `vela status`, `kubectl get sgcluster -o yaml` (conditions), `kubectl get sts,pods`, `patronictl list`. Ninety percent of provisioning tickets resolve by reading these five outputs in order.

# Databases, Roles, and Credentials in a Self-Service Model

## Two bootstrapping philosophies

Once a cluster exists, teams need databases, roles, and grants. Two workable models, usually blended:

**Model A — declarative bootstrap via SGScript.** The platform's CUE definition emits an SGScript that creates the team's baseline: a database, an application role, a migrations role, sane default privileges. Because SGScript runs as superuser and re-runs on version bumps, it must be strictly idempotent (`IF NOT EXISTS`, guarded `DO` blocks). Teams request *changes* to the baseline through Git, same as everything else. Strength: auditable, reproducible, restores cleanly onto a rebuilt cluster (rerun the scripts). Weakness: superuser-executed SQL authored by tenants is a privilege-escalation surface — **[PaaS team]** must own or review SGScript content, not just rubber-stamp it; a tenant-authored `ALTER ROLE postgres` hides easily in a long migration.

**Model B — delegated ownership role.** Bootstrap (via Model A) creates one privileged-*within-scope* role per team — owner of their database, `CREATEROLE` for their own role tree, no superuser — and hands the team its credential. From there, teams run their own SQL/migrations through their normal tooling (Flyway/Liquibase in CI). Strength: teams move at their own speed; migrations live with app code where they belong. Weakness: role sprawl and drift live outside Git; a restore reproduces whatever SQL state existed at backup time, not a declared intent.

The blend that works in practice, and likely what you'll find or build: **SGScript for the paved-road skeleton (database, owner role, defaults, monitoring grants), Model B for everything inside the team's database.** The boundary sentence for tenant docs: *the platform creates your house and hands you the keys; the furniture is yours.*

## Isolation patterns between teams

Whether isolation is per-cluster or within a shared cluster is a platform product decision; on an HFT platform with real blast-radius religion, **dedicated clusters per team is the sane default** — noisy-neighbor and privilege isolation come free, and StackGres makes cluster count cheap to operate. Where sharing happens anyway (cost, tiny workloads), the Postgres-native isolation kit, in the order it must be applied:

```sql
-- Separate databases per team; kill the PUBLIC backdoors:
REVOKE ALL ON DATABASE alpha_db FROM PUBLIC;        -- no CONNECT by default
-- (Postgres 15+ already revoked CREATE on schema public)

CREATE ROLE alpha_owner NOLOGIN;
CREATE ROLE alpha_app   LOGIN IN ROLE alpha_owner;  -- app connects, inherits
GRANT CONNECT ON DATABASE alpha_db TO alpha_owner;
ALTER DATABASE alpha_db OWNER TO alpha_owner;

-- Default privileges so future objects stay in-team:
ALTER DEFAULT PRIVILEGES FOR ROLE alpha_owner IN SCHEMA app
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO alpha_app;
```

The two classic mistakes: forgetting `REVOKE CONNECT ... FROM PUBLIC` (any role can connect to any database by default — "separate databases" alone is not isolation), and granting on existing tables while forgetting `ALTER DEFAULT PRIVILEGES` (next migration's new table silently lacks grants — the Monday-morning permission-denied classic). Remember also that **roles are cluster-global** in Postgres even when databases are separate: naming conventions (`<team>_*`) and CREATEROLE scoping matter, and Postgres 16+'s tightened CREATEROLE semantics (grantor tracking, no free membership grants) make the delegated-owner model meaningfully safer — a concrete argument for modern majors on shared clusters.

## Credential lifecycle — the Vault lens

Here is where your Vault depth pays off directly, because this is a secret-lifecycle problem you already know how to reason about, and the default StackGres posture is *behind* what Vault taught you to expect.

Out of the box: passwords live in Kubernetes Secrets — static, unleased, unaudited-on-read (short of apiserver audit logging), rotated never. Against the Vault model — short-TTL leased credentials, per-consumer identities, rotation as a background hum, revocation as a first-class verb — the gap is stark. Ways the platform can close it, in ascending order of investment:

1. **Static secrets, rotated by process**: SGScript or scheduled jobs `ALTER ROLE ... PASSWORD`, update the consuming Secret, roll app pods. Cheap; rotation is an *event* with coordination cost, so it happens quarterly at best. This is the floor, and many shops live here.
2. **External Secrets Operator / Vault Agent injection**: source-of-truth password in Vault, synced into K8s Secrets; rotation becomes a Vault-side action propagated automatically. Better custody and audit; rotation still discrete.
3. **Vault database secrets engine against StackGres**: Vault holds a root-ish credential (a dedicated `vault_admin` role — *not* the superuser Secret) and mints **dynamic, per-consumer, short-TTL roles** on demand. This is the full model you know: unique identity per pod-ish consumer, automatic expiry, revoke-on-compromise. It composes cleanly with StackGres — Vault only needs SQL access — and note the interplay with PgBouncer's `auth_query` (dynamic roles are real Postgres roles, so pooler auth keeps working). Costs: connection-churn from short TTLs (tune TTLs vs pool lifetimes), and Vault availability enters the database-connect path.
4. The SPIFFE-shaped end state (cert-based auth mapped to roles) remains the open question flagged in Volume 1's security chapter.

**Day-one confirmations:** which rung the firm is on; where the superuser Secret is allowed to be read and by whom; whether anything rotates today. **[PaaS team]** owns rungs and rotation machinery; **[App team]** owns consuming credentials correctly (no baking passwords into images — the eternal sin).

# Day-to-Day Operational Commands

The operating discipline from Volume 1, restated as the header of this section: **state via patronictl, configuration via CRDs, operations via SGDbOps.** Everything below is read-path or state-path; if you find yourself wanting to *edit* something imperatively, stop and go through Git.

```bash
# ---- Platform-level sweep (start of day / start of incident) ----
kubectl get sgclusters -A                      # fleet at a glance
kubectl get sgcluster -n NS NAME -o yaml | less   # read .status.conditions first
kubectl get sgdbops -A                         # anything running/stuck?
kubectl get sgbackups -n NS \
  --sort-by=.status.process.timing.stored      # recency of backups

# ---- One cluster: topology & health ----
kubectl exec -n NS NAME-0 -c patroni -- patronictl list
#  +---------+---------+---------+---------+----+-----------+
#  | Member  | Host    | Role    | State   | TL | Lag in MB |
#  Read: exactly one Leader; States all running/streaming;
#  one timeline number; lag ~0. Anything else -> §13.
kubectl exec -n NS NAME-0 -c patroni -- patronictl history   # timeline forks
kubectl get svc,endpoints -n NS | grep NAME    # primary endpoint sanity
kubectl get pods -n NS -l app=StackGresCluster,stackgres.io/cluster-name=NAME -o wide

# ---- Replication & pooling detail (via postgres-util or psql) ----
kubectl exec -it -n NS NAME-0 -c postgres-util -- psql -c \
  "select application_name, state, sync_state,
          pg_wal_lsn_diff(pg_current_wal_lsn(), replay_lsn) as lag_bytes
     from pg_stat_replication;"
# PgBouncer console (per pod):
#   psql -p <pooler-port> pgbouncer -c 'SHOW POOLS;'   # cl_active/waiting, sv_*
#   'SHOW STATS;'                                       # per-db traffic

# ---- Operator & controllers when reconciliation stalls ----
kubectl logs -n stackgres deploy/stackgres-operator --since=15m
kubectl logs -n NS NAME-0 -c cluster-controller --since=15m
kubectl describe sgcluster -n NS NAME          # events + conditions
flux get kustomizations -A | grep -v True      # is the stall upstream of StackGres?
vela status APP -n NS                          # ...or in the Vela layer?

# ---- State operations (deliberate, announced, logged) ----
kubectl exec -n NS NAME-0 -c patroni -- patronictl switchover \
    --candidate NAME-1 --force                  # planned primary move
kubectl exec -n NS NAME-0 -c patroni -- patronictl reinit CLUSTER NAME-2 --force
                                                # wipe & reclone a broken member
```

Three habits worth forming immediately. First, `.status.conditions` on the SGCluster before anything else — the operator narrates its own blockage there. Second, `patronictl list` is the single highest-information command on the platform; read it as a sentence ("one leader, everyone streaming, one timeline, no lag") and any deviation names its own runbook. Third, when reconciliation stalls, walk the queue chain *downstream to upstream* — operator logs, then Vela, then Flux — because the symptom appears at the bottom regardless of where the cause sits.

The web console / REST API is a legitimate *read* surface (nice topology and backup views) — treat writes through it as drift against GitOps, to be used only in declared break-glass.

# Monitoring and Alerting

## What flows into VictoriaMetrics

Each database pod's **prometheus-postgres-exporter** sidecar exposes Postgres internals (`pg_*` series: replication state, connection counts, database sizes, transaction age, bgwriter/checkpoint stats); **Patroni's REST API on 8008 exposes `/metrics`** with cluster-topology truth (`patroni_master`/`patroni_primary`, `patroni_replica`, timeline, xlog positions, DCS health); **PgBouncer stats** (pools, waiting clients) arrive via the exporter or the pgbouncer console depending on version wiring; the **operator** exposes its own reconciliation metrics. `prometheusAutobind: true` on the SGCluster generates the monitor objects; on this platform vmagent's operator-CRD compatibility discovers them into VictoriaMetrics. **Day-one confirmation:** that autobind's generated objects match what vmagent actually watches (ServiceMonitor/PodMonitor vs VMServiceScrape translation) — the classic silent gap where a cluster is "monitored" but scraping nothing.

## Healthy at a glance

A healthy cluster, as a dashboard sentence: *exactly one series reports primary=1 per cluster; every replica streams with lag under seconds; connections comfortably under max with zero pooler wait queue; last successful backup age under 26h and WAL archiving success within minutes; xact age far from wraparound; disk under 70%.* Build the fleet overview so each cluster is one row of exactly those cells — during a platform incident you triage thirty databases in one screenful.

## Actionable signals vs noise

Alerts that page, because each has a runbook and a deadline:

- **No primary** (`max(patroni_primary) == 0` per cluster, sustained > ~1–2 min — i.e. beyond a normal failover window) → §13.1. The one alert that must never be delayed.
- **Multiple primaries** reported → treat as potential split-brain artifact; page immediately (almost always a metrics-staleness artifact, but the cost of checking is one `patronictl list`).
- **Replication broken or lag beyond policy** (no streaming replicas, or lag > threshold for > 5–10 min) → §13.2. On sync/quorum clusters, lag alerts are effectively *write-latency* alerts — tighter thresholds.
- **WAL archiving failing** (`pg_stat_archiver` failure counter increasing, or archive lag growing) → your PITR horizon has stopped advancing *and* `pg_wal` will eventually fill the disk. Quiet, urgent, and the single most-ignored database alert in the industry.
- **Backup staleness** (no successful SGBackup within schedule + slack) → §13.3.
- **Pooler saturation** (`cl_waiting` sustained > 0, or client conns near `max_client_conn`) → §13.4.
- **Disk** (>80% warning with days-to-full projection; >90% page) → §13.5. Include `pg_wal` growth as its own series — WAL-driven disk-full has different causes (slots, archiving) than data-driven.
- **Transaction ID age** (`age(datfrozenxid)` > ~1.5B) → autovacuum is losing; this is a slow-motion outage with a hard deadline.
- **Stuck SGDbOps** (running > expected duration, or Failed) → §13.6.

Deliberately *not* paging: individual pod restarts (Patroni's job is to make these boring — alert on the consequence, not the event), single failed *scrapes*, checkpoint frequency warnings, CPU spikes without symptom correlation, replica restarts during declared SGDbOps windows (silence by maintenance annotation). Every alert must name whose move it is: no-primary and archiving failures are **[PaaS team]**; pooler saturation opens **[Shared]** (is it platform sizing or app connection-leak?); slow queries are **[App team]** with platform consultation.

## SGDistributedLogs vs the Fluent Bit → VictoriaLogs pipeline

These are *parallel, competing* paths for the same data. StackGres ships a fluent-bit sidecar in every database pod; its output either (a) feeds SGDistributedLogs — StackGres's own Postgres+timescaledb log store, queryable in the web console, itself now an SGCluster you must operate, upgrade, and monitor — or (b) joins the platform-standard pipeline into VictoriaLogs. Running both doubles storage and splits the query surface; running SGDistributedLogs means *operating a database to observe your databases*, with circular-dependency flavor during platform-wide incidents. On a platform that already owns VictoriaLogs, the coherent posture is: **route database pod logs through the standard pipeline, skip SGDistributedLogs entirely**, and accept losing the console's integrated log view. **Day-one confirmation:** which choice was made; if SGDistributedLogs exists, it belongs on the fleet dashboard like any other cluster — it is one, since 1.15 literally so.

# Incident Runbooks

Format: symptom → diagnosis sequence → resolution → *escalate when*. All commands assume `NS`/`NAME` set. Universal step zero for every runbook: `patronictl list` + SGCluster `.status.conditions` + last 15m of operator logs — thirty seconds that classifies most incidents.

## 13.1 Primary unreachable / failover not completing

**Symptom:** no-primary alert; writes failing; `patronictl list` shows no Leader, or a Leader with State ≠ running.

**Diagnose, in order:**
1. `patronictl list` from a *surviving* pod. Case A: a healthy Leader exists → the problem is routing/pooling, not Patroni; jump to step 4. Case B: no Leader → continue.
2. Why is no one promoting? Check candidate eligibility: replicas present and running? Lag beyond `maximum_lag_on_failover` (shown in list output / Patroni logs: "not healthy enough to become leader")? On sync/quorum clusters, only sync-confirmed members are eligible — if all eligible members are gone, Patroni is *correctly* refusing a lossy promotion.
3. Check the DCS path: can pods reach the apiserver? (`kubectl logs ... -c patroni | grep -i 'dcs\|failed to update leader'`). Apiserver/etcd degradation, a new CiliumNetworkPolicy, or webhook interference on Endpoints writes each produce "healthy Postgres, demoting" log lines — this is the platform-coupling failure; scope check: are *other* clusters also leaderless? If yes, this is a platform control-plane incident wearing a database costume.
4. If a Leader exists but clients fail: `kubectl get endpoints NAME` — does it point at the leader pod's IP? Test path: `psql` from postgres-util on another pod to the primary Service. Endpoint correct + connection refused → check the pooler container on the primary; endpoint stale → Patroni can't write it (back to step 3).

**Resolve:** Case A routing issues: fix the network-policy/apiserver cause; endpoints self-heal on the next Patroni loop. Case B with an eligible-but-lagging replica and business pressure: promoting a lagging replica is a *data-loss decision* — quantify the lag in bytes/time first, and treat it as an explicit, recorded business call, not a reflex (`patronictl failover --candidate ...` once authorized). Case B with a recoverable old primary (node rebooting): often the fastest lossless path is letting it return. Never `pg_ctl promote` by hand behind Patroni's back — that manufactures split-brain.

**Escalate when:** any promotion would lose data (business decision, not an engineering one); the DCS path is broken (platform incident commander — scope is bigger than this cluster); or two members ever both claim writability (all-hands data-integrity event: fence one immediately by scaling its pod down, then audit timelines before accepting writes).

## 13.2 Replica lagging or replication broken

**Symptom:** lag alert; `patronictl list` shows a member stopped/`in archive recovery`, or streaming with growing lag.

**Diagnose:** 1. One replica or all? All-replicas lag → primary-side cause (WAL burst from a bulk load, primary I/O saturation, network) — check `pg_stat_replication` sent-vs-replay deltas and primary I/O metrics. One replica → that pod's story: `kubectl describe pod` (evictions, OOM), its Postgres logs, its volume metrics (a sick Weka mount shows here first). 2. Broken vs slow: `pg_stat_replication` missing the member entirely means the replication connection is down — check replica logs for the reason: authentication, network policy, or the fatal one: `requested WAL segment ... has already been removed`. 3. After a recent failover, check for timeline divergence: replica logs mentioning timeline mismatch / pg_rewind failure.

**Resolve:** Slow-but-streaming usually self-heals once the burst passes; the decision is whether to *shed* the replica from the read Service if stale reads violate app expectations. WAL-already-removed or rewind-failed → `patronictl reinit` (full reclone; know the hours-for-this-size number from your restore drills, and mind read capacity while it runs). Recurring lag on one member → suspect its node/volume; drain-and-reschedule before deeper archaeology.

**Escalate when:** the *last* replica of a sync/quorum cluster is failing (primary write availability is now hostage — see the strict-sync behavior in Vol 1); reinit would leave zero streaming replicas on a cluster whose policy requires one (schedule, don't improvise); or lag is caused by primary disk pressure (this is really §13.5).

## 13.3 Backup failures

**Symptom:** backup-staleness alert, or SGBackup objects in Failed.

**Diagnose:** 1. `kubectl get sgbackups -n NS` — one failure or a streak? 2. Read the failed object: `kubectl get sgbackup X -o yaml` → `.status.process.failure`. 3. Classify: object-storage reachability (network policy egress! credentials expired; endpoint down), disk-side (no room for the backup temp/bandwidth), or timeout/interruption (backup ran into a restart window). 4. **Always also check WAL archiving** (`pg_stat_archiver`): base backups and archiving fail independently, and archiving failure is the more urgent of the two (PITR horizon + disk).

**Resolve:** fix the classified cause, then *trigger a manual backup now* rather than waiting for tomorrow's cron — create an SGBackup object referencing the cluster and watch it complete. Prune any half-written garbage per WAL-G's own accounting (don't hand-delete bucket objects).

**Escalate when:** archiving has been down long enough that `pg_wal` growth threatens disk (§13.5 takes over); or the failure streak means the newest restorable point is older than the cluster's declared RPO — at that point this is a data-protection incident with mandatory stakeholder notification, not a cron babysitting task. **[PaaS team]** owns this entire runbook; teams should never see it.

## 13.4 Connection pool exhaustion

**Symptom:** apps report timeouts acquiring connections; `SHOW POOLS` shows sustained `cl_waiting`; possibly `max_client_conn` errors.

**Diagnose:** 1. Which side is saturated? `cl_active` at `max_client_conn` → client-side ceiling; `sv_active` pegged at pool_size with `cl_waiting` queued → server-pool ceiling. 2. Server-pool saturation: are server connections *busy* or *stuck*? `psql: select state, wait_event_type, count(*) from pg_stat_activity group by 1,2;` — a pile of `idle in transaction` is an application bug (transactions opened and abandoned — the classic ORM leak), and no pool size fixes it. Long-running queries holding connections → app/query problem. Genuinely busy short transactions → real capacity pressure. 3. Check for the transaction-mode trap: a deploy that introduced session state (prepared statements without pooler support, session `SET`s) can manifest as mysterious errors that teams describe as "pool problems."

**Resolve:** idle-in-transaction → the owning **[App team]** fixes the leak; platform's mitigations are `idle_in_transaction_session_timeout` in SGPostgresConfig (a policy worth having fleet-wide anyway) and, in extremis, targeted `pg_terminate_backend`. Genuine capacity → raise pool sizes via SGPoolingConfig *through Git* (mind the math: pool_size × databases × pods vs `max_connections`), or scale the instance profile. Never fix a leak by raising `max_connections` — you convert a visible queue into invisible memory pressure on the primary.

**Escalate when:** it's cross-tenant on a shared cluster (one team's leak starving another's pool is exactly the blast-radius argument for dedicated clusters — document and use it); or saturation is a symptom of a slow primary (→ 13.1/13.5).

## 13.5 Storage/disk pressure on Weka-backed PVCs

**Symptom:** disk alert; in the worst case Postgres PANICs on full `pg_wal` and the member crashes.

**Diagnose — the crucial split is *what* is growing:** 1. `df` in the pod; then apportion: data (`base/`) vs WAL (`pg_wal/`) vs logs/temp. 2. **WAL growing** → three suspects in order: (a) **orphaned replication slot** — `select slot_name, active, pg_size_pretty(pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)) from pg_replication_slots;` — an inactive slot with huge retained bytes is the smoking gun (a decommissioned CDC consumer, a long-dead replica); (b) **archiving failing** (13.3's urgent twin — WAL can't be recycled until archived); (c) a legitimate write burst outrunning checkpoints. 3. **Data growing** → bloat vs genuine growth: dead-tuple stats, autovacuum health, or simply the workload outgrowing the volume.

**Resolve:** orphaned slot → confirm the consumer is truly dead, then `pg_drop_replication_slot` (WAL frees on next checkpoint — visible relief in minutes). Archiving → fix per 13.3. Genuine growth → PVC expansion through Git (size field on the SGCluster), verifying the Weka online-expansion behavior established in Vol 1 — **and see §15 for the resize sharp edges before you start, not after**. Bloat → `SGDbOps op: repack` in a window. If Postgres already PANICked on full disk: expansion (or emergency slot-drop) *before* restart attempts; a crashed-on-full-WAL member recovers cleanly once space exists.

**Escalate when:** growth is fleet-wide (Weka capacity event — storage team, now); expansion isn't possible online and a window must be negotiated; or you are within hours of full on a primary (declare the incident *before* the PANIC, not after).

## 13.6 Stuck SGDbOps

**Symptom:** an SGDbOps has been Running far beyond expectation, or Failed midway — worst case a major upgrade half-applied.

**Diagnose:** 1. `kubectl get sgdbops X -o yaml` — `.status.conditions` and per-op status say which *step* stalled. 2. Find the executor: the op's Job/pod logs (or, 1.18 rollout-style ops, the operator's own logs). 3. Classify the stall: waiting on a pod that can't schedule (resources? anti-affinity on a full fleet?); waiting on a member restart that Patroni won't perform (cluster unhealthy → the op is *correctly* refusing to reduce redundancy); or the op crashed leaving partial state. 4. For half-applied *version* ops specifically, establish per-member versions: `patronictl list` + container image tags per pod — know exactly who is on what before touching anything.

**Resolve:** stalls with an external cause (scheduling, cluster health) → fix the cause; the op resumes or can be retried (`maxRetries`, or delete-and-recreate the SGDbOps for a fresh attempt — safe for restart-class ops, which are idempotent by design). Failed restart/minor-upgrade ops are generally rerunnable. **A failed majorVersionUpgrade is its own category**: do not improvise, do not rerun reflexively — the safe paths are (a) resume/retry per the op's documented behavior if pre-checks failed early (nothing applied), or (b) restore-from-backup into a new cluster if data-directory conversion partially ran. §14 covers why the pre-upgrade backup is non-negotiable.

**Escalate when:** any major-upgrade op fails after pg_upgrade began (senior + vendor-docs territory, restore path on standby); or a stuck op is holding a cluster in reduced redundancy across trading hours (explicit risk acceptance to wait vs act).

# Version Upgrades and Maintenance

**The severity ladder, in operational terms.** *Restart / security upgrade* (container/image refresh, same Postgres version — and note since 1.18 these two are logically equivalent, with an operator-driven rollout path that no longer uses Jobs): rolling, replicas first, switchover, old primary last; impact = one switchover's worth of connection resets; safe in a low-activity window, arguably even business hours for tolerant clusters — but on an HFT platform, *"safe" is a market-hours policy question, not a technical one*, and the answer is almost certainly "outside trading hours anyway." *Minor version upgrade* (17.5→17.6): same choreography plus new binaries; same impact class; do read release notes for the rare replication-relevant fix. *Major version upgrade* (16→17): the big one — `pg_upgrade` on the data directory, full cluster restart, real downtime, and **no in-place rollback once conversion has run**.

**What a real major-upgrade night looks like:** (1) Days before: provision a scratch cluster restored from production backup; run the major upgrade *there*; run the app team's smoke tests against it. This rehearsal is the single highest-value step and converts the night from exploration to execution. (2) Night of: confirm cluster health (`patronictl list` clean, no lag); **take a fresh backup and verify it completed** — this is the rollback plan; freeze tenant merges for the namespace. (3) Apply the SGDbOps (`op: majorVersionUpgrade`, target version + matching SGPostgresConfig for the new major — pre-staged in Git, and remember extensions: each pinned extension needs a compatible version declared, the most common pre-check failure). (4) Watch phase by phase; downtime begins at the shutdown-for-pg_upgrade step; with `--link`-style upgrades conversion is minutes, not hours, but *plan* for the pessimistic case. (5) Validate: connections, replication re-established, `ANALYZE` completed (post-upgrade planner statistics are empty — the classic "upgrade succeeded, everything is slow" cause), app smoke tests. (6) Rollback reality: before conversion = abort freely; after = **restore the pre-upgrade backup into a new cluster and cut over** — which is why step 2's backup and your rehearsed restore time (Vol 1's drills) are the actual safety net. Blast radius: one cluster, its full write path, for shutdown+conversion+validation; schedule against the market calendar, never mid-week hope.

**Operator upgrades** are the other maintenance class: upgrading StackGres itself updates CRDs/webhooks immediately, but running clusters keep their old pod shape until each receives a security-upgrade/restart — a deliberately decoupled, per-cluster-scheduled rollout. Track "operator version vs cluster version" fleet-wide; a long tail of never-restarted clusters is silent risk accumulation (and known-CVE exposure).

# Sharp Edges and Gotchas

A curated list of the places where the abstraction leaks — where you must reason about raw Patroni/PgBouncer/Postgres state despite the operator, or where a reasonable-looking action has unreasonable consequences.

**Config that "applied" but isn't in force.** SGPostgresConfig changes reconcile into Patroni's config, but restart-required parameters (`shared_buffers`, `max_connections`, ...) sit pending until each member restarts. The CRD looks applied; the database runs the old values — for weeks, if nobody restarts. Detection: `patronictl list` marks pending-restart members; `select name, setting, pending_restart from pg_settings where pending_restart;` is ground truth. Discipline: every restart-required config change is *scheduled with* its restart (an SGDbOps in the same MR), never left to be picked up "eventually" — because "eventually" is the next unplanned failover, at which point the new primary silently boots with different settings than the old one had, and you debug a performance mystery with a two-week-old cause.

**The Patroni-managed parameter set.** `max_connections` and friends are DCS-enforced with ordering constraints across members (replicas must restart before the primary when *decreasing*, etc.). Patroni handles the ordering, but combined with the pending-restart edge above, a `max_connections` change is the config change most likely to surprise. Treat it as a mini-maintenance, not an edit.

**Resize gone wrong.** PVC expansion: the SGCluster size field propagates to PVCs, but StatefulSet volumeClaimTemplates are immutable — operators (StackGres included) handle this with patch-PVC + recreate-StatefulSet choreography. Where it wedges: storage class without `allowVolumeExpansion`; CSI requiring offline expansion (verify Weka's behavior *before* the first production resize, per Vol 1); a resize mid-flight when a pod restarts, leaving one PVC bigger than its filesystem until kubelet's expansion completes. Never *shrink* (unsupported everywhere); never edit the StatefulSet directly (the operator owns it and will fight you). And on Weka specifically: PVC "capacity" and the shared backend's real capacity are separately exhaustible — a fleet of comfortable-looking PVCs can still hit a Weka capacity wall together (§13.5's fleet-wide escalation).

**Switchover vs failover judgment.** The recurring call: a primary is degraded-but-alive. Reach for switchover *early* — it needs the primary alive to be lossless, so the window closes as the node deteriorates. Waiting for Patroni to "handle it" trades a few seconds of planned disruption for tens of seconds plus the async loss window. The inverse trap: switchover to a lagging replica blocks (or should); don't force it — check lag first, always.

**Manual actions behind the operator's back.** `patronictl edit-config` gets reconciled away (Vol 1); `pg_ctl promote` manufactures split-brain; hand-editing generated ConfigMaps/StatefulSets is drift the operator reverts. The one sanctioned imperative surface is Patroni *state* ops (switchover, restart, reinit) — and even those should be SGDbOps when time permits, for the audit trail. If you genuinely must stop reconciliation to hold an unusual state during an incident, StackGres supports a reconciliation-pause annotation on the cluster — use it declared and time-boxed, because a paused cluster is a cluster whose safety machinery is off.

**SGScript versioning semantics.** Editing script SQL without bumping `version` = no-op (silently — the change *looks* deployed in Git). Bumping version = the entry runs again in full, as superuser. Both directions have burned people: the first as "why didn't my grant apply," the second as a non-idempotent script re-running against production. Enforce idempotency in review as a hard rule, and remember SGScript = superuser SQL (Vol 1's RBAC warning).

**Timeline divergence after messy failovers.** After any unplanned failover, `patronictl history` and per-member timelines are worth thirty seconds: a member stuck on an old timeline that pg_rewind couldn't fix will look "merely lagging" in casual inspection but will never catch up — it needs reinit, and until then your redundancy is fictional. Fictional redundancy discovered during the *next* incident is how bad nights become terrible ones.

**Backup path hygiene.** Reused object-storage paths poison PITR (Vol 1). Also: `retention` counts *base backups* — a cluster whose scheduled backups have been failing for a month still "has 7 backups"; they're just a month old. Alert on backup *age*, never count.

**The apiserver is load-bearing for the databases.** Restated once more because every platform eventually forgets during some unrelated maintenance: apiserver/etcd maintenance windows are *database availability-risk windows* (leader-key refresh). Sequence control-plane maintenance with the same care as database maintenance, and never do both at once.

# Incident Scenarios, Walked

## Scenario 1 — "The database is gone" (routing, not database)

*09:31, minutes after market open: team alpha reports every connection to `trades` timing out. Their dashboard shows their app pods healthy.*

You start with the universal step zero. `patronictl list` from `trades-0`: **Leader present, `trades-1`, running; replicas streaming; lag 0; timeline unchanged.** Thirty seconds in, you already know the most important fact: *this is not a Patroni event* — no failover happened, the database is healthy. The alert page agrees: no no-primary alert fired. So the break is between clients and a healthy primary: Service, endpoints, policy, or pooler.

`kubectl get endpoints trades -n trading-alpha` → points at `trades-1`'s pod IP. Correct. From `postgres-util` on `trades-0`, `psql -h trades.trading-alpha -U postgres` → connects fine. So in-namespace connectivity to the Service works; the client side doesn't. That shape — works from inside the namespace, fails from the client namespace — is a network-policy shape. `kubectl get cnp -n trading-alpha` and recent Flux applies: a platform-wide "default-deny hardening" Kustomization merged at 09:20 replaced the namespace's policy set, and the new DB-ingress rule matches label `role: app` while alpha's client pods carry `app.kubernetes.io/component: app`. Cilium policy drop metrics (`hubble observe --verdict DROPPED --to-namespace trading-alpha` if Hubble is on; the `cilium_drop_count` metrics otherwise) confirm drops from the client identity.

Resolution: revert the policy commit via Git (fast-forward fix MR; this is exactly what Flux makes fast), verify drops cease, connections restore. Total: ~10 minutes, no database action taken. Postmortem items: policy changes to database namespaces get the schema-migration review bar (Vol 1); add a synthetic client-namespace connect probe so this pages as *itself* rather than as tenant reports. The teachable structure: `patronictl list` first cleaved the incident space in half — every minute spent "checking the database" would have been waste.

## Scenario 2 — replica down, then the real problem

*Tuesday 14:00: lag alert for `risk-store`, one replica (`risk-store-2`) stopped. Async 3-node cluster; primary fine; app impact none (reads shifted to the surviving replica).*

Step zero: `patronictl list` — leader `risk-store-0` healthy, `risk-store-1` streaming lag 0, `risk-store-2` state `stopped`. Not urgent, but redundancy is degraded: policy says fix same-day. Pod events for `-2`: OOMKilled at 13:52 — interesting but secondary. Postgres logs on restart: `requested WAL segment 000000030000A1... has already been removed`. And there's the real story: the replica was down long enough (or WAL churn high enough) that the primary recycled the segments it needs; streaming cannot resume. §13.2 says reinit — but before recloning multi-hundred-GB over the afternoon, you check *why* WAL churned: `pg_replication_slots` on the primary shows Patroni-managed slots for both replicas... and slot retention for `-2` capped out because — checking further — the cluster's WAL sizing (`max_slot_wal_keep_size` in the platform's SGPostgresConfig) deliberately bounds retained WAL. Working as designed: the platform chose bounded-disk over unbounded-retention, and the price is reclone-after-long-outage.

`patronictl reinit risk-store risk-store-2 --force`, watch the basebackup stream; 70 minutes later it's streaming, lag burning down, list is clean by 15:30. The OOM thread continues separately: `-2`'s memory profile vs siblings shows a runaway analytics query pattern pinned to that replica (a team job targeting a pod, not the Service — a governance finding). Escalation check per §13.2: never needed — but you note that had this been the *sync* member of a quorum cluster, the same afternoon would have been a write-latency incident, and you file that asymmetry into capacity planning. The teachable structure: the alert said "lag," the pod said "OOM," the logs said "WAL gone" — three layers, and the *resolution* keyed off the third while the *prevention* keys off the second.

## Scenario 3 — disk pressure with a deadline

*Thursday 22:40: disk 85% and climbing on `fills` primary, projection says full by 03:00. Market opens 09:00; backup window at 01:30.*

Step zero is clean — topology healthy. §13.5's split question: *what* is growing? `du` apportionment: `pg_wal` is 260GB of a 500GB volume and growing ~15GB/hour. Data directory flat. So: slots, archiving, or burst. `pg_stat_archiver`: failures incrementing since 19:10. `pg_replication_slots`: healthy, active, small retention. Archiving it is — WAL can't recycle. The archive_command failure detail in Postgres logs: connection timeouts to the object-storage endpoint. From the pod, the endpoint doesn't respond; from *outside* the namespace it does. Recent changes: 19:00 change window on the object-store's front load balancer moved it to new VIPs — and the database namespaces' egress policy pins the *old* CIDR. Same class as Scenario 1 (policy-shaped break), different blast: this one was silent for hours because backups don't have users to complain.

Two clocks now: fix-the-cause and the 03:00 disk deadline. Cause: emergency MR updating the egress CIDR (platform on-call approves; Flux applies in minutes). `pg_stat_archiver` flips to successes; WAL begins draining as archiving catches up — but catch-up of ~5 hours of segments takes time, and the 01:30 base backup will *add* I/O. Decision: let the scheduled backup run (13.3's principle — after archive gaps you want a fresh base backup *anyway*, since PITR across the gap is what you're repairing), accept the temporary I/O overlap, and pre-approve an emergency PVC expansion as the hedge if draining loses the race. By 02:10 WAL is at 140GB and falling; no expansion needed; backup completed; PITR horizon verified continuous by checking the newest archived segment. Morning postmortem: archiving-failure alert existed but was routed to a non-paging channel — rerouted to page (per §12, it's in the *actionable* list precisely because of nights like this); object-store endpoint changes now include database-namespace egress policy in the change checklist. The teachable structure: the urgent symptom (disk) and the important cause (PITR horizon stopped) were the same incident — fixing the disk without verifying the archive continuity would have left the quieter, worse problem in place.

---

*End of Volume 2. Companion: Volume 1 — Architecture & Concepts.*
