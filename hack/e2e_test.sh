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

# stub <name> <body>: an executable that logs "<name> <args>" then runs body
# (read from stdin when body is -).
stub() {
  local body=$2
  [ "$body" != - ] || body=$(cat)
  printf '#!/usr/bin/env bash\necho "%s $*" >>"$STUB_LOG"\n%s\n' "$1" "$body" >"$stubs/$1"
  chmod +x "$stubs/$1"
}

stub kind '
case "$1" in
  get) [ -z "${STUB_KIND_EXISTS:-}" ] || echo upgradescope-demo; exit 0 ;;
  create) for a; do [ "$p" = --config ] && cp "$a" "$STUB_KIND_CONFIG"; p=$a; done ;;
esac
exit 0'
stub kubectl '
case "$*" in
  *"get --raw /version"*) echo "{\"major\":\"1\",\"minor\":\"${STUB_SERVER_MINOR:-31}+\"}" ;;
  *"jsonpath={.status.targets[0].score}"*) echo 40 ;;
  *"jsonpath={.metadata.generation}"*) [ -f "$STUB_STATE/ignore.json" ] && echo 3 || echo 2 ;;
  *"get clusterreadiness cluster -o json"*)
    [ -z "${STUB_FINALIZER:-}" ] || meta="\"metadata\":{\"name\":\"cluster\",\"finalizers\":[\"upgradescope.dev/hold\"]},"
    # Past the horizon (STUB_HORIZON, default 1.37), the agent records the
    # kb-coverage gap, unless STUB_CR_NO_KB_GAP. Once spec.ignore holds
    # rules (a merge patch), the agent has evaluated generation 3 (unless
    # STUB_CR_STALE) and accepted the blockers (unless STUB_CR_STILL_BLOCKED):
    # the gap alone leaves the verdict unknown.
    next=${STUB_NEXT:-1.32} horizon=${STUB_HORIZON:-1.37} gaps="" note=""
    if [ "${next#1.}" -gt "${horizon#1.}" ] && [ -z "${STUB_CR_NO_KB_GAP:-}" ]; then
      gaps=",\"notAssessed\":[\"kb-coverage: target $next is newer than the knowledge base (Kubernetes $horizon)\"]"
      note="; not fully assessed: kb-coverage (required); see status.notAssessed"
    fi
    gen=2 target="{\"target\":\"$next\",\"score\":40,\"verdict\":\"blocked\",\"ready\":false,\"blockers\":2,\"topFindings\":[{\"category\":\"eol-addon\",\"severity\":\"blocker\"},{\"category\":\"chart-incompat\",\"severity\":\"blocker\"},{\"category\":\"eol-addon\",\"severity\":\"warning\"}]}"
    ready="{\"type\":\"Ready\",\"status\":\"False\",\"reason\":\"Blocked\",\"message\":\"$next: 2 blocker(s) (score 40)$note\"}"
    if [ -f "$STUB_STATE/ignore.json" ]; then
      [ -n "${STUB_CR_STALE:-}" ] || gen=3
      if [ -z "${STUB_CR_STILL_BLOCKED:-}" ]; then
        target="{\"target\":\"$next\",\"score\":90,\"verdict\":\"unknown\",\"ready\":false,\"blockers\":0,\"suppressed\":2}"
        ready="{\"type\":\"Ready\",\"status\":\"Unknown\",\"reason\":\"NotAssessed\",\"message\":\"$next: no blockers found, but a required check was not assessed: kb-coverage (required); see status.notAssessed (score 90)\"}"
      fi
    fi
    echo "{${meta:-}\"spec\":{\"targets\":[\"$next\"]},\"status\":{\"observedGeneration\":$gen,\"targets\":[$target]$gaps,\"conditions\":[$ready]}}" ;;
  *"patch clusterreadiness cluster --type merge -p "*)
    for a; do [ "$p" != -p ] || printf "%s\n" "$a" >"$STUB_STATE/ignore.json"; p=$a; done ;;
  *"patch clusterreadiness cluster --type json -p "*)
    [ -z "${STUB_UNPATCH_FAIL:-}" ] || exit 1
    rm -f "$STUB_STATE/ignore.json" ;;
  *"port-forward"*) exec sleep 30 ;;
  *"get validatingwebhookconfigurations,mutatingwebhookconfigurations -o name"*)
    echo validatingwebhookconfiguration.admissionregistration.k8s.io/ingress-nginx-admission
    [ -z "${STUB_NEW_WEBHOOK:-}" ] || [ ! -f "$STUB_STATE/installed" ] ||
      echo validatingwebhookconfiguration.admissionregistration.k8s.io/upgradescope ;;
  *"get clusterrole,clusterrolebinding -l"*) [ -n "${STUB_LEFTOVER:-}" ] && echo clusterrole/upgradescope-agent; exit 0 ;;
  *"get clusterrole upgradescope-agent"* | *"get clusterrolebinding upgradescope-agent"*) exit 1 ;;
  *"api-versions"*) [ -n "${STUB_NOT_SERVED:-}" ] || printf "v1\\nflowcontrol.apiserver.k8s.io/v1beta3\\nresource.k8s.io/v1beta1\\n" ;;
  *"apply --server-side"*) cat >"$STUB_STATE/applied.yaml" ;;
  *"apply -f hack/e2e/istio-teams.yaml"*) [ -z "${STUB_ISTIO_APPLY_FAIL:-}" ] || exit 1; : >"$STUB_STATE/istio" ;;
  *"delete -f hack/e2e/istio-teams.yaml"*) [ -z "${STUB_ISTIO_DELETE_FAIL:-}" ] || exit 1; rm -f "$STUB_STATE/istio" ;;
