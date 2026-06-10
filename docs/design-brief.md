# upgradepilot — design brief (pre-design, to be refined in a brainstorming session)

This is NOT a finished design. It is the starting point for a design conversation in a fresh Claude Code session: constraints, proposed shape, open questions, and phase sketch. Challenge everything here before committing.

## Product shape (proposed)

Two deployables, one repo, Go:

1. **Agent (in-cluster, read-only)** — watches the apiserver (informers over all API groups via discovery), continuously maintaining an inventory: GVKs in use (with counts, namespaces, owners), Helm releases (from release secrets), container images of known add-ons, node/kubelet/control-plane versions. Pushes compact snapshots/diffs to the server. RBAC: cluster-wide read-only, no secrets content beyond Helm release metadata.
2. **Server (self-hosted, single binary + Postgres or SQLite)** — ingests inventories from N clusters, evaluates them against the knowledge base, serves: REST API, web dashboard (readiness score per cluster per target version, findings by team/namespace), CI gate webhook (assess a manifest bundle against target version), notifiers (Slack/webhook).

Single-cluster mode: agent and server can run as one deployment for small setups.

### Why agent→server and not an operator with CRDs?
A fleet product needs cross-cluster aggregation, history, and reports — that's a database-backed service, not per-cluster CRD status. CRDs may still be a *convenience surface* later (e.g. `ReadinessCheck` CR for GitOps users) — decide in design. (Also: the author has already built a CRD operator, GoPlatform; the interesting new muscles here are the informer-based collector at scale, the knowledge-base pipeline, and the evaluation engine.)

## The knowledge base (the actual moat)

Three datasets, three different maintenance strategies:

| Dataset | Source | Strategy |
|---|---|---|
| K8s API lifecycle (deprecated/removed per version) | machine-derivable from k8s.io/api types + upstream deprecation docs; pluto/kubent ship comparable data | generator script + vendored snapshot, CI-refreshed |
| Add-on EOL/maintenance status (ingress-nginx, cert-manager, old CNI versions, etc.) | hand-curated YAML registry (top ~30 add-ons), community PRs | the community-building opportunity; keep schema dead simple |
| Version-skew policy (kubelet vs apiserver, client versions) | upstream skew policy, basically static | hardcoded rules engine |

Detection of add-ons: by image reference + chart name heuristics (curated matchers in the registry).

## Evaluation engine

Pure function: `inventory + knowledge base + target version → findings + score`. Findings have severity (blocker / warning / info), category (removed-api, deprecated-api, eol-addon, skew, chart-compat), owner attribution (namespace → team mapping), and remediation hints (e.g. "migrate to networking.k8s.io/v1; see ingress2gateway"). Score = weighted, explainable, stable (same input → same number; document the formula).

Testing: golden-file tests — recorded inventories + KB fixtures → expected findings. The engine being a pure function is what makes this project's test story excellent.

## Phase sketch

- **P1 — single cluster, CLI-first:** agent library + `upgradepilot scan` (connects to kubeconfig, builds inventory, evaluates, prints findings + score; JSON output). Already beats pluto/kubent by combining live API usage + EOL registry + skew. Demo: run against a kind cluster running an old ingress-nginx.
- **P2 — continuous + server:** in-cluster agent Deployment (Helm chart), server with Postgres, history, REST API, Slack notifier, readiness trend.
- **P3 — fleet + gates:** multi-cluster, team attribution, CI gate endpoint (assess rendered manifests pre-merge), auditor report export (PDF/CSV).
- **P4 — dashboard + community KB:** web UI, public add-on registry with contribution docs, KB auto-refresh pipeline.

## Constraints / notes for the design session

- Go + Kubernetes (client-go informers, discovery, Helm SDK for release decoding). K8s for infra; kind for tests; envtest for informer tests.
- Read-only by principle — this tool must be trivially safe to install. No mutating webhooks, no writes to the cluster (P1-P3).
- Clean-room: no chkk proprietary anything; all knowledge from upstream/public sources.
- Honest naming check before going public: "preflight" is taken (Replicated troubleshoot); verify "upgradepilot" or pick alternative during design.
- Open questions for brainstorming: CRD surface yes/no; audit-log-based detection of *requests* using deprecated APIs (vs object state) — apiserver audit webhook is powerful but operationally heavy, maybe P3+; how to attribute namespace→team (annotations? config?); scoring formula.
