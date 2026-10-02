#!/usr/bin/env bash
# kind end-to-end on one Kubernetes minor (make e2e E2E_MINOR=1.31; CI's
# kube job runs it once per minor of hack/kind-node-images.txt):
#
#   1. a kind cluster on that minor's digest-pinned node image, with kind and
#      kubectl from hack/install-tool.sh (pinned, sha256-verified), an audit
#      log, and one deprecated group/version served (hack/e2e/kind-config.yaml);
#      `scan` against an unreachable API server exits 1;
#   2. regression (#3): the vanilla cluster, scanned at its next minor, has
#      zero removed-api blockers — a fresh cluster cannot be blocked by APIs
#      nobody uses (skipped, with a warning, when an existing cluster is
#      reused: it is no longer vanilla); then an object applied through that
#      deprecated group/version (hack/e2e/deprecated-api.txt) is reported
#      with its field manager, and once re-applied through the GA version it
#      is not (skipped on a reused cluster too); on the newest minor, whose
#      next one is past the knowledge base's horizon (`upgradescope version`),
#      the vanilla cluster scanned there is unknown with a required
#      kb-coverage gap and exits 2, and exits 0 with --allow-incomplete
#      (N/A on other minors, but a warning on the newest minor of
#      hack/kind-node-images.txt when its next one is within the horizon:
#      then no minor runs it; skipped on a reused cluster);
#   3. the EOL ingress-nginx demo add-on (hack/demo/kind-setup.sh), which
#      scan reports as a blocker (exit 2); a second release of that chart,
#      uninstalled with --keep-history, yields no EOL finding; Istio pods on
#      three release lines in three teams' namespaces
#      (hack/e2e/istio-teams.yaml) get findings that each name only their
#      own line's namespace and team, and EOL 1.28 blocks (#129), after
#      which they are deleted, so the later ingress-nginx checks see only
#      ingress-nginx; and the
#      scan + agent integration tests (UPGRADESCOPE_IT=1);
#   4. the image built from this tree, kind-loaded, and the chart installed
#      from deploy/chart with the server enabled, --wait;
#   5. the agent's ClusterReadiness gets a score and a verdict for the next
#      minor (blocked, with the ingress-nginx eol-addon finding), and on the
#      newest minor names the required kb-coverage gap of that target and,
#      once spec.ignore accepts the demo add-on's blockers, is unknown with
#      Ready Unknown/NotAssessed (the rules are removed again);
#      the install added no admission webhook, and the CR carries no
#      finalizer or owner reference;
#   6. the server ingested the agent's snapshot (GET /api/v1/clusters);
#   7. `helm upgrade --set agent.targets={<next minor>}` succeeds and the CR
#      spec follows (#41);
#   8. `helm uninstall` leaves no ClusterRole/ClusterRoleBinding and no
#      namespaced release object behind, and keeps the CRD, as the chart
#      README documents (Helm never deletes crds/);
#   9. from the API server's audit log (hack/e2e/audit-policy.yaml, Metadata
#      level, over the whole run): `upgradescope scan` and the agent's
#      ServiceAccount made no deprecated-API request (k8s.io/deprecated)
#      outside hack/e2e/deprecated-request-allowlist.txt, while step 2's
#      apply through the deprecated group/version is annotated (the
#      positive control: the annotation is being recorded); the agent wrote
#      only its ClusterReadiness (+ status) and the ClusterReadiness CRD;
#      Secrets were read only through Helm's owner=helm list and GETs of
#      Helm release Secrets; scan wrote nothing. Skipped on a reused
#      cluster, whose log (if any) is not this run's.
#
# Steps marked best-effort below guard bugs that are not fixed on this
# branch's base yet: they report PASS/FAIL in the job summary
# ($GITHUB_STEP_SUMMARY) and as a warning, but do not fail the run. Every
# other step gates.
#
# Safety: every kubectl/helm call names the kind context explicitly
# (kind-upgradescope-demo); nothing here reads the current context.
# hack/demo/kind-setup.sh does switch the current context to the kind
# cluster, as `make demo-up` always has. The audit log is read from the
# control-plane node with `docker exec`. The cluster is left running for
# inspection; `make demo-down` deletes it.
#
# Needs Docker, helm, go, jq and curl; installs kind and kubectl itself.
# Docker must be this machine's engine: the audit policy is an extraMount of
# this checkout's path, which the engine's host resolves; kubectl, scan and
# the integration tests dial the 127.0.0.1:<port> kind writes into the
# kubeconfig, which is the engine's host too. With a remote engine, run the
# e2e on that machine.
#
# Knobs: E2E_MINOR (default 1.37). For hack/e2e_test.sh, which runs this
# against stubs: E2E_INSTALL_TOOL, E2E_UPGRADESCOPE (the binary `make build`
# produces), E2E_NAP (seconds between polls, overriding each poll's own),
# E2E_DEPRECATED_ALLOWLIST (default hack/e2e/deprecated-request-allowlist.txt),
# E2E_DEPRECATED_TABLE (default hack/e2e/deprecated-api.txt).
set -euo pipefail
cd "$(dirname "$0")/.."

