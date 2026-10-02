#!/usr/bin/env bash
# Tests that a kb-refresh bot PR gets CI (make hack-test, #11). The bot opens
# its PRs with the default GITHUB_TOKEN, and events that token creates start
# no pull_request or push workflow, so the PRs showed zero checks. GitHub
# exempts workflow_dispatch: each kb-refresh job dispatches ci.yml on its bot
# branch after it creates or updates the PR, and the checks attach to the
# PR's head commit. This pins both halves:
#  - kb-refresh.yml: each PR-opening job dispatches ci.yml on its branch
#    (PR sets) exactly when the PR was created or updated, with actions:
#    write on that job only;
#  - ci.yml: a workflow_dispatch run is not path-filtered (changes skips, and
#    release-check, action and kube treat that as "everything changed"), and
#    the registry job runs on it too, judging "touched registry/" against the
#    default branch. Its diff once ran in a depth-1 checkout, found no merge
#    base and read that as "untouched", so the job passed without running
#    anything; it now has full history and a failed diff fails the job.
# Expressions are evaluated with node (they use only ==, !=, !, && and ||,
# which JavaScript evaluates the same way); run blocks run the way Actions
# runs a step with no shell: (bash -e), against a stub gh or a scratch git
# remote. Offline.
set -euo pipefail
cd "$(dirname "$0")/.."

command -v node >/dev/null || { echo "kb-refresh-ci_test: node is required" >&2; exit 1; }
command -v git >/dev/null || { echo "kb-refresh-ci_test: git is required" >&2; exit 1; }

kb=.github/workflows/kb-refresh.yml
ci=.github/workflows/ci.yml
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
: >"$work/results"
ok() { echo "ok   $1" | tee -a "$work/results"; }
fail() { echo "FAIL $1" >&2; echo "FAIL $1" >>"$work/results"; }

# job <file> <id>: the job's lines, from `  <id>:` to the next top-level-of-
# jobs line.
job() { awk -v want="  $2:" '$0 == want { c = 1; print; next } c && /^  [^ ]/ { exit } c' "$1"; }
# step <name>: from a job on stdin, the step that starts `- name: <name>`.
step() { awk -v want="      - name: $1" '$0 == want { c = 1; print; next } c && (/^      - / || /^    [^ ]/) { exit } c'; }
# field <indent> <key>: from stdin, the first <key> at exactly <indent>
# spaces; a folded (>-) value is joined onto one line.
field() {
  awk -v pre="$(printf '%*s' "$1" '')$2: " -v ind="$1" '
    !found && index($0, pre) == 1 {
      v = substr($0, length(pre) + 1); found = 1
      if (v ~ /^>/) { fold = 1; v = ""; next }
      print v; exit
    }
    fold {
      match($0, /^ */)
      if ($0 !~ /^ *$/ && RLENGTH <= ind) { fold = 0; print v; exit }
      sub(/^ +/, ""); v = v (v == "" ? "" : " ") $0
    }
    END { if (fold) print v }
  '
}
# run_block: from a step on stdin, its `run: |` block, dedented.
run_block() { awk '/^        run: \|$/ { r = 1; next } r { if ($0 !~ /^ *$/ && $0 !~ /^          /) exit; print substr($0, 11) }'; }

# expr <expression> <context-json>: the expression evaluated against the
# context ({github, inputs, needs, steps}); prints the value. `a.b-c` is
# read as a property named b-c, as GitHub does.
expr() {
  [ -n "$1" ] || return 1
  node -e '
    const [src, ctxJSON] = process.argv.slice(1);
    const ctx = Object.assign({ github: {}, inputs: {}, needs: {}, steps: {} }, JSON.parse(ctxJSON));
    const e = src.trim().replace(/^\$\{\{([\s\S]*)\}\}$/, "$1")
      .replace(/\.([A-Za-z_]\w*(?:-[\w]+)+)/g, (_, k) => "[" + JSON.stringify(k) + "]");
    const v = new Function("github", "inputs", "needs", "steps", "cancelled", "always",
      "return (" + e + ");")(ctx.github, ctx.inputs, ctx.needs, ctx.steps, () => false, () => true);
    process.stdout.write(v === null || v === undefined ? "" : String(v));
  ' "$1" "$2"
}
# want_expr <label> <expression> <want> <context-json>
want_expr() {
  local got
  got=$(expr "$2" "$4") || { fail "$1: cannot evaluate '$2'"; return; }
  if [ "$got" = "$3" ]; then ok "$1"; else fail "$1: '$2' is '$got' (want '$3') for $4"; fi
}

