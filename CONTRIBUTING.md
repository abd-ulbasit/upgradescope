# Contributing to upgradescope

Thanks for helping. This guide gets you from a fresh clone to a green pull
request. For how the pieces fit together, read
[`docs/architecture.md`](docs/architecture.md) first. It is short and it names
the contracts you must not break.

Most contributions are one of these three:

- **A registry entry** (a new add-on, or a corrected EOL date or compat row).
  Follow [`registry/CONTRIBUTING.md`](registry/CONTRIBUTING.md). You need Go
  and nothing else.
- **A detection or scoring change.** This lives in `internal/engine` and comes
  with a golden-file update. See [Golden files](#golden-files).
- **A collector, agent, server or dashboard change.** Read the matching section
  of the architecture guide before you start.

By participating you agree to the [Code of Conduct](CODE_OF_CONDUCT.md).
Report security issues privately, as described in [SECURITY.md](SECURITY.md),
and never in a public issue.

## Development setup

| Tool | Version | Needed for |
|---|---|---|
| Go | 1.26 (see `go.mod`) | everything |
| Node.js + npm | 20 or newer (the `Dockerfile` builds with `node:20`; 22 LTS works) | only the web dashboard (`web/`, `make web`) |
| helm | 3.x | only `make chart-test` (lint and render, no cluster) |
| kind, kubectl | recent | only integration and e2e tests |
| Docker | any | only `make pg-test`, `make docker-build` and `make agent-e2e` |

Unit tests, golden tests, lint and the build need **only Go**. No Docker, no
cluster and no network beyond the Go module proxy.

```sh
git clone https://github.com/abd-ulbasit/upgradescope
cd upgradescope
make build        # bin/upgradescope (no dashboard, see below)
make test         # go test ./...
make lint         # go vet + pinned staticcheck, the same gate CI runs
```

The binary embeds the dashboard from `internal/server/webdist/`, which in a
fresh clone holds only `.gitkeep`. `make build` therefore produces a binary
whose `serve` answers `/` with a "dashboard not built" message. To build with
the dashboard, run `make web build`, which needs Node.

## Repository map

```
cmd/upgradescope/      main: wires cli.Root() and maps errors to exit codes
internal/
  inventory/           the Inventory contract (what was observed) + Version parsing
  collect/             builds an Inventory from a live cluster or rendered manifests
  kb/                  loads the knowledge base: API lifecycle data, registry, skew policy
    data/              apilifecycle.json (generated) + supplement.json (hand-curated)
  engine/              pure Evaluate(inventory, kb, target, now) → Report, plus Score
  crd/                 ClusterReadiness types, CRD manifest, status projection
  agent/               in-cluster loop: collect → evaluate → CRD status → push
  server/              ingest + read API, what-if, gate, exports, notifiers, SPA
    store/             Store interface, SQLite and Postgres implementations, migrations
    notify/            Slack and generic webhook delivery
  cli/                 cobra commands: scan, agent, serve, tokens
  sarif/               SARIF 2.1.0 rendering shared by the CLI and the gate
registry/              the add-on EOL/compat dataset (data/*.yaml), its schema and validator
tools/
  gen-kb/              separate Go module: regenerates internal/kb/data/apilifecycle.json
  eol-sync/            separate Go module: syncs registry EOL fields with endoflife.date
deploy/chart/          Helm chart (agent, optional server, CRD, RBAC)
web/                   React + TypeScript dashboard (Vite), embedded via go:embed
action/                composite GitHub Action wrapping `upgradescope scan`
hack/                  test and demo scripts (kind setup, chart tests, Postgres tests)
```

## Running tests

### Unit tests

```sh
make test                              # go test ./...
go test -race ./internal/agent/...     # run -race on the packages you touched
(cd tools/eol-sync && go test ./...)   # tools/ are separate modules: test them separately
(cd tools/gen-kb && go vet ./...)
```

CI also runs these checks, and you should run them before pushing:

```sh
gofmt -l cmd internal registry tools   # must print nothing
make lint
```

### Golden files

`internal/engine` is tested with golden files. Each directory under
`internal/engine/testdata/<case>/` holds an `inventory.json` input and an
`expected.json` report. All cases share `internal/engine/testdata/kb.json`.

When you change detection or scoring on purpose, regenerate the goldens and
**read the diff**. A golden diff is the behaviour change your PR makes, and
reviewers will treat it that way:

```sh
go test ./internal/engine -run Golden -update
git diff internal/engine/testdata
```

To add a scenario, create a new directory with an `inventory.json`, run with
`-update`, and check that the generated `expected.json` says what you expect.
Never run `-update` just to make a failing test pass.

### Integration and end-to-end tests (opt-in)

These tests are skipped unless you set an environment variable, so
`go test ./...` never touches a cluster or a database.

> **Warning:** the cluster integration tests use your **current kubeconfig
> context**. They create the `ClusterReadiness` CRD and an object in whatever
> cluster that context points at. Run them only against the throwaway kind
> cluster. Check with `kubectl config current-context` first. It must print
> `kind-upgradescope-demo`.

| What | How | Needs |
|---|---|---|
| scan + agent against a real cluster | `make demo-up` then `make it` (`UPGRADESCOPE_IT=1`) | kind, helm, kubectl |
| agent e2e: image build, `helm install`, CRD and server asserts | `make agent-e2e` | Docker, kind, helm, kubectl |
| Postgres store conformance | `make pg-test`, or set `UPGRADESCOPE_PG_TEST_DSN=postgres://…` and run `go test ./internal/server/store/ -run TestPostgresConformance` | Docker, or any Postgres you own |
| Helm chart contract | `make chart-test` | helm only |
| dashboard unit tests | `cd web && npm ci && npm test` | Node |

`make demo-up` creates a kind cluster named `upgradescope-demo` and installs an
EOL ingress-nginx chart into it, so the scan has a real blocker to find.
`make demo-down` deletes it. CI runs the same kind job on every push and pull
request. To skip it on a docs-only change, put `[skip-e2e]` in the head commit
message.

### Knowledge-base tooling

- `make gen-kb` regenerates `internal/kb/data/apilifecycle.json` from the
  `k8s.io/api` version pinned in `tools/gen-kb/go.mod`. CI fails if the
  committed file differs from the generator output, so never hand-edit it.
  Types that upstream has already deleted from `k8s.io/api` go in
  `internal/kb/data/supplement.json` instead. Verify those entries against the
  upstream deprecation guide, and say so in the file's `generatedFrom` note.
- `make eol-sync` and `make eol-check` reconcile registry entries that declare
  `endoflife_product` with the endoflife.date API. Both need network access.
- A weekly workflow (`kb-refresh.yml`) runs both and opens a pull request. You
  rarely need to bump `k8s.io/api` yourself.

## Commit conventions

- Use [Conventional Commits](https://www.conventionalcommits.org/) subjects:
  `feat:`, `fix:`, `docs:`, `test:`, `refactor:`, `ci:`, `chore:`, with an
  optional scope such as `fix(agent):` or `feat(registry):`. Release notes
  are generated from these, and `docs`, `chore` and `test` commits are left
  out of them.
- Use the body to explain **why**. The diff already shows what changed.
- Reference the issue in the footer: `Refs #123`, or `Fixes #123` when the PR
  meets all of that issue's acceptance criteria.
- Make small, logical commits. A refactor and the behaviour change it enables
  go in separate commits.
- **There is no DCO and no CLA.** Contributions are accepted under the
  project's Apache-2.0 license (section 5 of the license), and you do not need
  to sign off commits.

## Pull requests

1. Open an issue first for anything larger than a bug fix or a registry entry,
   so the design can be agreed before you write code.
2. Keep each PR to one concern, and fill in the PR template.
3. Write tests first. A bug fix comes with a test that failed before the fix.
   A detection change comes with a golden case.

PR checklist (the template repeats it):

- [ ] `make test` and `make lint` pass, and `gofmt -l cmd internal registry tools` prints nothing
- [ ] `go test -race` passes for the packages you touched
- [ ] new behaviour has tests, and golden diffs are intentional and explained
- [ ] user-facing changes are documented (README, chart README, `--help` text)
- [ ] registry changes follow [`registry/CONTRIBUTING.md`](registry/CONTRIBUTING.md), and `make eol-check` passes
- [ ] the commit messages follow the conventions above

## Design rules that review will enforce

These come from [`docs/architecture.md`](docs/architecture.md). PRs that
break them are sent back:

- `engine.Evaluate` stays pure: no I/O, no clock reads, and deterministic output.
- A collector failure becomes "not assessed (reason)" in the report. It never
  crashes the scan and is never silently dropped.
- The agent is read-only except for the `ClusterReadiness` CRD and its own
  object. Nothing else in a user's cluster is mutated.
- Every registry claim carries an upstream citation, and the validator
  enforces it.
- Clean-room: data comes from upstream or public sources only. Do not copy
  other tools' datasets.
