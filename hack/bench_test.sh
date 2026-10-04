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
  jq -n --argjson real "$1" --argjson fake "${2:-0}" '{items: (([range($real)] | map({metadata: {name: "real-\(.)"}})) + ([range($fake)] | map({metadata: {name: "fake-\(.)", annotations: {"kwok.x-k8s.io/node": "fake"}}})))}'
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
  # Four steady ticks, the count of a default run (BENCH_TICKS=5), in no
  # particular order: the median of an even count is the mean of the two middle
  # values, not the lower of them.
  tick "fill=2 nodes=3000" 1 3001 900 50000 1500
  tick "fill=2 nodes=3000" 2 3001 70 4000 1500
  tick "fill=2 nodes=3000" 3 3001 40 1000 1500
  tick "fill=2 nodes=3000" 4 3001 60 3000 1500
  tick "fill=2 nodes=3000" 5 3001 50 2000 1500
} >"$work/agent.jsonl"
"hack/bench/agent-report.sh" "$work/agent.jsonl" >"$work/report" 2>&1 || true
# The median of the steady ticks (after the first), the levels in order of size:
# two steady ticks (the mean of both: 0.9 s and 1.1 s give 1.0), three (the
# middle one) and four (the mean of the middle two: 40, 50, 60 and 70 requests
# give 55, and 1, 2, 3 and 4 s give 2.5, not the lower median 2).
if grep -qF "| 0 | 1 | 0 | 17 | 5 | 0 | 10 | 3 | 1 | 0.5 |" "$work/report" &&
  grep -qF "| 1 | 2001 | 1000 | 1310 | 5 | 1000 | 10 | 3 | 21 | 10.5 |" "$work/report" &&
  grep -qF "| 2 | 3001 | 1500 | 55 | 5 | 1500 | 10 | 3 | 2.5 | 1.3 |" "$work/report" &&
  grep -qF "| 900, 10, 50, 25 |" "$work/report" &&
  [ "$(grep -n '^| 0 |' "$work/report" | head -1 | cut -d: -f1)" -lt "$(grep -n '^| 1 |' "$work/report" | head -1 | cut -d: -f1)" ]; then
  ok "agent-report.sh: medians of the ticks after the first, one row per level, smallest first"
else
  fail "agent-report.sh table" "$work/report"
fi
grep -qF "| GET | secrets | 1500 |" "$work/report" && ok "agent-report.sh: requests by verb and resource at the last level" || fail "agent-report.sh breakdown" "$work/report"
BENCH_REPORT_FORMAT=json "hack/bench/agent-report.sh" "$work/agent.jsonl" >"$work/report.json" 2>&1 || true
[ "$(jq 'length' "$work/report.json" 2>/dev/null)" = 3 ] && ok "agent-report.sh: BENCH_REPORT_FORMAT=json" || fail "agent-report.sh json" "$work/report.json"
expect "agent-report.sh: no file is a usage error" 2 "usage" -- hack/bench/agent-report.sh
# A run without the GitOps fill prints no GitOps table (its ticks have no
# gitopsCharts), and the json summary says gitops: null.
if grep -qF "GitOps reads per tick" "$work/report"; then fail "agent-report.sh: a run without the GitOps fill printed a GitOps table" "$work/report"; else ok "agent-report.sh: no GitOps table without the GitOps fill"; fi
[ "$(jq '[.[] | .gitops] | unique' "$work/report.json" 2>/dev/null | tr -d ' \n')" = "[null]" ] && ok "agent-report.sh: json gitops is null without the fill" || fail "agent-report.sh json gitops without the fill" "$work/report.json"

