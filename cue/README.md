# CUE Lang — Lesson Plan & Deep Dive

> **Your lab:** GitLab CE at `192.168.1.26`, k3s cluster at `192.168.1.25`  
> **Why CUE matters for you:** MacDonalds uses CUE as the configuration layer across their entire PaaS stack — KubeVela component definitions, application manifests, policy enforcement. It touches everything.

---

## Why CUE Exists (Read This First)

At scale, YAML-based config management breaks down:

- **Helm** = text templating. Go templates in YAML. No type safety. A typo renders silently wrong YAML.
- **Kustomize** = patching. Better than Helm for overlays, but no validation, no abstraction.
- **Raw YAML** = copy-paste hell. 10 teams, 10 slightly-different Deployment templates.

CUE solves this by treating **configuration as a constraint satisfaction problem**. Instead of "fill in this template", you define "this value must satisfy these constraints" — and CUE tells you immediately if it doesn't.

Google ran a similar internal language (GCL) at scale for years. CUE's author (Marcel van Lohuizen) worked on that at Google, then built CUE as the open-source evolution of those ideas.

---

## The One Mental Model You Must Internalize

Everything in CUE is **unification** (`&`). When two constraints are unified, the result must satisfy both. If they're incompatible, CUE errors immediately.

```cue
// Unification examples — read these carefully
"hello" & string        // = "hello"  (concrete value satisfies type constraint)
int & 42                // = 42       (42 satisfies int)
>5 & <10                // = >5 & <10 (still a constraint range, not concrete)
>5 & <10 & 7            // = 7        (7 satisfies both bounds)
>5 & <10 & 15           // = ERROR    (15 violates <10)
string & 42             // = ERROR    (42 is not a string)
```

This is unlike any template language you've used. There's no "render" step that might fail silently — constraints either unify or they don't, and you find out immediately.

---

## Lesson Plan

### Module 1 — Mental Model & CLI Basics (Day 1)
**Goal:** Install CUE, understand unification, write your first schema and value.

Topics:
- Installing the `cue` CLI
- CUE's type system: `string`, `int`, `bool`, `float`, `bytes`, `null`
- Structs and fields
- Unification (`&`) — the core operation
- `cue eval`, `cue vet`, `cue export`

Lab exercise: Write a CUE schema for a Kubernetes Pod resource and validate a YAML file against it.

---

### Module 2 — Core Language Features (Day 2)
**Goal:** Write real schemas with constraints, optionals, defaults, and disjunctions.

Topics:
- Definitions (`#MySchema`) — schema vs concrete value
- Optional fields (`field?:`)
- Default values (`field: string | *"default"`)
- Disjunctions (`|`) — one of these values
- Comprehensions (like list/struct `for` loops)
- String interpolation (`"\(variable)"`)

Lab exercise: Write a CUE schema for a `#Deployment` that enforces: image must be non-empty, replicas between 1–10, resource limits required.

---

### Module 3 — Packages & Imports (Day 3)
**Goal:** Organise CUE across multiple files and packages.

Topics:
- CUE packages (`package myapp`)
- Importing stdlib (`encoding/json`, `encoding/yaml`, `list`, `strings`, `math`)
- Importing local packages
- How MacDonalds likely organises their CUE: platform schemas vs team configs

Lab exercise: Split a schema into a `platform` package (defines `#App`, `#Database`) and a `team` package that imports and uses it.

---

### Module 4 — CUE for Kubernetes (Day 4–5)
**Goal:** Replace YAML + Helm with CUE-generated manifests.

Topics:
- `cue export --out yaml` — generate Kubernetes YAML from CUE
- Importing existing YAML/JSON into CUE (`cue import`)
- Abstracting Deployment + Service + ConfigMap into a single `#App` definition
- How KubeVela uses CUE: component definitions, trait definitions, workflow steps
- The CUEiosphere: Timoni, CUE-generated Helm, etc.

Lab exercise (uses k3s): Write a `#App` CUE definition, fill in a concrete value for your demo app, `cue export` to YAML, and `kubectl apply` it to your k3s cluster.

---

### Module 5 — CUE + GitLab CI + FluxCD (Day 6–7)
**Goal:** Build the real pull-based GitOps loop MacDonalds uses — NOT push-based CI/CD.

