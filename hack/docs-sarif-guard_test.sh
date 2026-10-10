#!/usr/bin/env bash
# The SARIF upload step in the CI-gate workflow examples must skip a pull
# request from a fork (its token is read-only, so upload-sarif would turn
# the job red for a reason that is not the gate, #357). The example is
# copied into docs/getting-started/ci-gate.md, action/README.md and the
# README (one flow-style line there); nothing else read it, so a copy could
# lose the guard unnoticed. This extracts the
# upload step's if: from each page and evaluates it with node (it uses only
# !, !=, ==, &&, || and property access, which JavaScript evaluates the same
# way) for a push, a same-repository pull request, a fork pull request and
# a pull request whose fork was deleted (null head.repo; GitHub expressions
# read a property of null as null, so this uses ?. for every access), with
# and without a SARIF file. Offline; needs node.
set -euo pipefail
cd "$(dirname "$0")/.."

command -v node >/dev/null || { echo "docs-sarif-guard_test: node is required" >&2; exit 1; }
results=$(mktemp)
trap 'rm -f "$results"' EXIT
ok() { echo "ok   $1" | tee -a "$results"; }
fail() { echo "FAIL $1" >&2; echo "FAIL $1" >>"$results"; }

pages="docs/getting-started/ci-gate.md action/README.md README.md"

# cond <file>: the if: of the first upload-sarif step, a folded (>-) block
# or one line, as one line of text.
cond() {
  awk '
    /^      - uses: github\/codeql-action\/upload-sarif@/ { s = 1; next }
    s && /^      - / { exit }
    s && /^```/ { exit }
    s && /^        if: >-$/ { f = 1; next }
    s && /^        if: / { sub(/^        if: /, ""); print; exit }
    s && f { if ($0 !~ /^          /) exit; sub(/^ +/, ""); printf "%s ", $0 }
  ' "$1"
}

# want <label> <want> <condition> <context-json>
want() {
  local got
  got=$(node -e '
    const [src, ctxJSON] = process.argv.slice(1);
    const ctx = JSON.parse(ctxJSON);
    const e = src.trim().replace(/^\$\{\{([\s\S]*)\}\}$/, "$1").replace(/\.([A-Za-z_]\w*(?:-[\w]+)+)/g, (_, k) => "?.[" + JSON.stringify(k) + "]").replace(/(?<!\?)\.([A-Za-z_]\w*)/g, "?.$1");
    const cancelled = () => false;
    const v = new Function("github", "steps", "cancelled", "return (" + e + ");")(ctx.github, ctx.steps, cancelled);
    process.stdout.write(String(Boolean(v)));
  ' "$3" "$4") || { fail "$1: cannot evaluate '$3'"; return; }
  if [ "$got" = "$2" ]; then ok "$1"; else fail "$1: '$3' is $got, want $2 for $4"; fi
}
# ctx <event> <head-repo-json> <sarif-file>
ctx() {
  local pr='{}'
  [ "$1" != pull_request ] || pr="{\"pull_request\":{\"head\":{\"repo\":$2}}}"
  echo "{\"github\":{\"event_name\":\"$1\",\"repository\":\"o/r\",\"event\":$pr},\"steps\":{\"gate\":{\"outputs\":{\"sarif-file\":\"$3\"}}}}"
}

for page in $pages; do
  c=$(cond "$page")
  if [ -z "$c" ]; then fail "$page: no upload-sarif step with an if:"; continue; fi
  want "$page: a push uploads" true "$c" "$(ctx push null s.sarif)"
  want "$page: a same-repository pull request uploads" true "$c" "$(ctx pull_request '{"full_name":"o/r"}' s.sarif)"
  want "$page: a fork pull request does not upload" false "$c" "$(ctx pull_request '{"full_name":"x/r"}' s.sarif)"
  want "$page: a pull request from a deleted fork does not upload" false "$c" "$(ctx pull_request null s.sarif)"
  want "$page: no SARIF file, no upload (push)" false "$c" "$(ctx push null '')"
  want "$page: no SARIF file, no upload (same-repository pull request)" false "$c" "$(ctx pull_request '{"full_name":"o/r"}' '')"
  grep -qF '!cancelled()' <<<"$c" && ok "$page: the upload still runs after a failed gate" \
    || fail "$page: the if: lost !cancelled()"
done

pass=$(grep -c '^ok' "$results" || true)
nfail=$(grep -c '^FAIL' "$results" || true)
echo "docs-sarif-guard_test: $pass passed, $nfail failed"
[ "$nfail" -eq 0 ] && [ "$pass" -gt 0 ]
