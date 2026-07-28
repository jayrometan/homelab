# StackGres Production Deployment via Flux

> Deployed: 2026-07-27 on jay1 (192.168.1.25), k3s v1.36.2, Cilium 1.19.5, Flux v2, StackGres 1.19.0

This document captures every decision, step, and YAML involved in deploying a
production-grade StackGres PostgreSQL cluster via GitOps. It is written for a
platform engineer at a 2000-person firm who needs to understand not just *what*
was done but *why* each piece is structured the way it is.

---

## Context and Goals

**MacDonalds** uses StackGres as the managed PostgreSQL layer on its Kubernetes
PaaS platform. When an engineering team needs a database, they open a PR adding
an SGCluster manifest to the GitOps repo. Flux reconciles it. No Helm commands,
no `kubectl apply` — Git is the single source of truth.

Goals for this deployment:

- **GitOps-native**: every resource defined in Git, reconciled by Flux.
- **Production-grade configuration**: postgresql.conf tuned for OLTP, PgBouncer
  in transaction mode, proper role hierarchy.
- **Externally accessible**: LoadBalancer IP from Cilium LB-IPAM pool for DBA
  access and the admin UI. Applications inside the cluster use ClusterIP via DNS.
- **Secure by default**: no passwords in Git, secrets managed out-of-band via a
  companion script.
- **Documented operational runbook**: this file.

---

## Architecture

```
                         ┌────────────────────────────────────────────┐
                         │                  Git repo                  │
                         │  clusters/jay1/                            │
                         │    stackgres-operator.yaml  ─────────────► │─────┐
                         │    stackgres-cluster.yaml   ─────────────► │──┐  │
                         └────────────────────────────────────────────┘  │  │
                                                                          │  │
                    ┌─────────────────────────────────────────────────────┘  │
                    ▼                                                         │
         infrastructure/stackgres/operator/                                  │
           helmrepository.yaml  ─────────────────────────────────────────┐   │
           helmrelease.yaml  ────────► StackGres Operator (ns: stackgres) │   │
                                       - stackgres-operator Deployment    │   │
                                       - stackgres-restapi Deployment     │   │
                                       - All CRDs installed               │   │
                                       - Admin UI: LoadBalancer IP        │   │
                                                                          │   ▼
                                                             infrastructure/stackgres/cluster/
                                                               sginstanceprofile.yaml
                                                               sgpostgresconfig.yaml
                                                               sgpoolingconfig.yaml
                                                               sgscript.yaml
                                                               sgcluster.yaml
                                                                    │
                                                                    ▼
                                                     SGCluster 'primary' (ns: postgres)
                                                       StatefulSet: 1 pod (homelab)
                                                       ├── postgres container
                                                       ├── patroni (HA agent)
                                                       ├── pgbouncer (pooler)
                                                       ├── envoy (L7 proxy)
                                                       ├── postgres_exporter
                                                       └── fluent-bit (log shipper)
                                                     Services:
                                                       primary          → LoadBalancer 192.168.1.x
                                                       primary-replicas → ClusterIP
                                                       primary-pooler   → ClusterIP (apps use this)
                                                     Admin UI:
                                                       stackgres-restapi → LoadBalancer 192.168.1.x
```

---

## Repository Structure

```
homelab/
├── clusters/jay1/
│   ├── stackgres-operator.yaml      ← Flux Kustomization (operator)
│   └── stackgres-cluster.yaml       ← Flux Kustomization (cluster, depends on operator)
├── infrastructure/
│   └── stackgres/
│       ├── operator/
│       │   ├── kustomization.yaml   ← kustomize build list
│       │   ├── namespace.yaml       ← stackgres namespace
│       │   ├── helmrepository.yaml  ← Flux HelmRepository source
│       │   └── helmrelease.yaml     ← StackGres operator HelmRelease
│       └── cluster/
│           ├── kustomization.yaml   ← kustomize build list
│           ├── namespace.yaml       ← postgres namespace
│           ├── sginstanceprofile.yaml
│           ├── sgpostgresconfig.yaml
│           ├── sgpoolingconfig.yaml
│           ├── sgscript.yaml
│           └── sgcluster.yaml
└── scripts/
    └── create-pg-superuser.sh       ← post-deploy user/password setup
```

---

## Step-by-Step Deployment

### Step 1 — Why two separate Flux Kustomizations?

The operator and the cluster are intentionally split across two Flux
Kustomization objects (`stackgres-operator` and `stackgres-cluster`), linked
by `dependsOn`.

**Reason**: StackGres CRDs (`SGCluster`, `SGInstanceProfile`, etc.) are
installed by the operator Helm chart. If Flux tried to apply SGCluster manifests
before the CRDs exist, it would fail with `no kind registered for "SGCluster"`.

`dependsOn` enforces ordering: Flux applies `stackgres-operator` first, waits
for it to report `Ready: True` (which includes the HelmRelease being installed
and all CRDs present), then applies `stackgres-cluster`.

