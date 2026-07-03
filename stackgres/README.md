# StackGres — Deep Dive Learning Notes

> Context: Tower Research Capital uses StackGres heavily in production as the managed PostgreSQL layer on their Kubernetes PaaS platform.

---

## What Is StackGres

StackGres is a **Kubernetes operator for PostgreSQL** built by OnGres. It manages the full lifecycle of production-grade Postgres clusters — HA, failover, backups, connection pooling, monitoring, extensions — all through Kubernetes CRDs.

Think of it as: "what if you took a DBA's entire operational runbook and encoded it as a Kubernetes controller."

It is **not** a thin wrapper around `StatefulSet + postgres container`. Every pod StackGres creates is a full sidecar stack — Patroni, PgBouncer, Envoy, Prometheus exporter, and Fluent Bit all run alongside Postgres in the same pod.

---

## Architecture

### The Operator Pattern

StackGres follows the standard Kubernetes operator model:

```
User applies SGCluster manifest
        │
        ▼
StackGres Operator (watches CRDs)
        │
        ├─ Creates StatefulSet (N pods, one per Postgres instance)
        ├─ Creates Services (primary, replicas, pooler)
        ├─ Creates Secrets (superuser credentials, replication user)
        ├─ Creates ConfigMaps (Patroni config, PgBouncer config)
        └─ Manages ongoing reconciliation (failover, backups, upgrades)
```

The operator runs in the `stackgres` namespace and has cluster-wide RBAC — it needs to watch and manage resources across all namespaces.

### Pod Anatomy (the sidecar stack)

Every StackGres-managed Postgres pod runs these containers:

```
┌─────────────────────────────────────────────────────┐
│                   StackGres Pod                     │
│                                                     │
│  patroni          ← HA agent, leader election,      │
│                     streaming replication           │
│                                                     │
│  postgres         ← actual Postgres process         │
│                     (managed by Patroni, not direct)│
│                                                     │
│  pgbouncer        ← connection pooler               │
│                     apps connect here, not to PG    │
│                                                     │
│  envoy            ← L7 proxy, traffic management   │
│                     used for pooler routing         │
│                                                     │
│  prometheus       ← postgres_exporter               │
│  exporter            scrape endpoint: :9187         │
│                                                     │
│  fluent-bit       ← ships Postgres logs to          │
│                     SGDistributedLogs               │
└─────────────────────────────────────────────────────┘
```

### High Availability — Patroni

Patroni is the HA brain. It:
- Runs in every pod, coordinates via a **Distributed Configuration Store (DCS)**
- In StackGres, the DCS backend is the **Kubernetes API itself** (using Lease objects) — no external etcd dependency
- Elects a primary; all replicas stream WAL from the primary
- On primary failure: Patroni on the replicas holds an election, promotes the best replica, updates the Kubernetes Service endpoint to point to the new primary
- Applications using the primary Service see an endpoint update — downtime is typically under 10 seconds

```
                  ┌──────────────────────────────┐
                  │   Kubernetes API (DCS)        │
                  │   (Patroni writes leader key) │
                  └──────┬──────────┬─────────────┘
                         │          │
              ┌──────────▼──┐  ┌────▼───────────┐
              │  Pod 0      │  │  Pod 1          │
              │  PRIMARY    │  │  REPLICA        │
              │  Patroni ●  │  │  Patroni ○      │
              │  Postgres ●◄├──┤► Postgres ○     │ (streaming replication)
              └──────┬──────┘  └─────────────────┘
                     │
         ┌───────────▼────────────┐
         │  primary Service       │
         │  (endpoint = Pod 0)    │
         └────────────────────────┘
              apps connect here
```

### Connection Pooling — PgBouncer

Every SGCluster has a **pooler** (PgBouncer) deployed alongside it. In Kubernetes, your app pods can easily scale to 50+ replicas — each one opening its own DB connection would exhaust Postgres's `max_connections` fast.

PgBouncer sits in front and maintains a small, fixed pool of actual Postgres connections, multiplexing many application connections onto them.

