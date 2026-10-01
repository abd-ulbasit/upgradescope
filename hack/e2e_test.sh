#!/usr/bin/env bash
# Tests for hack/e2e.sh's control flow, offline and without a cluster: every
# external command it runs (kind, kubectl, helm, docker, go, make, curl, the
# upgradescope binary, the tool installer) is a stub that logs its arguments
# and answers from canned data. Checks which steps gate and which only
# report, what lands in the job summary, and that every kubectl/helm call on
# the release names the kind context explicitly. Needs bash and jq.
set -euo pipefail
cd "$(dirname "$0")/.."

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
stubs="$work/stubs"
mkdir -p "$stubs"

# stub <name> <body>: an executable that logs "<name> <args>" then runs body.
stub() {
  printf '#!/usr/bin/env bash\necho "%s $*" >>"$STUB_LOG"\n%s\n' "$1" "$2" >"$stubs/$1"
  chmod +x "$stubs/$1"
}

stub kind 'case "$1" in get) exit 0 ;; esac; exit 0'
stub kubectl '
case "$*" in
  *"get --raw /version"*) echo "{\"major\":\"1\",\"minor\":\"${STUB_SERVER_MINOR:-31}+\"}" ;;
  *"jsonpath={.status.targets[0].score}"*) echo 40 ;;
  *"get clusterreadiness cluster -o json"*)
    echo "{\"spec\":{\"targets\":[\"1.32\"]},\"status\":{\"targets\":[{\"target\":\"1.32\",\"score\":40,\"verdict\":\"blocked\",\"ready\":false,\"topFindings\":[{\"category\":\"eol-addon\"}]}]}}" ;;
  *"port-forward"*) exec sleep 30 ;;
  *"get clusterrole,clusterrolebinding -l"*) [ -n "${STUB_LEFTOVER:-}" ] && echo clusterrole/upgradescope-agent; exit 0 ;;
  *"get clusterrole upgradescope-agent"* | *"get clusterrolebinding upgradescope-agent"*) exit 1 ;;
esac
exit 0'
stub helm '
case "$*" in
  *"agent.targets="*) [ -z "${STUB_TARGETS_FAIL:-}" ] || { echo "Error: ClusterReadiness \"cluster\" exists and cannot be imported" >&2; exit 1; } ;;
esac
exit 0'
stub docker 'case "$1" in version) echo linux/amd64 ;; save) for a; do [ "$p" = -o ] && : >"$a"; p=$a; done ;; esac; exit 0'
stub go 'exit 0'
stub make 'exit 0'
stub curl '
case "$*" in
  */api/v1/clusters*) echo "[{\"name\":\"kind\",\"score\":40}]" ;;
  */healthz*) echo "{\"status\":\"ok\"}" ;;
esac'
stub upgradescope '
if [ -n "${STUB_REMOVED:-}" ]; then
  echo "{\"findings\":[{\"category\":\"removed-api\",\"severity\":\"blocker\",\"title\":\"flowschemas v1beta3\"}]}"
else
  echo "{\"findings\":[{\"category\":\"eol-addon\",\"severity\":\"blocker\",\"title\":\"ingress-nginx\"}]}"
fi'
stub install-tool 'echo "'"$stubs"'/$1"'

: >"$work/results"
# run <name> <want-exit> [env...]: run e2e.sh against the stubs; the log and
# summary of the last run stay in $work for the assertions that follow.
run() {
  local name=$1 want=$2 got=0
  shift 2
  : >"$work/log"
  : >"$work/summary"
  env PATH="$stubs:$PATH" KUBECONFIG="$work/no-kubeconfig" STUB_LOG="$work/log" \
    GITHUB_STEP_SUMMARY="$work/summary" E2E_MINOR=1.31 E2E_NAP=0 \
    E2E_INSTALL_TOOL="$stubs/install-tool" E2E_UPGRADESCOPE="$stubs/upgradescope" \
    "$@" hack/e2e.sh >"$work/out" 2>&1 || got=$?
  if [ "$got" = "$want" ]; then
    echo "ok   $name" | tee -a "$work/results"
  else
    echo "FAIL $name: exit $got (want $want), output:" >&2
    tail -30 "$work/out" | sed 's/^/     /' >&2
    echo "FAIL $name" >>"$work/results"
  fi
}
# has <name> <file> <substring>
has() {
  if grep -qF -- "$3" "$2"; then
    echo "ok   $1" | tee -a "$work/results"
  else
    echo "FAIL $1: '$3' not in $2:" >&2
    sed 's/^/     /' "$2" >&2
    echo "FAIL $1" >>"$work/results"
  fi
}