This is also the correct operational separation: the operator is platform-team
infrastructure (upgraded infrequently, cluster-wide impact), while clusters are
workload resources (created/modified by dev/DBA teams without touching the
operator).

```yaml
# clusters/jay1/stackgres-cluster.yaml — the key field:
spec:
  dependsOn:
    - name: stackgres-operator   # Flux will NOT apply cluster resources
                                 # until operator Kustomization is Ready
```

### Step 2 — HelmRepository and HelmRelease

Flux's `source-controller` fetches and caches the StackGres Helm chart index
from `https://stackgres.io/downloads/stackgres-k8s/stackgres/helm/`. The
`HelmRelease` in `infrastructure/stackgres/operator/helmrelease.yaml` tells the
`helm-controller` to install chart version `1.18.8` with specific values.

Key HelmRelease values explained:

```yaml
# Chart version is pinned. Never use version: "*" in production.
# Bumping this version (PR + review) is how you upgrade the operator.
version: "1.18.8"

# Flux retries 3 times on install failure, then rolls back.
# Prevents a half-installed operator from blocking future reconciles.
install:
  remediation:
    retries: 3

# Admin UI exposed as LoadBalancer so DBA team can reach it from their
# laptops without kubectl port-forward.
adminui:
  service:
    type: LoadBalancer
```

### Step 3 — StackGres CRDs

After the HelmRelease is applied, the cluster has these new CRDs:

| CRD | Purpose |
|-----|---------|
| `SGCluster` | The PostgreSQL cluster definition — the main object |
| `SGInstanceProfile` | CPU/memory "t-shirt size" for cluster pods |
| `SGPostgresConfig` | postgresql.conf settings managed declaratively |
| `SGPoolingConfig` | PgBouncer configuration |
| `SGObjectStorage` | Backup storage destination (S3, GCS, etc.) |
| `SGBackupConfig` | Backup schedule and retention policy |
| `SGBackup` | Represents one backup (manual or scheduled) |
| `SGScript` | SQL to run at cluster initialisation |
| `SGDbOps` | Operational tasks: vacuum, upgrade, restart, benchmark |
| `SGDistributedLogs` | Central log aggregation for all cluster instances |
| `SGShardedCluster` | (Advanced) Citus-style sharded cluster |

### Step 4 — SGInstanceProfile: CPU and memory sizing

```yaml
# infrastructure/stackgres/cluster/sginstanceprofile.yaml
spec:
  cpu: "2"      # homelab; production: "8"
  memory: "4Gi" # homelab; production: "16Gi"
```

StackGres manages the per-container resource split internally. Each pod runs
Postgres plus 5 sidecars. Rough sidecar overhead: ~350MB RAM and ~150m CPU.
So with 4Gi, Postgres itself gets ~3.65Gi of working memory.

**Rule of thumb**: `shared_buffers` = 25% of (total memory - sidecar overhead).
With 4Gi: `(4096MB - 350MB) × 0.25 ≈ 937MB` → set to `1GB`.

### Step 5 — SGPostgresConfig: production postgresql.conf

The most critical settings for a 2000-person OLTP deployment:

```
max_connections: 200        # Keep low — PgBouncer handles client-side concurrency
shared_buffers: 1GB         # 25% of usable RAM (production: 4GB of 16GB)
effective_cache_size: 3GB   # 75% of RAM — planner hint only, no allocation
work_mem: 32MB              # per sort/hash op; × connections × parallelism
random_page_cost: 1.1       # SSD value; default 4.0 is for spinning disk
wal_level: replica          # required for streaming replication
log_min_duration_statement: 1000  # log queries > 1s
shared_preload_libraries: pg_stat_statements  # query performance tracking
```

**The `random_page_cost` mistake**: the most common production misconfiguration
on SSD clusters. Leaving it at 4.0 (the HDD default) causes the planner to
choose sequential scans over index scans on large tables, resulting in
full-table scans where index lookups would be 10–100× faster.

**`work_mem` math**: With 200 `max_connections` and `max_parallel_workers_per_gather: 4`,
worst case peak memory from work_mem alone is `200 × 4 × 32MB = 25.6GB` —
more than the node has. This is normal; in practice most connections are idle
and most queries are single-threaded. Monitor `pg_stat_activity` and the
postgres_exporter `pg_settings_work_mem` metric; raise work_mem only if sort
spills to disk are frequent (`pg_stat_statements.temp_blks_written`).

### Step 6 — SGPoolingConfig: PgBouncer transaction mode

```
pool_mode: transaction      # server connection released after each transaction
max_client_conn: 1000       # total app connections accepted by PgBouncer
default_pool_size: 25       # actual Postgres connections maintained
```

**Why transaction mode?** With 2000 employees generating API traffic, your
application tier might have 50+ pod replicas each holding a connection pool of
10. That's 500 connections. PgBouncer's transaction mode collapses 500 client
connections to 25 actual Postgres connections, keeping `max_connections` well
under the limit.

**Application gotchas in transaction mode**:
- `SET variable = value` does NOT persist across transactions (connection may be
  given to another client after your transaction commits).
