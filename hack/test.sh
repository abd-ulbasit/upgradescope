#!/usr/bin/env bash
# The Go unit gate (make test; CI's test job), for the main module and the
# separate tools/ modules:
#   - go vet;
#   - gofmt over every tracked Go file, tools/ included;
#   - go test -race -count=1. The agent loop, the server's ingest/gate caps
#     and the notifier are concurrent; -count=1 so a cached pass never stands
#     in for a run. Integration suites stay env-gated (UPGRADESCOPE_IT,
#     UPGRADESCOPE_PG_TEST_DSN), so this needs no cluster, database or Docker.
set -euo pipefail
cd "$(dirname "$0")/.."

echo "== gofmt"
unformatted="$(git ls-files -z '*.go' | xargs -0 gofmt -l)"
if [ -n "$unformatted" ]; then
  echo "::error::gofmt needed on:" >&2
  echo "$unformatted" >&2
  exit 1
fi

echo "== go vet + go test -race (main module)"
go vet ./...
go test ./... -race -count=1

for mod in tools/*/; do
  [ -f "$mod/go.mod" ] || continue
  echo "== go vet + go test -race ($mod, separate module)"
  (cd "$mod" && go vet ./... && go test ./... -race -count=1)
done
echo "test: OK"
