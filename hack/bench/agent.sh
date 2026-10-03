#!/usr/bin/env bash
# agent.sh — what one agent tick costs a kube-apiserver on a large cluster
# (#71; docs/operations/scale.md). It fills a lab cluster with KWOK fake
# nodes, pods, ConfigMaps, Deployments and Helm release Secrets in steps, and
# at each fill level runs the agent's real tick (collect, evaluate, write the
# ClusterReadiness status) several times against it, built from this checkout,
# reporting per tick the API requests by verb and resource, the bytes
# transferred, the wall time, and the agent's live heap and peak RSS.
#
#   KUBECONFIG=/path/to/lab-kubeconfig hack/bench/agent.sh
#   make bench-agent KUBECONFIG=/path/to/lab-kubeconfig
#
# KUBECONFIG is required and is the only kubeconfig read: the default one
# (~/.kube/config) and the current context are never used, so a benchmark
# cannot reach a cluster you did not name. The cluster must be disposable: it
# gets about 20k objects, 2k nodes and 1k Secrets, and the KWOK controller
# in kube-system. The script refuses a cluster that already has workloads
# (more than 3 real nodes, or a namespace beyond the kind defaults), and never
# deletes anything: reset the cluster by recreating it (BENCH_RESET_CMD runs
# before and after if set, e.g. your cluster-recreate script).
#
# Knobs (environment):
#   BENCH_STEPS      fill levels as fractions of the full seed, default "0 0.25 0.5 1"
#                    (0 is the vanilla cluster; 1 is 2000 nodes, 10000 pods,
#                    6000 ConfigMaps, 4000 Deployments, 1000 Helm releases)
#   BENCH_TICKS      ticks measured per level, default 5
#   BENCH_HELM_REVISIONS  stored revisions per Helm release, default 1
#   BENCH_BIN        where tools and results go, default bin/bench (gitignored)
#   BENCH_RESET_CMD  a command run before and after (a cluster-recreate script)
#   BENCH_RUN_ON     an ssh host to run the measured agent tick on (default: this
#                    machine). Set it to the lab's host: the benchmark binary is
#                    cross-compiled for linux/amd64 and copied there with the
#                    kubeconfig (a private temp directory, removed at the end),
#                    so the tick sees the apiserver's LAN latency instead of
#                    this machine's. The seeding always runs here.
#   BENCH_CP_STATS_CMD  a command that prints the control plane's memory (run
#                    after each level and shown in the table; e.g. over ssh
#                    to the kind host: docker stats --no-stream)
# Needs: go, kubectl, jq, curl, shasum or sha256sum, and a cluster that can
# pull registry.k8s.io/kwok/kwok.
set -euo pipefail
cd "$(dirname "$0")/../.."

die() { echo "bench-agent: $*" >&2; exit 1; }

# --- the one cluster this may touch -------------------------------------
[ -n "${KUBECONFIG:-}" ] || die "KUBECONFIG is required: the kubeconfig of a disposable lab cluster (the default kubeconfig is never used)"
case "$KUBECONFIG" in *:*) die "KUBECONFIG must name a single file, not a list" ;; esac
[ -f "$KUBECONFIG" ] || die "KUBECONFIG=$KUBECONFIG is not a file"
[ "$(cd "$(dirname "$KUBECONFIG")" && pwd -P)/$(basename "$KUBECONFIG")" != "$(cd "$HOME/.kube" 2>/dev/null && pwd -P)/config" ] ||
  die "KUBECONFIG is ~/.kube/config; use the lab cluster's own kubeconfig file"
export KUBECONFIG
kc() { kubectl --kubeconfig "$KUBECONFIG" "$@"; }

BENCH_BIN=${BENCH_BIN:-bin/bench}
BENCH_STEPS=${BENCH_STEPS:-0 0.25 0.5 1}
BENCH_TICKS=${BENCH_TICKS:-5}
BENCH_HELM_REVISIONS=${BENCH_HELM_REVISIONS:-1}
BENCH_RUN_ON=${BENCH_RUN_ON:-local}
mkdir -p "$BENCH_BIN"
BENCH_BIN=$(cd "$BENCH_BIN" && pwd -P)