# --- kb-refresh.yml: dispatch ci.yml on each bot branch ---------------------

sed '/^on:/q' "$kb" >"$work/header"
grep -q 'GITHUB_TOKEN' "$work/header" && grep -q 'gh workflow run ci.yml' "$work/header" &&
  ok "$kb's header explains the GITHUB_TOKEN gap and the dispatch" ||
  fail "$kb's header comment does not explain why and how ci.yml is dispatched"
if job "$kb" report-failure | grep -q 'actions: write' ||
  awk '/^permissions:/{p=1;next} p&&/^[^ ]/{exit} p' "$kb" | grep -q 'actions:'; then
  fail "actions: write is granted beyond the PR-opening jobs"
else
  ok "actions: write is granted to the PR-opening jobs only"
fi

mkdir -p "$work/bin"
cat >"$work/bin/gh" <<'EOF'
#!/usr/bin/env bash
echo "gh $* (token:${GH_TOKEN:+set} repo:${GH_REPO:-})" >>"$GH_LOG"
EOF
chmod +x "$work/bin/gh"

# check_dispatch <job> <branch>
check_dispatch() {
  local j=$1 branch=$2 id cond op want_if
  job "$kb" "$j" >"$work/$j.job"
  [ -s "$work/$j.job" ] || { fail "$j: no such job in $kb"; return; }

  awk '/^    permissions:/{p=1;next} p&&/^    [^ ]/{exit} p' "$work/$j.job" >"$work/$j.perms"
  if grep -qE '^      actions: write( |$)' "$work/$j.perms" && grep -qE '^      contents: write( |$)' "$work/$j.perms" &&
    grep -qE '^      pull-requests: write( |$)' "$work/$j.perms" && [ "$(grep -c '^      [a-z]' "$work/$j.perms")" = 3 ]; then
    ok "$j: job permissions are contents, pull-requests and actions write, nothing more"
  else
    fail "$j: job permissions are not exactly contents/pull-requests/actions: write: $(tr -s ' \n' ' ' <"$work/$j.perms")"
  fi

  step 'Open PR if anything changed' <"$work/$j.job" >"$work/$j.pr"
  grep -q 'uses: peter-evans/create-pull-request@' "$work/$j.pr" || { fail "$j: no create-pull-request step"; return; }
  [ "$(field 8 branch <"$work/$j.pr")" = "$branch" ] || [ "$(field 10 branch <"$work/$j.pr")" = "$branch" ] ||
    { fail "$j: the PR step's branch is not $branch"; return; }
  id=$(field 8 id <"$work/$j.pr")
  [ -n "$id" ] || { fail "$j: the create-pull-request step has no id, so nothing can read its outputs"; return; }

  step 'Run CI on the PR' <"$work/$j.job" >"$work/$j.ci"
  [ -s "$work/$j.ci" ] || { fail "$j: no 'Run CI on the PR' step after the PR step"; return; }
  # It must come after the PR step.
  [ "$(grep -n -- '- name: Open PR if anything changed' "$work/$j.job" | cut -d: -f1)" -lt \
    "$(grep -n -- '- name: Run CI on the PR' "$work/$j.job" | cut -d: -f1)" ] || { fail "$j: CI is dispatched before the PR step"; return; }

  cond=$(field 8 if <"$work/$j.ci")
  [ -n "$cond" ] || { fail "$j: the dispatch step has no if:"; return; }
  # Every output it reads is the PR step's pull-request-operation.
  if grep -oE 'steps\.[A-Za-z0-9_-]+\.outputs\.[A-Za-z0-9_-]+' <<<"$cond" | sort -u | grep -vqx "steps\.$id\.outputs\.pull-request-operation"; then
    fail "$j: the dispatch step's if: reads something other than steps.$id.outputs.pull-request-operation: $cond"
    return
  fi
  for op in created updated none closed ''; do
    case "$op" in created | updated) want_if=true ;; *) want_if=false ;; esac
    want_expr "$j: dispatch when the PR operation is '${op:-<unset>}': $want_if" "$cond" "$want_if" \
      "{\"steps\":{\"$id\":{\"outputs\":{\"pull-request-operation\":\"$op\"}}}}"
  done

  [ "$(field 10 BRANCH <"$work/$j.ci")" = "\${{ steps.$id.outputs.pull-request-branch }}" ] &&
    [ "$(field 10 GH_TOKEN <"$work/$j.ci")" = '${{ github.token }}' ] &&
    [ "$(field 10 GH_REPO <"$work/$j.ci")" = '${{ github.repository }}' ] ||
    { fail "$j: the dispatch step's env is not BRANCH from steps.$id, GH_TOKEN github.token, GH_REPO github.repository"; return; }

  run_block <"$work/$j.ci" >"$work/$j.sh"
  : >"$work/$j.log"
  if PATH="$work/bin:$PATH" GH_LOG="$work/$j.log" GH_TOKEN=stub GH_REPO=o/r BRANCH="$branch" HEAD_SHA=abc123 \
    bash --noprofile --norc -e "$work/$j.sh" >"$work/$j.out" 2>&1; then
    if [ "$(cat "$work/$j.log")" = "gh workflow run ci.yml --ref $branch -f full-matrix=false (token:set repo:o/r)" ]; then
      ok "$j: dispatches ci.yml (PR sets) on $branch"
    else
      fail "$j: gh was called as: $(cat "$work/$j.log")"
    fi
  else
    fail "$j: the dispatch step failed: $(cat "$work/$j.out")"
  fi
}
check_dispatch api-lifecycle bot/kb-refresh-api
check_dispatch registry bot/kb-refresh-registry

