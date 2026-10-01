.PHONY: build test it lint
build:
	go build -o bin/upgradescope ./cmd/upgradescope

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
# it writes to a cluster (the agent IT installs a CRD), so the tests refuse
# any context that is not kind-*; set UPGRADESCOPE_IT_CONTEXT=<context> to
# use a different disposable cluster.
it:
	UPGRADESCOPE_IT=1 go test ./... -run Integration -v
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

# Offline tests for the gate itself: a stub scanner drives hack/vulncheck.sh
# through its pass, fail and fail-closed paths.
vuln-test:
	./hack/vulncheck_test.sh

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
# release archive (and per-arch image) into dist/ without publishing, checks
# the archive names against action/action.yml, and that the binary serves the
# dashboard. No Docker engine? `make release-check GORELEASER_SKIP=publish,sign,sbom,docker`.
#
# `go run` builds GoReleaser with the repository's toolchain, so the pin must
# not need a newer Go than go.mod: v2.17.1 needs Go 1.26.5; v2.18.0 and later
# need Go 1.27. `make check-toolchain` (run in CI) checks this.
GORELEASER_VERSION ?= v2.17.1
.PHONY: release-check
release-check:
	GORELEASER_VERSION=$(GORELEASER_VERSION) ./hack/release-check.sh

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
# scan + agent ITs, image build + kind load, chart install, ClusterReadiness
# verdict, server ingest, agent.targets upgrade, clean uninstall. Needs
# Docker, helm, go, jq, curl; installs pinned kind and kubectl itself.
# `make e2e E2E_MINOR=1.31`; `make demo-down` deletes the cluster.
E2E_MINOR ?= 1.37
.PHONY: e2e agent-e2e
e2e:
	E2E_MINOR=$(E2E_MINOR) ./hack/e2e.sh
agent-e2e: e2e

# Offline self-tests of the hack/ scripts CI is built from (stubs and
# fixtures only: no network, cluster or Docker).
.PHONY: hack-test
hack-test:
	./hack/vulncheck_test.sh
	./hack/check-toolchain_test.sh
	./hack/install-tool_test.sh
	./hack/kind-images_test.sh
	./hack/e2e_test.sh

.PHONY: demo-up demo-down
demo-up:
	./hack/demo/kind-setup.sh
demo-down:
	kind delete cluster --name upgradescope-demo
