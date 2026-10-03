#!/usr/bin/env bash
# Offline tests for hack/bench/ (make hack-test; #71): the safety checks of
# agent.sh (it touches only the kubeconfig it is given, and only a cluster
# that looks disposable), serve.sh's backend and Docker checks, and the two
# report scripts over fixture results. kubectl, go and docker are stubs, so
# nothing here needs a cluster, a database or a build. Needs bash and jq.
set -euo pipefail
cd "$(dirname "$0")/.."
repo=$PWD

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
: >"$work/results"
ok() { echo "ok   $1" | tee -a "$work/results"; }
fail() {
  echo "FAIL $1" >&2
  [ -z "${2:-}" ] || sed 's/^/     /' "$2" >&2
  echo "FAIL $1" >>"$work/results"
}

# expect <name> <want exit> <want output substring> -- <command...>
expect() {
  local name=$1 want=$2 sub=$3
  shift 4
  local rc=0
  "$@" >"$work/out" 2>&1 || rc=$?
  if [ "$rc" = "$want" ] && grep -qF -- "$sub" "$work/out"; then ok "$name"; else fail "$name (exit $rc, want $want containing '$sub')" "$work/out"; fi
}

# --- stubs ---------------------------------------------------------------
stubs=$work/stubs
mkdir -p "$stubs"
# kubectl answers what the guard asks from fixture files: $NODES_JSON, $NS_JSON.
cat >"$stubs/kubectl" <<'STUB'
#!/usr/bin/env bash
args="$*"
case "$args" in
  *"config view"*) echo "https://lab.example:6443" ;;
  *"config current-context"*) echo "kind-lab" ;;
  *"get nodes -o json"*) cat "$NODES_JSON" ;;
  *"get ns -o json"*) cat "$NS_JSON" ;;
  *) echo "kubectl stub: unexpected $args" >&2; exit 99 ;;
esac
STUB
# go stops the script right after the guard: exit 7 is the sentinel.
cat >"$stubs/go" <<'STUB'
#!/usr/bin/env bash
echo "go stub: $*" >&2
exit "${GO_STUB_RC:-7}"
STUB
cat >"$stubs/docker" <<'STUB'
#!/usr/bin/env bash
case "$1" in
  info) exit 1 ;;
  context) echo "unix:///var/run/docker.sock" ;;