- `LISTEN/NOTIFY` requires session mode (or a dedicated session-mode connection).
- Prepared statements require `server_reset_query = DISCARD ALL` and special
  PgBouncer configuration.

Most standard ORMs (SQLAlchemy, ActiveRecord, GORM, Hibernate) work fine in
transaction mode. Verify during testing, not in production.

### Step 7 — SGScript: database and role initialisation

The `SGScript` defines SQL that runs exactly once on first cluster boot. The
design follows the principle of least privilege:

```
postgres     ← StackGres-managed superuser (do NOT give to apps)
dba          ← superuser for platform/DBA team; never in app code
app_user     ← read/write on app_db; what application pods use
readonly     ← SELECT only; for analysts, BI tools, read replicas
monitoring   ← pg_monitor role; for Prometheus postgres_exporter
```

**Why no passwords here?** Putting passwords in a YAML file commits them to
Git history forever. Even "private" repos get cloned, archived, and shared in
ways you don't control. Passwords are set post-deployment by
`scripts/create-pg-superuser.sh`, which generates secrets and stores them in
Kubernetes Secrets. The Secret values never touch Git.

**Default privileges** (set in SGScript):
```sql
ALTER DEFAULT PRIVILEGES IN SCHEMA public
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_user;
```
This means any table created by the `postgres` user (which your migration tool
runs as) is automatically readable/writable by `app_user`. Without this, every
new table needs an explicit GRANT after migration — a common operational
footgun.

### Step 8 — SGCluster: the primary object

```yaml
spec:
  instances: 1             # homelab; production: 3
  postgres:
    version: "16"
  pods:
    persistentVolume:
      size: "20Gi"         # homelab; production: "500Gi"
      storageClass: local-path
  configurations:
    sgInstanceProfile: production
    sgPostgresConfig: production-pg16
    sgPoolingConfig: production-pgbouncer
  postgresServices:
    primary:
      type: LoadBalancer   # Cilium LB-IPAM assigns from homelab-pool
    replicas:
      type: ClusterIP
  managedSql:
    scripts:
      - id: 1
        sgScript: init-database
```

**`postgresServices.primary.type: LoadBalancer`** causes StackGres to create
the primary Service with `type: LoadBalancer`. Cilium's LB-IPAM controller sees
the new LoadBalancer service and assigns it an IP from the `homelab-pool`
CIDR (`192.168.1.240/28`). This IP is then reachable from any machine on the
LAN and is announced via BGP to the router.

**Services created automatically**:

| Service name | Type | Port | Purpose |
|---|---|---|---|
| `primary` | LoadBalancer | 5432 | Direct Postgres (DBA access, pg_dump) |
| `primary-replicas` | ClusterIP | 5432 | Read replicas (internal read-only) |
| `primary-pooler` | ClusterIP | 5432 | PgBouncer (all application connections) |

**Secret created automatically**:

| Secret name | Keys |
|---|---|
| `primary` | `superuser-username`, `superuser-password`, `replication-username`, `replication-password` |

### Step 9 — Post-deploy: setting user passwords

After Flux reconciles and the cluster reaches Running state:

```bash
# Verify the cluster is up
kubectl get sgcluster primary -n postgres

# Verify pods are running (all containers Ready)
kubectl get pods -n postgres

# Get the LoadBalancer IP assigned to the primary service
kubectl get svc primary -n postgres

# Run the password setup script
./scripts/create-pg-superuser.sh
```

The script:
1. Generates a 32-character random password for each role
2. Applies `ALTER ROLE <user> PASSWORD '...'` via a temporary psql pod
3. Stores the password in a Kubernetes Secret (`pg-credentials-<user>`)
4. The Secret contains the full DSN string for convenient app config

**Referencing credentials in an application Deployment**:
```yaml
env:
- name: DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: pg-credentials-app_user
      key: dsn
```

---

## Verification Checklist

```bash
# 1. Flux Kustomizations are Ready
flux get kustomizations
# stackgres-operator   Ready
# stackgres-cluster    Ready

# 2. Operator pods running
kubectl get pods -n stackgres
# stackgres-operator-xxx    Running
# stackgres-restapi-xxx     Running

# 3. SGCluster is in Running state
kubectl get sgcluster primary -n postgres
# NAME      INSTANCES  PODS   RUNNING  METADATA

# 4. All cluster pods healthy (6+ containers per pod)
kubectl get pods -n postgres -o wide

# 5. Services exist and have IPs
kubectl get svc -n postgres
# primary          LoadBalancer  10.x.x.x  192.168.1.x   5432:xxxxx/TCP
# primary-replicas ClusterIP    10.x.x.x  <none>        5432/TCP
# primary-pooler   ClusterIP    10.x.x.x  <none>        5432/TCP

# 6. Connect via pooler (from inside cluster)
kubectl run pg-test --rm -it --image=postgres:16-alpine --restart=Never \
  -- psql -h primary-pooler.postgres.svc.cluster.local -U app_user -d app_db

# 7. Check Patroni state
kubectl exec -it primary-0 -n postgres -c patroni -- patronictl list

# 8. Verify PgBouncer stats
kubectl exec -it primary-0 -n postgres -c pgbouncer -- \
  psql -h 127.0.0.1 -p 6432 -U pgbouncer pgbouncer -c "SHOW POOLS;"

# 9. Connect to Admin UI (from your laptop)
# Get Admin UI LoadBalancer IP:
kubectl get svc -n stackgres -l "app=StackGresConfig"
# Open https://<ip>:443 in browser
# Default credentials: admin / (get from: kubectl get secret stackgres -n stackgres
#   -o jsonpath='{.data.clearPassword}' | base64 -d)
```