```
App Pod 1 ──┐
App Pod 2 ──┤──► PgBouncer Service ──► PgBouncer ──► Postgres
App Pod 3 ──┘        (port 5432)       (pool: 20)   (max_connections: 100)
...
App Pod 50 ─┘
```

**Critical:** Your application should always connect to the **pooler service**, not directly to the Postgres service. Connecting directly bypasses pooling and can exhaust connections under load.

---

## Key CRDs

### SGCluster — the main object

Defines a PostgreSQL cluster. Everything derives from this.

```yaml
apiVersion: stackgres.io/v1
kind: SGCluster
metadata:
  name: trading-db
  namespace: quant-team
spec:
  instances: 2                        # 1 primary + 1 replica
  postgres:
    version: '16'
  pods:
    persistentVolume:
      size: '100Gi'
      storageClass: weka-block        # your CSI StorageClass
  sgInstanceProfile: 'medium'         # CPU/memory profile (see below)
  configurations:
    sgPostgresConfig: 'tuned-pg16'    # custom postgresql.conf
    sgPoolingConfig: 'pgbouncer-std'  # PgBouncer settings
  managedSql:
    scripts:
    - sgScript: 'init-schema'         # runs SQL on first boot
```

### SGInstanceProfile — T-shirt sizes

Defines CPU and memory limits for cluster pods. Platform teams create a set of standard profiles (small/medium/large) and devs pick one.

```yaml
apiVersion: stackgres.io/v1
kind: SGInstanceProfile
metadata:
  name: medium
  namespace: quant-team
spec:
  cpu: '4'
  memory: '8Gi'
```

### SGPostgresConfig — postgresql.conf as a CRD

Lets you manage `postgresql.conf` settings declaratively. Patroni applies them across all instances.

```yaml
apiVersion: stackgres.io/v1
kind: SGPostgresConfig
metadata:
  name: tuned-pg16
  namespace: quant-team
spec:
  postgresVersion: '16'
  postgresql.conf:
    max_connections: '200'
    shared_buffers: '2GB'
    effective_cache_size: '6GB'
    wal_level: 'replica'
    max_wal_senders: '10'
    synchronous_commit: 'off'         # HFT: trade durability for latency
```

### SGPoolingConfig — PgBouncer as a CRD

```yaml
apiVersion: stackgres.io/v1
kind: SGPoolingConfig
metadata:
  name: pgbouncer-std
spec:
  pgBouncer:
    pgbouncer.ini:
      pool_mode: transaction           # transaction pooling (most efficient)
      max_client_conn: '1000'
      default_pool_size: '25'
```

### SGObjectStorage — backup destination

Defines where backups go. Platform team creates this once, clusters reference it.

```yaml
apiVersion: stackgres.io/v1
kind: SGObjectStorage
metadata:
  name: s3-backups
spec:
  type: s3Compatible
  s3Compatible:
    bucket: 'tower-pg-backups'
    endpoint: 'https://minio.internal'
    awsCredentials:
      secretKeySelectors:
        accessKeyId:
          name: backup-s3-creds
          key: access-key
        secretAccessKey:
          name: backup-s3-creds
          key: secret-key
```

### SGBackup — trigger or schedule a backup

```yaml
apiVersion: stackgres.io/v1
kind: SGBackup
metadata:
  name: trading-db-manual-backup
spec:
  sgCluster: trading-db
  managedLifecycle: false             # manual, not managed by operator
```

### SGDbOps — operational tasks

This is StackGres's equivalent of a DBA runbook entry. You declare what operation to run, the operator executes it safely:

```yaml
apiVersion: stackgres.io/v1
kind: SGDbOps
metadata:
  name: major-version-upgrade
spec:
  sgCluster: trading-db
  op: majorVersionUpgrade             # pg14 → pg16
  majorVersionUpgrade:
    postgresVersion: '16'
    backupPath: 's3://tower-pg-backups/pre-upgrade'
```

Operations supported: `vacuum`, `repack`, `minorVersionUpgrade`, `majorVersionUpgrade`, `restart`, `benchmark`, `clone`

### SGScript — seed SQL

