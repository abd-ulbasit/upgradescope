.PHONY: build test it lint
build:
	go build -o bin/upgradescope ./cmd/upgradescope
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

.PHONY: eol-sync eol-check
eol-sync:
	cd tools/eol-sync && go run . -dir ../../registry/data
eol-check:
	cd tools/eol-sync && go run . -dir ../../registry/data -check

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