---

## Connecting from Applications

### Inside the cluster (recommended)

```yaml
# Application Deployment env section
env:
- name: DB_HOST
  value: "primary-pooler.postgres.svc.cluster.local"
- name: DB_PORT
  value: "5432"
- name: DB_NAME
  value: "app_db"
- name: DB_USER
  valueFrom:
    secretKeyRef:
      name: pg-credentials-app_user
      key: username
- name: DB_PASSWORD
  valueFrom:
    secretKeyRef:
      name: pg-credentials-app_user
      key: password
```

### From outside the cluster (DBA access)

```bash
# Get the LoadBalancer IP
LB_IP=$(kubectl get svc primary -n postgres \
  -o jsonpath='{.status.loadBalancer.ingress[0].ip}')

# Connect directly (bypasses PgBouncer — for admin/pg_dump only)
PGPASSWORD=$(kubectl get secret pg-credentials-dba -n postgres \
  -o jsonpath='{.data.password}' | base64 -d) \
psql -h ${LB_IP} -U dba -d app_db
```

### From your laptop (add to /etc/hosts or use the LB IP directly)

```
# /etc/hosts or DNS
192.168.1.x  postgres.homelab.local
```

---

## Day-2 Operations

### Trigger a manual backup (once SGObjectStorage is configured)

```yaml
apiVersion: stackgres.io/v1
kind: SGBackup
metadata:
  name: manual-backup-$(date +%Y%m%d)
  namespace: postgres
spec:
  sgCluster: primary
  managedLifecycle: false
```
```bash
kubectl apply -f backup.yaml
kubectl get sgbackup -n postgres -w
```

### Run VACUUM ANALYZE

```yaml
apiVersion: stackgres.io/v1
kind: SGDbOps
metadata:
  name: vacuum-$(date +%Y%m%d)
  namespace: postgres
spec:
  sgCluster: primary
  op: vacuum
  vacuum:
    full: false
    freeze: false
    analyze: true
    disablePageSkipping: false
```

### Planned restart (rolling, no downtime in HA setup)

```yaml
apiVersion: stackgres.io/v1
kind: SGDbOps
metadata:
  name: rolling-restart
  namespace: postgres
spec:
  sgCluster: primary
  op: restart
  restart:
    method: InPlace       # rolling restart, minimal downtime
```

### Scale to 3 instances (for production)

Edit `infrastructure/stackgres/cluster/sgcluster.yaml`:
```yaml
spec:
  instances: 3   # was: 1
```
Push to Git → Flux reconciles → StackGres creates 2 new pods with streaming
replication from the primary. Takes ~5 minutes depending on data size.

### Minor version upgrade

```yaml
apiVersion: stackgres.io/v1
kind: SGDbOps
metadata:
  name: minor-upgrade
  namespace: postgres
spec:
  sgCluster: primary
  op: minorVersionUpgrade
```

### Password rotation

```bash
./scripts/create-pg-superuser.sh --rotate app_user
```
Updates the Postgres role password AND the Kubernetes Secret atomically.
Applications using `secretKeyRef` pick up the new password on next pod restart
(or on next Secret read if using volume-mounted secrets with `subPath`).

---

## Backup Configuration (Production — Requires Object Storage)

For production, add these resources to `infrastructure/stackgres/cluster/`:

```yaml
# sgobjectstorage.yaml
apiVersion: stackgres.io/v1
kind: SGObjectStorage
metadata:
  name: s3-backups
  namespace: postgres
spec:
  type: s3Compatible
  s3Compatible:
    bucket: "macdonald-pg-backups"
    endpoint: "https://minio.internal.macdonald.com"
    region: "us-east-1"
    enablePathStyleAddressing: true
    awsCredentials:
      secretKeySelectors:
        accessKeyId:
          name: minio-backup-creds   # Kubernetes Secret with S3 credentials
          key: access-key
        secretAccessKey:
          name: minio-backup-creds
          key: secret-key
---
# sgbackupconfig.yaml
apiVersion: stackgres.io/v1
kind: SGBackupConfig
metadata:
  name: daily-backups
  namespace: postgres
spec:
  storage:
    sgObjectStorage: s3-backups
  baseBackups:
    cronSchedule: "0 2 * * *"   # 2am UTC daily full backup
    retention: 30               # keep 30 days of base backups
  # WAL archiving is always-on when sgbackupconfig is referenced.
  # This enables PITR (point-in-time recovery) to any second within the
  # retention window.
```