esac
exit 0'
stub helm '
case "$*" in
  *"upgrade --install upgradescope deploy/chart"*) : >"$STUB_STATE/installed" ;;
  *"upgrade --install ingress-nginx "*) : >"$STUB_STATE/ingress-nginx" ;;
  *"agent.targets="*) [ -z "${STUB_TARGETS_FAIL:-}" ] || { echo "Error: ClusterReadiness \"cluster\" exists and cannot be imported" >&2; exit 1; } ;;
  *" history "*) echo "[{\"revision\":1,\"status\":\"superseded\"},{\"revision\":2,\"status\":\"${STUB_HISTORY_STATUS:-uninstalled}\"}]" ;;
esac
exit 0'
stub docker '
case "$1" in
  version) echo linux/amd64 ;;
  save) for a; do [ "$p" = -o ] && : >"$a"; p=$a; done ;;
  exec) cat "$STUB_AUDIT" ;;
esac
exit 0'
stub go '
case "$1" in
  test)
    if [ -n "${STUB_IT_SKIP:-}" ]; then
      echo "--- SKIP: TestScanIntegration_KindEOLIngressNginx (0.00s)"
    else
      echo "--- PASS: TestScanIntegration_KindEOLIngressNginx (4.10s)"
    fi
    echo "--- PASS: TestAgentIntegration_CRDStatusOnKind (6.20s)"
    echo "ok  	github.com/abd-ulbasit/upgradescope/internal/cli	10.4s" ;;
esac'
stub make 'exit 0'
stub curl '
case "$*" in
  */api/v1/clusters*) echo "[{\"name\":\"kind\",\"score\":40}]" ;;
  */healthz*) echo "{\"status\":\"ok\"}" ;;
esac'
# The scanner: `version --output json` reports the knowledge base horizon
# STUB_HORIZON (default 1.37); an unreachable --kubeconfig exits 1;
# otherwise the EOL ingress-nginx blocker once kind-setup.sh installed it,
# plus the object the e2e last applied while it went
# through a deprecated version (that apiVersion in the key, its manager),
# plus, once hack/e2e/istio-teams.yaml is applied, Istio's findings per
# release line (STUB_ISTIO=merged: each naming all three installs, as
# before #129; no-team: right namespaces, no teams; no-eol: without the
# 1.28 blocker) until it is deleted. A --target past the horizon adds a
# required kb-coverage gap (unless STUB_IGNORE_HORIZON): the verdict is
# then unknown without a blocker. Exit 2 on a blocker, or on an unknown
# verdict without --allow-incomplete (STUB_ALLOW_IGNORED: with it too),
# unless --fail-on never.
stub upgradescope - <<'EOF'
if [ "$*" = "version --output json" ]; then
  echo "{\"version\":\"0.0.0\",\"kbHorizon\":\"${STUB_HORIZON:-1.37}\"}"
  exit 0
fi
never="" unreachable="" allow="" target="" prev=""
for a; do
  case "$a" in --kubeconfig) unreachable=1 ;; never) never=1 ;; --allow-incomplete) allow=1 ;; esac
  [ "$prev" != --target ] || target=$a
  prev=$a
done
[ -z "${STUB_ALLOW_IGNORED:-}" ] || allow=""
horizon=${STUB_HORIZON:-1.37} past=""
[ -z "$target" ] || [ -n "${STUB_IGNORE_HORIZON:-}" ] || [ "${target#1.}" -le "${horizon#1.}" ] || past=1
if [ -n "$unreachable" ]; then
  [ -z "${STUB_UNREACHABLE_OK:-}" ] || { echo '{"findings":[]}'; exit 0; }
  echo "Error: dial tcp 127.0.0.1:1: connect: connection refused" >&2
  exit 1
fi
applied="" via=""
if [ -f "$STUB_STATE/applied.yaml" ]; then
  applied=$(sed -n 's/^apiVersion: //p' "$STUB_STATE/applied.yaml")
  via=$(sed -n 's/^ *e2e.upgradescope.dev\/applied-via: //p' "$STUB_STATE/applied.yaml")
fi
[ "$via" != ga ] || [ -n "${STUB_GA_STILL_REPORTED:-}" ] || applied=""
istio=""
[ ! -f "$STUB_STATE/istio" ] || istio=${STUB_ISTIO:-per-line}
noeol=${STUB_NO_EOL:-}
[ -f "$STUB_STATE/ingress-nginx" ] || noeol=1
jq -n --arg removed "${STUB_REMOVED:-}" --arg noeol "$noeol" --arg kept "${STUB_KEPT_FINDING:-}" \
  --arg applied "$applied" --arg manager "${STUB_MANAGER-upgradescope-e2e}" --arg istio "$istio" \
  --arg past "$past" --arg target "$target" '
  def mesh($line): if $istio == "merged"
    then {namespaces: ["e2e-istio-mid", "e2e-istio-new", "e2e-istio-old"], teams: ["e2e-team-mid", "e2e-team-new", "e2e-team-old"]}
    elif $istio == "no-team" then {namespaces: ["e2e-istio-" + $line]}
    else {namespaces: ["e2e-istio-" + $line], teams: ["e2e-team-" + $line]} end;
  {findings: (
    (if $removed == "" then [] else [{category: "removed-api", severity: "blocker", title: "flowschemas v1beta3"}] end)
    + (if $noeol != "" then [] else [{category: "eol-addon", severity: "blocker", key: "eol-addon/ingress-nginx",
        title: "Ingress NGINX", namespaces: (["ingress-nginx"] + (if $kept == "" then [] else ["e2e-keep-history"] end))}] end)
    + (if $applied == "" then [] else [{category: "removed-api", severity: "blocker", key: ("removed-api/" + $applied + "/Kind"),
        title: ($applied + " removed"), objects: [{name: "upgradescope-e2e-deprecated", manager: $manager}]}] end)
    + (if $istio == "" then [] else
        (if $istio == "no-eol" then [] else [{category: "eol-addon", severity: "blocker", key: "eol-addon/istio/1.28",
          title: "Istio"} + mesh("old")] end)
        + [{category: "chart-incompat", severity: "blocker", key: "chart-incompat/istio/1.28", title: "Istio"} + mesh("old"),
           {category: "eol-approaching", severity: "warning", key: "eol-approaching/istio/1.30", title: "Istio"} + mesh("mid")] end))}
  | .notAssessed = (if $past == "" then [] else [{capability: "kb-coverage", required: true,
      reason: ("target " + $target + " is newer than the knowledge base")}] end)
  | .verdict = (if any(.findings[]; .severity == "blocker") then "blocked"
      elif .notAssessed != [] then "unknown" else "ready" end)
  | .ready = (.verdict == "ready")' >"$STUB_STATE/report.json"
