---
title: "CUE for the PaaS Stack"
subtitle: "Configuration Authoring Under KubeVela and Kubernetes — A Reference Booklet"
author: "Platform Engineering Reference Series"
date: "July 2026"
toc: true
toc-depth: 2
numbersections: true
papersize: a4
fontsize: 10pt
mainfont: "DejaVu Serif"
sansfont: "DejaVu Sans"
monofont: "DejaVu Sans Mono"
monofontoptions: "Scale=0.85"
geometry: "margin=2.2cm"
highlight-style: tango
header-includes:
  - \usepackage{fvextra}
  - \DefineVerbatimEnvironment{Highlighting}{Verbatim}{breaklines,breakanywhere,commandchars=\\\{\}}
  - \usepackage{fancyvrb}
  - \fvset{breaklines=true,breakanywhere=true}
---

\newpage

# Introduction: What This Booklet Covers and What It Deliberately Ignores

CUE at the firm lives in exactly one layer: the application-delivery configuration layer that sits under KubeVela and above Kubernetes. Platform engineers author `ComponentDefinition`s, `TraitDefinition`s, `WorkflowStepDefinition`s, and `Policy` definitions in CUE; application teams supply concrete values that flow through those definitions; KubeVela's controller evaluates the CUE at reconcile time and emits Kubernetes objects. CUE also appears at author time in GitLab CI, where `cue vet` and `cue export` gate merge requests before Flux ever sees a commit.

This booklet does not cover CUE as a general-purpose infrastructure configuration tool — no server provisioning, no Terraform replacement talk, no `cue cmd` scripting frameworks for fleet management. Your team does not own that layer. Everything here is scoped to: platform team writes and locks down CUE schemas; app teams fill in leaf values; KubeVela renders; Kubernetes runs it.

The single most important thing to internalize before page one: **CUE is not a templating language and it is not a merge engine.** If you approach it with Helm's or Kustomize's mental model — "layer things and the later thing wins" — CUE will fight you constantly, and its errors will look insane. If you approach it as a constraint system — "every file adds constraints, and the answer is whatever satisfies all of them simultaneously" — it becomes predictable and, frankly, a much better fit for a platform team enforcing invariants against internal customers than anything in the Helm lineage.

**Ownership boundary, stated up front and repeated throughout:** the platform team owns closed `#Definition`s — the schemas, the constraints, the defaults, the shape of what is allowed. App teams own concrete leaf values that must fit inside those schemas. In CUE this boundary is not a convention or a code-review policy; it is enforced by the evaluator itself. That is the entire reason this stack uses CUE.

\newpage

# CUE's Core Mental Model: Unification, Not Templating, Not Merging

## The lattice, in practical terms

Every CUE value lives on a spectrum from *completely unconstrained* to *fully concrete*. At the top sits `_` (called "top"), which means "any value at all." At the bottom sits `_|_` ("bottom"), which means "no value can satisfy this" — an error. In between live types (`string`, `int`), constraints (`>=100`, `=~"^app-"`), and concrete values (`"trading-gateway"`, `4096`).

The one operation that matters is **unification**, written `&`. Unifying two values produces the *most general value that satisfies both*. It is set intersection over the space of allowed values:

```cue
// string & "gateway"  →  "gateway"     (the concrete value fits the type)
// int & >=100 & <=200 & 150  →  150    (150 satisfies all constraints)
// "gateway" & "sidecar"  →  _|_        (no value is both; conflict)
```

Three properties follow immediately, and they are the properties that make CUE behave nothing like the tools you know:

**Commutative.** `a & b` is `b & a`. There is no "later wins." File order does not matter. Declaration order does not matter. This is the first thing that breaks the Helm/Kustomize intuition.

**Idempotent.** `a & a` is `a`. Saying the same thing twice is harmless. Two files can both declare `replicas: 3` and nothing conflicts — they agree.

**Associative.** Grouping doesn't matter. You can split configuration across ten files and the evaluator composes them in any order with an identical result.

## Contrast with Helm: text templating

Helm's `{{ .Values.replicas }}` is string interpolation into YAML text. Helm has no idea what a Deployment is; it knows what a byte stream is. Every Helm failure mode you have ever debugged — whitespace-sensitive `nindent` bugs, quoting errors, `toYaml` producing subtly wrong indentation, a typo'd values key silently rendering as empty string — comes from the fact that Helm operates *below* the data model. Validation happens, if at all, after rendering, when the API server rejects the output.

CUE operates entirely *inside* the data model. There is no render step that can produce syntactically valid but semantically garbage YAML, because there is no text substitution. A typo'd field against a closed schema is an evaluation error at `cue vet` time, before anything is exported. Think of the difference between HAProxy config generated by `sed` versus config validated by `haproxy -c` — except in CUE, the equivalent of `-c` runs on every evaluation, structurally, against a schema you wrote.

