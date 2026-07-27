---
title: "Redpanda for the PaaS Stack"
subtitle: "Volume 1 — Architecture & Concepts"
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

# How to Read This Booklet

This is Volume 1 of a two-volume owner's reference for Redpanda on our platform. It assumes working knowledge of Kafka's *client-side* concepts — topics, partitions, consumer groups, offsets — and assumes zero prior experience *operating* Kafka or Redpanda. Everything operational is taught from the ground up. Volume 1 covers architecture and concepts; Volume 2 is the operations and self-service playbook.

Conventions, matching the Cilium and StackGres volumes:

- **[Kafka-protocol]** marks behavior that is part of the Kafka wire protocol and client contract — anything a Kafka client library, tenant application, or ecosystem tool depends on. It would behave the same against Apache Kafka.
- **[Redpanda-specific]** marks behavior that is Redpanda's implementation choice — where the operator mental model diverges from Kafka, or where Kafka-ecosystem folklore actively misleads.
- **[PaaS team]** / **[App team]** / **[Shared]** mark ownership boundaries. These are repeated throughout rather than confined to one section, deliberately.
- **PROD:** callouts mark sharp edges that have bitten real production environments.
- **LATENCY:** callouts mark decisions that move the tail (p99/p999), because on this platform the tail is the product.

The Consul analogy is used hard throughout, because it is genuinely the right one: Redpanda is best understood as *a fleet of thousands of tiny Consul-like Raft clusters sharing a set of processes*, plus one special Raft cluster that plays the role Consul's own catalog plays. If you keep your Consul mental model of leaders, terms, quorum commit, and leadership transfer in working memory while reading, most of Redpanda's behavior becomes predictable rather than memorized.

# Why Redpanda Exists

## The architecture it replaces

Apache Kafka is a JVM application from 2011 whose performance model is built on two big bets: the OS page cache and sequential disk I/O. A Kafka broker writes messages to log segment files *without* fsyncing them on the hot path by default; durability comes from replication, and read performance comes from the kernel keeping recently written segments in the page cache so that consumers reading "from the tip" never touch disk. Metadata coordination historically lived in ZooKeeper — an external Raft-ish (ZAB) consensus system — and in modern Kafka lives in KRaft, an internal Raft quorum of controller nodes. Data replication, however, is *not* Raft: Kafka uses its own primary-backup protocol built around the ISR (in-sync replica set), which we will contrast carefully in §3.2 because the failure semantics differ in ways an operator must internalize.

This architecture works, and works at enormous scale, but it has costs that matter disproportionately in a latency-sensitive environment:

The JVM brings garbage collection pauses. However well-tuned, GC introduces multi-millisecond stalls that land directly in the produce/fetch tail. p50 looks fine; p999 tells the truth. Kafka tail latency work is substantially GC-and-page-cache archaeology.

The page-cache dependence means broker performance is entangled with kernel memory management. A consumer doing a historical backfill read evicts hot data from the page cache and degrades producers — a classic Kafka incident pattern. The broker cannot reason about its own caching because the kernel owns it.

The thread model is conventional: pools of network threads, I/O threads, and request handler threads, with locks and cross-core data movement between them. Every lock and every cache-line bounce is variance, and variance is tail latency.

Operationally, Kafka is a *system of systems*: brokers + ZooKeeper/KRaft controllers + Schema Registry + REST proxy, each a separate deployable with its own lifecycle.

## What Redpanda changes

**[Redpanda-specific]** Redpanda is a from-scratch reimplementation of the Kafka protocol in C++ on the Seastar framework — the same framework underneath ScyllaDB. One statically linked binary contains the Kafka API, the Raft consensus implementation, the storage engine, Schema Registry, an HTTP proxy (pandaproxy), and an Admin API. There is no JVM, no ZooKeeper, no external controller quorum, and — this is the load-bearing difference — no reliance on the OS page cache.

The architectural pillars:

**Thread-per-core (shard) execution.** Seastar pins one OS thread to each allocated CPU core and calls that pairing a *shard*. Each shard runs its own cooperative userspace scheduler, owns its own slice of memory (allocated up front and split evenly across shards), and owns its own I/O queues. Shards do not share mutable state and do not take locks against each other; when shard 3 needs something shard 7 owns, it sends an asynchronous message and continues. Every partition replica hosted on a broker is owned by exactly one shard: all Raft work, all storage I/O, and all Kafka request handling for that partition happens on that one core. This is shared-nothing at the core level, the same discipline that makes a well-partitioned distributed system fast, applied *inside* one machine.

**Direct I/O with self-managed caching.** Redpanda opens log files with `O_DIRECT` (DMA), bypassing the page cache entirely, and manages its own read-ahead and caching inside its own memory allocation. The kernel never gets to make caching decisions on Redpanda's behalf, which removes an entire category of Kafka performance mystery — but also invalidates an entire category of Kafka capacity folklore, covered in §4.

**Raft for everything.** Cluster metadata and data replication both use the same Raft implementation. §3 is dedicated to this.

## What thread-per-core buys, and what it demands

What it buys is *variance elimination*. In a conventional threaded server, a request's latency depends on which threads it bounces across, what locks it contends on, which cores' caches it pollutes, and what the kernel scheduler decides. Under load these effects compound in the tail. In the shard model, a produce request for partition P lands on P's shard, is processed run-to-completion by cooperative tasks with no lock contention and no involuntary migration, and the data structures it touches are already hot in that core's cache. The p999 story of Redpanda is not "faster code" so much as *fewer sources of jitter*. This is the same instinct as HFT userspace networking: own the resource, exclude the arbitrator.

