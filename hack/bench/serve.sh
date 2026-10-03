#!/usr/bin/env bash
# serve.sh — how `upgradescope serve` ingests a fleet (#71;
# docs/operations/scale.md): 200 clusters with realistic inventories, each
# push evaluated for 3 targets, on SQLite and on Postgres. Per round and per
# number of concurrent pushers it reports throughput, p50/p99 push latency
# (as an agent sees it, retries included), the database's growth per snapshot,
# the process's CPU per push and its peak heap.
#
#   hack/bench/serve.sh                      # SQLite, then Postgres
#   BENCH_BACKENDS=sqlite hack/bench/serve.sh
#   BENCH_RUN_ON=thinkpad hack/bench/serve.sh  # run the server and its load on the Docker host
#
# Postgres is a throwaway container on whatever engine `docker` is bound to
# (here a remote one: its published port binds on that host, so it is reached
# at the host's address, not localhost), with its own random password, removed
# when the script ends. BENCH_RUN_ON=local (default) runs the server on this
# machine, so Postgres round trips cross the network; BENCH_RUN_ON=<ssh host>
# cross-compiles the benchmark for linux/amd64, copies it to that host and
# runs it there, next to the database (the host must be the Docker host).
#
# Knobs (environment):
#   BENCH_BACKENDS      default "sqlite postgres"
#   BENCH_CONCURRENCY   concurrent pushers, comma list, default "1,16,200"
#   BENCH_CLUSTERS      default 200
#   BENCH_PG_VERSION    default 17 (image postgres:<v>-alpine)
#   BENCH_BIN           results directory, default bin/bench
# Needs: go, jq, and for Postgres a reachable Docker engine.
set -euo pipefail
cd "$(dirname "$0")/../.."

BENCH_BACKENDS=${BENCH_BACKENDS:-sqlite postgres}
BENCH_CONCURRENCY=${BENCH_CONCURRENCY:-1,16,200}
BENCH_CLUSTERS=${BENCH_CLUSTERS:-200}
BENCH_PG_VERSION=${BENCH_PG_VERSION:-17}
BENCH_RUN_ON=${BENCH_RUN_ON:-local}
BENCH_BIN=${BENCH_BIN:-bin/bench}
mkdir -p "$BENCH_BIN"
BENCH_BIN=$(cd "$BENCH_BIN" && pwd -P)

die() { echo "bench-serve: $*" >&2; exit 1; }
command -v jq >/dev/null || die "jq is required"
# Every name before the build: a typo in the last must not cost the first
# backend's whole run.
for backend in $BENCH_BACKENDS; do
  case "$backend" in
    sqlite | postgres) ;;
    *) die "unknown backend $backend (sqlite or postgres)" ;;
  esac
done

stamp=$(date -u +%Y%m%dT%H%M%SZ)
results=$BENCH_BIN/serve-$BENCH_RUN_ON-$stamp.jsonl
: >"$results"