cat "$STUB_STATE/report.json"
[ -z "$never" ] || exit 0
jq -e '[.findings[] | select(.severity == "blocker")] | length == 0' "$STUB_STATE/report.json" >/dev/null || exit 2
[ -n "$allow" ] || jq -e '.verdict != "unknown"' "$STUB_STATE/report.json" >/dev/null || exit 2
EOF
stub install-tool 'echo "'"$stubs"'/$1"'

# The API server's audit log (one JSON event per line, Metadata level) as
# the e2e reads it with docker exec. ev <user> <userAgent> <verb> <apiGroup>
# <apiVersion> <resource> <name> <requestURI> [deprecated] [subresource].
SCAN_UA="upgradescope/v0.0.0 (linux/amd64) kubernetes/\$Format"
AGENT=system:serviceaccount:upgradescope:upgradescope
KUBECTL_UA="kubectl/v1.37.1 (linux/amd64) kubernetes/abc"
ev() {
  jq -nc --arg user "$1" --arg ua "$2" --arg verb "$3" --arg group "$4" --arg version "$5" \
    --arg resource "$6" --arg name "$7" --arg uri "$8" --arg dep "${9:-}" --arg sub "${10:-}" '{
      kind: "Event", apiVersion: "audit.k8s.io/v1", level: "Metadata", stage: "ResponseComplete",
      verb: $verb, requestURI: $uri, user: {username: $user}, userAgent: $ua,
      objectRef: ({resource: $resource, apiVersion: $version}
        + (if $group == "" then {} else {apiGroup: $group} end)
        + (if $name == "" then {} else {name: $name} end)
        + (if $sub == "" then {} else {subresource: $sub} end)),
      responseStatus: {code: 200},
      annotations: (if $dep == "" then {} else {"k8s.io/deprecated": "true"} end)}'
}
{
  ev kubernetes-admin "$SCAN_UA" list flowcontrol.apiserver.k8s.io v1 flowschemas "" "/apis/flowcontrol.apiserver.k8s.io/v1/flowschemas?limit=500"
  ev kubernetes-admin "$SCAN_UA" list "" v1 secrets "" "/api/v1/secrets?labelSelector=owner%3Dhelm&limit=500"
  ev kubernetes-admin "$SCAN_UA" get "" v1 secrets sh.helm.release.v1.ingress-nginx.v1 "/api/v1/namespaces/ingress-nginx/secrets/sh.helm.release.v1.ingress-nginx.v1"
  ev "$AGENT" "$SCAN_UA" list "" v1 secrets "" "/api/v1/secrets?labelSelector=owner%3Dhelm&limit=500"
  ev "$AGENT" "$SCAN_UA" get "" v1 secrets sh.helm.release.v1.ingress-nginx.v1 "/api/v1/namespaces/ingress-nginx/secrets/sh.helm.release.v1.ingress-nginx.v1"
  ev "$AGENT" "$SCAN_UA" patch apiextensions.k8s.io v1 customresourcedefinitions clusterreadinesses.upgradescope.dev "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/clusterreadinesses.upgradescope.dev?fieldManager=upgradescope-agent"
  ev "$AGENT" "$SCAN_UA" create upgradescope.dev v1alpha1 clusterreadinesses cluster "/apis/upgradescope.dev/v1alpha1/clusterreadinesses"
  ev "$AGENT" "$SCAN_UA" update upgradescope.dev v1alpha1 clusterreadinesses cluster "/apis/upgradescope.dev/v1alpha1/clusterreadinesses/cluster/status" "" status
  # Not upgradescope: kubectl applying the deprecated-API fixture (1.31's and
  # 1.37's row; the audit gates' positive control), the in-process ITs
  # writing their own CRD and CR, a controller writing a Secret.
  ev kubernetes-admin "$KUBECTL_UA" patch flowcontrol.apiserver.k8s.io v1beta3 flowschemas upgradescope-e2e-deprecated "/apis/flowcontrol.apiserver.k8s.io/v1beta3/flowschemas/upgradescope-e2e-deprecated?fieldManager=upgradescope-e2e" deprecated
  ev kubernetes-admin "$KUBECTL_UA" patch resource.k8s.io v1beta1 deviceclasses upgradescope-e2e-deprecated "/apis/resource.k8s.io/v1beta1/deviceclasses/upgradescope-e2e-deprecated?fieldManager=upgradescope-e2e" deprecated
  ev kubernetes-admin "cli.test/v0.0.0 (linux/amd64) kubernetes/\$Format" create upgradescope.dev v1alpha1 clusterreadinesses it-agent "/apis/upgradescope.dev/v1alpha1/clusterreadinesses"
  ev system:serviceaccount:kube-system:token-cleaner kube-controller-manager update "" v1 secrets bootstrap-token-abcdef "/api/v1/namespaces/kube-system/secrets/bootstrap-token-abcdef"
} >"$work/audit.jsonl"
# audit_with <name> <event...>: the good log plus these events, in $work/<name>.
audit_with() {
  local f="$work/$1"
  shift
  { cat "$work/audit.jsonl"; for e in "$@"; do echo "$e"; done; } >"$f"
}

