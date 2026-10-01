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
# assert_env_secret FILE ENV SECRET KEY DESC: env var ENV is set from
# secretKeyRef SECRET/KEY (the lines right after "- name: ENV").
assert_env_secret() {
  local block
  block="$(grep -A5 -xE "[[:space:]]*- name: $2" "$1" || true)"
  if printf '%s\n' "$block" | grep -qxE "[[:space:]]*name: $3" &&
     printf '%s\n' "$block" | grep -qxE "[[:space:]]*key: $4"; then
    pass "$5"
  else
    fail "$5 (env $2 is not secretKeyRef $3/$4)"
  fi
}

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
assert_line "$TMP/default.yaml" '              - ALL'                "all capabilities dropped"
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
assert_env_secret "$TMP/external.yaml" UPGRADESCOPE_SERVER_TOKEN my-secret serverToken "agent token from agent.existingSecret"
assert_no_line "$TMP/external.yaml" 'kind: Secret' "no generated Secret when existingSecret set"

echo "== agent assertions: inline agent.serverToken goes into a chart Secret"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set agent.serverUrl=https://uscope.example.com --set agent.serverToken=s3cret > "$TMP/inline-agent.yaml"
assert_env_secret "$TMP/inline-agent.yaml" UPGRADESCOPE_SERVER_TOKEN upgradescope-agent-token serverToken "agent token from the chart's agent Secret"
assert_contains "$TMP/inline-agent.yaml" 'serverToken: "s3cret"' "inline token stored in the Secret"

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
assert_env_secret "$TMP/server.yaml" UPGRADESCOPE_INGEST_TOKEN upgradescope-server-tokens ingestToken "ingest token from the Secret via env"
assert_contains "$TMP/server.yaml" 'ingestToken: "test-token"' "ingest token in Secret stringData"
assert_env_secret "$TMP/server.yaml" UPGRADESCOPE_SERVER_TOKEN upgradescope-server-tokens ingestToken "agent pushes with the server's ingest token"
assert_no_line "$TMP/server.yaml" '  serverToken: "test-token"' "no copy of the ingest token in a second Secret"
assert_contains "$TMP/server.yaml" '--server-url=http://upgradescope-server.upgradescope.svc:8080' "agent points at in-chart server"
assert_contains "$TMP/server.yaml" 'path: /healthz' "healthz probes"
assert_not_contains "$TMP/server.yaml" 'UPGRADESCOPE_READ_TOKEN' "no read token unless set"
# serve refuses an open read API on a non-loopback --listen; the chart's
# empty readToken default opts in explicitly (NOTES.txt warns about it).
assert_contains "$TMP/server.yaml" '--allow-anonymous-read' "empty readToken opts in to anonymous reads"