MINOR=${E2E_MINOR:-1.37}
INSTALL_TOOL=${E2E_INSTALL_TOOL:-hack/install-tool.sh}
UPGRADESCOPE=${E2E_UPGRADESCOPE:-bin/upgradescope}
ALLOWLIST=${E2E_DEPRECATED_ALLOWLIST:-hack/e2e/deprecated-request-allowlist.txt}
DEPRECATED_TABLE=${E2E_DEPRECATED_TABLE:-hack/e2e/deprecated-api.txt}
CLUSTER=upgradescope-demo # the cluster kind-setup.sh and the ITs use
CTX=kind-$CLUSTER
NS=upgradescope
RELEASE=upgradescope
IMAGE=ghcr.io/abd-ulbasit/upgradescope
TAG=e2e
TOKEN=e2e-token
CR=cluster
# The chart's agent ServiceAccount: the release name, which contains the
# chart name (templates/_helpers.tpl fullname).
AGENT_SA=system:serviceaccount:$NS:$RELEASE
CRD=clusterreadinesses.upgradescope.dev
# Inside the control-plane node (hack/e2e/kind-config.yaml).
AUDIT_LOGS=/var/log/kubernetes/upgradescope-e2e

NODE_IMAGE=$(hack/kind-images.sh image "$MINOR")
NEXT=$(hack/kind-images.sh next "$MINOR")

# The allowlist as "<apiVersion> <resource>" lines; a malformed line fails
# the run before anything starts.
read_allowlist() {
  local apiv res issue reason
  while read -r apiv res issue reason; do
    case "$apiv" in '' | '#'*) continue ;; esac
    [[ "$issue" =~ ^#[0-9]+$ ]] && [ -n "$res" ] && [ -n "$reason" ] || {
      echo "ERROR: $ALLOWLIST: '$apiv $res $issue $reason' is not '<apiVersion> <resource> #<issue> <reason>'" >&2
      return 1
    }
    echo "$apiv $res"
  done <"$ALLOWLIST"
}
allowed=$(read_allowlist)

# This minor's row of the deprecated-API table: "<deprecated GV> <GA GV>
# <manifest>". No row (or two) fails the run before anything starts.
deprecated_api_row() {
  local from to dep ga manifest m=${MINOR#1.} rows=""
  while read -r from to dep ga manifest; do
    case "$from" in '' | '#'*) continue ;; esac
    if [ "$m" -ge "${from#1.}" ] && [ "$m" -le "${to#1.}" ]; then rows+="$dep $ga $manifest"$'\n'; fi
  done <"$DEPRECATED_TABLE"
  [ "$(grep -c . <<<"$rows")" = 1 ] || { echo "ERROR: $DEPRECATED_TABLE: want exactly one row for $MINOR" >&2; return 1; }
  echo "$rows"
}
row=$(deprecated_api_row)
read -r DEP_GV GA_GV DEP_MANIFEST <<<"$row"
DEP_OBJ=upgradescope-e2e-deprecated # metadata.name in every hack/e2e/fixtures manifest
FIELD_MANAGER=upgradescope-e2e
# A second release of the demo add-on's chart, uninstalled with --keep-history.
KEPT_RELEASE=e2e-eol-uninstalled
KEPT_NS=e2e-keep-history

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

# not_applicable <name> <why>: a check for another minor (no warning: every
# run of this minor is like this one).
not_applicable() {
  results+=("- N/A — $1 ($2)")
  echo "== $1: N/A ($2)"
}

reused=""
create_cluster() {
  if grep -qx "$CLUSTER" <<<"$(kind get clusters 2>/dev/null)"; then
    echo "kind cluster '$CLUSTER' already exists, reusing it"
    reused=1
  else
    # kind needs the audit policy's absolute path (an extraMount).
    sed -e "s|__AUDIT_POLICY__|$PWD/hack/e2e/audit-policy.yaml|" -e "s|__DEPRECATED_GV__|$DEP_GV|" \
      hack/e2e/kind-config.yaml >"$work/kind-config.yaml" || return 1
    kind create cluster --name "$CLUSTER" --image "$NODE_IMAGE" --config "$work/kind-config.yaml" --wait 120s || return 1
  fi
  local got
  got=$(k get --raw /version | jq -r '.major + "." + (.minor | rtrimstr("+"))')
  [ "$got" = "$MINOR" ] || { echo "cluster runs Kubernetes $got, want $MINOR (delete the old cluster: make demo-down)" >&2; return 1; }
}

# #3: gating since the api-usage collector judges authorship (managedFields,
# last-applied) instead of counting every object a deprecated endpoint returns.
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

