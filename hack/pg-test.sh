#!/usr/bin/env bash
# pg-test.sh — run the Postgres store conformance suite, and the server
# tests that run on both stores, against a real Postgres (make pg-test; CI's
# pg-conformance job).
#
#   ./hack/pg-test.sh                       # throwaway postgres:17-alpine container
#   PG_VERSION=14 ./hack/pg-test.sh         # another major (postgres:14-alpine)
#   PG_TEST_PORT=55433 ./hack/pg-test.sh
#   UPGRADESCOPE_PG_TEST_DSN=postgres://... ./hack/pg-test.sh
#                                           # a database you already run (CI: the
#                                           # job's postgres service); no Docker
#
# The Go tests are env-gated on UPGRADESCOPE_PG_TEST_DSN and skip without it,
# so plain `go test ./...` never needs Docker. This script, in turn, fails if
# the conformance test skipped instead of passing: a skipped suite is not a
# passing one.
set -euo pipefail
cd "$(dirname "$0")/.."

if [ -z "${UPGRADESCOPE_PG_TEST_DSN:-}" ]; then
  CONTAINER=upgradescope-pg-test
  PORT="${PG_TEST_PORT:-55432}"
  PASSWORD=pg-test-secret
  IMAGE="postgres:${PG_VERSION:-17}-alpine"

  if ! docker info >/dev/null 2>&1; then
    echo "pg-test: docker daemon not reachable (or set UPGRADESCOPE_PG_TEST_DSN)" >&2
    exit 1
  fi

  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  docker run --rm -d --name "$CONTAINER" \
    -e POSTGRES_PASSWORD="$PASSWORD" \
    -e POSTGRES_DB=upgradescope \
    -p "127.0.0.1:${PORT}:5432" \
    "$IMAGE" >/dev/null
  trap 'docker stop "$CONTAINER" >/dev/null 2>&1 || true' EXIT

  echo "pg-test: waiting for $IMAGE on 127.0.0.1:${PORT} ..."
  for _ in $(seq 1 60); do
    # pg_isready alone is not enough: the entrypoint restarts the server once
    # after init, so also require the target DB to answer a real query.
    if docker exec "$CONTAINER" psql -U postgres -d upgradescope -c 'SELECT 1' >/dev/null 2>&1; then
      ready=1
      break
    fi
    sleep 0.5
  done
  if [ -z "${ready:-}" ]; then
    echo "pg-test: postgres did not become ready in 30s" >&2
    docker logs "$CONTAINER" | tail -20 >&2
    exit 1
  fi
  export UPGRADESCOPE_PG_TEST_DSN="postgres://postgres:${PASSWORD}@127.0.0.1:${PORT}/upgradescope?sslmode=disable"
fi

out=$(mktemp)
trap 'rm -f "$out"; [ -z "${CONTAINER:-}" ] || docker stop "$CONTAINER" >/dev/null 2>&1 || true' EXIT
go test ./internal/server/store/... -run 'Postgres' -count=1 -race -v "$@" 2>&1 | tee "$out"
grep -q '^--- PASS: TestPostgresConformance' "$out" ||
  { echo "pg-test: TestPostgresConformance did not pass (skipped?)" >&2; exit 1; }

# The server package's tests that run on both stores (forEachStore: the
# notification baselines of #380, the stored annotations of #381, ...) skip
# their postgres subtest without the DSN, so they run here too. They run after
# the store package, never beside it: both packages migrate the same database
# under one advisory lock, and the store's migration-lock tests probe that lock.
# The list is read from the test files, so a new forEachStore test joins it.
names=$(awk '/^func Test[A-Za-z0-9_]*\(t \*testing\.T\)/ { match($0, /Test[A-Za-z0-9_]*/); cur = substr($0, RSTART, RLENGTH) }
  /forEachStore\(/ && cur != "" { print cur; cur = "" }' internal/server/*_test.go | sort -u | paste -sd'|' -)
[ -n "$names" ] || { echo "pg-test: found no forEachStore test in internal/server" >&2; exit 1; }
go test ./internal/server/ -run "^(${names})\$" -count=1 -race -v "$@" 2>&1 | tee "$out"
grep -Eq '^ *--- PASS: [A-Za-z0-9_]+/postgres' "$out" ||
  { echo "pg-test: no internal/server postgres subtest passed (skipped?)" >&2; exit 1; }
if grep -Eq '^ *--- SKIP: [A-Za-z0-9_]+/postgres' "$out"; then
  echo "pg-test: an internal/server postgres subtest skipped:" >&2
  grep -E '^ *--- SKIP: [A-Za-z0-9_]+/postgres' "$out" >&2
  exit 1
fi
