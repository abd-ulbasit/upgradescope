#!/usr/bin/env bash
# Chart contract tests: helm lint + rendered-template grep assertions.
# No cluster needed. Run: make chart-test
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CHART="$ROOT/deploy/chart"
command -v helm >/dev/null || { echo "ERROR: helm not found in PATH" >&2; exit 1; }

FAILED=0
fail() { echo "FAIL: $*" >&2; FAILED=1; }
pass() { echo "  ok: $*"; }
# Substring assertions (fixed string, safe for leading dashes).
assert_contains()     { grep -qF -- "$2" "$1" && pass "$3" || fail "$3 (missing: $2)"; }
assert_not_contains() { grep -qF -- "$2" "$1" && fail "$3 (unexpected: $2)" || pass "$3"; }
# Exact-line assertions ('kind: Service' must not match 'kind: ServiceAccount').
assert_line()    { grep -qxF -- "$2" "$1" && pass "$3" || fail "$3 (missing line: $2)"; }
assert_no_line() { grep -qxF -- "$2" "$1" && fail "$3 (unexpected line: $2)" || pass "$3"; }

echo "== helm lint"
helm lint --strict "$CHART"

echo "== crds/ copy in sync with internal/crd/manifest.yaml"
if diff -u "$ROOT/internal/crd/manifest.yaml" "$CHART/crds/clusterreadinesses.upgradescope.dev.yaml"; then
  pass "CRD copy in sync"
else
  fail "CRD copy out of sync — run: cp internal/crd/manifest.yaml deploy/chart/crds/clusterreadinesses.upgradescope.dev.yaml"
fi

echo "== Chart.yaml version and appVersion agree"
# release.yml stamps both from the git tag (vX.Y.Z -> version X.Y.Z,
# appVersion vX.Y.Z); the committed values must follow the same rule so the
# in-repo chart never names a release that does not exist.
CHART_VERSION="$(sed -n 's/^version: *//p' "$CHART/Chart.yaml" | tr -d '"')"
APP_VERSION="$(sed -n 's/^appVersion: *//p' "$CHART/Chart.yaml" | tr -d '"')"
if [ "v$CHART_VERSION" = "$APP_VERSION" ]; then
  pass "appVersion $APP_VERSION = v + version $CHART_VERSION"
else
  fail "appVersion '$APP_VERSION' must be 'v' + version '$CHART_VERSION'"
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "== default render (agent only, CRD-only mode)"
helm template upgradescope "$CHART" --namespace upgradescope > "$TMP/default.yaml"

echo "== server-enabled render (combined single-cluster install)"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set server.enabled=true --set server.ingestToken=test-token > "$TMP/server.yaml"

echo "== external-server render (agent pushes to remote, token from existing secret)"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set agent.serverUrl=https://uscope.example.com \
  --set agent.existingSecret=my-secret > "$TMP/external.yaml"

echo "== targets render (passed to the agent; the chart never renders the CR)"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set 'agent.targets={1.37,1.38}' > "$TMP/targets.yaml"

# --- AGENT ASSERTIONS (H3) ---
echo "== agent assertions: default render"
assert_line "$TMP/default.yaml" 'kind: ServiceAccount'     "ServiceAccount rendered"
assert_line "$TMP/default.yaml" 'kind: ClusterRole'        "ClusterRole rendered"
assert_line "$TMP/default.yaml" 'kind: ClusterRoleBinding' "ClusterRoleBinding rendered"
assert_line "$TMP/default.yaml" 'kind: Deployment'         "agent Deployment rendered"
assert_contains "$TMP/default.yaml" 'nonResourceURLs: ["/version", "/metrics"]' "version+metrics nonResourceURLs"
assert_contains "$TMP/default.yaml" 'clusterreadinesses/status'              "status subresource rule"
assert_contains "$TMP/default.yaml" 'resourceNames: ["clusterreadinesses.upgradescope.dev"]' "CRD writes scoped to our CRD"
assert_contains "$TMP/default.yaml" 'resourceNames: ["cluster"]' "CR writes scoped to agent.crName"
assert_contains "$TMP/default.yaml" 'resources: ["secrets"]' "Helm Secret read on by default (rbac.helmSecrets)"
assert_contains "$TMP/default.yaml" '--manage-crd=true' "agent manages the CRD schema by default"
assert_not_contains "$TMP/default.yaml" '"delete"'           "no delete verb anywhere"
assert_not_contains "$TMP/default.yaml" '"deletecollection"' "no deletecollection verb"
assert_not_contains "$TMP/default.yaml" '"escalate"'         "no escalate verb"
assert_not_contains "$TMP/default.yaml" '"watch"'            "no watch verb (the agent polls)"
grep -vE '^[[:space:]]*#' "$TMP/default.yaml" > "$TMP/default.nocomments.yaml"  # rules only, not their prose
assert_not_contains "$TMP/default.nocomments.yaml" '"*"'         "no wildcard group/resource/verb"
assert_not_contains "$TMP/default.nocomments.yaml" 'nodes/proxy' "no nodes/proxy (kubelet exec)"
assert_not_contains "$TMP/default.nocomments.yaml" '/*'         "no wildcard subresources"