**Helm also has no concept of a constraint.** A Helm chart cannot say "replicas must be between 1 and 20 and memory limits are mandatory." It can only interpolate whatever it's given, or fail with a template error via `required`. CUE schemas *are* constraints; the enforcement is the evaluation.

## Contrast with Kustomize: strategic merge patches

Kustomize is closer — it operates on structured data, not text. But its core operation is the *patch*: an overlay that overrides base fields, with "last layer wins" semantics governed by per-field merge strategies (replace, merge, delete directives). Two consequences:

1. **Order matters.** The overlay stack is a pipeline; reordering changes the output.
2. **Overrides are silent.** An overlay can stomp a base value and nothing tells you. The base author's intent is advisory.

In CUE, an "overlay" cannot override anything. If the base says `replicas: 3` and another file says `replicas: 5`, that is not an override — it is a *conflict*, and evaluation fails with `_|_`. If the base wants to permit variation, it must say so explicitly: `replicas: int & >=1 & <=20` leaves room for an app team to supply a concrete value, and `replicas: *3 | int` supplies a default that concrete values may replace. The base author decides, in the schema, exactly how much freedom downstream consumers get. This inversion — *variation is granted by the schema owner, not seized by the overlay author* — is the ownership boundary mechanism your platform team relies on.

## Contrast with HCL: override semantics and evaluation order

HCL/Terraform sits in a middle ground you know well: it is declarative-ish, but has genuinely imperative corners — `override.tf` files, variable precedence order (env vars beat tfvars beat defaults), `locals` computed in dependency order, `count`/`for_each` meta-arguments. Terraform's answer to "two places define the same thing" is a documented precedence chain. You have memorized that chain; it is load-bearing knowledge.

CUE has no precedence chain to memorize, because there is no precedence. Two definitions of the same field either agree (unify to something non-bottom) or the configuration is invalid. The closest CUE gets to precedence is the *default marker* `*`, and even that is not "this file wins" — it is "in the absence of any other concrete value, use this." Defaults are part of the value lattice, not an evaluation-order trick.

One more HCL contrast worth internalizing: HCL separates types (in `variable` blocks) from values (in tfvars). CUE does not. Which brings us to the type system.