What it demands is *honest, exclusive CPU*. The entire model assumes each Seastar reactor thread genuinely owns a physical core. The reactor busy-polls at low utilization sleeps aside — it is designed to be the only significant tenant of its core. Three ways Kubernetes defaults break this:

1. **Fractional or shared CPUs.** With CFS shares/quota and a non-`Guaranteed` pod, the reactor thread gets time-sliced against neighbors. Every involuntary preemption freezes a shard mid-task; every partition on that shard sees the pause as latency. Worse, CFS *quota* (limits) introduces throttle periods: the reactor burns its quota polling, gets throttled for the remainder of the 100ms period, and you see periodic latency spikes with a suspiciously regular cadence. This is the single most common self-inflicted Redpanda-on-K8s wound.
2. **Core migration.** If the kubelet doesn't pin the pod, the scheduler may migrate reactor threads across cores, destroying cache locality and, on multi-socket hosts, silently crossing NUMA boundaries.
3. **SMT sibling pollution.** A reactor sharing a physical core with a hyperthread sibling running someone else's workload shares L1/L2 and execution ports with it. For strict tail-latency work, treat SMT siblings as *not free*.

**PROD:** The platform answer is: kubelet CPU Manager `static` policy on Redpanda nodes, Redpanda pods in **Guaranteed QoS** (requests == limits, integer CPU values, same for memory), so the kubelet grants exclusive cpusets. You already run this pattern of reasoning for latency-critical trading workloads; Redpanda belongs in the same class. Verify with `kubectl exec` → `cat /sys/fs/cgroup/cpuset.cpus` and confirm Redpanda was started with `--smp <N>` matching the integer request. If `--smp` exceeds the exclusive cpuset, shards time-share cores and you get the fractional-CPU pathology with extra steps. **[PaaS team]** owns node pool configuration, CPU manager policy, and the resource classes; **[App team]** never touches broker resources.

**LATENCY:** `--smp` (core count) is also your *shard count*, and shard count determines partition-per-shard density and per-shard Raft workload. Sizing cores is not just throughput math; it is a tail-latency and metadata-scalability decision. Volume 2 §12 covers sizing.

The Consul contrast is instructive here: Consul is also a single Go binary with Raft inside, but it is a conventional goroutine-scheduled program — the Go runtime multiplexes thousands of goroutines over a few OS threads and the kernel schedules those. Consul accepts scheduler jitter as a cost of programming model simplicity, which is fine for a control plane doing thousands of RPCs per second. Redpanda is a *data* plane doing millions of operations per second where jitter is the enemy, so it inverts the model: the application becomes the scheduler.

# Raft Everywhere

This section is written directly against your Consul depth. Consul gave you: a single Raft group; a leader elected per term; writes committed at quorum (majority); followers streaming AppendEntries; leadership transfer as a graceful operation distinct from failure-driven election; and the operational reflexes around `last_contact`, election storms, and quorum loss. Redpanda takes that exact machinery and instantiates it *hundreds or thousands of times per cluster*.

## Two kinds of Raft groups

**The controller Raft group** (internally group 0) is the ZooKeeper/KRaft replacement, and it is the closest thing to "your Consul cluster" inside Redpanda. Every broker participates as a voter (in large clusters, a bounded set of voters with the rest learning, but on our cluster sizes: every broker votes). Its replicated log is the cluster's metadata history: topic creation/deletion and configuration, partition assignments to brokers/shards, broker membership and lifecycle (joining, active, in maintenance, decommissioning), users, ACLs, security settings, feature flags, and licensing. Every broker applies this log to build an identical in-memory view of cluster state — exactly like Consul agents materializing the catalog from the Raft log. The elected *controller leader* is the broker that performs cluster-scoped coordination: creating partition Raft groups when a topic is created, orchestrating partition movement, driving decommission, and running the partition leadership balancer.

**Per-partition Raft groups** are the data plane and the ISR replacement. Every partition with replication factor 3 is its own three-member Raft group; its replicas are the group members; its leader is the Raft leader; the followers replicate via AppendEntries. A 1,000-partition, RF=3 cluster is 1,001 concurrent Raft groups (1,000 data + 1 controller). Each broker is simultaneously a leader in some groups and a follower in others, and each *shard* on each broker runs the Raft state machines for the partitions it owns.

This is the mental model shift from Consul: stop thinking "the cluster has a leader." The cluster has a *controller leader* (metadata) and *thousands of partition leaders* (data), and they move independently. "Who is the leader?" is always "of which group?"

Contrast with Patroni/StackGres, which you know well: Patroni is one leader per Postgres cluster, full stop, because Postgres replicates one monolithic WAL. Redpanda is what you get when the unit of leadership shrinks from "the database" to "one partition": leadership becomes cheap, plentiful, and spreadable, and a broker failure triggers not one failover but hundreds of small, independent, sub-second ones. Blast radius per election shrinks correspondingly — but *correlated* elections (a broker death fails over every group it led) become the operational event to reason about.

## The write path as Raft quorum commit

**[Kafka-protocol]** A producer sends `acks=all` (with `min.insync.replicas` folklore attached from the Kafka world). **[Redpanda-specific]** What Redpanda actually does with it: the produce request lands on the partition leader's shard; the leader appends the batch to its local log and dispatches AppendEntries to followers; when a *majority* of the Raft group (leader included) has the entry durable, the entry is committed and the ack returns. `acks=all` in Redpanda *is* Raft quorum commit — the same rule that governed every Consul KV write you've ever made. For RF=3 that means leader + 1 follower; the slowest replica is *not* on the ack path. `acks=1` returns after the leader's local append only (data not yet quorum-committed — a leader failure can lose it, exactly like acknowledging a Consul write before Raft commit would); `acks=0` is fire-and-forget.