```yaml
apiVersion: stackgres.io/v1
kind: SGScript
metadata:
  name: init-schema
spec:
  scripts:
  - name: create-app-user
    script: |
      CREATE USER appuser WITH PASSWORD 'changeme';
      CREATE DATABASE appdb OWNER appuser;
      GRANT ALL PRIVILEGES ON DATABASE appdb TO appuser;
```

---

## How Tower's Platform Team Likely Deploys This

Based on their stack (FluxCD + CUE + Weka CSI + SPIFFE/SPIRE + VictoriaMetrics), here's the realistic deployment model:

### 1. Operator installation (platform team, one-time)

StackGres operator is installed via a FluxCD `HelmRelease`:

```yaml
# clusters/production/stackgres/operator.yaml
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: stackgres-operator
  namespace: stackgres
spec:
  chart:
    spec:
      chart: stackgres-operator
      sourceRef:
        kind: HelmRepository
        name: stackgres-charts
  values:
    grafana:
      autoDiscoverDashboards: true
```

### 2. Platform-managed shared resources

The platform team creates shared CRDs that all teams reference:

```
platform/stackgres/
  instance-profiles/
    small.yaml       # 1 CPU, 2Gi
    medium.yaml      # 4 CPU, 8Gi
    large.yaml       # 8 CPU, 16Gi
  postgres-configs/
    pg16-default.yaml
    pg16-low-latency.yaml   # tuned for HFT: synchronous_commit=off, etc.
  pooling-configs/
    pgbouncer-transaction.yaml
  object-storage/
    weka-s3.yaml     # backup destination on internal object store
```

All of these are reconciled by FluxCD from the GitOps repo.

### 3. Dev team requests a database

Tower uses CUE lang for config. A dev team's database request would be a CUE file that generates the SGCluster manifest:

```cue
// teams/quant-alpha/databases/market-data.cue
package databases

import "platform.tower.internal/schemas/database:v1"

marketDataDB: database.#Cluster & {
    name:         "market-data"
    namespace:    "quant-alpha"
    instances:    2
    profile:      "medium"
    pgVersion:    "16"
    pgConfig:     "pg16-low-latency"
    storageSize:  "200Gi"
    backups: {
        enabled:  true
        schedule: "0 2 * * *"    // 2am daily
    }
}
```

The CUE schema (owned by platform team) generates the full SGCluster, SGBackup, and RBAC manifests. The dev team only specifies what they need — the platform enforces the rest.

**Workflow:**

```
Dev team writes CUE config
        │
        ▼
Opens PR in GitOps repo
        │
        ▼
Platform team reviews (or automated policy check via CI)
        │
        ▼
PR merged → FluxCD detects change
        │
        ▼
CUE rendered → SGCluster manifest applied
        │
        ▼
StackGres operator creates the cluster
        │
        ▼
Secret auto-created: <cluster-name>  (contains host, port, user, password)
        │
        ▼
Dev team mounts the secret in their app deployment
```

### 4. How apps connect

StackGres creates these Services automatically per cluster:

```
<cluster-name>              → Patroni primary (port 5432, direct Postgres)
<cluster-name>-replicas     → read replicas (port 5432)
<cluster-name>-pooler       → PgBouncer (port 5432) ← apps use this
```

And this Secret:

```
<cluster-name>              → contains: superuser-username, superuser-password,
                              replication-username, replication-password
```

App deployment references it:

```yaml
env:
- name: DB_HOST
  value: "market-data-pooler.quant-alpha.svc.cluster.local"
- name: DB_PASSWORD
  valueFrom:
    secretKeyRef:
      name: market-data
      key: superuser-password
```

---

## SPIFFE/SPIRE Integration (Tower-specific)

Tower uses SPIFFE/SPIRE for zero-trust workload identity. In this model:

- App pods don't use static DB passwords from Secrets
- Instead, the app presents its SPIFFE SVID (workload certificate) to a Vault-like intermediary or directly to Postgres (via `cert` auth method)
- StackGres supports `ssl` authentication mode in `pg_hba.conf`
- Platform team configures `SGPostgresConfig` with `ssl=on` and `pg_hba.conf` entries requiring cert auth for specific users

This is a more advanced setup — likely only for production clusters, not dev environments.

---

## VictoriaMetrics Integration

