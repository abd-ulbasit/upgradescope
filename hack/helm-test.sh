#!/usr/bin/env bash
# The chart gate (make helm-test; CI's helm job). No cluster needed:
#   1. helm lint --strict, default and server-enabled values;
#   2. every documented values combination renders (the matrix below);
#   3. each render validates against the Kubernetes OpenAPI schemas of the
#      oldest and newest minor the kube job tests, with a pinned,
#      checksum-verified kubeconform (hack/install-tool.sh) in strict mode,
#      so an unknown or misplaced field fails. The CRD (crds/) is not in the
#      renders: the schema set has no CustomResourceDefinition schema, and
#      the CRD is held to internal/crd/manifest.yaml by test-chart.sh and
#      applied to a real apiserver by the kube job;
#   4. hack/test-chart.sh, the chart's contract assertions (RBAC, image tag,
#      required tokens, ...).
# kubeconform reads its schemas from github.com/yannh/kubernetes-json-schema,
# so step 3 needs network access (cached under bin/tools/kubeconform-cache).
set -euo pipefail
cd "$(dirname "$0")/.."
CHART=deploy/chart

command -v helm >/dev/null || { echo "ERROR: helm not found in PATH" >&2; exit 1; }
kubeconform=$(hack/install-tool.sh kubeconform)
cache=bin/tools/kubeconform-cache
mkdir -p "$cache"

# The ends of the kube job's version range (hack/kind-node-images.txt).
KUBE_VERSIONS=${KUBE_VERSIONS:-1.29.0 1.37.0}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

echo "== helm lint --strict"
helm lint --strict "$CHART"
helm lint --strict "$CHART" --set server.enabled=true --set server.ingestToken=t

# validate <file>: strict schema validation at every KUBE_VERSIONS minor.
# ClusterReadiness is our own CR: kubeconform has no schema for it, and the
# CRD's structural schema is enforced by a real apiserver in the kube job.
validate() {
  local v
  for v in $KUBE_VERSIONS; do
    "$kubeconform" -strict -summary -cache "$cache" -kubernetes-version "$v" \
      -skip ClusterReadiness "$1"
  done
}

echo "== kubeconform fails closed on an invalid manifest"
cat >"$work/invalid.yaml" <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: broken
spec:
  replicas: "two"
  selector: {matchLabels: {app: x}}
  template:
    metadata: {labels: {app: x}}
    spec:
      containers: [{name: x, image: x, notAField: true}]
EOF
if validate "$work/invalid.yaml" >"$work/invalid.out" 2>&1; then
  cat "$work/invalid.out" >&2
  echo "FAIL: kubeconform accepted an invalid Deployment; schema validation is not running" >&2
  exit 1
fi
echo "  ok: invalid Deployment rejected"

echo "== helm template (values matrix) + kubeconform"
while IFS='|' read -r name args; do
  [ -n "$name" ] || continue
  echo "-- $name ($args)"
  # shellcheck disable=SC2086 # args is a deliberate word list
  helm template upgradescope "$CHART" --namespace upgradescope $args >"$work/$name.yaml"
  validate "$work/$name.yaml"
done <<'EOF'
default|
server|--set server.enabled=true --set server.ingestToken=t
server-readtoken|--set server.enabled=true --set server.ingestToken=t --set server.readToken=r
server-existing-secret|--set server.enabled=true --set server.existingSecret=s --set agent.existingSecret=s
server-no-persistence|--set server.enabled=true --set server.ingestToken=t --set server.persistence.enabled=false
server-targets-notify|--set server.enabled=true --set server.ingestToken=t --set server.targets={1.37} --set server.webhook=https://hooks.example.com/x
external-server|--set agent.serverUrl=https://uscope.example.com --set agent.existingSecret=s
targets|--set agent.targets={1.37,1.38}
byo-serviceaccount|--set serviceAccount.create=false --set serviceAccount.name=sa --set rbac.create=false
image-override|--set image.tag=dev
hub|--set agent.enabled=false --set server.enabled=true --set server.ingestToken=t --set server.adminToken=a
hub-postgres|--set agent.enabled=false --set server.enabled=true --set server.existingSecret=s --set server.readTokenFromSecret=true --set server.adminTokenFromSecret=true --set server.database.existingSecret=pg --set server.replicas=2
hub-ingress|--set agent.enabled=false --set server.enabled=true --set server.ingestToken=t --set server.readToken=r --set server.ingress.enabled=true --set server.ingress.host=uscope.example.com --set server.ingress.className=nginx
hub-teams|--set agent.enabled=false --set server.enabled=true --set server.ingestToken=t --set server.teamMap[0].pattern=payments-* --set server.teamMap[0].team=payments
hub-netpol|--set agent.enabled=false --set server.enabled=true --set server.ingestToken=t --set networkPolicy.enabled=true --set networkPolicy.serverIngressFrom[0].namespaceSelector.matchLabels.team=ingress
EOF

echo "== chart contract (hack/test-chart.sh)"
hack/test-chart.sh
echo "helm-test: OK"