# With it: ticks that read GitOps charts and record the bytes of each
# resource. Steady ticks after the first: Applications 20, 22, 24 and 26
# requests (the mean of the middle two is 23), 1, 2, 3 and 4 MiB (2.5 MiB);
# HelmReleases 20 requests at 2 MiB; 5 OCIRepository GETs of 0.5 MiB in all.
gtick() { # gtick <fill> <n> <charts> <app requests> <app MiB>
  jq -nc --arg label "fill=$1 nodes=10 argocd=100 flux=100" --argjson tick "$2" --argjson charts "$3" --argjson areq "$4" --argjson amib "$5" '{
    label: $label, tick: $tick, wallMs: 2000, collectMs: 1900, cpuMs: 1000, requests: ($areq + 40),
    byVerbResource: [{verb: "LIST", resource: "applications", count: $areq, bytes: ($amib * 1048576)},
      {verb: "LIST", resource: "helmreleases", count: 20, bytes: 2097152},
      {verb: "GET", resource: "ocirepositories", count: 5, bytes: 524288},
      {verb: "LIST", resource: "pods", count: 5, bytes: 1048576}],
    bodyBytes: 10485760, wireDownBytes: 2097152, wireUpBytes: 1048576, connections: 1,
    peakHeapBytes: 52428800, peakRuntimeBytes: 83886080, maxRssBytes: 104857600,
    nodes: 11, namespaces: 3, helmReleases: 10, gitopsCharts: $charts, addOns: 0, apiUsage: 0, targets: 1, capabilities: {}}'
}
{
  gtick 0 1 0 1 0
  gtick 0 2 0 1 0
  gtick 1 1 200 99 9
  gtick 1 2 200 20 1
  gtick 1 3 200 26 4
  gtick 1 4 200 22 2
  gtick 1 5 200 24 3
} >"$work/gitops.jsonl"
"hack/bench/agent-report.sh" "$work/gitops.jsonl" >"$work/gitops-report" 2>&1 || true
if grep -qF "GitOps reads per tick" "$work/gitops-report" &&
  grep -qF "| 1 | 200 | 23, 2.5 | 20, 2 | 5, 0.5 | 48, 5 | 124 |" "$work/gitops-report"; then
  ok "agent-report.sh: the GitOps table, medians of requests and response MiB per resource"
else
  fail "agent-report.sh GitOps table" "$work/gitops-report"
