#!/usr/bin/env bash
# Creates the upgradescope demo kind cluster and installs an EOL add-on
# (ingress-nginx chart 4.7.1 → controller v1.8.x) so `upgradescope scan`
# has a real blocker to find.
#
# Usage: ./hack/demo/kind-setup.sh
#        KIND_NODE_IMAGE=$(hack/kind-images.sh image 1.31) ./hack/demo/kind-setup.sh
#        (pins the cluster's Kubernetes version; default: kind's own default
#        node image. Only used when the cluster is created, not reused.)
# Teardown: kind delete cluster --name upgradescope-demo  (or `make demo-down`)
set -euo pipefail

CLUSTER=upgradescope-demo
NS=ingress-nginx
CHART_VERSION=4.7.1 # old chart, controller v1.8.x — already EOL upstream

for tool in kind helm kubectl; do
  command -v "$tool" >/dev/null || { echo "ERROR: $tool not found in PATH" >&2; exit 1; }
done

if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  echo "kind cluster '$CLUSTER' already exists, reusing"
else
  kind create cluster --name "$CLUSTER" --wait 120s ${KIND_NODE_IMAGE:+--image "$KIND_NODE_IMAGE"}
fi

# The current context is switched for the demo (scan/IT commands that follow
# use it); every command below still names the kind context explicitly.
kubectl config use-context "kind-$CLUSTER"

helm repo add ingress-nginx https://kubernetes.github.io/ingress-nginx --force-update >/dev/null
helm repo update ingress-nginx >/dev/null

# ClusterIP: kind has no LoadBalancer. Admission webhooks off: on kind the
# webhook job can keep the release from going Ready — the scan only needs
# the controller pod running so its image is visible.
helm --kube-context "kind-$CLUSTER" upgrade --install ingress-nginx ingress-nginx/ingress-nginx \
  --version "$CHART_VERSION" \
  --namespace "$NS" --create-namespace \
  --set controller.service.type=ClusterIP \
  --set controller.admissionWebhooks.enabled=false \
  --wait --timeout 5m

echo
echo "Demo cluster ready. Controller image:"
kubectl --context "kind-$CLUSTER" -n "$NS" get pods -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.containers[0].image}{"\n"}{end}'
echo
echo "Next: UPGRADESCOPE_IT=1 go test ./internal/cli/ -run Integration -v"
