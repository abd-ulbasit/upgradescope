#!/usr/bin/env bash
# Tests the wiring of the docs-site smoke check in .github/workflows/docs.yml
# (make hack-test, #284). hack/docs-live-check_test.sh proves the script; this
# proves the workflow runs it when it must, and nowhere it must not, because
# the failure it exists for was a workflow that went red on every push to main
# for weeks with nothing looking at the live site:
#  - which jobs run for a pull request, a push to main, a manual run on main
#    and on another branch, and the weekly schedule, and when a job fails.
#    The jobs' `if:` and `needs:` are read from the file and evaluated with
#    node (the expressions use only ==, !=, && and ||, which JavaScript
#    evaluates the same way) against a model of Actions' rule that a job runs
#    only when everything it needs succeeded and its `if:` holds. So `smoke`
#    runs after a successful deploy and only then, and the schedule runs
#    `smoke-published` alone: no build, no deploy;
#  - the smoke jobs hold `contents: read` and nothing else, pin their actions
#    by sha, keep no token in the checkout, and pass the deployed URL through
#    an environment variable, not an expression inside the script;
#  - the schedule is a weekly cron; the README and the check itself are among
#    the paths whose push redeploys (and so re-checks);
#  - the deploy job still holds only pages and id-token, and publishes the
#    deployed URL as an output;
#  - the Makefile's hack-test runs both tests.
# Offline; needs node.
set -euo pipefail
cd "$(dirname "$0")/.."

command -v node >/dev/null || { echo "docs-live-workflow_test: node is required" >&2; exit 1; }

wf=${DOCS_WORKFLOW:-.github/workflows/docs.yml}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
: >"$work/results"
ok() { echo "ok   $1" | tee -a "$work/results"; }
fail() {
  echo "FAIL $1" >&2
  [ -z "${2:-}" ] || sed 's/^/     /' "$2" >&2
  echo "FAIL $1" >>"$work/results"
}
# expect <name> <got> <want>
expect() {
  if [ "$2" = "$3" ]; then ok "$1"; else
    printf 'got:  %s\nwant: %s\n' "$2" "$3" >"$work/why"
    fail "$1" "$work/why"
  fi
}

# job <id>: the job's lines, from `  <id>:` to the next job.
job() { awk -v want="  $1:" '$0 == want { c = 1; print; next } c && /^  [^ ]/ { exit } c && /^[^ ]/ { exit } c' "$wf"; }
# key <indent> <key>: from a job on stdin, the value of the first <key> at
# exactly <indent> spaces, without a trailing comment.
key() {
  awk -v pre="$(printf '%*s' "$1" '')$2:" 'index($0, pre) == 1 { v = substr($0, length(pre) + 1); sub(/^ +/, "", v); sub(/ +#.*$/, "", v); print v; exit }'
}
# scopes: from a job on stdin, "scope access" for each line of its permissions
# block (four-space `permissions:`, six-space scopes).
scopes() { awk '/^    permissions:/ { p = 1; next } p && /^      [a-z-]+:/ { s = $1; sub(/:$/, "", s); print s, $2; next } p { exit }'; }
# top_scopes: the same for the workflow's own block (no indent, two-space scopes).
top_scopes() { awk '/^permissions:/ { p = 1; next } p && /^  [a-z-]+:/ { s = $1; sub(/:$/, "", s); print s, $2; next } p { exit }' "$wf"; }

for id in references build deploy smoke smoke-published; do
  [ -n "$(job "$id")" ] || { echo "FAIL $wf has no $id job" >&2; exit 1; }
done

# --- which jobs run -----------------------------------------------------

