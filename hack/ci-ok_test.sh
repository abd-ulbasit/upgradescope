#!/usr/bin/env bash
# Tests ci.yml's ci-ok job (run by `make hack-test`). ci-ok is the one check
# a branch ruleset can require (#58): the kube job names change with the
# matrix and most jobs skip on some events, so none of them is a stable
# required check. ci-ok is only as good as its needs list, so this asserts
# it names every other job in ci.yml: a job added without it would never
# block a merge. Offline.
set -euo pipefail
cd "$(dirname "$0")/.."

ci=.github/workflows/ci.yml

# Job ids: two-space-indented keys under the top-level `jobs:`.
jobs=$(awk '/^jobs:/{j=1;next} j&&/^[^ #]/{j=0} j&&/^  [a-z0-9_-]+:[ ]*$/{sub(/^  /,"");sub(/:.*/,"");print}' "$ci" | sort)
grep -qx ci-ok <<<"$jobs" || { echo "FAIL $ci has no ci-ok job" >&2; exit 1; }

# ci-ok's needs, written as a flow list: needs: [a, b, ...] (one line).
needs=$(awk '/^  ci-ok:/{c=1;next} c&&/^  [^ ]/{c=0} c&&/^    needs:/{sub(/^    needs: *\[/,"");sub(/\].*/,"");print}' "$ci" |
  tr ',' '\n' | tr -d ' ' | sed '/^$/d' | sort)
[ -n "$needs" ] || { echo "FAIL ci-ok has no one-line needs: [...] list" >&2; exit 1; }

want=$(echo "$jobs" | grep -vx ci-ok)
missing=$(comm -23 <(echo "$want") <(echo "$needs"))
extra=$(comm -13 <(echo "$want") <(echo "$needs"))
if [ -n "$missing$extra" ]; then
  [ -z "$missing" ] || echo "FAIL ci-ok does not need: $(echo $missing)" >&2
  [ -z "$extra" ] || echo "FAIL ci-ok needs jobs that do not exist: $(echo $extra)" >&2
  exit 1
fi
echo "ok   $ci ci-ok needs every other job ($(echo "$want" | wc -l | tr -d ' '))"

# It must run when a job it needs failed or was cancelled (a plain needs
# would skip it, and a skipped required check counts as passing).
grep -q "^    if: always()" <<<"$(awk '/^  ci-ok:/{c=1;next} c&&/^  [^ ]/{c=0} c' "$ci")" ||
  { echo "FAIL ci-ok does not run with if: always()" >&2; exit 1; }
echo "ok   ci-ok runs with if: always()"

# ---- the verdict ----------------------------------------------------------
# ci-ok's step runs hack/ci-ok.sh with the needs context and the event.
steps=$(awk '/^  ci-ok:/{c=1;next} c&&/^  [^ ]/{c=0} c' "$ci")
grep -qF 'NEEDS: ${{ toJSON(needs) }}' <<<"$steps" && grep -qF 'EVENT: ${{ github.event_name }}' <<<"$steps" &&
  grep -qE '^        run: hack/ci-ok\.sh$' <<<"$steps" ||
  { echo "FAIL ci-ok does not run hack/ci-ok.sh with NEEDS (toJSON(needs)) and EVENT (github.event_name)" >&2; exit 1; }
echo "ok   ci-ok runs hack/ci-ok.sh on the needs context and the event"
grep -qF 'sparse-checkout: hack/ci-ok.sh' <<<"$steps" || { echo "FAIL ci-ok does not check out hack/ci-ok.sh" >&2; exit 1; }
echo "ok   ci-ok checks out the script it runs"

command -v jq >/dev/null || { echo "ci-ok_test: jq is required" >&2; exit 1; }
rc=0
# needs_json <changes outputs json> [job=result ...]: every job in ci-ok's
# needs, success unless overridden, the changes job with those outputs.
needs_json() {
  local outputs=$1 ov
  shift
  ov=$(printf '%s\n' "$@" | jq -R 'select(length > 0) | split("=") | {(.[0]): .[1]}' | jq -sc 'add // {}')
  jq -nc --arg jobs "$needs" --argjson o "$outputs" --argjson ov "$ov" '
    ($jobs | split("\n") | map(select(length > 0))) as $js
    | reduce $js[] as $j ({}; .[$j] = {result: "success", outputs: {}})
    | .changes.outputs = $o
    | reduce ($ov | to_entries[]) as $e (.; .[$e.key].result = $e.value)'
}
# verdict <name> <pass|fail> <event> <changes outputs> [job=result ...]
verdict() {
  local name=$1 want=$2 event=$3 outputs=$4 got=pass
  shift 4
  NEEDS=$(needs_json "$outputs" "$@") EVENT=$event hack/ci-ok.sh >"${TMPDIR:-/tmp}/ci-ok.out.$$" 2>&1 || got=fail
  if [ "$got" = "$want" ]; then
    echo "ok   $name"
  else
    echo "FAIL $name: wanted $want, got $got: $(tail -4 "${TMPDIR:-/tmp}/ci-ok.out.$$" | tr '\n' ' ')" >&2
    rc=1
  fi
  rm -f "${TMPDIR:-/tmp}/ci-ok.out.$$"
}