Every StackGres pod exposes a Prometheus endpoint at `:9187` via the built-in `postgres_exporter`. VictoriaMetrics scrapes this via a `PodMonitor`:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: PodMonitor
metadata:
  name: stackgres-clusters
spec:
  selector:
    matchLabels:
      app: StackGresCluster
  podMetricsEndpoints:
  - port: pgexporter
    path: /metrics
```

Key metrics to watch:
- `pg_up` — is Postgres alive
- `pg_replication_lag` — replica lag in seconds
- `pgbouncer_pools_cl_active` — active client connections
- `pg_stat_activity_count` — connections by state
- `pg_database_size_bytes` — database size growth

---

## Production Sharp Edges

**1. PVC sizing is one-way**
You can expand a PVC but not shrink it. Size your storage correctly upfront. The operator will resize the PVC on `SGCluster` update (if your CSI driver supports volume expansion — Weka does).

**2. Connect to the pooler, always**
Direct connections to the Postgres service bypass PgBouncer and will exhaust `max_connections` under load. `synchronous_commit=off` + high `max_connections` is an HFT pattern but PgBouncer is still essential.

**3. Patroni DCS is the Kubernetes API**
If your kube-apiserver has a blip, Patroni elections are blocked. In a cluster where etcd is under pressure (e.g., high object churn from other workloads), Patroni can log DCS timeouts. This is worth monitoring at Tower given the operational scale.

**4. Operator upgrade ≠ cluster upgrade**
Upgrading the StackGres operator does not upgrade your Postgres clusters. Cluster upgrades are done separately via `SGDbOps minorVersionUpgrade` or `majorVersionUpgrade`.

**5. Backup storage must exist first**
If your SGCluster references an `SGObjectStorage` that doesn't exist or has wrong credentials, the cluster still creates but backup jobs will silently fail. Always validate backup config independently.

**6. Namespace scoping**
SGInstanceProfile, SGPostgresConfig, SGPoolingConfig — these must be in the **same namespace** as the SGCluster that references them. The platform team either creates these in every team namespace (via FluxCD) or teams create their own.

**7. Resource overhead**
Each StackGres pod runs 5+ containers. The overhead beyond Postgres itself is roughly:
- Patroni: ~100MB
- PgBouncer: ~50MB
- Envoy: ~100MB
- Postgres exporter: ~50MB
- Fluent Bit: ~50MB

Total sidecar overhead: ~350MB per pod. Factor this into your SGInstanceProfile.

**8. Switchover vs failover**
- `switchover` (planned): Patroni gracefully demotes primary, promotes replica. Zero data loss. Use `SGDbOps restart` for this.
- `failover` (unplanned): Primary dies, Patroni elects new primary. Possible data loss if async replication was behind.
- For HFT: use `synchronous_standby_names` if you need zero-loss failover, but this adds write latency.

---

## Useful Commands

```bash
# List all clusters across namespaces
kubectl get sgclusters -A

# Get cluster status (shows primary, replicas, lag)
kubectl describe sgcluster <name> -n <namespace>

# Get auto-generated credentials
kubectl get secret <cluster-name> -n <namespace> -o jsonpath='{.data.superuser-password}' | base64 -d

# Connect via psql (using pooler)
kubectl run psql --rm -it --image=postgres:16 -- \
  psql -h <cluster-name>-pooler.<namespace>.svc.cluster.local -U postgres

# Check Patroni state
kubectl exec -it <pod-name> -n <namespace> -c patroni -- patronictl list

# Trigger a manual backup
kubectl apply -f backup.yaml
kubectl get sgbackup -n <namespace>

# Run a vacuum
kubectl apply -f - <<EOF
apiVersion: stackgres.io/v1
kind: SGDbOps
metadata:
  name: vacuum-now
  namespace: <namespace>
spec:
  sgCluster: <cluster-name>
  op: vacuum
  vacuum:
    full: false
    analyze: true
EOF

# Check replication lag
kubectl exec -it <primary-pod> -n <namespace> -c postgres -- \
  psql -U postgres -c "SELECT client_addr, state, sent_lsn, replay_lsn, (sent_lsn - replay_lsn) AS lag FROM pg_stat_replication;"
```
