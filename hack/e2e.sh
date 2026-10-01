#!/usr/bin/env bash
# kind end-to-end on one Kubernetes minor (make e2e E2E_MINOR=1.31; CI's
# kube job runs it once per minor of hack/kind-node-images.txt):
#
#   1. a kind cluster on that minor's digest-pinned node image, with kind and
#      kubectl from hack/install-tool.sh (pinned, sha256-verified);
#   2. regression (#3): the vanilla cluster, scanned at its next minor, has
#      zero removed-api blockers — a fresh cluster cannot be blocked by APIs
#      nobody uses (skipped, with a warning, when an existing cluster is
#      reused: it is no longer vanilla);
#   3. the EOL ingress-nginx demo add-on (hack/demo/kind-setup.sh) and the
#      scan + agent integration tests (UPGRADESCOPE_IT=1);
#   4. the image built from this tree, kind-loaded, and the chart installed
#      from deploy/chart with the server enabled, --wait;
#   5. the agent's ClusterReadiness gets a score and a verdict for the next
#      minor (blocked, with the ingress-nginx eol-addon finding);
#   6. the server ingested the agent's snapshot (GET /api/v1/clusters);
#   7. `helm upgrade --set agent.targets={<next minor>}` succeeds and the CR
#      spec follows (#41);
#   8. `helm uninstall` leaves no ClusterRole/ClusterRoleBinding and no
#      namespaced release object behind, and keeps the CRD, as the chart
#      README documents (Helm never deletes crds/).
#
# Steps marked best-effort below guard bugs that are not fixed on this
# branch's base yet: they report PASS/FAIL in the job summary
# ($GITHUB_STEP_SUMMARY) and as a warning, but do not fail the run. Every
# other step gates.
#
# Safety: every kubectl/helm call names the kind context explicitly
# (kind-upgradescope-demo); nothing here reads the current context.
# hack/demo/kind-setup.sh does switch the current context to the kind
# cluster, as `make demo-up` always has. The cluster is left running for
# inspection; `make demo-down` deletes it.
#
# Needs Docker, helm, go, jq and curl; installs kind and kubectl itself.
#
# Knobs: E2E_MINOR (default 1.37). For hack/e2e_test.sh, which runs this
# against stubs: E2E_INSTALL_TOOL, E2E_UPGRADESCOPE (the binary `make build`
# produces), E2E_NAP (seconds between polls, overriding each poll's own).
set -euo pipefail
cd "$(dirname "$0")/.."

MINOR=${E2E_MINOR:-1.37}
INSTALL_TOOL=${E2E_INSTALL_TOOL:-hack/install-tool.sh}
UPGRADESCOPE=${E2E_UPGRADESCOPE:-bin/upgradescope}
CLUSTER=upgradescope-demo # the cluster kind-setup.sh and the ITs use
CTX=kind-$CLUSTER
NS=upgradescope
RELEASE=upgradescope
IMAGE=ghcr.io/abd-ulbasit/upgradescope
TAG=e2e
TOKEN=e2e-token
CR=cluster

NODE_IMAGE=$(hack/kind-images.sh image "$MINOR")
NEXT=$(hack/kind-images.sh next "$MINOR")

for tool in docker helm go jq curl; do
  command -v "$tool" >/dev/null || { echo "ERROR: $tool not found in PATH" >&2; exit 1; }
done
kind=$("$INSTALL_TOOL" kind)
kubectl=$("$INSTALL_TOOL" kubectl)
PATH="$(dirname "$kind"):$(dirname "$kubectl"):$PATH"
export PATH

k() { kubectl --context "$CTX" "$@"; }
h() { helm --kube-context "$CTX" "$@"; }
nap() { sleep "${E2E_NAP:-$1}"; }

work=$(mktemp -d)
pf_pid=""
results=()

# summary: the outcome of every step, in the job summary and the log.
summary() {
  local out=${GITHUB_STEP_SUMMARY:-/dev/null}
  {
    echo "## kind e2e — Kubernetes $MINOR ($NODE_IMAGE)"
    echo ""
    printf '%s\n' ${results[@]+"${results[@]}"}
    echo ""
  } | tee -a "$out"
}
cleanup() {
  [ -z "$pf_pid" ] || kill "$pf_pid" 2>/dev/null || true
  rm -rf "$work"
  summary
}
trap cleanup EXIT