echo "== secrets never reach argv (no \$(VAR) expansion into args)"
for f in "$TMP"/*.yaml; do
  assert_not_contains "$f" '$(UPGRADESCOPE_' "$(basename "$f"): no \$(UPGRADESCOPE_*) in args"
  assert_not_contains "$f" '--ingest-token' "$(basename "$f"): no --ingest-token arg"
  assert_not_contains "$f" '--server-token' "$(basename "$f"): no --server-token arg"
done

echo "== server assertions: read token set"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set server.enabled=true --set server.ingestToken=t \
  --set server.readToken=r > "$TMP/readtoken.yaml"
assert_env_secret "$TMP/readtoken.yaml" UPGRADESCOPE_READ_TOKEN upgradescope-server-tokens readToken "read token from the Secret via env"
assert_not_contains "$TMP/readtoken.yaml" '--read-token' "no read token in args"
assert_not_contains "$TMP/readtoken.yaml" '--allow-anonymous-read' "no anonymous reads with a read token"

echo "== server assertions: no ingest token supplied -> chart generates one (one-command install)"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set server.enabled=true > "$TMP/gentoken.yaml"
if grep -qE '^  ingestToken: "[A-Za-z0-9]{40}"$' "$TMP/gentoken.yaml"; then
  pass "random 40-char ingest token generated"
else
  fail "no generated ingestToken in the server Secret"
fi
assert_env_secret "$TMP/gentoken.yaml" UPGRADESCOPE_SERVER_TOKEN upgradescope-server-tokens ingestToken "agent uses the generated token"

echo "== server assertions: webhook URLs live only in the Secret"
SLACK='https://hooks.slack.com/services/T000/B000/XXXX'
HOOK='https://hooks.example.com/upgradescope'
helm template upgradescope "$CHART" --namespace upgradescope \
  --set server.enabled=true --set server.ingestToken=t \
  --set server.slackWebhook="$SLACK" --set server.webhook="$HOOK" > "$TMP/webhooks.yaml"
perl -0777 -ne 'print grep { !/^kind: Secret$/m } split /^---$/m' "$TMP/webhooks.yaml" > "$TMP/webhooks-nosecret.txt"
assert_contains "$TMP/webhooks.yaml" "slackWebhook: \"$SLACK\"" "Slack URL in the Secret"
assert_not_contains "$TMP/webhooks-nosecret.txt" "$SLACK" "Slack URL nowhere outside the Secret"
assert_not_contains "$TMP/webhooks-nosecret.txt" "$HOOK" "webhook URL nowhere outside the Secret"
assert_env_secret "$TMP/webhooks.yaml" UPGRADESCOPE_SLACK_WEBHOOK upgradescope-server-tokens slackWebhook "Slack URL via env"
assert_env_secret "$TMP/webhooks.yaml" UPGRADESCOPE_WEBHOOK_URL upgradescope-server-tokens webhook "webhook URL via env"

echo "== server assertions: server.existingSecret alone wires every key"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set server.enabled=true --set server.existingSecret=mysec > "$TMP/existing.yaml"
assert_no_line "$TMP/existing.yaml" 'kind: Secret' "no chart Secret with server.existingSecret"
assert_env_secret "$TMP/existing.yaml" UPGRADESCOPE_INGEST_TOKEN mysec ingestToken "server ingest token from existingSecret"
assert_env_secret "$TMP/existing.yaml" UPGRADESCOPE_SERVER_TOKEN mysec ingestToken "agent token from the same existingSecret"
assert_env_secret "$TMP/existing.yaml" UPGRADESCOPE_SLACK_WEBHOOK mysec slackWebhook "optional slackWebhook key"
assert_env_secret "$TMP/existing.yaml" UPGRADESCOPE_WEBHOOK_URL mysec webhook "optional webhook key"
assert_contains "$TMP/existing.yaml" '--allow-anonymous-read' "no read token unless asked for"

echo "== server assertions: read token from existingSecret, no inline value"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set server.enabled=true --set server.existingSecret=mysec \
  --set server.readTokenFromSecret=true > "$TMP/existing-read.yaml"
assert_env_secret "$TMP/existing-read.yaml" UPGRADESCOPE_READ_TOKEN mysec readToken "read token from existingSecret"
assert_not_contains "$TMP/existing-read.yaml" '--allow-anonymous-read' "no anonymous reads"
if helm template upgradescope "$CHART" --set server.enabled=true --set server.readTokenFromSecret=true >/dev/null 2>&1; then
  fail "readTokenFromSecret without existingSecret should fail"
else
  pass "readTokenFromSecret requires existingSecret"
fi

echo "== server assertions: emptyDir when persistence disabled"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set server.enabled=true --set server.ingestToken=t \
  --set server.persistence.enabled=false > "$TMP/nopvc.yaml"
assert_no_line "$TMP/nopvc.yaml" 'kind: PersistentVolumeClaim' "no PVC when persistence disabled"
assert_contains "$TMP/nopvc.yaml" 'emptyDir: {}' "emptyDir fallback"

# --- PRODUCTION KNOBS (#42) ---
echo "== knobs: pull secrets, scheduling, extra env/volumes/args render on both pods"
cat > "$TMP/knobs-values.yaml" <<'EOF'
imagePullSecrets:
  - name: regcred
server:
  enabled: true
  ingestToken: t
  nodeSelector: {pool: srv}
  tolerations: [{key: dedicated, operator: Exists, effect: NoSchedule}]
  affinity: {nodeAffinity: {requiredDuringSchedulingIgnoredDuringExecution: {nodeSelectorTerms: [{matchExpressions: [{key: srv-aff, operator: Exists}]}]}}}
  priorityClassName: srv-priority
  podAnnotations: {srv-ann: "1"}
  podLabels: {srv-label: "1"}
  extraArgs: ["--team-map=/etc/upgradescope/teams.yaml"]
  extraEnv: [{name: SRV_EXTRA, value: "1"}]
  extraVolumes: [{name: teams, configMap: {name: team-map}}]
  extraVolumeMounts: [{name: teams, mountPath: /etc/upgradescope}]
agent:
  nodeSelector: {pool: agent}
  tolerations: [{key: agent-taint, operator: Exists}]
  affinity: {nodeAffinity: {requiredDuringSchedulingIgnoredDuringExecution: {nodeSelectorTerms: [{matchExpressions: [{key: agent-aff, operator: Exists}]}]}}}
  priorityClassName: agent-priority
  podAnnotations: {agent-ann: "1"}
  podLabels: {agent-label: "1"}
  extraArgs: ["--force-sync-every=2h"]
  # Private CA for pushes: Go adds every PEM file in SSL_CERT_DIR to the
  # system roots it already loads from the image's CA bundle file.
  extraEnv: [{name: SSL_CERT_DIR, value: /etc/upgradescope/ca}]
  extraVolumes: [{name: ca, configMap: {name: corp-ca}}]
  extraVolumeMounts: [{name: ca, mountPath: /etc/upgradescope/ca, readOnly: true}]
EOF
helm template upgradescope "$CHART" --namespace upgradescope -f "$TMP/knobs-values.yaml" > "$TMP/knobs.yaml"
[ "$(grep -cxF '        - name: regcred' "$TMP/knobs.yaml")" = 2 ] && pass "imagePullSecrets on both pods" || fail "imagePullSecrets not on both pods"
for c in srv agent; do
  assert_contains "$TMP/knobs.yaml" "pool: $c"              "$c nodeSelector"
  assert_contains "$TMP/knobs.yaml" "key: $c-aff"           "$c affinity"
  assert_contains "$TMP/knobs.yaml" "priorityClassName: \"$c-priority\"" "$c priorityClassName"
  assert_contains "$TMP/knobs.yaml" "$c-ann: \"1\""         "$c podAnnotations"
  assert_contains "$TMP/knobs.yaml" "$c-label: \"1\""       "$c podLabels"
done
assert_contains "$TMP/knobs.yaml" 'key: dedicated'    "server tolerations"
assert_contains "$TMP/knobs.yaml" 'key: agent-taint'  "agent tolerations"
assert_contains "$TMP/knobs.yaml" '- "--team-map=/etc/upgradescope/teams.yaml"' "server extraArgs"
assert_contains "$TMP/knobs.yaml" '- "--force-sync-every=2h"' "agent extraArgs"
assert_contains "$TMP/knobs.yaml" 'name: SRV_EXTRA'   "server extraEnv"
assert_contains "$TMP/knobs.yaml" 'name: SSL_CERT_DIR' "agent extraEnv (CA bundle)"
assert_contains "$TMP/knobs.yaml" 'name: corp-ca'     "agent extraVolumes"
assert_contains "$TMP/knobs.yaml" 'mountPath: /etc/upgradescope/ca' "agent extraVolumeMounts"
assert_contains "$TMP/knobs.yaml" 'name: team-map'    "server extraVolumes"
assert_contains "$TMP/knobs.yaml" 'mountPath: /data'  "server data mount kept alongside extra mounts"

echo "== knobs: OpenShift restricted-v2 (unset UID/GID/fsGroup)"
cat > "$TMP/openshift-values.yaml" <<'EOF'
server:
  enabled: true
  ingestToken: t
  podSecurityContext: {runAsUser: null, runAsGroup: null, fsGroup: null}
agent:
  podSecurityContext: {runAsUser: null, runAsGroup: null}
EOF
helm template upgradescope "$CHART" --namespace upgradescope -f "$TMP/openshift-values.yaml" > "$TMP/openshift.yaml"
assert_not_contains "$TMP/openshift.yaml" 'runAsUser:'  "no runAsUser"
assert_not_contains "$TMP/openshift.yaml" 'runAsGroup:' "no runAsGroup"
assert_not_contains "$TMP/openshift.yaml" 'fsGroup:'    "no fsGroup"
[ "$(grep -cF 'runAsNonRoot: true' "$TMP/openshift.yaml")" = 2 ] && pass "runAsNonRoot kept on both pods" || fail "runAsNonRoot missing"
[ "$(grep -cF 'type: RuntimeDefault' "$TMP/openshift.yaml")" = 2 ] && pass "seccomp RuntimeDefault kept on both pods" || fail "seccompProfile missing"
assert_contains "$TMP/default.yaml" 'runAsUser: 65532' "default UID still 65532"

echo "== knobs: templated args are quoted (YAML-special characters survive)"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set-string 'agent.clusterName=team: payments #2' > "$TMP/clustername.yaml"
assert_line "$TMP/clustername.yaml" '            - "--cluster-name=team: payments #2"' "cluster name is one exact arg"

echo "== knobs: server ServiceAccount without an API token"
assert_contains "$TMP/server.yaml" 'serviceAccountName: upgradescope-server' "server pod has its own SA"
assert_contains "$TMP/server.yaml" 'automountServiceAccountToken: false' "server SA token not mounted"
assert_not_contains "$TMP/default.yaml" 'automountServiceAccountToken: false' "agent keeps its API token"

echo "== knobs: NetworkPolicy (opt-in)"
assert_no_line "$TMP/server.yaml" 'kind: NetworkPolicy' "no NetworkPolicy by default"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set server.enabled=true --set server.ingestToken=t --set networkPolicy.enabled=true > "$TMP/netpol.yaml"
assert_line "$TMP/netpol.yaml" 'kind: NetworkPolicy' "NetworkPolicy rendered when enabled"
assert_contains "$TMP/netpol.yaml" 'app.kubernetes.io/component: agent' "agent pods may reach the server"

echo "== values.schema.json rejects bad values"
for bad in 'server.enable=true' 'agent.interval=30s' 'agent.interval=10' 'agent.targets={latest}' 'rbac.helmSecret=false'; do
  if helm template upgradescope "$CHART" --set "$bad" >/dev/null 2>&1; then
    fail "schema accepted --set $bad"
  else
    pass "schema rejects --set $bad"
  fi
done
for good in 'agent.interval=1m' 'agent.interval=1h30m' 'agent.targets={1.37,v1.38}'; do
  if helm template upgradescope "$CHART" --set "$good" >/dev/null 2>&1; then
    pass "schema accepts --set $good"
  else
    fail "schema rejected --set $good"
  fi
done

[ "$FAILED" -eq 0 ] || { echo "chart-test: FAILED" >&2; exit 1; }
echo "chart-test: all assertions passed"
