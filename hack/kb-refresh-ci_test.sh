#!/usr/bin/env bash
# Tests that a kb-refresh bot PR gets CI that the PR counts (make hack-test,
# #11). The bot opens its PRs with the default GITHUB_TOKEN as
# github-actions[bot]; GitHub creates the PR's pull_request runs but holds
# them for approval (conclusion action_required), so the PR showed no checks
# and the required ci-ok never reported. A dispatched ci.yml run (#151) is no
# substitute: its checks land on the commit, but the PR's check rollup
# ignores them. So each PR-opening job approves its PR's held runs. This pins:
#  - kb-refresh.yml (#244): the jobs that run code (go get of the newest
#    k8s.io modules, make gen-kb, go test, make eol-sync) hold contents: read
#    only, keep no token in their checkout and save no Go cache; the *-pr
#    jobs that hold the writes run no repository or dependency code, and
#    apply only a patch of their PR's paths (refusing anything else,
#    renames, copies and symlinks, against crafted patches);
#  - kb-refresh.yml: each PR-opening job runs 'Approve the PR's CI runs'
#    exactly when the PR was created or updated, with actions: write on that
#    job only; against a stub gh it polls the head commit's pull_request
#    runs (bounded), approves each held one, and is fail-soft: no runs, a
#    failed listing or a refused approval is a warning naming the
#    maintainer's commands, never a failed job;
#  - ci.yml: a workflow_dispatch run (kept for running CI on a branch by
#    hand) is not path-filtered (changes skips, and release-check, action
#    and kube treat that as "everything changed"), and the registry job runs
#    on it too, judging "touched registry/" against the default branch. Its
#    diff once ran in a depth-1 checkout, found no merge base and read that
#    as "untouched", so the job passed without running anything; it now has
#    full history and a failed diff fails the job.
# Expressions are evaluated with node (they use only ==, !=, !, && and ||,
# which JavaScript evaluates the same way); run blocks run the way Actions
# runs a step with no shell: (bash -e), against stub gh and sleep commands or
# a scratch git remote. Offline.
set -euo pipefail
cd "$(dirname "$0")/.."

command -v node >/dev/null || { echo "kb-refresh-ci_test: node is required" >&2; exit 1; }
command -v git >/dev/null || { echo "kb-refresh-ci_test: git is required" >&2; exit 1; }
command -v jq >/dev/null || { echo "kb-refresh-ci_test: jq is required (the stub gh applies --jq with it)" >&2; exit 1; }

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

# --- kb-refresh.yml: approve each bot PR's held CI runs ----------------------

sed '/^on:/q' "$kb" >"$work/header"
grep -q 'GITHUB_TOKEN' "$work/header" && grep -q 'action_required' "$work/header" && grep -q '/approve' "$work/header" &&
  ok "$kb's header explains the held runs and the approval" ||
  fail "$kb's header comment does not explain why and how the PR's held CI runs are approved"
if job "$kb" report-failure | grep -q 'actions: write' ||
  awk '/^permissions:/{p=1;next} p&&/^[^ ]/{exit} p' "$kb" | grep -q 'actions:'; then
  fail "actions: write is granted beyond the PR-opening jobs"
else
  ok "actions: write is granted to the PR-opening jobs only"
fi

mkdir -p "$work/bin"
# gh: logs each call. `gh api <runs url> --jq F` answers with line n of
# $STUB_RUNS on the n-th listing (the last line once they run out; a line
# FAIL is an HTTP 500) through jq -r F, as gh --jq does; `gh api -X POST
# .../approve` succeeds unless STUB_APPROVE=403.
cat >"$work/bin/gh" <<'STUB'
#!/usr/bin/env bash
echo "gh $* (token:${GH_TOKEN:+set} repo:${GH_REPO:-})" >>"$GH_LOG"
[ "$1" = api ] || { echo "stub gh: unexpected call: $*" >&2; exit 2; }
if [ "$2" = -X ]; then
  [ "$3 ${4##*/}" = "POST approve" ] || { echo "stub gh: unexpected call: $*" >&2; exit 2; }
  [ "${STUB_APPROVE:-ok}" = ok ] && { echo '{}'; exit 0; }
  echo 'gh: Resource not accessible by integration (HTTP 403)' >&2
  exit 1
