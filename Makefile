.PHONY: build test it lint
build:
	go build -o bin/upgradescope ./cmd/upgradescope
test:
	go test ./...
it:
	UPGRADESCOPE_IT=1 go test ./... -run Integration -v
lint:
	command -v golangci-lint >/dev/null && golangci-lint run || echo "golangci-lint not installed, skipping"

.PHONY: gen-kb
gen-kb:
	cd tools/gen-kb && go run . -out ../../internal/kb/data/apilifecycle.json