Then reference in SGCluster:
```yaml
spec:
  configurations:
    sgBackupConfig: daily-backups
```

---

## Observability

StackGres pods expose Prometheus metrics at `:9187/metrics` (postgres_exporter).
Key metrics to alert on:

| Metric | Alert threshold | Meaning |
|--------|----------------|---------|
| `pg_up` | < 1 for > 30s | Postgres process down |
| `pg_replication_lag_seconds` | > 30s | Replica falling behind primary |
| `pgbouncer_pools_cl_waiting` | > 0 for > 5s | Clients waiting for connections |
| `pg_stat_activity_count{state="idle in transaction"}` | > 10 | Possible connection leak |
| `pg_database_size_bytes` | > 80% of PVC | Storage nearly full |
| `pg_locks_count{mode="ExclusiveLock"}` | spike | Lock contention |

Add a PodMonitor to scrape them (requires Prometheus Operator):
```yaml
apiVersion: monitoring.coreos.com/v1
kind: PodMonitor
metadata:
  name: stackgres-clusters
  namespace: postgres
spec:
  selector:
    matchLabels:
      app: StackGresCluster
  podMetricsEndpoints:
  - port: pgexporter
    path: /metrics
    interval: 30s
```

---

## Troubleshooting

### Cluster stuck in non-Running state

```bash
kubectl describe sgcluster primary -n postgres
kubectl get events -n postgres --sort-by='.lastTimestamp'
kubectl logs -n stackgres deploy/stackgres-operator --tail=50
```

### PVC not binding (local-path provisioner quirk)

`local-path` uses `WaitForFirstConsumer` binding mode — the PVC only binds
after the pod is scheduled. If the pod is Pending, check node affinity and
taints:
```bash
kubectl describe pod primary-0 -n postgres
kubectl get pvc -n postgres
```

### Patroni split-brain (both nodes think they're primary)

This should NOT happen with the Kubernetes DCS. If it does:
```bash
kubectl exec -it primary-0 -n postgres -c patroni -- patronictl list
kubectl exec -it primary-0 -n postgres -c patroni -- patronictl failover primary
```

### PgBouncer rejecting connections

```bash
# Check PgBouncer logs
kubectl logs primary-0 -n postgres -c pgbouncer --tail=100

# Connect to PgBouncer admin console
kubectl exec -it primary-0 -n postgres -c pgbouncer -- \
  psql -h 127.0.0.1 -p 6432 -U pgbouncer pgbouncer -c "SHOW DATABASES;"
```

Common cause: application using a database name or user that doesn't exist in
PgBouncer's `pg_db` table. Check that the database name in the connection string
matches what's defined in SGPoolingConfig.

### Check replication lag

```bash
kubectl exec -it primary-0 -n postgres -c postgres -- \
  psql -U postgres -c "
    SELECT application_name, state,
           pg_wal_lsn_diff(sent_lsn, replay_lsn) AS lag_bytes
    FROM pg_stat_replication;"
```

---

## What This Demonstrates

**GitOps for stateful infrastructure**: the entire Postgres cluster — operator,
config, cluster definition, and initialisation SQL — lives in Git and is
reconciled by Flux. No Helm commands, no ad-hoc `kubectl apply`.

**Dependency ordering**: `dependsOn` in Flux Kustomizations enforces that CRDs
exist before custom resources are applied. This is the correct pattern for any
operator-backed workload.

**Cilium LB-IPAM as cloud-style LoadBalancer**: the `primary` service gets a
real routable IP from the LAN subnet (`192.168.1.240/28`) announced via BGP,
identical to how a cloud LoadBalancer service works in EKS or GKE.

**Production configuration in a homelab**: the postgresql.conf, PgBouncer, and
role hierarchy are identical to what you'd deploy for a real workload. The only
homelab concessions are instance count (1 instead of 3) and storage size (20Gi
instead of 500Gi).

**Secrets never in Git**: passwords are generated and stored in Kubernetes
Secrets by `scripts/create-pg-superuser.sh`. The script can be re-run for
password rotation. Applications reference secrets by name, not by value.


---

## Upgrade Notes: StackGres 1.18.x → 1.19.0 (k3s 1.36 Compatibility)

> This section documents every schema error hit during the actual deployment on
> k3s v1.36.2. If you are deploying StackGres 1.19.0 fresh, read this before
> writing any manifests — it will save hours.

### Why 1.19.0?

StackGres 1.19.0 ships with `kubeVersion: 1.18.0-0 - 1.35.x-O` in its Helm
chart. k3s v1.36.2 falls outside that range and the HelmRelease fails with:

```
chart requires kubeVersion: 1.18.0-0 - 1.35.x-O which is incompatible
with Kubernetes v1.36.2+k3s1
```