echo "== RBAC: rbac.helmSecrets=false drops the Secret rule"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set rbac.helmSecrets=false > "$TMP/nosecrets.yaml"
assert_not_contains "$TMP/nosecrets.yaml" '"secrets"' "no Secret rule without helmSecrets"

echo "== RBAC: agent.manageCRD=false drops all CRD write access"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set agent.manageCRD=false > "$TMP/nocrd.yaml"
assert_not_contains "$TMP/nocrd.yaml" 'resourceNames: ["clusterreadinesses.upgradescope.dev"]' "no CRD write rule"
assert_contains "$TMP/nocrd.yaml" '--manage-crd=false' "agent told not to touch the CRD"

echo "== RBAC: rendered rules vs collector calls (upstream rbac Covers) and KB sync"
# deploy/chart/rbac_test.go renders the chart, then requires ALLOW for every
# call the collectors make and DENY for nodes/proxy, pods/log, pods/exec,
# foreign CRDs and CRs; it also fails when files/kb-rbac-rules.yaml drifts
# from the embedded KB.
command -v go >/dev/null || { echo "ERROR: go not found in PATH (needed for the RBAC tests)" >&2; exit 1; }
if (cd "$ROOT" && UPGRADESCOPE_CHART_TEST=1 go test ./deploy/chart/); then
  pass "go test ./deploy/chart"
else
  fail "go test ./deploy/chart (RBAC coverage)"
fi
assert_contains "$TMP/default.yaml" "image: \"ghcr.io/abd-ulbasit/upgradescope:$APP_VERSION\"" "default image tag is the chart appVersion"
assert_not_contains "$TMP/default.yaml" 'upgradescope:dev"' "default image is not the unpublished :dev"
assert_contains "$TMP/default.yaml" 'imagePullPolicy: IfNotPresent'   "pullPolicy IfNotPresent"
assert_contains "$TMP/default.yaml" 'runAsNonRoot: true'              "runAsNonRoot"
assert_contains "$TMP/default.yaml" 'readOnlyRootFilesystem: true'    "readOnlyRootFilesystem"
assert_contains "$TMP/default.yaml" 'allowPrivilegeEscalation: false' "no privilege escalation"
assert_contains "$TMP/default.yaml" 'drop: ["ALL"]'                   "all capabilities dropped"
assert_contains "$TMP/default.yaml" '--interval=10m'   "default interval flag"
assert_contains "$TMP/default.yaml" '--cr-name=cluster' "default cr-name flag"
assert_contains "$TMP/default.yaml" '--team-label=team' "default team-label flag"
assert_not_contains "$TMP/default.yaml" '--server-url' "CRD-only mode: no server-url flag"
assert_no_line "$TMP/default.yaml" 'kind: Secret'           "no token Secret in CRD-only mode"
assert_no_line "$TMP/default.yaml" 'kind: ClusterReadiness' "chart never renders the CR (agent owns it)"
assert_not_contains "$TMP/default.yaml" '--targets' "no --targets flag by default (spec.targets left to the CR)"

echo "== image.tag override (side-loaded dev image, as in agent-e2e)"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set server.enabled=true --set server.ingestToken=t \
  --set image.tag=dev > "$TMP/devtag.yaml"
assert_contains "$TMP/devtag.yaml" 'image: "ghcr.io/abd-ulbasit/upgradescope:dev"' "image.tag overrides appVersion"
assert_not_contains "$TMP/devtag.yaml" "upgradescope:$APP_VERSION\"" "no appVersion image left when image.tag is set (agent + server)"

echo "== agent assertions: external-server render"
assert_contains "$TMP/external.yaml" '--server-url=https://uscope.example.com' "explicit serverUrl wins"
assert_contains "$TMP/external.yaml" 'name: my-secret' "existingSecret referenced"
assert_no_line "$TMP/external.yaml" 'kind: Secret' "no generated Secret when existingSecret set"

