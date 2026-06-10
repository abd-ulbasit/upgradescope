# upgradescope — project context for Claude Code

## What this is
**upgradescope** — standalone, Apache-2.0, continuously-running, fleet-aware Kubernetes upgrade-readiness scanner with a curated add-on EOL/compat knowledge base. Detects deprecated/removed API usage (objects + active callers via apiserver metrics), EOL add-ons (e.g. ingress-nginx, past EOL since March 2026), version skew, and chart compatibility — with a readiness score, `ClusterReadiness` CRD, CI gate, and auditor reports. Clean-room: upstream/public sources only.

(Seeded as "upgradepilot"; renamed during design — chkk.io's flagship is branded "Upgrade Copilot". Local dir may still say upgradepilot.)

## Current state
**All four phases shipped 2026-06-11 (v0.1)**: scan CLI; agent + CRD + serve (SQLite/Postgres, embedded SPA dashboard); fleet rollups, team scores, CI gate + Action, auditor export, tokens; registry pipeline (endoflife.date sync, CONTRIBUTING, weekly KB-refresh CI). Next: publish repo, demo GIF, launch. Read in order:
1. `docs/superpowers/specs/2026-06-10-upgradescope-design.md` — THE spec (architecture, decisions log, scoring, phases)
2. `docs/superpowers/plans/` — implementation plans
3. `docs/research.md` — market evidence; `docs/design-brief.md` — superseded seed

## Architecture (locked — see spec for detail)
- One module `github.com/abd-ulbasit/upgradescope`, one binary, subcommands: `scan` / `agent` / `serve`
- Smart edge: pure `engine.Evaluate(Inventory, KB, target) → Report` embedded in CLI, agent, server
- Packages: `collect` (degradable sub-collectors), `kb`, `engine` (pure, golden-file tested), `registry` (standalone public EOL dataset, citations required), `crd` (`ClusterReadiness`, group `upgradescope.dev`), `server` (REST/JSON, SQLite→Postgres via store interface, embedded SPA), `cli`
- All four phases committed: P1 scan CLI → P2 continuous (agent+CRD+serve) → P3 fleet (Postgres, CI gate, reports) → P4 product (SPA dashboard, community registry pipeline)

## About the developer (Basit)
- Goal: Go/Kubernetes platform-engineering portfolio for a remote platform engineer role; production-grade, interview-explainable, AND genuinely useful to end users (not resume-ware).
- Background: interned at chkk.io (this domain) — **clean-room**, no proprietary reuse.
- Existing projects: GoGate (L4/L7 proxy), GoQueue (message queue), GoPlatform (kubebuilder operator), pgbranch (Postgres branching, sibling dir).
- Style: agentic development — Claude writes code, Basit directs and learns by reading. TDD, frequent commits, honest READMEs with measured numbers. Parallel subagents encouraged for independent tasks; never at the cost of output quality.

## Conventions (match sibling project pgbranch)
- Go 1.26+, module `github.com/abd-ulbasit/upgradescope`
- Apache-2.0, `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>` commit trailer
- Specs in `docs/superpowers/specs/`, plans in `docs/superpowers/plans/`
- Integration tests env-gated (`UPGRADESCOPE_IT=1`), kind/envtest for cluster tests; Docker runs via Colima on this Mac