StackGres 1.19.0 raised the ceiling to `1.25.0-0 - 1.36.x-0`. Change the
HelmRelease chart version to `"1.19.0"` to fix this.

---

### Six CRD Schema Breaking Changes in 1.19.0

Each of these caused a Flux dry-run failure on the actual cluster, in this order:

---

#### 1. SGConfig — removed grafana/prometheus fields

**Error:**
```
SGConfig in version "v1" cannot be handled as a SGConfig: strict decoding
error: unknown field "spec.grafana.autoDiscoverDashboards",
unknown field "spec.prometheus.allowAutodiscovery"
```

**Root cause:** These fields existed in SGConfig 1.18.x but were removed from
the CRD schema in 1.19.0. The Helm chart install job creates an SGConfig and
the API server rejects it.

**Fix:** Remove both from HelmRelease `values:`:

```yaml
# REMOVE from helmrelease.yaml values (not in 1.19.0 schema):
#   grafana:
#     autoDiscoverDashboards: false
#   prometheus:
#     allowAutodiscovery: false
```

The 1.19.0 equivalents live under `spec.observability` in SGConfig.

---

#### 2. SGPoolingConfig — pgbouncer settings must be nested

**Error:**
```
SGPoolingConfig dry-run failed:
.spec.pgBouncer.pgbouncer.ini.reserve_pool_timeout: field not declared in schema
```

**Root cause:** The CRD restructured `pgbouncer.ini` in 1.19.0. It now has
three explicit sub-sections — `pgbouncer`, `databases`, `users` — matching the
three sections of an actual pgbouncer.ini file. Global settings must go under
`pgbouncer.ini.pgbouncer`, not flat under `pgbouncer.ini`.

**Wrong (1.18.x style):**
```yaml
spec:
  pgBouncer:
    pgbouncer.ini:
      pool_mode: transaction
      max_client_conn: "1000"
      reserve_pool_timeout: "3"
```

**Correct (1.19.0):**
```yaml
spec:
  pgBouncer:
    pgbouncer.ini:
      pgbouncer:            # <-- one level deeper
        pool_mode: transaction
        max_client_conn: 1000
        default_pool_size: 25
        min_pool_size: 5
        reserve_pool_size: 10
        reserve_pool_timeout: "3"
        max_db_connections: 100
        server_idle_timeout: "600"
        server_lifetime: "3600"
        server_connect_timeout: "15"
        auth_type: scram-sha-256
        log_connections: "0"
        log_pooler_errors: "1"
        stats_period: "60"
        ignore_startup_parameters: extra_float_digits,options
```

The `pgbouncer` sub-section has `additionalProperties: true` in the CRD, so
any standard PgBouncer setting is accepted there. Inspect the live schema:

```bash
kubectl get crd sgpoolconfigs.stackgres.io -o json \
  | python3 -c "
import sys, json
d = json.load(sys.stdin)
ini = (d['spec']['versions'][0]['schema']['openAPIV3Schema']
       ['properties']['spec']['properties']['pgBouncer']
       ['properties']['pgbouncer.ini'])
print(json.dumps(ini, indent=2))
"
```

---

#### 3. SGCluster — sgInstanceProfile moved to spec level

**Error:**
```
SGCluster dry-run failed:
.spec.configurations.sgInstanceProfile: field not declared in schema
```

**Root cause:** In 1.19.0, `sgInstanceProfile` was promoted from
`spec.configurations` to `spec.sgInstanceProfile` at the top level of spec.
`sgPostgresConfig` and `sgPoolingConfig` remain inside `spec.configurations`.

**Wrong (1.18.x):**
```yaml
spec:
  configurations:
    sgInstanceProfile: production
    sgPostgresConfig: production-pg16
    sgPoolingConfig: production-pgbouncer
```

**Correct (1.19.0):**
```yaml
spec:
  sgInstanceProfile: production          # promoted to top-level
  configurations:
    sgPostgresConfig: production-pg16    # stays here
    sgPoolingConfig: production-pgbouncer
```

List all allowed top-level spec fields:
```bash
kubectl get crd sgclusters.stackgres.io -o json \
  | python3 -c "
import sys, json
d = json.load(sys.stdin)
spec = d['spec']['versions'][0]['schema']['openAPIV3Schema']['properties']['spec']
print(json.dumps(list(spec['properties'].keys()), indent=2))
"
```

---

#### 4. SGPostgresConfig — Patroni-managed parameters blocklisted

**Error:**
```
admission webhook "sgpgconfig.validating-webhook.stackgres.io" denied the
request: Invalid postgres configuration, properties: wal_log_hints cannot
be settled
```

**Root cause:** StackGres manages several postgresql.conf parameters internally
via Patroni. The admission webhook rejects any SGPostgresConfig that sets them.
Confirmed blocklisted in 1.19.0:

| Parameter       | Why it is managed                                       |
|-----------------|---------------------------------------------------------|
| `wal_log_hints` | Required for pg_rewind (old primary re-sync after failover) |
| `hot_standby`   | Patroni controls replica read access based on cluster state |
| `listen_addresses` | StackGres controls pod networking                    |
| `cluster_name`  | Set by Patroni for DCS identity                         |