container=""
cleanup() {
  if [ -n "$container" ]; then
    docker rm -f "$container" >/dev/null 2>&1 || true
  fi
  if [ -n "${remote_dir:-}" ]; then
    # shellcheck disable=SC2029 # expanded here on purpose
    ssh "$BENCH_RUN_ON" "rm -rf '$remote_dir'" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

# The address the Docker host's published ports are reached at: the engine's
# host (resolved through ssh config, so an alias such as "thinkpad" works),
# or loopback for a local engine.
docker_host_address() {
  local ep host
  ep=$(docker context inspect --format '{{.Endpoints.docker.Host}}' 2>/dev/null || true)
  case "$ep" in
    ssh://*)
      host=${ep#ssh://}
      host=${host#*@}
      host=${host%%:*}
      host=${host%%/*}
      ssh -G "$host" 2>/dev/null | awk '/^hostname /{print $2; exit}' | grep . || echo "$host"
      ;;
    tcp://*)
      host=${ep#tcp://}
      echo "${host%%:*}"
      ;;
    *) echo 127.0.0.1 ;;
  esac
}

start_postgres() { # sets PG_DSN (as the machine running the benchmark reaches it)
  docker info >/dev/null 2>&1 || die "no Docker engine reachable (needed for the postgres backend)"
  local addr password port
  addr=$(docker_host_address)
  # od reads a fixed amount: a `tr </dev/urandom | head` pipeline dies of SIGPIPE
  # under pipefail, and set -e ends the script without a word.
  password=$(od -An -N18 -tx1 /dev/urandom | tr -d ' \n')
  container=upgradescope-bench-pg-$$
  # Published on the engine host's address only (not every interface).
  # The password goes by name (-e NAME, read from this process's environment),
  # so it is on no command line `ps` would list; the engine host still holds it
  # in the container's configuration, which dies with the container.
  POSTGRES_PASSWORD=$password docker run -d --name "$container" -e POSTGRES_PASSWORD -e POSTGRES_DB=upgradescope \
    -p "$addr::5432" "postgres:${BENCH_PG_VERSION}-alpine" >/dev/null
  port=$(docker port "$container" 5432/tcp | head -1 | sed 's/.*://')
  local ready=""
  for _ in $(seq 1 90); do
    if docker exec "$container" psql -U postgres -d upgradescope -c 'SELECT 1' >/dev/null 2>&1; then ready=1; break; fi
    sleep 0.5
  done
  [ -n "$ready" ] || { docker logs "$container" | tail -20 >&2; die "postgres did not become ready"; }
  PG_VERSION_STRING=$(docker exec "$container" psql -U postgres -At -c 'SHOW server_version')
  # The port is published on $addr only, which the Docker host reaches
  # itself as well as anyone else does (not on its loopback).
  PG_DSN="postgres://postgres:$password@$addr:$port/upgradescope?sslmode=disable"
  echo "bench-serve: postgres $PG_VERSION_STRING ($container) published at $addr:$port" >&2
}

# --- build the benchmark from this checkout -------------------------------
if [ "$BENCH_RUN_ON" = local ]; then
  go test -c -o "$BENCH_BIN/server-bench.test" ./internal/server
  runner=("$BENCH_BIN/server-bench.test")
else
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -o "$BENCH_BIN/server-bench-linux-amd64.test" ./internal/server
  remote_dir=$(ssh "$BENCH_RUN_ON" 'mktemp -d /tmp/upgradescope-bench.XXXXXX')
  scp -q "$BENCH_BIN/server-bench-linux-amd64.test" "$BENCH_RUN_ON:$remote_dir/server.test"
fi

run_backend() { # run_backend <backend> [PG_DSN]
  local backend=$1 dsn=${2:-} out=$results
  local vars=(UPGRADESCOPE_BENCH_INGEST=1 UPGRADESCOPE_BENCH_BACKEND="$backend" UPGRADESCOPE_BENCH_CONCURRENCY="$BENCH_CONCURRENCY"
    UPGRADESCOPE_BENCH_CLUSTERS="$BENCH_CLUSTERS")
  [ -z "$dsn" ] || vars+=(UPGRADESCOPE_BENCH_PG_DSN="$dsn")
  if [ "$BENCH_RUN_ON" = local ]; then
    env "${vars[@]}" UPGRADESCOPE_BENCH_OUT="$out" "${runner[@]}" -test.run '^TestBenchServeIngest$' -test.v -test.timeout 60m >&2
  else
    local remote_out=$remote_dir/out.jsonl
    # The environment goes over stdin into a private file: the DSN holds the
    # throwaway database's password, which a command line would show to
    # anyone listing processes on either machine. $remote_dir is expanded
    # here on purpose, the remote shell gets the finished path.
    # shellcheck disable=SC2029
    {
      for kv in "${vars[@]}"; do printf 'export %s\n' "$(printf '%q' "$kv")"; done
    } | ssh "$BENCH_RUN_ON" "umask 077; cat > '$remote_dir/env'"
    # shellcheck disable=SC2029
    ssh "$BENCH_RUN_ON" "cd '$remote_dir' && . ./env && rm -f '$remote_out' && UPGRADESCOPE_BENCH_OUT='$remote_out' ./server.test -test.run '^TestBenchServeIngest\$' -test.v -test.timeout 60m" >&2
    # shellcheck disable=SC2029
    ssh "$BENCH_RUN_ON" "cat '$remote_out'" >>"$out"
  fi
}

for backend in $BENCH_BACKENDS; do
  case "$backend" in
    sqlite) run_backend sqlite ;;
    postgres) start_postgres; run_backend postgres "$PG_DSN"; docker rm -f "$container" >/dev/null; container="" ;;
  esac
done

"$(dirname "$0")/serve-report.sh" "$results"
echo "bench-serve: raw per-round results: $results" >&2
