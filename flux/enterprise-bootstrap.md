# FluxCD — Enterprise Bootstrap Guide

How a platform team at scale provisions and bootstraps FluxCD. Contrast with the homelab approach (manual `flux bootstrap` CLI) — this is what an IaC-driven platform looks like.

---

## The Problem With Manual Bootstrap

What we did in the homelab:
```bash
flux bootstrap git \
  --url=http://192.168.1.26/root/homelab.git \
  --token-auth=true \
  --password='glpat-...'  # hardcoded personal token
```

Problems at scale:
- **Not reproducible** — if the cluster dies, how do you recreate it exactly?
- **Not auditable** — who ran this? when? what flags?
- **Personal credentials** — token is tied to a person, not a machine account
- **Not versioned** — the bootstrap command isn't in Git
- **Manual** — someone had to SSH in and run it

At 1500 engineers across many clusters, every one of these is a real operational risk.

---

## The Enterprise Model — IaC All The Way Down

The core principle: **cluster provisioning and Flux bootstrap are a single, automated, Terraform-driven pipeline.** No human runs manual commands against a cluster.

```
Engineer opens MR: "provision cluster prod-eu-2"
        │
        ▼
GitLab CI: terraform plan  →  reviewer sees exactly what will change
        │
        ▼
MR approved + merged
        │
        ▼
GitLab CI: terraform apply
        ├─ provisions cluster nodes (Kubespray/cloud provider)
        ├─ installs CNI (Cilium)
        ├─ reads deploy key from Vault
        └─ runs flux_bootstrap_git Terraform resource
                │
                ▼
        Flux is live on the new cluster
        watching the GitOps repo
        No human ever touched the cluster directly
```

---

## The Terraform Flux Provider

HashiCorp maintains an official Flux provider: `fluxcd/flux`

### Provider Configuration

```hcl
# providers.tf
terraform {
  required_providers {
    flux = {
      source  = "fluxcd/flux"
      version = ">= 1.3"
    }
    vault = {
      source  = "hashicorp/vault"
      version = ">= 3.0"
    }
  }
}

# Read deploy key from Vault (not hardcoded anywhere)
data "vault_generic_secret" "flux_deploy_key" {
  path = "secret/platform/flux/deploy-key"
}

provider "flux" {
  kubernetes = {
    # kubeconfig written to disk by the Kubespray playbook earlier in the pipeline
    config_path = "/tmp/kubeconfigs/${var.cluster_name}.yaml"
  }
  git = {
    url = "ssh://git@gitlab.internal/platform/gitops.git"
    ssh = {
      username    = "git"
      private_key = data.vault_generic_secret.flux_deploy_key.data["private_key"]
    }
  }
}
```

### Bootstrap Resource

```hcl
# flux.tf
resource "flux_bootstrap_git" "this" {
  # Where in the repo Flux writes its own manifests
  # and where it looks for cluster config
  path = "clusters/${var.cluster_name}"

  # Flux version to install
  version = "v2.9.0"

  # Namespace to install Flux controllers into
  namespace = "flux-system"

  # Reconcile interval for the root Kustomization
  interval = "5m"
}
```

This single resource:
1. Generates and commits `clusters/<name>/flux-system/` manifests to the GitOps repo
2. Applies the Flux controllers to the cluster
3. Creates the `flux-system` secret with Git credentials (from Vault)
4. Flux begins reconciling immediately

### Full Cluster Module

```hcl
# modules/cluster/main.tf

variable "cluster_name" { type = string }
variable "environment"  { type = string }  # dev, staging, prod

locals {
  gitops_repo = "ssh://git@gitlab.internal/platform/gitops.git"
  gitops_path = "clusters/${var.cluster_name}"
}

# 1. Pull credentials from Vault
data "vault_generic_secret" "flux_key" {
  path = "secret/platform/flux/deploy-key"
}

# 2. Provision the cluster (example: calling a Kubespray module)
module "cluster" {
  source       = "../kubespray"
  cluster_name = var.cluster_name
  node_count   = var.environment == "prod" ? 6 : 3
}

# 3. Bootstrap Flux after cluster is ready
resource "flux_bootstrap_git" "this" {
  depends_on = [module.cluster]
  path       = local.gitops_path
}

# Output the cluster's kubeconfig path for downstream use
output "kubeconfig_path" {
  value = module.cluster.kubeconfig_path
}
```