: >"$work/results"
# run <name> <want-exit> [env...]: run e2e.sh against the stubs; the log and
# summary of the last run stay in $work for the assertions that follow.
run() {
  local name=$1 want=$2 got=0
  shift 2
  : >"$work/log"
  : >"$work/summary"
  rm -rf "$work/state" "$work/kind-config.yaml"
  mkdir -p "$work/state"
  env PATH="$stubs:$PATH" KUBECONFIG="$work/no-kubeconfig" STUB_LOG="$work/log" STUB_STATE="$work/state" \
    GITHUB_STEP_SUMMARY="$work/summary" E2E_MINOR=1.31 E2E_NAP=0 \
    STUB_AUDIT="$work/audit.jsonl" STUB_KIND_CONFIG="$work/kind-config.yaml" \
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
has "the cluster is created from the e2e kind config" "$work/log" "--config "
has "the kind config mounts the audit policy by absolute path" "$work/kind-config.yaml" "- hostPath: $PWD/hack/e2e/audit-policy.yaml"
has "the deprecated-request audit gate passes" "$work/summary" "- PASS — audit: scan and the agent made no deprecated-API request outside the allowlist"
has "the agent write-set audit gate passes" "$work/summary" "- PASS — audit: the agent wrote only ClusterReadiness/cluster (+ status) and the clusterreadinesses.upgradescope.dev CRD"
has "the Secrets audit gate passes" "$work/summary" "- PASS — audit: Secrets were read only through Helm's owner=helm list and release GETs"
has "the scan-writes audit gate passes" "$work/summary" "- PASS — audit: scan wrote nothing"
has "the positive control is reported" "$work/out" "positive control: 1 request(s) through flowcontrol.apiserver.k8s.io/v1beta3 annotated k8s.io/deprecated"
has "the webhook/finalizer gate passes" "$work/summary" "- PASS — the install added no webhook configuration; ClusterReadiness/cluster has no finalizer or owner reference"
has "the unreachable-server gate passes" "$work/summary" "- PASS — scan against an unreachable API server exits 1"
has "the deprecated-object gate passes" "$work/summary" "- PASS — an object written through flowcontrol.apiserver.k8s.io/v1beta3 is reported with its manager; re-applied through flowcontrol.apiserver.k8s.io/v1 it is not"
has "the EOL add-on gate passes" "$work/summary" "- PASS — scan reports the EOL ingress-nginx installed from the upstream chart as a blocker and exits 2"
has "the keep-history gate passes" "$work/summary" "- PASS — a Helm release uninstalled with --keep-history yields no EOL finding"
has "the Istio release-line gate passes" "$work/summary" "- PASS — Istio on three release lines in three teams' namespaces: each finding names only its own line's; 1.28 blocks"
has "the Istio pods are applied from hack/e2e/istio-teams.yaml" "$work/log" "kubectl --context kind-upgradescope-demo apply -f hack/e2e/istio-teams.yaml"
has "the Istio pods are deleted once judged" "$work/log" "kubectl --context kind-upgradescope-demo delete -f hack/e2e/istio-teams.yaml --ignore-not-found --wait --timeout 2m"
# Gone before the integration tests and the agent: their eol-addon checks
# are about ingress-nginx, and Istio 1.28's blocker would satisfy them.
istio_at=$(grep -n -- 'apply -f hack/e2e/istio-teams.yaml' "$work/log" | head -1 | cut -d: -f1 || true)
istio_gone=$(grep -n -- 'delete -f hack/e2e/istio-teams.yaml' "$work/log" | head -1 | cut -d: -f1 || true)
it_at=$(grep -n -- '^go test ./internal/cli/ -run Integration' "$work/log" | head -1 | cut -d: -f1 || true)
if [ -n "$istio_at" ] && [ -n "$istio_gone" ] && [ -n "$it_at" ] && [ "$istio_at" -lt "$istio_gone" ] &&
  [ "$istio_gone" -lt "$it_at" ] && [ ! -e "$work/state/istio" ]; then
  echo "ok   the Istio pods are gone before the integration tests" | tee -a "$work/results"
else
  echo "FAIL the Istio pods outlive their gate (apply at log line ${istio_at:-none}, delete ${istio_gone:-none}, go test ${it_at:-none})" >&2
  echo "FAIL Istio pods outlive their gate" >>"$work/results"
fi
has "1.31's kind config serves flowcontrol v1beta3" "$work/kind-config.yaml" '"flowcontrol.apiserver.k8s.io/v1beta3": "true"'
has "the object is applied server-side under a named manager" "$work/log" "kubectl --context kind-upgradescope-demo apply --server-side --field-manager=upgradescope-e2e -f -"
has "the last apply went through the GA version" "$work/state/applied.yaml" "apiVersion: flowcontrol.apiserver.k8s.io/v1"
has "the kept release is uninstalled with --keep-history" "$work/log" "helm --kube-context kind-upgradescope-demo uninstall e2e-eol-uninstalled --namespace e2e-keep-history --keep-history"
# Every kubectl call names the kind context (kind-setup.sh's use-context is
# the one documented exception), and so does every helm call on the release.
if grep '^kubectl ' "$work/log" | grep -v '^kubectl config use-context kind-upgradescope-demo$' |
  grep -v -- '--context kind-upgradescope-demo' >"$work/stray"; then
  echo "FAIL kubectl calls without --context:" >&2; sed 's/^/     /' "$work/stray" >&2
  echo "FAIL kubectl context" >>"$work/results"
else
  echo "ok   every kubectl call names the kind context" | tee -a "$work/results"
fi
if grep -E '^helm ' "$work/log" | grep -v '^helm repo ' |
  grep -v -- '--kube-context kind-upgradescope-demo' >"$work/stray"; then
  echo "FAIL helm release calls without --kube-context:" >&2; sed 's/^/     /' "$work/stray" >&2
  echo "FAIL helm context" >>"$work/results"
else
  echo "ok   every helm call outside helm repo names the kind context" | tee -a "$work/results"
fi

run "a removed-api blocker on a vanilla cluster fails the run (#3 is fixed; the check gates)" 1 STUB_REMOVED=1
has "the #3 regression is a FAIL in the summary" "$work/summary" "- **FAIL** — vanilla 1.31 cluster scanned at 1.32 has zero removed-api blockers"

# A local re-run reuses the cluster, which already has the demo add-on, the
# CRD and the chart's leftovers: not the vanilla cluster #3's check is for.
run "a reused cluster still runs the gates" 0 STUB_KIND_EXISTS=1 STUB_REMOVED=1
has "the reused cluster is not recreated" "$work/out" "kind cluster 'upgradescope-demo' already exists, reusing it"
has "the #3 check is a SKIP in the summary, not a PASS" "$work/summary" "- SKIP — vanilla 1.31 cluster scanned at 1.32 has zero removed-api blockers (cluster reused, not vanilla; make demo-down first)"
has "the skip is a warning" "$work/out" "::warning title=kind e2e 1.31: check skipped::"
has "the deprecated-object check is a SKIP on a reused cluster" "$work/summary" "- SKIP — an object written through flowcontrol.apiserver.k8s.io/v1beta3"
has "the audit gates are a SKIP on a reused cluster" "$work/summary" "- SKIP — audit: scan wrote nothing (cluster reused, its audit log is not this run's"
if grep -q '^docker exec' "$work/log"; then
  echo "FAIL a reused cluster's audit log was still read" >&2
  echo "FAIL reused cluster audited" >>"$work/results"
else
  echo "ok   a reused cluster's audit log is not read" | tee -a "$work/results"
fi
# The vanilla scan is the one before the demo add-on goes in (kind-setup.sh's
# use-context); the later ones (EOL add-on, kept release) still run.
if sed '/^kubectl config use-context/q' "$work/log" | grep -q '^upgradescope scan --context'; then
  echo "FAIL a reused cluster was still scanned as vanilla" >&2
  echo "FAIL reused cluster scanned" >>"$work/results"
else
  echo "ok   a reused cluster is not scanned as vanilla" | tee -a "$work/results"
fi

run "a failing agent.targets upgrade fails the run (#41 is fixed; the check gates)" 1 STUB_TARGETS_FAIL=1
has "the upgrade gate is a FAIL in the summary" "$work/summary" "- **FAIL** — helm upgrade --set agent.targets={1.32}"

# RB-04: nothing in the cluster changes because of a finding.
run "a webhook configuration added by the install fails the run" 1 STUB_NEW_WEBHOOK=1
has "the new webhook is named" "$work/out" "  validatingwebhookconfiguration.admissionregistration.k8s.io/upgradescope"
has "the webhook/finalizer gate is a FAIL in the summary" "$work/summary" "- **FAIL** — the install added no webhook configuration"

run "a finalizer on the ClusterReadiness fails the run" 1 STUB_FINALIZER=1
has "the finalizer is named" "$work/out" "upgradescope.dev/hold"

run "a ClusterRole left after uninstall fails the run" 1 STUB_LEFTOVER=1
has "the leftover is named" "$work/out" "clusterrole/upgradescope-agent"
has "the uninstall gate is a FAIL in the summary" "$work/summary" "- **FAIL** — helm uninstall leaves no ClusterRole/ClusterRoleBinding"

# The ITs skip, not fail, when the context guard refuses, and go test is
# green then: the gate must see each one PASS by name.
run "a skipped integration test fails the run" 1 STUB_IT_SKIP=1
has "the skipped IT is named" "$work/out" "TestScanIntegration_KindEOLIngressNginx did not PASS"
has "the IT gate is a FAIL in the summary" "$work/summary" "- **FAIL** — scan + agent integration tests"

# The audit gates, each against a log with one offending event added.
audit_with dep.jsonl "$(ev kubernetes-admin "$SCAN_UA" list coordination.k8s.io v1beta1 leasecandidates "" "/apis/coordination.k8s.io/v1beta1/leasecandidates?limit=500" deprecated)"
run "a deprecated-API request from scan outside the allowlist fails the run" 1 STUB_AUDIT="$work/dep.jsonl"
has "the request is named" "$work/out" "coordination.k8s.io/v1beta1 leasecandidates scan list"
has "the deprecated-request gate is a FAIL in the summary" "$work/summary" "- **FAIL** — audit: scan and the agent made no deprecated-API request"

# The positive control: with the apiserver no longer annotating the e2e's
# own deprecated apply, a zero count proves nothing and must not pass.
grep -v "$KUBECTL_UA" "$work/audit.jsonl" >"$work/nocontrol.jsonl"
run "a log in which the e2e's own deprecated apply is not annotated fails the run" 1 STUB_AUDIT="$work/nocontrol.jsonl"
has "the missing control is explained" "$work/out" "the flowcontrol.apiserver.k8s.io/v1beta3 apply of upgradescope-e2e-deprecated is not annotated k8s.io/deprecated"
has "the control failure is the deprecated-request gate's" "$work/summary" "- **FAIL** — audit: scan and the agent made no deprecated-API request"

printf '# nothing allowed\n' >"$work/empty-allowlist.txt"
run "an empty allowlist passes: scan and the agent make no deprecated request (#123)" 0 E2E_DEPRECATED_ALLOWLIST="$work/empty-allowlist.txt"

printf 'v1 componentstatuses no issue number here\n' >"$work/bad-allowlist.txt"
run "a malformed allowlist line fails before anything runs" 1 E2E_DEPRECATED_ALLOWLIST="$work/bad-allowlist.txt"
has "the bad line is quoted" "$work/out" "is not '<apiVersion> <resource> #<issue> <reason>'"

audit_with cm.jsonl "$(ev "$AGENT" "$SCAN_UA" patch "" v1 configmaps kube-root-ca.crt "/api/v1/namespaces/default/configmaps/kube-root-ca.crt")"
run "an agent write outside its CR fails the run" 1 STUB_AUDIT="$work/cm.jsonl"
has "the write is named" "$work/out" "agent patch /api/v1/namespaces/default/configmaps/kube-root-ca.crt"
has "the write-set gate is a FAIL in the summary" "$work/summary" "- **FAIL** — audit: the agent wrote only ClusterReadiness/cluster"

audit_with othercr.jsonl "$(ev "$AGENT" "$SCAN_UA" update upgradescope.dev v1alpha1 clusterreadinesses someone-elses "/apis/upgradescope.dev/v1alpha1/clusterreadinesses/someone-elses/status" "" status)"
run "an agent write to another ClusterReadiness fails the run" 1 STUB_AUDIT="$work/othercr.jsonl"
has "the other CR is named" "$work/out" "clusterreadinesses/someone-elses/status"

audit_with secret.jsonl "$(ev "$AGENT" "$SCAN_UA" get "" v1 secrets db-password "/api/v1/namespaces/default/secrets/db-password")"
run "an agent GET of a non-Helm Secret fails the run" 1 STUB_AUDIT="$work/secret.jsonl"
has "the Secret is named" "$work/out" "agent get /api/v1/namespaces/default/secrets/db-password"
has "the Secrets gate is a FAIL in the summary" "$work/summary" "- **FAIL** — audit: Secrets were read only through Helm's"

audit_with list.jsonl "$(ev kubernetes-admin "$SCAN_UA" list "" v1 secrets "" "/api/v1/secrets?limit=500")"
run "a Secret list without Helm's selector fails the run" 1 STUB_AUDIT="$work/list.jsonl"
has "the unselected list is named" "$work/out" "scan list /api/v1/secrets?limit=500"

audit_with write.jsonl "$(ev kubernetes-admin "$SCAN_UA" create "" v1 events "" "/api/v1/namespaces/default/events")"
run "a write by scan fails the run" 1 STUB_AUDIT="$work/write.jsonl"
has "the scan write is named" "$work/out" "scan create /api/v1/namespaces/default/events"
has "the scan-writes gate is a FAIL in the summary" "$work/summary" "- **FAIL** — audit: scan wrote nothing"

grep -v "$AGENT" "$work/audit.jsonl" >"$work/noagent.jsonl"
run "a log without the agent's requests fails the run, not passes vacuously" 1 STUB_AUDIT="$work/noagent.jsonl"
has "the missing actor is named" "$work/out" "no request from agent in the audit log"

: >"$work/empty.jsonl"
run "an empty audit log fails the run" 1 STUB_AUDIT="$work/empty.jsonl"
has "the empty log is explained" "$work/out" "audit log is empty"

# The behaviour gates, each against the scanner getting it wrong.
run "a scan that exits 0 against an unreachable API server fails the run" 1 STUB_UNREACHABLE_OK=1
has "the exit code is named" "$work/out" "scan against an unreachable API server exited 0, want 1"

run "a deprecated object reported without its manager fails the run" 1 STUB_MANAGER=
has "the missing manager is explained" "$work/out" "no finding lists upgradescope-e2e-deprecated at flowcontrol.apiserver.k8s.io/v1beta3 with manager upgradescope-e2e"
has "the deprecated-object gate is a FAIL in the summary" "$work/summary" "- **FAIL** — an object written through flowcontrol.apiserver.k8s.io/v1beta3"

run "a deprecated object still reported after its GA re-apply fails the run" 1 STUB_GA_STILL_REPORTED=1
has "the stale finding is explained" "$work/out" "re-applied through flowcontrol.apiserver.k8s.io/v1, is still reported"

run "a deprecated group/version the cluster does not serve fails the run" 1 STUB_NOT_SERVED=1
has "the missing runtimeConfig is explained" "$work/out" "flowcontrol.apiserver.k8s.io/v1beta3 is not served"

run "a missing EOL ingress-nginx blocker fails the run" 1 STUB_NO_EOL=1
has "the EOL gate is a FAIL in the summary" "$work/summary" "- **FAIL** — scan reports the EOL ingress-nginx installed from the upstream chart"

run "an EOL finding for an uninstalled --keep-history release fails the run" 1 STUB_KEPT_FINDING=1
has "the kept release's namespace is named" "$work/out" "eol-addon finding in e2e-keep-history"
has "the keep-history gate is a FAIL in the summary" "$work/summary" "- **FAIL** — a Helm release uninstalled with --keep-history yields no EOL finding"

run "a kept release that is not uninstalled fails the run" 1 STUB_HISTORY_STATUS=deployed
has "the precondition is explained" "$work/out" "e2e-eol-uninstalled's last revision is deployed, not uninstalled"

# #129: Istio on three release lines, in three teams' namespaces.
run "Istio findings that name every install, not their own line's, fail the run" 1 STUB_ISTIO=merged
has "the over-attributed finding is named" "$work/out" 'eol-addon/istio/1.28 ns=["e2e-istio-mid","e2e-istio-new","e2e-istio-old"]'
has "the Istio gate is a FAIL in the summary" "$work/summary" "- **FAIL** — Istio on three release lines in three teams' namespaces"

run "a missing Istio 1.28 EOL blocker fails the run" 1 STUB_ISTIO=no-eol
has "the missing blocker is explained" "$work/out" "no eol-addon/istio/1.28 blocker naming only e2e-istio-old and e2e-team-old"

run "Istio findings with the right namespaces but no team fail the run" 1 STUB_ISTIO=no-team
has "the unattributed finding is named" "$work/out" 'eol-addon/istio/1.28 ns=["e2e-istio-old"] teams=[]'

run "Istio pods that cannot be applied fail the run" 1 STUB_ISTIO_APPLY_FAIL=1
has "the failed apply is explained" "$work/out" "kubectl apply -f hack/e2e/istio-teams.yaml failed 15 times"

run "Istio pods that cannot be deleted fail the run" 1 STUB_ISTIO_DELETE_FAIL=1
has "the failed delete is explained" "$work/out" "could not delete hack/e2e/istio-teams.yaml"

# 1.37's row: the DRA DeviceClass through resource.k8s.io/v1beta1.
run "1.37 writes a DeviceClass through resource.k8s.io/v1beta1" 0 E2E_MINOR=1.37 STUB_SERVER_MINOR=37 STUB_NEXT=1.38
has "1.37's kind config serves resource.k8s.io/v1beta1" "$work/kind-config.yaml" '"resource.k8s.io/v1beta1": "true"'
has "1.37 applies the DeviceClass fixture" "$work/state/applied.yaml" "kind: DeviceClass"

# VS-11 (#130): on the newest minor the next one is past the knowledge
# base's horizon. The vanilla cluster scanned there is unknown (exit 2),
# --allow-incomplete exits 0, and the agent's default ClusterReadiness
# names the kb-coverage gap and, its blockers accepted, is unknown.
horizon_gate="a vanilla cluster scanned past the KB horizon is unknown with a required kb-coverage gap (exit 2), and --allow-incomplete exits 0"
cr_gap_gate="ClusterReadiness for the default target past the KB horizon names the required kb-coverage gap"
cr_unknown_gate="ClusterReadiness for the default target past the KB horizon, its blockers accepted, is unknown with Ready Unknown/NotAssessed"
has "the horizon is read from the binary under test" "$work/log" "upgradescope version --output json"
has "1.37 at 1.38: the past-horizon scan gates and passes" "$work/summary" "- PASS — $horizon_gate"
has "1.37 at 1.38: --allow-incomplete is scanned" "$work/log" "upgradescope scan --context kind-upgradescope-demo --target 1.38 --allow-incomplete --output json"
has "1.37 at 1.38: the CR kb-coverage gate gates and passes" "$work/summary" "- PASS — $cr_gap_gate"
has "1.37 at 1.38: the CR unknown gate gates and passes" "$work/summary" "- PASS — $cr_unknown_gate"
has "the CR's blocker categories are accepted in ingress-nginx only" "$work/log" \
  'kubectl --context kind-upgradescope-demo patch clusterreadiness cluster --type merge -p {"spec":{"ignore":[{"category":"chart-incompat","namespace":"ingress-nginx","reason":"e2e: accepted to observe the kb-coverage gap alone"},{"category":"eol-addon","namespace":"ingress-nginx","reason":"e2e: accepted to observe the kb-coverage gap alone"}]}}'
# The rules are removed again, before the later gates read the CR.
accept_at=$(grep -n -- '--type merge' "$work/log" | head -1 | cut -d: -f1 || true)
unaccept_at=$(grep -n -- 'patch clusterreadiness cluster --type json -p \[{"op":"remove","path":"/spec/ignore"}\]' "$work/log" | head -1 | cut -d: -f1 || true)
server_at=$(grep -n -- 'port-forward' "$work/log" | head -1 | cut -d: -f1 || true)
if [ -n "$accept_at" ] && [ -n "$unaccept_at" ] && [ -n "$server_at" ] && [ "$accept_at" -lt "$unaccept_at" ] &&
  [ "$unaccept_at" -lt "$server_at" ] && [ ! -e "$work/state/ignore.json" ]; then
  echo "ok   the spec.ignore rules are removed before the later gates" | tee -a "$work/results"
else
  echo "FAIL the spec.ignore rules outlive their gate (accepted at log line ${accept_at:-none}, removed ${unaccept_at:-none}, next gate ${server_at:-none})" >&2
  echo "FAIL spec.ignore outlives its gate" >>"$work/results"
fi
# The past-horizon scan is of the vanilla cluster: before the deprecated
# apply and the EOL add-on, either of which is a blocker at 1.38.
allow_at=$(grep -n -- '--allow-incomplete' "$work/log" | head -1 | cut -d: -f1 || true)
apply_at=$(grep -n -- 'apply --server-side' "$work/log" | head -1 | cut -d: -f1 || true)
if [ -n "$allow_at" ] && [ -n "$apply_at" ] && [ "$allow_at" -lt "$apply_at" ]; then
  echo "ok   the past-horizon scan runs on the vanilla cluster" | tee -a "$work/results"
else
  echo "FAIL the past-horizon scan is not before the deprecated apply (--allow-incomplete at ${allow_at:-none}, apply at ${apply_at:-none})" >&2
  echo "FAIL past-horizon scan order" >>"$work/results"
fi

run "a scan past the KB horizon that is not unknown fails the run" 1 E2E_MINOR=1.37 STUB_SERVER_MINOR=37 STUB_NEXT=1.38 STUB_IGNORE_HORIZON=1
has "the exit code is named" "$work/out" "scan --target 1.38 (past the KB horizon 1.37) exited 0, want 2"
has "the past-horizon gate is a FAIL in the summary" "$work/summary" "- **FAIL** — $horizon_gate"

run "--allow-incomplete that still exits 2 past the KB horizon fails the run" 1 E2E_MINOR=1.37 STUB_SERVER_MINOR=37 STUB_NEXT=1.38 STUB_ALLOW_IGNORED=1
has "the --allow-incomplete exit code is named" "$work/out" "scan --target 1.38 --allow-incomplete exited 2, want 0"

run "a ClusterReadiness past the KB horizon without the kb-coverage gap fails the run" 1 E2E_MINOR=1.37 STUB_SERVER_MINOR=37 STUB_NEXT=1.38 STUB_CR_NO_KB_GAP=1
has "the CR gap gate is a FAIL in the summary" "$work/summary" "- **FAIL** — $cr_gap_gate"

run "a ClusterReadiness still blocked once its blockers are accepted fails the run" 1 E2E_MINOR=1.37 STUB_SERVER_MINOR=37 STUB_NEXT=1.38 STUB_CR_STILL_BLOCKED=1
has "the CR unknown gate is a FAIL in the summary" "$work/summary" "- **FAIL** — $cr_unknown_gate"
has "what the CR reported is shown" "$work/out" '"verdict":"blocked"'
if [ -e "$work/state/ignore.json" ]; then
  echo "FAIL a failing CR unknown gate left its spec.ignore rules behind" >&2; echo "FAIL rules left behind" >>"$work/results"
else
  echo "ok   a failing CR unknown gate still removes its spec.ignore rules" | tee -a "$work/results"
fi

run "a ClusterReadiness whose agent never evaluates the accepted generation fails the run" 1 E2E_MINOR=1.37 STUB_SERVER_MINOR=37 STUB_NEXT=1.38 STUB_CR_STALE=1
has "the stale generation is shown" "$work/out" '"observedGeneration":2'
has "the stale CR gate is a FAIL in the summary" "$work/summary" "- **FAIL** — $cr_unknown_gate"

run "spec.ignore rules that cannot be removed fail the run" 1 E2E_MINOR=1.37 STUB_SERVER_MINOR=37 STUB_NEXT=1.38 STUB_UNPATCH_FAIL=1
has "the failed removal is explained" "$work/out" "could not remove the spec.ignore rules from clusterreadiness/cluster"

run "a reused 1.37 cluster skips the vanilla past-horizon scan" 0 E2E_MINOR=1.37 STUB_SERVER_MINOR=37 STUB_NEXT=1.38 STUB_KIND_EXISTS=1
has "the past-horizon scan is a SKIP on a reused cluster" "$work/summary" "- SKIP — $horizon_gate (cluster reused, not vanilla"

# Below the newest minor the next one is within the horizon: not a check
# that can mean anything there, and not a warning either.
run "1.31 at 1.32 is within the KB horizon" 0
has "the past-horizon scan is N/A within the horizon" "$work/summary" "- N/A — $horizon_gate (1.32 is within the KB horizon 1.37)"
has "the CR gap check is N/A within the horizon" "$work/summary" "- N/A — $cr_gap_gate (1.32 is within the KB horizon 1.37)"
has "the CR unknown check is N/A within the horizon" "$work/summary" "- N/A — $cr_unknown_gate (1.32 is within the KB horizon 1.37)"
if grep -q -- 'patch clusterreadiness' "$work/log"; then
  echo "FAIL 1.31 still patched the ClusterReadiness" >&2; echo "FAIL 1.31 CR patched" >>"$work/results"
else
  echo "ok   1.31 does not patch the ClusterReadiness" | tee -a "$work/results"
fi
if grep -q -- '--allow-incomplete' "$work/log"; then
  echo "FAIL 1.31 still scanned past the horizon" >&2; echo "FAIL 1.31 past-horizon scan" >>"$work/results"
else
  echo "ok   1.31 runs no past-horizon scan" | tee -a "$work/results"
fi
if grep -q 'check skipped::a vanilla cluster scanned past the KB horizon' "$work/out"; then
  echo "FAIL N/A is a warning" >&2; echo "FAIL N/A warns" >>"$work/results"
else
  echo "ok   N/A is not a warning" | tee -a "$work/results"
fi

printf '1.29 1.30 flowcontrol.apiserver.k8s.io/v1beta3 flowcontrol.apiserver.k8s.io/v1 flowschema.yaml\n' >"$work/short-table.txt"
run "a minor without a deprecated-API row fails before anything runs" 1 E2E_DEPRECATED_TABLE="$work/short-table.txt"
has "the missing row is named" "$work/out" "want exactly one row for 1.31"
if [ -s "$work/log" ]; then
  echo "FAIL a minor without a row still ran:" >&2; sed 's/^/     /' "$work/log" >&2
  echo "FAIL missing row ran commands" >>"$work/results"
else
  echo "ok   a minor without a row runs no command at all" | tee -a "$work/results"
fi

# The real table: one row per minor the kube job runs, each manifest present
# with both placeholders.
while read -r minor _; do
  case "$minor" in '' | '#'*) continue ;; esac
  m=${minor#1.} n=0
  while read -r from to dep ga manifest; do
    case "$from" in '' | '#'*) continue ;; esac
    if [ "$m" -ge "${from#1.}" ] && [ "$m" -le "${to#1.}" ]; then
      n=$((n + 1))
      f="hack/e2e/fixtures/$manifest"
      { [ -f "$f" ] && grep -q '^apiVersion: __API_VERSION__$' "$f" && grep -q '__APPLIED_VIA__' "$f"; } ||
        { echo "FAIL $f: missing, or without __API_VERSION__/__APPLIED_VIA__" >&2; echo "FAIL fixture $manifest" >>"$work/results"; }
    fi
  done <hack/e2e/deprecated-api.txt
  if [ "$n" = 1 ]; then
    echo "ok   hack/e2e/deprecated-api.txt has one row for $minor" | tee -a "$work/results"
  else
    echo "FAIL hack/e2e/deprecated-api.txt has $n rows for $minor, want 1" >&2
    echo "FAIL deprecated-api rows for $minor" >>"$work/results"
  fi
done <hack/kind-node-images.txt

run "a cluster on the wrong minor fails the run" 1 STUB_SERVER_MINOR=30
has "the version mismatch is explained" "$work/out" "cluster runs Kubernetes 1.30, want 1.31"

run "an unknown minor fails before anything runs" 1 E2E_MINOR=1.12
has "the unknown minor is named" "$work/out" "no kind node image for 1.12"
if [ -s "$work/log" ]; then
  echo "FAIL an unknown minor still ran:" >&2; sed 's/^/     /' "$work/log" >&2
  echo "FAIL unknown minor ran commands" >>"$work/results"
else
  echo "ok   an unknown minor runs no command at all" | tee -a "$work/results"
fi

pass=$(grep -c '^ok' "$work/results" || true)
fail=$(grep -c '^FAIL' "$work/results" || true)
echo "e2e_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ] && [ "$pass" -gt 0 ]