**Fix:** Remove these from `spec.postgresql.conf` in your SGPostgresConfig.

```yaml
# REMOVE — Patroni manages these, admission webhook will block them:
#   wal_log_hints: "on"
#   hot_standby: "on"
```

To see what Patroni currently has set:
```bash
kubectl exec -n postgres primary-0 -c patroni -- \
  patronictl -c /etc/patroni/postgres.yml show-config
```

---

#### 5. SGScript — continueOnSGScriptError renamed

**Error:**
```
SGScript dry-run failed:
.spec.continueOnSGScriptError: field not declared in schema
```

**Fix:** Rename to `continueOnError`.

```yaml
spec:
  continueOnError: false   # was: continueOnSGScriptError in 1.18.x
```

---

#### 6. SGScript SQL — SELECT does not execute DDL; use DO/EXECUTE

This is a logic bug, not a schema error. The admission webhook will not catch it.

**Wrong:**
```sql
-- Returns the string "CREATE DATABASE app_db" as a row. Does NOT create anything.
SELECT 'CREATE DATABASE app_db'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'app_db');
```

**Correct:**
```sql
DO $$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_database WHERE datname = 'app_db') THEN
    EXECUTE 'CREATE DATABASE app_db';
  END IF;
END $$;
```

**Shell quoting trap:** When writing YAML via SSH heredoc or `python3 -c` inside
a double-quoted shell string, `$$` expands to the current process ID. A DO
block written as `DO $$` may arrive on the server as `DO 45233`.

The error is:
```
ERROR: syntax error at or near "45233"   Position: 219
```

**Safe transfer pattern:** Use base64 to move files with dollar signs:
```bash
# Encode locally, decode on server
cat file.yaml | base64 | ssh root@server "base64 -d > /remote/path/file.yaml"
```

---

### Debugging Flux Reconciliation Failures

The full error path is in the `MESSAGE` column or the Kubernetes condition:

```bash
# Quick overview
flux get kustomizations

# Full error message for a specific kustomization
kubectl get kustomization stackgres-cluster -n flux-system \
  -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}'

# Force reconcile after pushing a fix
flux reconcile source git flux-system
kubectl annotate kustomization stackgres-cluster -n flux-system \
  reconcile.fluxcd.io/requestedAt="$(date -u +%FT%TZ)" --overwrite

# Watch conditions live
kubectl get kustomization stackgres-cluster -n flux-system -w
```

**Reading the error path:** Flux dry-run failures report the full JSON path of
the offending field. Example:

```
.spec.pgBouncer.pgbouncer.ini.reserve_pool_timeout: field not declared in schema
```

Use that path to navigate the live CRD schema and understand what structure is
actually expected. This is faster than reading docs that may be outdated.

---

### SGScript Execution and Re-run Behaviour

StackGres tracks which scripts ran in `SGCluster.status.managedSql`. Check it:

```bash
kubectl get sgcluster primary -n postgres \
  -o jsonpath='{.status.managedSql}' | python3 -m json.tool
```

The output shows each script's `id`, `version`, `completedAt`/`failedAt`, and
any `failure` message with its Postgres error code.

When `managedVersions: true` (the default), changing a script's content in the
SGScript object causes the operator to detect the change via hash and re-run
that script. However, if the operator is not actively watching for changes,
annotate the SGCluster to trigger a reconcile:

```bash
kubectl annotate sgcluster primary -n postgres \
  stackgres.io/reconciliation-pause="false" --overwrite
```

**Manual fallback via postgres-util:** If the script mechanism is stuck, run
SQL directly. The `postgres-util` container is always present, connects via
Unix socket (no password, no network):

```bash
# List databases
kubectl exec -n postgres primary-0 -c postgres-util -- psql -U postgres -c "\l"

# Create database
kubectl exec -n postgres primary-0 -c postgres-util -- \
  psql -U postgres -c "CREATE DATABASE app_db"

# Connect to a specific database
kubectl exec -n postgres primary-0 -c postgres-util -- \
  psql -U postgres -d app_db -c "GRANT CONNECT ON DATABASE app_db TO app_user"

# Run a multi-statement file
kubectl cp ./init.sql postgres/primary-0:/tmp/init.sql -c postgres-util
kubectl exec -n postgres primary-0 -c postgres-util -- \
  psql -U postgres -f /tmp/init.sql
```

---

### Full Verification Checklist