# gate <name> <cmd...>: run it; a failure fails the run.
gate() {
  local name=$1
  shift
  echo "== $name"
  if "$@"; then
    results+=("- PASS — $name")
  else
    results+=("- **FAIL** — $name")
    echo "::error title=kind e2e $MINOR::$name failed"
    exit 1
  fi
}
# best_effort <issue> <name> <cmd...>: run it; a failure is reported, not fatal.
best_effort() {
  local issue=$1 name=$2
  shift 2
  echo "== $name (best-effort until $issue is fixed)"
  if "$@"; then
    results+=("- PASS — $name (best-effort, $issue)")
  else
    results+=("- **FAIL** — $name (best-effort until $issue lands; not gating)")
    echo "::warning title=kind e2e $MINOR: best-effort check failed::$name — tracked by $issue; not gating, but not passing either"
  fi
}

# skip <name> <why>: a check that cannot mean anything on this run.
skip() {
  results+=("- SKIP — $1 ($2)")
  echo "::warning title=kind e2e $MINOR: check skipped::$1 — $2"
}

reused=""
create_cluster() {
  if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
    echo "kind cluster '$CLUSTER' already exists, reusing it"
    reused=1
  else
    kind create cluster --name "$CLUSTER" --image "$NODE_IMAGE" --wait 120s || return 1
  fi
  local got
  got=$(k get --raw /version | jq -r '.major + "." + (.minor | rtrimstr("+"))')
  [ "$got" = "$MINOR" ] || { echo "cluster runs Kubernetes $got, want $MINOR (delete the old cluster: make demo-down)" >&2; return 1; }
}

# TODO(#3): make this gating once the api-usage collector stops counting
# objects served at a deprecated version as removed-API users.
no_removed_api_blockers() {
  "$UPGRADESCOPE" scan --context "$CTX" --target "$NEXT" --output json --fail-on never >"$work/vanilla.json" ||
    { echo "scan failed" >&2; return 1; }
  local n
  n=$(jq '[.findings[] | select(.category == "removed-api" and .severity == "blocker")] | length' "$work/vanilla.json") ||
    return 1
  if [ "$n" != 0 ]; then
    echo "a vanilla $MINOR cluster has $n removed-api blocker(s) at target $NEXT:" >&2
    jq -r '.findings[] | select(.category == "removed-api" and .severity == "blocker") | "  - \(.title)"' "$work/vanilla.json" >&2
    return 1
  fi
}

integration_tests() {
  UPGRADESCOPE_IT=1 UPGRADESCOPE_IT_CONTEXT="$CTX" go test ./internal/cli/ -run Integration -count=1 -v
}

load_image() {
  docker build --build-arg VERSION="$TAG" -t "$IMAGE:$TAG" . || return 1
  # `kind load docker-image` fails with "content digest not found" when the
  # daemon (containerd image store) exports a manifest list whose
  # other-platform blobs are absent; a single-platform archive sidesteps it.
  local plat
  plat=$(docker version --format '{{.Server.Os}}/{{.Server.Arch}}') || return 1
  docker save --platform "$plat" "$IMAGE:$TAG" -o "$work/image.tar" 2>/dev/null ||
    docker save "$IMAGE:$TAG" -o "$work/image.tar" || return 1
  kind load image-archive "$work/image.tar" --name "$CLUSTER"
}

install_chart() {
  # interval=1m so a second tick comes quickly; the first fires at start.
  h upgrade --install "$RELEASE" deploy/chart --namespace "$NS" --create-namespace \
    --set image.tag="$TAG" \
    --set server.enabled=true --set server.ingestToken="$TOKEN" \
    --set agent.interval=1m \
    --wait --timeout 5m
}

cr_has_verdict() {
  local score="" i
  for i in $(seq 1 36); do
    score=$(k get clusterreadiness "$CR" -o jsonpath='{.status.targets[0].score}' 2>/dev/null || true)
    [ -n "$score" ] && break
    nap 5
  done
  if [ -z "$score" ]; then
    k -n "$NS" logs "deploy/$RELEASE-agent" --tail=50 >&2 || true
    echo "no status.targets[0].score on clusterreadiness/$CR after 180s" >&2
    return 1
  fi
  k get clusterreadiness "$CR" -o json >"$work/cr.json" || return 1
  jq '.status.targets[0] | {target, score, verdict, ready, blockers}' "$work/cr.json"
  # Default target (no spec.targets): the next minor above the server.
  jq -e --arg t "$NEXT" '.status.targets[0].target == $t' "$work/cr.json" >/dev/null ||
    { echo "status.targets[0].target is not $NEXT" >&2; return 1; }
  # The EOL ingress-nginx add-on blocks the upgrade whatever else is found.
  jq -e '.status.targets[0].verdict == "blocked" and .status.targets[0].ready == false' "$work/cr.json" >/dev/null ||
    { echo "verdict is not blocked (the EOL ingress-nginx add-on is installed)" >&2; return 1; }
  grep -q '"eol-addon"' "$work/cr.json" || { echo "no eol-addon finding in the CR status" >&2; return 1; }
}