# The knowledge base's horizon, from the binary under test: the newest
# minor it knows. A target past it cannot be fully judged.
HORIZON=""
read_kb_horizon() {
  HORIZON=$("$UPGRADESCOPE" version --output json | jq -r .kbHorizon) || return 1
  [[ $HORIZON =~ ^1\.[0-9]+$ ]] || { echo "upgradescope version --output json reports kbHorizon '$HORIZON'" >&2; return 1; }
  echo "knowledge base horizon $HORIZON; this run's next minor $NEXT"
}
past_horizon() { [ "${NEXT#1.}" -gt "${HORIZON#1.}" ]; }
# within_horizon <name>: a past-horizon check this run cannot exercise. N/A
# below the matrix's newest minor; on the newest one a warning, since then
# no minor of hack/kind-node-images.txt runs it (the KB horizon caught up
# with kind: add the next minor's node image).
within_horizon() {
  local newest
  newest=$(hack/kind-images.sh matrix all | jq -r 'max_by(ltrimstr("1.") | tonumber)') || return 1
  if [ "$MINOR" = "$newest" ]; then
    skip "$1" "$NEXT is within the KB horizon $HORIZON on the newest minor of hack/kind-node-images.txt, so no minor runs this check; add a kind node image for $NEXT"
  else
    not_applicable "$1" "$NEXT is within the KB horizon $HORIZON"
  fi
}

