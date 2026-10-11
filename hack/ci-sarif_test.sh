#!/usr/bin/env bash
# CI's action job uploads its fixture SARIF to code scanning only from a
# dispatched run on a branch other than the default (make hack-test, #244).
# The fixture holds two blockers on purpose; uploaded from main on every
# push, they were open error alerts in the public Security tab. This
# evaluates the upload step's if: with node (it uses only ==, !=, &&, ||,
# format() and property access, which JavaScript evaluates the same way)
# for each kind of run, and checks its category. Offline; needs node.
set -euo pipefail
cd "$(dirname "$0")/.."

ci=.github/workflows/ci.yml
command -v node >/dev/null || { echo "ci-sarif_test: node is required" >&2; exit 1; }
results=$(mktemp)
trap 'rm -f "$results"' EXIT
ok() { echo "ok   $1" | tee -a "$results"; }
fail() { echo "FAIL $1" >&2; echo "FAIL $1" >>"$results"; }

# stepof <name>: the action job's step called <name>, to the next step.
stepof() {
  awk -v want="      - name: $1" '$0 == "  action:" { j = 1; next } j && /^  [a-z0-9_-]+:/ { exit }
    j && $0 == want { s = 1; print; next }
    s && /^      - / { exit } s { print }' "$ci"
}
# condof <step text>: its if:, a folded (>-) block or one line.
condof() {
  awk '/^        if: >-$/ { f = 1; next } /^        if: / { sub(/^        if: /, ""); print; exit }
    f { if ($0 !~ /^          /) exit; sub(/^ +/, ""); printf "%s ", $0 }' <<<"$1"
}
up_name='GitHub code scanning accepts the SARIF'
sup_name='GitHub code scanning accepts the suppressed fixture'
chk_name='code scanning holds no alert for the suppressed fixture'
step=$(stepof "$up_name")
[ -n "$step" ] || { echo "FAIL no '$up_name' step in $ci's action job" >&2; exit 1; }
cond=$(condof "$step")
[ -n "$cond" ] || { echo "FAIL the upload step has no if:" >&2; exit 1; }
supstep=$(stepof "$sup_name")
[ -n "$supstep" ] || { echo "FAIL no '$sup_name' step in $ci's action job" >&2; exit 1; }
chkstep=$(stepof "$chk_name")
[ -n "$chkstep" ] || { echo "FAIL no '$chk_name' step in $ci's action job" >&2; exit 1; }

# want <label> <want> <context-json>
want() {
  local got
  got=$(node -e '
    const [src, ctxJSON] = process.argv.slice(1);
    const ctx = JSON.parse(ctxJSON);
    const e = src.trim().replace(/^\$\{\{([\s\S]*)\}\}$/, "$1").replace(/\.([A-Za-z_]\w*(?:-[\w]+)+)/g, (_, k) => "[" + JSON.stringify(k) + "]");
    const format = (f, ...a) => f.replace(/\{(\d+)\}/g, (_, i) => String(a[i]));
    const v = new Function("github", "inputs", "matrix", "format", "return (" + e + ");")(ctx.github, ctx.inputs || {}, ctx.matrix, format);
    process.stdout.write(String(Boolean(v)));
  ' "$cond" "$3") || { fail "$1: cannot evaluate '$cond'"; return; }
  if [ "$got" = "$2" ]; then ok "$1"; else fail "$1: '$cond' is $got, want $2 for $3"; fi
}
ctx() { # ctx <event> <ref> <upload-fixture-sarif|""> <os>
  local inputs='{}'
  [ -z "$3" ] || inputs="{\"upload-fixture-sarif\":$3}"
  echo "{\"github\":{\"event_name\":\"$1\",\"ref\":\"$2\",\"event\":{\"repository\":{\"default_branch\":\"main\"}}},\"inputs\":$inputs,\"matrix\":{\"os\":\"$4\"}}"
}
matrix() {
  want "a push to main uploads nothing (the #244 alerts)" false "$(ctx push refs/heads/main "" ubuntu-latest)"
  want "a pull request uploads nothing" false "$(ctx pull_request refs/pull/1/merge "" ubuntu-latest)"
  want "the schedule uploads nothing" false "$(ctx schedule refs/heads/main "" ubuntu-latest)"
  want "a release run (tag push via workflow_call) uploads nothing" false "$(ctx push refs/tags/v0.2.0 "" ubuntu-latest)"
  want "a dispatch without upload-fixture-sarif uploads nothing" false "$(ctx workflow_dispatch refs/heads/sarif-check false ubuntu-latest)"
  want "a dispatch on the default branch never uploads" false "$(ctx workflow_dispatch refs/heads/main true ubuntu-latest)"
  want "a dispatch on another branch with upload-fixture-sarif uploads" true "$(ctx workflow_dispatch refs/heads/sarif-check true ubuntu-latest)"
  want "only the ubuntu leg uploads" false "$(ctx workflow_dispatch refs/heads/sarif-check true macos-latest)"
}
matrix
# The suppressed fixture's upload and the alert check run under exactly the
# upload's condition, so the check never runs where nothing was uploaded.
for pair in "suppressed upload:$supstep" "alert check:$chkstep"; do
  label=${pair%%:*}; body=${pair#*:}
  if [ "$(condof "$body")" = "$cond" ]; then ok "the $label has the fixture upload's condition"; else fail "the $label's if: differs from the fixture upload's"; fi
done
if grep -qxF '          category: upgradescope-action-fixture-suppressed' <<<"$supstep" &&
   grep -qF 'steps.suppressed.outputs.sarif-file' <<<"$supstep"; then
  ok "the suppressed fixture is uploaded from the suppressed step, under a category of its own"
else
  fail "the suppressed upload is not steps.suppressed's sarif-file under upgradescope-action-fixture-suppressed"
fi
if grep -qF 'code-scanning/alerts' <<<"$chkstep" && grep -qF 'state=open' <<<"$chkstep" &&
   grep -qF 'upgradescope-action-fixture-suppressed' <<<"$chkstep" &&
   grep -qF "'upgradescope-action-fixture'" <<<"$chkstep" && grep -qF '"$held" -eq 0' <<<"$chkstep" &&
   grep -qF '"$control" -ge 2' <<<"$chkstep"; then
  ok "the check lists the ref's open alerts through the alerts API, for both categories, the unsuppressed one as the control"
else
  fail "the alert check does not query code-scanning/alerts for open alerts of both fixture categories"
fi
if grep -qxF '          category: upgradescope-action-fixture' <<<"$step"; then
  ok "the fixture upload has a category of its own"
else
  fail "the fixture upload's category is not upgradescope-action-fixture"
fi
# The input exists, off by default.
if awk '/^  workflow_dispatch:/ { d = 1 } d && /^      upload-fixture-sarif:$/ { u = 1; next } u && /^        type: boolean$/ { t = 1 } u && /^        default: false$/ { f = 1 } u && /^      [a-z-]+:$/ { u = 0 } END { exit !(t && f) }' "$ci"; then
  ok "workflow_dispatch has upload-fixture-sarif, a boolean, false by default"
else
  fail "workflow_dispatch has no boolean upload-fixture-sarif input defaulting to false"
fi

pass=$(grep -c '^ok' "$results" || true)
nfail=$(grep -c '^FAIL' "$results" || true)
echo "ci-sarif_test: $pass passed, $nfail failed"
[ "$nfail" -eq 0 ] && [ "$pass" -gt 0 ]