docs_only='{"go":"false","cross":"false","test-scope":"none","e2e":"false","release":"false","action":"false"}'
docs_scope='{"go":"false","cross":"false","test-scope":"docs","e2e":"false","release":"false","action":"false"}'
code='{"go":"true","cross":"true","test-scope":"all","e2e":"true","release":"false","action":"true"}'
code_nocross='{"go":"true","cross":"false","test-scope":"all","e2e":"true","release":"false","action":"false"}'
off="test=skipped test-heap=skipped build=skipped cross-build=skipped examples=skipped images=skipped vuln=skipped pg-conformance=skipped kube=skipped envtest=skipped release-check=skipped action=skipped"

# A pull request that changes nothing a gated job reads: they are skipped, and
# that is fine because the changes job ran and said so.
verdict "PR touching no code: every gated job skipped, ci-ok passes" pass pull_request "$docs_only" $off
verdict "PR touching no code: the gated jobs may also have run" pass pull_request "$docs_only"
verdict "PR touching docs only: the docs readers run, the rest are skipped" pass pull_request "$docs_scope" $(sed 's/test=skipped//' <<<"$off")
verdict "PR touching Go code: every job ran" pass pull_request "$code"
verdict "PR touching Go code but no compile input: cross-build skipped" pass pull_request "$code_nocross" cross-build=skipped action=skipped release-check=skipped

# The case a reviewer will try: a gated job skipped though the changes job
# said it is needed, whichever job it is.
for job in test test-heap build examples images vuln pg-conformance kube envtest; do
  verdict "PR touching code: $job skipped though needed fails ci-ok" fail pull_request "$code" "$job=skipped"
done
verdict "PR touching code: cross-build skipped though needed fails ci-ok" fail pull_request "$code" cross-build=skipped
verdict "PR touching docs only: test skipped though the docs readers are needed fails ci-ok" fail pull_request "$docs_scope" test=skipped
verdict "PR touching docs only: test-heap skipped is fine, build skipped is fine, test skipped is not" fail pull_request "$docs_scope" test-heap=skipped build=skipped test=skipped

# The changes job's own failure modes: a gated job that was skipped for the
# want of an answer must not turn green.
verdict "changes failed, gated jobs skipped: ci-ok fails" fail pull_request '{}' changes=failure $off
verdict "changes cancelled, gated jobs skipped: ci-ok fails" fail pull_request '{}' changes=cancelled $off
verdict "changes skipped on a PR, gated jobs skipped: ci-ok fails" fail pull_request "$docs_only" changes=skipped $off
verdict "changes failed though every other job passed: ci-ok fails" fail pull_request "$code" changes=failure
verdict "changes succeeded with no outputs, test-heap skipped: ci-ok fails" fail pull_request '{}' test-heap=skipped
verdict "changes output is garbage, test-heap skipped: ci-ok fails" fail pull_request '{"go":"maybe"}' test-heap=skipped
verdict "changes says the PR needs code jobs, but nothing ran: ci-ok fails" fail pull_request "$code" $off

# Failed and cancelled gated jobs fail, needed or not.
for r in failure cancelled; do
  verdict "PR touching no code: a gated job that $r fails" fail pull_request "$docs_only" test-heap=$r
  verdict "PR touching code: test $r fails" fail pull_request "$code" test=$r
  verdict "PR touching code: a non-gated job that $r fails" fail pull_request "$code" lint=$r
done

