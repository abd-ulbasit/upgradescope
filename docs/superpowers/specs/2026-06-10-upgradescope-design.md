# upgradescope — design spec

Date: 2026-06-10
Status: approved (brainstorming session, all open questions from `docs/design-brief.md` resolved)
Supersedes: `docs/design-brief.md`

## 1. What this is

**upgradescope** — a standalone, Apache-2.0, continuously-running, fleet-aware Kubernetes
upgrade-readiness scanner with a curated add-on EOL/compatibility knowledge base.

Positioning (survives competitive scrutiny as of 2026-06): *"pluto + kubent + an EOL registry,
running continuously in your cluster, with a readiness score you can show your auditor —
self-hosted and free."* No OSS tool combines continuous in-cluster operation, fleet awareness,
deprecated/removed API detection, add-on EOL data, version skew, and chart compatibility.
Nearest misses: Plural (continuous but a whole AGPL/commercial CD platform), kubescape (one
hardcoded ingress-nginx EOL control). chkk.io is the commercial benchmark; this build is
**clean-room** — upstream/public sources only.

### Name

`upgradescope` (module `github.com/abd-ulbasit/upgradescope`). Chosen over the seed name
"upgradepilot" because chkk's flagship product is branded "Upgrade Copilot" — near-identical
naming would invite confusing-similarity optics. Verified available (GitHub, .io/.dev domains)
on 2026-06-10. CRD API group: `upgradescope.dev`.

## 2. Decisions log (from the design session)

| Question | Decision |
|---|---|
| V1 wedge | CLI **and** continuous agent/server (CLI first for adoption, continuous as differentiator) |
| Committed scope | All four phases, including web dashboard and community KB pipeline |
| CRD surface | Yes, from the start: one status-rich `ClusterReadiness` CRD, agent-managed |
| Deprecated-API detection | Objects (informer/endpoint inventory) + apiserver `apiserver_requested_deprecated_apis` metric; audit-log pipeline documented as future option, not built |
| EOL registry | Standalone public artifact: versioned schema, importable module, citations required, endoflife.date sync |
| Dashboard | Embedded React/TS SPA via `go:embed` (single-binary deploy preserved) |
| Team attribution | Namespace label (configurable, default `team`) + server-side mapping override |
| Architecture | Smart edge: pure evaluation engine embedded in CLI, agent, and server |
| Agent→server protocol | REST/JSON, per-cluster bearer token (curlable > clever) |
| Storage | Store interface; SQLite default (P2), Postgres backend added in fleet phase (P3) |

## 3. Architecture

One repo, one Go module, **one binary**, three subcommands:

```
upgradescope scan    # CLI: kubeconfig or --files → inventory → evaluate → table/JSON/SARIF
upgradescope agent   # in-cluster: continuous collect → evaluate → CRD status + push to server
upgradescope serve   # server: ingest + store + REST API + embedded SPA + notifiers + CI gate
```

### Core packages

| Package | Responsibility | Depends on |
|---|---|---|
| `collect` | Build `Inventory` from a live cluster (or rendered manifests) | client-go, `registry` matchers |
| `kb` | Load/version the three datasets (API lifecycle, EOL registry, skew rules) | `registry` |
| `engine` | Pure function `Evaluate(Inventory, KB, target) Report` — no I/O | `kb` types |
| `registry` | Standalone add-on EOL dataset: schema, validator, endoflife.date sync | (none — importable alone) |
| `crd` | `ClusterReadiness` types + apply/status logic | client-go |
| `server` | Ingest API, store (SQLite/Postgres), REST API, notifiers, gate, SPA | `engine`, `kb` |
| `cli` | cobra commands wiring the above | all |

Each unit is independently testable; `engine` and `registry` have zero Kubernetes dependencies.

### Data flow (continuous mode)

agent informers/tickers → `Inventory` snapshot (compact JSON, content-hashed, pushed only on
change) → local `engine.Evaluate` → write `ClusterReadiness` status **and** push snapshot to
server (REST, bearer token) → server stores snapshots → server re-evaluates for what-if
targets, history, fleet rollups → REST API, SPA, notifiers, CI gate consume.

Single-cluster mode: `scan` alone (no install), or agent + CRD with no server. The agent's
local value never depends on server availability.