# --- ci.yml: a dispatched run is the whole PR suite --------------------------

pr='{"github":{"event_name":"pull_request","base_ref":"main","event":{"repository":{"default_branch":"main"}}}}'
dispatch='{"github":{"event_name":"workflow_dispatch","base_ref":"","event":{"repository":{"default_branch":"main"}}},"inputs":{"full-matrix":false}}'
redo='{"github":{"event_name":"workflow_dispatch","base_ref":"","event":{"repository":{"default_branch":"main"}}},"inputs":{"release":true}}'
push='{"github":{"event_name":"push","ref":"refs/heads/main","base_ref":""}}'
sched='{"github":{"event_name":"schedule","base_ref":""}}'
# A dispatched run's needs: changes skipped, kube-matrix done.
dneeds='"needs":{"changes":{"result":"skipped","outputs":{}},"kube-matrix":{"result":"success","outputs":{"minors":"[]"}}}'

want_expr "changes is skipped on workflow_dispatch (no paths to filter)" "$(job "$ci" changes | field 4 if)" false "$dispatch"
for j in release-check action kube; do
  want_expr "$j runs on a dispatched run with changes skipped" "$(job "$ci" "$j" | field 4 if)" true "${dispatch%\}},$dneeds}"
done

job "$ci" registry >"$work/registry.job"
regif=$(field 4 if <"$work/registry.job")
want_expr "registry runs on pull_request" "$regif" true "$pr"
want_expr "registry runs on workflow_dispatch (a kb-refresh bot branch)" "$regif" true "$dispatch"
want_expr "registry skips a release re-dispatch" "$regif" false "$redo"
want_expr "registry skips push" "$regif" false "$push"
want_expr "registry skips the schedule" "$regif" false "$sched"
grep -qE '^          fetch-depth: 0( |$)' "$work/registry.job" &&
  ok "registry checks out full history (its three-dot diff needs the merge base)" ||
  fail "registry's checkout is shallow: the diff against the base finds no merge base"