# A push to main runs everything (the gates apply to pull requests) but keeps
# the kind e2e and envtest skip after a docs-only push.
push_all='{"go":"true","cross":"true","test-scope":"all","e2e":"true","release":"true","action":"true"}'
push_docs='{"go":"true","cross":"true","test-scope":"all","e2e":"false","release":"false","action":"false"}'
verdict "push to main: every job ran" pass push "$push_all"
verdict "push to main after a docs-only change: kube and envtest skipped" pass push "$push_docs" kube=skipped envtest=skipped release-check=skipped action=skipped
for job in test test-heap build cross-build examples images vuln pg-conformance; do
  verdict "push to main: $job skipped fails ci-ok" fail push "$push_docs" "$job=skipped"
done

# On a push the gates do not apply: a gated job other than the docs-only skips
# a push keeps (e2e, release) must run even if the changes outputs say
# otherwise, so ci-ok does not lean on hack/ci-changes.sh printing go=true.
# (Dropping `[ "$changes" = success ] &&` from the skip branch of ci-ok.sh is
# an equivalent mutant: the results loop and, on a pull request, the changes
# check already fail a changes job that did not succeed.)
push_lying='{"go":"false","cross":"false","test-scope":"none","e2e":"false","release":"false","action":"false"}'
for job in test test-heap build cross-build examples images vuln pg-conformance; do
  verdict "push to main with changes saying not needed: $job skipped still fails ci-ok" fail push "$push_lying" "$job=skipped"
done
verdict "push to main: kube, envtest and release-check may skip on e2e=false and release=false" pass push "$push_lying" kube=skipped envtest=skipped release-check=skipped action=skipped
verdict "push to main with changes failed: kube skipped fails ci-ok" fail push "$push_lying" changes=failure kube=skipped
verdict "push to main with e2e=true: kube skipped fails ci-ok" fail push "$push_all" kube=skipped
verdict "dispatch with outputs claiming not needed (changes ran): test-heap skipped fails ci-ok" fail workflow_dispatch "$push_lying" test-heap=skipped

# Schedule: the changes job does not run; the jobs marked as skipped on a
# schedule are skipped, the rest must run.
verdict "schedule: the PR-only jobs skipped, the rest ran" pass schedule '{}' changes=skipped test=skipped test-heap=skipped build=skipped cross-build=skipped examples=skipped images=skipped release-check=skipped lint=skipped registry=skipped
for job in vuln pg-conformance kube envtest; do
  verdict "schedule: $job skipped fails ci-ok" fail schedule '{}' changes=skipped "$job=skipped"
done

# Dispatch and release: changes is skipped, and every gated job runs.
verdict "dispatch: every job ran (changes skipped)" pass workflow_dispatch '{}' changes=skipped registry=skipped
verdict "release (a tag push through workflow_call): every job ran, action and registry skipped by their own ifs" pass push '{}' changes=skipped action=skipped registry=skipped
for job in test test-heap build cross-build examples images vuln pg-conformance kube envtest release-check; do
  verdict "release: $job skipped fails ci-ok" fail push '{}' changes=skipped "$job=skipped"
  verdict "dispatch: $job skipped fails ci-ok" fail workflow_dispatch '{}' changes=skipped "$job=skipped"
done

# The jobs no path gates must succeed: skipped is not a pass for them (a stray
# `if` cannot quietly drop lint or the repository checks), but the ones that
# skip on a schedule by their own `if` may be skipped there.
for job in lint repo-checks web helm notices kb-freshness kube-matrix pg-matrix; do
  verdict "PR: $job skipped fails ci-ok" fail pull_request "$code" "$job=skipped"
  verdict "push to main: $job skipped fails ci-ok" fail push "$push_all" "$job=skipped"
  verdict "release: $job skipped fails ci-ok" fail push '{}' changes=skipped "$job=skipped"
done
for job in lint repo-checks web helm notices; do
  verdict "schedule: $job skipped is fine (it skips there by its own if)" pass schedule '{}' changes=skipped test=skipped test-heap=skipped build=skipped cross-build=skipped examples=skipped images=skipped release-check=skipped "$job=skipped"
done
for job in kb-freshness kube-matrix pg-matrix; do
  verdict "schedule: $job skipped fails ci-ok (it runs on a schedule)" fail schedule '{}' changes=skipped test=skipped test-heap=skipped build=skipped cross-build=skipped examples=skipped images=skipped release-check=skipped "$job=skipped"
done

# A gated job that is not among ci-ok's needs cannot be judged.
doc=$(needs_json "$code" | jq -c 'del(.["test-heap"])')
NEEDS=$doc EVENT=pull_request hack/ci-ok.sh >/dev/null 2>&1 && { echo "FAIL ci-ok passed with test-heap missing from its needs" >&2; rc=1; } || echo "ok   a gated job missing from needs fails ci-ok"