fi
filter='.'
while [ $# -gt 0 ]; do [ "$1" = --jq ] && filter=$2; shift; done
n=$(($(cat "$GH_LOG.n" 2>/dev/null || echo 0) + 1))
echo "$n" >"$GH_LOG.n"
total=$(wc -l <"$STUB_RUNS")
line=$(sed -n "$((n < total ? n : total))p" "$STUB_RUNS")
[ "$line" != FAIL ] || { echo 'gh: Server Error (HTTP 500)' >&2; exit 1; }
jq -r "$filter" <<<"$line"
STUB
cat >"$work/bin/sleep" <<'STUB'
#!/usr/bin/env bash
echo "$*" >>"$SLEEP_LOG"
STUB
chmod +x "$work/bin/gh" "$work/bin/sleep"

# Listings: held runs are completed with conclusion action_required (seen
# live on PR #152); status action_required is accepted too.
none='{"total_count":0,"workflow_runs":[]}'
held='{"total_count":2,"workflow_runs":[{"id":101,"name":"ci","status":"completed","conclusion":"action_required"},{"id":102,"name":"docs","status":"action_required","conclusion":null}]}'
held1='{"total_count":1,"workflow_runs":[{"id":101,"name":"ci","status":"completed","conclusion":"action_required"}]}'
running='{"total_count":2,"workflow_runs":[{"id":101,"name":"ci","status":"queued","conclusion":null},{"id":102,"name":"docs","status":"in_progress","conclusion":null}]}'

# approve_case <job> <branch> <label> <approve: ok|403> <listing>...: runs
# the job's approve block; leaves its output, gh log, sleeps and summary in
# $work/case.*. Fails the case if the block exits non-zero.
approve_case() {
  local j=$1 branch=$2 label=$3 approve=$4
  shift 4
  printf '%s\n' "$@" >"$work/case.runs"
  rm -f "$work/case.gh" "$work/case.gh.n" "$work/case.sleep" "$work/case.summary"
  touch "$work/case.gh" "$work/case.sleep" "$work/case.summary"
  echo 0 >"$work/case.gh.n"
  if PATH="$work/bin:$PATH" GH_LOG="$work/case.gh" SLEEP_LOG="$work/case.sleep" STUB_RUNS="$work/case.runs" \
    STUB_APPROVE="$approve" GITHUB_STEP_SUMMARY="$work/case.summary" GH_TOKEN=stub GH_REPO=o/r \
    BRANCH="$branch" HEAD_SHA=abc123 bash --noprofile --norc -e "$work/$j.sh" >"$work/case.out" 2>&1; then
    return 0
  fi
  fail "$j: $label: the step failed (it must be fail-soft): $(tail -5 "$work/case.out")"
  return 1
}
approvals() { grep -oE 'api -X POST [^ ]+' "$work/case.gh" | sort | tr '\n' ' ' | sed 's/ $//' || true; }
listings() { cat "$work/case.gh.n"; }
slept() { awk '{ s += $1 } END { print s + 0 }' "$work/case.sleep"; }

# check_approve <job> <branch>
check_approve() {
  local j=$1 branch=$2 id cond op want_if got
  job "$kb" "$j" >"$work/$j.job"
  [ -s "$work/$j.job" ] || { fail "$j: no such job in $kb"; return; }

  awk '/^    permissions:/{p=1;next} p&&/^    [^ ]/{exit} p' "$work/$j.job" >"$work/$j.perms"
  if grep -qE '^      actions: write( |$)' "$work/$j.perms" && grep -qE '^      contents: write( |$)' "$work/$j.perms" &&
    grep -qE '^      pull-requests: write( |$)' "$work/$j.perms" && [ "$(grep -c '^      [a-z]' "$work/$j.perms")" = 3 ]; then
    ok "$j: job permissions are contents, pull-requests and actions write, nothing more"
  else
    fail "$j: job permissions are not exactly contents/pull-requests/actions: write: $(tr -s ' \n' ' ' <"$work/$j.perms")"
  fi
  if grep -q 'gh workflow run' "$work/$j.job"; then
    fail "$j: still dispatches a workflow; a dispatched run's checks do not count on the PR"
  else
    ok "$j: dispatches no workflow"
  fi

  step 'Open PR if anything changed' <"$work/$j.job" >"$work/$j.pr"
  grep -q 'uses: peter-evans/create-pull-request@' "$work/$j.pr" || { fail "$j: no create-pull-request step"; return; }
  [ "$(field 8 branch <"$work/$j.pr")" = "$branch" ] || [ "$(field 10 branch <"$work/$j.pr")" = "$branch" ] ||
    { fail "$j: the PR step's branch is not $branch"; return; }
  id=$(field 8 id <"$work/$j.pr")
  [ -n "$id" ] || { fail "$j: the create-pull-request step has no id, so nothing can read its outputs"; return; }

  step "Approve the PR's CI runs" <"$work/$j.job" >"$work/$j.ci"
  [ -s "$work/$j.ci" ] || { fail "$j: no 'Approve the PR's CI runs' step"; return; }
  [ "$(grep -n -- '- name: Open PR if anything changed' "$work/$j.job" | cut -d: -f1)" -lt \
    "$(grep -n -- "- name: Approve the PR's CI runs" "$work/$j.job" | cut -d: -f1)" ] ||
    { fail "$j: the approve step runs before the PR step"; return; }

  cond=$(field 8 if <"$work/$j.ci")
  [ -n "$cond" ] || { fail "$j: the approve step has no if:"; return; }
  # Every output it reads is the PR step's pull-request-operation.
  if grep -oE 'steps\.[A-Za-z0-9_-]+\.outputs\.[A-Za-z0-9_-]+' <<<"$cond" | sort -u | grep -vqx "steps\.$id\.outputs\.pull-request-operation"; then
    fail "$j: the approve step's if: reads something other than steps.$id.outputs.pull-request-operation: $cond"
    return
  fi
  for op in created updated none closed ''; do
    case "$op" in created | updated) want_if=true ;; *) want_if=false ;; esac
    want_expr "$j: approve when the PR operation is '${op:-<unset>}': $want_if" "$cond" "$want_if" \
      "{\"steps\":{\"$id\":{\"outputs\":{\"pull-request-operation\":\"$op\"}}}}"
  done

  [ "$(field 10 BRANCH <"$work/$j.ci")" = "\${{ steps.$id.outputs.pull-request-branch }}" ] &&
    [ "$(field 10 HEAD_SHA <"$work/$j.ci")" = "\${{ steps.$id.outputs.pull-request-head-sha }}" ] &&
    [ "$(field 10 GH_TOKEN <"$work/$j.ci")" = '${{ github.token }}' ] &&
    [ "$(field 10 GH_REPO <"$work/$j.ci")" = '${{ github.repository }}' ] ||
    { fail "$j: the approve step's env is not BRANCH and HEAD_SHA from steps.$id, GH_TOKEN github.token, GH_REPO github.repository"; return; }

  run_block <"$work/$j.ci" >"$work/$j.sh"
  [ -s "$work/$j.sh" ] || { fail "$j: the approve step has no run block"; return; }

  # Runs not created yet, a failed listing, then two held runs: each is
  # approved once, the next listing shows them running, and it stops.
  if approve_case "$j" "$branch" "held runs" ok "$none" FAIL "$held" "$running"; then
    got=$(approvals)
    if [ "$got" = "api -X POST repos/o/r/actions/runs/101/approve api -X POST repos/o/r/actions/runs/102/approve" ] &&
      ! grep -q '::warning::' "$work/case.out" && [ "$(listings)" = 4 ]; then
      ok "$j: polls until the held runs appear, approves each once, stops when none is held"
    else
      fail "$j: held runs: approvals '$got', $(listings) listings (want 101 and 102, 4 listings, no warning): $(cat "$work/case.out")"
    fi
    if grep -v -- '-X POST' "$work/case.gh" | grep -vqE '^gh api repos/o/r/actions/runs\?([^ ]*&)?head_sha=abc123(&| )' ||
      grep -v -- '-X POST' "$work/case.gh" | grep -vqE '[?&]event=pull_request(&| )' ||
      grep -vq '(token:set repo:o/r)$' "$work/case.gh"; then
      fail "$j: listings are not the head commit's pull_request runs with the job's token: $(cat "$work/case.gh")"
    else
      ok "$j: lists only the head commit's pull_request runs, with the job's token"
    fi
  fi

  # Runs already running (GitHub held nothing): nothing to approve, no
  # warning, no waiting out the poll.
  if approve_case "$j" "$branch" "runs not held" ok "$running"; then
    if [ -z "$(approvals)" ] && ! grep -q '::warning::' "$work/case.out" && [ "$(listings)" = 1 ]; then
      ok "$j: runs that are not held: approves nothing, no warning, stops"
    else
      fail "$j: runs not held: approvals '$(approvals)', $(listings) listings: $(cat "$work/case.out")"
    fi
  fi

  # No run ever appears: a warning with the maintainer's commands after a
  # bounded wait (about five minutes), and the job still passes.
  if approve_case "$j" "$branch" "no runs" ok "$none"; then
    got=$(slept)
    if [ -z "$(approvals)" ] && [ "$got" -ge 240 ] && [ "$got" -le 330 ] &&
      grep -q "::warning::.*gh run list --branch $branch" "$work/case.out" &&
      grep -qF '::warning::' "$work/case.out" && grep -qF "gh api -X POST repos/o/r/actions/runs/<id>/approve" "$work/case.out" &&
      grep -q "gh run list --branch $branch" "$work/case.summary"; then
      ok "$j: no runs after ${got}s: a warning and a summary line with the maintainer's commands, exit 0"
    else
      fail "$j: no runs: slept ${got}s (want 240-330), approvals '$(approvals)': $(cat "$work/case.out") / summary: $(cat "$work/case.summary")"
    fi
  fi

  # GITHUB_TOKEN may not approve: tried once, then a warning with the exact
  # command for that run, and the job still passes.
  if approve_case "$j" "$branch" "approve refused" 403 "$held1"; then
    if [ "$(approvals)" = "api -X POST repos/o/r/actions/runs/101/approve" ] &&
      grep -qF "gh api -X POST repos/o/r/actions/runs/101/approve" "$work/case.out" &&
      grep -q "::warning::.*gh run list --branch $branch" "$work/case.out" &&
      grep -qF 'actions/runs/101/approve' "$work/case.summary"; then
      ok "$j: approval refused (403): tried once, a warning and a summary line with the exact command, exit 0"
    else
      fail "$j: approve refused: approvals '$(approvals)': $(cat "$work/case.out") / summary: $(cat "$work/case.summary")"
    fi
  fi
}
check_approve api-lifecycle-pr bot/kb-refresh-api
check_approve registry-pr bot/kb-refresh-registry