## 4. Collectors (the `collect` package)

Sub-collectors, each degrading independently (RBAC denial → capability marked `unavailable`
with reason in the Inventory, surfaced in reports as "not assessed"):

1. **API usage** — discovery to enumerate served GVKs; for each KB-flagged deprecated/removed
   GV, list objects via that exact endpoint (metadata-only, paged) to count real usage with
   namespaces and owner refs. (`kubectl get`-style reads convert objects to the newest
   version — listing the deprecated endpoint directly is what detects residency.)
2. **Deprecated-API callers** — GET apiserver `/metrics` (RBAC: `nonResourceURLs:["/metrics"]`,
   verb `get`), parse `apiserver_requested_deprecated_apis` (labels: group, version, resource,
   subresource, removed_release; STABLE metric since 1.19) via `prometheus/common/expfmt`.
   Catches active callers — pluto/kubent's blind spot. Known limits documented: gauge resets
   on apiserver restart; HA apiservers report independently; some managed planes block scraping.
3. **Helm releases** — read Secrets of type `helm.sh/release.v1`, decode base64→gunzip→JSON
   into a minimal struct (name, namespace, chart, version, status). No Helm SDK dependency.
4. **Add-ons** — match pod container images + Helm chart names against `registry` matchers.
   Unmatched images are counted in an "unrecognized" stat (registry gap visibility, no finding).
5. **Versions** — node kubelet versions, control-plane version (`/version`), client versions
   where observable → feeds skew evaluation.

## 5. Knowledge base (`kb` + `registry`)

Three datasets, three maintenance strategies:

1. **API lifecycle** (machine-generated): `tools/gen-kb` imports a pinned `k8s.io/api`, walks
   all registered types, calls the generated `APILifecycleIntroduced/Deprecated/Removed/
   Replacement()` methods, emits versioned JSON, embedded via `go:embed`. CI re-runs on new
   `k8s.io/api` tags and opens a PR. This derives **from upstream source** (cleaner clean-room
   story than copying pluto's YAML; also ahead of the human-written migration guide — upstream
   code already encodes removals through 1.38+ while the guide stops at 1.32). pluto/kubent
   data used only as cross-check in tests, never copied.
2. **Add-on EOL registry** (the standalone artifact): YAML entries under `/registry`, one file
   per add-on. Schema (versioned, validated in CI): identity, image/chart matchers, support
   status, EOL dates, K8s compatibility ranges, **required upstream citation URL per claim**.
   ~15 add-ons hand-curated (ingress-nginx first — already past its March 2026 EOL, the demo
   centerpiece), ~15 synced from endoflife.date's API (istio, cilium, calico, cert-manager,
   argo-cd, flux, …). Contribution docs + validator make it PR-able; entries we curate that
   endoflife.date lacks get upstreamed there too.
3. **Skew rules** (hardcoded table from the upstream version-skew policy): kubelet ≤ apiserver,
   up to 3 minors older; kubectl ±1; HA apiservers within 1; controller-manager/scheduler not
   newer, ≤1 older; kube-proxy ≤3 minors older.

KB staleness is itself a finding: embedded KB older than the cluster's K8s version → warning.

## 6. Evaluation engine

Pure function: `Evaluate(inv Inventory, kb KB, target Version) Report`. Deterministic,
no I/O, no clock reads (a `Now` is passed in for EOL-window math).

**Findings**: `category` ∈ {removed-api, deprecated-api, deprecated-api-in-use, eol-addon,
eol-approaching, version-skew, chart-incompat}; `severity` ∈ {blocker, warning, info};
evidence (GVK+count+namespaces, or add-on+version+EOL date+citation); owner (team via
namespace label, server override wins); remediation hint (replacement GVK, migration link —
e.g. ingress-nginx → Gateway API + ingress2gateway).

Severity is relative to target: removed at target = blocker; removed at target+1 = warning;
deprecated only = info. Add-on past EOL = blocker; EOL within 90 days = warning.

**Score** (documented in README, stable: same input → same number):

```
score  = max(0, 100 − Σ penalties)
         blocker: 25 each (category-capped at 75 total)
         warning:  5 each (capped at 20 total)
         info:     0 (listed, never scored)
ready  = (blockers == 0)        # the boolean CI gates on
```

