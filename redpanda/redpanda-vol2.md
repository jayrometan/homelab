---
title: "Redpanda for the PaaS Stack"
subtitle: "Volume 2 — Operations & Self-Service Playbook"
author: "Platform Engineering Reference Series"
date: "July 2026"
toc: true
toc-depth: 2
numbersections: true
geometry: margin=2.2cm
fontsize: 10pt
mainfont: "DejaVu Serif"
sansfont: "DejaVu Sans"
monofont: "DejaVu Sans Mono"
papersize: a4
---

\newpage

# Orientation

Volume 1 built the architecture; this volume is the owner's playbook: the self-service model tenants consume, the daily tooling, monitoring, capacity engineering, incident runbooks, upgrade procedure, and the sharp-edge catalog. Conventions carry over: **[Kafka-protocol]** / **[Redpanda-specific]** tags, **[PaaS team]** / **[App team]** / **[Shared]** ownership markers, **PROD** and **LATENCY** callouts. Runbooks use the symptom → diagnosis → resolution → escalation shape from the StackGres volume, each ending with a blast-radius statement and the market-hours judgment call.

# Self-Service Topic Provisioning

## The path

Tenants get Redpanda resources the same way they get everything else: a change in their Git repository, reconciled by Flux, rendered through the platform's KubeVela/CUE definitions, landing as Kubernetes resources the Redpanda operator (or an ancillary controller) actualizes against the cluster. Nothing tenant-driven touches `rpk` or the Admin API directly — the Git history *is* the audit log and the rollback mechanism, exactly as with StackGres clusters.

What a tenant requests, in their application's OAM component or trait:

```cue
kafkaTopic: {
    name:       "alpha.md.normalized-ticks.v1"
    partitions: 12
    retention:  "72h"
    profile:    "durable" | "loss-tolerant-lowlat"
    acl: {
        producers: ["svc-alpha-mdgw"]
        consumers: ["svc-alpha-strat-eng", "svc-beta-risk"]
    }
    quotaMiBps: {produce: 50, fetch: 200}
}
```

## What the platform-owned CUE definition pins vs exposes

This is the same closed-`#Definition` discipline as everywhere else on the platform: the platform authors a closed struct; tenants fill leaf values; unification rejects anything outside the contract at render time — *before* Flux ever applies it. Concretely:

**Pinned (tenant cannot set):** replication factor (3, always — RF is a durability policy, not a preference); `cleanup.policy` unless the request declares a changelog use-case; write-caching (derived from `profile`, not free-form); segment sizing; tiered-storage upload flags; any `redpanda.*` broker-level property; ACL shapes outside the producer/consumer lists; principal names (derived `svc-<team>-<app>`, validated against the requesting repo's team).

**Exposed with bounds:** `partitions: int & >=1 & <=48` (see partition policy below); `retention` within `1h..30d` (longer ⇒ platform conversation, likely tiered storage); quotas up to a per-team ceiling; compatibility mode for the schema subject from an allowlist (`BACKWARD` default).

**Derived:** topic name prefix validated by regex against the team (`=~"^\(team)\\."`) — the namespacing convention from Vol 1 §8.4 enforced by unification, not review comments; CiliumNetworkPolicy allowing the listed principals' namespaces to the broker listener; monitoring resources (per-topic recording rules, lag SLO for declared consumer groups).

## How it lands

Two implementation options, choose deliberately:

1. **Operator `Topic` CRD** (`cluster.redpanda.com/v1alpha2` `Topic`): declarative, reconciled, status-reporting; the KubeVela component renders `Topic` objects and Flux applies them. This is the default recommendation — it keeps the entire chain declarative and lets drift detection work (someone `rpk topic alter-config`s by hand → the operator reverts it, which is exactly the behavior you want on a platform).
2. **Declarative tooling job** (a reconcile Job running `rpk`/console-config against a rendered desired-state file): more flexible for ACLs/users/quotas if the operator's CRD coverage lags those objects. **PROD:** check the operator's current coverage of users/ACLs/quotas as a day-one item; historically topics landed in CRDs first and security objects lagged, so many real installations run CRD-for-topics + a small reconciler-for-ACLs/users. Either way the principle holds: *one* writer per object class — never both the operator and a script owning the same resource, or you've built a drift-fight.

## Partition-count policy (encode it, don't advise it)

Partition count is the one knob tenants most want and most misuse. The physics: partitions cost controller state, per-shard Raft groups, file handles, and rebalance time — and **[Kafka-protocol]** *you can never shrink a topic's partition count* (§15.1). The policy the platform encodes:

- Default 6; ceiling 48 without a platform conversation.
- Sizing rule of thumb published to tenants: target partition count ≈ max(consumer parallelism you'll realistically deploy, sustained MiB/s ÷ 30) rounded up to a multiple of 3 — and when in doubt go *lower*, because adding partitions later is a one-line Git change [Kafka-protocol: with the key-distribution caveat — adding partitions changes key→partition mapping for new data; keyed topics that depend on per-key ordering should start with headroom] while removing them is a topic migration.
- The CUE bound is the enforcement; the doc is the explanation.

## Users, ACLs, credentials — the Vault analogy, explicitly

Mirror of StackGres Vol 2's credential section. The shape you know from Vault database secrets engines — *the platform issues, stores, delivers, and rotates; the app only ever reads its mount* — reproduced here:

- **Issuance:** the tenant's component declares principals; the reconcile path creates the SCRAM user with a generated password, writes it into the tenant namespace as a Secret (or, better, into Vault at `secret/<team>/redpanda/<principal>` with the app consuming via the established Vault delivery pattern), and applies prefixed ACLs binding the principal to the team's namespace.
- **Rotation:** SCRAM supports altering a user's credentials in place; rotation = write new password → update Vault/Secret → rolling-restart consumers/producers (or SIGHUP-style reload where the client supports it). Platform runs rotation on a schedule and on demand (offboarding, suspected leak). **PROD:** [Kafka-protocol] clients authenticate per-connection; existing connections survive a password change until they reconnect — rotation is therefore *not* instant revocation. Instant revocation = delete the user *and* bounce connections (or rely on short-lived mTLS if/when the SPIFFE path from Vol 1 §8.5 lands, which is the real fix).
- **Boundary statement, repeated deliberately:** **[PaaS team]** owns broker config, RF, quota defaults, ACL machinery, credential plumbing, the CUE definition itself. **[App team]** owns topic layout within their prefix, partition sizing within bounds, schema evolution, consumer-group hygiene, replay/idempotence handling. **[Shared]:** capacity forecasting (tenants declare growth; platform provisions), and lag SLOs (tenants declare targets; platform alerts on them).

# rpk Mastery

`rpk` is the owner's daily tool — one binary, talks to the Admin API and Kafka API, and is deliberately good at cluster operations that raw Kafka tooling makes painful. Run it from a toolbox pod with network-policy access to the admin port, or `kubectl exec` into a broker. The vocabulary to know cold, grouped by intent, with what-you're-looking-for notes:

**Cluster health & topology**

```
rpk cluster health
rpk cluster info                      # brokers, addresses, versions
rpk redpanda admin brokers list       # membership + status incl. maintenance/decommission state
rpk cluster partitions list --leaderless --disabled
rpk cluster partitions list -t <topic>
rpk cluster logdirs describe          # per-broker/per-partition disk usage
```

`cluster health` is your first command in every incident: it reports unhealthy reasons directly — leaderless partitions, under-replicated partitions, down brokers, and whether the *controller* has a leader (Vol 1 §3.5: if the controller line is bad, stop diagnosing topics and diagnose group 0). `--leaderless` listing tells you the blast radius in topic terms immediately.

**Topics & configs**

```
rpk topic list / describe <t> -p       # -p: per-partition leaders, replicas, offsets, high watermark
rpk topic create/delete
rpk topic add-partitions <t> -n <N>
rpk topic alter-config <t> --set retention.ms=...
rpk cluster config get/set <key>; rpk cluster config edit; rpk cluster config status
```

**PROD:** on this platform, mutating topic/cluster config by hand is for incidents only — the Git path owns desired state, and `cluster config status` tells you whether a config change requires broker restart to take effect (some do; the operator handles the choreography when driven declaratively).

**Consumer groups**

```
rpk group list
rpk group describe <g>                 # per-partition: current offset, log end, LAG, member/host
rpk group seek <g> --to start|end|<ts> # the replay/skip tool — with the app team, never unilaterally
```

`group describe` is the lag-triage workhorse: lag concentrated on *one* partition (hot key / stuck member) reads completely differently from lag uniform across partitions (throughput deficit or broker-side problem) — §13.3 builds the whole triage on this distinction.

**Maintenance & lifecycle**

```
rpk cluster maintenance enable <node-id> [--wait]
rpk cluster maintenance status
rpk cluster maintenance disable <node-id>
rpk redpanda admin brokers decommission <node-id>   # permanent removal: drains ALL replicas
rpk redpanda admin brokers decommission-status <id>
rpk redpanda admin brokers recommission <node-id>    # abort an in-flight decommission
rpk redpanda admin partitions transfer-leadership ...
```

The distinction to hold precisely, because confusing them is a resume-generating event: **maintenance mode** moves *leaderships* off a broker (replicas stay; cheap; reversible; for restarts/upgrades). **Decommission** moves *replicas* off (data movement; expensive; for permanent removal/node replacement). Decommissioning when you meant maintenance turns a 2-minute restart into hours of cluster-wide replication traffic. `--wait` on maintenance enable blocks until leadership drain completes — script your rolling operations around it.

**Diagnostics**

```
rpk debug bundle                       # the support artifact: metrics, logs, configs, kernel state
rpk cluster partitions move ...        # manual replica placement (rare; balancer usually owns this)
rpk cluster partitions balancer-status
```

**Host tuning (mostly bare-metal; see §12.4 for K8s applicability)**

```
rpk redpanda tune all / rpk redpanda tune list
rpk iotune                             # measures disk, writes io-config for the scheduler
```

# Monitoring and Alerting into VictoriaMetrics

## Endpoints and scrape strategy

Two Prometheus endpoints on the admin port: `/public_metrics` (curated, bounded cardinality, stable names — the fleet/alerting feed) and `/metrics` (internal, high-cardinality, per-shard detail — the deep-debugging feed). Platform stance: vmagent scrapes `/public_metrics` at 10–15s for everything; `/metrics` scraped at low frequency into a short-retention tenant or enabled ad hoc during incidents — Vol 1's shard model means `/metrics` is *labelled per shard*, which is exactly what you want at 3am and exactly what you don't want ingested 86,400 times a day per broker. Logs: broker stderr → Fluent Bit → VictoriaLogs with the standard platform pipeline; the log lines that matter most are Raft election/timeout messages, storage errors, and OOM/allocator warnings.

## The signals that page

| Signal | Metric basis | Why it pages |
|---|---|---|
| Leaderless partitions > 0 for 1m | `redpanda_cluster_` `unavailable_partitions_count` | Writes unavailable — quorum lost somewhere (Vol 1 §3.3) |
| Under-replicated partitions sustained | `redpanda_kafka_` `under_replicated_replicas` | One failure from unavailability; durability margin gone |
| Broker down / membership change | `redpanda_cluster_brokers` vs expected, up{} | Correlated-election event; start §13.2 |
| Controller leadership churn | `redpanda_raft_` `leadership_changes` (controller group) rate | Cluster-scoped coordination degraded (Vol 1 §3.5) |
| Storage: free space alert firing | `redpanda_storage_` `disk_free_space_alert` > 0 | Redpanda's own low/degraded space signal — §13.4 immediately |
| Produce/fetch p99/p999 breach | histogram `redpanda_kafka_` `request_latency_seconds` (produce/consume) | The product SLO on this platform; page on p999 sustained |
| Consumer lag SLO breach (declared groups) | `redpanda_kafka_` `max_offset` − group committed (or kminion-style exporter) | Tenant-facing; routed per §13.3 triage to the right owner |
| RPC/Raft request errors between brokers | internal RPC error rates | Fabric or peer problem before it becomes elections |

Explicit *noise* (ticket, don't page): transient leadership transfers during known maintenance; single-digit-second under-replication during rolling restarts; rebalance events; quota throttling firing (that's the control *working* — dashboard it per tenant, alert the *tenant's* channel, not the platform pager).

**The one-row-per-cluster fleet sentence** — the dashboard row must answer, at a glance: *"All N brokers up, controller stable, 0 leaderless / 0 under-replicated, disk ≥ X% free everywhere, produce p999 ≤ SLO, no tenant lag SLO burning."* Green on that row means don't open the drill-down. Every element of the sentence is one stat panel; anything not in the sentence lives in the drill-down dashboards (per-broker, per-topic, per-tenant, Raft internals).

**LATENCY:** alert on p999 with `histogram_quantile` over meaningful windows (1–5m) and *also* record p50 next to it — tail-only regressions (p50 flat, p999 up) point at jitter sources (CPU contention §12.4, storage sync outliers, elections); whole-distribution shifts point at load or capacity. That one comparison halves your differential immediately.

# Capacity and Performance Engineering

## Sizing: cores, shards, partitions, memory

Cores are the primary sizing unit because cores are shards (Vol 1 §2.3). Working numbers to start from, then benchmark:

- **Cores:** modern guidance is roughly up to ~1 GB/s of throughput per handful of cores on decent NVMe; for our scale, start brokers at 4–8 dedicated cores (Guaranteed QoS, integer values) and let the benchmark (§12.5) argue for more. More smaller brokers beats fewer huge ones for blast radius, up to the point where partition spread and replication fan-out say otherwise.
- **Partitions per shard:** be conservative: plan ≤1,000 partition *replicas* per core as a ceiling and stay well under it (a few hundred) for latency-sensitive clusters — every replica on a shard is a Raft state machine sharing that core's run queue. Cluster-wide partition budget = cores × density target; the CUE partition ceilings (§9.4) are this budget's enforcement arm.
- **Memory:** ≥2 GiB per core as the floor (Redpanda enforces a minimum per shard); memory divides evenly across shards, and each shard's slice must hold its partitions' in-memory state + cache. High partition density raises the per-core memory floor. Pod memory request = limit (Guaranteed), and leave the OS/container overhead *outside* Redpanda's `--memory` (the operator/chart handles the reservation split — verify it did).
- **Disk:** local retention × ingest rate × RF ÷ brokers, plus 25–30% headroom below the storage alert thresholds (§13.4). With tiered storage, size for local-retention cache + upload buffer, not total history.

## The autotuner, and how much survives Kubernetes

`rpk redpanda tune all` on bare metal reshapes the host: AIO event limits (`fs.aio-max-nr`), disk scheduler → `none`/noop for NVMe, disk nomerges, IRQ affinity pinned away from reactor cores (ideally onto dedicated housekeeping cores), NIC RSS/queue tuning, CPU governor → performance, disabling deep C-states, transparent hugepages, clocksource sanity (tsc), swappiness. It writes an io-config from `rpk iotune` measurements so Seastar's I/O scheduler knows the device's real concurrency/bandwidth.

On our stack the honest split is:

- **Container-applicable:** essentially only Redpanda's own flags (`--smp`, `--memory`, io-config). The tuner's host-level changes cannot be made from a pod meaningfully.
- **Node-level, [PaaS team]-owned, applied via the node provisioning path (Kubespray/Ansible role for the Redpanda node pool):** aio-max-nr, disk scheduler and nomerges udev rules, governor/C-states, THP, clocksource, swappiness, and IRQ affinity mapping IRQs off the cpuset ranges the kubelet hands to Redpanda pods. This is the same discipline as the sysctl work you did for Consul, promoted to a node-pool profile. **PROD:** encode it as configuration management, not a hand-run tuner — a replacement node that misses the profile is a silent tail-latency degradation that no Redpanda metric will name for you.
- **NUMA:** keep a broker's cpuset within one NUMA node (CPU Manager does this when topology manager policy is set appropriately); a broker spanning sockets pays cross-node memory latency on random shards — the worst kind of jitter, intermittent and unattributable.

## Benchmarking honestly

Rules first, tools second: **open-loop load** (arrival rate fixed independent of response latency — closed-loop load generators hide latency collapse by slowing down when the system does: coordinated omission), **percentiles not averages** (p50/p99/p999/max, plotted over time, never a single summary number), **measure at the client** (the SLO lives there), and change one variable per run. Tooling: `rpk` includes a workload generator adequate for smoke tests; for real profiles use the OpenMessaging benchmark framework with fixed-rate drivers, run from pods placed like real tenant clients (same fabric path, same listener). The runs that matter: (1) produce-latency ladder at increasing fixed rates until p999 breaks — that knee is your capacity number, not the max throughput; (2) same ladder during a rolling restart and during a broker kill — the *operational* p999 is the honest SLO; (3) fio `O_DIRECT` sync-write microbenchmark on the actual storage class first, before any Redpanda numbers, so storage-vs-Redpanda attribution is settled in advance (Vol 1 §5.3).

# Incident Runbooks

Format per StackGres Vol 2: symptom → diagnosis → resolution → escalation, then **Blast radius** and the **market-hours call**. First command in *every* scenario: `rpk cluster health`.

## 13.1 Leaderless / under-replicated partitions

**Symptom:** unavailable-partitions alert; producers timing out on specific topics; `rpk cluster health` unhealthy with leaderless list. **Diagnosis:** `rpk cluster partitions list --leaderless` → do the affected partitions share a broker set? Almost always yes → this is a broker-availability incident wearing partition clothing; go to §13.2 for the down broker(s). If brokers are all up but partitions are leaderless: check controller health (leadership churn metric) — a sick controller can't complete elections' bookkeeping; check for a partition whose *majority* of replicas sits on genuinely-dead-and-not-coming-back nodes. **Resolution:** restore quorum — recover any one broker of the affected pairs and elections complete in seconds unaided. If a majority of a partition's replicas are permanently lost, you are in data-decision territory: forcing a partition back with only a minority replica means accepting truncation of committed-but-unreplicated-to-survivor entries; that is a with-the-tenant, with-management decision, never a solo one. **Escalation:** immediately if leaderless persists >5m with all brokers apparently up (controller or bug territory), or the moment "permanently lost majority" enters the differential. **Blast radius:** writes down on affected partitions only; reads from followers of those partitions also unavailable (fetches serve from leaders). **Market-hours call:** restoring a crashed broker: do it now, any hour — it only adds a quorum member back. Force-recovery/truncation decisions: never intra-day unless the tenant declares the topic's unavailability worse than its truncation.

## 13.2 A broker down: crash vs slow death

**Symptom:** broker-down alert; correlated small latency blip as its leaderships failed over. **Diagnosis — the fork that decides everything:** *clean crash/kill* (pod restarting, node rebooted) vs *slow death* (broker up but degrading: storage errors in VictoriaLogs, climbing request latency, falling behind as a follower). Clean crash: `kubectl describe pod` / node status; let it return, watch `rpk cluster health` clear under-replication as it catches up, watch the leadership balancer restore its share over the following minutes. Slow death is the dangerous one because Raft keeps counting a slow-but-alive broker in quorums and clients keep talking to its leaderships: **immediately `rpk cluster maintenance enable <id> --wait`** to strip its leaderships (client pain stops now), then diagnose at leisure. **Resolution:** transient cause (node pressure, storage blip) → fix, disable maintenance, done. Node/hardware dead or storage lost → this is broker *replacement*: `decommission` the old ID (replicas drain to survivors — hours of background replication; watch `decommission-status` and cluster ingest headroom), then join a fresh broker on the replacement node. Recommission only aborts an *in-flight* decommission — it does not resurrect a completed one. **Escalation:** solo-safe: maintenance mode, restarting a crashed pod. Escalate before: decommission during market hours, anything touching a second broker while one is down (you are one mistake from §13.1), any storage-loss determination. **Blast radius:** one broker down = zero data unavailability by design; the cost is durability margin (RF3→2 on its partitions) and the failover blip. **Market-hours call:** maintenance-enable a sick broker any time — it strictly reduces client pain. Decommission's replication storm competes with production traffic: default to after-hours unless under-replication exposure across the trading day is judged worse.

## 13.3 Consumer group lag exploding — whose move is it

**Symptom:** tenant lag SLO alert or tenant complaint. **Diagnosis — the ownership triage, run in this order:** (1) `rpk group describe <g>`: lag on *all* partitions or *some*? (2) Broker side healthy? — cluster health clean, fetch p99 normal, no throttling on this principal (`quota` metrics): if fetch latency and cluster health are clean, the platform is likely *serving* fine and the app isn't *consuming* fine. (3) Uniform lag + healthy brokers + members present-and-stable → app throughput deficit: processing slower than ingest ([App team]: scale consumers up to ≤ partition count, or fix per-message cost). (4) Lag on specific partitions → hot key/skewed producer ([App team] data-model issue) or one stuck member/one slow broker hosting those leaders ([Shared]: check which). (5) Members churning in `group describe` (generation bumping, members appearing/vanishing) → rebalance storm / `max.poll.interval` death spiral (Vol 1 §6.2) → app-side, but platform's blessed-profile docs are the fix vector. (6) Throttling non-zero → they hit their fetch quota: working as designed; conversation, not incident. **Resolution:** platform-side only if (2) found broker degradation — then it's really §13.2/§13.5. Otherwise hand the tenant the specific finding, not a shrug: "lag is uniform, brokers clean, your 4 members each process ~X msg/s against ingest Y — you need ≥N members or faster handling." `rpk group seek` (skip/replay) is executed by platform *only* on tenant's written request. **Escalation:** to the tenant team lead when lag threatens *their* downstream SLOs; platform-internal only if broker-side degradation found. **Blast radius:** the group itself; broker-side, sustained massive fetch backlogs can pressure cache/disk-read paths — quotas (Vol 1 §8.3) exist precisely to cap this. **Market-hours call:** triage immediately always (cheap, read-only); seeks/resets during trading only with the owning team's explicit sign-off.

## 13.4 Disk / storage pressure

**Symptom:** `redpanda_storage_` `disk_free_space_alert` nonzero, or free-space trend alert; degraded mode blocks writes at the floor. **Diagnosis:** `rpk cluster logdirs describe` sorted → is growth broad (organic ingest > retention plan) or concentrated (one topic ballooning: retention misconfig, compaction not keeping up, a runaway producer — cross-check per-topic ingest and the principal's quota)? One broker only → replica skew or that node's disk shrank (other tenants of the node?). **Resolution ladder, mirroring StackGres 13.5:** (1) *Now:* identify the offending topic; with tenant, tighten `retention.ms/bytes` — reclamation is fast (segment deletion) — and/or quota-throttle the runaway producer. (2) *Short-term:* rebalance replicas off the pressured broker if skewed; expand PVC/node storage if the platform path allows. (3) *Structural:* tiered storage (Vol 1 §4.4) converts this whole class from data-loss threat to cache sizing; capacity-plan review with the tenant's declared growth (§9.5, [Shared]). **PROD:** never delete segment files by hand on a broker's disk; the storage layer's view must change via Redpanda (retention) or not at all. **Escalation:** solo-safe: retention tightening with tenant consent, quota application. Escalate: anything at the degraded/blocking threshold during market hours; PVC surgery. **Blast radius:** at the hard floor, writes block on the affected broker's partitions — effectively §13.1 with a countdown clock visible in advance. That visibility is the point of the trend alert: this incident should never actually happen; only its warning phase should. **Market-hours call:** retention tightening and quota throttles are safe and fast intra-day; expansion/rebalance waits.

## 13.5 Controller / Raft instability

**Symptom:** the weird one (Vol 1 §3.5): data paths mostly fine, but topic creation hangs, decommission stalls, leadership balancing stops, `rpk cluster health` slow or reporting no controller leader; controller-group leadership-change metric churning. **Diagnosis:** elections churn for Consul-familiar reasons — a flapping broker (up-down-up cycling through §13.2 without settling: check restart counts), network partition between broker subsets (Cilium/BGP health, node connectivity — coordinate with the CNI runbook), or overload starving the controller's shard (extreme: check shard-level `/metrics` on the controller leader's shard 0). **Resolution:** remove the flapper (maintenance mode / cordon its participation) and elections settle in seconds; heal the partition; if overload, reduce the metadata operation storm (bulk topic creation scripts are the classic trigger — the self-service path should batch/rate-limit topic operations for exactly this reason). **Escalation:** early — controller instability is rare, cluster-scoped, and the precursor state to worse; get a second pair of eyes at 15 minutes unresolved. **Blast radius:** cluster-scoped *administrative* unavailability; established data paths continue, which buys you calm — use it. **Market-hours call:** stabilization actions (removing a flapper) immediately; do not run *any* elective admin operation (topic creation, moves, config changes) until the controller is stable for 10+ minutes.

## 13.6 Rolling restart gone wrong mid-flight

**Symptom:** upgrade/config rollout stuck: broker N won't rejoin healthy, or the operator paused, and you're mid-fleet on mixed versions/configs. **Diagnosis:** freeze first — the standing rule mid-roll is *never let a second broker go down while one is unhealthy* (quorum math: one down is designed-for; two down leaderless-es every partition sharing those two). Then classify broker N's failure: config rejection at startup (logs: it never comes up → the *change* is bad → halt rollout, revert the change via the Git path, let N restart on old config); resource/scheduling (pending pod, cpuset/PVC issues → K8s-side fix); version-specific crash (came up then died → §14's rollback reality decides options). **Resolution:** stuck-but-mixed is *stable* — Redpanda tolerates mixed versions mid-upgrade indefinitely-enough to think; resolve N fully (healthy, caught up, under-replication zero, leadership share restored) before resuming, whether resuming forward or rolling back. If N is genuinely dead (hardware), you're in §13.2 broker-replacement *while* mid-upgrade: replace at the *old* version first, finish the fleet state coherently, then resume. **Escalation:** immediately on "second broker unhealthy," and before any decision to roll forward past a crash. **Blast radius:** mid-flight per se: none beyond the one broker; the risk is entirely in the next action taken hastily. **Market-hours call:** freezing is always safe. Everything else about this scenario is why §14 says fleet-wide rolls happen in windows.


# Upgrades and Maintenance

## The rolling upgrade procedure

The choreography, one broker at a time, whether the operator drives it or you do:

1. Preconditions: `rpk cluster health` fully green; zero under-replicated; controller stable; no decommission in flight; recent debug bundle stashed; release notes read for the *target and every skipped patch*.
2. Per broker: `rpk cluster maintenance enable <id> --wait` (leadership drains — this is the step that makes the restart client-invisible; Vol 1 §3.4) → restart on the new version → wait for: broker healthy, caught up (under-replication back to zero), *then* `maintenance disable` → let the leadership balancer restore its share → observe produce p999 for a settle period → next broker.
3. After the last broker: the cluster's **logical version** advances and gated **feature flags** for the new version activate (some auto-enable, some require explicit enablement). This activation is the durability point-of-no-return below.

Operator-driven vs manual: the operator performs exactly this choreography on `spec` image change and is the default path — but it gates on broker health, not on your produce-p999 dashboard, so for market-sensitive clusters run it in a window *and watch*, or drive manually with explicit per-broker gates. **PROD:** version rules [Redpanda-specific]: upgrade one feature release at a time (e.g. 24.2 → 24.3 → 25.1, never skipping a feature version; patches within a release are free), and never begin an upgrade on a cluster that isn't fully healthy — mixed-version tolerance is designed for transit, not residence.

## Rollback reality

Honest statement: **rollback is only clean before new-version feature flags activate.** Mid-roll (mixed versions, features not yet enabled): roll the upgraded brokers back one at a time with the same maintenance choreography — supported and fine. After completion + feature activation: on-disk/metadata formats may have moved; downgrade is somewhere between unsupported and impossible depending on the features involved. Therefore: treat the feature-activation step as a deliberate gate, not an automatic afterthought — upgrade the binaries, soak on the new version with features un-activated for a defined period if the release allows it, *then* activate. Your real rollback strategy after that point is restore-shaped (tiered-storage remote recovery if enabled, or forward-fix), which is one more argument for §4.4's tiered storage.

## What's safe intra-day vs windowed

- **Any hour, no ceremony:** maintenance-enable/disable one broker; leadership transfers; retention/quota changes with tenant consent; adding partitions to a topic (tenant-initiated via Git); adding ACLs/users.
- **Any hour, with care and a second person:** single-broker restart (maintenance choreography) for an urgent fix; single-broker replacement *start* (the decommission replication load is the concern — throttle-aware).
- **Windowed, always:** fleet-wide rolling restarts/upgrades; feature-flag activation; partition rebalancing campaigns; cluster expansion's rebalance phase; storage class or listener/advertised-address changes (client-visible by construction).
- **Never during trading, full stop:** anything from §13.1's force-recovery family; authorization-mode flips; changes to the fsync/write-caching posture of durable topics.

## Expansion and rebalancing as planned operations

Adding brokers: join is trivial (StatefulSet scale-up via the CRD); the *rebalance* to move replicas onto the new capacity is the real operation — continuous balancing (where licensed/enabled) or explicit `rpk cluster partitions move` plans. Replication traffic competes with production ingest: schedule, watch produce p999 as the canary, and use the balancer's throttles. Same discipline for planned decommissions. **[PaaS team]** owns all placement; tenants never see it.

# Sharp Edges and Gotchas

**15.1 Partition-count regret.** [Kafka-protocol] Partition count only goes up. Adding partitions also remaps keys (new data only) — a keyed topic relying on per-key ordering across the change will interleave old-key-order and new-key-order histories. The platform's ceilings-and-defaults policy (§9.4) exists because of this asymmetry; the migration for "too many/too few, keyed" is a new topic + dual-write/cutover, which is a project, not a config change.

**15.2 Shard imbalance.** [Redpanda-specific] Partitions are balanced across brokers *and* shards, but hot partitions are not equal partitions: one firehose partition pins one core while its 7 siblings idle, and broker-level CPU graphs average it away into invisibility. Symptom: p999 elevated on a subset of topic-partitions with unremarkable broker CPU. Diagnosis: per-shard `/metrics` (reactor utilization per shard). Fix vector: more partitions on the hot topic (producer keying permitting) to spread the load, or accept and isolate. This is the observability argument for keeping the internal metrics endpoint reachable in anger.

**15.3 Direct I/O on networked storage.** Vol 1 §5.3's warning, restated as the symptom you'll actually see: fsync-latency outliers on the storage backend translate *directly* into produce p999 spikes with healthy CPU, healthy network, healthy everything-Redpanda — because the ack path ends at the device (Vol 1 §4.3). If brokers ever run on Weka volumes, put a storage-side sync-latency panel *on the Redpanda dashboard row* so the correlation is one glance, not one hour.

**15.4 Advertised-address misconfiguration.** The maddening signature: `rpk cluster info` fine from inside, bootstrap connection succeeds from the client, then per-partition operations time out or pile into `NOT_LEADER`/connection-refused loops — because metadata handed the client addresses it can't reach (Vol 1 §6.1). Debug ritual: from the *client's* network position, resolve and dial every address returned in metadata (`rpk cluster info -X brokers=<bootstrap>` from a pod in the client's namespace, then `nc` each advertised host:port). Any listener change is a client-visible change and belongs in the windowed category (§14.3).

**15.5 Consumer rebalance storms.** Eager-protocol groups + K8s rolling deploys of many replicas = revoke-all/reassign-all per pod cycle; with slow `max.poll` handling it self-sustains (Vol 1 §6.2). Platform fix vectors, in order: cooperative assignor in the blessed profile, static membership for K8s consumers, `maxSurge/maxUnavailable` tuned so consumer deploys move in small steps. Detection: group generation counter climbing fast in `group describe` while lag rises.

**15.6 Tiered-storage edges.** First historical read after long quiet = object-store fetch latency cliff (document it to tenants: "reads older than local retention are seconds-to-first-byte, by design"); cache sizing too small for a big backfill = thrash between object store and cache; upload lag under sustained peak ingest quietly grows *local* disk usage beyond local-retention targets (alert on upload lag, not just disk); bucket lifecycle/permissions changes made storage-side can break uploads silently until the lag alert fires. And remote recovery is a *restore procedure* — rehearse it before believing it (same rule as StackGres backups).

**15.7 Where Kafka compatibility is imperfect.** The protocol coverage is broad and mainstream clients (Java, librdkafka lineage) just work, but assume nothing for tooling that pokes at internals: anything that reads ZooKeeper (ancient tools) is dead on arrival; tools that assume broker JMX metrics, Kafka's exact config-key namespace, log-dir file layouts, or Cruise-Control-style rebalancing APIs will misbehave — Redpanda has its own balancer and Admin API instead. Some Kafka broker configs accepted for compatibility are silently mapped or ignored (check `rpk topic describe`'s *effective* config rather than trusting what a tool set). Ecosystem stances: Kafka Connect runs fine *against* Redpanda but is a separate deployable you'd own — decide deliberately whether the platform offers it; Kafka Streams apps work [Kafka-protocol] including their transactions/EOS dependency (supported, with the §6.1 latency caveats). When a tenant reports "Kafka tool X acts weird," the first question is which API family it's touching: protocol (should work — investigate) vs operational internals (unsupported by design — redirect to the platform's equivalent).

# Walked Incident Scenarios

## 16.1 Broker failure: node dies at 09:40

*You see:* broker-down page for `redpanda-3`; 20–30 tenant produce-latency blips at 09:40:12 that self-cleared; under-replicated count now steady at ~340. *Check, in order:* (1) `rpk cluster health` — one broker down, no leaderless: quorum intact everywhere; breathe. (2) `kubectl get pod/node` — node NotReady, hardware event per node logs. (3) Under-replication *steady* (not shrinking — its replicas have no source of recovery until it returns) and leadership blip already absorbed: clients are fine *now*; confirm on produce p999 panel. (4) Judgment fork, and this is the actual decision of the incident: node likely back soon (reboot) → wait; broker rejoins, catches up from peers, under-replication drains to zero, balancer restores leadership — total platform action: watching. Node dead-dead → you're at RF2 exposure on 340 partitions for the duration: decommission now (hours of replication during trading, produce-p999 risk, watch throttles) vs at close (hours of RF2 exposure). Default: if hardware ETA > close-of-day and ingest headroom is comfortable, decommission now with balancer throttles on and p999 on-screen; otherwise ride to the window. Either way say the decision and its exposure out loud in the incident channel — the durability-margin call is exactly the kind of thing the platform states rather than silently assumes. *Resolution:* replacement broker joins post-decommission; verify health green, balanced leadership, under-replication zero, and close with the durability-exposure interval documented.

## 16.2 Client-visible latency: "produces got slow at 14:00"

*You see:* tenant reports produce p99 up 8× since 14:00 on `alpha.md.*`; platform p999 panel confirms tail-only elevation (p50 flat) on two brokers. *Check, in order:* (1) `rpk cluster health` — green: not an availability event; this is a jitter hunt (§11's p50-vs-p999 split just paid for itself). (2) Scope: only partitions led by brokers 1 and 4 → what do those two share? (3) The usual suspects in fixed order: *storage* (per-broker fsync/IO latency panels — a Weka-neighbor effect if on shared storage; local-disk latency outlier if not), *CPU* (were the pods' cpusets disturbed? node-level: did something land on the Redpanda nodes at 14:00 — a daemonset rollout, a burst workload, IRQ storm on reactor cores? `kubectl describe node` events + per-shard reactor utilization), *network* (Cilium/BGP events, drops on those nodes' NICs), *neighbor within Redpanda* (per-shard metrics: one hot shard on each — did a tenant's 14:00 job start hammering one partition co-located on both? §15.2). (4) In this scenario the classic finding is (storage) or (a 14:00 cron/deploy landed load on those nodes). *Resolve:* evict/repel the noisy neighbor (node isolation is [PaaS team] turf), or maintenance-enable the affected brokers one at a time to move leaders off while the node-level cause is fixed — client pain stops at leadership transfer, minutes into the incident, *before* root cause is fully closed. That ordering — stop the pain via leadership movement, then diagnose the broker at leisure — is the single most reusable move in this booklet (§13.2). *Market-hours:* everything above is safe intra-day.

## 16.3 Capacity: disk trend alert on Friday

*You see:* trend alert projecting broker disks hit the storage-alert threshold in ~6 days; growth started Tuesday. *Check, in order:* (1) `rpk cluster logdirs describe` sorted → growth concentrated in `beta.sim.replays.v1`. (2) Per-topic ingest panel: stepped 5× on Tuesday; retention is 14d — so steady state lands ~5× the topic's disk plan and the alert did math for you early. (3) Principal's produce quota: they're within quota — this is legitimate growth someone forgot to declare, not a runaway (different conversation, same math). *Resolve:* with team beta, in preference order — (a) they confirm the new rate is permanent and actually needs 14d → capacity change: expand storage / add a broker, as a planned §14.4 operation next window, and update their declared-growth record; (b) 14d was aspirational → tighten retention to what replays truly need; reclamation is immediate; (c) medium-term either way: this topic is the poster child for tiered storage — long retention, replay access pattern, latency-insensitive history (Vol 1 §4.4) — feed it into that evaluation. *Close* by fixing the process gap, not just the disk: growth-declaration in the §9 request flow is [Shared] ownership; the alert working ≠ the planning working. *Market-hours:* all diagnosis and the retention conversation are safe any time; expansion is windowed.

\newpage

# Appendix: Day-One Question List

Carried forward from both volumes, in the order they'll come up:

1. Where does Redpanda run today (in-cluster vs bare metal), and on what storage class? If Weka: does anyone have fio `O_DIRECT` sync-write numbers for it?
2. Is there a blessed internal S3 endpoint, and is tiered storage enabled anywhere?
3. Operator version and its current CRD coverage: topics only, or users/ACLs/quotas too? What owns the objects the CRDs don't?
4. Which listeners exist, with what auth, and what do external clients actually dial (LB-IPAM VIPs? something else?) — get one metadata response from a real client's network position and read the advertised addresses.
5. Is `kafka_enable_authorization` on, and are quotas applied per tenant principal today?
6. Any SPIRE↔Redpanda glue in place or planned; what CA signs listener certs?
7. Kubelet CPU manager policy on the Redpanda node pool, and are broker pods Guaranteed with integer CPUs? Node tuning profile applied via which Kubespray/Ansible role?
8. Which metrics endpoint feeds VictoriaMetrics, at what interval, and does the paging set match §11's table? Where do controller-churn and storage-space alerts route?
9. What's the declared upgrade cadence, and has feature-flag activation ever been treated as a separate gate?
10. Has remote recovery (or any restore path) ever been rehearsed?
