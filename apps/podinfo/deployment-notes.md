# Deploying a Test App via Flux — podinfo

> Deployed: 2026-07-15 on jay1 (192.168.1.25), k3s v1.36.2, Cilium 1.19.5, Flux v2

This walkthrough deploys [podinfo](https://github.com/stefanprodan/podinfo) — a lightweight Go microservice used as the canonical Flux demo app — entirely through GitOps. No `kubectl apply` is run manually. Flux detects the Git commit and applies everything.

---

## Why podinfo

- ~10MB image, minimal resource footprint (10m CPU / 32Mi RAM)
- Built-in `/healthz` and `/readyz` endpoints — works cleanly with Kubernetes health checks
- Returns useful JSON from its API (hostname, version, runtime info)
- Maintained by the Flux author — designed for exactly this use case

---

## Repository Layout

Two things were added to the homelab repo:

```
homelab/
├── clusters/jay1/
│   ├── flux-system/          ← existing (Flux manages itself)
│   └── apps.yaml             ← NEW: Flux Kustomization CRD
└── apps/
    └── podinfo/              ← NEW: app manifests
        ├── kustomization.yaml
        ├── namespace.yaml
        ├── deployment.yaml
        └── service.yaml
```

**Why two separate places?**

- `apps/podinfo/` — the actual Kubernetes manifests (Deployment, Service, Namespace). This is what gets applied to the cluster.
- `clusters/jay1/apps.yaml` — the Flux `Kustomization` CRD that tells Flux *to apply* the manifests in `apps/podinfo/`. This is the GitOps wiring.

The existing `flux-system` Kustomization already watches the entire `clusters/jay1/` directory. By adding `apps.yaml` there, Flux picks it up automatically on the next reconcile — no restart, no manual step.

---

## The Manifests

### `apps/podinfo/namespace.yaml`

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: podinfo
  labels:
    app.kubernetes.io/managed-by: flux
```

### `apps/podinfo/deployment.yaml`

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: podinfo
  namespace: podinfo
  labels:
    app.kubernetes.io/name: podinfo
spec:
  replicas: 2
  selector:
    matchLabels:
      app: podinfo
  template:
    metadata:
      labels:
        app: podinfo
    spec:
      containers:
      - name: podinfo
        image: ghcr.io/stefanprodan/podinfo:6.7.0
        ports:
        - containerPort: 9898
          name: http
        readinessProbe:
          httpGet:
            path: /readyz
            port: 9898
          initialDelaySeconds: 5
          periodSeconds: 10
        livenessProbe:
          httpGet:
            path: /healthz
            port: 9898
          initialDelaySeconds: 5
          periodSeconds: 30
        resources:
          requests:
            cpu: 10m
            memory: 32Mi
          limits:
            cpu: 100m
            memory: 64Mi
```

### `apps/podinfo/service.yaml`

```yaml
apiVersion: v1
kind: Service
metadata:
  name: podinfo
  namespace: podinfo
spec:
  type: LoadBalancer    # Cilium LB-IPAM assigns an IP from the homelab-pool
  selector:
    app: podinfo
  ports:
  - name: http
    port: 9898
    targetPort: 9898
```

The Service type is `LoadBalancer`. On a cloud cluster this would hit the cloud provider. Here, Cilium's LB-IPAM assigned an IP from the `homelab-pool` (`192.168.1.240/28`) automatically — no extra config needed.

### `apps/podinfo/kustomization.yaml` (Kustomize tool config — not a Flux CRD)

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - namespace.yaml
  - deployment.yaml
  - service.yaml
```

This is the standard `kustomize` tool config file — it lists which files to include when `kustomize build` is run. Flux's kustomize-controller runs `kustomize build` on this directory before applying.

---

## The Flux Wiring

### `clusters/jay1/apps.yaml` — Flux Kustomization CRD

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: apps
  namespace: flux-system
spec:
  interval: 5m          # re-reconcile every 5 minutes
  sourceRef:
    kind: GitRepository
    name: flux-system   # reuse the existing GitRepository that watches this repo
  path: ./apps/podinfo  # which directory to apply
  prune: true           # delete resources that are removed from Git
  wait: true            # wait for Deployment to be Ready before marking as complete
  timeout: 3m
  healthChecks:
    - apiVersion: apps/v1
      kind: Deployment
      name: podinfo
      namespace: podinfo
```

**Key fields explained:**

| Field | Value | Why |
|---|---|---|
| `sourceRef` | `flux-system` GitRepository | Reuses the existing source that watches `192.168.1.26/root/homelab.git` — no new GitRepository needed |
| `path` | `./apps/podinfo` | The directory Flux will run `kustomize build` on |
| `prune: true` | enabled | If you delete a file from Git, Flux deletes the resource from the cluster |
| `wait: true` | enabled | Flux waits for all applied resources to be healthy before reporting `Ready: True` |
| `healthChecks` | Deployment/podinfo | Explicitly checks this Deployment's readiness as the health gate |

---

## Deployment Flow

```
1. Manifests committed to homelab repo (git push → GitLab at 192.168.1.26)
2. flux reconcile source git flux-system   ← forced immediately (normally polls every 1m)
3. flux reconcile kustomization flux-system ← root Kustomization picks up apps.yaml
4. Flux creates the apps Kustomization CRD in the cluster
5. apps Kustomization runs kustomize build ./apps/podinfo
6. Applies: Namespace → Deployment → Service
7. Cilium LB-IPAM assigns 192.168.1.240 to the LoadBalancer Service
8. Flux waits for Deployment health check (readinessProbe on /readyz)
9. apps Kustomization reports Ready: True
```

Total time from `git push` to `Ready: True`: ~37 seconds (dominated by image pull).

---

## Verification

```bash
# Check both Kustomizations are Ready
flux get kustomizations
# NAME        REVISION            SUSPENDED  READY  MESSAGE
# apps        main@sha1:7f3a54a6  False      True   Applied revision: main@sha1:7f3a54a6
# flux-system main@sha1:7f3a54a6  False      True   Applied revision: main@sha1:7f3a54a6

# Check pods
kubectl get pods -n podinfo
# NAME                      READY   STATUS    RESTARTS   AGE
# podinfo-59b998584-grcl2   1/1     Running   0          37s
# podinfo-59b998584-pw7hm   1/1     Running   0          37s

# Check service — note the EXTERNAL-IP assigned by Cilium LB-IPAM
kubectl get svc -n podinfo
# NAME      TYPE           CLUSTER-IP    EXTERNAL-IP     PORT(S)          AGE
# podinfo   LoadBalancer   10.43.239.5   192.168.1.240   9898:31120/TCP   37s

# Hit the app
curl http://192.168.1.240:9898
# {
#     "hostname": "podinfo-59b998584-grcl2",
#     "version": "6.7.0",
#     "message": "greetings from podinfo v6.7.0",
#     "goos": "linux",
#     "goarch": "amd64",
#     "num_cpu": "12"
# }
```

The app is reachable at `http://192.168.1.240:9898` from anywhere on the home network.

---

## What This Demonstrates

**GitOps in action:** No `kubectl apply` was run. The only action taken was committing YAML to Git and pushing. Flux detected the commit and applied everything.

**LB-IPAM:** The LoadBalancer Service got a real IP (`192.168.1.240`) from the Cilium pool without any cloud provider. This is the same mechanism production bare-metal clusters use.

**Health-gated reconciliation:** Flux only reported `Ready: True` after the Deployment's readiness probe passed. If the image had failed to start, the Kustomization would have reported `HealthCheckFailed` and the deployment would be visibly broken in `flux get kustomizations`.

**Pruning:** If `apps/podinfo/` is deleted from Git and pushed, Flux will delete the Namespace, Deployment, and Service from the cluster. Git is the source of truth in both directions.

---

## Useful Commands

```bash
# Watch reconciliation live
flux get kustomizations --watch

# Force a reconcile without waiting for the 5m interval
flux reconcile kustomization apps --with-source

# Check Flux logs for this Kustomization
flux logs --kind=Kustomization --name=apps

# Suspend (pause Flux from touching this app — for manual intervention)
flux suspend kustomization apps

# Resume
flux resume kustomization apps

# See what Flux would change without applying
flux diff kustomization apps

# Trace the podinfo deployment back to its Flux source
flux trace deployment podinfo --namespace podinfo
```

---

## Next Steps

- **Add an HTTPRoute** — expose podinfo via the Cilium Gateway API instead of a raw LoadBalancer Service
- **Add a second overlay** — create `apps/podinfo/staging/` and `apps/podinfo/prod/` to practise environment promotion
- **Pin and promote an image tag** — change `6.7.0` to `6.6.0` in Git, push, watch Flux roll it out; then roll forward
- **Test pruning** — delete a file from `apps/podinfo/`, push, watch Flux remove the resource from the cluster