echo "== agent assertions: targets render"
# The agent creates ClusterReadiness/<crName> on its first tick. Were the
# chart to render it too, 'helm upgrade --set agent.targets=...' after a
# default install would fail: Helm will not adopt the agent-created object.
assert_no_line "$TMP/targets.yaml" 'kind: ClusterReadiness' "no CR rendered even with agent.targets"
assert_contains "$TMP/targets.yaml" '- "--targets=1.37,1.38"' "targets passed to the agent"

echo "== upgrade path: setting targets after a default install changes only the agent"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set 'agent.targets={1.37}' > "$TMP/targets-upgrade.yaml"
if diff <(grep -vF -e '--targets=' -e 'checksum/' "$TMP/targets-upgrade.yaml") \
        <(grep -vF -e 'checksum/' "$TMP/default.yaml") >/dev/null; then
  pass "only the --targets arg differs from the default render"
else
  fail "agent.targets changes more than the agent's --targets arg"
fi

echo "== render must fail when pushing without any token"
if helm template upgradescope "$CHART" --set agent.serverUrl=https://x.example >/dev/null 2>&1; then
  fail "agent.serverUrl without a token should fail the required check"
else
  pass "push without token fails render"
fi

# --- SERVER ASSERTIONS (H4) ---
echo "== server assertions: default render has no server objects"
assert_no_line "$TMP/default.yaml" 'kind: Service'               "no Service when server disabled"
assert_no_line "$TMP/default.yaml" 'kind: PersistentVolumeClaim' "no PVC when server disabled"

echo "== server assertions: server-enabled render"
assert_line "$TMP/server.yaml" 'kind: Service'               "server Service rendered"
assert_line "$TMP/server.yaml" 'kind: PersistentVolumeClaim' "server PVC rendered"
assert_line "$TMP/server.yaml" 'kind: Secret'                "Secrets rendered"
assert_contains "$TMP/server.yaml" 'name: upgradescope-server' "server resources named *-server"
assert_contains "$TMP/server.yaml" 'type: Recreate' "Recreate strategy (single SQLite writer)"
assert_contains "$TMP/server.yaml" '--db=/data/upgradescope.sqlite' "db on the data volume"
assert_contains "$TMP/server.yaml" '--ingest-token=$(UPGRADESCOPE_INGEST_TOKEN)' "ingest token via env expansion"
assert_contains "$TMP/server.yaml" 'ingestToken: "test-token"' "ingest token in Secret stringData"
assert_contains "$TMP/server.yaml" 'serverToken: "test-token"' "agent token defaults to ingestToken"
assert_contains "$TMP/server.yaml" '--server-url=http://upgradescope-server.upgradescope.svc:8080' "agent points at in-chart server"
assert_contains "$TMP/server.yaml" 'path: /healthz' "healthz probes"
assert_not_contains "$TMP/server.yaml" '--read-token' "no read-token flag unless set"
# serve refuses an open read API on a non-loopback --listen; the chart's
# empty readToken default opts in explicitly (NOTES.txt warns about it).
assert_contains "$TMP/server.yaml" '--allow-anonymous-read' "empty readToken opts in to anonymous reads"

echo "== server assertions: read token set"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set server.enabled=true --set server.ingestToken=t \
  --set server.readToken=r > "$TMP/readtoken.yaml"
assert_contains "$TMP/readtoken.yaml" '--read-token=$(UPGRADESCOPE_READ_TOKEN)' "read token via env expansion"
assert_not_contains "$TMP/readtoken.yaml" '--allow-anonymous-read' "no anonymous reads with a read token"

echo "== server assertions: render fails without ingest token"
if helm template upgradescope "$CHART" --set server.enabled=true >/dev/null 2>&1; then
  fail "server.enabled without ingestToken should fail the required check"
else
  pass "server without ingestToken fails render"
fi

echo "== server assertions: emptyDir when persistence disabled"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set server.enabled=true --set server.ingestToken=t \
  --set server.persistence.enabled=false > "$TMP/nopvc.yaml"
assert_no_line "$TMP/nopvc.yaml" 'kind: PersistentVolumeClaim' "no PVC when persistence disabled"
assert_contains "$TMP/nopvc.yaml" 'emptyDir: {}' "emptyDir fallback"

[ "$FAILED" -eq 0 ] || { echo "chart-test: FAILED" >&2; exit 1; }
echo "chart-test: all assertions passed"
