# upgradescope

**Continuous Kubernetes upgrade-readiness for everyone — the standalone, Apache-2.0 alternative to commercial "operational safety" platforms.**

> Status: **P1–P3 shipped** — point-in-time scan, continuous agent + `ClusterReadiness` CRD + server (SQLite/Postgres), fleet rollups, per-team scores, CI gate endpoint + GitHub Action, auditor CSV/HTML export, per-cluster tokens. **P4 in progress** — community registry pipeline + weekly KB auto-refresh shipped; web dashboard in development.

## Install

```sh
go install github.com/abd-ulbasit/upgradescope/cmd/upgradescope@latest
```

## Quickstart

```sh
# Scan the current kubeconfig context for readiness against Kubernetes 1.36
upgradescope scan --target 1.36

# Machine-readable report
upgradescope scan --target 1.36 --output json

# CI annotation: SARIF output + gate (exit 2 if any blocker, exit 1 on error)
upgradescope scan --target 1.36 --output sarif --fail-on blocker > scan.sarif

# Offline / CI: scan rendered manifests instead of a live cluster
upgradescope scan --target 1.36 --files ./rendered-manifests
```

Exit codes: `0` ready (or below `--fail-on` threshold), `1` scan error, `2` gate failed.

### The readiness score

The score is deterministic and explainable — same inventory, same knowledge base, same number:

```
score = max(0, 100 − min(75, 25 × blockers) − min(20, 5 × warnings))
ready = (blockers == 0)        # what --fail-on blocker gates on
```

Blockers are findings that break at the target version (a removed API in use, an EOL add-on);
warnings break one version later or are approaching EOL; info findings are listed but never
scored. Caps keep one noisy category from zeroing the score.

## Continuous mode (P2)

Run the agent in-cluster via the Helm chart — it keeps a `ClusterReadiness` resource up to date and (optionally) pushes to a self-hosted server:

```sh
helm install upgradescope ./deploy/chart -n upgradescope --create-namespace \
  --set server.enabled=true        # single-cluster all-in-one

kubectl get ucr                    # ClusterReadiness: TARGET / SCORE / READY
curl -s $SERVER/api/v1/clusters    # fleet API: clusters, findings, history, what-if (?target=1.38)
```

- Agent is **read-only** (plus status on its own CRD); works with no server at all.
- Server: single binary + SQLite (Postgres in P3), Slack/webhook alerts on *new* blockers only.
- Auth: bearer tokens (`--ingest-token`, optional `--read-token`).

> Integration tests are env-gated: `make demo-up && make it`; full agent e2e: `make agent-e2e` (kind + Helm + Docker/Colima required).

## Fleet mode (P3)

With several clusters pushing to one server, the read API rolls them up:

```sh
# Cluster × target score matrix (latest stored evaluations; no recompute)
curl -s "$SERVER/api/v1/fleet?targets=1.37,1.38"

# Per-team rollup across the fleet: worst score, total blockers, affected clusters
curl -s "$SERVER/api/v1/fleet/teams?target=1.38"

# Per-team scores for one cluster (also embedded as `teams` in /report)
curl -s "$SERVER/api/v1/clusters/1/teams?target=1.38"
```

Team attribution comes from namespace labels (`--team-label`, default `team`),
optionally overridden server-side with `serve --team-map teams.yaml`:

```yaml
# first matching glob wins; namespaces matching nothing keep their label
- pattern: "payments-*"
  team: payments
- pattern: "kube-*"
  team: platform
```

The CLI table output grows a `TEAMS` section whenever at least one finding is
attributed to a named team.

## CI gate

Two ways to block a PR that adds a removed API:

**1. CLI (no server needed):**

```sh
helm template ./chart > rendered.yaml   # or kustomize build
upgradescope scan --files . --target 1.38 --output sarif --fail-on blocker > results.sarif
```