run "everything passes" 0
has "summary names the minor and pinned image" "$work/summary" "Kubernetes 1.31 (kindest/node:v1.31.14@sha256:"
has "regression passes on a clean scan" "$work/summary" "- PASS — vanilla 1.31 cluster scanned at 1.32 has zero removed-api blockers"
has "uninstall check gates and passes" "$work/summary" "- PASS — helm uninstall leaves no ClusterRole/ClusterRoleBinding"
has "the cluster is created on the pinned image" "$work/log" "kind create cluster --name upgradescope-demo --image kindest/node:v1.31.14@sha256:"
has "the agent-targets upgrade uses --set-string" "$work/log" "--set-string agent.targets={1.32}"
# Every kubectl call names the kind context (kind-setup.sh's use-context is
# the one documented exception), and so does every helm call on the release.
if grep '^kubectl ' "$work/log" | grep -v '^kubectl config use-context kind-upgradescope-demo$' |
  grep -v -- '--context kind-upgradescope-demo' >"$work/stray"; then
  echo "FAIL kubectl calls without --context:" >&2; sed 's/^/     /' "$work/stray" >&2
  echo "FAIL kubectl context" >>"$work/results"
else
  echo "ok   every kubectl call names the kind context" | tee -a "$work/results"
fi
if grep -E '^helm (upgrade|uninstall) .*(deploy/chart|upgradescope --namespace)' "$work/log" |
  grep -v -- '--kube-context kind-upgradescope-demo' >"$work/stray"; then
  echo "FAIL helm release calls without --kube-context:" >&2; sed 's/^/     /' "$work/stray" >&2
  echo "FAIL helm context" >>"$work/results"
else
  echo "ok   every helm call on the release names the kind context" | tee -a "$work/results"
fi

run "a removed-api blocker on a vanilla cluster is reported, not fatal (#3)" 0 STUB_REMOVED=1
has "the #3 regression is a FAIL in the summary" "$work/summary" "- **FAIL** — vanilla 1.31 cluster scanned at 1.32 has zero removed-api blockers (best-effort until #3"
has "the #3 regression is a warning" "$work/out" "::warning title=kind e2e 1.31: best-effort check failed::"

run "a failing agent.targets upgrade is reported, not fatal (#41)" 0 STUB_TARGETS_FAIL=1
has "the #41 upgrade is a FAIL in the summary" "$work/summary" "- **FAIL** — helm upgrade --set agent.targets={1.32} (best-effort until #41"

run "a ClusterRole left after uninstall fails the run" 1 STUB_LEFTOVER=1
has "the leftover is named" "$work/out" "clusterrole/upgradescope-agent"
has "the uninstall gate is a FAIL in the summary" "$work/summary" "- **FAIL** — helm uninstall leaves no ClusterRole/ClusterRoleBinding"

run "a cluster on the wrong minor fails the run" 1 STUB_SERVER_MINOR=30
has "the version mismatch is explained" "$work/out" "cluster runs Kubernetes 1.30, want 1.31"

run "an unknown minor fails before anything runs" 1 E2E_MINOR=1.12
has "nothing was created" "$work/out" "no kind node image for 1.12"

pass=$(grep -c '^ok' "$work/results" || true)
fail=$(grep -c '^FAIL' "$work/results" || true)
echo "e2e_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ] && [ "$pass" -gt 0 ]