# --- kb-refresh.yml: the code runs read-only, the writes run no code (#244) --

# The jobs that run repository or dependency code (go, make, setup-go) hold
# contents: read and nothing else; the PR jobs, which hold the writes, run
# none, and take only a patch of their PR's paths from the first job.
jobs=$(awk '/^jobs:/{j=1;next} j&&/^[^ #]/{j=0} j&&/^  [a-z0-9_-]+:[ ]*$/{sub(/^  /,"");sub(/:.*/,"");print}' "$kb")
runs_code() { grep -qE '^ +(run: .*\b(go|make) |uses: actions/setup-go@)|^ +(go|make|cd [^ ]+ && go) ' <<<"$1"; }
perms_of() { awk '/^    permissions:/{p=1;next} p&&/^    [^ ]/{exit} p' <<<"$1" | sed 's/ *#.*//; s/^ *//' | grep -v '^$' | tr '\n' ' ' | sed 's/ $//'; }
for j in $jobs; do
  body=$(job "$kb" "$j")
  if runs_code "$body"; then
    if [ "$(perms_of "$body")" = "contents: read" ]; then
      ok "$j runs repository or dependency code with contents: read only"
    else
      fail "$j runs repository or dependency code (go, make, setup-go) with permissions: $(perms_of "$body")"
    fi
    if grep -qE '^          cache: false( |$)' <<<"$body"; then ok "$j saves no Go cache a later job could restore"; else
      fail "$j runs unreviewed code with a Go cache (set setup-go's cache: false)"; fi
  elif grep -qE '^      [a-z-]+: write' <<<"$body" && [ "$j" != report-failure ]; then
    ok "$j holds write permissions and runs no repository or dependency code"
  fi
