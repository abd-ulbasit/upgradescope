#!/usr/bin/env bash
# pg-test.sh — run the Postgres store conformance suite against a real,
# throwaway postgres:17-alpine container (Docker via Colima on this Mac).
#
#   ./hack/pg-test.sh            # start container, run tests, tear down
#   PG_TEST_PORT=55433 ./hack/pg-test.sh
#
# The Go tests are env-gated on UPGRADESCOPE_PG_TEST_DSN and skip without it,
# so plain `go test ./...` never needs Docker.
set -euo pipefail
cd "$(dirname "$0")/.."

CONTAINER=upgradescope-pg-test
PORT="${PG_TEST_PORT:-55432}"
PASSWORD=pg-test-secret

if ! docker info >/dev/null 2>&1; then
    echo "pg-test: docker daemon not reachable — try: colima start" >&2
    exit 1
fi

docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
docker run --rm -d --name "$CONTAINER" \
    -e POSTGRES_PASSWORD="$PASSWORD" \
    -e POSTGRES_DB=upgradescope \
    -p "127.0.0.1:${PORT}:5432" \
    postgres:17-alpine >/dev/null
trap 'docker stop "$CONTAINER" >/dev/null 2>&1 || true' EXIT

echo "pg-test: waiting for postgres on 127.0.0.1:${PORT} ..."
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
go test ./internal/server/store/... -run 'Postgres' -count=1 -race -v "$@"