Usage:
```hcl
# environments/prod/eu-1/main.tf
module "prod_eu_1" {
  source       = "../../../modules/cluster"
  cluster_name = "prod-eu-1"
  environment  = "prod"
}
```

---

## Machine Accounts — Not Personal Tokens

In a 1500-person company you never use a personal account for automation.

### GitLab Setup (done once by platform team)

```
1. Create a dedicated GitLab user: flux-bot
   - No 2FA required (it's a machine)
   - No email notifications
   - Locked down to read_repository on the gitops repo only

2. Generate a project access token scoped to the gitops repo:
   - Scope: read_repository (Flux only needs to read)
   - Or: an SSH deploy key (read-only) added to the gitops project

3. Store in Vault:
   vault kv put secret/platform/flux/deploy-key \
     private_key=@/tmp/flux_ed25519 \
     public_key=@/tmp/flux_ed25519.pub
```

### Why SSH Deploy Keys Over PATs

| | PAT | SSH Deploy Key |
|--|-----|---------------|
| Scope | User-level or project | Project-specific |
| Rotation | Manual or Vault dynamic | Vault PKI or manual |
| Audit | Token usage logs | SSH access logs |
| Risk if leaked | User-level access | Repo-level access only |
| Flux support | Yes (`--token-auth`) | Yes (default) |

SSH deploy keys are preferred because:
- Blast radius is limited to one repo if the key leaks
- SSH keys don't expire by default (rotation is explicit and controlled)
- Standard protocol — works the same across GitHub, GitLab, Gitea

---

## Multi-Cluster Repository Structure

One GitOps repo, many clusters. Each cluster's Flux watches its own path.

```
gitops-repo/
│
├── clusters/
│   ├── prod-eu-1/
│   │   ├── flux-system/          ← Flux self-manages (generated by bootstrap)
│   │   │   ├── gotk-components.yaml
│   │   │   ├── gotk-sync.yaml
│   │   │   └── kustomization.yaml
│   │   ├── infrastructure.yaml   ← Kustomization: apply infrastructure/
│   │   └── apps.yaml             ← Kustomization: apply apps/
│   │
│   ├── staging/
│   │   ├── flux-system/
│   │   ├── infrastructure.yaml
│   │   └── apps.yaml
│   │
│   └── dev/
│       ├── flux-system/
│       ├── infrastructure.yaml
│       └── apps.yaml
│
├── infrastructure/               ← shared across clusters
│   ├── base/
│   │   ├── cilium/
│   │   │   └── helmrelease.yaml
│   │   ├── cert-manager/
│   │   │   └── helmrelease.yaml
│   │   └── stackgres/
│   │       └── helmrelease.yaml
│   └── overlays/
│       ├── prod/                 ← prod-specific values (HA, larger resources)
│       └── dev/                  ← dev-specific values (single replica, smaller)
│
└── apps/
    ├── team-quant/
    │   ├── market-data/
    │   └── risk-engine/
    └── team-platform/
        └── internal-tools/
```

### How Each Cluster Wires Up Infrastructure

```yaml
# clusters/prod-eu-1/infrastructure.yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: infrastructure
  namespace: flux-system
spec:
  interval: 10m
  path: ./infrastructure/overlays/prod    # prod gets HA configs
  prune: true
  sourceRef:
    kind: GitRepository
    name: flux-system
  dependsOn:
    - name: flux-system   # wait for Flux itself to be ready
```

```yaml
# clusters/dev/infrastructure.yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: infrastructure
  namespace: flux-system
spec:
  interval: 10m
  path: ./infrastructure/overlays/dev    # dev gets lighter configs
  prune: true
  sourceRef:
    kind: GitRepository
    name: flux-system
```

Same repo, different paths, different clusters. Prod and dev can diverge in config while sharing the same base manifests.

---

## The GitLab CI Bootstrap Pipeline

The Terraform runs inside a GitLab CI pipeline, not on someone's laptop.

