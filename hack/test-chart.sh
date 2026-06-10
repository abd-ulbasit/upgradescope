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
helm lint "$CHART"

echo "== crds/ copy in sync with internal/crd/manifest.yaml"
if diff -u "$ROOT/internal/crd/manifest.yaml" "$CHART/crds/clusterreadinesses.upgradescope.dev.yaml"; then
  pass "CRD copy in sync"
else
  fail "CRD copy out of sync — run: cp internal/crd/manifest.yaml deploy/chart/crds/clusterreadinesses.upgradescope.dev.yaml"
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

echo "== targets render (ClusterReadiness CR created by chart)"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set 'agent.targets={1.37,1.38}' > "$TMP/targets.yaml"

# --- AGENT ASSERTIONS (H3) ---
echo "== agent assertions: default render"
assert_line "$TMP/default.yaml" 'kind: ServiceAccount'     "ServiceAccount rendered"
assert_line "$TMP/default.yaml" 'kind: ClusterRole'        "ClusterRole rendered"
assert_line "$TMP/default.yaml" 'kind: ClusterRoleBinding' "ClusterRoleBinding rendered"
assert_line "$TMP/default.yaml" 'kind: Deployment'         "agent Deployment rendered"
assert_contains "$TMP/default.yaml" 'verbs: ["get", "list", "watch"]'        "broad rule is read-only"
assert_contains "$TMP/default.yaml" 'nonResourceURLs: ["/metrics", "/version"]' "metrics+version nonResourceURLs"
assert_contains "$TMP/default.yaml" 'clusterreadinesses/status'              "status subresource rule"
assert_not_contains "$TMP/default.yaml" '"delete"'           "no delete verb anywhere"
assert_not_contains "$TMP/default.yaml" '"deletecollection"' "no deletecollection verb"
assert_not_contains "$TMP/default.yaml" '"escalate"'         "no escalate verb"
assert_contains "$TMP/default.yaml" 'image: "ghcr.io/abd-ulbasit/upgradescope:dev"' "default image ref"
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
assert_no_line "$TMP/default.yaml" 'kind: ClusterReadiness' "no CR without agent.targets"

echo "== agent assertions: external-server render"
assert_contains "$TMP/external.yaml" '--server-url=https://uscope.example.com' "explicit serverUrl wins"
assert_contains "$TMP/external.yaml" 'name: my-secret' "existingSecret referenced"
assert_no_line "$TMP/external.yaml" 'kind: Secret' "no generated Secret when existingSecret set"

echo "== agent assertions: targets render"
assert_line "$TMP/targets.yaml" 'kind: ClusterReadiness' "CR rendered when agent.targets set"
assert_contains "$TMP/targets.yaml" '- "1.37"' "target 1.37 in CR spec"

echo "== render must fail when pushing without any token"
if helm template upgradescope "$CHART" --set agent.serverUrl=https://x.example >/dev/null 2>&1; then
  fail "agent.serverUrl without a token should fail the required check"
else
  pass "push without token fails render"
fi

# --- SERVER ASSERTIONS (H4) ---

[ "$FAILED" -eq 0 ] || { echo "chart-test: FAILED" >&2; exit 1; }
echo "chart-test: all assertions passed"