# --- KWOK, pinned. Only the controller's manifests are needed: the fake nodes
# are created by the seeder, so kwok and kwokctl themselves are not installed.
KWOK_VERSION=v0.8.0
KWOK_SHA256_kwok_yaml=a4c16e6431e382dcb5c1903139344b7a68652f16a6460337fe17a678a426f405
KWOK_SHA256_stage_fast_yaml=2f28d95564ec43056c0873f7a25ac7d2a5bba4c8496c72f8b3ee73fd4f54ee24

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

fetch_pinned() { # fetch_pinned <asset> <sha256>
  local f=$BENCH_BIN/kwok-$KWOK_VERSION-$1
  if [ ! -f "$f" ] || [ "$(sha256_of "$f")" != "$2" ]; then
    curl -fsSL -o "$f.tmp" "https://github.com/kubernetes-sigs/kwok/releases/download/$KWOK_VERSION/$1" || die "download of kwok $1 failed"
    [ "$(sha256_of "$f.tmp")" = "$2" ] || { rm -f "$f.tmp"; die "kwok $1 does not match its pinned sha256"; }
    mv "$f.tmp" "$f"
  fi
  echo "$f"
}

# --- refuse a cluster that is not a lab ---------------------------------
guard_cluster() {
  local server real extra
  server=$(kc config view --minify -o jsonpath='{.clusters[0].cluster.server}')
  echo "bench-agent: cluster $server (context $(kc config current-context))" >&2
  real=$(kc get nodes -o json | jq '[.items[] | select(.metadata.annotations["kwok.x-k8s.io/node"] != "fake")] | length')
  [ "$real" -le 3 ] || die "$server has $real real nodes; the benchmark fills a cluster and wants a disposable one (at most 3 real nodes)"
  extra=$(kc get ns -o json | jq -r '[.items[].metadata.name | select((. | IN("default","kube-system","kube-public","kube-node-lease","local-path-storage") or startswith("bench-ns-")) | not)] | join(" ")')
  [ -z "$extra" ] || die "$server has namespaces beyond a vanilla kind cluster's ($extra); use a disposable cluster"
}

reset_hook() {
  [ -z "${BENCH_RESET_CMD:-}" ] || { echo "bench-agent: reset: $BENCH_RESET_CMD" >&2; bash -c "$BENCH_RESET_CMD" >&2; }
}

scale() { # scale <full-count> <fraction> -> rounded count
  awk -v n="$1" -v f="$2" 'BEGIN { printf "%d", n * f + 0.5 }'
}