fi
# The empty level belongs in the table of a run that had GitOps charts, with none read.
grep -qE '^\| 0 \| 0 \| 1, 0 \|' "$work/gitops-report" && ok "agent-report.sh: the empty level is in the GitOps table, with no charts" || fail "agent-report.sh GitOps table lacks the empty level" "$work/gitops-report"
grep -qF "| LIST | applications | 20 | 1024 |" "$work/gitops-report" && ok "agent-report.sh: the breakdown shows each resource's response KiB" || fail "agent-report.sh breakdown bytes" "$work/gitops-report"
BENCH_REPORT_FORMAT=json "hack/bench/agent-report.sh" "$work/gitops.jsonl" 2>&1 | jq -e ".[0].gitops.charts == 0 and .[1].gitops.charts == 200 and .[1].gitops.requests == 48" >/dev/null && ok "agent-report.sh: json carries the GitOps summary" || fail "agent-report.sh json gitops" "$work/gitops-report"

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
# running the benchmark.
mkdir -p "$work/stubs-docker"
cat >"$work/stubs-docker/docker" <<'STUB'
#!/usr/bin/env bash
echo "$*" >>"${DOCKER_LOG:-/dev/null}"
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
# The go stub builds a fake benchmark binary (exit status FAKE_BENCH_RC, else
# it appends $ROUND_FIXTURE to the results as the real one would). A stub env
# on the PATH records what serve.sh asks it to run.
okbin=$work/stubs-ok
mkdir -p "$okbin"
cp "$work/stubs-docker/docker" "$okbin/docker"
cat >"$okbin/go" <<'STUB'
#!/usr/bin/env bash
out=""
while [ $# -gt 0 ]; do
  if [ "$1" = -o ]; then out=$2; shift; fi
  shift
done
cat >"$out" <<'BIN'
#!/usr/bin/env bash
echo "$*" >>"${ARGV_LOG:-/dev/null}"
[ -z "${UPGRADESCOPE_BENCH_PG_DSN:-}" ] || echo seen >>"${DSN_LOG:-/dev/null}"
[ -z "${FAKE_BENCH_RC:-}" ] || exit "$FAKE_BENCH_RC"
cat "$ROUND_FIXTURE" >>"$UPGRADESCOPE_BENCH_OUT"
BIN
chmod +x "$out"
STUB
cat >"$okbin/env" <<'STUB'
#!/usr/bin/env bash
echo "$*" >>"${ENV_LOG:-/dev/null}"
exec /usr/bin/env "$@"
STUB
chmod +x "$okbin"/*
# The benchmark fails (exit 7): the run still got as far as a started Postgres.
expect "serve.sh: a Docker engine that answers gets a postgres, with a password drawn" 7 "published at 127.0.0.1:55432" -- env PATH="$okbin:$PATH" DOCKER_LOG="$work/docker-log" DOCKER_RUN_LOG="$work/docker-run" FAKE_BENCH_RC=7 BENCH_BACKENDS=postgres BENCH_BIN="$work/bin2" hack/bench/serve.sh
# The throwaway Postgres is removed when the script ends, even when the run
# fails: the container the script started is the one it removed.
pg_name=$(sed -n 's/.*--name \([^ ]*\).*/\1/p' "$work/docker-run")
if [ -n "$pg_name" ] && grep -qxF -- "rm -f $pg_name" "$work/docker-log"; then
  ok "serve.sh: the throwaway Postgres is removed when the run fails (trap)"
else
  fail "serve.sh: no 'rm -f ${pg_name:-<container>}' after a failed run" "$work/docker-log"
fi
# Readiness is probed over TCP: while the image initialises the database its
# temporary server listens on the unix socket only.
if grep -qE -- '^exec .* pg_isready .*-h 127\.0\.0\.1' "$work/docker-log"; then
  ok "serve.sh: Postgres readiness is probed over TCP, not the unix socket"
else
  fail "serve.sh: readiness is not probed with pg_isready -h 127.0.0.1" "$work/docker-log"
fi
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

# A run that succeeds: the container is removed on the normal path too, and
# the DSN, which holds the password, reaches the benchmark through its
# environment, never as an argument of any process serve.sh starts (the stub
# env recorded what it was asked to run).
round postgres 1 new 38.6 120.5 >"$work/pg-round.jsonl"
rm -f "$work/docker-log" "$work/docker-run"
: >"$work/argv-log" && : >"$work/dsn-log" && : >"$work/env-log"
expect "serve.sh: a successful postgres run ends in the report" 0 "raw per-round results" -- env PATH="$okbin:$PATH" DOCKER_LOG="$work/docker-log" DOCKER_RUN_LOG="$work/docker-run" ARGV_LOG="$work/argv-log" DSN_LOG="$work/dsn-log" ENV_LOG="$work/env-log" ROUND_FIXTURE="$work/pg-round.jsonl" BENCH_BACKENDS=postgres BENCH_BIN="$work/bin3" hack/bench/serve.sh
pg_name=$(sed -n 's/.*--name \([^ ]*\).*/\1/p' "$work/docker-run")
if [ -n "$pg_name" ] && grep -qxF -- "rm -f $pg_name" "$work/docker-log"; then
  ok "serve.sh: the throwaway Postgres is removed when the run succeeds"
else
  fail "serve.sh: no 'rm -f ${pg_name:-<container>}' after a successful run" "$work/docker-log"
fi
if [ -s "$work/dsn-log" ] && ! grep -qF -- "postgres://" "$work/argv-log" "$work/env-log"; then
  ok "serve.sh: the Postgres DSN reaches the benchmark in its environment, not on any command line"
else
  fail "serve.sh: the DSN was missing from the benchmark's environment or was on a command line" "$work/env-log"
fi

# --- make bench-agent ----------------------------------------------------------
# The Makefile target refuses before running anything unless KUBECONFIG is on
# make's command line: a KUBECONFIG that is only in the environment (the
# shell's default, an exported variable) is never enough.
expect "make bench-agent: no KUBECONFIG at all is refused" 2 "on the command line" -- env -u KUBECONFIG make bench-agent
expect "make bench-agent: KUBECONFIG only in the environment is refused" 2 "on the command line" -- env "KUBECONFIG=$lab" make bench-agent
if grep -qE "bench-agent: cluster|building the seeder|go stub|kubectl stub" "$work/out"; then
  fail "make bench-agent: the refusal came after the script started work" "$work/out"
else
  ok "make bench-agent: the refusal runs nothing (no kubectl, no go)"
fi
nodes 1 >"$NODES_JSON"
namespaces "${vanilla_ns[@]}" >"$NS_JSON"
expect "make bench-agent: KUBECONFIG on the command line reaches agent.sh (make reports its failure as 2)" 2 "building the seeder and the agent benchmark" -- env -u KUBECONFIG "BENCH_BIN=$work/bin" GO_STUB_RC=7 make bench-agent "KUBECONFIG=$lab"

# --- agent.sh and serve.sh from any directory --------------------------------
# A relative KUBECONFIG is relative to the caller, and the scripts find the
# repository when started from their own directory. (The scripts change
# directory to the repository root, so a relative path to the kubeconfig
# would not exist any more if it were resolved after that.)
cp "$lab" "$work/rel-kubeconfig"
expect "agent.sh: a relative KUBECONFIG is resolved against the caller's directory" 7 "building the seeder and the agent benchmark" -- bash -c 'cd "$1" && KUBECONFIG=rel-kubeconfig BENCH_BIN="$1/bin" GO_STUB_RC=7 "$2/hack/bench/agent.sh"' _ "$work" "$repo"
expect "agent.sh: started from its own directory it finds the repository" 7 "building the seeder and the agent benchmark" -- bash -c 'cd "$1/hack/bench" && KUBECONFIG="$2" BENCH_BIN="$3/bin" GO_STUB_RC=7 ./agent.sh' _ "$repo" "$lab" "$work"
expect "serve.sh: started from its own directory it finds the repository" 1 "unknown backend nope" -- bash -c 'cd "$1/hack/bench" && BENCH_BACKENDS=nope BENCH_BIN="$2/bin" ./serve.sh' _ "$repo" "$work"

# --- agent.sh: BENCH_NO_HELM_CACHE -------------------------------------------
# The knob that gives the Helm step no cache (the "before" rows of
# docs/operations/scale.md) is 1 or unset; anything else is refused before a
# build or a cluster call, not after the lab was seeded. And when it is set, the
# benchmark process gets it (UPGRADESCOPE_BENCH_NO_HELM_CACHE=1); when it is
# not, the benchmark gets nothing, so the cache is on.
nodes 1 2000 >"$NODES_JSON"
namespaces "${vanilla_ns[@]}" bench-ns-001 >"$NS_JSON"
for bad in 0 true yes; do
  expect "agent.sh: BENCH_NO_HELM_CACHE=$bad is refused, before the build" 1 "BENCH_NO_HELM_CACHE" -- env "KUBECONFIG=$lab" "BENCH_BIN=$work/bin" "BENCH_NO_HELM_CACHE=$bad" GO_STUB_RC=0 "$agent"
  if grep -qE "go stub|bench-agent: cluster" "$work/out"; then
    fail "agent.sh: BENCH_NO_HELM_CACHE=$bad was refused after the script started work" "$work/out"
  else
    ok "agent.sh: BENCH_NO_HELM_CACHE=$bad is refused before anything is built or any cluster is contacted"
  fi
done
# A run that gets as far as the measured process, with everything it would
# download or build stubbed: the go stub makes a fake benchmark binary that
# records the knob's value, kubectl accepts the KWOK install, curl "downloads"
# files and sha256sum answers with the digests agent.sh pins.
nc=$work/stubs-nocache
mkdir -p "$nc"
cat >"$nc/kubectl" <<'STUB'
#!/usr/bin/env bash
args="$*"
case "$args" in
  *"config view"*) echo "https://lab.example:6443" ;;
  *"config current-context"*) echo "kind-lab" ;;
  *"get nodes -o json"*) cat "$NODES_JSON" ;;
  *"get ns -o json"*) cat "$NS_JSON" ;;
  *apply* | *rollout* | *wait*) exit 0 ;;
  *) echo "kubectl stub: unexpected $args" >&2; exit 99 ;;