# ---- the gated jobs are the ones hack/ci-ok.sh lists -----------------------
# job_block <job>: the job's lines. job_if <job>: its `if:` expression.
job_block() { awk -v j="$1" '$0 == "  " j ":" { c = 1; next } c && /^  [a-z0-9_-]+:[ ]*$/ { c = 0 } c' "$ci"; }
job_if() { job_block "$1" | awk '/^    if:/ { c = 1; sub(/^    if: *(>-)? */, ""); print; next } c && /^    [a-z-]+:/ { c = 0 } c'; }
table=$(awk '/^GATED=/{c=1;next} c&&/^'"'"'/{c=0} c&&NF' hack/ci-ok.sh)
[ -n "$table" ] || { echo "FAIL hack/ci-ok.sh has no GATED table" >&2; exit 1; }
# Jobs whose if reads one of the changes job's gate outputs (not release/action
# as a list: those two are in the table only for release-check).
in_ci=""
for j in $jobs; do
  if job_if "$j" | grep -qE 'needs\.changes\.outputs\.(go|cross|test-scope|e2e|release)\b'; then in_ci="$in_ci $j"; fi
done
in_table=$(awk '{print $1}' <<<"$table" | sort | tr '\n' ' ')
in_ci=$(tr ' ' '\n' <<<"$in_ci" | sed '/^$/d' | sort | tr '\n' ' ')
if [ "$in_ci" != "$in_table" ]; then
  echo "FAIL the jobs gated on the changes job in ci.yml ($in_ci) differ from hack/ci-ok.sh's GATED table ($in_table)" >&2
  rc=1
else
  echo "ok   hack/ci-ok.sh's GATED table is the set of jobs ci.yml gates on the changes job"
fi
while read -r job out notneeded onsched; do
  cond=$(job_if "$job")
  grep -qE "needs\.changes\.outputs\.$out\b" <<<"$cond" || { echo "FAIL $job's if does not read needs.changes.outputs.$out, which ci-ok judges it by" >&2; rc=1; }
  grep -qF "needs.changes.result != 'success'" <<<"$cond" || { echo "FAIL $job's if does not run it when the changes job did not succeed (skipped on a schedule, dispatch or release; failed on a push to main, which keeps its old behaviour)" >&2; rc=1; }
  grep -qF '!cancelled()' <<<"$cond" || { echo "FAIL $job's if has no !cancelled(): a skipped or failed changes job would skip it with no status check" >&2; rc=1; }
  job_block "$job" | grep -qE '^    needs:.*\bchanges\b' || { echo "FAIL $job does not need changes" >&2; rc=1; }
  if [ "$onsched" = yes ]; then
    grep -qF "github.event_name != 'schedule'" <<<"$cond" || { echo "FAIL $job is marked skipped-on-schedule but its if runs it on a schedule" >&2; rc=1; }
  else
    ! grep -qF "github.event_name != 'schedule'" <<<"$cond" || { echo "FAIL $job is marked as running on a schedule but its if skips it there" >&2; rc=1; }
  fi
done <<<"$table"
# The ungated lists name real jobs, whose ifs agree with the schedule split.
for job in $(sed -n "s/^UNGATED_OFF_SCHEDULE='\(.*\)'/\1/p" hack/ci-ok.sh); do
  grep -qx "$job" <<<"$jobs" || { echo "FAIL hack/ci-ok.sh names $job, which is not a job in ci.yml" >&2; rc=1; }
  job_if "$job" | grep -qF "github.event_name != 'schedule'" || { echo "FAIL $job is listed as skipped on a schedule, but its if does not skip it there" >&2; rc=1; }
done
for job in $(sed -n "s/^UNGATED_ALWAYS='\(.*\)'/\1/p" hack/ci-ok.sh); do
  grep -qx "$job" <<<"$jobs" || { echo "FAIL hack/ci-ok.sh names $job, which is not a job in ci.yml" >&2; rc=1; }
  if job_if "$job" | grep -qF "github.event_name != 'schedule'"; then echo "FAIL $job is listed as running on a schedule, but its if skips it there" >&2; rc=1; fi
done
[ "$rc" = 0 ] && echo "ok   every gated job reads its output, runs when changes was skipped, and agrees with the schedule column"
exit "$rc"