step 'Did this branch touch registry/?' <"$work/registry.job" >"$work/changed.step"
run_block <"$work/changed.step" >"$work/changed.sh"
[ -s "$work/changed.sh" ] || { fail "no 'Did this branch touch registry/?' run block in $ci's registry job"; }
base_expr=$(field 10 BASE <"$work/changed.step")
want_expr "registry diffs a PR against its base" "$base_expr" main "$pr"
want_expr "registry diffs a dispatched branch against the default branch" "$base_expr" main "$dispatch"

# A scratch remote: main touched registry/ before and after the bot branches
# forked; the registry branch changes registry/, the api branch does not.
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.invalid \
  GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.invalid
o=$work/origin
commit() { mkdir -p "$o/$(dirname "$1")" && echo "$2" >"$o/$1" && git -C "$o" add -A && git -C "$o" commit -qm "$1"; }
git init -q -b main "$o"
commit registry/data/a.yaml 1
commit README.md 1
git -C "$o" branch bot/kb-refresh-registry
git -C "$o" branch bot/kb-refresh-api
commit registry/data/b.yaml 2
git -C "$o" checkout -q bot/kb-refresh-registry && commit registry/data/a.yaml 2
git -C "$o" checkout -q bot/kb-refresh-api && commit internal/kb/data/x.json 2
git -C "$o" checkout -q --detach main
git -C "$o" merge -q --no-ff -m merge bot/kb-refresh-registry && git -C "$o" update-ref refs/pull/1/merge HEAD
git -C "$o" checkout -q --detach main
git -C "$o" merge -q --no-ff -m merge bot/kb-refresh-api && git -C "$o" update-ref refs/pull/2/merge HEAD
git -C "$o" checkout -q main

# touched <label> <ref> <depth|full> <want: true|false|fails>
touched() {
  local label=$1 ref=$2 depth=$3 want=$4 d got=0
  d=$work/co-$(echo "$label" | tr -c 'a-z0-9' -)
  # As actions/checkout does: fetch the ref (the PR's merge ref, or the
  # dispatched branch) at the job's depth and check it out detached.
  if [ "$depth" = full ]; then
    git clone -q "file://$o" "$d" && git -C "$d" fetch -q origin "$ref"
  else
    git clone -q --depth "$depth" "file://$o" "$d" && git -C "$d" fetch -q --depth "$depth" origin "$ref"
  fi
  git -C "$d" checkout -q --detach FETCH_HEAD
  : >"$d.out"
  (cd "$d" && GITHUB_OUTPUT="$d.out" BASE=main bash --noprofile --norc -e "$work/changed.sh") >"$d.log" 2>&1 || got=$?
  if [ "$want" = fails ]; then
    if [ "$got" -ne 0 ] && ! grep -q '^registry=' "$d.out"; then ok "$label"; else fail "$label: exit $got, output $(cat "$d.out")"; fi
  elif [ "$got" -eq 0 ] && [ "$(cat "$d.out")" = "registry=$want" ]; then
    ok "$label"
  else
    fail "$label: exit $got, output '$(cat "$d.out")' (want registry=$want): $(tail -3 "$d.log")"
  fi
}
touched "dispatched on the registry bot branch: registry touched" bot/kb-refresh-registry full true
touched "dispatched on the api bot branch: registry untouched (main's later registry change is not the branch's)" bot/kb-refresh-api full false
touched "dispatched on main: nothing to check" main full false
touched "PR merging the registry branch: registry touched" refs/pull/1/merge full true
touched "PR merging the api branch: registry untouched" refs/pull/2/merge full false
touched "a shallow checkout fails the job instead of reading as untouched" refs/pull/1/merge 1 fails

pass=$(grep -c '^ok' "$work/results" || true)
failed=$(grep -c '^FAIL' "$work/results" || true)
echo "kb-refresh-ci_test: $pass passed, $failed failed"
[ "$failed" -eq 0 ] && [ "$pass" -gt 0 ]