esac
STUB
cat >"$nc/go" <<'STUB'
#!/usr/bin/env bash
out=""
while [ $# -gt 0 ]; do
  if [ "$1" = -o ]; then out=$2; shift; fi
  shift
done
cat >"$out" <<'BIN'
#!/usr/bin/env bash
echo "knob=${UPGRADESCOPE_BENCH_NO_HELM_CACHE-unset}" >>"$KNOB_LOG"
BIN
chmod +x "$out"
STUB
cat >"$nc/curl" <<'STUB'
#!/usr/bin/env bash
while [ $# -gt 0 ]; do
  if [ "$1" = -o ]; then echo stub >"$2"; shift; fi
  shift
done
STUB
cat >"$nc/sha256sum" <<'STUB'
#!/usr/bin/env bash
case "$1" in *stage-fast*) echo "$SHA_STAGE  $1" ;; *) echo "$SHA_KWOK  $1" ;; esac
STUB
cp "$nc/sha256sum" "$nc/shasum"
chmod +x "$nc"/*
sha_kwok=$(sed -n 's/^KWOK_SHA256_kwok_yaml=//p' hack/bench/agent.sh)
sha_stage=$(sed -n 's/^KWOK_SHA256_stage_fast_yaml=//p' hack/bench/agent.sh)
run_nocache() { # run_nocache <log> [<BENCH_NO_HELM_CACHE value>]: one tick at the vanilla fill
  : >"$1"
  local vars=(env -u UPGRADESCOPE_BENCH_NO_HELM_CACHE -u BENCH_NO_HELM_CACHE PATH="$nc:$PATH" "KUBECONFIG=$lab" "BENCH_BIN=$work/bin-nc" BENCH_STEPS=0 BENCH_TICKS=1 "KNOB_LOG=$1" "SHA_KWOK=$sha_kwok" "SHA_STAGE=$sha_stage")
  [ $# -lt 2 ] || vars+=("BENCH_NO_HELM_CACHE=$2")
  # The report that follows finds no results (the fake binary writes none): exit 2.
  "${vars[@]}" "$agent" >"$work/out" 2>&1 || true
}
run_nocache "$work/knob-set" 1
if [ "$(cat "$work/knob-set")" = "knob=1" ]; then
  ok "agent.sh: BENCH_NO_HELM_CACHE=1 reaches the benchmark as UPGRADESCOPE_BENCH_NO_HELM_CACHE=1"
else
  fail "agent.sh: BENCH_NO_HELM_CACHE=1 did not reach the benchmark" "$work/out"
fi
run_nocache "$work/knob-unset"
if [ "$(cat "$work/knob-unset")" = "knob=unset" ]; then
  ok "agent.sh: without BENCH_NO_HELM_CACHE the benchmark's variable is unset (the cache is on)"
else
  fail "agent.sh: the benchmark got a no-cache variable nobody asked for" "$work/out"
fi


# --- agent.sh: BENCH_GITOPS ---------------------------------------------------
# The opt-in GitOps fill (#233): anything but 1 is refused before a build or a
# cluster call; unset, nothing of it happens (no CRD installed, no seeder flag,
# no benchmark variable), so existing runs are unchanged; set, the pinned CRDs
# (and only the OCIRepository one of source-controller's six) are applied, the
# seeder gets counts scaled by the fill, and the benchmark is told how many
# GitOps charts the collector must read back.
for bad in 0 true yes; do
  expect "agent.sh: BENCH_GITOPS=$bad is refused, before the build" 1 "BENCH_GITOPS" -- env "KUBECONFIG=$lab" "BENCH_BIN=$work/bin" "BENCH_GITOPS=$bad" GO_STUB_RC=0 "$agent"
  if grep -qE "go stub|bench-agent: cluster" "$work/out"; then fail "agent.sh: BENCH_GITOPS=$bad was refused after the script started work" "$work/out"; else ok "agent.sh: BENCH_GITOPS=$bad is refused before anything is built or any cluster is contacted"; fi
done
expect "agent.sh: BENCH_GITOPS_APPS must be a number" 1 "BENCH_GITOPS_APPS" -- env "KUBECONFIG=$lab" "BENCH_BIN=$work/bin" BENCH_GITOPS=1 BENCH_GITOPS_APPS=ten GO_STUB_RC=0 "$agent"
gs=$work/stubs-gitops
mkdir -p "$gs"
cp "$nc/kubectl" "$gs/kubectl"
sed -i.bak 's|^args="\$\*"$|args="$*"; echo "$args" >>"${KUBECTL_LOG:-/dev/null}"|' "$gs/kubectl"
rm -f "$gs/kubectl.bak"
cat >"$gs/go" <<'STUB'
#!/usr/bin/env bash
out=""
while [ $# -gt 0 ]; do
  if [ "$1" = -o ]; then out=$2; shift; fi
  shift
done
cat >"$out" <<'BIN'
#!/usr/bin/env bash
echo "args=$* gitops=${UPGRADESCOPE_BENCH_GITOPS-unset} expect=${UPGRADESCOPE_BENCH_EXPECT_GITOPS-unset}" >>"$KNOB_LOG"
BIN
chmod +x "$out"
STUB
cat >"$gs/curl" <<'STUB'
#!/usr/bin/env bash
out="" url=""
while [ $# -gt 0 ]; do
  if [ "$1" = -o ]; then out=$2; shift; else url=$1; fi
  shift
done
case "$url" in
  *source-controller*) printf -- '---\nkind: CustomResourceDefinition\nmetadata:\n  name: buckets.source.toolkit.fluxcd.io\n---\nkind: CustomResourceDefinition\nmetadata:\n  name: ocirepositories.source.toolkit.fluxcd.io\n---\nkind: CustomResourceDefinition\nmetadata:\n  name: gitrepositories.source.toolkit.fluxcd.io\n' >"$out" ;;
  *) echo stub >"$out" ;;
esac
STUB
cat >"$gs/sha256sum" <<'STUB'
#!/usr/bin/env bash
case "$1" in
  *stage-fast*) d=$SHA_STAGE ;;
  *kwok.yaml*) d=$SHA_KWOK ;;
  *application-crd*) d=$SHA_ARGO ;;
  *helm-controller*) d=$SHA_HELM ;;
  *source-controller*) d=$SHA_SRC ;;
  *) d=unknown ;;
esac
echo "$d  $1"
STUB
cp "$gs/sha256sum" "$gs/shasum"
chmod +x "$gs"/*
pin() { sed -n "s/^$1=//p" hack/bench/agent.sh; }
run_gitops() { # run_gitops <log prefix> [BENCH_GITOPS value]: half of full, no settle, one tick
  : >"$1.knob"; : >"$1.kubectl"
  local vars=(env -u BENCH_GITOPS PATH="$gs:$PATH" "KUBECONFIG=$lab" "BENCH_BIN=$work/bin-gs-$$-$RANDOM" BENCH_STEPS=0.5 BENCH_TICKS=1 BENCH_SETTLE_SECONDS=0
    "KNOB_LOG=$1.knob" "KUBECTL_LOG=$1.kubectl" "SHA_KWOK=$sha_kwok" "SHA_STAGE=$sha_stage"
    "SHA_ARGO=$(pin ARGOCD_SHA256_application_crd)" "SHA_HELM=$(pin FLUX_SHA256_helm_controller_crds)" "SHA_SRC=$(pin FLUX_SHA256_source_controller_crds)")
  [ $# -lt 2 ] || vars+=("BENCH_GITOPS=$2")
  "${vars[@]}" "$agent" >"$work/out" 2>&1 || true
}
run_gitops "$work/gs-on" 1
if grep -qF -- "--argocd-apps 500 --flux-helmreleases 500" "$work/gs-on.knob" &&
  grep -qF "gitops=1 expect=1000" "$work/gs-on.knob" &&
  grep -q "apply --server-side.*application-crd.*helm-controller.*ocirepositories-crd" "$work/gs-on.kubectl" &&
  grep -q "wait --for=condition=Established crd/applications.argoproj.io crd/helmreleases.helm.toolkit.fluxcd.io crd/ocirepositories.source.toolkit.fluxcd.io" "$work/gs-on.kubectl"; then
  ok "agent.sh: BENCH_GITOPS=1 installs the pinned CRDs, seeds counts scaled by the fill, and tells the benchmark what to expect"
else
  fail "agent.sh: BENCH_GITOPS=1 did not install the CRDs, seed 500 and 500, and expect 1000 charts" "$work/out"
  cat "$work/gs-on.knob" "$work/gs-on.kubectl" >&2
fi
oci=$(ls "$work"/bin-gs-*/flux-ocirepositories-crd-*.yaml | head -1)
if [ "$(grep -c '^kind: CustomResourceDefinition' "$oci")" = 1 ] && grep -qF "ocirepositories.source.toolkit.fluxcd.io" "$oci" && ! grep -qE "buckets|gitrepositories" "$oci"; then
  ok "agent.sh: only the OCIRepository CRD is cut out of source-controller's manifest"
else
  fail "agent.sh: the extracted CRD file is not the OCIRepository CRD alone" "$oci"
fi
run_gitops "$work/gs-off"
if ! grep -qE -- "--argocd-apps|--flux-helmreleases" "$work/gs-off.knob" && ! grep -q "gitops=1" "$work/gs-off.knob" &&
  ! grep -qE "application-crd|helm-controller|ocirepositories" "$work/gs-off.kubectl" && grep -q "gitops=unset expect=unset" "$work/gs-off.knob"; then
  ok "agent.sh: without BENCH_GITOPS no CRD is installed, no seeder flag is passed and the benchmark is told nothing"
else
  fail "agent.sh: the GitOps fill leaked into a run that did not ask for it" "$work/out"
  cat "$work/gs-off.knob" "$work/gs-off.kubectl" >&2
fi

# --- syntax -----------------------------------------------------------------
for f in hack/bench/*.sh; do
  if bash -n "$f" 2>"$work/syntax"; then ok "bash -n $f"; else fail "bash -n $f" "$work/syntax"; fi
done

! grep -q '^FAIL' "$work/results" || exit 1
echo "bench_test: OK ($(grep -c '^ok' "$work/results") checks)"
cd "$repo"