# One line per job: id, needs (comma-separated), if.
awk '
  function flush() { if (id != "") printf "%s\t%s\t%s\n", id, needs, cond }
  /^jobs:/ { j = 1; next }
  j && /^[^ #]/ { j = 0 }
  j && /^  [a-z0-9_-]+:[ ]*(#.*)?$/ { flush(); id = $1; sub(/:$/, "", id); needs = ""; cond = ""; next }
  j && /^    needs:/ { v = $0; sub(/^    needs:[ ]*/, "", v); gsub(/\[/, "", v); gsub(/\]/, "", v); gsub(/ /, "", v); needs = v; next }
  j && /^    if:/ { v = $0; sub(/^    if:[ ]*/, "", v); sub(/[ ]+#.*$/, "", v); cond = v; next }
  END { flush() }
' "$wf" >"$work/jobs.tsv"

# ran <event> <ref> [failing job]: the jobs that run, sorted, one line.
ran() {
  node -e '
    const fs = require("fs");
    const [file, event_name, ref, failing] = process.argv.slice(1);
    const jobs = fs.readFileSync(file, "utf8").split("\n").filter(Boolean).map((l) => {
      const [id, needs, cond] = l.split("\t");
      return { id, needs: needs ? needs.split(",") : [], cond };
    });
    const github = { event_name, ref };
    const result = {};
    const ran = [];
    for (const j of jobs) {
      let r = "skipped";
      if (j.needs.every((n) => result[n] === "success")) {
        let run = true;
        if (j.cond) {
          const e = j.cond.replace(/^\$\{\{\s*|\s*\}\}$/g, "");
          if (/\b(always|cancelled|failure|success)\s*\(/.test(e)) {
            console.log("STATUS-FUNCTION in " + j.id + ": the model assumes the implicit success()");
            process.exit(3);
          }
          const needs = {};
          for (const n of j.needs) needs[n] = { result: result[n] };
          run = new Function("github", "needs", "return (" + e + ");")(github, needs);
        }
        if (run) { r = j.id === failing ? "failure" : "success"; ran.push(j.id); }
      }
      result[j.id] = r;
    }
    console.log(ran.sort().join(" "));
  ' "$work/jobs.tsv" "$1" "$2" "${3:-}"
}

main=refs/heads/main
expect "a pull request builds and checks references; it neither deploys nor smokes" \
  "$(ran pull_request refs/pull/7/merge)" "build references"
expect "a push to main builds, deploys, then smoke-checks the deployed site" \
  "$(ran push $main)" "build deploy references smoke"
expect "a manual run on main does the same" \
  "$(ran workflow_dispatch $main)" "build deploy references smoke"
expect "a manual run on another branch does not deploy, so it does not smoke" \
  "$(ran workflow_dispatch refs/heads/feature)" "build references"
expect "the weekly schedule runs the published-site check alone: no build, no deploy" \
  "$(ran schedule $main)" "smoke-published"
expect "a failed deploy is not smoke-checked" \
  "$(ran push $main deploy)" "build deploy references"
expect "a failed build deploys and checks nothing" \
  "$(ran push $main build)" "build references"
expect "failed references deploy and check nothing" \
  "$(ran push $main references)" "build references"

# --- least privilege, pins and the URL ----------------------------------

expect "the workflow's own permissions are contents: read" "$(top_scopes)" "contents read"
expect "deploy holds pages and id-token write only" "$(job deploy | scopes | tr '\n' ',')" "pages write,id-token write,"
for id in smoke smoke-published; do
  expect "$id holds contents: read and nothing else" "$(job "$id" | scopes)" "contents read"
  expect "$id has a timeout" "$([ -n "$(job "$id" | key 4 timeout-minutes)" ] && echo yes || echo no)" yes
  if job "$id" | grep -E 'uses:' | grep -vqE 'uses: [^ @]+@[0-9a-f]{40} # v[0-9]'; then
    fail "$id has an action not pinned by sha with a version comment" <(job "$id" | grep 'uses:')
  else ok "$id pins every action by sha"; fi
  expect "$id keeps no token in its checkout" "$(job "$id" | grep -c 'persist-credentials: false')" 1
  if job "$id" | grep -E '^ +- run:|^ +run:' | grep -q '\${{'; then
    fail "$id puts an expression in a run: line (pass it through env:)" <(job "$id" | grep 'run:')
  else ok "$id puts no expression in a run: line"; fi
done
expect "smoke needs deploy only" "$(job smoke | key 4 needs | tr -d '[] ')" deploy
expect "smoke takes the URL the deploy reported" "$(job smoke | key 6 PAGE_URL)" '${{ needs.deploy.outputs.page_url }}'
expect "smoke runs the check on that URL" "$(job smoke | grep -E '^      - run:' | sed 's/^ *- run: *//')" './hack/docs-live-check.sh "$PAGE_URL"'
expect "deploy publishes the deployed URL as an output" "$(job deploy | key 6 page_url)" '${{ steps.deployment.outputs.page_url }}'
expect "smoke-published has no needs" "$(job smoke-published | key 4 needs)" ""
expect "smoke-published checks the site_url of mkdocs.yml" "$(job smoke-published | grep -E '^      - run:' | sed 's/^ *- run: *//')" './hack/docs-live-check.sh'

# --- the schedule and the paths -----------------------------------------

cron=$(awk '/^on:/ { o = 1; next } o && /^[^ ]/ { exit } o && /cron:/ { v = $0; sub(/^ *- cron: */, "", v); sub(/ +#.*$/, "", v); print v }' "$wf")
if grep -qE "^'[0-9]+ [0-9]+ \* \* [0-6]'$" <<<"$cron"; then ok "the schedule is a weekly cron ($cron)"; else fail "the schedule is not a weekly cron: '$cron'" /dev/null; fi
paths=$(awk '/^on:/ { o = 1; next } o && /^[^ ]/ { exit } o && /^    paths:/ { p = 1; next } o && p && /^      - / { v = $0; sub(/^ *- */, "", v); sub(/ +#.*$/, "", v); gsub(/"/, "", v); print v; next } p { exit }' "$wf")
for p in README.md hack/docs-live-check.sh .github/workflows/docs.yml; do
  if grep -qxF "$p" <<<"$paths"; then ok "a push touching $p redeploys, and so re-checks"; else fail "$p is not among the push paths" /dev/null; fi
done

# --- the files and the Makefile -----------------------------------------

[ -x hack/docs-live-check.sh ] && ok "hack/docs-live-check.sh is executable" || fail "hack/docs-live-check.sh is not executable" /dev/null
for t in docs-live-check_test.sh docs-live-workflow_test.sh; do
  [ -x "hack/$t" ] && ok "hack/$t is executable" || fail "hack/$t is not executable" /dev/null
  if grep -qxF $'\t'"./hack/$t" Makefile; then ok "make hack-test runs $t"; else fail "Makefile's hack-test does not run $t" /dev/null; fi
done

echo
if grep -q '^FAIL' "$work/results"; then
  echo "docs-live-workflow_test: $(grep -c '^FAIL' "$work/results") failed" >&2
  exit 1
fi
echo "docs-live-workflow_test: all $(grep -c '^ok' "$work/results") cases passed"