**2. Server gate endpoint** — evaluates manifests *inside a known cluster's
context* (its add-ons, version skew, and namespace→team labels are merged in;
the manifests replace the cluster's API usage):

```sh
curl -sf -X POST "$SERVER/api/v1/gate?target=1.38&cluster=prod-eu-1&format=sarif" \
  -H "Authorization: Bearer $READ_TOKEN" \
  -H "Content-Type: application/x-yaml" \
  --data-binary @rendered.yaml > results.sarif
```

Omit `cluster=` to evaluate the manifests standalone; `format=json` (default)
returns the full report.

**GitHub Action** (composite, in `action/`):

```yaml
jobs:
  upgrade-gate:
    runs-on: ubuntu-latest
    permissions:
      security-events: write   # for SARIF upload
    steps:
      - uses: actions/checkout@v4
      - run: helm template ./chart --output-dir rendered
      - uses: abd-ulbasit/upgradescope/action@main
        id: gate
        with:
          path: rendered
          target: "1.38"
          fail-on: blocker
      - uses: github/codeql-action/upload-sarif@v3
        if: always()           # annotate the PR even when the gate fails
        with:
          sarif_file: ${{ steps.gate.outputs.sarif-file }}
```

## Auditor export

One self-contained artifact per cluster per target — no JS, no CDN, prints cleanly:

```sh
# findings as CSV (one row per finding, citations included)
curl -s "$SERVER/api/v1/clusters/1/export?target=1.38&format=csv" -o report.csv

# single-file HTML report: score badge, findings by severity, score-history sparkline
curl -s "$SERVER/api/v1/clusters/1/export?target=1.38&format=html" -o report.html
```

Exports always reflect the latest **stored** evaluation (what the system
actually recorded, with its `evaluatedAt`), never an on-the-fly recompute.

## Ingest tokens

- Dev / single cluster: one shared `serve --ingest-token <secret>` for all agents.
- Fleet: per-cluster tokens — `upgradescope tokens create <cluster> --db ...`
  prints a token (once) valid **only** for that cluster name (403 on mismatch);
  revoke with `upgradescope tokens revoke <cluster>`. The shared `--ingest-token`
  keeps working as a back-compat/dev path.
- Postgres backend: `serve --db-url postgres://...` (mutually exclusive with `--db`).

## The problem

Upgrading a Kubernetes cluster safely requires answering, *continuously*, questions that today are answered by one-shot CLIs or expensive commercial tools:

- Which workloads still use APIs that are deprecated or **removed** in my target version?
- Which of my add-ons (controllers, CRDs, charts) are **end-of-life or unmaintained** — e.g. teams that missed that Ingress NGINX hit EOL in March 2026 and now fail compliance scans?
- Is my version skew (kubelets vs control plane vs clients) within policy?
- Are my Helm releases compatible with the target Kubernetes version?
- Can I prove all of the above to a compliance auditor, per team, over time?

Open-source answers (pluto, kubent) are **point-in-time CLI scans** that depend on where manifests live and require manual wiring into CI. The continuous, in-cluster, fleet-aware version of this is commercial-only.

## What upgradescope is

A self-hosted service + in-cluster agent that **continuously** watches what actually runs in your clusters, evaluates it against a curated knowledge base (API deprecations/removals per Kubernetes version, add-on EOL data, chart compatibility), and produces:

- a **readiness score per cluster per target version**, broken down by team/namespace,
- a live findings feed (what breaks at 1.37? what's EOL today?),
- compliance-friendly reports and a CI gate webhook ("block this PR — it adds a removed API").

## Architecture

One binary, three subcommands, one pure evaluation core embedded everywhere:

```
                    knowledge base (versioned with the code)
                    ├── internal/kb/data/apilifecycle.json   ← gen-kb ← k8s.io/api source
                    └── registry/data/*.yaml (cited)         ← eol-sync ← endoflife.date API
                                      │
                                      ▼
   CLI / CI                engine.Evaluate(inventory, kb, target) → report
   ───────────             (pure, deterministic, golden-file tested)
   upgradescope scan ──────────┐      ▲      ┌────────────────────────────────┐
   (kubeconfig or --files,     │      │      │ in-cluster (Helm chart)        │
    table/json/sarif,          │      └──────│ upgradescope agent             │
    exit codes for gating)     │             │  · read-only collectors       ─┼─► apiserver,
                               │             │  · ClusterReadiness CRD status │   metrics, Helm
                               ▼             └───────────────┬────────────────┘
                        ┌─────────────────────────┐          │ push (bearer token,
                        │ upgradescope serve      │◄─────────┘  per-cluster)
                        │  SQLite / Postgres      │
                        │  /api/v1: fleet matrix, │
                        │  reports, history, gate,│
                        │  CSV/HTML export        │
                        └─────────────────────────┘
```

The agent degrades gracefully: each collector (objects, apiserver metrics,
Helm, nodes) fails independently and the report says what it could not see.

## Measured numbers

Measured 2026-06-11 on an Apple-silicon MacBook (Docker via Colima), against
the kind demo cluster (single node, Kubernetes v1.35, demo workloads + EOL
ingress-nginx installed):

| What                                                              | Measured                          |
|-------------------------------------------------------------------|-----------------------------------|
| `upgradescope scan --target 1.36` against the live demo cluster   | 0.22–0.30 s wall (3 runs)         |
| Release binary, darwin/arm64 (`-ldflags "-s -w"`)                  | 52 MB (75 MB unstripped)          |
| Docker image (multi-stage, distroless static, nonroot)             | 47 MB                             |
| API lifecycle dataset                                              | 160 entries from k8s.io/api v0.36.1 |
| Add-on registry                                                    | 18 add-ons, every claim cited     |

## The knowledge base stays fresh by itself

- `tools/gen-kb` regenerates the API lifecycle dataset from upstream
  `k8s.io/api` source (the same generated lifecycle methods the apiserver
  uses) — never hand-copied tables.
- `tools/eol-sync` syncs registry entries that declare an
  `endoflife_product` slug against the live endoflife.date API; CI fails on
  drift (`make eol-check`).
- A weekly GitHub Actions cron (`kb-refresh.yml`) bumps `k8s.io/api`, reruns
  both tools, and opens a reviewable PR — no silent dataset changes.

Want to add an add-on? See [`registry/CONTRIBUTING.md`](registry/CONTRIBUTING.md).

## How it compares

| | pluto | kubent | upgradescope |
|---|---|---|---|
| Deprecated/removed API detection | manifests in repos | live cluster, one-shot | live cluster + manifests |
| Detects *clients still calling* deprecated APIs | — | — | ✅ (apiserver metrics) |
| Add-on EOL detection (e.g. ingress-nginx) | — | — | ✅ (curated, cited registry) |
| Version-skew checks | — | — | ✅ |
| Helm chart ↔ K8s compatibility | — | — | ✅ |
| Readiness score + CI gate | — | — | ✅ (SARIF, exit codes) |
| Continuous, in-cluster | — | — | ✅ (agent + CRD + server) |
| Fleet rollups, team scores, auditor export | — | — | ✅ |

Every EOL claim in the knowledge base carries an upstream citation URL — auditable, not a black box. The API lifecycle dataset is generated from upstream `k8s.io/api` source, not hand-copied.

## License

Apache-2.0.
