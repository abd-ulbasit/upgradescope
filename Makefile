.PHONY: build test it lint
build:
	go build -o bin/upgradescope ./cmd/upgradescope

# web builds the dashboard and stages it for go:embed: `make web build`
# yields a binary serving the SPA at /. Without `make web` the binary still
# builds (webdist/ holds only .gitkeep) and / explains how to get the UI.
.PHONY: web
web:
	cd web && npm install --no-fund --no-audit && npm run build
	find internal/server/webdist -type f ! -name .gitkeep -delete
	find internal/server/webdist -type d -mindepth 1 -empty -delete
	cp -R web/dist/. internal/server/webdist/
test:
	go test ./...
it:
	UPGRADESCOPE_IT=1 go test ./... -run Integration -v
lint:
	command -v golangci-lint >/dev/null && golangci-lint run || echo "golangci-lint not installed, skipping"

.PHONY: pg-test
pg-test:
	./hack/pg-test.sh

.PHONY: gen-kb
gen-kb:
	cd tools/gen-kb && go run . -out ../../internal/kb/data/apilifecycle.json

IMAGE ?= ghcr.io/abd-ulbasit/upgradescope
TAG ?= dev
VERSION ?= dev

.PHONY: docker-build kind-load
docker-build:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(TAG) .
kind-load: docker-build
	kind load docker-image $(IMAGE):$(TAG) --name upgradescope-demo

.PHONY: chart-test
chart-test:
	./hack/test-chart.sh

.PHONY: agent-e2e
agent-e2e:
	./hack/demo/agent-e2e.sh

.PHONY: demo-up demo-down
demo-up:
	./hack/demo/kind-setup.sh
demo-down:
	kind delete cluster --name upgradescope-demo
