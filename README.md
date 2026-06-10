# upgradepilot

**Continuous Kubernetes upgrade-readiness for everyone — the open-source alternative to commercial "operational safety" platforms.**

> Status: pre-design. Start here, then read `docs/research.md` (why this should exist) and `docs/design-brief.md` (what to build). This repo is meant to be opened in a fresh Claude Code session — `CLAUDE.md` carries the working context.

## The problem

Upgrading a Kubernetes cluster safely requires answering, *continuously*, questions that today are answered by one-shot CLIs or expensive commercial tools:

- Which workloads still use APIs that are deprecated or **removed** in my target version?
- Which of my add-ons (controllers, CRDs, charts) are **end-of-life or unmaintained** — e.g. teams that missed that Ingress NGINX hit EOL in March 2026 and now fail compliance scans?
- Is my version skew (kubelets vs control plane vs clients) within policy?
- Are my Helm releases compatible with the target Kubernetes version?
- Can I prove all of the above to a compliance auditor, per team, over time?

Open-source answers (pluto, kubent) are **point-in-time CLI scans** that depend on where manifests live and require manual wiring into CI. The continuous, in-cluster, fleet-aware version of this is commercial-only (chkk.io and similar).

## What upgradepilot is

A self-hosted service + in-cluster agent that **continuously** watches what actually runs in your clusters, evaluates it against a curated knowledge base (API deprecations/removals per Kubernetes version, add-on EOL data, chart compatibility), and produces:

- a **readiness score per cluster per target version**, broken down by team/namespace,
- a live findings feed (what breaks at 1.34? what's EOL today?),
- compliance-friendly reports and a CI gate webhook ("block this PR — it adds a removed API").

## Why this is a strong portfolio project

- Validated gap (see research doc): OSS = stale one-shot CLIs; continuous = commercial.
- Timely hook: the Ingress NGINX retirement (March 2026) made "EOL software running in the data path" a compliance fire across the industry.
- Builder's edge: the author interned at chkk.io — domain familiarity without copying anything proprietary.
- Honest scope for a solo build: the curated knowledge base is the moat *and* the main ongoing cost — the design brief proposes starting with Kubernetes API lifecycle data (machine-derivable) plus a small hand-curated add-on EOL registry.

## License

Apache-2.0 (intended).