**PROD:** The most common early mistake for engineers from the Helm/Kustomize/HCL world is writing a second file to "override" a platform value and being confused when CUE reports a conflict. The error is not a bug and not a syntax problem — it is CUE telling you that the platform schema did not grant that freedom. The fix is never "find the override syntax" (there isn't one); it is either supplying a value the schema permits, or changing the schema — which is a platform-team change, deliberately.

\newpage

# The Type System: Types Are Values

## The spectrum

CUE collapses the type/value distinction. `string` is a value — an abstract one that admits every string. `"gateway"` is a value — a concrete one that admits exactly itself. `=~"^svc-"` is a value — a constraint admitting strings matching the regex. They all live on the same lattice and all compose with `&`. "Type checking" in CUE is just unification: `"gateway" & string` succeeds, `"gateway" & int` is bottom.

This means a schema, a constraint set, and a partially-filled config are the same kind of artifact. A platform `#Definition` is simply a value that is not yet fully concrete. App teams make it concrete. Export requires full concreteness — `cue export` fails if any required field is still abstract, which is exactly the property you want in CI: a half-configured application cannot render.

```cue
#Resources: {
	cpu:    string & =~"^[0-9]+m?$"        // constraint: k8s CPU quantity shape
	memory: string & =~"^[0-9]+(Mi|Gi)$"   // constraint: k8s memory quantity shape
}
```

## Bounds and constraints

Numeric bounds are first-class values: `>=1`, `<=20`, `>0 & <1024`. String constraints via regex: `=~"pattern"`, `!~"pattern"`. These compose like everything else:

```cue
replicas: int & >=1 & <=20
port:     int & >1024 & <65536
env:      "dev" | "staging" | "prod"
```

That last line is a **disjunction** — a sum type. `|` means "any of these." Unify with a concrete value and only matching branches survive: `("dev" | "prod") & "prod"` is `"prod"`. Unify with something matching no branch and you get bottom with a per-branch explanation of why each failed.

## Defaults

A default is a disjunction with a marked preferred branch:

```cue
replicas: *3 | int      // default 3; any int accepted
logLevel: *"info" | "debug" | "warn" | "error"
```

Semantics: if evaluation ends and no concrete value has been unified in, the default is chosen. If a concrete value *is* supplied, it must still satisfy the disjunction, and it simply wins over the marker — this is the only "override-like" behavior in CUE, and note who controls it: the schema author wrote the `*`. The platform team grants the default; the app team may replace it *within the allowed set*. Compare Vault policy design: the policy author enumerates capabilities; the client can act within them but cannot extend them. Same governance shape.

**PROD:** A subtle trap: `*3 | int` and `3 | int` are different. Without the `*`, an unresolved disjunction is not concrete, and `cue export` fails with "incomplete value." If app teams report "it worked in vet but export says incomplete," a missing default marker in a platform schema is a frequent cause. `cue vet` checks consistency; `cue export` additionally demands concreteness.

## Optional and required fields

```cue
#Component: {
	name:      string          // required (must become concrete)
	sidecar?:  string          // optional: may be absent entirely
	replicas!: int             // explicitly required (v0.6+ syntax)
}
```

`?` marks a field that may be omitted. Inside definitions, plain fields are effectively required-for-export anyway; the `!` marker (required fields, introduced around CUE v0.6) makes the requirement explicit at vet time rather than surfacing only as an incompleteness at export. For platform schemas, prefer `!` on fields where you want app teams to get a loud, early "you must set this" rather than a late "incomplete value" — the error quality difference matters when your internal customers are quants, not YAML whisperers.

## Structs unify recursively

Unification descends into structs field by field:

```cue
a: {replicas: >=1, image: string}
a: {replicas: 3,   image: "registry.internal/gw:1.4"}
// a is {replicas: 3, image: "registry.internal/gw:1.4"}
```

Both declarations of `a` are true simultaneously; the result satisfies both. This is how a platform schema file and an app-team values file combine without any explicit merge call, patch directive, or import of one into the other's namespace — they simply both constrain the same path.

\newpage

# Closedness: The Ownership Boundary Mechanism

## Open structs vs closed structs vs definitions

A plain CUE struct is **open**: unifying it with a struct containing new fields simply adds those fields.

```cue
a: {x: 1}
a: {y: 2}      // fine; a is {x: 1, y: 2}
```

A **definition** — any field whose name starts with `#` — is **closed** by default: unification may refine existing fields but may not introduce new ones.

```cue
#A: {x: int}
b: #A & {x: 1}          // ok
c: #A & {x: 1, y: 2}    // _|_ : field y not allowed in closed struct
```

This is the single most important feature for your role. Read it as an access-control statement: **a `#Definition` is the platform team declaring the complete universe of what an app team may express.** Typos become errors instead of silently ignored fields (the classic Helm values-file failure: `replcias: 5` interpolates as nothing and you find out during the incident). Unknown fields become errors instead of unreviewed API surface.

The analogy to Consul/Vault ACL design is genuinely apt: an open struct is a default-allow policy; a closed definition is default-deny with an explicit allowlist. A platform team enforcing blast-radius discipline wants default-deny at the schema layer for the same reason it wants it at the network layer.

## Deliberately opening holes

Sometimes the platform genuinely wants app teams to attach arbitrary content in one spot — labels, annotations, env vars. Closedness is per-struct, so you open exactly the hole you intend:

```cue
#Workload: {
	name:     string & =~"^[a-z][a-z0-9-]{2,40}$"
	replicas: int & >=1 & <=20
	labels: [string]: string      // pattern constraint: any label key, string values
	env: [string]: string          // open map, but values must be strings
	...                            // (only if you want the whole struct open — usually you don't)
}
```

The `[string]: string` form is a *pattern constraint*: it doesn't open the struct to anything, it says "fields matching this name pattern are allowed and must satisfy this value constraint." That is the right tool for labels/env. The `...` ellipsis opens a definition entirely and should be rare in platform schemas — it surrenders the typo protection.

**PROD:** Every `...` in a platform `#Definition` is a small governance hole: fields flowing through it are unvalidated surface that app teams will start depending on. Treat adding `...` to a shared definition with the same seriousness as adding a `*` capability to a Vault policy. Grep for it in review.

## Closedness error messages

The error you and your app teams will see most:

```
field not allowed: sidecarImage:
    ./platform/workload.cue:3:1
    ./teams/statarb/app.cue:12:2
```

Decoding: the first location is *where the closed definition was declared* (the schema that forbids the field); the second is *where the offending field was written*. New CUE users read this as "there's an error in the platform file" and file tickets against your team. The platform file is fine; it is being cited as the authority. Teach this reading early — it will cut your inbound support load measurably.

## What the platform locks down vs what stays open — the standing pattern

Stated once more, because it is the design center of the whole stack:

- **Platform-owned (closed, in `#Definitions`):** field universe, types, bounds, naming regexes, mandatory fields (resource limits, probes, security context), defaults, the mapping from abstract spec to concrete Kubernetes objects.
- **App-team-owned (leaf values):** concrete values within the granted ranges — image tag, replica count within bounds, env vars through the pattern-constraint hole, which optional traits to attach.
- **Nobody:** overriding. The concept does not exist. Changes to what is allowed are schema changes, made in platform-owned files, reviewed by the platform team, rolled out deliberately.

\newpage

# Composition Patterns: Layering Instead of Conditionals

## General-to-specific across files

Because unification is order-independent and idempotent, the idiomatic structure is layers of increasingly specific constraint, each in its own file, all in the same package:

```
platform/
  schema.cue        // #Workload definition: types, bounds, closedness
  defaults.cue      // *defaults for the org
  prod.cue          // tighter constraints applied in prod contexts
teams/statarb/
  app.cue           // concrete leaf values
```

```cue
// schema.cue
package delivery
#Workload: {
	replicas: int & >=1 & <=50
	image:    string & =~"^registry\\.internal/"
}

// defaults.cue
package delivery
#Workload: replicas: *2 | int

// prod.cue  (loaded only for prod evaluation)
package delivery
#Workload: replicas: >=2        // no single-replica prod workloads
```

Note what is absent: no `if env == "prod"` wrapping the whole config, no values-file precedence, no overlay ordering. Each file states facts; prod evaluation includes the prod facts. The prod file cannot *loosen* anything (loosening would require removing a constraint, which means editing the platform file) — it can only tighten. Constraints are monotonic; adding a file can never make an invalid config valid. That property is worth pausing on: it means reviewing a change requires reading only the diff, not re-deriving the whole precedence stack. Compare debugging a Kustomize output where you must mentally replay every overlay in order.

CUE does have `if` for genuine structural conditionality inside definitions (used heavily in KubeVela templates, next chapter), but *environment variation* is expressed as layered facts, not branching.

## Packages, modules, imports

- A **package** is a set of `.cue` files sharing a `package name` clause; all files in a directory with the same package clause are unified together automatically. This is the mechanism behind the layering above — no import needed between files of one package.
- A **module** is the repo-level unit, declared in `cue.mod/module.cue` (e.g. `module: "internal.corp/platform"`), giving imports an absolute root.
- **Imports** reference packages by path: `import "internal.corp/platform/delivery"`, then use exported (capitalized or `#`-prefixed) identifiers. Since CUE v0.8+ the module system supports OCI-registry-backed dependencies (`cue mod tidy`, registry config via `CUE_REGISTRY`) — relevant because your Artifactory instance can serve CUE modules the same way it serves OCI images, versioned and immutable, which is how a platform team should ship schema releases to consuming repos rather than via git submodules or copy-paste.

**PROD:** Version your schema module and publish tagged releases. App repos pinning `internal.corp/platform/delivery@v1.4.0` gives you Consul-style config versioning discipline: schema tightening ships as a version bump that teams adopt on their own migration schedule (with a deprecation window), instead of a change to a shared branch that breaks every team's CI simultaneously at 9am. Schema changes are API changes. Treat them with the same ceremony.

\newpage

# CUE in KubeVela: Definitions, Traits, Policies

## Where CUE actually executes

KubeVela's controller embeds a CUE evaluator. Platform-authored `ComponentDefinition`/`TraitDefinition` CRs contain CUE templates as strings in `spec.schematic.cue.template`. When an `Application` CR reconciles, the controller unifies each component's template with a `parameter` struct built from the Application's `properties`, plus a `context` struct it injects (app name, namespace, revision, etc.), then materializes the `output`/`outputs` fields as Kubernetes objects and applies them. So: **CUE evaluation happens inside the KubeVela controller at reconcile time** — in-cluster, on every reconcile, against the vendored CUE runtime KubeVela was compiled with. (Author-time evaluation in CI is a separate, earlier gate — chapter 9.)

## A real ComponentDefinition, line by line

```cue
apiVersion: "core.oam.dev/v1beta1"
kind:       "ComponentDefinition"
metadata: {
	name:      "internal-service"
	namespace: "vela-system"
	annotations: "definition.oam.dev/description": "Stateless internal service, firm baseline"
}
spec: {
	workload: definition: {apiVersion: "apps/v1", kind: "Deployment"}
	schematic: cue: template: """
		// ---- parameter: the app team's entire API surface ----
		parameter: {
			image:    string & =~"^registry\\\\.internal/"   // must come from Artifactory
			replicas: *2 | int & >=1 & <=20
			port:     *8080 | int & >1024 & <65536
			env: [string]: string                            // open hole, values typed
			resources: {
				cpu:    *"500m" | string & =~"^[0-9]+m?$"
				memory: *"512Mi" | string & =~"^[0-9]+(Mi|Gi)$"
			}
			exposeMetrics: *true | bool
		}
		// ---- output: the primary workload object ----
		output: {
			apiVersion: "apps/v1"
			kind:       "Deployment"
			metadata: labels: "app.oam.dev/component": context.name
			spec: {
				replicas: parameter.replicas
				selector: matchLabels: app: context.name
				template: {
					metadata: labels: app: context.name
					spec: containers: [{
						name:  context.name
						image: parameter.image
						ports: [{containerPort: parameter.port}]
						env: [for k, v in parameter.env {name: k, value: v}]
						resources: {
							requests: {cpu: parameter.resources.cpu, memory: parameter.resources.memory}
							limits:   {cpu: parameter.resources.cpu, memory: parameter.resources.memory}
						}
						securityContext: {
							runAsNonRoot:             true    // not parameterized: policy, not preference
							allowPrivilegeEscalation: false
						}
					}]
				}
			}
		}
		// ---- outputs: auxiliary objects, conditionally ----
		outputs: {
			if parameter.exposeMetrics {
				service: {
					apiVersion: "v1"
					kind:       "Service"
					metadata: name: context.name
					spec: {
						selector: app: context.name
						ports: [{port: parameter.port, targetPort: parameter.port}]
					}
				}
			}
		}
		"""
}
```

Annotations on the load-bearing lines:

- **`parameter`** is the contract. Everything the app team can say is here; everything else in the template is platform territory they cannot reach. The Application CR's `properties` block is unified against `parameter` — a property not declared here is `field not allowed`. This is closedness doing its job at the KubeVela layer.
- **Every parameter carries a constraint and most carry a default.** The image regex enforces Artifactory provenance in the schema, not in an admission webhook you have to run and monitor separately. Defense in depth says keep the webhook too; but the schema catches it in CI, weeks earlier.
- **`context`** is controller-injected (name, namespace, appRevision, workflow context…). Templates should derive identity from `context`, never from parameters, so app teams cannot mislabel workloads.
- **`securityContext` is hardcoded.** Deliberate. If it were a parameter, it would be negotiable. What is absent from `parameter` is as much a design decision as what is present.
- **The `for` comprehension** turns the open `env` map into the list shape Kubernetes wants — structural transformation without text templating.
- **`if parameter.exposeMetrics`** is CUE's structural conditional: the Service struct exists or does not. This is the legitimate home of `if` — shaping *output structure* — as opposed to environment branching, which stays in layered files.

## TraitDefinitions and Policies, briefly

A **TraitDefinition** is the same schematic pattern, but instead of producing a workload it produces auxiliary objects (`outputs`) and/or **patches the component's workload** via a `patch` field, which is — unusually for CUE — a genuine strategic-merge-flavored mechanism KubeVela layers on top (with `patchKey` annotations for list merging). Traits are how you ship opt-in platform capabilities: a `gateway` trait emitting Cilium Gateway API `HTTPRoute`s, a `scaler` trait patching replicas, an `otel` trait injecting collector sidecar env. Ownership boundary again: platform writes the trait; app team writes `type: gateway, properties: {...}` — attachment plus leaf values, nothing more.

**Policies** (e.g. `topology`, `override`, `apply-once`) govern multi-cluster placement and delivery behavior at the Application level. Note the irony without confusion: KubeVela's `override` *policy* is a KubeVela-level concept for per-cluster property variation — it operates before/around CUE unification, not as an exception to it.

**PROD:** Trait `patch` conflicts are a real category of incident: two traits patching the same workload path produce either a CUE conflict (rendering fails, Application unhealthy) or, with list `patchKey` misuse, silently duplicated entries (e.g. two identical volume mounts → pod rejected by kubelet). When an Application goes unhealthy right after "just adding a trait," diff the rendered output of the component with each trait enabled in isolation via `vela dry-run`.

\newpage

# CLI Reference: The Commands That Matter and Where They Run

Two execution contexts, keep them straight: **author time** (your laptop, GitLab CI) uses the standalone `cue` binary; **apply time** (KubeVela controller reconciling) uses the CUE runtime *vendored inside KubeVela* — a version you do not choose independently. The CLI reference below is the author-time world.

## `cue vet` — validate

```
cue vet ./...                          # vet every package in the module
cue vet -c ./teams/statarb/            # -c: require full concreteness
cue vet schema.cue data.yaml           # validate YAML/JSON data files against CUE schema
cue vet -d '#Workload' schema.cue app.yaml   # -d: pick the definition to validate against
```

The workhorse. Without `-c`, vet checks *consistency* (no conflicts); with `-c` it additionally checks *concreteness* (exportable). CI should run both, in that order — a consistency failure and an incompleteness failure mean different things to the fixing engineer, and separating them makes pipeline output legible. `cue vet -d '#SomeDef' schema.cue file.yaml` is also your cheap gift to app teams: schema-validating raw YAML without converting anything.

## `cue eval` — inspect

```
cue eval ./...                         # human-oriented CUE-syntax output
cue eval -e 'appConfig.replicas' .     # -e: evaluate one expression/path
cue eval --all                         # include optional/hidden fields
cue eval -c                            # demand concrete result
```

Debugging tool, not an export path. Output is CUE syntax, shows unresolved constraints and defaults (marked with `*`) — exactly what you need when asking "what does the evaluator think this field is right now." Reach for `-e` the way you reach for `jq` on a big blob.

## `cue export` — render

```
cue export ./... --out yaml            # the render step; JSON by default
cue export -e output --out yaml        # export one expression
cue export --out yaml -o rendered.yaml # write to file
```

Export demands full concreteness — any incomplete field is a hard error listing the offending paths. In this stack you export mostly as a CI verification artifact and for `vela dry-run`-style inspection; the production render happens inside KubeVela, not from your exported files. Keep that distinction sharp when reasoning about "but it exported fine on my machine."

## `cue fmt`, `cue def`, `cue trim`, `cue import`, `cue mod`

```
cue fmt ./...            # canonical formatting; run as CI check (cue fmt --check on v0.11+; otherwise fmt + git diff --exit-code)
cue def ./...            # output the schema view; good for docs/review of the effective API
cue def -e '#Workload'   # show one definition's full, composed shape
cue trim ./...           # remove fields already implied by definitions/defaults — makes app files minimal
cue import k8s.yaml      # convert YAML/JSON into .cue files (bootstrap; imports existing manifests)
cue import -f -l 'strings.ToLower(kind)' -l 'metadata.name' k8s.yaml   # -l: place docs at computed paths
cue mod init internal.corp/platform    # create cue.mod
cue mod tidy             # resolve/pin module deps (registry-backed, v0.8+)
cue mod publish v1.4.0   # publish module version to OCI registry (Artifactory)
```

`cue trim` deserves a highlight: run it over app-team files and everything already guaranteed by the platform schema disappears, leaving only the deltas — the config equivalent of `terraform fmt` plus dead-code elimination. Smaller app files means the review surface is exactly the team's actual decisions.

## GitLab CI shape

```yaml
cue-validate:
  stage: validate
  image: registry.internal/tooling/cue:0.11   # PIN THIS — see sharp edges
  script:
    - cue fmt ./... && git diff --exit-code    # formatting drift fails the MR
    - cue vet ./...                            # consistency across the module
    - cue vet -c ./apps/...                    # concreteness where concreteness is due
    - cue export ./apps/... --out yaml -o /dev/null   # proves renderability
```

This pipeline is the author-time gate: it runs before merge, therefore before Flux sees the commit, therefore before KubeVela evaluates anything in-cluster. The entire point is that the class of error which in Helm-world surfaces as a failed deployment surfaces here as a red MR.

**PROD:** Pin the CUE image tag and treat CLI upgrades as a change with a rollout, not a `latest` drift. The evaluator has meaningfully changed behavior across versions (v0.6 required fields, v0.7+ evaluator changes, v0.8 modules, the v0.9–0.11 new evaluator work) — a silent CLI bump can flip CI from green to red across every repo in one morning, or worse, green in CI while the older evaluator inside KubeVela disagrees at reconcile time.

\newpage

# Worked Examples

## Example 1: platform resource schema with validation

The platform file:

```cue
package delivery

#ResourceSpec: {
	cpu:    string & =~"^[0-9]+m$" | string & =~"^[0-9]+$"
	memory: string & =~"^[0-9]+(Mi|Gi)$"
}

#Workload: {
	name:     string & =~"^[a-z][a-z0-9-]{2,40}$"
	replicas: *2 | int & >=1 & <=20
	requests: #ResourceSpec
	limits:   #ResourceSpec
	// firm invariant: limits.memory == requests.memory (no memory overcommit surprises)
	limits: memory: requests.memory
}
```

That last line is a technique with no Helm/Kustomize equivalent: a *cross-field constraint*. It does not "copy" requests into limits; it states that the two paths must unify. If an app team writes `limits: memory: "2Gi"` while `requests: memory: "1Gi"`, the config is invalid — the invariant is enforced by evaluation, not by a code-review checklist item someone forgets during an incident-driven Friday deploy.

App team file:

```cue
package delivery

gw: #Workload & {
	name: "exec-gateway"
	requests: {cpu: "500m", memory: "1Gi"}
	limits: cpu: "1000m"
	// replicas: absent → default 2
	// limits.memory: absent → forced to "1Gi" by the cross-field constraint
}
```

`cue export -e gw --out yaml` renders with `replicas: 2` and `limits.memory: "1Gi"` filled in. The app team wrote seven lines; the platform guaranteed everything else.

## Example 2: a unification conflict, decoded

App team attempts the classic "override":

```cue
// platform/prod.cue
package delivery
#Workload: replicas: int & >=2 & <=20

// teams/statarb/app.cue
package delivery
app: #Workload & {
	name:     "signal-feed"
	replicas: 1        // "it's just a canary, one replica is fine"
	requests: {cpu: "250m", memory: "512Mi"}
	limits: cpu: "250m"
}
```

The error:

```
app.replicas: invalid value 1 (out of bound >=2):
    ./platform/prod.cue:3:23
    ./teams/statarb/app.cue:5:12
```

Line by line: the **path** (`app.replicas`) tells you which field died. The **message** names the concrete value and the specific bound it violates — not "conflict" in general, the exact failed constraint. The **first location** points at the constraint's declaration (platform file, the authority); the **second** points at the concrete value (app file, the violation). Nothing here is a syntax error, though the format panics newcomers. The fix is a conversation, not code: either the canary belongs in a non-prod evaluation context where `>=2` isn't asserted, or the platform relaxes the bound — a platform decision, in a platform file.

Contrast with struct conflicts between two concrete values:

```
app.image: conflicting values "registry.internal/feed:1.2" and "registry.internal/feed:1.3":
    ./teams/statarb/app.cue:4:9
    ./teams/statarb/canary.cue:2:9
```

Two files in the same package both made `image` concrete, differently. In Kustomize this would be a silent last-layer-wins; in CUE it is an error demanding the humans decide which fact is true. When you see `conflicting values X and Y`, the question is never "which wins" — it is "why do two files disagree about reality."

## Example 3: disjunction failure readout

```cue
env: "dev" | "staging" | "prod"
env: "produciton"
```

```
env: 3 errors in empty disjunction:
env: conflicting values "dev" and "produciton"
env: conflicting values "staging" and "produciton"
env: conflicting values "prod" and "produciton"
```

"Empty disjunction" = every branch failed; CUE then shows you why each one did. On large disjunctions (KubeVela templates contain some) this output gets long — read the branch list for the one that *almost* matched; nine times in ten it is a typo like this one.

\newpage

# Sharp Edges and Production Gotchas

## 1. Conflicts that read like syntax errors

Covered above but worth its own heading because it is the number-one onboarding friction: `field not allowed`, `conflicting values`, `out of bound`, and `incomplete value` are *semantic* verdicts about your configuration's consistency, presented in a compiler-ish costume. Triage rule: two source locations in the error means unification conflict — go read *both* files before touching either.

## 2. "How do I override this?" — the wrong question

App teams coming from Helm will ask for the override mechanism. The honest answer: there isn't one, on purpose, and the request usually decodes to one of three real needs — (a) the schema's bound is genuinely too tight → platform schema change with review; (b) the value varies by environment → move it into a layered environment file, not an override; (c) the team wants to escape a policy → that is a governance conversation, and the fact that CUE forces it into the open instead of letting an overlay silently win is a feature your team should defend, in exactly the way you would defend a Vault deny policy against a "can you just add a wildcard" ticket.

## 3. Incomplete vs invalid

`cue vet` green + `cue export` red confuses everyone once. Vet (without `-c`) proves *no contradictions*; export demands *full concreteness*. A field left as bare `int` is consistent but unexportable. Symptoms: `incomplete value int` with a path. Cause is usually a missing required leaf value, or a default marker `*` the platform forgot. Put `cue vet -c` in CI on app packages and this class dies before merge.

## 4. Defaults across disjunction composition

Defaults interact subtly when disjunctions unify with disjunctions (default markers can be eliminated in the intersection, yielding a value that no longer has a default and suddenly reports incomplete). Keep platform defaults on leaf fields, singular and simple; avoid nesting defaulted disjunctions inside defaulted disjunctions. If a previously-defaulting field starts demanding concreteness after a schema refactor, this is the first suspect.

## 5. Performance on large evaluation graphs

CUE evaluation is not free. Comprehensions over large lists, deeply nested disjunctions, and huge unified packages (thousands of manifests in one module) can push evaluation from milliseconds to minutes in pathological shapes — the old evaluator had known super-linear cases; the v0.9+ evaluator rework substantially improved but did not repeal this. Practical hygiene: keep app packages small and per-team rather than one firm-wide mega-package; avoid disjunctions where a pattern constraint expresses the same thing; if CI vet time creeps up, bisect with `cue eval -e` on subpaths before blaming the runner. Inside KubeVela the same applies per-Application — grotesque templates slow every reconcile of every app using that definition, which on a controller reconciling hundreds of Applications is a platform-wide latency you inflicted on yourself.

## 6. String templates inside YAML inside CRs

ComponentDefinitions embed CUE as a *string* inside a YAML CR. Consequences: your editor doesn't syntax-check it, escaping gets hairy (note the quadruple backslash in the image regex earlier — regex escaping through CUE string through YAML string), and a template typo becomes a runtime rendering failure rather than an authoring error. Mitigation that mature setups use: keep definitions as native `.cue` files in git, validated by real `cue vet` in CI, and generate/apply the CR with `vela def apply` (which wraps them) — never hand-edit templates inside YAML. `vela def vet` exists; use it in the pipeline for definition repos.

## 7. CLI vs vendored-runtime version skew

The `cue` binary in CI and the CUE runtime compiled into your KubeVela release are independent artifacts. Skew failure modes: template uses newer syntax (e.g. required-field `!`) that CI's newer CLI accepts but the controller's older vendored runtime rejects → renders fine locally, Application unhealthy in cluster with a parse error in status. Or evaluator behavior differences in edge cases (default elimination, closedness of embedded values) between versions. Operational stance: know your KubeVela release's vendored CUE version (it's in KubeVela's `go.mod`), pin CI's CLI at or below it feature-wise, and add "check vendored CUE delta" to the KubeVela upgrade runbook checklist alongside CRD changes.

## 8. List semantics

Lists unify element-wise by position — there is no key-based list merge in core CUE (KubeVela's trait `patch` with `patchKey` bolts that on). Two files contributing different-length lists to the same path conflict. Idiom: model collections as structs keyed by name (`containers: [Name=string]: {...}`) and comprehend into list form only at the output edge, as the ComponentDefinition example did with `env`. If you find yourself wanting to "append to a list from another file," restructure to a keyed struct; that instinct is the Kustomize patch reflex misfiring.

\newpage

# The Flux → KubeVela Handoff: Where CUE Failures Surface

## The pipeline, end to end

1. **Author time (MR, GitLab CI):** engineer edits `.cue` (definitions repo) or `Application` YAML (app repo). CI runs `cue fmt`/`vet`/`export` and `vela dry-run` where wired. Failures here are red MRs. Cheapest possible failure location; the platform's job is to maximize the fraction of failures caught here.
2. **Merge → Flux:** Flux's source-controller pulls the commit; kustomize-controller applies the manifests — which, for this layer, means applying `Application` CRs and (for platform repos) `ComponentDefinition`/`TraitDefinition` CRs. **Flux does not evaluate CUE. Ever.** To Flux, an Application CR is opaque YAML like any other. If it is syntactically valid YAML and the CRD admits it structurally, Flux's Kustomization goes Ready and Flux considers its job done.
3. **Apply time (KubeVela controller):** the Application controller picks up the CR, unifies `properties` against each definition's `parameter`, renders `output`/`outputs`, runs the workflow, applies resources. **This is where in-cluster CUE evaluation happens**, and therefore where CUE errors surface at runtime.

## Failure taxonomy — what shows up where

- **CUE evaluation failure (conflict, field-not-allowed, incomplete):** Flux Kustomization: `Ready=True`. Application: stuck, `status.status: rendering` failed or workflow suspended; the actual CUE error string is in `status.conditions` / `status.services` and in `vela status <app> -n <ns>` output, and in the vela-core controller logs. **Green Flux + unhealthy Application = look at KubeVela, the error is a CUE error.** This split-brain is the single most important diagnostic fact in this chapter; engineers who don't know it burn an hour reading flux logs that say everything is fine.
- **Bad definition applied:** a broken ComponentDefinition can apply cleanly (it's just a CR with a string in it) and then break rendering for *every Application referencing it* on their next reconcile. Blast radius: platform-wide, delayed-fuse. This is why definition repos need the strictest CI of anything you own, why `vela def vet` belongs in that pipeline, and why definition changes deserve progressive rollout (new definition version under a new name or `defRevision`, migrate apps deliberately) rather than in-place mutation of a definition hundreds of apps reference. Analogy that will land with your team: mutating a live shared ComponentDefinition is editing a Consul KV that every service watches — the propagation is immediate, global, and not gated by any deploy.
- **Flux-level failure:** invalid YAML, CRD schema rejection, kustomize build error, RBAC/service-account denial. These *do* show as Kustomization not-Ready with the API server's message. Ordinary Flux triage applies; CUE is not involved.

## The runbook shape

For "app team says their deploy is stuck":

```
flux get kustomizations -n <tenant-ns>        # Ready? If False → Flux-layer problem, stop here
kubectl get application <app> -n <ns> -o yaml # status.status / conditions → CUE error text lives here
vela status <app> -n <ns>                     # same, human-formatted, per-component
vela dry-run -f app.yaml -n <ns>              # reproduce the render locally against live definitions
kubectl logs -n vela-system deploy/kubevela-vela-core | grep <app>   # controller-side detail
```

`vela dry-run` is the bridge tool: it performs the same evaluation the controller would, from your terminal, making an apply-time failure reproducible as an author-time one. Wire it into app-repo CI (against a definitions snapshot) and you convert the whole apply-time failure class into MR failures — which closes the loop on the design goal this entire stack exists for: **every configuration error is a red pipeline, not a paged incident.**

One open architectural question to confirm on day one, because it changes the triage tree above: whether Flux applies `Application` objects directly (the model assumed here), or an intermediate layer owns that handoff. Ask it in your first architecture conversation — it is precisely the kind of sharp, specific question that signals you did the reading.

\newpage

# Appendix: One-Page Mental Model

- `&` is intersection. Order never matters. Nothing overrides anything.
- Types are values; a schema is just a value that isn't concrete yet.
- `*x | T` grants a replaceable default; the schema author grants all freedom that exists.
- `#Definitions` are closed: default-deny field universes. `...` and `[pattern]:` open deliberate holes.
- Environments are layers of facts, not conditionals. Added files can only tighten.
- Platform owns schemas/defaults/closedness; app teams own leaf values; "override" is not a concept.
- `vet` = consistent; `vet -c`/`export` = also concrete. Different failures, different fixes.
- Author-time CUE runs in CI with your pinned CLI; apply-time CUE runs inside KubeVela's vendored runtime. They can disagree; manage the skew.
- Flux never evaluates CUE. Green Flux + stuck Application → the error is in `vela status`, and it is a CUE error.
- Two source locations in an error = unification conflict = read both files, then decide which *fact* is wrong.