```yaml
# .gitlab-ci.yml in the platform-infra repo

stages:
  - plan
  - apply

variables:
  TF_ROOT: environments/${CLUSTER_ENV}/${CLUSTER_NAME}
  VAULT_ADDR: https://vault.internal

before_script:
  # Authenticate to Vault using GitLab CI OIDC
  - export VAULT_TOKEN=$(vault write -field=token auth/jwt/login
      role=terraform
      jwt=$CI_JOB_JWT)
  - cd $TF_ROOT && terraform init

terraform-plan:
  stage: plan
  script:
    - terraform plan -out=tfplan
  artifacts:
    paths: [tfplan]

terraform-apply:
  stage: apply
  script:
    - terraform apply tfplan
  when: manual          # requires explicit trigger after plan review
  environment:
    name: $CLUSTER_NAME
  only:
    - main
```

Key details:
- Vault authentication uses **GitLab CI OIDC** — no static Vault token in CI variables
- `terraform apply` is `when: manual` — requires a human to review the plan and click apply
- The pipeline runs from `main` only — no one bootstraps clusters from feature branches
- Each cluster is its own GitLab CI environment — gives you deployment history and rollback

---

## Secret Management Alongside Flux

Flux applies what's in Git. Secrets in Git plaintext = bad. Three patterns used in production:

### Option 1 — SOPS (Secrets OPerationS)
Secrets are encrypted in Git using age or GPG. Flux decrypts at apply time using a key stored as a cluster secret.

```yaml
# In your gitops repo — encrypted with age, safe to commit
apiVersion: v1
kind: Secret
metadata:
  name: db-password
data:
  password: ENC[AES256_GCM,data:abc123...,type:str]
```

```yaml
# Kustomization tells Flux to decrypt SOPS secrets
spec:
  decryption:
    provider: sops
    secretRef:
      name: sops-age-key
```

### Option 2 — External Secrets Operator (ESO)
Flux deploys ESO. ESO pulls secrets from Vault/AWS SM/GCP SM at runtime and creates Kubernetes Secrets. Nothing sensitive is in Git.

```yaml
# In gitops repo — safe to commit, no actual secret value
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata:
  name: db-password
spec:
  secretStoreRef:
    name: vault-backend
    kind: ClusterSecretStore
  target:
    name: db-password
  data:
  - secretKey: password
    remoteRef:
      key: secret/teams/quant/db
      property: password
```

### Option 3 — SPIFFE/SPIRE (MacDonalds' approach)
Eliminates static secrets for workload-to-workload auth entirely. Apps present SPIFFE SVIDs (short-lived X.509 certs) instead of passwords. No secret to store, rotate, or leak.

---

## Homelab vs Enterprise — Full Comparison

| Concern | Homelab (what we did) | Enterprise |
|---------|----------------------|------------|
| Bootstrap trigger | Manual SSH + CLI | GitLab CI pipeline |
| Bootstrap tooling | `flux bootstrap` CLI | Terraform `flux_bootstrap_git` |
| Credentials | Personal PAT, hardcoded | Machine account, Vault-managed |
| Transport | HTTP | HTTPS (TLS) |
| Secret management | None | SOPS or ESO or SPIFFE |
| Multi-cluster | Single cluster | One repo, N cluster paths |
| Reproducibility | Manual, undocumented | Full IaC, re-runnable |
| Audit trail | None | Terraform state + MR history + CI logs |
| Token rotation | Manual | Vault dynamic secrets |
| Cluster death recovery | Re-run CLI manually | Re-run Terraform pipeline |

---

## TL;DR — The Three Key Differences

1. **Bootstrap is Terraform, not CLI** — `flux_bootstrap_git` resource, same as any other infrastructure component. Triggered by a CI pipeline on MR merge.

2. **Credentials come from Vault** — machine account SSH key, read from Vault at pipeline runtime. No personal tokens, no hardcoded secrets.

3. **One repo, many cluster paths** — `clusters/prod-eu-1/`, `clusters/dev/`, etc. Each cluster's Flux watches its own path. Shared infrastructure lives in `infrastructure/` and is referenced by each cluster's Kustomization.