**The key distinction:**
- ❌ Push model (not MacDonalds): CI runner does `kubectl apply` directly — CI has cluster credentials, pushes changes in
- ✅ Pull model (MacDonalds): CI validates and commits rendered YAML to Git — FluxCD on the cluster pulls and applies

```
Developer pushes CUE config to GitLab
          │
          ▼
GitLab CI: cue vet (validate schema)
          │
          ▼
GitLab CI: cue export → commit YAML to manifests/ in Git
          │
          ▼ (FluxCD polls GitLab every 1 minute)
FluxCD on k3s detects new commit in manifests/
          │
          ▼
FluxCD reconciles → cluster reaches desired state
```

The shell runner on jay2 never touches the cluster directly. It only validates and commits. The cluster drives itself.

Topics:
- Installing FluxCD on k3s, pointing at your GitLab instance (192.168.1.26)
- FluxCD `GitRepository` + `Kustomization` CRDs
- GitLab CI pipeline: validate CUE → export YAML → commit to `manifests/` branch
- FluxCD picking up the committed manifests automatically
- Why this model is safer: CI doesn't need cluster credentials

Lab exercise (uses GitLab + k3s + FluxCD):
1. Install FluxCD on k3s with a `GitRepository` pointing at your GitLab repo
2. Push a CUE project to GitLab
3. Write `.gitlab-ci.yml`: stage 1 = `cue vet`, stage 2 = `cue export` → git commit YAML to `manifests/`
4. Watch FluxCD reconcile the committed YAML onto k3s without the runner touching the cluster
5. Break your CUE → watch CI fail before any YAML is committed → cluster unchanged

This is exactly how MacDonalds' GitOps loop works.

---

### Module 6 — KubeVela + CUE (Day 7–8)
**Goal:** Understand how KubeVela's engine is CUE-native.

Topics:
- OAM model: Application → Component → Trait → Policy → Workflow
- ComponentDefinition's `schematic.cue` block — this IS CUE
- Writing a custom component definition
- How MacDonalds' platform team likely defines their abstractions in CUE

Lab exercise: Deploy KubeVela on k3s, define a custom `ComponentDefinition` using CUE, deploy an `Application` that uses it.

---

### Module 7 — MacDonalds-style Config Workflow (Day 9–10)
**Goal:** Simulate MacDonalds' actual multi-team GitOps workflow end-to-end.

Topics:
- Platform-owned schemas (the "contract")
- Team-owned configs (the "implementation")
- Policy-as-code with CUE: enforce resource limits, required labels, image registry allowlist
- Full loop: dev team writes CUE → CI validates against platform schema → Flux applies to cluster

Lab exercise: Simulate two "teams" (two GitLab projects), one owning the schema, one submitting a CUE config. The CI pipeline validates the config against the schema before allowing merge.

---

## Lab Infrastructure (Already Set Up)

| Component | Host | Status |
|-----------|------|--------|
| k3s v1.36.2 + Cilium | 192.168.1.25 (jay1) | Running |
| GitLab CE 19.1.1 | 192.168.1.26 (jay2) | Running |
| GitLab Runner (shell) | 192.168.1.26 (jay2) | Online |
| FluxCD v2.9.0 | k3s on jay1 | Bootstrapped |
| CUE CLI v0.17.0 | jay1 | Installed |

**FluxCD is bootstrapped against the local GitLab** (`http://192.168.1.26/root/homelab.git`, branch `main`, path `clusters/jay1`). Anything committed to that path is automatically reconciled onto k3s. This is your GitOps loop — ready to use from Module 5 onwards.

**Repo remotes on jay1:**
- `origin` → `github.com/jayrometan/homelab` (public notes backup)
- `local` → `http://192.168.1.26/root/homelab.git` (FluxCD watches this)

When doing lab exercises, push to `local` to trigger FluxCD reconciliation. Push to `origin` to keep GitHub in sync.

## Prerequisites — Install CUE CLI

Already installed on jay1 (`cue version v0.17.0`). To install elsewhere:

```bash
CUEVERSION=$(curl -s https://api.github.com/repos/cue-lang/cue/releases/latest | grep '"tag_name"' | cut -d'"' -f4)
curl -LO "https://github.com/cue-lang/cue/releases/download/${CUEVERSION}/cue_${CUEVERSION}_linux_amd64.tar.gz"
tar -xzf cue_${CUEVERSION}_linux_amd64.tar.gz && mv cue /usr/local/bin/
cue version
```

---

## Module 1 — Deep Dive: Mental Model & CLI

### The Type Lattice

CUE has a lattice of types. `_` (top) is the most general value — everything satisfies it. `_|_` (bottom) is an error — nothing satisfies it.

```
        _           ← top: every value satisfies this
       / \
    string  int   bool  ...
      |      |
   "hello"   42      ← concrete values at the bottom
```

When you unify two values, you move DOWN the lattice (more specific). If you can't go further without contradiction, you get `_|_` (error).

### Structs

```cue
// Open struct — allows additional fields
person: {
    name: string
    age:  int
}

// Closed struct (definition) — NO additional fields allowed
#Person: {
    name: string
    age:  int
}

// This would error with #Person (but not with person):
alice: #Person & {
    name:     "Alice"
    age:      30
    nickname: "Al"   // ERROR: field not allowed in closed struct
}
```

**Key rule:** Regular structs (`{}`) are open — extra fields allowed. Definitions (`#{}`) are closed — extra fields error. MacDonalds' schemas are definitions, ensuring teams can't sneak in arbitrary fields.

### Constraints on Values

```cue
// Numeric constraints
port:    >0 & <=65535
age:     >=0 & <=150
timeout: >0

// String constraints (using stdlib)
import "strings"
label:   strings.MinRunes(1)          // non-empty string
image:   strings.HasPrefix("registry.tower.internal/")  // enforce registry

// Enum-style (disjunction)
env: "dev" | "staging" | "prod"

// With default
replicas: >=1 & <=10 | *1             // default: 1
```

### Definitions — the Schema Pattern

```cue
// platform/schemas.cue
package platform

#App: {
    // Required fields — no defaults, must be provided
    name:  string
    image: string

    // Optional with defaults
    replicas: >=1 & <=10 | *1
    port:     >0 & <=65535 | *8080

    // Optional field (may be absent)
    env?: "dev" | "staging" | "prod"

    // Computed field — derived, cannot be overridden
    _fullName: "\(name)-app"
}
```

```cue
// teams/alpha/app.cue
package alpha

import "platform"

myApp: platform.#App & {
    name:  "market-data"
    image: "registry.internal/market-data:v1.2.3"
    replicas: 3
    env: "prod"
}
```

### The `cue` CLI Commands

```bash
# Evaluate and pretty-print result
cue eval ./...

# Validate data files against schema
cue vet schema.cue data.yaml

# Export to JSON
cue export --out json ./...

# Export to YAML
cue export --out yaml ./...

# Import existing YAML/JSON → CUE
cue import deployment.yaml

# Format CUE files (like gofmt)
cue fmt ./...

# Show all definitions
cue def ./...
```

---

## Module 1 — Lab Exercise

**Goal:** Validate a Kubernetes Deployment YAML using CUE.

### Step 1 — Create the project

On jay1:
```bash
mkdir -p ~/cue-lab/module1 && cd ~/cue-lab/module1
cue mod init homelab.local/module1
```

### Step 2 — Write the schema

```cue
// schema.cue
package module1

#Deployment: {
    apiVersion: "apps/v1"
    kind:       "Deployment"
    metadata: {
        name:      string
        namespace: string
        labels: [string]: string
    }
    spec: {
        replicas: >=1 & <=10
        selector: matchLabels: [string]: string
        template: {
            metadata: labels: [string]: string
            spec: containers: [...#Container]
        }
    }
}

#Container: {
    name:  string
    image: string
    ports?: [...{containerPort: >0 & <=65535}]
    resources?: {
        requests?: {cpu?: string, memory?: string}
        limits?:   {cpu?: string, memory?: string}
    }
}
```

### Step 3 — Write a valid value

```cue
// deployment.cue
package module1

myDeployment: #Deployment & {
    apiVersion: "apps/v1"
    kind:       "Deployment"
    metadata: {
        name:      "demo-app"
        namespace: "default"
        labels: app: "demo"
    }
    spec: {
        replicas: 2
        selector: matchLabels: app: "demo"
        template: {
            metadata: labels: app: "demo"
            spec: containers: [{
                name:  "demo"
                image: "nginx:alpine"
                ports: [{containerPort: 80}]
            }]
        }
    }
}
```

