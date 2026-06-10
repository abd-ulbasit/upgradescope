#!/usr/bin/env bash
# End-to-end: build image -> kind load -> helm install (agent + server) ->
# assert ClusterReadiness status, the ingress-nginx blocker, and the server API.
#
# Reuses the P1 demo cluster (hack/demo/kind-setup.sh: kind 'upgradescope-demo'
# with EOL ingress-nginx chart 4.7.1 installed).
#
# Usage: ./hack/demo/agent-e2e.sh   (or: make agent-e2e)
# Teardown: helm -n upgradescope uninstall upgradescope
#           kubectl delete crd clusterreadinesses.upgradescope.dev
#           make demo-down
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
NS=upgradescope
RELEASE=upgradescope
TOKEN=dev-token
CR=cluster

for tool in docker kind helm kubectl curl; do
  command -v "$tool" >/dev/null || { echo "ERROR: $tool not found in PATH" >&2; exit 1; }
done

fail() { echo "FAIL: $*" >&2; exit 1; }

"$ROOT/hack/demo/kind-setup.sh"
make -C "$ROOT" kind-load

# interval=1m so the e2e doesn't wait 10 minutes for the second tick; the
# first tick fires immediately either way.
helm upgrade --install "$RELEASE" "$ROOT/deploy/chart" \
  --namespace "$NS" --create-namespace \
  --set server.enabled=true \
  --set server.ingestToken="$TOKEN" \
  --set agent.interval=1m \
  --wait --timeout 3m

kubectl -n "$NS" rollout status "deploy/$RELEASE-server" --timeout=120s
kubectl -n "$NS" rollout status "deploy/$RELEASE-agent" --timeout=120s

echo "== waiting for ClusterReadiness status (first agent tick)"
SCORE=""
for _ in $(seq 1 36); do
  SCORE=$(kubectl get clusterreadiness "$CR" -o jsonpath='{.status.targets[0].score}' 2>/dev/null || true)
  [ -n "$SCORE" ] && break
  sleep 5
done
if [ -z "$SCORE" ]; then
  kubectl -n "$NS" logs "deploy/$RELEASE-agent" --tail=50 >&2 || true
  fail "no status.targets[0].score on clusterreadiness/$CR after 180s"
fi
echo "PASS: clusterreadiness/$CR has score=$SCORE"

READY=$(kubectl get clusterreadiness "$CR" -o jsonpath='{.status.targets[0].ready}')
[ "$READY" = "false" ] || fail "targets[0].ready=$READY, want false (EOL ingress-nginx blocker expected)"
echo "PASS: ready=false (blocker present)"

kubectl get clusterreadiness "$CR" -o json | grep -q '"eol-addon"' \
  || fail "no eol-addon finding in clusterreadiness status"
echo "PASS: eol-addon (ingress-nginx) finding in CRD status"

echo "== checking server API via port-forward"
kubectl -n "$NS" port-forward "svc/$RELEASE-server" 18080:8080 >/dev/null 2>&1 &
PF_PID=$!
trap 'kill "$PF_PID" 2>/dev/null || true' EXIT
sleep 3

BODY=""
for _ in $(seq 1 24); do
  BODY=$(curl -fsS http://127.0.0.1:18080/api/v1/clusters 2>/dev/null || true)
  echo "$BODY" | grep -q '"name"' && break
  sleep 5
done
echo "$BODY" | grep -q '"name"'  || fail "server /api/v1/clusters lists no cluster after 120s (push failing?): $BODY"
echo "$BODY" | grep -q '"score"' || fail "cluster summary has no score: $BODY"
echo "PASS: server lists the cluster: $BODY"

curl -fsS http://127.0.0.1:18080/healthz | grep -q '"ok"' || fail "/healthz not ok"
echo "PASS: /healthz ok"

echo
echo "agent-e2e: ALL PASS"
echo "Teardown: helm -n $NS uninstall $RELEASE && kubectl delete crd clusterreadinesses.upgradescope.dev"
