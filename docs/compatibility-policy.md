# Compatibility policy

What you can build on, and what may change between releases. upgradescope
follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html); until
1.0, a **minor** release (0.x.0) may make breaking changes, and every one is
listed under **Changed** in the [changelog](changelog.md) with what to do. A
patch release (0.x.y) never breaks anything below.

## The machine-readable contracts

| Contract | Version marker | What stays stable within it |
|---|---|---|
| JSON report (`scan --output json`, `--write-baseline`) | `schemaVersion` (1), [schema](reference/json-report.md) | Fields are only added: never renamed, removed, retyped or given a new meaning. Ignore unknown fields. |
| REST API | the `/api/v1` path prefix, [OpenAPI document](reference/api.md) | Paths, parameters and response fields keep their meaning; fields may be added. A breaking change goes under a new prefix (`/api/v2`) and `/api/v1` keeps serving for at least one minor release. |
| Webhook payload | `schemaVersion` (1), [schema](reference/webhook.md) | As for the JSON report. |
| Snapshot push protocol (agent to server) | envelope `schemaVersion` (1) and inventory `schemaVersion` (1) | The server refuses a version it does not know (422) rather than misreading it; a newer server keeps judging older agents' pushes, naming what it cannot judge. |
| `ClusterReadiness` CRD | `upgradescope.dev/v1alpha1` | See below. |
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

## Deprecation

A deprecated flag, field or endpoint is named in the changelog, keeps
working for at least one minor release, and, where the tool can tell, warns
when used. Security fixes are the exception: they ship as soon as they are
ready, and the changelog says why the usual notice was not possible.

## Supported versions

Only the latest minor release gets fixes, as patch releases
([SECURITY.md](https://github.com/abd-ulbasit/upgradescope/blob/main/SECURITY.md)).
Kubernetes versions: the kind end-to-end suite runs every pull request on
the oldest and newest tested minors and weekly on every minor from 1.29 to
the newest ([claims ledger](claims.md), IR-04).