done
# Every checkout keeps no token (zizmor: artipacked).
n=$(grep -c 'uses: actions/checkout@' "$kb")
m=$(grep -A2 'uses: actions/checkout@' "$kb" | grep -cE '^ +persist-credentials: false( |$)')
[ "$n" -gt 0 ] && [ "$n" = "$m" ] && ok "all $n checkouts in $kb set persist-credentials: false" ||
  fail "$((n - m)) of $n checkouts in $kb keep their token (persist-credentials)"

for p in api-lifecycle:gen-kb registry:eol-sync; do
  j=${p%%:*} target=${p#*:}
  body=$(job "$kb" "$j")
  grep -qE "^        run: make $target$" <<<"$body" && ok "$j runs make $target" || fail "$j does not run make $target"
  grep -qE "^          name: kb-refresh-$j$" <<<"$body" && grep -q 'uses: actions/upload-artifact@' <<<"$body" &&
    ok "$j hands its patch over as the kb-refresh-$j artifact" || fail "$j does not upload a kb-refresh-$j artifact"
  pr=$(job "$kb" "$j-pr")
  grep -qE "^    needs: $j$" <<<"$pr" && ok "$j-pr needs $j" || fail "$j-pr does not need $j"
  uses=$(grep -oE 'uses: [a-z0-9_.-]+/[a-z0-9_.-]+@' <<<"$pr" | sort -u | tr '\n' ' ')
  [ "$uses" = "uses: actions/checkout@ uses: actions/download-artifact@ uses: peter-evans/create-pull-request@ " ] &&
    ok "$j-pr uses only checkout, download-artifact and create-pull-request" ||
    fail "$j-pr uses more than checkout, download-artifact and create-pull-request: $uses"
  # Every command its steps run (run: lines and run: | blocks), comments out.
  cmds=$(awk '
    /^        run: \|$/ { r = 1; next }
    /^        run: / { sub(/^        run: /, ""); print; next }
    r { if ($0 !~ /^ *$/ && $0 !~ /^          /) { r = 0; next } print }
  ' <<<"$pr" | sed 's/^ *//' | grep -v '^#' || true)
  [ -n "$cmds" ] || fail "$j-pr: no run commands found to check"
  if grep -E '(^|[^a-z_])(make|go|npm|node|python3?|bash|sh)( |$)|hack/|\./' <<<"$cmds" >"$work/pr-code"; then
    fail "$j-pr runs repository or dependency code: $(head -3 "$work/pr-code")"
  else
    ok "$j-pr runs no make, go, hack/ or ./ command"
  fi
done

# The apply step, against crafted patches in a scratch repository: only the
# PR's paths, no renames or copies, no symlinks; an empty patch is a no-op.
step 'Apply the regenerated files' < <(job "$kb" api-lifecycle-pr) | run_block >"$work/apply.sh"
[ -s "$work/apply.sh" ] || fail "api-lifecycle-pr has no 'Apply the regenerated files' run block"
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.invalid \
  GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.invalid
a=$work/apply-repo
git init -q -b main "$a"
mkdir -p "$a/tools/gen-kb" "$a/internal/kb/data" "$a/.github/workflows"
echo v1 >"$a/tools/gen-kb/go.mod" && echo '{}' >"$a/internal/kb/data/apilifecycle.json" && echo wf >"$a/.github/workflows/ci.yml"
git -C "$a" add -A && git -C "$a" commit -qm base
# mkpatch <name> <commands run in a copy>: the patch of what they change.
mkpatch() {
  rm -rf "$work/pp" && cp -R "$a" "$work/pp"
  (cd "$work/pp" && eval "$2" && git add -A && git diff --cached --binary) >"$work/$1.patch"
}
mkpatch good 'echo v2 >tools/gen-kb/go.mod; echo "{\"a\":1}" >internal/kb/data/apilifecycle.json; printf "\x00\x01" >internal/kb/data/new.bin'
mkpatch outside 'echo v2 >tools/gen-kb/go.mod; echo evil >.github/workflows/ci.yml'
mkpatch rename 'git mv -k .github/workflows/ci.yml tools/gen-kb/ci.yml'
mkpatch link 'ln -s ../../.git/config tools/gen-kb/cfg'
mkpatch quoted 'echo x >"tools/gen-kb/a\"b"'
: >"$work/empty.patch"
# apply_case <label> <patch|missing> <want: applied|refused|noop> [changed-file]
apply_case() {
  local label=$1 patch=$2 want=$3 file=${4:-} rc=0
  git -C "$a" reset -q --hard && git -C "$a" clean -qfd
  local path="$work/$patch.patch"
  [ "$patch" != missing ] || path=$work/no-such.patch
  (cd "$a" && PATCH=$path RUNNER_TEMP=$work bash --noprofile --norc -e "$work/apply.sh") >"$work/apply.out" 2>&1 || rc=$?
  local changed
  changed=$(git -C "$a" status --porcelain | sort | tr '\n' ' ')
  case $want in
    applied) [ "$rc" = 0 ] && grep -q "$file" <<<"$changed" && ! grep -q '.github' <<<"$changed" ;;
    refused) [ "$rc" != 0 ] && [ -z "$changed" ] && grep -q '::error::' "$work/apply.out" ;;
    noop) [ "$rc" = 0 ] && [ -z "$changed" ] ;;
  esac && ok "apply: $label" || fail "apply: $label: exit $rc, changed '$changed': $(cat "$work/apply.out")"
}
apply_case "a patch of the PR's paths (binary included) is applied" good applied internal/kb/data/new.bin
apply_case "a patch that also changes .github/workflows is refused, nothing applied" outside refused
apply_case "a rename is refused" rename refused
apply_case "a symlink is refused" link refused
apply_case "a path git has to quote is refused" quoted refused
apply_case "an empty patch (nothing regenerated) changes nothing" empty noop
apply_case "a missing patch fails" missing refused

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
want_expr "registry runs on workflow_dispatch (e.g. by hand on a kb-refresh bot branch)" "$regif" true "$dispatch"
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