# VS-11 (#130): on the newest minor, the next one is past the horizon. The
# vanilla cluster has no blocker there, but kb-coverage, a required check,
# cannot be assessed: the verdict is unknown and the gate exits 2, while
# --allow-incomplete gates on findings alone and exits 0. Runs before the
# deprecated-API apply and the EOL add-on, which are blockers at that target.
past_horizon_is_unknown() {
  local rc=0
  "$UPGRADESCOPE" scan --context "$CTX" --target "$NEXT" --output json >"$work/horizon.json" || rc=$?
  [ "$rc" = 2 ] || {
    echo "scan --target $NEXT (past the KB horizon $HORIZON) exited $rc, want 2 (verdict unknown)" >&2
    return 1
  }
  jq -e '.verdict == "unknown" and .ready == false
      and ([.findings[] | select(.severity == "blocker")] | length == 0)
      and any((.notAssessed // [])[]; .capability == "kb-coverage" and .required == true)' "$work/horizon.json" >/dev/null || {
    echo "scan --target $NEXT: want verdict unknown, no blocker and a required kb-coverage gap; got:" >&2
    jq -c '{verdict, ready, blockers: [.findings[] | select(.severity == "blocker") | .title], notAssessed}' "$work/horizon.json" >&2
    return 1
  }
  rc=0
  "$UPGRADESCOPE" scan --context "$CTX" --target "$NEXT" --allow-incomplete --output json >"$work/horizon-allow.json" || rc=$?
  [ "$rc" = 0 ] || {
    echo "scan --target $NEXT --allow-incomplete exited $rc, want 0 (no blocker; the gap is accepted)" >&2
    return 1
  }
}

# The ITs skip rather than fail when their guard refuses a context, and
# `-run Integration` matching nothing (a renamed test) passes too: a green
# `go test` alone proves nothing, so every IT must report PASS by name.
ITS="TestScanIntegration_KindEOLIngressNginx TestAgentIntegration_CRDStatusOnKind"
# scan's exit code on a live cluster it cannot reach: 1 (an error), never a
# verdict.
unreachable_scan_exits_1() {
  cat >"$work/unreachable.kubeconfig" <<'EOF'
apiVersion: v1
kind: Config
clusters:
  - name: unreachable
    cluster:
      server: https://127.0.0.1:1
users:
  - name: unreachable
    user:
      token: e2e
contexts:
  - name: unreachable
    context:
      cluster: unreachable
      user: unreachable
current-context: unreachable
EOF
  local rc=0
  "$UPGRADESCOPE" scan --kubeconfig "$work/unreachable.kubeconfig" --target "$NEXT" --output json \
    >"$work/unreachable.json" 2>"$work/unreachable.err" || rc=$?
  [ "$rc" = 1 ] || {
    echo "scan against an unreachable API server exited $rc, want 1 (an error, not a verdict)" >&2
    sed 's/^/  /' "$work/unreachable.json" "$work/unreachable.err" >&2
    return 1
  }
}

# apply_fixture <apiVersion> <applied-via>: this minor's manifest through
# that version, server-side under FIELD_MANAGER (so managedFields name it).
apply_fixture() {
  sed -e "s|__API_VERSION__|$1|" -e "s|__APPLIED_VIA__|$2|" "hack/e2e/fixtures/$DEP_MANIFEST" |
    k apply --server-side --field-manager="$FIELD_MANAGER" -f -
}

# The object's refs in a scan report, one "<finding key> <manager>" each.
refs_to_object() {
  jq -r --arg n "$DEP_OBJ" '.findings[] | .key as $k | .objects[]? | select(.name == $n) | "\($k) \(.manager // "")"' "$1"
}

# API-01: authorship, not residency. The apiserver serves every object at
# every served version, so only the managedFields entry of whoever wrote it
# through the deprecated version can tell; once its manager re-applies it
# through GA, nothing is left to migrate.
deprecated_object_reported() {
  local served
  served=$(k api-versions) || return 1 # not piped into grep -q: SIGPIPE under pipefail
  grep -qxF "$DEP_GV" <<<"$served" ||
    { echo "$DEP_GV is not served: the runtimeConfig in hack/e2e/kind-config.yaml did not take" >&2; return 1; }
  apply_fixture "$DEP_GV" deprecated || return 1
  "$UPGRADESCOPE" scan --context "$CTX" --target "$NEXT" --output json --fail-on never >"$work/deprecated.json" ||
    { echo "scan failed" >&2; return 1; }
  local refs
  refs=$(refs_to_object "$work/deprecated.json") || return 1
  echo "after the $DEP_GV apply: ${refs:-no finding lists $DEP_OBJ}"
  awk -v gv="/$DEP_GV/" -v m="$FIELD_MANAGER" 'index($1, gv) && $2 == m { f = 1 } END { exit !f }' <<<"$refs" ||
    { echo "no finding lists $DEP_OBJ at $DEP_GV with manager $FIELD_MANAGER" >&2; return 1; }
  apply_fixture "$GA_GV" ga || return 1
  "$UPGRADESCOPE" scan --context "$CTX" --target "$NEXT" --output json --fail-on never >"$work/ga.json" ||
    { echo "scan failed" >&2; return 1; }
  refs=$(refs_to_object "$work/ga.json") || return 1
  [ -z "$refs" ] || { echo "$DEP_OBJ, re-applied through $GA_GV, is still reported:" >&2; echo "$refs" | sed 's/^/  /' >&2; return 1; }
}

# The demo add-on, installed from the upstream chart: scan gates on it.
eol_ingress_nginx_blocks() {
  local rc=0
  "$UPGRADESCOPE" scan --context "$CTX" --target "$NEXT" --output json >"$work/eol.json" || rc=$?
  [ "$rc" = 2 ] || { echo "scan exited $rc with an EOL add-on installed, want 2 (gate failed)" >&2; return 1; }
  jq -e '[.findings[] | select(.category == "eol-addon" and .severity == "blocker"
      and ((.namespaces // []) | index("ingress-nginx")))] | length > 0' "$work/eol.json" >/dev/null ||
    { echo "no eol-addon blocker in namespace ingress-nginx" >&2; return 1; }
}

# helm uninstall --keep-history keeps the release's Secrets (status
# uninstalled) but deletes what it ran: nothing to report. Scaled to zero
# with an IngressClass of its own, it shares nothing with the demo release.
keep_history_not_reported() {
  # A local re-run finds last run's kept history; purge it first.
  h uninstall "$KEPT_RELEASE" --namespace "$KEPT_NS" --ignore-not-found >/dev/null || return 1
  h install "$KEPT_RELEASE" ingress-nginx/ingress-nginx --version 4.7.1 --namespace "$KEPT_NS" --create-namespace \
    --set controller.replicaCount=0 --set controller.admissionWebhooks.enabled=false \
    --set controller.service.type=ClusterIP --set controller.ingressClassResource.name="$KEPT_RELEASE" \
    --set controller.ingressClassResource.controllerValue="k8s.io/$KEPT_RELEASE" || return 1
  h uninstall "$KEPT_RELEASE" --namespace "$KEPT_NS" --keep-history --wait --timeout 5m || return 1
  local status
  status=$(h history "$KEPT_RELEASE" --namespace "$KEPT_NS" -o json | jq -r 'last.status') || return 1
  [ "$status" = uninstalled ] || { echo "$KEPT_RELEASE's last revision is $status, not uninstalled" >&2; return 1; }
  "$UPGRADESCOPE" scan --context "$CTX" --target "$NEXT" --output json --fail-on never >"$work/kept.json" ||
    { echo "scan failed" >&2; return 1; }
  # The same scan still sees the installed release: the check is not blind.
  jq -e '[.findings[] | select(.category == "eol-addon") | (.namespaces // [])[]] | index("ingress-nginx")' \
    "$work/kept.json" >/dev/null || { echo "no eol-addon finding for the installed ingress-nginx either" >&2; return 1; }
  ! jq -e --arg ns "$KEPT_NS" '[.findings[] | select(.category == "eol-addon") | (.namespaces // [])[]] | index($ns)' \
    "$work/kept.json" >/dev/null || { echo "eol-addon finding in $KEPT_NS, whose release is uninstalled" >&2; return 1; }
}

# #129: each install of an add-on is judged on its own. hack/e2e/istio-teams.yaml
# runs one Istio pod per release line, each in a namespace labelled for a
# team of its own: "<cycle> <namespace> <team>". Every <category>/istio/<cycle>
# finding must name only that line's namespace and team, and 1.28 (EOL
# since 2026-07-01) must block. What 1.30 and 1.31 report depends on the
# date (EOL approaching or not), so only where they point is checked.
ISTIO_LINES='1.28 e2e-istio-old e2e-team-old
1.30 e2e-istio-mid e2e-team-mid
1.31 e2e-istio-new e2e-team-new'
istio_lines_judged_per_install() {
  # A pod in a namespace created by the same apply can be refused until the
  # namespace's default ServiceAccount exists; apply is idempotent, so retry.
  local i
  for i in $(seq 1 15); do
    k apply -f hack/e2e/istio-teams.yaml && break
    [ "$i" -lt 15 ] || { echo "kubectl apply -f hack/e2e/istio-teams.yaml failed 15 times" >&2; return 1; }
    nap 2
  done
  "$UPGRADESCOPE" scan --context "$CTX" --target "$NEXT" --output json --fail-on never >"$work/istio.json" ||
    { echo "scan failed" >&2; return 1; }
  local want bad
  want=$(jq -Rn '[inputs | split(" ") | {key: .[0], value: {namespaces: [.[1]], teams: [.[2]]}}] | from_entries' \
    <<<"$ISTIO_LINES") || return 1
  jq -r '.findings[] | select((.key // "") | test("^[^/]+/istio/")) |
    "  \(.severity) \(.key) ns=\(.namespaces // [] | tojson) teams=\(.teams // [] | tojson)"' "$work/istio.json" || return 1
  bad=$(jq -r --argjson want "$want" '.findings[] | select((.key // "") | test("^[^/]+/istio/[^/]+$"))
      | (.key | split("/")[2]) as $c
      | select({namespaces: (.namespaces // []), teams: (.teams // [])} != $want[$c])
      | "\(.key) ns=\(.namespaces // [] | tojson) teams=\(.teams // [] | tojson)"' "$work/istio.json") || return 1
  [ -z "$bad" ] || {
    echo "Istio findings that do not name exactly their own release line's namespace and team:" >&2
    echo "$bad" | sed 's/^/  /' >&2
    return 1
  }
  jq -e --argjson want "$want" '[.findings[] | select(.key == "eol-addon/istio/1.28" and .severity == "blocker"
      and {namespaces, teams} == $want["1.28"])] | length == 1' "$work/istio.json" >/dev/null ||
    { echo "no eol-addon/istio/1.28 blocker naming only e2e-istio-old and e2e-team-old" >&2; return 1; }
  # Later gates (the CR verdict, the agent IT) prove ingress-nginx is found
  # on the agent's path by its eol-addon blocker; 1.28's would satisfy them.
  k delete -f hack/e2e/istio-teams.yaml --ignore-not-found --wait --timeout 2m ||
    { echo "could not delete hack/e2e/istio-teams.yaml; later gates would see its Istio blockers" >&2; return 1; }
}

integration_tests() {
  UPGRADESCOPE_IT=1 UPGRADESCOPE_IT_CONTEXT="$CTX" go test ./internal/cli/ -run Integration -count=1 -v | tee "$work/it.log" ||
    return 1
  local t ok=0
  for t in $ITS; do
    grep -q -- "^--- PASS: $t " "$work/it.log" || { echo "$t did not PASS (skipped, renamed or not run)" >&2; ok=1; }
  done
  ! grep -- "^--- SKIP: " "$work/it.log" >&2 || { echo "an integration test skipped" >&2; ok=1; }
  return "$ok"
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

webhook_configs() { k get validatingwebhookconfigurations,mutatingwebhookconfigurations -o name; }

webhooks_before=""
install_chart() {
  webhooks_before=$(webhook_configs) || return 1
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

# VS-11 (#130): the agent's default target is the next minor, so on the
# newest minor its ClusterReadiness is judged past the horizon and says so:
# a kb-coverage entry in status.notAssessed, named required in the Ready
# condition. Its verdict is blocked, not unknown (cr_has_verdict): the EOL
# ingress-nginx is a blocker here, and a blocker outranks a gap;
# cr_unknown_once_blockers_accepted shows the same CR unknown without one.
cr_reports_kb_coverage_gap() {
  k get clusterreadiness "$CR" -o json >"$work/cr.json" || return 1
  jq -e --arg t "$NEXT" '.status.targets[0].target == $t
      and any((.status.notAssessed // [])[]; startswith("kb-coverage: "))
      and any((.status.conditions // [])[]; .type == "Ready" and (.message | contains("kb-coverage (required)")))' \
    "$work/cr.json" >/dev/null || {
    echo "clusterreadiness/$CR for $NEXT (past the KB horizon $HORIZON) does not name a required kb-coverage gap:" >&2
    jq -c '.status | {target: .targets[0].target, notAssessed, conditions}' "$work/cr.json" >&2
    return 1
  }
}

# VS-11 (#130): with nothing blocking, the default ClusterReadiness past the
# horizon is unknown. Its blockers here are the demo add-on's, in
# $ACCEPTED_NS (EOL, and a chart judged at a minor past the horizon), so
# spec.ignore accepts each blocker category it lists, narrowed to that
# namespace; the agent applies the rules on its next tick, leaving the
# kb-coverage gap alone: verdict unknown, no blocker, Ready
# Unknown/NotAssessed naming it. A blocker elsewhere is not accepted and
# fails the gate. The rules are removed again before the gate judges, so
# the later gates see the CR as the install left it.
ACCEPTED_NS=ingress-nginx
cr_unknown_once_blockers_accepted() {
  k get clusterreadiness "$CR" -o json >"$work/cr.json" || return 1
  local cats patch gen i
  cats=$(jq -c '[.status.targets[0].topFindings[]? | select(.severity == "blocker") | .category] | unique' "$work/cr.json") ||
    return 1
  [ "$cats" != "[]" ] || { echo "clusterreadiness/$CR lists no blocker to accept (cr_has_verdict saw it blocked)" >&2; return 1; }
  patch=$(jq -c --arg ns "$ACCEPTED_NS" '{spec: {ignore: [.[] | {category: ., namespace: $ns,
      reason: "e2e: accepted to observe the kb-coverage gap alone"}]}}' <<<"$cats") || return 1
  echo "accepting blocker categories $cats in namespace $ACCEPTED_NS"
  k patch clusterreadiness "$CR" --type merge -p "$patch" || return 1
  gen=$(k get clusterreadiness "$CR" -o jsonpath='{.metadata.generation}') || return 1
  [[ $gen =~ ^[0-9]+$ ]] || { echo "clusterreadiness/$CR metadata.generation is '$gen'" >&2; return 1; }
  # agent.interval=1m: the next tick evaluates the new generation.
  for i in $(seq 1 36); do
    k get clusterreadiness "$CR" -o json >"$work/cr-accepted.json" || return 1
    jq -e --argjson g "$gen" '(.status.observedGeneration // 0) >= $g' "$work/cr-accepted.json" >/dev/null && break
    nap 5
  done
  k patch clusterreadiness "$CR" --type json -p '[{"op":"remove","path":"/spec/ignore"}]' ||
    { echo "could not remove the spec.ignore rules from clusterreadiness/$CR" >&2; return 1; }
  jq -e --argjson g "$gen" --arg t "$NEXT" '.status as $s | $s.targets[0] as $t0
      | ($s.observedGeneration // 0) >= $g
      and $t0.target == $t and $t0.verdict == "unknown" and $t0.ready == false
      and $t0.blockers == 0 and ($t0.suppressed // 0) > 0
      and any(($s.notAssessed // [])[]; startswith("kb-coverage: "))
      and any(($s.conditions // [])[]; .type == "Ready" and .status == "Unknown" and .reason == "NotAssessed"
        and (.message | contains("kb-coverage (required)")))' "$work/cr-accepted.json" >/dev/null || {
    echo "clusterreadiness/$CR for $NEXT, its blockers in $ACCEPTED_NS accepted (generation $gen): want verdict unknown, no blocker and Ready Unknown/NotAssessed naming kb-coverage (required); got:" >&2
    jq -c '.status | {observedGeneration, target: .targets[0], notAssessed, conditions}' "$work/cr-accepted.json" >&2
    return 1
  }
}

# RB-04: nothing in the cluster changes because of a finding. With the EOL
# blocker installed and the agent ticking, the install added no admission
# webhook, and the agent's ClusterReadiness holds nothing up on deletion
# and belongs to nothing (the audit gates cover the agent's writes).
no_webhooks_or_finalizers() {
  local now added
  now=$(webhook_configs) || return 1
  added=$(comm -13 <(sort <<<"$webhooks_before") <(sort <<<"$now"))
  [ -z "$added" ] || { echo "webhook configurations added by the install:" >&2; echo "$added" | sed 's/^/  /' >&2; return 1; }
  k get clusterreadiness "$CR" -o json >"$work/cr.json" || return 1
  jq -e '(.metadata.finalizers // []) == [] and (.metadata.ownerReferences // []) == []' "$work/cr.json" >/dev/null || {
    echo "clusterreadiness/$CR has finalizers or owner references:" >&2
    jq -c '.metadata | {finalizers, ownerReferences}' "$work/cr.json" >&2
    return 1
  }
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
  grep -q '"ok"' <<<"$(curl -fsS http://127.0.0.1:18080/healthz)" || { echo "/healthz is not ok" >&2; return 1; }
  kill "$pf_pid" 2>/dev/null || true
  pf_pid=""
}

# #41: the chart never renders the ClusterReadiness; agent.targets reaches the
# agent as --targets, and the restarted agent reconciles spec.targets, so poll.
upgrade_with_targets() {
  # --set-string: `--set agent.targets={1.30}` would render the float 1.3.
  h upgrade "$RELEASE" deploy/chart --namespace "$NS" --reuse-values \
    --set-string "agent.targets={$NEXT}" --wait --timeout 5m || return 1
  local i
  for i in $(seq 1 60); do
    k get clusterreadiness "$CR" -o json | jq -e --arg t "$NEXT" '.spec.targets | index($t) != null' >/dev/null && return 0
    nap 2
  done
  echo "clusterreadiness/$CR spec.targets does not contain $NEXT 120s after the upgrade" >&2
  return 1
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

# --- audit log (step 9) ---------------------------------------------------
# Who is who in the log: `scan` runs as the kind admin with client-go's
# default user agent, "upgradescope/<version> (...)"; the agent is the
# chart's ServiceAccount. The ITs run in-process as cli.test and are
# neither (the agent IT creates and deletes its own CRD and CR).
JQ_ACTORS='
  def actor:
    if .user.username == $sa then "agent"
    elif ((.userAgent // "") | startswith("upgradescope/"))
      and ((.user.username // "") | startswith("system:serviceaccount:") | not) then "scan"
    else empty end;
  def write: .verb | IN("create", "update", "patch", "delete", "deletecollection");
  def gv: ((.objectRef.apiGroup // "") | if . == "" then "" else . + "/" end) + (.objectRef.apiVersion // "");
  def show: "\(actor) \(.verb) \(.requestURI) -> \(.responseStatus.code // "?")";
'
# audit <jq filter>: the filter over every event of this run's audit log,
# with JQ_ACTORS and $sa, $cr, $crd, $dep and $obj bound.
audit() {
  if [ ! -s "$work/audit.log" ]; then
    docker exec "$CLUSTER-control-plane" sh -c "cat $AUDIT_LOGS/audit*.log" >"$work/audit.log" || return 1
    [ -s "$work/audit.log" ] || { echo "the API server's audit log is empty (hack/e2e/kind-config.yaml not applied?)" >&2; return 1; }
    echo "audit log: $(wc -l <"$work/audit.log" | tr -d ' ') events" >&2
  fi
  jq -r --arg sa "$AGENT_SA" --arg cr "$CR" --arg crd "$CRD" --arg dep "$DEP_GV" --arg obj "$DEP_OBJ" \
    "$JQ_ACTORS $1" "$work/audit.log"
}

# count <jq filter>: how many events it selects.
count() {
  local out
  out=$(audit "$1 | 1") || return 1
  grep -c . <<<"$out" || true
}

# Every assertion below would pass on a log that never saw the actor, so
# each first proves the log holds that actor's traffic.
audit_saw_both() {
  local a n
  for a in scan agent; do
    n=$(count "select(actor == \"$a\")") || return 1
    [ "$n" -gt 0 ] || { echo "no request from $a in the audit log: the assertions below would be vacuous" >&2; return 1; }
    echo "$a: $n requests"
  done
}

audit_no_deprecated_requests() {
  audit_saw_both || return 1
  # The positive control: deprecated_object_reported applied the fixture
  # through $DEP_GV, so the log must carry that request annotated. If the
  # API server stopped recording k8s.io/deprecated (policy level, renamed
  # annotation), zero deprecated requests below would prove nothing.
  local n
  n=$(count 'select(gv == $dep and .objectRef.name == $obj and .annotations["k8s.io/deprecated"] == "true")') || return 1
  [ "$n" -gt 0 ] || {
    echo "the $DEP_GV apply of $DEP_OBJ is not annotated k8s.io/deprecated in the audit log: a clean result would be vacuous" >&2
    return 1
  }
  echo "positive control: $n request(s) through $DEP_GV annotated k8s.io/deprecated"
  local got
  got=$(audit 'select(.annotations["k8s.io/deprecated"] == "true") | select(actor) | "\(gv) \(.objectRef.resource) \(actor) \(.verb) \(.requestURI)"' |
    sort -u) || return 1
  local bad gv res rest
  bad=$(while read -r gv res rest; do
    [ -n "$gv" ] || continue
    grep -qxF "$gv $res" <<<"$allowed" || echo "$gv $res $rest"
  done <<<"$got")
  [ -z "$got" ] || { echo "deprecated-API requests seen:"; echo "$got" | awk '{print "  " $1 " " $2 " (" $3 ")"}' | sort -u; }
  [ -z "$bad" ] || {
    echo "deprecated-API requests from upgradescope not in $ALLOWLIST:" >&2
    echo "$bad" | sed 's/^/  /' >&2
    return 1
  }
}

audit_agent_writes_only_its_cr() {
  audit_saw_both || return 1
  local n bad
  n=$(count 'select(actor == "agent" and write and .objectRef.resource == "clusterreadinesses")') || return 1
  [ "$n" -gt 0 ] || { echo "the agent never wrote its ClusterReadiness: the write set is not observed" >&2; return 1; }
  bad=$(audit 'select(actor == "agent" and write) | select(
      ((.objectRef.apiGroup == "upgradescope.dev" and .objectRef.resource == "clusterreadinesses"
        and ((.objectRef.subresource // "") | IN("", "status"))
        and (.verb | IN("create", "update", "patch"))
        and ((.objectRef.name // "") == $cr or (.verb == "create" and (.objectRef.name // "") == "")))
       or (.objectRef.apiGroup == "apiextensions.k8s.io" and .objectRef.resource == "customresourcedefinitions"
        and .objectRef.name == $crd and (.verb | IN("create", "update", "patch")))) | not) | show') || return 1
  echo "agent writes: $n to clusterreadinesses/$CR"
  [ -z "$bad" ] || { echo "the agent wrote something other than ClusterReadiness/$CR and $CRD:" >&2; echo "$bad" | sed 's/^/  /' >&2; return 1; }
}

audit_secrets_helm_only() {
  audit_saw_both || return 1
  local a n bad
  for a in scan agent; do
    n=$(count "select(actor == \"$a\" and .verb == \"list\" and .objectRef.resource == \"secrets\")") || return 1
    [ "$n" -gt 0 ] || { echo "$a never listed Secrets: Helm detection is not observed" >&2; return 1; }
  done
  bad=$(audit 'select(actor and .objectRef.resource == "secrets" and (.objectRef.apiGroup // "") == "") | select(
      (.verb == "list" and (.requestURI | test("[?&]labelSelector=owner(%3D|=)helm(&|$)")))
      or (.verb == "get" and ((.objectRef.name // "") | startswith("sh.helm.release.v1."))) | not) | show') || return 1
  [ -z "$bad" ] || { echo "Secret requests other than Helm's owner=helm list and release GETs:" >&2; echo "$bad" | sed 's/^/  /' >&2; return 1; }
}

audit_scan_writes_nothing() {
  audit_saw_both || return 1
  local bad
  bad=$(audit 'select(actor == "scan" and write) | show') || return 1
  [ -z "$bad" ] || { echo "upgradescope scan wrote to the cluster:" >&2; echo "$bad" | sed 's/^/  /' >&2; return 1; }
}

gate "kind cluster on Kubernetes $MINOR" create_cluster
gate "build bin/upgradescope" make build
gate "the binary under test reports its knowledge base horizon" read_kb_horizon
gate "scan against an unreachable API server exits 1" unreachable_scan_exits_1
# A reused cluster (a local re-run) already has the demo add-on, the CRD and
# whatever the last run left, so it is not the vanilla cluster this guards.
vanilla="vanilla $MINOR cluster scanned at $NEXT has zero removed-api blockers"
if [ -n "$reused" ]; then
  skip "$vanilla" "cluster reused, not vanilla; make demo-down first"
else
  gate "$vanilla" no_removed_api_blockers
fi
horizon_gate="a vanilla cluster scanned past the KB horizon is unknown with a required kb-coverage gap (exit 2), and --allow-incomplete exits 0"
if ! past_horizon; then
  within_horizon "$horizon_gate"
elif [ -n "$reused" ]; then
  skip "$horizon_gate" "cluster reused, not vanilla; make demo-down first"
else
  gate "$horizon_gate" past_horizon_is_unknown
fi
deprecated="an object written through $DEP_GV is reported with its manager; re-applied through $GA_GV it is not"
if [ -n "$reused" ]; then
  skip "$deprecated" "cluster reused, its kind config may not serve $DEP_GV; make demo-down first"
else
  gate "$deprecated" deprecated_object_reported
fi
gate "EOL ingress-nginx demo add-on" env KIND_NODE_IMAGE="$NODE_IMAGE" hack/demo/kind-setup.sh
gate "scan reports the EOL ingress-nginx installed from the upstream chart as a blocker and exits 2" eol_ingress_nginx_blocks
gate "a Helm release uninstalled with --keep-history yields no EOL finding" keep_history_not_reported
gate "Istio on three release lines in three teams' namespaces: each finding names only its own line's; 1.28 blocks" istio_lines_judged_per_install
gate "scan + agent integration tests" integration_tests
gate "image built from this tree, kind-loaded" load_image
gate "helm install deploy/chart (server enabled) --wait" install_chart
gate "ClusterReadiness has a score and a blocked verdict for $NEXT" cr_has_verdict
cr_gap_gate="ClusterReadiness for the default target past the KB horizon names the required kb-coverage gap"
cr_unknown_gate="ClusterReadiness for the default target past the KB horizon, its blockers accepted, is unknown with Ready Unknown/NotAssessed"
if past_horizon; then
  gate "$cr_gap_gate" cr_reports_kb_coverage_gap
  gate "$cr_unknown_gate" cr_unknown_once_blockers_accepted
else
  within_horizon "$cr_gap_gate"
  within_horizon "$cr_unknown_gate"
fi
gate "the install added no webhook configuration; ClusterReadiness/$CR has no finalizer or owner reference" no_webhooks_or_finalizers
gate "server ingested the agent's snapshot" server_ingested
gate "helm upgrade --set agent.targets={$NEXT}" upgrade_with_targets
gate "helm uninstall leaves no ClusterRole/ClusterRoleBinding or release object; CRD kept" uninstall_leaves_nothing
audit_checks=(
  "audit: scan and the agent made no deprecated-API request outside the allowlist|audit_no_deprecated_requests"
  "audit: the agent wrote only ClusterReadiness/$CR (+ status) and the $CRD CRD|audit_agent_writes_only_its_cr"
  "audit: Secrets were read only through Helm's owner=helm list and release GETs|audit_secrets_helm_only"
  "audit: scan wrote nothing|audit_scan_writes_nothing"
)
for c in "${audit_checks[@]}"; do
  if [ -n "$reused" ]; then
    skip "${c%|*}" "cluster reused, its audit log is not this run's; make demo-down first"
  else
    gate "${c%|*}" "${c#*|}"
  fi
done
echo "e2e $MINOR: OK"
