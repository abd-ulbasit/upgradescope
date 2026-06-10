# upgradepilot — project context for Claude Code

## What this is
Continuous Kubernetes upgrade-readiness scanner — open-source alternative to commercial operational-safety platforms (chkk.io et al.). Detects deprecated/removed API usage, EOL add-ons (e.g. ingress-nginx post-March-2026), version skew, and chart compatibility — continuously, fleet-aware, with a readiness score and compliance-friendly reports.

## Current state
**Pre-design.** Nothing built yet. Read in order:
1. `README.md` — pitch and gap
2. `docs/research.md` — market evidence (compiled 2026-06-10, with sources)
3. `docs/design-brief.md` — proposed shape, phase sketch, **open questions to resolve in a brainstorming/design session before any code**

## How to start a session here
Begin with brainstorming/design (challenge the brief, resolve its open questions, write a real design spec), then a phased implementation plan, then TDD execution. Do not start coding from the brief directly — it is deliberately unfinished.

## About the developer (Basit)
- Goal: Go/Kubernetes platform-engineering portfolio for a remote platform engineer role; projects must be production-grade and interview-explainable.
- Relevant background: interned at chkk.io (this exact domain) — build **clean-room**, public/upstream sources only, no proprietary reuse.
- Existing projects: GoGate (L4/L7 proxy), GoQueue (message queue), GoPlatform (kubebuilder operator — he's done CRD operators; prefer the agent→server architecture in the brief unless design review finds strong reasons for CRDs), pgbranch (Postgres branching engine, sibling dir — being built in another session).
- Style: agentic development — Claude writes code, Basit directs and learns by reading. TDD, frequent commits, honest READMEs with measured numbers.

## Conventions (match sibling project pgbranch)
- Go 1.26+, module `github.com/abd-ulbasit/upgradepilot`
- Apache-2.0, `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>` commit trailer
- Specs in `docs/superpowers/specs/`, plans in `docs/superpowers/plans/`
- Integration tests env-gated (`UPGRADEPILOT_IT=1`), kind/envtest for cluster tests; Docker runs via Colima on this Mac