Per-team score: same formula over the team's findings subset.

## 7. CRD surface

One cluster-scoped CRD: `ClusterReadiness` (`upgradescope.dev/v1alpha1`), agent-managed.
- **spec**: target version(s) to evaluate against.
- **status**: per-target score + ready + finding counts (by severity and category), KB
  version, lastEvaluated, top 20 findings by severity (full list lives in server/CLI; CRD
  status is size-bounded).

This is the GitOps/policy surface: Argo health checks, kyverno policies,
`kubectl get clusterreadiness`. Uninstall is clean: one CRD, one Deployment, read-only
ClusterRole + write on this single resource.

## 8. Server

- **Ingest**: `POST /api/v1/snapshots` (bearer token per cluster; cluster registers on first
  push). Content-hash dedup.
- **Store**: interface; SQLite (P2 default, zero-dep single binary), Postgres (P3, fleets).
  Tables: clusters, snapshots, findings, scores(history), teams(mapping), tokens.
- **API**: REST/JSON — clusters, snapshots, findings (filter by team/category/severity),
  scores + trends, what-if (`?target=1.38` re-evaluates stored inventory with server's KB).
- **CI gate**: `POST /api/v1/gate` — rendered manifest bundle + cluster context + target →
  findings + ready/not; also `scan --files` offline mode; SARIF output; GitHub Action wrapper.
- **Notifiers**: Slack + generic webhook, fired on finding *delta* (new blocker, add-on
  entering 90-day EOL window) — never on every snapshot.
- **Reports**: auditor export per cluster/team/timerange — CSV + self-contained HTML
  (print-to-PDF) with citations and score trend.
- **Dashboard**: React/TS SPA, `go:embed`-ed. Views: fleet matrix (clusters × target
  versions), cluster drill-down (findings by team/category, trend), registry browser.
  Auth: static token in V1 (self-hosted); SSO post-launch.

## 9. Error handling principles

- Collectors degrade independently; "not assessed (reason)" beats silent omission or crash.
- Agent buffers latest snapshot, retries with backoff; CRD status keeps updating regardless.
- Unknown images → "unrecognized" stat, not findings.
- KB staleness → explicit warning finding.
- Server rejects malformed snapshots with structured errors; never partial-writes a snapshot.

## 10. Testing strategy

- **engine**: golden-file tests — inventory fixtures + KB fixtures → expected Report JSON.
  The showcase suite; every finding category and the score formula covered.
- **collect**: envtest for discovery/informer behavior; fixtures for Helm secret decode and
  metrics parsing.
- **registry**: schema validation tests; citation-URL presence enforced.
- **gen-kb**: output cross-checked against pluto/kubent datasets in tests (sanity only).
- **e2e** (env-gated `UPGRADESCOPE_IT=1`, kind via Colima): cluster + old ingress-nginx +
  deprecated-API object → scan finds both, score drops; recorded for README demo.
- TDD throughout; CI on every push (lint, test, gen-kb freshness).

## 11. Phases (all committed)

- **P1 — `scan`**: collect + kb + engine + registry (initial entries) + CLI (table/JSON/SARIF)
  + `--files` mode. Day-one demo beats pluto+kubent combined.
- **P2 — continuous**: `agent` subcommand, `ClusterReadiness` CRD, Helm chart, `serve` with
  SQLite, ingest + REST API, history, Slack notifier.
- **P3 — fleet**: multi-cluster rollups, team attribution + server mapping, Postgres backend,
  CI gate endpoint + GitHub Action, auditor CSV/HTML export.
- **P4 — product**: embedded SPA dashboard, registry contribution pipeline (docs + validator
  CI + endoflife.date sync), KB auto-refresh CI, launch polish (README with measured numbers,
  demo GIF).

## 12. Out of scope

- Audit-log ingestion (documented as a future option; operationally heavy on managed planes).
- Mutating anything in user clusters beyond the `ClusterReadiness` CR. Read-only by principle.
- Upgrade *execution* (Cluster API's job); we detect, score, and recommend.
- SSO/multi-tenant auth (post-launch).
- Renaming note: local directory may still be `upgradepilot`; repo publishes as
  `upgradescope` with module `github.com/abd-ulbasit/upgradescope`.
