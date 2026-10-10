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
  if grep -qxE "[[:space:]]*name: $3" <<<"$block" &&
     grep -qxE "[[:space:]]*key: $4" <<<"$block"; then
    pass "$5"
  else
    fail "$5 (env $2 is not secretKeyRef $3/$4)"
  fi
}

# assert_file_secret FILE FLAG PATH SECRET KEY VOLUME DESC: the container is
# passed --FLAG-file=PATH, and the pod volume VOLUME is the Secret SECRET with
# KEY as the file PATH ends in (optional: VOLUME is marked so when the
# description says "optional").
assert_file_secret() {
  local file=$1 flag=$2 path=$3 secret=$4 key=$5 vol=$6 desc=$7 block
  block="$(awk -v v="        - name: $vol" '
    $0 == v { on = 1; print; next }
    on && (/^        - name:/ || /^ {0,7}[^ ]/) { exit }
    on { print }' "$file")"
  if grep -qxF -- "            - \"--$flag-file=$path\"" "$file" &&
     grep -qxE "[[:space:]]*secretName: $secret" <<<"$block" &&
     grep -qF -- "{key: $key, path: ${path##*/}}" <<<"$block"; then
    if [[ "$desc" == *optional* ]]; then
      grep -qxE '[[:space:]]*optional: true' <<<"$block" && pass "$desc" || fail "$desc (volume $vol is not optional: true)"
    else
      grep -qE 'optional: true' <<<"$block" && fail "$desc (volume $vol is optional)" || pass "$desc"
    fi
  else
    fail "$desc (--$flag-file=$path is not key $key of Secret $secret in volume $vol)"
  fi
}
# assert_no_env_secret FILE: no container reads a secret from the environment
# but the database URL (tokens, webhook URLs and keys are mounted files).
assert_no_env_secret() {
  local n
  n="$(grep -cE 'secretKeyRef' "$1" || true)"
  if [ "$n" = "${2:-0}" ]; then pass "$3"; else fail "$3 ($n secretKeyRef, want ${2:-0})"; fi
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
# from the embedded KB. -count=1: the test cache cannot see template edits
# (helm reads them in a subprocess), so a cached pass could be stale.
command -v go >/dev/null || { echo "ERROR: go not found in PATH (needed for the RBAC tests)" >&2; exit 1; }
if (cd "$ROOT" && UPGRADESCOPE_CHART_TEST=1 go test -count=1 ./deploy/chart/); then
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
assert_contains "$TMP/default.yaml" '--pod-pass-every=3' "default pod-pass-every flag (#228)"
assert_contains "$TMP/default.yaml" '--pod-pass-max-age=1h' "default pod-pass-max-age flag (#228)"
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
assert_not_contains "$TMP/default.yaml" '@sha256:' "the in-repo chart pins no digest (release.yml sets it at package time)"

echo "== image.digest pins the image (the published chart sets it)"
DIGEST="sha256:$(printf '%064d' 7)"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set server.enabled=true --set server.ingestToken=t \
  --set image.digest="$DIGEST" > "$TMP/digest.yaml"
[ "$(grep -cF "image: \"ghcr.io/abd-ulbasit/upgradescope:$APP_VERSION@$DIGEST\"" "$TMP/digest.yaml")" = 2 ] \
  && pass "image.digest pins agent and server to repository:tag@digest" \
  || fail "image.digest does not pin both pods to ghcr.io/abd-ulbasit/upgradescope:$APP_VERSION@$DIGEST"
# The published chart carries a digest; overriding only the tag must run
# that tag, not repo:newtag@olddigest (the old bytes under a new name).
helm template upgradescope "$CHART" --namespace upgradescope \
  --set server.enabled=true --set server.ingestToken=t \
  --set image.digest="$DIGEST" --set image.tag=v9.9.9 > "$TMP/digest-tag.yaml"
[ "$(grep -cF 'image: "ghcr.io/abd-ulbasit/upgradescope:v9.9.9"' "$TMP/digest-tag.yaml")" = 2 ] \
  && pass "image.tag set: the chart's image.digest is not applied to another tag" \
  || fail "image.tag=v9.9.9 with image.digest set does not render ghcr.io/abd-ulbasit/upgradescope:v9.9.9 on both pods"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set server.enabled=true --set server.ingestToken=t \
  --set image.digest="$DIGEST" --set image.tag="v9.9.9@sha256:$(printf '%064d' 9)" > "$TMP/tag-digest.yaml"
[ "$(grep -cF "image: \"ghcr.io/abd-ulbasit/upgradescope:v9.9.9@sha256:$(printf '%064d' 9)\"" "$TMP/tag-digest.yaml")" = 2 ] \
  && pass "image.tag tag@digest pins another tag by its own digest" \
  || fail "image.tag=v9.9.9@sha256:... does not render as written on both pods"
for bad in 'image.digest=latest' 'image.digest=sha256:abc' "image.digest=$APP_VERSION"; do
  if helm template upgradescope "$CHART" --set "$bad" >/dev/null 2>&1; then
    fail "schema accepted --set $bad (want sha256:<64 hex>)"
  else
    pass "schema rejects --set $bad"
  fi
done

echo "== agent assertions: external-server render"
assert_contains "$TMP/external.yaml" '--server-url=https://uscope.example.com' "explicit serverUrl wins"
assert_file_secret "$TMP/external.yaml" server-token /etc/upgradescope/push-token/token my-secret serverToken push-token "agent token from agent.existingSecret, a mounted file"
assert_no_env_secret "$TMP/external.yaml" 0 "the agent token is not an environment variable"
assert_no_line "$TMP/external.yaml" 'kind: Secret' "no generated Secret when existingSecret set"

echo "== agent assertions: inline agent.serverToken goes into a chart Secret"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set agent.serverUrl=https://uscope.example.com --set agent.serverToken=s3cret > "$TMP/inline-agent.yaml"
assert_file_secret "$TMP/inline-agent.yaml" server-token /etc/upgradescope/push-token/token upgradescope-agent-token serverToken push-token "agent token from the chart's agent Secret, a mounted file"
assert_contains "$TMP/inline-agent.yaml" "serverToken: \"$(printf s3cret | base64)\"" "inline token stored in the Secret (data, base64: a removed one leaves no key)"

echo "== agent assertions: targets render"
# The agent creates ClusterReadiness/<crName> on its first tick. Were the
# chart to render it too, 'helm upgrade --set agent.targets=...' after a
# default install would fail: Helm will not adopt the agent-created object.
assert_no_line "$TMP/targets.yaml" 'kind: ClusterReadiness' "no CR rendered even with agent.targets"
assert_contains "$TMP/targets.yaml" '- "--targets=1.37,1.38"' "targets passed to the agent"

echo "== upgrade path: setting targets after a default install changes only the agent"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set 'agent.targets={1.37}' > "$TMP/targets-upgrade.yaml"
if diff <(grep -vF -e '--targets=' "$TMP/targets-upgrade.yaml") "$TMP/default.yaml" >/dev/null; then
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
assert_file_secret "$TMP/server.yaml" ingest-token /etc/upgradescope/secret-files/ingestToken upgradescope-server-tokens ingestToken secret-files "ingest token from the Secret, a mounted file"
assert_contains "$TMP/server.yaml" 'ingestToken: "dGVzdC10b2tlbg=="' "ingest token in Secret data (base64), so switching it off removes it"
assert_file_secret "$TMP/server.yaml" server-token /etc/upgradescope/push-token/token upgradescope-server-tokens ingestToken push-token "agent pushes with the server's ingest token"
assert_no_line "$TMP/server.yaml" '  serverToken: "test-token"' "no copy of the ingest token in a second Secret"
assert_contains "$TMP/server.yaml" '--server-url=http://upgradescope-server.upgradescope.svc:8080' "agent points at in-chart server"
assert_contains "$TMP/server.yaml" 'path: /healthz' "healthz probes"
assert_not_contains "$TMP/server.yaml" '--read-token-file' "no read token unless set"
# serve refuses an open read API on a non-loopback --listen; the chart's
# empty readToken default opts in explicitly (NOTES.txt warns about it).
assert_contains "$TMP/server.yaml" '--allow-anonymous-read' "empty readToken opts in to anonymous reads"

echo "== secrets never reach argv (no \$(VAR) expansion into args)"
for f in "$TMP"/*.yaml; do
  assert_not_contains "$f" '$(UPGRADESCOPE_' "$(basename "$f"): no \$(UPGRADESCOPE_*) in args"
  assert_not_contains "$f" '--ingest-token=' "$(basename "$f"): no --ingest-token= arg (the value is a mounted file)"
  assert_not_contains "$f" '--server-token=' "$(basename "$f"): no --server-token= arg (the value is a mounted file)"
done

echo "== server assertions: read token set"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set server.enabled=true --set server.ingestToken=t \
  --set server.readToken=r > "$TMP/readtoken.yaml"
assert_file_secret "$TMP/readtoken.yaml" read-token /etc/upgradescope/secret-files/readToken upgradescope-server-tokens readToken secret-files "read token from the Secret, a mounted file"
assert_not_contains "$TMP/readtoken.yaml" '--read-token=' "no read token value in args"
assert_not_contains "$TMP/readtoken.yaml" '--allow-anonymous-read' "no anonymous reads with a read token"

echo "== server assertions: no ingest token supplied -> chart generates one (one-command install)"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set server.enabled=true > "$TMP/gentoken.yaml"
if grep -qE '^  ingestToken: "[A-Za-z0-9+/]{54}=="$' "$TMP/gentoken.yaml"; then  # base64 of 40 characters
  pass "random 40-char ingest token generated"
else
  fail "no generated ingestToken in the server Secret"
fi
assert_file_secret "$TMP/gentoken.yaml" server-token /etc/upgradescope/push-token/token upgradescope-server-tokens ingestToken push-token "agent uses the generated token"

echo "== server assertions: webhook URLs live only in the Secret"
SLACK='https://hooks.slack.com/services/T000/B000/XXXX'
HOOK='https://hooks.example.com/upgradescope'
helm template upgradescope "$CHART" --namespace upgradescope \
  --set server.enabled=true --set server.ingestToken=t \
  --set server.slackWebhook="$SLACK" --set server.webhook="$HOOK" > "$TMP/webhooks.yaml"
perl -0777 -ne 'print grep { !/^kind: Secret$/m } split /^---$/m' "$TMP/webhooks.yaml" > "$TMP/webhooks-nosecret.txt"
assert_contains "$TMP/webhooks.yaml" "slackWebhook: \"$(printf %s "$SLACK" | base64)\"" "Slack URL in the Secret (data, base64)"
assert_not_contains "$TMP/webhooks-nosecret.txt" "$SLACK" "Slack URL nowhere outside the Secret"
assert_not_contains "$TMP/webhooks-nosecret.txt" "$HOOK" "webhook URL nowhere outside the Secret"
assert_file_secret "$TMP/webhooks.yaml" slack-webhook /etc/upgradescope/secret-files/slackWebhook upgradescope-server-tokens slackWebhook secret-files "Slack URL, a mounted file"
assert_file_secret "$TMP/webhooks.yaml" webhook /etc/upgradescope/secret-files/webhook upgradescope-server-tokens webhook secret-files "webhook URL, a mounted file"

echo "== server assertions: server.existingSecret alone wires every key"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set server.enabled=true --set server.existingSecret=mysec > "$TMP/existing.yaml"
assert_no_line "$TMP/existing.yaml" 'kind: Secret' "no chart Secret with server.existingSecret"
assert_file_secret "$TMP/existing.yaml" ingest-token /etc/upgradescope/secret-files/ingestToken mysec ingestToken secret-files "server ingest token from existingSecret (required with the in-chart agent)"
assert_file_secret "$TMP/existing.yaml" server-token /etc/upgradescope/push-token/token mysec ingestToken push-token "agent token from the same existingSecret"
assert_file_secret "$TMP/existing.yaml" slack-webhook /etc/upgradescope/secret-files-optional/slackWebhook mysec slackWebhook secret-files-optional "optional slackWebhook key"
assert_file_secret "$TMP/existing.yaml" webhook /etc/upgradescope/secret-files-optional/webhook mysec webhook secret-files-optional "optional webhook key"
assert_contains "$TMP/existing.yaml" '--optional-secret-file=slack-webhook,webhook,webhook-secret' "serve is told which files may be absent"
assert_contains "$TMP/existing.yaml" '--allow-anonymous-read' "no read token unless asked for"

echo "== server assertions: read token from existingSecret, no inline value"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set server.enabled=true --set server.existingSecret=mysec \
  --set server.readTokenFromSecret=true > "$TMP/existing-read.yaml"
assert_file_secret "$TMP/existing-read.yaml" read-token /etc/upgradescope/secret-files/readToken mysec readToken secret-files "read token from existingSecret"
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

# --- FLEET HUB (#43) ---
echo "== hub: retention and staleness reach serve"
assert_contains "$TMP/server.yaml" '- "--retention=90d"'  "retention window passed to serve"
assert_contains "$TMP/server.yaml" '- "--stale-after=2h"' "stale-after passed to serve"
# The documented `--set server.retention=0` arrives as a number, not "0".
helm template upgradescope "$CHART" --namespace upgradescope \
  --set server.enabled=true --set server.ingestToken=t --set server.retention=0 > "$TMP/keep.yaml"
assert_contains "$TMP/keep.yaml" '- "--retention=0"' "--set server.retention=0 keeps everything"

echo "== hub: server-only mode (agent.enabled=false) renders no agent objects"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set agent.enabled=false --set server.enabled=true --set server.ingestToken=t > "$TMP/hub.yaml"
assert_line "$TMP/hub.yaml" 'kind: Deployment'               "server Deployment rendered"
assert_no_line "$TMP/hub.yaml" '  name: upgradescope-agent'  "no agent Deployment or ClusterRole"
assert_no_line "$TMP/hub.yaml" '  name: upgradescope'        "no agent ServiceAccount"
assert_no_line "$TMP/hub.yaml" 'kind: ClusterRole'           "no ClusterRole"
assert_no_line "$TMP/hub.yaml" 'kind: ClusterRoleBinding'    "no ClusterRoleBinding"
assert_not_contains "$TMP/hub.yaml" '--server-token-file' "no agent push token"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set agent.enabled=false --set server.enabled=true --set server.ingestToken=t \
  --set agent.serverToken=x --set agent.serverUrl=https://x.example \
  --set metrics.serviceMonitor.enabled=true --set metrics.prometheusRule.enabled=true \
  --set networkPolicy.enabled=true \
  --set-json 'networkPolicy.serverIngressFrom=[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"ingress"}}}]' > "$TMP/hub-extras.yaml"
assert_line "$TMP/hub-extras.yaml" 'kind: NetworkPolicy' "hub NetworkPolicy rendered with its peers"
if helm template upgradescope "$CHART" --set agent.enabled=false --set server.enabled=true --set server.ingestToken=t \
  --set networkPolicy.enabled=true >/dev/null 2>&1; then
  fail "a hub NetworkPolicy without peers would admit every source; the render should fail"
else
  pass "hub NetworkPolicy needs networkPolicy.serverIngressFrom"
fi
assert_no_line "$TMP/hub-extras.yaml" '  name: upgradescope-agent-token'   "no agent token Secret"
assert_no_line "$TMP/hub-extras.yaml" '  name: upgradescope-agent-metrics' "no agent metrics Service"
assert_not_contains "$TMP/hub-extras.yaml" 'upgradescope-agent'          "no agent ServiceMonitor, rule group or NetworkPolicy peer"
assert_contains "$TMP/hub-extras.yaml" 'UpgradescopeClusterStale'        "server rules still rendered"

echo "== hub: Postgres via an existing Secret, no PVC, several replicas"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set agent.enabled=false --set server.enabled=true --set server.ingestToken=t \
  --set server.database.existingSecret=pg-dsn --set server.database.key=url \
  --set server.replicas=2 > "$TMP/hub-pg.yaml"
assert_env_secret "$TMP/hub-pg.yaml" UPGRADESCOPE_DB_URL pg-dsn url "Postgres DSN from the Secret via env"
assert_not_contains "$TMP/hub-pg.yaml" '--db=' "no SQLite --db"
assert_not_contains "$TMP/hub-pg.yaml" '--db-url' "the DSN never reaches argv"
assert_no_line "$TMP/hub-pg.yaml" 'kind: PersistentVolumeClaim' "no PVC with Postgres"
assert_not_contains "$TMP/hub-pg.yaml" 'mountPath: /data' "no data volume with Postgres"
assert_line "$TMP/hub-pg.yaml" '  replicas: 2' "replicas honoured with Postgres"
assert_contains "$TMP/hub-pg.yaml" 'type: RollingUpdate' "rolling updates with Postgres"
if helm template upgradescope "$CHART" --set server.enabled=true --set server.ingestToken=t --set server.replicas=2 >/dev/null 2>&1; then
  fail "server.replicas=2 on SQLite should fail the render"
else
  pass "several replicas need server.database"
fi

echo "== hub: team map ConfigMap"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set server.enabled=true --set server.ingestToken=t \
  --set-json 'server.teamMap=[{"pattern":"payments-*","team":"payments"}]' > "$TMP/hub-teams.yaml"
assert_line "$TMP/hub-teams.yaml" 'kind: ConfigMap' "team map ConfigMap rendered"
assert_contains "$TMP/hub-teams.yaml" 'pattern: payments-*' "team map rules in the ConfigMap"
assert_contains "$TMP/hub-teams.yaml" '- "--team-map=/etc/upgradescope/team-map/team-map.yaml"' "--team-map points at the mount"
assert_contains "$TMP/hub-teams.yaml" 'mountPath: /etc/upgradescope/team-map' "team map mounted"
assert_contains "$TMP/hub-teams.yaml" 'checksum/team-map:' "pods roll when the team map changes"
assert_no_line "$TMP/server.yaml" 'kind: ConfigMap' "no team map ConfigMap by default"

echo "== hub: Ingress with TLS"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set server.enabled=true --set server.ingestToken=t --set server.readToken=r \
  --set server.ingress.enabled=true --set server.ingress.host=uscope.example.com \
  --set server.ingress.className=nginx > "$TMP/hub-ingress.yaml"
assert_line "$TMP/hub-ingress.yaml" 'kind: Ingress' "Ingress rendered"
assert_contains "$TMP/hub-ingress.yaml" 'host: uscope.example.com' "Ingress host"
assert_contains "$TMP/hub-ingress.yaml" 'secretName: upgradescope-server-tls' "TLS block with the default Secret name"
assert_contains "$TMP/hub-ingress.yaml" 'ingressClassName: nginx' "ingress class"
assert_no_line "$TMP/server.yaml" 'kind: Ingress' "no Ingress by default"
if helm template upgradescope "$CHART" --set server.enabled=true --set server.ingestToken=t --set server.readToken=r \
  --set server.ingress.enabled=true >/dev/null 2>&1; then
  fail "an Ingress without a host should fail the render"
else
  pass "Ingress needs server.ingress.host"
fi
if helm template upgradescope "$CHART" --set server.enabled=true --set server.ingestToken=t \
  --set server.ingress.enabled=true --set server.ingress.host=u.example.com >/dev/null 2>&1; then
  fail "an Ingress in front of an open read API should fail the render"
else
  pass "Ingress needs a read token (or allowAnonymousRead behind an auth layer)"
fi
helm template upgradescope "$CHART" --namespace upgradescope --set server.enabled=true --set server.ingestToken=t \
  --set server.ingress.enabled=true --set server.ingress.host=u.example.com \
  --set server.ingress.allowAnonymousRead=true > "$TMP/hub-ingress-proxy.yaml"
assert_line "$TMP/hub-ingress-proxy.yaml" 'kind: Ingress' "Ingress with allowAnonymousRead behind an auth layer"

echo "== hub: admin token and webhook secret, inline or from server.existingSecret"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set server.enabled=true --set server.ingestToken=t \
  --set server.adminToken=adm --set server.webhookSecret=whk > "$TMP/hub-admin.yaml"
assert_file_secret "$TMP/hub-admin.yaml" admin-token /etc/upgradescope/secret-files/adminToken upgradescope-server-tokens adminToken secret-files "admin token from the chart Secret"
assert_file_secret "$TMP/hub-admin.yaml" webhook-secret /etc/upgradescope/secret-files/webhookSecret upgradescope-server-tokens webhookSecret secret-files "webhook secret from the chart Secret"
assert_contains "$TMP/hub-admin.yaml" "adminToken: \"$(printf adm | base64)\"" "admin token stored in the Secret (data, base64)"
assert_not_contains "$TMP/server.yaml" '--admin-token-file' "no admin token unless set"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set agent.enabled=false --set server.enabled=true --set server.existingSecret=hub \
  --set server.readTokenFromSecret=true --set server.adminTokenFromSecret=true > "$TMP/hub-existing.yaml"
assert_file_secret "$TMP/hub-existing.yaml" admin-token /etc/upgradescope/secret-files/adminToken hub adminToken secret-files "admin token from existingSecret"
assert_file_secret "$TMP/hub-existing.yaml" read-token /etc/upgradescope/secret-files/readToken hub readToken secret-files "read token from existingSecret"
assert_file_secret "$TMP/hub-existing.yaml" ingest-token /etc/upgradescope/secret-files-optional/ingestToken hub ingestToken secret-files-optional "a hub's shared ingest token is optional (per-cluster tokens suffice)"
assert_file_secret "$TMP/hub-existing.yaml" webhook-secret /etc/upgradescope/secret-files-optional/webhookSecret hub webhookSecret secret-files-optional "optional webhookSecret key"
if helm template upgradescope "$CHART" --set server.enabled=true --set server.adminTokenFromSecret=true >/dev/null 2>&1; then
  fail "adminTokenFromSecret without existingSecret should fail"
else
  pass "adminTokenFromSecret requires existingSecret"
fi

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
  # A reserved selector key is dropped, not rendered as a duplicate key.
  podLabels: {agent-label: "1", app.kubernetes.io/component: hijack}
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
assert_not_contains "$TMP/knobs.yaml" 'hijack' "podLabels cannot override the selector labels"
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
# agent.targets end up in the CR's spec.targets, which the CRD pins to
# MAJOR.MINOR: v1.38 or 1.37.2 would make every create/patch 422.
# agent.interval: Go durations of at least 1m render in any spelling v0.1
# (which had no schema) let through, e.g. 300s or 1.5h; under 1m or a bare
# number fails. server.targets takes at most 4 minors, as serve --targets
# does: the server's memory limit is sized for that many. agent.targets takes
# at most 8, the CRD's spec.targets maxItems. agent.crName is an RFC 1123
# subdomain, as the agent requires at start.
for bad in 'server.enable=true' 'agent.interval=30s' 'agent.interval=59s' 'agent.interval=59.9s' \
  'agent.interval=0.5m' 'agent.interval=100ms' 'agent.interval=10' 'agent.targets={latest}' \
  'agent.targets={v1.38}' 'agent.targets={1.37.2}' 'rbac.helmSecret=false' \
  'server.targets={1.36,1.37,1.38,1.39,1.40}' \
  'agent.targets={1.30,1.31,1.32,1.33,1.34,1.35,1.36,1.37,1.38}' \
  'agent.crName=a..b' 'agent.crName=a.-b' 'agent.crName=Prod' \
  'agent.podPassEvery=0' 'agent.podPassEvery=-1' 'agent.podPassEvery=1.5' 'agent.podPassMaxAge=30s' 'agent.podPassMaxAge=1'; do
  if helm template upgradescope "$CHART" --set "$bad" >/dev/null 2>&1; then
    fail "schema accepted --set $bad"
  else
    pass "schema rejects --set $bad"
  fi
done
for good in 'agent.interval=1m' 'agent.interval=1h30m' 'agent.interval=10m0s' 'agent.interval=60s' \
  'agent.interval=90s' 'agent.interval=300s' 'agent.interval=1.5h' 'agent.interval=0.5h' \
  'agent.interval=1.5m' 'agent.interval=2m30.5s' 'agent.targets={1.37,1.38}' \
  'server.targets={1.36,1.37,1.38,1.39}' \
  'agent.targets={1.30,1.31,1.32,1.33,1.34,1.35,1.36,1.37}' \
  'agent.crName=prod.eu-west-1' 'agent.podPassEvery=1' 'agent.podPassEvery=6' 'agent.podPassMaxAge=2h'; do
  if helm template upgradescope "$CHART" --set "$good" >/dev/null 2>&1; then
    pass "schema accepts --set $good"
  else
    fail "schema rejected --set $good"
  fi
done
# The pod pass settings reach the agent as flags (#228).
helm template upgradescope "$CHART" --namespace upgradescope --set agent.podPassEvery=1 --set agent.podPassMaxAge=20m > "$TMP/podpass.yaml"
assert_contains "$TMP/podpass.yaml" '--pod-pass-every=1' "agent.podPassEvery renders --pod-pass-every"
assert_contains "$TMP/podpass.yaml" '--pod-pass-max-age=20m' "agent.podPassMaxAge renders --pod-pass-max-age"
# agent.crName takes 253 bytes, the most an object name has, and not 254.
name253=$(printf 'a%.0s' $(seq 1 253))
if helm template upgradescope "$CHART" --set "agent.crName=$name253" >/dev/null 2>&1; then
  pass "schema accepts a 253-byte agent.crName"
else
  fail "schema rejected a 253-byte agent.crName"
fi
if helm template upgradescope "$CHART" --set "agent.crName=${name253}a" >/dev/null 2>&1; then
  fail "schema accepted a 254-byte agent.crName"
else
  pass "schema rejects a 254-byte agent.crName"
fi

[ "$FAILED" -eq 0 ] || { echo "chart-test: FAILED" >&2; exit 1; }
echo "chart-test: all assertions passed"
