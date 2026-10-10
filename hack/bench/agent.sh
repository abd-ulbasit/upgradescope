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
# Opt-in GitOps fill (BENCH_GITOPS=1, #233): also installs the upstream Argo
# CD Application CRD and the Flux HelmRelease and OCIRepository CRDs (pinned
# by version and sha256, into the lab only), and fills it with Argo CD
# Applications (single- and multi-source) and Flux HelmReleases (chart
# sources and chartRefs to OCIRepositories) at each level, so the report also
# shows what the agent's GitOps lists cost a tick. Off by default: nothing
# of it runs, and the numbers are those of a cluster without the tools.
#
# Knobs (environment):
#   BENCH_STEPS      fill levels as fractions of the full seed, default "0 0.25 0.5 1"
#                    (0 is the vanilla cluster; 1 is 2000 nodes, 10000 pods,
#                    6000 ConfigMaps, 4000 Deployments, 1000 Helm releases)
#   BENCH_TICKS      ticks measured per level, default 5
#   BENCH_POD_PASS_EVERY  the agent's --pod-pass-every for the measured ticks
#                    (default: the agent's own, 3; 1 lists every pod every tick,
#                    as before #228). With more than 1, the report tells the
#                    ticks that listed every pod from those that reused the
#                    last pass: run enough ticks for both after the first
#                    (BENCH_TICKS=8 gives two of the one and five of the other
#                    at 3).
#   BENCH_GITOPS     1 adds the GitOps fill above; unset (or anything else is
#                    refused) leaves it out
#   BENCH_GITOPS_APPS, BENCH_GITOPS_HELMRELEASES  Argo CD Applications and Flux
#                    HelmReleases at full size (default 1000 each; the
#                    levels scale them like everything else)
#   BENCH_SETTLE_SECONDS  how long to let KWOK's heartbeats settle after each
#                    fill step, default 30
#   BENCH_HELM_REVISIONS  stored revisions per Helm release, default 1
#   BENCH_BIN        where tools and results go, default bin/bench (gitignored)
#   BENCH_RESET_CMD  a command run before and after (a cluster-recreate script)
#   BENCH_NO_HELM_CACHE  1 measures with no Helm release cache, as every
#                    tick did before #71 (the "before" rows of
#                    docs/operations/scale.md); unset keeps it, anything else
#                    is refused
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
# The script's own directory, absolute, before anything changes directory: it
# finds the report script and the repository root from wherever it is run, and
# a relative KUBECONFIG means a file relative to the caller's directory.
script_dir=$(cd "$(dirname "$0")" && pwd -P)
if [ -n "${KUBECONFIG:-}" ]; then
  case "$KUBECONFIG" in /* | *:*) ;; *) KUBECONFIG=$PWD/$KUBECONFIG ;; esac
fi
cd "$script_dir/../.."

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
BENCH_SETTLE_SECONDS=${BENCH_SETTLE_SECONDS:-30}
BENCH_GITOPS_APPS=${BENCH_GITOPS_APPS:-1000}
BENCH_GITOPS_HELMRELEASES=${BENCH_GITOPS_HELMRELEASES:-1000}
case "${BENCH_GITOPS:-}" in "" | 1) ;; *) die "BENCH_GITOPS=$BENCH_GITOPS: set it to 1 or leave it unset" ;; esac
for v in BENCH_SETTLE_SECONDS BENCH_GITOPS_APPS BENCH_GITOPS_HELMRELEASES; do
  case "${!v}" in "" | *[!0-9]*) die "$v=${!v}: set it to a whole number" ;; esac
done
# The benchmark itself refuses anything but 1, but only after a build and a
# fill of the lab; refuse here, before either.
case "${BENCH_POD_PASS_EVERY:-}" in "" | [1-9] | [1-9][0-9]*) ;; *) die "BENCH_POD_PASS_EVERY=$BENCH_POD_PASS_EVERY: set it to a whole number of at least 1, or leave it unset" ;; esac
case "${BENCH_NO_HELM_CACHE:-}" in "" | 1) ;; *) die "BENCH_NO_HELM_CACHE=$BENCH_NO_HELM_CACHE: set it to 1 or leave it unset" ;; esac
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

# fetch_url <file name in BENCH_BIN> <url> <sha256>: a download that must match
# its pinned digest, kept (and verified again) for the next run.
fetch_url() {
  local f=$BENCH_BIN/$1
  if [ ! -f "$f" ] || [ "$(sha256_of "$f")" != "$3" ]; then
    curl -fsSL -o "$f.tmp" "$2" || die "download of $2 failed"
    [ "$(sha256_of "$f.tmp")" = "$3" ] || { rm -f "$f.tmp"; die "$2 does not match its pinned sha256"; }
    mv "$f.tmp" "$f"
  fi
  echo "$f"
}

fetch_pinned() { # fetch_pinned <asset> <sha256>
  fetch_url "kwok-$KWOK_VERSION-$1" "https://github.com/kubernetes-sigs/kwok/releases/download/$KWOK_VERSION/$1" "$2"
}

# --- the GitOps CRDs (BENCH_GITOPS=1), upstream manifests pinned the same way.
# Only the CRDs the agent's GitOps collector reads are installed: Argo CD's
# Application, Flux's HelmRelease and OCIRepository. The source-controller
# manifest holds six CRDs; the one wanted is cut out of it after the whole
# file matched its digest.
ARGOCD_VERSION=v3.5.3
ARGOCD_SHA256_application_crd=5dde0e229249b6b707beb98674c1deae3949d5c319a6c45b9f5a80c99618e40c
FLUX_HELM_CONTROLLER_VERSION=v1.6.5
FLUX_SHA256_helm_controller_crds=8af19966e63cccde7d2c62e24c7bd3466010dd53e9d063e2ce22d1391e11f272
FLUX_SOURCE_CONTROLLER_VERSION=v1.9.6
FLUX_SHA256_source_controller_crds=5acf3b01c24f15647a162f2d43b1287f0d0156fab0026f4afb9865874cdbe16d

# extract_crd <manifest> <crd name>: the one document of a multi-document
# manifest whose metadata.name is <crd name> (documents are separated by a
# line holding only ---).
extract_crd() {
  awk -v want="  name: $2" '
    /^---$/ { if (hit) { done = 1; exit } doc = ""; next }
    { doc = doc $0 "\n"; if ($0 == want) hit = 1 }
    END { if (hit) printf "%s", doc }' "$1"
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

# measure <label> <expected nodes> <expected helm releases> <expected GitOps
# charts>: the tick benchmark at the cluster's current fill, appending one JSON
# line per tick to $results. The GitOps count is checked only with BENCH_GITOPS.
measure() {
  local vars=(UPGRADESCOPE_BENCH_TICKS="$BENCH_TICKS" UPGRADESCOPE_BENCH_LABEL="$1"
    UPGRADESCOPE_BENCH_EXPECT_NODES="$2" UPGRADESCOPE_BENCH_EXPECT_HELM="$3")
  [ -z "${BENCH_GITOPS:-}" ] || vars+=(UPGRADESCOPE_BENCH_GITOPS=1 UPGRADESCOPE_BENCH_EXPECT_GITOPS="$4")
  [ -z "${BENCH_POD_PASS_EVERY:-}" ] || vars+=(UPGRADESCOPE_BENCH_POD_PASS_EVERY="$BENCH_POD_PASS_EVERY")
  [ -z "${BENCH_NO_HELM_CACHE:-}" ] || vars+=(UPGRADESCOPE_BENCH_NO_HELM_CACHE="$BENCH_NO_HELM_CACHE")
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

if [ -n "${BENCH_GITOPS:-}" ]; then
  argo_crd=$(fetch_url "argocd-$ARGOCD_VERSION-application-crd.yaml" "https://raw.githubusercontent.com/argoproj/argo-cd/$ARGOCD_VERSION/manifests/crds/application-crd.yaml" "$ARGOCD_SHA256_application_crd")
  helm_crds=$(fetch_url "flux-helm-controller-$FLUX_HELM_CONTROLLER_VERSION-crds.yaml" "https://github.com/fluxcd/helm-controller/releases/download/$FLUX_HELM_CONTROLLER_VERSION/helm-controller.crds.yaml" "$FLUX_SHA256_helm_controller_crds")
  source_crds=$(fetch_url "flux-source-controller-$FLUX_SOURCE_CONTROLLER_VERSION-crds.yaml" "https://github.com/fluxcd/source-controller/releases/download/$FLUX_SOURCE_CONTROLLER_VERSION/source-controller.crds.yaml" "$FLUX_SHA256_source_controller_crds")
  oci_crd=$BENCH_BIN/flux-ocirepositories-crd-$FLUX_SOURCE_CONTROLLER_VERSION.yaml
  extract_crd "$source_crds" ocirepositories.source.toolkit.fluxcd.io >"$oci_crd"
  [ -s "$oci_crd" ] || die "no ocirepositories CRD in the source-controller manifest"
  echo "bench-agent: installing the Argo CD $ARGOCD_VERSION Application CRD and the Flux HelmRelease ($FLUX_HELM_CONTROLLER_VERSION) and OCIRepository ($FLUX_SOURCE_CONTROLLER_VERSION) CRDs in the lab" >&2
  # Server-side: the Application CRD is too large for client-side apply's annotation.
  kc apply --server-side --force-conflicts -f "$argo_crd" -f "$helm_crds" -f "$oci_crd" >&2
  kc wait --for=condition=Established crd/applications.argoproj.io crd/helmreleases.helm.toolkit.fluxcd.io crd/ocirepositories.source.toolkit.fluxcd.io --timeout=120s >&2
fi

stamp=$(date -u +%Y%m%dT%H%M%SZ)
results=$BENCH_BIN/agent-$stamp.jsonl
: >"$results"
steps_file=$BENCH_BIN/agent-$stamp.steps.jsonl
: >"$steps_file"

failed=""
full_nodes=2000 full_pods=10000 full_cms=6000 full_deps=4000 full_helm=1000 full_ns=100
for f in $BENCH_STEPS; do
  nodes=$(scale $full_nodes "$f") pods=$(scale $full_pods "$f") cms=$(scale $full_cms "$f")
  deps=$(scale $full_deps "$f") helm=$(scale $full_helm "$f")
  apps=0 hrs=0
  if [ -n "${BENCH_GITOPS:-}" ]; then apps=$(scale "$BENCH_GITOPS_APPS" "$f") hrs=$(scale "$BENCH_GITOPS_HELMRELEASES" "$f"); fi
  nss=$full_ns # constant: an object is placed by its number modulo this, so a step adds only the difference
  if [ "$f" != 0 ]; then
    echo "bench-agent: seeding to $f of full: $nodes nodes, $pods pods, $cms ConfigMaps, $deps Deployments, $helm Helm releases ($BENCH_HELM_REVISIONS revision each)${BENCH_GITOPS:+, $apps Argo CD Applications, $hrs Flux HelmReleases}" >&2
    gitops_flags=()
    [ -z "${BENCH_GITOPS:-}" ] || gitops_flags=(--argocd-apps "$apps" --flux-helmreleases "$hrs")
    "$BENCH_BIN/bench-seed" --kubeconfig "$KUBECONFIG" --nodes "$nodes" --pods "$pods" --configmaps "$cms" \
      --deployments "$deps" --helm-releases "$helm" --helm-revisions "$BENCH_HELM_REVISIONS" --namespaces "$nss" \
      ${gitops_flags[@]+"${gitops_flags[@]}"} >>"$steps_file"
    echo "bench-agent: letting KWOK heartbeats settle" >&2
    sleep "$BENCH_SETTLE_SECONDS"
  fi
  if [ -n "${BENCH_CP_STATS_CMD:-}" ]; then
    echo "bench-agent: control plane at $f: $(bash -c "$BENCH_CP_STATS_CMD" 2>&1 | tr '\n' ' ')" >&2
  fi
  echo "bench-agent: measuring $BENCH_TICKS ticks at fill $f" >&2
  measure "fill=$f nodes=$nodes pods=$pods configmaps=$cms deployments=$deps helm=$helm${BENCH_GITOPS:+ argocd=$apps flux=$hrs}" \
    $((nodes + $(kc get nodes -o json | jq '[.items[] | select(.metadata.annotations["kwok.x-k8s.io/node"] != "fake")] | length'))) "$helm" "$((apps + hrs))" || failed=1
done

"$script_dir/agent-report.sh" "$results"
echo "bench-agent: raw per-tick results: $results" >&2
reset_hook
[ -z "$failed" ] || die "a measurement failed (ticks with an error or an incomplete capability are not valid numbers): see the output above"