**LATENCY:** quorum-of-3 commit means your produce p99 is gated by the *median* replica's network+disk latency, not the max. This is a genuinely better tail story than "wait for all ISR members" under one slow disk. It is also why one broker with degraded storage hurts less than you'd fear as a follower — and catastrophically as a leader. Triage reflex: "is the slow broker leading the affected partitions?" (Volume 2 §13).

## ISR vs Raft: the failure-semantics contrast that matters

This is the deepest [Redpanda-specific] divergence and worth internalizing precisely, because Kafka operational folklore will otherwise mislead you.

**Kafka's model:** the leader appends; followers fetch; the ISR is the set of replicas "caught up within `replica.lag.time.max.ms`." `acks=all` waits for *all current ISR members*. The trap: the ISR can *shrink*. Under sustained follower lag, Kafka ejects followers from the ISR until — with `min.insync.replicas=1` or a misconfiguration — the ISR is just the leader, and `acks=all` degrades to `acks=1` while still reporting success. Combine with `unclean.leader.election.enable=true` and Kafka can elect a stale replica and silently truncate acknowledged data. Every experienced Kafka operator has a scar from some corner of this. Kafka's availability/durability dial is smooth, adjustable, and possible to set to "lying."

**Redpanda's model:** there is no ISR and no dial. Quorum is quorum. With RF=3, an acknowledged write is on ≥2 replicas, always; lose any one broker and the data survives and a new leader (necessarily holding all committed entries — the Raft election rule you know from Consul) takes over in sub-second time. Lose *two* of three and the partition is **leaderless and unavailable for writes** — it will not degrade to single-replica acking the way a shrunk-ISR Kafka partition will. Redpanda chooses CP where Kafka lets you drift toward AP without noticing. Operationally: Kafka fails "dirty but available"; Redpanda fails "clean but unavailable." On a trading platform, the Redpanda behavior is the one you want — silent acknowledged-data loss is strictly worse than a loud unavailable partition — but you must *staff* for it: leaderless partitions are a page, not a curiosity (Volume 2 §11, §13.1).

There is no `min.insync.replicas` knob doing real work, no unclean leader election to disable, no ISR-shrink alerts to tune. Entire pages of the Kafka runbook canon simply don't exist here. **[Shared]** understanding: tenant teams reading Kafka docs must be told which durability folklore does not apply — this belongs in the platform's client-facing documentation.

## Elections, leadership transfer, and balancing

