.PHONY: build test it lint
build:
	go build -o bin/upgradescope ./cmd/upgradescope

# go build + go vet of every package for every platform the release ships
# (linux, darwin, windows x amd64, arm64): CI's build job, so a change that
# breaks only one OS fails its PR instead of the tag's CI. Needs only Go.
.PHONY: cross-build
cross-build:
	./hack/cross-build.sh

# bin/upgradescope serves the embedded dashboard: index.html at / and every
# /assets/ file it references, 200 with a JS/CSS content type, from a serve
# on a free port (CI's build job; release-check runs it on the release
# binary). Needs Go and curl.
.PHONY: dashboard-smoke
dashboard-smoke: build
	./hack/dashboard-smoke.sh bin/upgradescope

# web rebuilds the dashboard and stages it for go:embed. The staged bundle
# in internal/server/webdist is committed (the Vite build is byte-for-byte
# reproducible from package-lock.json), so plain `go build`/`go install`
# serve the SPA at /. Run this after any web/ change and commit the result:
# CI's web job rebuilds and fails on a stale webdist.
.PHONY: web
web:
	cd web && npm ci --no-fund --no-audit && npm run build
	find internal/server/webdist -type f ! -name .gitkeep -delete
	find internal/server/webdist -mindepth 1 -type d -empty -delete
	cp -R web/dist/. internal/server/webdist/

# The CI dashboard gate: npm ci, vitest, typecheck + vite build, production
# dependency advisories, and the committed webdist equals the fresh build.
# Read-only outside web/. Needs Node (CI runs it on Node 22 and 24).
.PHONY: web-test
web-test:
	./hack/web-test.sh

# The CI unit gate: gofmt, go vet, go test -race -count=1, for the main
# module and every tools/ module. Needs only Go.
test:
	./hack/test.sh

# The heap-bound tests (the proofs of the server's and the Helm collector's
# memory bounds), without the race detector, under which they skip or
# shrink; CI's test-heap job runs this, apart from the test job. Needs
# only Go.
.PHONY: test-heap
test-heap:
	./hack/test-heap.sh

# it writes to a cluster (the agent IT installs a CRD), so the tests refuse
# any context that is not a kind-* context on a loopback API server; set
# UPGRADESCOPE_IT_CONTEXT=<context> to use a different disposable cluster.
it:
	UPGRADESCOPE_IT=1 go test ./... -run Integration -v

# CI's envtest job (#135, #69): the live collector and engine against a real
# kube-apiserver and etcd for each Kubernetes minor kind has no node image
# for, 1.24 to 1.28 (hack/envtest-versions.txt). `make envtest` runs them
# all, `make envtest ENVTEST_MINOR=1.24` one. It starts the apiserver on a
# loopback port itself: no Docker, and no kubeconfig or context is read.
# Needs Go and, on the first run, network (setup-envtest and the bundle).
.PHONY: envtest
envtest:
	./hack/envtest.sh $(ENVTEST_MINOR)

# The fleet-scale gate (#125): 500 clusters with ~35 KiB inventories on
# SQLite, 10 concurrent /fleet readers; fails when p95 is over 1s or the
# live heap peaks over 512MiB (inside the chart's 1Gi memory limit). Needs only Go.
.PHONY: bench-server
bench-server:
	UPGRADESCOPE_BENCH=1 go test ./internal/server -run TestBenchServerFleet -count=1 -v
# CI's lint job runs exactly this. golangci-lint-action lags Go releases (its
# binary must be built with a Go >= our toolchain), so vet + staticcheck are
# the gate. STATICCHECK_VERSION is pinned, so a clean `make lint` means a
# clean CI lint on the same day and every day after.
STATICCHECK_VERSION ?= v0.7.0
lint:
	go vet ./...
	go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...

# The CI supply-chain gate, verbatim: govulncheck (pinned in the script) in
# binary mode against the upgradescope binary built for linux/amd64, with the
# per-advisory, expiring allowlist in hack/vuln-allowlist.txt. CI runs this
# same script, so a local pass means a CI pass. Needs jq and network access
# (it installs the pinned govulncheck and reads vuln.go.dev).
.PHONY: vuln vuln-test
vuln:
	./hack/vulncheck.sh

# The same gate on the binary users download: the latest release's
# linux/amd64 archive, sha256- (and, with cosign, signature-) verified
# (.github/workflows/vuln-latest-release.yml runs it daily). Needs gh.
.PHONY: vuln-latest
vuln-latest:
	./hack/vuln-latest-release.sh

# Offline tests for the gate itself: a stub scanner drives hack/vulncheck.sh
# through its pass, fail and fail-closed paths.
vuln-test:
	./hack/vulncheck_test.sh

