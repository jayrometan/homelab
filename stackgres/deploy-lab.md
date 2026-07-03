# StackGres Lab Deployment — k3s on jay1 (192.168.1.25)

Hands-on deployment of StackGres on the single-node k3s cluster.

**Cluster:** k3s v1.36.2, Cilium CNI, Rocky Linux 9.3
**Storage:** local-path-provisioner (built into k3s)

---

## Step 1: Install the StackGres Operator

```bash
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml

helm repo add stackgres-charts https://stackgres.io/downloads/stackgres-k8s/aws/latest/helm/
helm repo update

helm install --create-namespace \
  --namespace stackgres \
  stackgres-operator \
  stackgres-charts/stackgres-operator
```

Wait for the operator to be ready:

```bash
kubectl rollout status deploy/stackgres-operator -n stackgres
kubectl rollout status deploy/stackgres-operator-restapi -n stackgres
```

Verify CRDs were installed:

```bash
kubectl get crd | grep stackgres
# Should show: sgclusters, sginstanceprofiles, sgpostgresconfigs, etc.
```

---

## Step 2: Create a Namespace for the Lab Cluster

```bash
kubectl create namespace sglab
```

---

## Step 3: Create an SGInstanceProfile (resource limits)

```yaml
# instance-profile.yaml
apiVersion: stackgres.io/v1
kind: SGInstanceProfile
metadata:
  name: lab-small
  namespace: sglab
spec:
  cpu: '1'
  memory: '2Gi'
```

```bash
kubectl apply -f instance-profile.yaml
kubectl get sginstanceprofiles -n sglab
```

---

## Step 4: Deploy a Single-Instance SGCluster

For a lab with local-path-provisioner, use 1 instance (no HA, but simpler):

```yaml
# lab-cluster.yaml
apiVersion: stackgres.io/v1
kind: SGCluster
metadata:
  name: lab-pg
  namespace: sglab
spec:
  instances: 1
  postgres:
    version: '16'
  pods:
    persistentVolume:
      size: '5Gi'
  sgInstanceProfile: lab-small
```

```bash
kubectl apply -f lab-cluster.yaml
```

Watch it come up (takes 2-3 minutes, it's pulling images):

```bash
kubectl get pods -n sglab -w
# You'll see: lab-pg-0 go through Init → Running
```

The pod is ready when you see `lab-pg-0` with all containers Running.

---

## Step 5: Verify the Cluster

```bash
# Check SGCluster status
kubectl describe sgcluster lab-pg -n sglab

# Check what Services were created
kubectl get svc -n sglab
# Expected:
#   lab-pg          → primary (direct Postgres)
#   lab-pg-replicas → replicas
#   lab-pg-pooler   → PgBouncer

# Check the auto-generated secret (credentials)
kubectl get secret -n sglab
kubectl get secret lab-pg -n sglab -o jsonpath='{.data.superuser-password}' | base64 -d && echo
```

---

## Step 6: Connect to Postgres

Run a one-shot psql pod to connect:

```bash
PGPASS=$(kubectl get secret lab-pg -n sglab -o jsonpath='{.data.superuser-password}' | base64 -d)

kubectl run psql --rm -it --restart=Never \
  --image=postgres:16 \
  --env="PGPASSWORD=$PGPASS" \
  -- psql -h lab-pg-pooler.sglab.svc.cluster.local -U postgres -d postgres
```

Once connected, check Patroni state from inside Postgres:

```sql
-- Check replication state
SELECT * FROM pg_stat_replication;

-- Check current role
SELECT pg_is_in_recovery();   -- false = primary, true = replica

-- Create a test table
CREATE TABLE test (id serial, val text, ts timestamptz default now());
INSERT INTO test (val) VALUES ('hello stackgres');
SELECT * FROM test;
```

---

## Step 7: Exec Into the Pod — Patroni CLI

```bash
kubectl exec -it lab-pg-0 -n sglab -c patroni -- bash

# Inside the pod:
patronictl list          # shows cluster topology, primary/replica, lag
patronictl history       # timeline history (shows past failovers)
patronictl version       # patroni version
```

---

## Step 8: Simulate What a Dev Team Does (full flow)

In a real platform (like MacDonalds), devs get their DB credentials via a Secret and reference them in their app. Simulate this:

```yaml
# app-deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: demo-app
  namespace: sglab
spec:
  replicas: 1
  selector:
    matchLabels:
      app: demo-app
  template:
    metadata:
      labels:
        app: demo-app
    spec:
      containers:
      - name: psql-demo
        image: postgres:16
        command: ["sleep", "3600"]
        env:
        - name: PGHOST
          value: "lab-pg-pooler.sglab.svc.cluster.local"
        - name: PGUSER
          value: "postgres"
        - name: PGPASSWORD
          valueFrom:
            secretKeyRef:
              name: lab-pg         # auto-created by StackGres
              key: superuser-password
        - name: PGDATABASE
          value: "postgres"
```

```bash
kubectl apply -f app-deployment.yaml

# Once running, exec in and verify connection
kubectl exec -it deploy/demo-app -n sglab -- psql -c "SELECT version();"
```

This demonstrates the full dev team experience: they only reference the Service name and Secret — no manual credential management.

---

## Teardown

```bash
# Delete the cluster (PVC is NOT deleted by default — data is safe)
kubectl delete sgcluster lab-pg -n sglab

# Delete the PVC explicitly if you want to free storage
kubectl delete pvc -n sglab --all

# Uninstall the operator
helm uninstall stackgres-operator -n stackgres
```

---

## What to Explore Next

- **SGBackup**: configure SGObjectStorage pointing at MinIO (deployable on k3s) and trigger a backup
- **SGDbOps restart**: perform a Patroni switchover (even on single node, it restarts cleanly)
- **2-instance cluster**: add a second instance to lab-pg to see actual HA and streaming replication
- **SGPostgresConfig**: tune `shared_buffers`, `max_connections`, observe how Patroni reloads config
- **PgBouncer pool_mode**: switch from `session` to `transaction` pooling and observe connection multiplexing