server_ingested() {
  k -n "$NS" port-forward "svc/$RELEASE-server" 18080:8080 >"$work/pf.log" 2>&1 &
  pf_pid=$!
  local body="" i
  for i in $(seq 1 24); do
    body=$(curl -fsS http://127.0.0.1:18080/api/v1/clusters 2>/dev/null || true)
    if grep -q '"name"' <<<"$body" && grep -q '"score"' <<<"$body"; then break; fi
    nap 5
  done
  echo "GET /api/v1/clusters: $body"
  grep -q '"name"' <<<"$body" && grep -q '"score"' <<<"$body" ||
    { echo "the server lists no scored cluster after 120s (agent push failing?)" >&2; return 1; }
  curl -fsS http://127.0.0.1:18080/healthz | grep -q '"ok"' || { echo "/healthz is not ok" >&2; return 1; }
  kill "$pf_pid" 2>/dev/null || true
  pf_pid=""
}

# TODO(#41): make this gating once the chart owns the ClusterReadiness it
# renders for agent.targets (today the agent-created CR blocks the upgrade).
upgrade_with_targets() {
  # --set-string: `--set agent.targets={1.30}` would render the float 1.3.
  h upgrade "$RELEASE" deploy/chart --namespace "$NS" --reuse-values \
    --set-string "agent.targets={$NEXT}" --wait --timeout 5m || return 1
  k get clusterreadiness "$CR" -o json | jq -e --arg t "$NEXT" '.spec.targets | index($t) != null' >/dev/null ||
    { echo "clusterreadiness/$CR spec.targets does not contain $NEXT after the upgrade" >&2; return 1; }
}

uninstall_leaves_nothing() {
  h uninstall "$RELEASE" --namespace "$NS" --wait --timeout 5m || return 1
  local sel="app.kubernetes.io/instance=$RELEASE" left="" i
  for i in $(seq 1 30); do
    # A failed list must not read as "nothing left".
    left=$(k get clusterrole,clusterrolebinding -l "$sel" -o name &&
      k -n "$NS" get deploy,svc,secret,pvc,serviceaccount -l "$sel" -o name) ||
      { echo "could not list what the release left behind" >&2; return 1; }
    [ -z "$left" ] && break
    nap 2
  done
  [ -z "$left" ] || { echo "left behind after helm uninstall:" >&2; echo "$left" >&2; return 1; }
  # Named, too, in case a label ever drifts from the release.
  ! k get clusterrole "$RELEASE-agent" >/dev/null 2>&1 || { echo "clusterrole/$RELEASE-agent left behind" >&2; return 1; }
  ! k get clusterrolebinding "$RELEASE-agent" >/dev/null 2>&1 || { echo "clusterrolebinding/$RELEASE-agent left behind" >&2; return 1; }
  # deploy/chart/README.md: Helm keeps crds/ on uninstall, by design.
  k get crd clusterreadinesses.upgradescope.dev >/dev/null ||
    { echo "the ClusterReadiness CRD is gone; the chart README says uninstall keeps it" >&2; return 1; }
}

gate "kind cluster on Kubernetes $MINOR" create_cluster
gate "build bin/upgradescope" make build
# A reused cluster (a local re-run) already has the demo add-on, the CRD and
# whatever the last run left, so it is not the vanilla cluster this guards.
vanilla="vanilla $MINOR cluster scanned at $NEXT has zero removed-api blockers"
if [ -n "$reused" ]; then
  skip "$vanilla" "cluster reused, not vanilla; make demo-down first"
else
  best_effort "#3" "$vanilla" no_removed_api_blockers
fi
gate "EOL ingress-nginx demo add-on" env KIND_NODE_IMAGE="$NODE_IMAGE" hack/demo/kind-setup.sh
gate "scan + agent integration tests" integration_tests
gate "image built from this tree, kind-loaded" load_image
gate "helm install deploy/chart (server enabled) --wait" install_chart
gate "ClusterReadiness has a score and a blocked verdict for $NEXT" cr_has_verdict
gate "server ingested the agent's snapshot" server_ingested
best_effort "#41" "helm upgrade --set agent.targets={$NEXT}" upgrade_with_targets
gate "helm uninstall leaves no ClusterRole/ClusterRoleBinding or release object; CRD kept" uninstall_leaves_nothing
echo "e2e $MINOR: OK"