# THIRD_PARTY_NOTICES: the license text of every Go module the binary links
# (go-licenses, pinned in the script) and every npm package in the dashboard
# bundle. `make notices` rewrites it (commit the result after a dependency
# change); `make notices-check` (CI's notices job) runs the script's offline
# tests, then fails on a stale file or on a dependency whose license is
# missing or not on the allowlist (GPL, AGPL, SSPL and unknown never are).
# Needs Go, jq, network, and Node when web/node_modules is incomplete.
.PHONY: notices notices-check
notices:
	./hack/notices.sh
notices-check:
	./hack/notices_test.sh
	./hack/notices.sh --check

# The README's `go install` path, from this checkout (CI's build job): the
# binary reports the module version and commit (the tag on a tagged commit)
# and serves the dashboard with every asset. Needs Go, git, jq, curl.
.PHONY: go-install-check
go-install-check:
	./hack/go-install-check.sh

# Shell completions and man pages, rendered from the command tree into
# packaging/generated (gitignored); GoReleaser runs the same before packaging.
.PHONY: completions
completions:
	go run ./tools/gen-docs -out packaging/generated

# The docs site's generated references, committed under docs/reference:
# CLI pages (cobra), the ClusterReadiness CRD (internal/crd/manifest.yaml)
# and the REST API (api/openapi.yaml). Run after changing a command, flag,
# CRD field or the OpenAPI document; TestReferenceIsFresh (go test) fails
# on a stale copy.
.PHONY: docs-gen
docs-gen: helm-docs
	go run ./tools/gen-docs -reference docs/reference

# The chart values tables, deploy/chart/README.md and
# docs/reference/helm-values.md, rendered by helm-docs (pinned,
# checksum-verified) from the `# --` comments in values.yaml. Edit the
# comments or hack/docs/*.gotmpl, never the output. Needs network on the
# first run (it installs helm-docs into bin/tools).
.PHONY: helm-docs
helm-docs:
	./hack/helm-docs.sh

# The docs site (mkdocs.yml, docs/): `make docs` builds it into bin/site
# with --strict, as .github/workflows/docs.yml does, so a broken link or
# anchor, or a page missing from the nav, fails here first; `make
# docs-serve` previews it on http://127.0.0.1:8000. Both install the
# pinned, hash-locked toolchain (hack/docs/requirements.txt) into a
# virtualenv under bin/ on first use. Needs Python 3.10 or newer.
# `make docs-check` is the drift check for every generated reference
# (CLI, CRD, REST API, Helm values); CI's docs workflow runs it.
PYTHON ?= python3
DOCS_VENV ?= bin/docs-venv
$(DOCS_VENV)/.installed: hack/docs/requirements.txt
	$(PYTHON) -m venv $(DOCS_VENV)
	$(DOCS_VENV)/bin/pip install --quiet --require-hashes -r hack/docs/requirements.txt
	touch $@

.PHONY: docs docs-serve docs-check
docs: $(DOCS_VENV)/.installed
	$(DOCS_VENV)/bin/mkdocs build --strict --site-dir bin/site
docs-serve: $(DOCS_VENV)/.installed
	$(DOCS_VENV)/bin/mkdocs serve --strict
docs-check:
	go test ./tools/gen-docs -run TestReferenceIsFresh -count=1
	./hack/helm-docs.sh --check

# Asserts the Dockerfile's golang base image matches go.mod's `go` directive,
# that GoReleaser is pinned to one version here and in release.yml, and that
# the pin needs no newer Go than go.mod (this part reads the module proxy).
# The golang image pins GOTOOLCHAIN=local, so drift breaks every image build
# (and the kind e2e that builds one) at `go mod download`.
.PHONY: check-toolchain
check-toolchain:
	./hack/check-toolchain_test.sh
	./hack/check-toolchain.sh

# Validates .goreleaser.yml with the GoReleaser release.yml pins, builds every
# release archive, deb/rpm/apk package (and per-arch image) into dist/
# without publishing, checks the archive names against action/run.sh, that
# archives, packages and images carry the licenses, completions and man
# pages, that the binary and archive sizes the README and Install page
# state are within 2% of the build, and that the binary is stamped and
# serves the dashboard. No Docker engine? `make release-check GORELEASER_SKIP=publish,sign,sbom,docker`.
#
# `go run` builds GoReleaser with the repository's toolchain, so the pin must
# not need a newer Go than go.mod: v2.17.1 needs Go 1.26.5; v2.18.0 and later
# need Go 1.27. `make check-toolchain` (run in CI) checks this.
GORELEASER_VERSION ?= v2.17.1
.PHONY: release-check
release-check:
	GORELEASER_VERSION=$(GORELEASER_VERSION) ./hack/release-check.sh

# Builds the last release tag and this tree and fails when a removed flag,
# changed default or changed usage line is not named under CHANGELOG.md's
# Changed (needs the tags; CI's release-check job, #167).
.PHONY: flags-diff
flags-diff:
	./hack/flags-diff.sh