### Step 4 — Validate and export

```bash
# Validate (should pass)
cue vet ./...

# Export to YAML
cue export --out yaml -e myDeployment ./...

# Apply directly to k3s
cue export --out yaml -e myDeployment ./... | kubectl apply -f -

# Break it intentionally and see CUE catch it
# Change replicas: 15 and run:
cue vet ./...
# Error: spec.replicas: invalid value 15 (out of bound <=10)
```

---

## CUE vs Helm — Concrete Comparison

The same app, two approaches:

**Helm (values.yaml + template):**
```yaml
# values.yaml
replicaCount: 2
image:
  repository: nginx
  tag: alpine
```
```yaml
# templates/deployment.yaml
apiVersion: apps/v1
kind: Deployment
spec:
  replicas: {{ .Values.replicaCount }}  # no validation, any value works
  template:
    spec:
      containers:
      - image: "{{ .Values.image.repository }}:{{ .Values.image.tag }}"
```
If you pass `replicaCount: "two"` — Helm renders it, kubectl rejects it, you get a confusing error from the API server.

**CUE:**
```cue
#App: {
    replicas: >=1 & <=10   // validated before anything leaves your machine
    image: string
}
myApp: #App & {
    replicas: "two"        // ERROR immediately: cannot use value "two" (type string)
    image: "nginx:alpine"  //                    as int
}
```
CUE catches it at `cue vet`. The API server never sees it.

---

## Sharp Edges

**1. CUE is not Turing-complete by design**
No recursion, no loops (only comprehensions over known sets). This makes CUE configs always terminate and always be analyzable — intentional, not a limitation.

**2. Open vs closed structs will bite you**
Regular `{}` allows extra fields silently. `#{}` (definition) rejects them. When validating Kubernetes YAML, use definitions — otherwise unknown fields pass through.

**3. `cue export` flattens everything**
If you have multiple top-level values in a package, `cue export` errors unless you specify `-e fieldName`. Structure your CUE so each file has one top-level exportable value, or use `-e`.

**4. Package paths are module-relative**
`import "platform"` won't work unless your `cue.mod/module.cue` defines the module correctly and `platform/` is a package inside that module.

**5. Disjunctions with defaults**
`string | *"default"` means: accept any string, default to "default". But once you specify a concrete value, the default is gone. You can't "reset to default" — the concrete value wins.

**6. CUE tooling (cue/tool) is separate from CUE evaluation**
`cue cmd` lets you define custom commands (like `cue cmd apply`). This is how teams build `cue cmd deploy` workflows. It's a separate layer from the core language — don't confuse them early.

---

## How This Fits MacDonalds' Stack

```
Developer writes CUE
  │  (references platform #App schema)
  ▼
GitLab CI: cue vet ./...
  │  (validates against platform schema)
  ▼
GitLab CI: cue export --out yaml
  │  (generates Kubernetes manifests)
  ▼
GitLab CI: kubectl apply / git commit manifests
  │
  ▼
FluxCD detects manifest change in Git
  │
  ▼
FluxCD reconciles → KubeVela processes Application CRD
  │  (KubeVela's internals use CUE to render component definitions)
  ▼
Kubernetes cluster reaches desired state
```

CUE is the **contract layer** — it's what ensures that what a dev team writes is actually valid before it ever touches a cluster. The platform team owns the schemas (`#App`, `#Database`, `#SGCluster`), teams fill in the values.

**Important:** The GitLab CI runner (jay2) never runs `kubectl apply`. It only validates CUE and commits rendered YAML back to Git. FluxCD on k3s pulls from GitLab and applies — the cluster is always driving itself to match Git state. This separation is fundamental to GitOps.

---

## Resources

- Official docs: https://cuelang.org/docs/
- CUE playground (browser): https://cuelang.org/play/
- CUE + Kubernetes tutorial: https://cuelang.org/docs/integrations/kubernetes/
- KubeVela CUE internals: https://kubevela.io/docs/platform-engineers/cue/basic
- `cuetorials.com` — best practical CUE tutorials outside the official docs