remote_dir=""
cleanup() {
  if [ -n "$remote_dir" ]; then
    # shellcheck disable=SC2029 # expanded here on purpose
    ssh "$BENCH_RUN_ON" "rm -rf '$remote_dir'" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

reset_hook
guard_cluster

echo "bench-agent: building the seeder and the agent benchmark from $(git rev-parse --short HEAD) ($(git rev-parse --abbrev-ref HEAD))" >&2
go build -o "$BENCH_BIN/bench-seed" ./hack/bench/seed
if [ "$BENCH_RUN_ON" = local ]; then
  go test -c -o "$BENCH_BIN/agent-bench.test" ./internal/agent
else
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -o "$BENCH_BIN/agent-bench-linux-amd64.test" ./internal/agent
  remote_dir=$(ssh "$BENCH_RUN_ON" 'umask 077; mktemp -d /tmp/upgradescope-bench.XXXXXX')
  scp -q "$BENCH_BIN/agent-bench-linux-amd64.test" "$BENCH_RUN_ON:$remote_dir/agent.test"
  scp -q "$KUBECONFIG" "$BENCH_RUN_ON:$remote_dir/kubeconfig"
  echo "bench-agent: the measured ticks run on $BENCH_RUN_ON, next to the apiserver" >&2
fi

# measure <label> <expected nodes> <expected helm releases>: the tick
# benchmark at the cluster's current fill, appending one JSON line per tick
# to $results.
measure() {
  local vars=(UPGRADESCOPE_BENCH_TICKS="$BENCH_TICKS" UPGRADESCOPE_BENCH_LABEL="$1"
    UPGRADESCOPE_BENCH_EXPECT_NODES="$2" UPGRADESCOPE_BENCH_EXPECT_HELM="$3")
  if [ "$BENCH_RUN_ON" = local ]; then
    env "${vars[@]}" UPGRADESCOPE_BENCH_KUBECONFIG="$KUBECONFIG" UPGRADESCOPE_BENCH_OUT="$results" \
      "$BENCH_BIN/agent-bench.test" -test.run '^TestBenchAgentTick$' -test.v -test.timeout 30m >&2
  else
    local remote_out=$remote_dir/out.jsonl
    # shellcheck disable=SC2029 # $remote_dir is expanded here on purpose
    {
      for kv in "${vars[@]}"; do printf 'export %s\n' "$(printf '%q' "$kv")"; done
    } | ssh "$BENCH_RUN_ON" "umask 077; cat > '$remote_dir/env'"
    # shellcheck disable=SC2029
    ssh "$BENCH_RUN_ON" "cd '$remote_dir' && . ./env && rm -f out.jsonl && UPGRADESCOPE_BENCH_KUBECONFIG='$remote_dir/kubeconfig' UPGRADESCOPE_BENCH_OUT='$remote_out' ./agent.test -test.run '^TestBenchAgentTick\$' -test.v -test.timeout 30m" >&2 || {
      # shellcheck disable=SC2029
      ssh "$BENCH_RUN_ON" "cat '$remote_out' 2>/dev/null" >>"$results" || true
      return 1
    }
    # shellcheck disable=SC2029
    ssh "$BENCH_RUN_ON" "cat '$remote_out'" >>"$results"
  fi
}

kwok=$(fetch_pinned kwok.yaml "$KWOK_SHA256_kwok_yaml")
stage=$(fetch_pinned stage-fast.yaml "$KWOK_SHA256_stage_fast_yaml")
echo "bench-agent: installing the KWOK $KWOK_VERSION controller in the lab (manages only nodes annotated kwok.x-k8s.io/node=fake)" >&2
kc apply --server-side --force-conflicts -f "$kwok" >&2
kc -n kube-system rollout status deploy/kwok-controller --timeout=300s >&2
# The Stage CRs need the CRDs established first.
kc wait --for=condition=Established crd/stages.kwok.x-k8s.io --timeout=60s >&2
kc apply --server-side --force-conflicts -f "$stage" >&2

stamp=$(date -u +%Y%m%dT%H%M%SZ)
results=$BENCH_BIN/agent-$stamp.jsonl
: >"$results"
steps_file=$BENCH_BIN/agent-$stamp.steps.jsonl
: >"$steps_file"

failed=""
full_nodes=2000 full_pods=10000 full_cms=6000 full_deps=4000 full_helm=1000 full_ns=100
for f in $BENCH_STEPS; do
  nodes=$(scale $full_nodes "$f") pods=$(scale $full_pods "$f") cms=$(scale $full_cms "$f")
  deps=$(scale $full_deps "$f") helm=$(scale $full_helm "$f") nss=$(scale $full_ns "$f")
  if [ "$f" != 0 ]; then
    echo "bench-agent: seeding to $f of full: $nodes nodes, $pods pods, $cms ConfigMaps, $deps Deployments, $helm Helm releases ($BENCH_HELM_REVISIONS revision each)" >&2
    "$BENCH_BIN/bench-seed" --kubeconfig "$KUBECONFIG" --nodes "$nodes" --pods "$pods" --configmaps "$cms" \
      --deployments "$deps" --helm-releases "$helm" --helm-revisions "$BENCH_HELM_REVISIONS" --namespaces "$nss" >>"$steps_file"
    echo "bench-agent: letting KWOK heartbeats settle" >&2
    sleep 30
  fi
  if [ -n "${BENCH_CP_STATS_CMD:-}" ]; then
    echo "bench-agent: control plane at $f: $(bash -c "$BENCH_CP_STATS_CMD" 2>&1 | tr '\n' ' ')" >&2
  fi
  echo "bench-agent: measuring $BENCH_TICKS ticks at fill $f" >&2
  measure "fill=$f nodes=$nodes pods=$pods configmaps=$cms deployments=$deps helm=$helm" \
    $((nodes + $(kc get nodes -o json | jq '[.items[] | select(.metadata.annotations["kwok.x-k8s.io/node"] != "fake")] | length'))) "$helm" || failed=1
done

"$(dirname "$0")/agent-report.sh" "$results"
echo "bench-agent: raw per-tick results: $results" >&2
reset_hook
[ -z "$failed" ] || die "a measurement failed (ticks with an error or an incomplete capability are not valid numbers): see the output above"