Failure-driven elections work exactly as in Consul: followers miss heartbeats for an election timeout, a candidate solicits votes for a new term, majority wins, and the winner is guaranteed to hold every committed entry. Expect low hundreds of milliseconds to ~1.5s of write unavailability for the affected partitions — but *only* those partitions. **[Kafka-protocol]** Clients see `NOT_LEADER_FOR_PARTITION`, refresh metadata, and retry against the new leader; well-configured clients absorb this invisibly (client tuning: Volume 2 §9's blessed profiles).

Graceful **leadership transfer** is Consul's `consul operator raft transfer-leader` writ large: the current leader hands off to a chosen up-to-date follower with no election timeout and no unavailability window worth measuring. This primitive is what makes maintenance civilized — **maintenance mode** (Volume 2 §10, §14) is essentially "transfer every leadership this broker holds, then stop giving it new ones," and it is the difference between a rolling restart being a non-event and being a market-hours incident.

The **partition leadership balancer** runs on the controller leader and continuously spreads leadership evenly across brokers (leadership = work: leaders serve all produces and, by default, all fetches). After a broker returns from restart it holds zero leaderships; the balancer transfers a fair share back gradually. **PROD:** the transient leadership skew after restarts is normal; leadership skew that *persists* is a signal (shard imbalance, a broker the balancer considers unhealthy, or a stuck controller — Volume 2 §13.5).

**PROD:** The controller group deserves Consul-grade respect. Data-partition Raft churn hurts the affected topics; *controller* instability (leader elections churning on group 0) degrades everything cluster-scoped — topic creation, partition movement, the leadership balancer, membership changes — while, notably, already-established data paths keep working. That split-brain of symptoms ("cluster seems fine but nothing administrative works") is the controller-instability signature. Watch `redpanda_raft_leadership_changes` filtered to the controller group as its own alert (Volume 2 §11).

# Storage Engine and the Data Path

## Log structure

Per partition, per shard, Redpanda maintains a log of **segments** — append-only files rolled at a size (`log_segment_size`, default 128MiB) or age (`log_segment_ms`) threshold — with accompanying index files mapping offsets and timestamps to file positions. So far this is structurally the same as Kafka and your intuition holds: sequential appends, immutable closed segments, deletion by dropping whole segments from the tail of the retention window.

The divergence is *how bytes reach those files*.

## Direct I/O: unlearning Kafka's page-cache religion

**[Redpanda-specific]** Redpanda opens segments with `O_DIRECT`. Writes are DMA'd from Redpanda's own aligned buffers to the device, bypassing the kernel page cache; reads likewise. Caching of hot data (the "tip" of each log that tailing consumers read) happens in Redpanda's own memory, in a unified cache it manages per shard with full knowledge of access patterns.

Kafka intuitions this invalidates — worth listing explicitly because Kafka-trained colleagues and Kafka-era blog posts will assert them confidently:

- *"Leave most of the machine's RAM free for page cache."* Meaningless here. Free host RAM does nothing for Redpanda. What matters is the memory *given to Redpanda* (`--memory`), which it carves per shard for its cache, Raft state, and buffers. On K8s: the pod memory request/limit is the capacity plan, not the node's free RAM.
- *"Watch page cache hit rates / use `cachestat` to debug broker reads."* The kernel-side observability story is empty by design. Cache behavior is visible only through Redpanda's own metrics.
- *"A backfilling consumer will evict hot data and hurt producers."* Much weaker here: Redpanda's cache is shard-local and self-managed, and historical reads (especially tiered-storage reads, §4.4) run through their own paths and byte-rate governors rather than fighting producers for kernel cache.

**PROD:** `O_DIRECT` also changes the *storage requirements* conversation: filesystems and volumes must support direct I/O well. Redpanda's blessed filesystem is **XFS** on a block device; ext4 works but XFS is the tested-hardest path. This becomes central to the Weka discussion in §5.3, because "does the CSI-mounted filesystem support `O_DIRECT` with good alignment behavior" is now a first-order question rather than trivia. If direct I/O is unavailable, Redpanda can fall back to buffered I/O — at which point you are running a system whose caching design assumes the page cache doesn't exist, on top of the page cache, and its performance model quietly becomes fiction.

## fsync, acks, and write caching

Here is a genuinely important **[Redpanda-specific]** durability difference in the *other* direction from what you might expect. Kafka, by default, does **not** fsync on the produce path — acknowledged data may live only in page cache on every replica, and a correlated power loss can lose acknowledged writes; Kafka's durability is replication-shaped, not fsync-shaped. Redpanda's default is the stricter contract: for `acks=all`, each replica fsyncs the batch to its device **before** counting toward quorum. An acknowledged write is on durable media on a majority of replicas. This is the correct default for anything that matters, and on our platform it stays on for anything resembling order/execution/risk data.

The escape hatch is **write caching** (`write_caching_default`, per-topic overridable): replicas ack after append-to-memory, fsyncing in the background on a size/time cadence (`raft_replica_max_pending_flush_bytes` / `raft_replica_max_flush_delay_ms`). This trades a bounded correlated-crash window (majority of replicas losing power within the flush interval) for removing the fsync from the ack path — which, on storage with nontrivial sync latency, is *the* dominant produce-latency term.

**LATENCY:** produce p99 with default fsync is approximately `network RTT to median follower + median fsync latency`, so device sync latency is on the critical path of every acknowledged write. On local enterprise NVMe this is tens of microseconds and you don't think about it. On networked storage it is the number to measure *first* (§5.3). Write caching is the platform's documented lever for latency-critical-but-loss-tolerant streams (e.g. telemetry, ticks that can be regenerated upstream) — the two blessed producer profiles in Volume 2 §9 encode exactly this split. **[PaaS team]** owns the default (leave strict); **[App team]** opts specific topics into write caching with eyes open, via a CUE knob the platform exposes deliberately.

## Retention, compaction, tiered storage

**Retention** [Kafka-protocol in semantics]: `retention.ms` / `retention.bytes` per topic; enforcement drops whole closed segments. **Compaction** (`cleanup.policy=compact`) retains the latest value per key — same semantics you know, used by the same kinds of changelog/table topics. **PROD:** compacted topics keep at least the active segment uncompacted, and compaction is per-shard background work; a huge hot compacted topic concentrates compaction load on the shards owning it.

**Tiered Storage (Shadow Indexing)** is Redpanda's local-disk/object-storage split, and architecturally it is the most consequential storage feature:

- Closed segments are uploaded to an S3-compatible object store asynchronously; a *shadow index* of uploaded segments is maintained.
- Two retention horizons emerge: **local retention** (`retention.local.target.ms/bytes`) — how much stays on broker disk — and total retention, enforced against the object store. Local disk becomes a hot cache of the log tip; the object store holds the deep history.
- **Read path:** consumers within local retention read exactly as normal. Historical reads beyond local retention are served by brokers fetching segment ranges from object storage into a bounded local cache and serving from there — transparent at the protocol level [Kafka-protocol: the consumer just fetches offsets], very much not transparent in latency [Redpanda-specific: first-byte latency in the tens-to-hundreds of ms, and throughput governed by object-store bandwidth and the cache].
- It also unlocks: **remote recovery** of a lost cluster from the bucket, and fast partition movement (a replica joining can hydrate from S3 rather than streaming everything from the leader).

**Does it make sense on our platform?** Reasoned position: *yes, strongly — contingent on an answer to "which S3 endpoint."* The value here is less "infinite retention" and more (a) capping local disk sizing so brokers stay small and rebuildable, (b) turning "disk pressure" incidents from data-loss threats into cache-tuning problems (Volume 2 §13.4), and (c) decoupling replay/backtest-style historical consumption from the latency-critical tip. The dependency is an S3-compatible endpoint with real bandwidth and availability: candidates are an internal object store (MinIO on Weka, or Weka's own S3 protocol front end if licensed/enabled here) — flag as a **day-one question**: *"do we have a blessed internal S3 endpoint, and is anyone already pointing Redpanda tiered storage at it?"* Sending trading-adjacent data to external cloud object storage is presumably a non-starter for policy reasons before technical ones. Until tiered storage exists, retention is bounded by local disk alone and §13.4's disk-pressure runbook runs in its harsher mode.

# Deployment Topologies on Our Stack

## The Kubernetes operator and workload shape

The supported path is the **Redpanda Operator** reconciling a `Redpanda` custom resource (API group `cluster.redpanda.com/v1alpha2`+). Lineage note that explains its shape: the v2 operator grew out of the official Helm chart — the CRD's `spec.clusterSpec` is essentially the chart's values schema, and the operator historically drove Flux HelmRelease machinery under the hood (bundled/vendored in current versions; you don't wire it into *our* Flux, but you'll recognize Helm-chart anatomy in what it renders). What it manages:

- A **StatefulSet** of brokers: stable identities (`redpanda-0..N-1`), one PVC per broker, ordered-ish lifecycle. A **headless Service** provides per-broker DNS (`redpanda-0.redpanda.<ns>.svc.cluster.local...`) — these names are the backbone of internal advertised listeners (§5).
- Bootstrap/config: node config rendered per broker; cluster config applied via the Admin API; a superuser bootstrap secret.
- Day-2: rolling restarts with maintenance-mode choreography on config/version changes (the operator puts a broker in maintenance, restarts it, waits for health, proceeds), decommission integration when scaling down.

**PROD:** the operator's rolling machinery is good but not clairvoyant — it gates on broker health, not on *your* SLOs. Volume 2 §14 covers when to let it drive vs when to hand-roll. Also **[Redpanda-specific]**: brokers are *not* interchangeable cattle; each holds replicas. The StatefulSet gives identity, but *data* placement is Redpanda's (the controller's), not Kubernetes'. Deleting a PVC is destroying a replica set member, with Raft consequences, not "the pod will just re-pull its state" — it will, actually, re-replicate from peers, but that is a data-movement event you schedule, not an accident you shrug at.

**Rack awareness:** set `rack` per broker (the operator maps it from a node label, e.g. `topology.kubernetes.io/zone` or our own failure-domain label) and enable `enable_rack_awareness`; the controller then spreads each partition's replicas across racks. On our bare-metal rooms the honest failure domains are power/switch/chassis groupings — use whatever label the platform already uses for StackGres anti-affinity, for the same reason: RF=3 with all three replicas behind one failed switch is RF=1. Pair with **pod anti-affinity** (required, hostname-level: never two brokers on one node) and a **PodDisruptionBudget of `maxUnavailable: 1`** — with Raft quorum-of-3, one broker down is routine and two is an outage, so the PDB must make voluntary disruption serialize. **[PaaS team]** owns all of this topology; it is not tenant-visible.

## The storage question, in full

Redpanda's design center is **fast local NVMe with direct I/O** — shard-per-core down to per-core I/O queues assumes the device is local, low-latency, and privately owned. Our realistic options:

**(a) Weka CSI.** You know Weka: a parallel filesystem over the network with genuinely low latency (hundreds of µs, not the ms of classic NAS), client-side POSIX with `O_DIRECT` support. It *works*. The honest analysis, mirroring the StackGres material:

- *Latency:* every write and every fsync crosses the network. Even excellent Weka sync latency is plausibly 5–20× local NVMe sync latency, and it sits directly on the `acks=all` path (§4.3). Measure with `fio` `O_DIRECT` sync-write at Redpanda-like block sizes on an actual Weka mount *before* believing anything (Volume 2 §12.5).
- *Correlated failure domain* — the StackGres argument, verbatim: Redpanda's durability model assumes replica independence. Three replicas on three brokers whose volumes all live on one Weka cluster share fate with that Weka cluster's availability, its client version rollouts, and its own maintenance events. RF=3 on one storage backend is not three independent copies; it is one very durable copy with three frontends. Additionally: replication now happens twice (Raft across brokers *and* Weka's own protection), paying network and capacity twice for protection you only need once.
- *Noisy neighbors:* Weka bandwidth/IOPS are shared with every other Weka consumer on the platform; someone's backfill job becomes your produce-latency incident, cross-system.

**(b) Local persistent volumes** (local PV static provisioner, or an LVM-based local provisioner such as TopoLVM on node NVMe). This matches Redpanda's design center: local latency, independent failure domains (a dead node takes out exactly one replica, which is the event Raft is built for), no cross-system noisy neighbors. Costs: node-affine PVCs pin brokers to nodes (a broker cannot reschedule elsewhere — but with per-node data it shouldn't anyway; a dead node's broker is *replaced* via decommission + new broker, Volume 2 §10/§14, exactly like replacing a failed Consul server rather than teleporting it); node pool management becomes storage capacity management; and the nodes need real NVMe provisioned.

**(c) Bare metal outside Kubernetes.** The traditional HFT answer, and Redpanda runs superbly this way — full `rpk redpanda tune` applicability, no orchestration layer between you and the cores. The cost is real, though: you lose the platform. No Flux/KubeVela/CUE delivery path, bespoke lifecycle automation, a second operational model to staff, and the self-service story (Volume 2 §9) gets harder because the thing tenants configure no longer lives where their other resources live.

**Recommendation.** Run Redpanda **in Kubernetes on local NVMe PVs** as the default posture: it preserves the platform delivery model while honoring the storage design center and the replica-independence argument. Use **Weka only for the tiered-storage object tier** (if a Weka-backed S3 endpoint exists — §4.4), where its shared-backend nature is fine because the object tier is explicitly a single logical store, and where latency is off the hot path. Reserve **bare metal** as the escalation if a future workload demands sub-100µs produce tails that even in-cluster local NVMe plus CNI can't meet — and treat that as a product decision, not a default. If organizational reality forces Weka for broker data volumes, demand the fio numbers first, budget the produce p99 accordingly or deploy write caching deliberately, and document the correlated-failure caveat in the platform's durability statement so nobody believes RF=3-on-one-Weka is more independent than it is.

| Option | Produce-path latency | Failure independence | Platform fit | Ops burden |
|---|---|---|---|---|
| Weka CSI | Worst (network + shared backend on fsync path) | Poor (shared backend; double replication) | Best (any node, PVC portability) | Low day-1, latent day-2 |
| Local NVMe PV | Excellent | Excellent (per-node) | Good (node-affine PVCs; replace-not-move brokers) | Medium (node pool + provisioner) |
| Bare metal | Best (full tuning) | Excellent | None (off-platform delivery/self-service) | High (second ops model) |


# Networking on Cilium BGP

This is the section where Kafka-family deployments on Kubernetes actually break in practice, and — unusually — our fabric makes the story *better* than the industry default, because native routing removes the pathology most people fight. But you have to understand the pathology first.

## The advertised-listeners problem

**[Kafka-protocol]** The Kafka protocol has a two-phase connection model that defeats every load-balancer instinct you have from HAProxy:

1. A client connects to any **bootstrap** address and asks for metadata.
2. The metadata response lists **every broker by its advertised address**, and which broker leads each partition.
3. The client then opens **direct connections to specific brokers** — the leader of each partition it produces to or fetches from — using those advertised addresses.

The consequence: you cannot put Kafka behind a single VIP the way you'd put Postgres behind HAProxy. A load-balanced VIP works for bootstrap and then becomes irrelevant — or actively harmful, if brokers *advertise* the VIP, because then a client asking for "broker 2" gets round-robined to whichever broker the LB picks, receives `NOT_LEADER_FOR_PARTITION`, refreshes metadata, and loops. The requirement is **per-broker addressability**: every client must be able to reach *each specific broker* at the exact address that broker advertises. Advertised addresses are configuration (`advertised_kafka_api` per listener per broker), and every maddening "it works from the bootstrap connection then times out" ticket in Kafka history is an advertised-address bug (Volume 2 §15).

Redpanda brokers expose multiple named **listeners** (e.g. `internal` on one port, `external` on another), each with its own advertised address, TLS, and auth config — so in-cluster and out-of-cluster clients can be given *different* answers to "where is broker 2."

## Our fabric changes the answer

On the default industry CNI (VXLAN overlay, pod IPs unroutable from outside), per-broker external addressability requires contortions: per-broker NodePorts, or per-broker LoadBalancers, or hostNetwork. Our fabric is **Cilium in native-routing BGP mode: pod CIDRs are advertised to the ToR over BGP, pod IPs are real routed addresses on the datacenter fabric, no encapsulation**. That fact restructures the whole decision:

**Option A — internal listener, headless-service DNS (in-cluster clients).** Brokers advertise their stable per-broker DNS names (`redpanda-0.redpanda.<ns>.svc...`). Pod restarts change the pod IP but not the name; clients re-resolve on reconnect. This is the default and correct answer for platform-internal consumers. **LATENCY:** in native routing, pod-to-pod is direct routed L3 — no encap overhead, no NAT on the data path; DNS is only on the connection path, not per-message.

**Option B — routable pod IPs directly (the HFT-attractive option, analyzed seriously).** Since pod IPs are fabric-routable, an off-cluster trading host can, in principle, connect directly to broker pod IPs: shortest possible path, zero middleboxes, no LB state, no conntrack on an LB node, symmetric routing. This is genuinely attractive and genuinely workable *for the data path*. The problems are all about **address stability**, and they are disqualifying as the *advertised* address for external clients:

- A pod reschedule assigns a new IP from a (possibly different node's) pod CIDR. The broker's advertised address must change; clients holding cached metadata get connection refused/timeout to the old IP until metadata refresh; anything that pinned or firewalled the old IP breaks. During a rolling restart this happens N times.
- External clients need *name-based* stability, and cluster DNS names don't resolve off-cluster unless we export cluster DNS to the corporate resolvers — possible, but now DNS infrastructure is on the trading data path's *connection* path.
- Per-IP CiliumNetworkPolicy/firewalling toward churning addresses is misery.

So: direct pod-IP reachability is a *property we exploit*, not an addressing scheme we advertise.

**Option C — per-broker LoadBalancer VIPs via Cilium LB-IPAM + BGP (recommended for external clients).** This is the fabric-native answer:

- Define a `CiliumLoadBalancerIPPool` carving a dedicated VIP range for Redpanda (e.g. one /28), and create **one LoadBalancer Service per broker** (selector pinned to the pod by StatefulSet identity label), each with a **pinned, stable IP** from the pool (via the LB-IPAM request annotation), plus one shared bootstrap Service across all brokers.
- Cilium advertises these VIPs to the ToR over the existing BGP sessions, via a `CiliumBGPAdvertisement` that matches Service VIPs. The VIP is stable *forever* — broker restarts, reschedules, even node replacement do not change what clients dial. The advertised external listener address for `redpanda-N` is its VIP (or a DNS name pointing at it, registered in real corporate DNS).
- Set `externalTrafficPolicy: Local` so the VIP is advertised only from the node actually hosting that broker pod: traffic lands directly on the right node, no second hop, no SNAT, source IPs preserved for policy/audit. With one backend pod per Service this is natural. **LATENCY:** the packet path is then ToR → node → pod via Cilium's eBPF service translation — one address translation, no extra network hop; on this fabric that is a very small tax over raw pod IP, bought back many times over in address stability.

**PROD — behavior during broker lifecycle events, the thing clients actually experience:**

- *Broker restart in place:* VIP unchanged; TCP connections reset; clients reconnect to the same VIP, which black-holes until the pod is back (`Local` policy: no endpoints → withdrawn/unreachable). Well-behaved clients ride this out via metadata refresh + retries against *other* brokers for partitions whose leadership moved (maintenance mode moved leadership *first* — this choreography is why restarts are client-invisible when done right, and client-visible when someone skips maintenance mode).
- *Leadership movement without restart:* pure [Kafka-protocol] — `NOT_LEADER` → metadata refresh → connect to the new leader's (stable) address. No infrastructure involvement at all.
- *Pod rescheduled to another node:* VIP still unchanged (LB-IPAM re-advertises from the new node); the *only* client-visible effect is the downtime of the move itself. This is the entire payoff of Option C.

**NodePorts** — mentioned for completeness, rejected: per-broker ports on every node, clients dialing node IPs that are themselves lifecycle-unstable, `externalTrafficPolicy` sharp edges, and it solves nothing Option C doesn't solve better on a BGP fabric.

## Network policy

CiliumNetworkPolicy patterns, all **[PaaS team]**-owned:

- **Broker↔broker:** allow RPC/Raft port (33145) and internal Kafka listener strictly among broker pods (label-selected), plus Admin API (9644) from platform tooling and the operator.
- **Client→broker:** default-deny the Kafka external/internal listeners; allow per tenant namespace (identity-based, which is why we don't want IP-based policy from Option B). The self-service path (Volume 2 §9) can stamp out per-tenant allows from the CUE definition — network access as part of topic provisioning.
- **Metrics:** allow VictoriaMetrics scrapers to 9644 `/public_metrics`.
- **Egress from brokers:** tiered storage S3 endpoint only, if/when enabled.

# The Client Contract

The platform's job is to publish a small number of *blessed client profiles* rather than let every tenant rediscover Kafka tuning folklore. The two profiles below are the deliverable; this section is the reasoning behind them.

## Producer path

**[Kafka-protocol]** knobs that matter, with Redpanda-specific notes:

- **`acks`** — see §3.2. Platform stance: `acks=all` is the default and the only setting permitted for topics marked durable; `acks=1`/`0` only on explicitly loss-tolerant topics.
- **Idempotent producer** (`enable.idempotence=true`): sequence numbers per producer/partition dedupe retries; removes the classic "retry duplicated my message" pathology; negligible cost. Blessed-on in both profiles, always.
- **Transactions / EOS:** honestly assessed — Kafka-protocol transactions give you *atomic multi-partition writes* and *exactly-once within a Kafka-to-Kafka read-process-write pipeline* (consume-transform-produce with offsets committed in the transaction). They do **not** give you exactly-once side effects into external systems (a database write inside a consumer still needs its own idempotence — same reasoning you've applied to "exactly-once" claims everywhere else). They add a transaction-coordinator round trip and commit-marker machinery to the latency budget, and `read_committed` consumers wait on LSO advancement, adding consumer-visible latency coupled to producer commit cadence. Platform stance: supported [Redpanda-specific: fully implemented], documented, and *not* in either blessed profile; tenants who need EOS pipelines opt in consciously and accept the tail cost.
- **Batching: `linger.ms` × `batch.size`** — the fundamental latency/throughput dial. `linger.ms=0` sends immediately (batches still form under backpressure — you get *some* batching for free when the pipe is busy); `linger.ms=2–10` trades milliseconds of intentional delay for dramatically better compression and throughput.
- **Compression:** `lz4` or `zstd` for throughput streams; **none** for the latency profile (compression is producer-side CPU *and* jitter).
- **`max.in.flight.requests.per.connection`:** with idempotence ≤5, ordering is preserved across retries; leave at 5.

**LATENCY — the two blessed profiles** (full config tables in Volume 2 §9):

- **`latency-critical`:** `acks=all` (durability is non-negotiable even here; if the fsync tail hurts, the *topic* opts into write caching — a broker-side decision made with the platform, not a client-side `acks` downgrade, because `acks=1` changes failure semantics while write caching only changes the crash-correlation window), idempotence on, `linger.ms=0`, no compression, tight `delivery.timeout.ms`/`request.timeout.ms` with bounded retries, metadata refresh tuned down so leadership moves are absorbed fast.
- **`throughput`:** `acks=all`, idempotence on, `linger.ms=5`, `batch.size` 256KiB–1MiB, `zstd`, generous timeouts.

## Consumer path

Consumer groups and rebalancing are **[Kafka-protocol]** and behave as you know conceptually; what you need operationally:

- **Rebalance protocols:** classic *eager* rebalancing stops the world — every member revokes all partitions, rejoins, reassigns; a deploy of a 50-instance consumer group can cascade into minutes of repeated full stops ("rebalance storm", Volume 2 §15). **Cooperative/incremental** rebalancing (`CooperativeStickyAssignor`, or librdkafka's incremental protocol) moves only the partitions that must move. Platform stance: cooperative assignor is in both blessed profiles; eager is a documented anti-pattern.
- **`session.timeout.ms` / `heartbeat.interval.ms` / `max.poll.interval.ms`:** the classic failure triad — a consumer that processes a batch longer than `max.poll.interval.ms` is ejected, triggering a rebalance, which lengthens processing, which ejects the next one: the death spiral behind half of all "consumer lag exploding" incidents (Volume 2 §13.3 triages platform-vs-app precisely along this line).
- **Static group membership** (`group.instance.id`): a restarting member reclaims its partitions without a rebalance if it returns within the session timeout — blessed for K8s-deployed consumers, where restarts are routine.
- **Offset commit cadence:** auto-commit intervals bound replay-on-crash; apps needing tighter guarantees commit manually per batch. Replay tolerance is an **[App team]** design obligation and the platform documentation says so bluntly: *the platform guarantees at-least-once delivery on durable topics; deduplication downstream is yours.*

# Schema Registry, HTTP Proxy, Admin API

**[Redpanda-specific]** All three ship inside the same broker binary — no separate deployables, no extra Raft/storage backends; Schema Registry state lives in an internal compacted topic (`_schemas`), readable by any broker.

- **Schema Registry** (port 8081): Confluent-API-compatible; schemas (Avro/Protobuf/JSON Schema) registered per *subject*, with compatibility rules (`BACKWARD`, `FORWARD`, `FULL`, transitive variants) enforced at registration. Governance stance for a multi-team platform: **schemas are part of the topic contract**. The self-service CUE definition (Volume 2 §9) should require a subject + compatibility mode for any topic crossing team boundaries, default `BACKWARD` (consumers upgrade first — the sane default for independently-deployed teams), and registration should flow through Git like everything else, not ad-hoc producer auto-registration (`auto.register.schemas=false` in blessed profiles; CI registers). **[Shared]:** platform owns the registry, enforcement defaults, and subject-naming convention (mirror topic namespacing, §8.4); app teams own schema content and evolution within the compatibility rules.
- **pandaproxy** (HTTP proxy, port 8082): REST produce/consume. Useful for low-rate, non-performance-critical producers (scripts, hooks) that don't want a Kafka client. Platform stance: exposed internally only, documented as *not* a data-path option — anything with rate or latency requirements uses a real client.
- **Admin API** (port 9644): the operational control surface — broker/cluster health, maintenance mode, decommission, cluster config, feature flags, `/public_metrics`. Everything `rpk` does lands here. **Never tenant-exposed**; network-policied to platform tooling and the operator. All tenant-facing mutation flows through Git → Flux → operator CRDs instead — same principle as "nobody port-forwards to Patroni's REST API."

# Security and Multi-Tenancy

## AuthN

- **SASL/SCRAM** (SCRAM-SHA-256/512): username+salted-password over TLS; users stored in the controller state. The workhorse for tenant credentials — think "Vault-issued database creds" shape, and Volume 2 §9 wires issuance/rotation accordingly.
- **mTLS listeners:** per-listener TLS with client-cert requirement; the client principal is extracted from the certificate DN (mapping rules configurable). Strongest option, and the hook for the SPIFFE question below.
- Listener-scoped: the internal listener, external listener, registry, proxy, and admin API each carry their own TLS/auth posture.

## AuthZ: ACLs

**[Kafka-protocol]** model: `(principal, host, operation, resource, permission)` — operations like `READ`/`WRITE`/`DESCRIBE`/`CREATE` on resources `topic:X` (literal or *prefixed*), `group:Y`, `cluster`, `transactional-id`. Prefixed ACLs are the multi-tenancy workhorse: grant team `alpha` `READ/WRITE/DESCRIBE` on `topic: alpha.*` and `group: alpha.*` and the namespace is closed. Default-deny once `kafka_enable_authorization=true` — **PROD:** flipping authorization on against an already-live cluster is a breaking migration; do it before tenants arrive, which is now.

## Quotas as blast-radius control

Per-principal (and per-client-id) **produce and fetch byte-rate throttling**: a client exceeding its budget gets throttle-time in responses — backpressure, not errors. This is the platform's primary *isolation* primitive, the moral equivalent of StackGres per-tenant resource bounds: one team's runaway backfill consumer or misconfigured firehose producer degrades *itself*, not the cluster. Platform stance: **every tenant principal gets a quota at provisioning time** (defaults encoded in CUE, raisable by request), because a quota added after the incident is a postmortem action item, and a quota added before is nothing at all. Partition-count ceilings (Volume 2 §9) are the companion control on metadata/shard blast radius.

## Namespacing conventions

Topic names: `<team>.<domain>.<stream>[.<version>]` (e.g. `alpha.md.normalized-ticks.v1`); consumer groups: `<team>.<app>[.<purpose>]`; SR subjects mirror topic names; principals: `svc-<team>-<app>`. Prefixed ACLs, quotas, dashboards, and chargeback all key off the `<team>.` prefix, which is why the convention is enforced by the CUE definition (regex on the field, not a wiki page asking nicely).

## The SPIFFE/SPIRE question — flag for day one

Redpanda has **no native SPIFFE/SPIRE integration**: no SDS-style cert delivery, no SVID-aware principal mapping out of the box. The honest options, mirroring the StackGres treatment:

1. **SPIRE-issued mTLS, glued:** run spiffe-helper (or the CSI driver + a reloader sidecar) to materialize SVID certs for broker listeners and clients; configure principal-mapping rules to extract the SPIFFE ID (URI SAN → this is the sharp edge: confirm Redpanda's principal extraction can key off URI SANs vs only Subject DN — if DN-only, encode identity in the DN at issuance) and write ACLs against those principals. Gets workload identity end-to-end; costs rotation choreography (Redpanda reloads certs on file change — verify listener behavior on rotation under load in the lab).
2. **SCRAM independent of the platform identity layer:** per-app SCRAM users provisioned/rotated through the self-service path, TLS from platform-internal CA. Simpler, proven, but Redpanda becomes an identity island exactly the way StackGres passwords are today.
3. **Pragmatic hybrid (likely recommendation):** SCRAM for tenant clients now, SPIRE-issued certs for *broker↔broker and platform-tooling* TLS where the integration is under platform control, converge tenant auth later if/when the SPIFFE story firms up.

**Day-one questions:** does any SPIRE→Redpanda glue already exist here; what CA signs broker listener certs today; and is there an appetite for URI-SAN principal mapping work. Write the answer into this booklet's margin.
