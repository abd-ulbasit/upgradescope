# upgradescope

**Continuous Kubernetes upgrade-readiness for everyone — the standalone, Apache-2.0 alternative to commercial "operational safety" platforms.**

> Status: **P1 + P2 shipped** — `upgradescope scan` (point-in-time CLI) and continuous mode: an in-cluster agent that maintains a `ClusterReadiness` CRD and pushes to a self-hosted server (history, REST API, what-if, Slack alerts). Fleet features and dashboard are in development (P3–P4).

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

## How it compares

| | pluto | kubent | upgradescope |
|---|---|---|---|
| Deprecated/removed API detection | manifests in repos | live cluster, one-shot | live cluster + manifests |
| Detects *clients still calling* deprecated APIs | — | — | ✅ (apiserver metrics) |
| Add-on EOL detection (e.g. ingress-nginx) | — | — | ✅ (curated, cited registry) |
| Version-skew checks | — | — | ✅ |
| Helm chart ↔ K8s compatibility | — | — | ✅ |
| Readiness score + CI gate | — | — | ✅ (SARIF, exit codes) |
| Continuous, in-cluster | — | — | P2 (in development) |

Every EOL claim in the knowledge base carries an upstream citation URL — auditable, not a black box. The API lifecycle dataset is generated from upstream `k8s.io/api` source, not hand-copied.

## License

Apache-2.0.