```bash
# 1. All four Flux kustomizations Ready at the same commit SHA
flux get kustomizations

# 2. Operator pods (operator + restapi)
kubectl get pods -n stackgres

# 3. Cluster pod — 5/5 containers (postgres, patroni, pgbouncer, exporter, fluent-bit)
kubectl get pods -n postgres

# 4. All config CRDs have objects
kubectl get sginstanceprofile,sgpgconfig,sgpoolconfig,sgscript,sgcluster -n postgres

# 5. Services — confirm LoadBalancer IP assigned
kubectl get svc -n postgres

# 6. Cluster conditions
kubectl get sgcluster primary -n postgres \
  -o jsonpath='{.status.conditions}' | python3 -m json.tool
# Expect: Bootstrapped=True, Failed=False, PendingRestart=False

# 7. Init scripts ran
kubectl get sgcluster primary -n postgres \
  -o jsonpath='{.status.managedSql}' | python3 -m json.tool

# 8. Databases and roles exist
kubectl exec -n postgres primary-0 -c postgres-util -- psql -U postgres -c "\l"
kubectl exec -n postgres primary-0 -c postgres-util -- \
  psql -U postgres -c "SELECT rolname, rolsuper FROM pg_roles ORDER BY rolname"

# 9. Credential Secrets exist
kubectl get secrets -n postgres | grep pg-credentials

# 10. Patroni cluster health
kubectl exec -n postgres primary-0 -c patroni -- \
  patronictl -c /etc/patroni/postgres.yml list
```

---

### Connection Reference

| Service | Type | Address | Use |
|---|---|---|---|
| `primary` | LoadBalancer | `192.168.1.242:5432` | DBA psql, pg_dump |
| `primary-pooler` | ClusterIP | `primary-pooler.postgres.svc:5432` | Applications (PgBouncer) |
| `primary-replicas` | ClusterIP | `primary-replicas.postgres.svc:5432` | Read-only queries |
| `primary-rest` | ClusterIP | `primary-rest.postgres.svc:8008` | Patroni REST API |

**Get an application DSN:**
```bash
kubectl get secret pg-credentials-app-user -n postgres \
  -o jsonpath='{.data.dsn}' | base64 -d
```

**Rotate a password:**
```bash
./scripts/create-pg-superuser.sh --rotate app_user
```

---

### Day-2 Operations Quick Reference

**Scale from 1 → 3 instances:**
Edit `sgcluster.yaml`: `instances: 3`. Flux rolls out two new replica pods.
Add pod anti-affinity to ensure they land on different nodes.

**Manual failover (test HA):**
```bash
kubectl exec -n postgres primary-0 -c patroni -- \
  patronictl -c /etc/patroni/postgres.yml failover primary --force
```

**PgBouncer admin console:**
```bash
kubectl exec -it primary-0 -n postgres -c pgbouncer -- \
  psql -h 127.0.0.1 -p 6432 -U pgbouncer pgbouncer -c "SHOW POOLS;"
# Commands: SHOW POOLS, SHOW CLIENTS, SHOW SERVERS, SHOW STATS, RECONNECT
```

**Check replication lag:**
```bash
kubectl exec -n postgres primary-0 -c postgres-util -- psql -U postgres -c "
  SELECT application_name, state,
         pg_wal_lsn_diff(sent_lsn, replay_lsn) AS lag_bytes
  FROM pg_stat_replication;"
```

**Slow query analysis:**
```bash
kubectl exec -n postgres primary-0 -c postgres-util -- psql -U postgres -d app_db -c "
  SELECT query, calls, total_exec_time::int, rows,
         (total_exec_time/calls)::int AS avg_ms
  FROM pg_stat_statements
  ORDER BY total_exec_time DESC LIMIT 10;"
```

**Postgres config currently in effect:**
```bash
kubectl exec -n postgres primary-0 -c postgres-util -- psql -U postgres -c "
  SELECT name, setting, unit, source
  FROM pg_settings
  WHERE source != 'default'
  ORDER BY name;"
```

---

### Conceptual Summary: Why StackGres vs Plain PostgreSQL on Kubernetes

Running PostgreSQL on Kubernetes manually (StatefulSet + ConfigMap + Service)
requires solving:

| Problem | DIY effort |
|---|---|
| HA and automatic failover | Patroni + etcd/k8s DCS config |
| Connection pooling | PgBouncer StatefulSet + lifecycle hooks |
| WAL archiving and backups | Custom scripts + object storage config |
| Secrets and TLS rotation | cert-manager integration |
| Metrics | postgres_exporter sidecar + ServiceMonitor |
| Log aggregation | Fluent-bit sidecar + parsing config |
| Major version upgrades | pg_upgrade scripts, downtime planning |

StackGres packages all of this. An SGCluster manifest is the equivalent of an
RDS instance definition — you declare what you want, the operator handles how.

The SGCluster pod runs these containers:

| Container | Role |
|---|---|
| `postgres` | PostgreSQL process |
| `patroni` | HA agent: leader election, failover, config push |
| `pgbouncer` | Connection pooler (transaction mode) |
| `postgres-exporter` | Prometheus `/metrics` endpoint |
| `fluent-bit` | Structured log shipping |
| `postgres-util` | Admin tooling: psql, pg_dump, patronictl, pg_rewind |

The trade-off: StackGres adds operational complexity at the operator layer.
The 1.18→1.19 upgrade demonstrated this — six schema changes in one release.
In production, always test operator upgrades in staging before applying to
production clusters.