esac
STUB
chmod +x "$stubs"/*
export PATH="$stubs:$PATH"

nodes() { # nodes <real> [<fake>] -> a node list
  jq -n --argjson real "$1" --argjson fake "${2:-0}" '{items: ([range($real)] | map({metadata: {name: "real-\(.)"}})) + ([range($fake)] | map({metadata: {name: "fake-\(.)", annotations: {"kwok.x-k8s.io/node": "fake"}}}))}'
}
namespaces() { jq -n --args '{items: ($ARGS.positional | map({metadata: {name: .}}))}' "$@"; }
vanilla_ns=(default kube-system kube-public kube-node-lease local-path-storage)

lab=$work/lab-kubeconfig
echo "apiVersion: v1" >"$lab"
export NODES_JSON=$work/nodes.json NS_JSON=$work/ns.json
nodes 1 >"$NODES_JSON"
namespaces "${vanilla_ns[@]}" >"$NS_JSON"

agent=hack/bench/agent.sh

# --- agent.sh: which cluster it will touch ---------------------------------
expect "agent.sh: no KUBECONFIG is refused" 1 "KUBECONFIG is required" -- env -u KUBECONFIG "$agent"
expect "agent.sh: a list of kubeconfigs is refused" 1 "single file" -- env "KUBECONFIG=$lab:$lab" "$agent"
expect "agent.sh: a missing file is refused" 1 "is not a file" -- env "KUBECONFIG=$work/nope" "$agent"
mkdir -p "$work/home/.kube"
cp "$lab" "$work/home/.kube/config"
expect "agent.sh: ~/.kube/config is refused" 1 "is ~/.kube/config" -- env "HOME=$work/home" "KUBECONFIG=$work/home/.kube/config" "$agent"

env_lab=(env "KUBECONFIG=$lab" "BENCH_BIN=$work/bin" "GO_STUB_RC=7")
nodes 4 >"$NODES_JSON"
expect "agent.sh: a cluster with 4 real nodes is refused" 1 "4 real nodes" -- "${env_lab[@]}" "$agent"
nodes 1 2000 >"$NODES_JSON"
namespaces "${vanilla_ns[@]}" production >"$NS_JSON"
expect "agent.sh: a cluster with a production namespace is refused" 1 "production" -- "${env_lab[@]}" "$agent"
namespaces "${vanilla_ns[@]}" bench-ns-001 bench-ns-002 >"$NS_JSON"
expect "agent.sh: a vanilla cluster, even a seeded one (fake nodes, bench namespaces), passes the guard" 7 "building the seeder and the agent benchmark" -- "${env_lab[@]}" "$agent"

# --- agent-report.sh over fixture ticks -------------------------------------
tick() { # tick <label> <n> <nodes> <requests> <wallMs> <getSecrets>
  jq -nc --arg label "$1" --argjson tick "$2" --argjson nodes "$3" --argjson req "$4" --argjson wall "$5" --argjson gets "$6" '{
    label: $label, tick: $tick, wallMs: $wall, collectMs: ($wall - 100), cpuMs: ($wall / 2), requests: $req,
    byVerbResource: [{verb: "GET", resource: "secrets", count: $gets}, {verb: "LIST", resource: "pods", count: 5}],
    bodyBytes: 10485760, wireDownBytes: 2097152, wireUpBytes: 1048576, connections: 1,
    peakHeapBytes: 52428800, peakRuntimeBytes: 83886080, maxRssBytes: (104857600 + $tick * 1048576),
    nodes: $nodes, namespaces: 3, helmReleases: $gets, addOns: 0, apiUsage: 0, targets: 1, capabilities: {}}'
}
{
  tick "fill=0 nodes=0" 1 1 17 3000 0
  tick "fill=0 nodes=0" 2 1 17 900 0
  tick "fill=0 nodes=0" 3 1 17 1100 0
  tick "fill=1 nodes=2000" 1 2001 1300 40000 1000
  tick "fill=1 nodes=2000" 2 2001 1310 20000 1000
  tick "fill=1 nodes=2000" 3 2001 1312 22000 1000
  tick "fill=1 nodes=2000" 4 2001 1308 21000 1000
} >"$work/agent.jsonl"
"hack/bench/agent-report.sh" "$work/agent.jsonl" >"$work/report" 2>&1 || true
# The median of the steady ticks (after the first), the levels in order of size.
if grep -qF "| 0 | 1 | 0 | 17 | 5 | 0 | 10 | 3 | 0.9 | 0.5 |" "$work/report" &&
  grep -qF "| 1 | 2001 | 1000 | 1310 | 5 | 1000 | 10 | 3 | 21 | 10.5 |" "$work/report" &&
  [ "$(grep -n '^| 0 |' "$work/report" | head -1 | cut -d: -f1)" -lt "$(grep -n '^| 1 |' "$work/report" | head -1 | cut -d: -f1)" ]; then
  ok "agent-report.sh: medians of the ticks after the first, one row per level, smallest first"
else
  fail "agent-report.sh table" "$work/report"
fi
grep -qF "| GET | secrets | 1000 |" "$work/report" && ok "agent-report.sh: requests by verb and resource at the last level" || fail "agent-report.sh breakdown" "$work/report"
BENCH_REPORT_FORMAT=json "hack/bench/agent-report.sh" "$work/agent.jsonl" >"$work/report.json" 2>&1 || true
[ "$(jq 'length' "$work/report.json" 2>/dev/null)" = 2 ] && ok "agent-report.sh: BENCH_REPORT_FORMAT=json" || fail "agent-report.sh json" "$work/report.json"
expect "agent-report.sh: no file is a usage error" 2 "usage" -- hack/bench/agent-report.sh

# --- serve.sh and serve-report.sh --------------------------------------------
expect "serve.sh: an unknown backend is refused" 1 "unknown backend mysql" -- env BENCH_BACKENDS=mysql BENCH_BIN="$work/bin" GO_STUB_RC=0 hack/bench/serve.sh
# Every backend name is checked before anything is built or run: a typo in the
# second must not cost the whole first benchmark (the go stub would say so).
rc=0
env BENCH_BACKENDS="sqlite bogus" BENCH_BIN="$work/bin" GO_STUB_RC=0 hack/bench/serve.sh >"$work/out" 2>&1 || rc=$?
if [ "$rc" = 1 ] && grep -qF "unknown backend bogus" "$work/out" && ! grep -qF "go stub" "$work/out"; then
  ok "serve.sh: a bad backend name is refused before the build, not after the first backend ran"
else
  fail "serve.sh: backend names are validated before the build (exit $rc)" "$work/out"
fi
expect "serve.sh: postgres without a Docker engine is refused" 1 "no Docker engine reachable" -- env BENCH_BACKENDS=postgres BENCH_BIN="$work/bin" GO_STUB_RC=0 hack/bench/serve.sh

# A Docker engine that answers: serve.sh starts its throwaway Postgres (a
# password drawn under pipefail once ended it silently) and gets as far as
# running the benchmark (127: the go stub built no binary to run).
mkdir -p "$work/stubs-docker"
cat >"$work/stubs-docker/docker" <<'STUB'
#!/usr/bin/env bash
case "$1" in
  info) exit 0 ;;
  context) echo "unix:///var/run/docker.sock" ;;
  run) echo "$*" >"$DOCKER_RUN_LOG"; echo "container-id" ;;
  port) echo "127.0.0.1:55432" ;;
  exec) echo "17.0" ;;
  rm) exit 0 ;;
esac
STUB
chmod +x "$work/stubs-docker/docker"
expect "serve.sh: a Docker engine that answers gets a postgres, with a password drawn" 127 "published at 127.0.0.1:55432" -- env PATH="$work/stubs-docker:$PATH" DOCKER_RUN_LOG="$work/docker-run" BENCH_BACKENDS=postgres BENCH_BIN="$work/bin2" GO_STUB_RC=0 hack/bench/serve.sh
# The password reaches the container through the environment, not as a value
# on a command line that `ps` would list.
if grep -qF -- "POSTGRES_PASSWORD" "$work/docker-run" && ! grep -qE -- "POSTGRES_PASSWORD=" "$work/docker-run"; then
  ok "serve.sh: the Postgres password is not on the docker command line"
else
  fail "serve.sh: the Postgres password is on the docker command line" "$work/docker-run"
fi

round() { # round <backend> <pushers> <name> <pushes/s> <p99>
  jq -nc --arg b "$1" --argjson c "$2" --arg r "$3" --argjson tps "$4" --argjson p99 "$5" '{
    backend: $b, concurrency: $c, round: $r, clusters: 200, targets: 3, pushes: 200, accepted: 200, duplicates: 0, failed: 0,
    retried: 2, busy503: 1, transportErrors: 3, pushesPerSecond: $tps, p50Ms: 10, p99Ms: $p99, maxMs: 500,
    cpuMsPerPush: 9.5, peakHeapMiB: 42.4, dbBytesAfter: 41943040, dbBytesPerSnapshot: 102400, postgresRttMs: (if $b == "postgres" then 31.2 else null end)}'
}
{ round sqlite 1 new 20.6 352.2; round postgres 16 changed 55.5 900.04; } >"$work/serve.jsonl"
hack/bench/serve-report.sh "$work/serve.jsonl" >"$work/serve-report" 2>&1 || true
if grep -qF "| sqlite | 1 | new | 20.6 | 10 | 352.2 | 500 | 2 (1 / 3) | 9.5 | 42 | 100 | 40 |" "$work/serve-report" &&
  grep -qF "| postgres (RTT 31.2 ms) | 16 | changed | 55.5 | 10 | 900 | 500 |" "$work/serve-report"; then
  ok "serve-report.sh: one row per round, with the database round trip time for postgres"
else
  fail "serve-report.sh table" "$work/serve-report"
fi

# --- syntax -----------------------------------------------------------------
for f in hack/bench/*.sh; do
  if bash -n "$f" 2>"$work/syntax"; then ok "bash -n $f"; else fail "bash -n $f" "$work/syntax"; fi
done

! grep -q '^FAIL' "$work/results" || exit 1
echo "bench_test: OK ($(grep -c '^ok' "$work/results") checks)"
cd "$repo"
