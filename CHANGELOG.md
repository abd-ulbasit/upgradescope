# Changelog

All notable changes to upgradescope are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
Before 1.0, a minor release may change flags, API responses or the CRD schema.
Any such change is listed under **Changed**.

The embedded knowledge base (API lifecycle data and the add-on registry) ships
inside the binary and the image. Knowledge-base updates therefore reach you
only through a new release.

## [Unreleased]

## [0.1.1] - 2026-07-27

A packaging release. Scanning, scoring, the agent, the server and the
knowledge base are unchanged since 0.1.0.

### Fixed

- The GitHub Action can install from a release. 0.1.0 published archives as
  `upgradescope-v0.1.0-<os>-<arch>.tar.gz`, but the Action requested
  `upgradescope_<os>_<arch>.tar.gz`. Every run fell back to `go install` and
  built from source. Archive names no longer contain the version, so
  `/releases/latest/download/<asset>` resolves to a stable filename. The
  Action also now extracts the binary from the tarball.
- The README examples target the knowledge-base horizon (Kubernetes 1.36,
  `maxKnownK8s` from `k8s.io/api` v0.36.1) instead of 1.38. With 1.38, the
  headline command's only finding was a `kb-stale` warning about the tool
  itself.

### Changed

- CI and `make lint` run `go vet` plus staticcheck pinned to v0.7.0, instead
  of golangci-lint or an unpinned `staticcheck@latest`, so an upstream
  linter release cannot fail an unrelated pull request. `make lint` runs the
  pinned version through `go run` and ignores any `staticcheck` on `PATH`.
- The release archives are `upgradescope_{linux,darwin}_{amd64,arm64}.tar.gz`,
  published with `checksums.txt`.

### Removed

- The Go Report Card badge. goreportcard.com retired the service.
- The unused `.golangci.yml`.

## [0.1.0] - 2026-06-11

First release.

### Added

- `upgradescope scan`: a point-in-time readiness scan of a live cluster
  (kubeconfig) or of rendered manifests (`--files`). It prints a table, JSON
  or SARIF 2.1.0. The CI exit codes are: 2 when findings reach `--fail-on`,
  1 on an operational error.
- Detection of:
  - deprecated and removed API objects, probed at the deprecated
    group/version endpoints with paged, metadata-only lists;
  - clients still calling deprecated APIs, read from the apiserver
    `apiserver_requested_deprecated_apis` metric;
  - end-of-life and soon-to-be-EOL add-ons, matched by container image and
    Helm chart;
  - add-on and Helm chart compatibility with the target Kubernetes version;
  - version skew between kubelets, HA apiservers, controller-manager,
    scheduler and kube-proxy;
  - a knowledge base too old for the cluster or the target (`kb-stale`).
- A deterministic readiness score,
  `max(0, 100 − min(75, 25×blockers) − min(20, 5×warnings))`, with
  `ready = no blockers`, and per-team scores from a namespace label.
- Collectors that degrade independently. A collector that is denied or
  broken is reported as "not assessed (reason)" instead of failing the scan.
- `upgradescope agent`: an in-cluster loop that maintains a cluster-scoped
  `ClusterReadiness` CRD (`upgradescope.dev/v1alpha1`, short name `ucr`) and
  optionally pushes content-deduplicated inventory snapshots to a server.
- `upgradescope serve`: snapshot ingest, SQLite or Postgres storage, score
  history, what-if re-evaluation (`?target=`), fleet rollups, the
  `POST /api/v1/gate` CI gate, auditor CSV and HTML exports, Slack and
  generic-webhook notifications on finding deltas, and an embedded React
  dashboard (fleet matrix, cluster drill-down, registry browser).
- `upgradescope tokens create|revoke`: per-cluster ingest tokens, stored
  hashed.
- A Helm chart (`deploy/chart`) for the agent and an optional server.
- A composite GitHub Action (`action/`) that wraps the manifest gate and
  produces SARIF.
- A knowledge base with an API-lifecycle dataset generated from upstream
  `k8s.io/api` source (`tools/gen-kb`) and an 18-entry add-on registry. Every
  claim in the registry requires an upstream citation, and 10 entries are
  synced with endoflife.date (`tools/eol-sync`). A weekly `kb-refresh`
  workflow opens a pull request when either dataset changes.

### Known limitations

- kubectl client skew is not evaluated, because client versions are visible
  only in apiserver audit logs.
- Managed control planes often block apiserver `/metrics`. The
  deprecated-callers signal is then reported as not assessed.
- `kind: List` items are not expanded in `--files` mode.

[Unreleased]: https://github.com/abd-ulbasit/upgradescope/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/abd-ulbasit/upgradescope/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/abd-ulbasit/upgradescope/releases/tag/v0.1.0