# Builds the snapshot of HEAD twice from fresh clones and fails unless the
# checksums (archives, packages) match; with GORELEASER_SKIP=publish,sign,sbom
# (Docker needed) the per-platform image IDs must match too.
.PHONY: release-repro
release-repro:
	GORELEASER_VERSION=$(GORELEASER_VERSION) ./hack/test-release-repro.sh

# The Action's offline self-test (CI's action job): both action.yml files
# keep inputs out of run: scripts and stay the same action; action/run.sh
# validates inputs, installs only a sha256-verified archive (stub curl and
# go), and sets the outputs, annotations and step summary on action/testdata
# with a binary built from this tree. Needs Go and jq.
.PHONY: action-test
action-test:
	./hack/action_test.sh

.PHONY: pg-test
pg-test:
	./hack/pg-test.sh

.PHONY: gen-kb
# gen-kb regenerates the import list, tidies go.mod and rewrites the API
# lifecycle dataset (see the go:generate lines in tools/gen-kb/main.go).
gen-kb:
	cd tools/gen-kb && go generate ./...

.PHONY: eol-sync eol-check
eol-sync:
	cd tools/eol-sync && go run . -dir ../../registry/data
eol-check:
	cd tools/eol-sync && go run . -dir ../../registry/data -check

IMAGE ?= ghcr.io/abd-ulbasit/upgradescope
TAG ?= dev
VERSION ?= dev

# Every Dockerfile for linux/amd64 and linux/arm64, nothing pushed (CI's
# images job). Needs Docker with a multi-platform buildx builder.
.PHONY: images
images:
	./hack/images.sh

.PHONY: docker-build kind-load
docker-build:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(TAG) .
kind-load: docker-build
	kind load docker-image $(IMAGE):$(TAG) --name upgradescope-demo

.PHONY: chart-test
chart-test:
	./hack/test-chart.sh

# The CI chart gate: lint --strict, the values render matrix, kubeconform
# (pinned, checksum-verified) on every render at the oldest and newest tested
# Kubernetes minor, then chart-test. Needs helm and network (schemas).
.PHONY: helm-test
helm-test:
	./hack/install-tool_test.sh
	./hack/helm-test.sh

# The kube CI job on one Kubernetes minor (hack/kind-node-images.txt): kind
# cluster on the pinned node image, the #3 zero-false-blocker regression,
# scan's behaviour (unreachable server exits 1, an object written through a
# deprecated API is reported with its manager and not after a GA re-apply,
# a target past the KB horizon is unknown (exit 2; 0 with --allow-incomplete),
# the EOL ingress-nginx blocks, a --keep-history uninstalled release does
# not), scan + agent ITs, image build + kind load, chart install, ClusterReadiness
# verdict, server ingest, agent.targets upgrade, clean uninstall, and the
# API-server audit log checks (no deprecated-API requests, the agent writes
# only its ClusterReadiness and CRD, Secrets only via Helm's selector). Needs
# Docker, helm, go, jq, curl; installs pinned kind and kubectl itself.
# `make e2e E2E_MINOR=1.31`; `make demo-down` deletes the cluster.
E2E_MINOR ?= 1.37
.PHONY: e2e agent-e2e
e2e:
	E2E_MINOR=$(E2E_MINOR) ./hack/e2e.sh
agent-e2e: e2e

# Every test, e2e gate, CI job, make target and file docs/claims.md names
# exists, so a public claim cannot lose its proof silently (CI's test job).
.PHONY: claims-check
claims-check:
	./hack/claims-check.sh

# Offline self-tests of the hack/ scripts CI is built from (stubs and
# fixtures only: no network, cluster or Docker).
.PHONY: hack-test
hack-test:
	./hack/claims-check_test.sh
	./hack/test-heap_test.sh
	./hack/cross-build_test.sh
	./hack/dashboard-smoke_test.sh
	./hack/vulncheck_test.sh
	./hack/check-toolchain_test.sh
	./hack/install-tool_test.sh
	./hack/kind-images_test.sh
	./hack/envtest_test.sh
	./hack/e2e_test.sh
	./hack/ci-concurrency_test.sh
	./hack/ci-ok_test.sh
	./hack/kb-refresh-ci_test.sh
	./hack/notices_test.sh
	./hack/check-changelog_test.sh
	./hack/flags-diff_test.sh
	./hack/check-doc-sizes_test.sh
	./hack/chart-release-annotations_test.sh
	./hack/vuln-latest-release_test.sh
	./hack/vuln-latest-release-workflow_test.sh
	./hack/release-artifacthub-workflow_test.sh
	./packaging/homebrew-tap/script/render-formula_test.sh

.PHONY: demo-up demo-down
demo-up:
	./hack/demo/kind-setup.sh
demo-down:
	kind delete cluster --name upgradescope-demo
