# Compatibility policy

What you can build on, and what may change between releases. upgradescope
follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html); until
1.0, a **minor** release (0.x.0) may make breaking changes, and every one is
listed under **Changed** in the [changelog](changelog.md) with what to do. A
patch release (0.x.y) never breaks anything below.

## The machine-readable contracts

| Contract | Version marker | What stays stable within it |
|---|---|---|
| JSON report (`scan --output json`, `--write-baseline`) | `schemaVersion` (1), [schema](reference/json-report.md) | Fields are only added: never renamed, removed, retyped or given a new meaning. Ignore unknown fields. A new finding category is an addition, not a break (see [Enumerated values](#enumerated-values)). |
| REST API | the `/api/v1` path prefix, [OpenAPI document](reference/api.md); report-shaped responses (a cluster's report, the gate's JSON answer) also lead with the JSON report's `schemaVersion` (1) and `toolVersion` | Paths, parameters and response fields keep their meaning; fields, and finding categories, may be added, so ignore unknown fields. The response schemas do not forbid unlisted fields, and a client generated from them keeps working when one is added. A breaking change goes under a new prefix (`/api/v2`) and `/api/v1` keeps serving for at least one minor release. |
| Webhook payload | `schemaVersion` (1), [schema](reference/webhook.md) | As for the JSON report. |
| Snapshot push protocol (agent to server) | envelope `schemaVersion` (1) and inventory `schemaVersion` (1) | The server refuses a version it does not know (422) rather than misreading it; a newer server keeps judging older agents' pushes, naming what it cannot judge. |
| `ClusterReadiness` CRD | `upgradescope.basit.engineer/v1alpha1` | See below. |
| SARIF | SARIF 2.1.0, `tool.driver.version` | Rule ids are finding keys, which are stable. |
| Finding keys | — | A finding's `key` (`removed-api/networking.k8s.io/v1beta1/Ingress`) is count-free and stable across runs and releases: baselines, ignore rules and notifications match on it. Changing a key format is a breaking change. |
| The score formula | — | `max(0, 100 − min(75, 25 × blockers) − min(20, 5 × warnings))`. Changing it is a breaking change. |
| Exit codes | — | 0 passed, 1 error, 2 gate failed. |

What is **not** a contract: the table and Markdown output (for humans),
finding titles and details (they may be reworded; match on `key`), log
lines other than the fixed messages documented in
[Metrics, logs and probes](observability.md#agent-logs), and the knowledge
base itself, which changes every release by design. A new knowledge base
changing a verdict is not a breaking change.

### Download the schemas

Each release (from v0.2.0) attaches the three published documents as
assets, listed in that release's `checksums.txt`, which is signed
([Verify a download](operations/install.md#verify-a-download)):

| Asset | Contract | Latest release |
|---|---|---|
| `report.schema.json` | JSON Schema of the JSON report | `https://github.com/abd-ulbasit/upgradescope/releases/latest/download/report.schema.json` |
| `webhook.schema.json` | JSON Schema of the webhook payload | `https://github.com/abd-ulbasit/upgradescope/releases/latest/download/webhook.schema.json` |
| `openapi.yaml` | OpenAPI 3.1 document of the REST API | `https://github.com/abd-ulbasit/upgradescope/releases/latest/download/openapi.yaml` |

Validate against the release you run: replace `latest/download` with
`download/vX.Y.Z`. The same files are in the repository under
[`api/`](https://github.com/abd-ulbasit/upgradescope/tree/main/api), where
`main` may be ahead of every release.

## Enumerated values

Finding **categories** grow as checks are added (`unknown-api` arrived
within `schemaVersion` 1), so a new category is an addition: it can ship in
any release, under the same `schemaVersion` and `/api/v1`. The published
schemas therefore give `category` as a string and list the known values in
its description, and a consumer should treat a category it does not know by
its `severity`. A test fails when the engine defines a category that the
schemas or the docs do not list. The `capability` of a not-assessed gap is
open in the same way (`target` was added within `schemaVersion` 1).

The other enumerated values are closed: `severity` (`blocker`, `warning`,
`info`), `verdict` (`ready`, `blocked`, `unknown`), `baselineState` (`new`,
`unchanged`), and the webhook's event `type` (`readiness.changed`) and
change `kind` (`new-blocker`, `became-ready`, `eol-approaching`). Adding a
value to one of them bumps `schemaVersion` (and, for the REST API, the path
prefix).

## CLI flags

Commands and flags keep their meaning within a minor release. Before 1.0 a
minor release may rename or remove a flag; the old name is kept working,
with a deprecation warning, for at least one minor release when that is
possible, and the change is listed under **Changed**. From 1.0, removing a
command or flag needs a major release.

## The CRD

`v1alpha1` means the schema may still change incompatibly between minor
releases. The path to `v1`:

1. **v1alpha1** (now): fields may change in a minor release, listed in the
   changelog. The agent updates the CRD schema itself on startup
   (`agent.manageCRD`), and status is rewritten every tick, so a status
   change needs no migration.
2. **v1beta1**: served alongside v1alpha1 for at least one minor release,
   with v1alpha1 deprecated. `spec` (targets, ignore rules) converts without
   loss; the conversion is a schema-level one (no conversion webhook), since
   the agent rewrites status every tick.
3. **v1**: once the beta schema has held for a release cycle. v1beta1 is
   then deprecated and removed no earlier than one minor release later.

`spec.targets` and `spec.ignore` are the only user input and will keep
their meaning across versions; `status` is the agent's to rewrite.

**The API group** is `upgradescope.basit.engineer`, on a domain the
maintainer owns, and so is the prefix of every annotation key
(`upgradescope.basit.engineer/ignore`, `…/ignore-reason`). v0.1.x and the
v0.2.0 release candidates used a group on a domain the project never
owned. The first stable release moved both, before anything could be
built on the old name: the scanner still reads the old annotation keys,
with a deprecation warning that names the new ones, until v0.3.0, and
nothing writes them. The agent leaves the old CRD installed and says how
to remove it ([Upgrade](operations/upgrade.md#the-api-group-moved)).
Changing the group again would be a breaking change, made only with a
major release.

## Deprecation

A deprecated flag, field or endpoint is named in the changelog, keeps
working for at least one minor release, and, where the tool can tell, warns
when used. Security fixes are the exception: they ship as soon as they are
ready, and the changelog says why the usual notice was not possible.

## Supported versions

Only the latest minor release gets fixes, as patch releases
([SECURITY.md](https://github.com/abd-ulbasit/upgradescope/blob/main/SECURITY.md)).
Kubernetes versions: the kind end-to-end suite runs on the oldest and newest
tested minors for every pull request that changes code (IR-24) and weekly on every minor from 1.29 to
the newest ([claims ledger](claims.md), IR-04).

### Tested Kubernetes range

Clusters from **1.24** to the newest minor are tested, in two ways:

| Minors | Tested against | What is exercised |
|---|---|---|
| 1.29 to the newest | a kind cluster ([`hack/kind-node-images.txt`](https://github.com/abd-ulbasit/upgradescope/blob/main/hack/kind-node-images.txt)): every pull request that changes code on the oldest and newest, weekly on all | the whole product: scan, agent, chart, CRD, server and the apiserver audit log |
| 1.24 to 1.28 | a real kube-apiserver and etcd started by [envtest](https://book.kubebuilder.io/reference/envtest) ([`hack/envtest-versions.txt`](https://github.com/abd-ulbasit/upgradescope/blob/main/hack/envtest-versions.txt)): 1.24 and 1.28 on every pull request that changes code, every minor (1.24 to 1.28) weekly | the live collector and engine only. A cluster holding GA objects has no removed-API finding or blocker; a second scan of an unchanged cluster is identical; an object written through a beta API that minor still serves is a blocker at its removal minor (the warning one minor before is asserted only on 1.27, where FlowSchema v1beta2 is removed in 1.29, so it runs in the weekly set and not on a pull request; on the other minors the removal is at the next minor); and any capability that comes back unavailable or partial is listed as not assessed, never as clean (on envtest that is `deprecated-calls`, partial because the scanner lists some deprecated endpoints itself) |

kind publishes no reliable node images for the older minors, which is why
they are covered by envtest instead. envtest has no kubelet, controller
manager, nodes or pods, so for 1.24 to 1.28 the agent, the chart, the
server and the checks that read nodes and pods (kubelet skew, container
runtimes, add-ons found from running images) are **not** exercised against
that Kubernetes version. Clusters older than 1.24 are untested: the knowledge
base still knows their APIs, but nothing checks that the collector works
against their apiserver.
