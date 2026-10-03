# Architecture

This guide is for contributors. It explains how upgradescope is put together,
which contracts hold it together, and which design boundaries are
deliberate. To *use* the tool, start with [the docs home](index.md). To
*change* it, read this first, then [CONTRIBUTING.md](https://github.com/abd-ulbasit/upgradescope/blob/main/CONTRIBUTING.md).

Contents:

1. [Overview](#overview)
2. [Components](#components)
3. [Data flow](#data-flow)
4. [Collectors](#collectors)
5. [The inventory contract](#the-inventory-contract)
6. [Evaluation rules](#evaluation-rules)
7. [The report contract](#the-report-contract)
8. [Scoring](#scoring)
9. [Failure handling](#failure-handling)
10. [Knowledge base and registry pipeline](#knowledge-base-and-registry-pipeline)
11. [The ClusterReadiness CRD](#the-clusterreadiness-crd)
12. [Server](#server)
13. [Design boundaries](#design-boundaries)
14. [Testing strategy](#testing-strategy)

## Overview

upgradescope answers one question: *what breaks if this cluster moves to
Kubernetes version X?* It answers it the same way everywhere:

```
inventory (what is running)  +  knowledge base (what upstream says)  +  target version
                              │
                              ▼
               engine.Evaluate(inventory, kb, target, now)
                              │
                              ▼
       report: findings + score + ready + "not assessed" gaps
```

One Go module produces one binary with three modes and two admin commands:

| Command | Runs | Produces |
|---|---|---|
| `upgradescope scan` | once, from a laptop or CI | a table, JSON, SARIF or Markdown report, and an exit code (0 gate passed, 2 gate failed, 1 error; the gate fails on a finding at or above `--fail-on`, default `blocker`, or on an `unknown` verdict unless `--allow-incomplete`; `never` always passes) |
| `upgradescope agent` | continuously, in the cluster | `ClusterReadiness` status, plus snapshot pushes to a server (optional) |
| `upgradescope serve` | continuously, anywhere | stored history, fleet rollups, what-if, CI gate, exports, notifications, dashboard |
| `upgradescope tokens` | on demand, next to `serve` | creates, lists and revokes per-cluster ingest tokens |
| `upgradescope clusters` | on demand, against a server or its database | lists, deletes and renames clusters |

The core idea is a **smart edge**. The evaluation engine is a pure function
with no Kubernetes or network dependencies, embedded in all three modes. The
agent can therefore produce a complete verdict without a server, and the
server can evaluate a stored inventory against a target nobody evaluated it
for (a what-if) without going back to the cluster.

## Components

| Package | Responsibility | Depends on |
|---|---|---|
| `internal/inventory` | The `Inventory` contract (what was observed) and Kubernetes `Version` parsing | nothing |
| `internal/collect` | Builds an `Inventory` from a live cluster (client-go) or from rendered manifests | `inventory`, `kb`, `registry` |
| `registry` | The add-on EOL/compatibility dataset: schema, validator, embedded YAML loader | nothing internal (importable on its own) |
| `internal/kb` | Loads the knowledge base: API lifecycle data, the registry, and the version-skew policy | `inventory`, `registry` |
| `internal/engine` | `Evaluate` and `Score`: pure, deterministic, no I/O | `inventory`, `kb`, `registry` |
| `internal/sarif` | Renders a report as SARIF 2.1.0 | `engine` |
| `internal/crd` | `ClusterReadiness` types, the embedded CRD manifest, and status projection and writes | `engine`, client-go |
| `internal/agent` | The in-cluster loop and the snapshot push client | `collect`, `engine`, `crd`, `kb` |
| `internal/server` | Ingest, read API, what-if, gate, exports, team mapping, delta notifications, SPA serving | `engine`, `kb`, `collect` (manifests only), `sarif` |
| `internal/server/store` | The `Store` interface and its SQLite and Postgres implementations, with embedded migrations | nothing internal |
| `internal/server/notify` | Slack and generic-webhook delivery | nothing internal |
| `internal/cli` | cobra commands, flag validation, output writers, exit codes | everything above |
| `tools/gen-kb` | A separate Go module that regenerates the API lifecycle dataset from `k8s.io/api` | `k8s.io/api` |
| `tools/eol-sync` | A separate Go module that syncs registry EOL fields with the endoflife.date API | stdlib only |
| `web/` | React + TypeScript dashboard, built by Vite and embedded with `go:embed` | the REST API |
| `deploy/chart` | Helm chart: agent, optional server, CRD, RBAC | — |

`engine`, `inventory`, `kb` and `registry` have no client-go dependency.
Everything that talks to a cluster sits in `collect`, `crd` and `agent`.

## Data flow

### `scan`

```
kb.Load()  ──►  collect.Collect(live clients)  or  collect.CollectFiles(dir)
                                │
                                ▼
                 engine.Evaluate(inv, kb, --target, now)
                                │
                                ▼
          table / json / sarif writer   ──►  exit code from --fail-on
```

`--target` is required. In `--files` mode API usage and add-ons are
assessed, add-ons from the images and labels of workload pod templates and
from IngressClasses; version skew, deprecated calls and Helm releases need a
cluster and are reported as not assessed with the reason "files mode".

### `agent`

Each tick runs once at startup, then every `--interval` (default 10m, minimum
1m) with ±10% jitter:

```
collect.Collect ──► ensure the ClusterReadiness object exists, read spec.targets
      │
      ▼
targets = spec.targets, or (when empty) the next minor above the server version
      │
      ▼
engine.Evaluate once per target ──► crd.WriteStatus        (always)
      │
      ▼
sha256(canonical inventory) changed, or --force-sync-every elapsed?
      └── yes ──► push snapshot to the server (when --server-url is set)
```

The CRD status is written on every tick, even when the server is
unreachable. The agent's local value never depends on the server. Pushes
buffer at most one payload (the latest replaces any pending one) and retry
transient failures with exponential backoff (a `Retry-After` on a 429 or 503
is honoured, up to one minute). Permanent 4xx responses drop the payload. A
redirect is never followed, since the push would turn into a body-less GET:
it counts as a permanent failure, and the log names the status and `Location`;
set `--server-url` to the final URL. The canonical hash zeroes `collectedAt`, so an unchanged cluster
does not produce a new snapshot every tick.

### `serve`

```
POST /api/v1/snapshots ──► auth (shared or per-cluster token)
        │
        ▼
canonicalize + hash ──► duplicate of latest? ──► 200 {duplicate: true}
        │ no
        ▼
store snapshot ──► apply --team-map ──► engine.Evaluate for (next minor + --targets)
        │
        ▼
store one evaluation per target ──► diff against the previous evaluation ──► notifiers
        │
        ▼
read API, fleet rollups, exports and dashboard read the stored evaluations.
What-if and the gate re-run engine.Evaluate on demand and store nothing.
```

The snapshot is durable before evaluation starts. A failed evaluation for
one target is logged and skipped, and never fails the ingest.

## Collectors

`collect.Collect` runs five sub-collectors in a fixed order, each under its
own deadline: an equal share of the time left before the caller's deadline
(the last step gets all that remains), so a stalled step cannot starve the
ones after it. client-go's `rest.Config.Timeout` (`--request-timeout`, default
30s) bounds each request. Each sub-collector owns one **capability**:

| Capability | What it reads | Notes |
|---|---|---|
| `versions` | `/version`, nodes (kubelet versions), namespaces (team label), kube-system control-plane pods (image tags) | The cluster ID is the `kube-system` namespace UID. Managed control planes expose no control-plane pods, so that list is empty there. |
| `helm` | Secrets of type `helm.sh/release.v1` | Decodes base64, gunzip and JSON into a minimal struct, keeping the latest revision per release. No Helm SDK. |
| `deprecated-calls` | apiserver `/metrics`, `apiserver_requested_deprecated_apis` | The runtime-caller signal: which deprecated APIs some client requested since the apiserver started, which manifest scanners cannot see. It does not say which client (audit logs do). The gauge resets when the apiserver restarts, HA apiservers report independently, and managed planes often deny access. |
| `addons` | pod container and init-container images and labels, `networking.k8s.io/v1` IngressClasses, plus the Helm releases from the `helm` step | Matches registry matchers. An image matcher of two or more segments is a repository-path suffix on whole segments of the normalised reference (a one-segment matcher is that repository exactly), so mirrors and pull-through caches match; provider builds (GKE, AKS) match only entries written for them. A chart matcher names a Helm release's chart or a pod's `helm.sh/chart` label. A pod running an image no matcher claims, whose `app.kubernetes.io/name`, `helm.sh/chart` chart name or `app.kubernetes.io/part-of` names an add-on, is that add-on, at its `app.kubernetes.io/version` when the name label (or, without one, the chart label) named it. An IngressClass with controller `k8s.io/ingress-nginx` is ingress-nginx, without a version, unless ingress-nginx, a vendor build of it or Traefik (which can serve that class) was found otherwise. Each namespace is its own install: a Helm release's `appVersion` wins there over image tags and labels on its release line (major.minor), and otherwise the oldest version its image tags and labels give; image tags and labels on another line than every release in the namespace are a second install there, at their oldest version, so an older canary revision beside a newer release is judged (#165). Image repositories no image matcher claims go to `unrecognizedImages` and never become findings. In files mode the same matcher runs over manifest pod templates and IngressClasses. |
| `api-usage` | discovery, then one **metadata-only, paged** list per resource that still serves a version the knowledge base flags, at a non-deprecated version | Detects *authorship*, not servability. See below. |
| `crds` | `apiextensions.k8s.io/v1` CustomResourceDefinitions, then, for each CRD with a deprecated or unserved version, one **metadata-only, paged** list of its custom resources at a served version that is not deprecated | Records `spec.versions` and `status.storedVersions`, and the custom resources a field manager still writes through a deprecated or unserved version (the same authorship rules as `api-usage`). A forbidden custom-resource list (the agent is granted none) makes it partial. In files mode, CRD manifests give the versions and every custom resource in the files counts; one without its CRD in the files makes it partial. See [CRD versions](concepts/api-usage-detection.md#crd-versions). |

Every cluster-wide list is paged (`limit=500`). The collectors are
read-only.

### API cost per tick

The agent holds no watch and no cache, so it asks the API server again on
every tick, and the number of requests grows with the cluster. What stays
bounded is the work held at once: one page of a list and one decoded Helm
release, not a copy of the cluster. The requests of one tick are:

- **One paged list per resource type, ceil(N / 500) requests for N
  objects, and at least one.** The types are nodes, namespaces, pods in
  `kube-system`, pods in all namespaces, IngressClasses,
  CustomResourceDefinitions, the `owner=helm` Secrets and the `owner=helm`
  ConfigMaps (both are still attempted with `rbac.helmSecrets=false`, and
  the API server refuses them), and a metadata-only list for each resource
  the knowledge base flags that the cluster still serves at a non-deprecated
  version. A CRD with a deprecated or unserved version adds a list of its
  custom resources.
- **One GET per decoded Helm release**: the full Secret (or ConfigMap) of its
  installed revision, up to the 1 MiB Kubernetes allows. It is fetched again
  on every tick, even when nothing changed. This is the one cost that scales
  with releases rather than with pages, and the largest request count of a
  big cluster.
- **A few calls that do not grow with the cluster**: `/version`, the
  `kube-system` namespace, `/metrics`, API discovery (it depends on the
  number of API groups, not objects) and the agent's own `ClusterReadiness`
  (a few reads and one status update).

So a tick sends about the sum of the page counts, plus the Helm releases,
plus that constant. Listing, not decoding, is what the page size bounds. The
client is limited to 50 requests per second (burst 300) unless the caller
sets a limit, so a tick lasts longer as the request count grows. The agent
never sends a `watch`, and the chart's role grants none
([pinned by a test](operations/security-model-and-rbac.md#the-agents-clusterrole)).
`--interval` sets how often this repeats.
`TestCollectAPIUsageFollowsListPagination` pins the paging and
`TestCollectHelmPeakHeapIsBoundedByOneRelease` the one GET per release.

### API usage: authorship, not residency

The apiserver serves every stored object at every served version of its
resource, converting on read. Listing a deprecated endpoint therefore
returns every object of the kind, including objects the apiserver creates
itself, and says nothing about who uses that version. It would also make
the scanner a deprecated-API caller in `apiserver_requested_deprecated_apis`.

So for each resource the cluster still serves at a flagged version, the
collector lists once. Among the served versions it prefers one the
knowledge base does not flag, then the group's preferred version, then the
newest. When every version the resource's own group serves is flagged, it
lists the same objects in the replacement's group instead, if that group
serves the resource at an unflagged version (`extensions/v1beta1`
ingresses are `networking.k8s.io/v1` ingresses on 1.19 to 1.21). Only when
neither exists does it list a deprecated endpoint. That is unavoidable,
and the scanner then appears in `apiserver_requested_deprecated_apis` for
that resource. On 1.33 and later this happens on every cluster for core
`v1` Endpoints and ComponentStatus, which only the deprecated `v1` serves.
Then, per flagged group/version:

- **The kind goes away** (the knowledge base entry has no replacement, the
  knowledge base has no version of the kind that is neither deprecated nor
  removed, and no non-deprecated version is served, e.g. `policy/v1beta1`
  PodSecurityPolicy): every object counts.
- **The kind continues** under another version: an object counts only when
  some writer still writes it through the flagged group/version. The
  `metadata.managedFields` entries are grouped by field manager (and
  subresource), and the two operations are judged differently. A manager
  has at most one server-side Apply entry, which each apply replaces,
  apiVersion included, so it is the manager's current configuration: an
  Apply entry for the flagged version counts. Update entries are keyed by
  apiVersion as well as manager, so after a manager moves to `v1`, by
  Update or by switching to server-side apply under the same name, its
  old entry stays for every field it still co-owns, and nothing clears
  it. An Update entry for the flagged version counts only when it is
  newer than every entry the manager has for another version, its Apply
  entry included. Equal or missing timestamps clear. When no manager's
  entries are left to judge by, the apiVersion in the
  `kubectl.kubernetes.io/last-applied-configuration` annotation decides.
  Only kubectl client-side apply rewrites that annotation, so it may be
  stale under any other writer. The finding names the field manager, or
  `kubectl last-applied`.
  Entries for the `status` subresource are ignored, and so are three
  control-plane managers whose entries only record what was current when
  that release wrote the object: `kube-apiserver`,
  `kube-controller-manager` and `api-priority-and-fairness-config-producer-v1`.
  APF objects with `apf.kubernetes.io/autoupdate-spec: "true"` are skipped,
  because the apiserver maintains them.

The trade-off: an object with no managedFields entry for that version (for
example, created before field tracking existed, or with its managedFields
cleared by a raw client) and no last-applied annotation goes undetected, and
so do writes by the excluded control-plane managers. Each manager is judged
on its own, so when an object moves from one tool to another (from
`kubectl apply` to Helm, say), the old tool's entry keeps the finding open
until that entry is gone, for example once the new tool owns those fields.
A Helm upgrade that changes nothing but the apiVersion can leave Helm's
old-version entry in place: Helm computes a patch against the live object,
finds no change and skips the write, so no newer entry is recorded. The
finding then stays until the object is next changed through the new
version. The finding names at most `inventory.MaxObjectRefs` (100) objects,
and its "written by" list comes from those. When objects were left out, the
detail says so: "Written by (first 100 of 250 objects)".
The `deprecated-calls` metric covers live callers. When both signals point at
the same group/version/kind, the engine emits one finding that carries
both pieces of evidence.

## The inventory contract

`inventory.Inventory` (`internal/inventory/types.go`) is the boundary between
observation and judgement. The collectors write it. The engine, the server
store and the push protocol read it. It is JSON on the wire and at rest.

```jsonc
{
  "schemaVersion": 1,
  "collectorSchema": 1,
  "clusterId": "…kube-system UID, or \"files\" / \"manifests\"",
  "collectedAt": "RFC 3339",
  "serverVersion": "v1.34.2",
  "capabilities": { "api-usage": {"available": true}, "helm": {"available": false, "reason": "…"} },
  "apiUsage":        [{ "group", "version", "kind", "count", "namespaces": {"ns": n} }],
  "deprecatedCalls": [{ "group", "version", "resource", "subresource", "removedRelease" }],
  "helmReleases":    [{ "name", "namespace", "chartName", "chartVersion", "appVersion", "status" }],
  "addOns":          [{ "id", "version", "namespaces", "source": "chart|image|labels|ingressclass" }],
  "nodes":           [{ "name", "kubeletVersion" }],
  "controlPlane":    [{ "component", "version", "node" /* kube-proxy only */ }],
  "namespaces":      [{ "name", "team" }],
  "unrecognizedImages": ["…deduped, sorted, capped at 200"],
  "unrecognizedImagesOmitted": 0,
  "crds":            [{ "group", "kind", "plural", "versions": [{ "name", "served", "storage", "deprecated", "deprecationWarning" }],
                        "storedVersions", "usage": [/* as apiUsage, per deprecated or unserved version */] }]
}
```

Rules for changing it:

- It describes **observations, not verdicts**. Severity, EOL status and
  scores never go in the inventory. They are derived from it, so a stored
  snapshot can be re-judged by a newer knowledge base.
- New fields are additive and `omitempty`. A breaking change bumps
  `schemaVersion`. The push envelope that carries the inventory has its own
  `schemaVersion` (currently 1), and the server rejects envelope versions it
  does not know.
- `capabilities` must contain an entry for every capability the collector
  attempted. An absent slice with `available: true` means "looked, found
  nothing". `available: false` means "could not look". A capability that
  is not in the map at all was not collected, and for a required one it is
  the same as `available: false`: `api-usage` always, `versions` and (when
  the knowledge base has add-ons) `addons` in a cluster inventory are
  required gaps when absent, so an inventory that leaves them out, or has
  no `capabilities` map, reads `unknown` at best, never `ready`. The
  server refuses with 422 an inventory whose `source` is anything but
  `cluster` (an absent `source` is a v0.1.x agent's, also a cluster
  inventory): a files inventory is judged without versions or add-ons, and
  only the agent pushes.
- `collectorSchema` is the generation of field meanings the collector
  filled the inventory with (`inventory.CurrentCollectorSchema`, 1 since
  v0.2.0). An inventory without it comes from a v0.1.x agent or a v0.2.0
  release candidate, which the server tells apart by `agentVersion`, and
  judges a v0.1.x agent's by what its collectors then meant (see
  [Running the server](operations.md)). A server refuses with 422 a
  generation it does not know, whose meanings it would misread.

## Evaluation rules

`engine.Evaluate(inv, kb, target, now)` is the entire decision logic. It
performs no I/O and reads no clock (`now` is injected), and the same inputs
always give the same bytes out.

| Category | Severity | Rule |
|---|---|---|
| `removed-api` | blocker | An object written through a group/version removed at or before the target (for a kind that goes away, any stored object). Matching `deprecated-calls` rows are folded in as evidence. |
| `removed-api` | warning | Removed in the minor after the target. |
| `deprecated-api` | info | Deprecated, with no removal within that window (a deprecation after the target is titled as one). |
| `deprecated-api` | warning | A Helm release's stored manifest uses a deprecated API that the target still serves. |
| `deprecated-api-in-use` | blocker / warning / info | Requests seen in the apiserver metric for an API with no `removed-api` or `deprecated-api` finding. Otherwise they are evidence on that finding, unless the row is more severe than it (the apiserver reports a removal release the knowledge base does not have); then the row stays a finding of its own. Same window as above. Info when the removal release is missing. |
| `eol-addon` | blocker | The product is retired (`support.status: eol`, or a past product EOL date), the installed version's release line has ended, or the version is older than the oldest tracked line and that line has ended (keyed `eol-addon/<id>/below-<line>`). A node container runtime's ended line is a warning. |
| `eol-approaching` | warning | The EOL date falls within the next 90 days. |
| `chart-incompat` | blocker | The installed release line's, or the first matching compat row's, Kubernetes range excludes the target; a Helm release's chart `kubeVersion` excludes the target (info when it does not parse). |
| `addon-no-data` | info | A detected add-on whose version has no lifecycle data: no version, a version between tracked lines or newer than the newest, or a product without lines. |
| `unknown-api` | info | An object of a built-in API group (core, or any group the generator's scheme registers (`k8s.io/api` plus the apiextensions and apiregistration schemes), whether or not the knowledge base has entries for it) at a version or kind the knowledge base does not know: whether the target serves it was not assessed. CRD groups produce nothing. |
| `crd-version` | blocker / warning / info | A CRD's own versions, whatever the target: custom resources at a version the CRD does not serve (blocker); custom resources written through a `deprecated: true` version (warning, quoting its `deprecationWarning`; info when none is found); a `status.storedVersions` entry the CRD no longer serves (warning). |
| `version-skew` | blocker / warning / info | Kubelets that would fall more than 3 minors behind after the upgrade (blocker), or are already behind (warning). Controller-manager or scheduler newer than the apiserver (blocker), or too far behind (warning). HA apiserver spread, and kube-proxy rules, including a kube-proxy too far from the kubelet on its own node (warning). Unparseable kubelet versions (info). |
| `kb-stale` | warning | The cluster or the target is newer than the newest minor the knowledge base knows (`maxKnownK8s`). |

API and skew severities depend on the target: the same cluster can be ready
for 1.34 and blocked for 1.35. End of life does not: an EOL add-on blocks
every target, the current minor included, and so does a controller-manager
or scheduler newer than the apiserver. The per-category list is in
[Verdict and score](concepts/verdict-and-score.md#severity-by-category).

kubectl client skew is in the skew policy but is deliberately not evaluated:
client versions appear only in apiserver audit logs, which no collector
reads.

## The report contract

`engine.Report` (`internal/engine/findings.go`) is what every output
renders: the table, JSON, SARIF, CRD status, stored evaluations, exports and
the dashboard.

```jsonc
{
  "clusterId": "…",
  "target": "1.35",
  "kbVersion": "k8s.io/api v0.37.1; lifecycle 696a4b81; registry de96a5da",
  "score": 70,
  "ready": false,
  "verdict": "blocked",
  "findings": [{
    "category": "eol-addon",
    "severity": "blocker",
    "key": "eol-addon/ingress-nginx",
    "title": "…one line…",
    "detail": "…evidence…",
    "teams": ["…"], "namespaces": ["…"],
    "remediation": "…",
    "citations": ["https://…"]
  }],
  "notAssessed": [{ "capability": "deprecated-calls", "reason": "…", "required": false }]
}
```

- `findings` is sorted by severity (blocker, warning, info), then category,
  then title.
- `key` is the finding's **stable, count-free identity**, for example
  `removed-api/extensions/v1beta1/Ingress`. Titles may contain counts that
  change between snapshots ("3 objects" becomes "2 objects"). The key never
  does. Notification deltas diff on the key, so changing a key format causes
  alerts to fire again for existing findings.
- `notAssessed` lists every unavailable or partial capability with its
  reason, and marks the ones the verdict requires. A report with a required
  gap reads `unknown`, never `ready`.
- `verdict` is `blocked` on any blocker, otherwise `unknown` on any required
  gap, otherwise `ready`; `ready` is `verdict == "ready"`.

## Scoring

```
score   = max(0, 100 − min(75, 25 × blockers) − min(20, 5 × warnings))
verdict = blocked if blockers > 0, else unknown if a required gap, else ready
ready   = (verdict == ready)
```

Info findings are listed but never scored. The caps keep a cluster with many
warnings distinguishable from a cluster with one blocker. CI gates on the
verdict (and `--fail-on`); the score is for trends and comparison.

Per-team scores (`engine.TeamScores`) apply the same formula to each team's
subset of findings. Teams come from a namespace label (`--team-label`,
default `team`), optionally overridden by the server's `--team-map`. A
finding that spans N teams counts for each of them, and an unattributed
finding is grouped under `""`. Each team also has a `verdict`: `blocked`
by a blocker of its own or an unattributed one (which cannot be ruled out
as the team's), otherwise `unknown` when the report has a required
not-assessed gap, otherwise `ready`. A team's `ready` is `verdict == "ready"`,
so no team reads ready while the cluster is `unknown`; another team's
blocker leaves it ready. Its score, blockers and warnings count only its
own findings, and a team with no findings is not listed.

The formula is part of the public contract: users compare scores over time.
Changing it is a breaking change that goes in the changelog.

## Failure handling

Several design rules apply throughout:

- **Collectors degrade independently.** A sub-collector error marks its
  capability `available: false` with the error as the reason, and collection
  continues. A step that runs out of its share of the deadline fails the
  same way, its reason naming the step deadline. A partial failure (one forbidden resource among many) keeps the
  capability available and records the skipped parts in the reason.
  `Collect` never returns an error. Reports surface these as "not assessed
  (reason)".
- **Unknown is not a finding.** Images that match no registry entry are
  listed in `unrecognizedImages`, in the inventory and the report, which
  makes registry gaps visible without producing findings.
- **A stale knowledge base is reported, not hidden.** See `kb-stale` above.
- **The agent never dies on a tick error.** Errors are logged with a
  consecutive-failure count, and the next tick retries.
- **The server never partially writes a snapshot.** Malformed input is
  rejected with a structured JSON error before anything is stored.
  Evaluation failures after storage are logged and do not fail the request.
- **Notifications are best-effort.** A notifier failure is logged and never
  blocks ingest.

## Knowledge base and registry pipeline

The knowledge base (`internal/kb`) combines three datasets. Each one has its
own maintenance strategy, and all three are **compiled into the binary**. A
knowledge-base update reaches users only through a new release.

```
k8s.io/api (pinned in tools/gen-kb/go.mod)
      │  tools/gen-kb: walks every registered type and calls the generated
      │  APILifecycleIntroduced/Deprecated/Removed/Replacement methods
      ▼
internal/kb/data/apilifecycle.json   (generated, never hand-edited)
registry/data/*.yaml                 (hand-curated + endoflife.date-synced, cited)
skew policy                          (internal/kb/skew.go, from the upstream version-skew policy)
      │
      ▼
kb.Load() ──► KB{Version: "k8s.io/api <v>; lifecycle <digest>; registry <digest>", …}
```

1. **API lifecycle (generated).** `tools/gen-kb` is a separate module so the
   main module does not depend on a pinned `k8s.io/api`. It derives the
   dataset from upstream source rather than from a hand-maintained table, so
   it covers removals scheduled further ahead than the human-written
   deprecation guide. CI regenerates the file and fails on any difference.
   It also checks that the generator's import list covers every
   `k8s.io/api` group/version package.
2. **Tombstones and fixups (in the generator).** Types such as
   `policy/v1beta1 PodSecurityPolicy` were deleted from current `k8s.io/api`;
   the generator recovers them from older releases. A registered type with no
   lifecycle markers (`rbac.authorization.k8s.io/v1alpha1`) gets its removal
   from the Kubernetes release notes, cited in `tools/gen-kb/fixups.go`, and
   kinds kube-apiserver never served are left out. There is no hand-written
   dataset beside the generated file.
3. **Add-on registry.** One YAML file per add-on under `registry/data/`,
   with schema version 2. `registry.Validate` enforces the schema, semver
   ranges and **citations** (at least one upstream URL for any non-`unknown`
   status, every release line and every compat row). An entry's `cycles`
   are its release lines, keyed on the app version, each with an EOL date
   (or `true`/`false`) and an optional Kubernetes range; entries with an
   `endoflife_product` slug have their cycles generated by `tools/eol-sync`
   from `https://endoflife.date/api/<slug>.json`, and `support` stays
   hand-curated for products retired as a whole. An add-on is judged at the
   release line of the version installed, not by its newest line. CI runs
   `eol-sync -check` on pull requests that touch `registry/`. See
   [`registry/CONTRIBUTING.md`](https://github.com/abd-ulbasit/upgradescope/blob/main/registry/CONTRIBUTING.md) and
   [`registry/DATA-LICENSE.md`](https://github.com/abd-ulbasit/upgradescope/blob/main/registry/DATA-LICENSE.md).
4. **Skew policy.** A small table of minor-version distances from the
   upstream version-skew policy (`kb.DefaultSkewPolicy`).

A weekly workflow (`kb-refresh.yml`) bumps `k8s.io/api` in `tools/gen-kb`,
reruns both tools and opens a pull request. The datasets never change
without review.

`kb.Load` fails loudly on an empty or corrupt dataset. A silently empty
knowledge base would produce silently green scans.

## The ClusterReadiness CRD

`ClusterReadiness` (`upgradescope.dev/v1alpha1`, cluster-scoped, short name
`ucr`) is the per-cluster projection, for `kubectl get ucr`, GitOps health
checks and policy engines that should not need to reach the server.

`kubectl get ucr` and the Argo CD health check
([GitOps guide](guides/gitops-argo-flux.md#argo-cd)) are tested. The policy
engine use is not: there is no example policy yet
([#79](https://github.com/abd-ulbasit/upgradescope/issues/79)). A policy
engine reads the object with its own service account, so that account needs
`get` and `list` on `clusterreadinesses` in the `upgradescope.dev` group. The
chart grants those verbs to the agent only, as the `clusterreadinesses` rows
of the [agent's ClusterRole](operations/security-model-and-rbac.md#the-agents-clusterrole)
show, and ships no read role for anything else.

- **`spec.targets`** is the only user input: the minors to evaluate against.
  When it is empty, the agent uses the next minor above the observed server
  version. Invalid entries are skipped and reported in `status.notAssessed`.
  The schema allows **at most 8** targets, since each one is a row in
  status. A CR written before that limit, with more, is evaluated for its
  first 8 (in spec order, repeats dropped) and `status.notAssessed` counts
  the others.
- **`status`** holds, per target, the score, ready flag, counts by severity
  and by category, and the **top 20** findings. It also records the observed
  server version, knowledge-base version, agent version and evaluation time.
  Status size is bounded by construction: at most 8 targets, 20 findings
  each, finding titles and remediation clipped (to 512 and 1024 bytes as
  stored, counting JSON escapes: a `<` is six bytes there), and at most 32
  `notAssessed` entries of 512 bytes. That is about 275 KiB at the limits
  whatever the characters are (measured, with ASCII, accented, emoji and
  escaped text alike), under the 1 MiB it is asserted at and far below the
  apiserver's 3 MiB request limit and etcd's 1.5 MiB object limit. The
  full finding list lives in the CLI and server output.

The agent applies the CRD on startup. A failure there is non-fatal, because
the chart also installs the CRD from `crds/`. The agent recreates its object
if it is deleted, and writes status with conflict retry.

## Server

- **Ingest** (`POST /api/v1/snapshots`): bearer auth accepts the shared
  `--ingest-token`, or a per-cluster token from `upgradescope tokens create`.
  A per-cluster token may push only for its own cluster (otherwise 403).
  A per-cluster token is stored as its sha256 hash and its first 8
  characters, never the token itself. The body can be gzip or identity,
  is capped at 20 MiB both on the wire and after decompression, and must use
  `schemaVersion` 1. Deduplication uses the hash of the canonical inventory
  JSON. Before it is decoded, the body's JSON values are counted against a
  node budget, and buffered bodies share one memory budget across
  requests ([Memory and request limits](operations.md#memory-and-request-limits)).
- **Store** (`store.Store`): SQLite by default (`--db`, WAL mode,
  pure-Go driver, so no cgo) or Postgres (`--db-url`). Tables are
  `clusters`, `snapshots`, `evaluations` (report JSON plus score, counts
  and the report's `notAssessed`, per target, so summaries never read the
  report) and `tokens`. Both backends must pass one shared conformance suite
  (`store/storetest`).
- **Read API** (`GET /api/v1/...`): clusters, the latest report for a
  target (`?target=`; the stored evaluation for that target when one exists,
  otherwise a what-if that evaluates the latest stored inventory with the
  server's knowledge base and stores nothing), findings filtered by severity
  or category, score history, team scores, fleet
  matrices, the registry, and CSV or HTML exports. Exports are built from
  the *stored* evaluation, so an audit artifact reflects what was recorded.
  Reads that load a cluster's stored snapshot run one at a time, and only
  a what-if decodes the whole inventory. The cluster list, the fleet
  matrix and `/metrics` read snapshot heads and each evaluation's summary
  columns (score, verdict, counts, `notAssessed`), never an inventory or a
  stored report, and run two at a time. Every read's response is built
  in its slot and waits for its client in one budget shared with `/gate`
  ([Memory and request limits](operations.md#memory-and-request-limits)).
  The read token is optional. Without one, the read API is open.
- **CI gate** (`POST /api/v1/gate`): the request body is a YAML manifest
  stream. With `?cluster=`, the cluster's latest stored inventory supplies
  the context (server version, nodes, add-ons, CRDs, team labels), and the
  manifests' API usage, add-ons and CRDs are merged into it; the posted
  custom resources are judged again against the merged CRDs. The question
  it answers is "would these manifests block this cluster's upgrade?": the
  cluster as it is serves as the baseline, and only the findings the
  manifests introduce count toward the verdict. Without `?cluster=`, the
  manifests are judged on their own (API usage, add-ons, and custom
  resources against the CRDs in the stream) as `scan --files` judges them.
  Either way, `upgradescope.dev/ignore` annotations and the ignore rules of
  a `.upgradescope.yaml` sent in `?config=` are applied with `scan`'s code.
  The gate stores nothing; it answers JSON (leading with `schemaVersion`
  and `toolVersion`, like the server's other report responses), SARIF,
  JUnit or GitLab Code Quality. The stream's YAML nodes, aliases at what
  they expand to, are counted against a node budget before anything is
  decoded, and one request at a time is decoded and evaluated
  ([Memory and request limits](operations.md#memory-and-request-limits)).
- **Notifications**: after each evaluation, the server diffs the new report
  against the previous one for that cluster and target, using finding keys.
  It emits `new-blocker` (capped at 5, plus an "N more" summary),
  `became-ready` and `eol-approaching` events. Nothing is sent on a
  cluster's first evaluation, and nothing is sent for unchanged snapshots.
  When a cluster upgrades, its new default target has no earlier
  evaluation; the previous default target's last decided evaluation is the
  baseline, so blockers the new target adds are announced and ones the
  cluster already had are not.
- **Dashboard**: the SPA under `web/` is built into
  `internal/server/webdist/` and embedded. Static assets are served without
  auth, and the SPA sends the read token on API calls. The build tag
  `nodashboard` produces a binary with no embedded UI.

## Design boundaries

These limits are deliberate. A PR that crosses one needs a design discussion
first.

- **Read-only in user clusters.** The only writes are the `ClusterReadiness`
  CRD and its own object. upgradescope detects, scores and recommends. It
  never executes upgrades or remediations.
- **Not an operator.** The agent is a timer loop, not a controller-runtime
  reconciler. The verdict depends on the knowledge base and on the calendar
  (EOL dates), and neither one produces a Kubernetes event. A watch-driven
  reconciler would requeue on unrelated churn and still miss the day an EOL
  date passes. A fixed interval with jitter instead re-collects, re-evaluates
  and rewrites the status on every tick, so a passing EOL date shows up
  within one interval, and the cost is predictable: it follows the
  [request formula above](#api-cost-per-tick), growing with the cluster's
  size and its Helm releases.
- **No audit-log ingestion.** Active callers come from the apiserver metric.
  Audit logs would add kubectl client skew and caller identity, but they are
  operationally heavy and often unavailable on managed control planes.
- **Single-tenant auth.** The server uses static bearer tokens. SSO and
  multi-tenancy are out of scope for now.
- **Clean-room data.** Every dataset comes from upstream source or public
  pages, and every registry claim carries a citation. Other scanners'
  datasets are never copied.
- **One binary.** The dashboard, migrations, CRD manifest and knowledge base
  are all embedded. Deploying never requires fetching data at runtime.

## Testing strategy

| Layer | How | Where |
|---|---|---|
| Engine | Golden files: an `inventory.json` and a shared `kb.json` produce `expected.json`, covering every category and the score formula | `internal/engine/testdata/`, updated with `go test ./internal/engine -run Golden -update`; a new case also needs a `goldenParams` entry (target and fixed `now`) in `evaluate_golden_test.go` |
| Collectors | Fake clientsets, plus fixtures for Helm secret decoding, metrics parsing and manifests | `internal/collect/*_test.go`, `testdata/` |
| Registry | Schema, citation, semver and duplicate checks over the real dataset | `registry/*_test.go` |
| Store | One conformance suite run against SQLite (always) and Postgres (`UPGRADESCOPE_PG_TEST_DSN`) | `internal/server/store/storetest` |
| Server | `httptest` against a fake store and a real SQLite store | `internal/server/*_test.go` |
| Chart | `helm lint` and assertions on rendered templates | `hack/test-chart.sh` |
| End to end | A kind cluster with an EOL ingress-nginx: scan, agent, chart install, CRD and API asserts (`UPGRADESCOPE_IT=1`) | `internal/cli/*_integration_test.go`, `hack/demo/` |

See [CONTRIBUTING.md](https://github.com/abd-ulbasit/upgradescope/blob/main/CONTRIBUTING.md#running-tests) for the commands.
