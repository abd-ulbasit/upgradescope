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

# --- SERVER ASSERTIONS (H4) ---

[ "$FAILED" -eq 0 ] || { echo "chart-test: FAILED" >&2; exit 1; }
echo "chart-test: all assertions passed"
