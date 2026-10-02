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
  *"get clusterreadiness cluster -o json"*)
    echo "{\"spec\":{\"targets\":[\"${STUB_NEXT:-1.32}\"]},\"status\":{\"targets\":[{\"target\":\"${STUB_NEXT:-1.32}\",\"score\":40,\"verdict\":\"blocked\",\"ready\":false,\"topFindings\":[{\"category\":\"eol-addon\"}]}]}}" ;;
  *"port-forward"*) exec sleep 30 ;;
  *"get clusterrole,clusterrolebinding -l"*) [ -n "${STUB_LEFTOVER:-}" ] && echo clusterrole/upgradescope-agent; exit 0 ;;
  *"get clusterrole upgradescope-agent"* | *"get clusterrolebinding upgradescope-agent"*) exit 1 ;;
  *"api-versions"*) [ -n "${STUB_NOT_SERVED:-}" ] || printf "v1\\nflowcontrol.apiserver.k8s.io/v1beta3\\nresource.k8s.io/v1beta1\\n" ;;
  *"apply --server-side"*) cat >"$STUB_STATE/applied.yaml" ;;
esac
exit 0'
stub helm '
case "$*" in
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
# The scanner: an unreachable --kubeconfig exits 1; otherwise the EOL
# ingress-nginx blocker, plus the object the e2e last applied while it went
# through a deprecated version (that apiVersion in the key, its manager),
# and exit 2 on a blocker unless --fail-on never.
stub upgradescope - <<'EOF'
never="" unreachable=""
for a; do
  case "$a" in --kubeconfig) unreachable=1 ;; never) never=1 ;; esac
done
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
jq -n --arg removed "${STUB_REMOVED:-}" --arg noeol "${STUB_NO_EOL:-}" --arg kept "${STUB_KEPT_FINDING:-}" \
  --arg applied "$applied" --arg manager "${STUB_MANAGER-upgradescope-e2e}" '{findings: (
    (if $removed == "" then [] else [{category: "removed-api", severity: "blocker", title: "flowschemas v1beta3"}] end)
    + (if $noeol != "" then [] else [{category: "eol-addon", severity: "blocker", key: "eol-addon/ingress-nginx",
        title: "Ingress NGINX", namespaces: (["ingress-nginx"] + (if $kept == "" then [] else ["e2e-keep-history"] end))}] end)
    + (if $applied == "" then [] else [{category: "removed-api", severity: "blocker", key: ("removed-api/" + $applied + "/Kind"),
        title: ($applied + " removed"), objects: [{name: "upgradescope-e2e-deprecated", manager: $manager}]}] end))}' >"$STUB_STATE/report.json"
cat "$STUB_STATE/report.json"
[ -z "$never" ] || exit 0
jq -e '[.findings[] | select(.severity == "blocker")] | length == 0' "$STUB_STATE/report.json" >/dev/null || exit 2
EOF
stub install-tool 'echo "'"$stubs"'/$1"'

# The API server's audit log (one JSON event per line, Metadata level) as
# the e2e reads it with docker exec. ev <user> <userAgent> <verb> <apiGroup>
# <apiVersion> <resource> <name> <requestURI> [deprecated] [subresource].
SCAN_UA="upgradescope/v0.0.0 (linux/amd64) kubernetes/\$Format"
AGENT=system:serviceaccount:upgradescope:upgradescope
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
  ev kubernetes-admin "$SCAN_UA" list "" v1 componentstatuses "" "/api/v1/componentstatuses?limit=500" deprecated
  ev kubernetes-admin "$SCAN_UA" list "" v1 secrets "" "/api/v1/secrets?labelSelector=owner%3Dhelm&limit=500"
  ev kubernetes-admin "$SCAN_UA" get "" v1 secrets sh.helm.release.v1.ingress-nginx.v1 "/api/v1/namespaces/ingress-nginx/secrets/sh.helm.release.v1.ingress-nginx.v1"
  ev "$AGENT" "$SCAN_UA" list "" v1 endpoints "" "/api/v1/endpoints?limit=500" deprecated
  ev "$AGENT" "$SCAN_UA" list "" v1 secrets "" "/api/v1/secrets?labelSelector=owner%3Dhelm&limit=500"
  ev "$AGENT" "$SCAN_UA" get "" v1 secrets sh.helm.release.v1.ingress-nginx.v1 "/api/v1/namespaces/ingress-nginx/secrets/sh.helm.release.v1.ingress-nginx.v1"
  ev "$AGENT" "$SCAN_UA" patch apiextensions.k8s.io v1 customresourcedefinitions clusterreadinesses.upgradescope.dev "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/clusterreadinesses.upgradescope.dev?fieldManager=upgradescope-agent"
  ev "$AGENT" "$SCAN_UA" create upgradescope.dev v1alpha1 clusterreadinesses cluster "/apis/upgradescope.dev/v1alpha1/clusterreadinesses"
  ev "$AGENT" "$SCAN_UA" update upgradescope.dev v1alpha1 clusterreadinesses cluster "/apis/upgradescope.dev/v1alpha1/clusterreadinesses/cluster/status" "" status
  # Not upgradescope: kubectl through a deprecated API, the in-process ITs
  # writing their own CRD and CR, a controller writing a Secret.
  ev kubernetes-admin "kubectl/v1.37.1 (linux/amd64) kubernetes/abc" patch flowcontrol.apiserver.k8s.io v1beta3 flowschemas e2e "/apis/flowcontrol.apiserver.k8s.io/v1beta3/flowschemas/e2e" deprecated
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
has "allowlisted requests are listed" "$work/out" "  v1 componentstatuses (scan)"
has "the unreachable-server gate passes" "$work/summary" "- PASS — scan against an unreachable API server exits 1"
has "the deprecated-object gate passes" "$work/summary" "- PASS — an object written through flowcontrol.apiserver.k8s.io/v1beta3 is reported with its manager; re-applied through flowcontrol.apiserver.k8s.io/v1 it is not"
has "the EOL add-on gate passes" "$work/summary" "- PASS — scan reports the EOL ingress-nginx installed from the upstream chart as a blocker and exits 2"
has "the keep-history gate passes" "$work/summary" "- PASS — a Helm release uninstalled with --keep-history yields no EOL finding"
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

printf '# nothing allowed\n' >"$work/empty-allowlist.txt"
run "an emptied allowlist makes today's self-requests fail" 1 E2E_DEPRECATED_ALLOWLIST="$work/empty-allowlist.txt"
has "the agent's endpoints LIST is named" "$work/out" "v1 endpoints agent list"

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

# 1.37's row: the DRA DeviceClass through resource.k8s.io/v1beta1.
run "1.37 writes a DeviceClass through resource.k8s.io/v1beta1" 0 E2E_MINOR=1.37 STUB_SERVER_MINOR=37 STUB_NEXT=1.38
has "1.37's kind config serves resource.k8s.io/v1beta1" "$work/kind-config.yaml" '"resource.k8s.io/v1beta1": "true"'
has "1.37 applies the DeviceClass fixture" "$work/state/applied.yaml" "kind: DeviceClass"

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
