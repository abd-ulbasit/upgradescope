#!/usr/bin/env bash
# Tests that kb-refresh's unreviewed build runs outside main's cache scope
# and its artifact namespace (make hack-test, #273, IR-22).
#
# kb-refresh used to build freshly bumped, unreviewed modules (go get of the
# newest k8s.io modules, make gen-kb, go test, make eol-sync) in a job of a
# workflow scheduled from main. Any job can write the GitHub Actions cache
# with the runner's token, whatever its permissions: block, and writes go to
# the scope of the run's ref: main's CI and a tag's release run restore what
# main's scope holds. The two pipelines also shared one run's artifact
# namespace, so unreviewed code could upload under the other pipeline's name.
# So the structure is now:
#  - kb-refresh.yml (main): its `build` job points bot/kb-refresh-build at
#    main's head, dispatches kb-refresh-build.yml on it, waits, and outputs
#    the run id and each pipeline's result; it runs gh only (no checkout, go,
#    make or setup-go); nothing else in the file runs unreviewed code, and
#    the file uploads no artifact;
#  - kb-refresh-build.yml: triggered by workflow_dispatch only, every job
#    guarded to run only on refs/heads/bot/kb-refresh-build (never main),
#    contents: read only, no cache restored or saved (setup-go cache: false,
#    no actions/cache), one artifact per pipeline under its own name;
#  - the *-pr jobs take their pipeline's patch from that run by run id
#    (download-artifact run-id, github-token), under that pipeline's name,
#    and only when that pipeline's build succeeded.
# The structure rules run against the real workflows and against mutants that
# must each fail the rule they break (the old structure, in which the
# unreviewed jobs sit in the file scheduled from main, is one of them); the
# `build` job's script runs against a stub gh. Offline. Needs node and jq.
#
# KB_ENTRY / KB_BUILD override the two workflow paths (to point the audit at
# another version of them).
set -euo pipefail
cd "$(dirname "$0")/.."

command -v node >/dev/null || { echo "kb-refresh-scope_test: node is required" >&2; exit 1; }
command -v jq >/dev/null || { echo "kb-refresh-scope_test: jq is required (the stub gh applies --jq with it)" >&2; exit 1; }

entry=${KB_ENTRY:-.github/workflows/kb-refresh.yml}
build=${KB_BUILD:-.github/workflows/kb-refresh-build.yml}
branch=bot/kb-refresh-build
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
: >"$work/results"
ok() { echo "ok   $1" | tee -a "$work/results"; }
fail() { echo "FAIL $1" >&2; echo "FAIL $1" >>"$work/results"; }

# job <file> <id>: the job's lines, from `  <id>:` to the next job.
job() { awk -v want="  $2:" '$0 == want { c = 1; print; next } c && /^  [^ ]/ { exit } c' "$1"; }
job_ids() { awk '/^jobs:/{j=1;next} j&&/^[^ #]/{j=0} j&&/^  [a-z0-9_-]+:[ ]*$/{sub(/^  /,"");sub(/:.*/,"");print}' "$1"; }
step() { awk -v want="      - name: $1" '$0 == want { c = 1; print; next } c && (/^      - / || /^    [^ ]/) { exit } c'; }
# field <indent> <key>: the first <key> at exactly <indent> spaces; a folded
# (>-) value is joined onto one line.
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
run_block() { awk '/^        run: \|$/ { r = 1; next } r { if ($0 !~ /^ *$/ && $0 !~ /^          /) exit; print substr($0, 11) }'; }
# perms_of <job text>: the job's permissions: block, one line.
perms_of() { awk '/^    permissions:/{p=1;next} p&&/^    [^ ]/{exit} p' <<<"$1" | sed 's/ *#.*//; s/^ *//' | grep -v '^$' | tr '\n' ' ' | sed 's/ $//'; }
# triggers <file>: the keys under on:
triggers() { awk '/^on:/{o=1;next} o&&/^[^ #]/{exit} o&&/^  [a-z_]+:/{sub(/^  /,"");sub(/:.*/,"");print}' "$1" | sort | tr '\n' ' ' | sed 's/ $//'; }
# runs_code <text>: does it run repository or dependency code?
runs_code() { grep -qE '^ +(run: .*\b(go|make) |uses: actions/setup-go@)|^ +(go|make|cd [^ ]+ && go) ' <<<"$1"; }

# expr <expression> <context-json>: the expression's value (the same
# evaluation as kb-refresh-ci_test.sh).
expr() {
  [ -n "$1" ] || return 1
  node -e '
    const [src, ctxJSON] = process.argv.slice(1);
    const ctx = Object.assign({ github: {}, inputs: {}, needs: {}, steps: {} }, JSON.parse(ctxJSON));
    const e = src.trim().replace(/^\$\{\{([\s\S]*)\}\}$/, "$1")
      .replace(/\.([A-Za-z_]\w*(?:-[\w]+)+)/g, (_, k) => "[" + JSON.stringify(k) + "]");
    const v = new Function("github", "inputs", "needs", "steps", "cancelled", "always", "failure", "startsWith",
      "return (" + e + ");")(ctx.github, ctx.inputs, ctx.needs, ctx.steps, () => false, () => true,
      () => Object.values(ctx.needs).some((n) => n.result === "failure"),
      (s, p) => String(s).toLowerCase().startsWith(String(p).toLowerCase()));
    process.stdout.write(v === null || v === undefined ? "" : String(v));
  ' "$1" "$2"
}
# expr_bad <expression> <want> <context-json>: prints a line when wrong.
expr_bad() {
  local got
  local key
  key=$(printf '%s\n%s' "$1" "$3" | cksum | tr -d ' ')
  # Memoised: the mutants repeat the real file's expressions.
  if [ -f "$work/memo.$key" ]; then got=$(cat "$work/memo.$key"); else
    got=$(expr "$1" "$3" 2>/dev/null) || { echo "cannot evaluate '$1'"; return; }
    printf '%s' "$got" >"$work/memo.$key"
  fi
  [ "$got" = "$2" ] || echo "'$1' is '$got' (want '$2') for $3"
}

# --- the structure rules ------------------------------------------------------

# audit <entry> <build>: one line per violated rule, "<tag>: what"; nothing
# when the structure holds.
audit() {
  local e=$1 b=$2 j body ids n m expected pr up name names dup cond guard s
  [ -f "$e" ] && [ -f "$b" ] || { echo "files: $e or $b is missing"; return; }

  # The build workflow is only ever dispatched, and only on the staging branch.
  [ "$(triggers "$b")" = workflow_dispatch ] ||
    echo "trigger: $b is triggered by '$(triggers "$b")', not workflow_dispatch alone"
  [ "$(triggers "$e")" = "schedule workflow_dispatch" ] ||
    echo "entry-trigger: $e is triggered by '$(triggers "$e")', not schedule and workflow_dispatch"
  ids=$(job_ids "$b")
  [ "$ids" = "$(printf 'api-lifecycle\nregistry')" ] || echo "jobs: $b has jobs '$(tr '\n' ' ' <<<"$ids")', want api-lifecycle and registry"
  for j in $ids; do
    body=$(job "$b" "$j")
    guard=$(field 4 if <<<"$body")
    if [ -z "$guard" ]; then
      echo "ref-guard: $b: job $j has no if: restricting it to $branch"
    else
      for s in "refs/heads/main" "refs/heads/feature" "refs/tags/v0.2.0" "refs/pull/1/merge" ""; do
        expr_bad "$guard" false "{\"github\":{\"ref\":\"$s\"}}" | sed "s|^|ref-guard: $b: job $j, ref '$s': |"
      done
      expr_bad "$guard" true "{\"github\":{\"ref\":\"refs/heads/$branch\"}}" | sed "s|^|ref-guard: $b: job $j: |"
    fi
    [ "$(perms_of "$body")" = "contents: read" ] || echo "perms: $b: job $j has permissions '$(perms_of "$body")', want contents: read"
    runs_code "$body" || echo "build-runs-code: $b: job $j runs no go or make (is the unreviewed build still here?)"
    grep -qE '^          cache: false( |$)' <<<"$body" || echo "cache: $b: job $j runs unreviewed code without setup-go cache: false"
  done
  awk '/^permissions:/{p=1;next} p&&/^[^ ]/{exit} p' "$b" | sed 's/ *#.*//; s/^ *//' | grep -v '^$' | tr '\n' ' ' | grep -qx 'contents: read ' ||
    echo "perms: $b: the workflow-level permissions are not contents: read"

  # No job that runs unreviewed code restores or saves a cache, in either file.
  for f in "$e" "$b"; do
    grep -nE 'uses: actions/cache(/restore|/save)?@|type=gha|cache-from|cache-to|uses: actions/setup-node@' "$f" |
      sed "s|^|cache: $f: |" || true
    grep -nE '^ +cache(-dependency-path)?:' "$f" | grep -vE 'cache: false( |$)' | sed "s|^|cache: $f: a cache setting that is not false: |" || true
  done

  # The file scheduled from main builds nothing and uploads nothing.
  for j in $(job_ids "$e"); do
    body=$(job "$e" "$j")
    if runs_code "$body"; then echo "entry-runs-code: $e: job $j runs go, make or setup-go on the ref the schedule runs on"; fi
    if grep -q 'uses: actions/upload-artifact@' <<<"$body"; then echo "entry-artifact: $e: job $j uploads an artifact"; fi
  done
  body=$(job "$e" build)
  if [ -z "$body" ]; then
    echo "build-job: $e has no build job"
  else
    [ "$(perms_of "$body")" = "contents: write actions: write" ] ||
      echo "build-job: $e: build has permissions '$(perms_of "$body")', want contents: write and actions: write only"
    grep -qE 'uses: actions/(checkout|setup-[a-z]+)@' <<<"$body" && echo "build-job: $e: build checks out or sets up code; it must run gh only"
    grep -qE "gh workflow run $(basename "$b") --ref \"\\\$BUILD_BRANCH\"" <<<"$body" ||
      echo "build-job: $e: build does not dispatch $(basename "$b") on \$BUILD_BRANCH"
    [ "$(field 6 BUILD_BRANCH <<<"$body")" = "$branch" ] || echo "build-job: $e: BUILD_BRANCH is not $branch (the build workflow's guard)"
  fi

  # Artifacts: one per pipeline, distinct names, in the build run; the PR
  # jobs read them by run id and by that name, and only after that
  # pipeline's build succeeded.
  names=''
  for pr in api-lifecycle registry; do
    expected=kb-refresh-$pr
    body=$(job "$b" "$pr")
    n=$(grep -c 'uses: actions/upload-artifact@' <<<"$body" || true)
    up=$(grep -A3 'uses: actions/upload-artifact@' <<<"$body" | sed -n 's/^ *name: //p')
    if [ "$n" = 1 ] && [ "$up" = "$expected" ]; then names="$names $up"; else
      echo "artifact-names: $b: job $pr uploads $n artifact(s) named '$up', want exactly one named $expected"; fi
    body=$(job "$e" "$pr-pr")
    if [ -z "$body" ]; then echo "pr-job: $e has no $pr-pr job"; continue; fi
    grep -qE '^    needs: build$' <<<"$body" || echo "pr-job: $e: $pr-pr does not need build alone"
    cond=$(field 4 if <<<"$body")
    if [ -z "$cond" ]; then echo "pr-job: $e: $pr-pr runs even when the build did not succeed"; else
      expr_bad "$cond" true "{\"needs\":{\"build\":{\"result\":\"success\",\"outputs\":{\"$pr-result\":\"success\"}}}}" | sed "s|^|pr-job: $e: $pr-pr: |"
      expr_bad "$cond" false "{\"needs\":{\"build\":{\"result\":\"success\",\"outputs\":{\"$pr-result\":\"failure\"}}}}" | sed "s|^|pr-job: $e: $pr-pr: |"
      expr_bad "$cond" false "{\"needs\":{\"build\":{\"result\":\"success\",\"outputs\":{\"$pr-result\":\"missing\"}}}}" | sed "s|^|pr-job: $e: $pr-pr: |"
      expr_bad "$cond" false "{\"needs\":{\"build\":{\"result\":\"failure\",\"outputs\":{\"$pr-result\":\"success\"}}}}" | sed "s|^|pr-job: $e: $pr-pr: |"
    fi
    # The download: this pipeline's name, from the build run, by id.
    dl=$(grep -A8 'uses: actions/download-artifact@' <<<"$body" | awk 'NR > 1 && /^      - /{exit} {print}')
    [ "$(grep -c 'uses: actions/download-artifact@' <<<"$body" || true)" = 1 ] || echo "download: $e: $pr-pr does not download exactly one artifact"
    grep -qE "^          name: $expected$" <<<"$dl" || echo "download: $e: $pr-pr does not download $expected"
    grep -qF 'run-id: ${{ needs.build.outputs.run-id }}' <<<"$dl" ||
      echo "download: $e: $pr-pr downloads without run-id: \${{ needs.build.outputs.run-id }}, so it reads this run's artifacts by name"
    grep -qF 'github-token: ${{ github.token }}' <<<"$dl" || echo "download: $e: $pr-pr's download names no github-token (a run-id download needs one)"
  done
  dup=$(tr ' ' '\n' <<<"$names" | grep -v '^$' | sort | uniq -d || true)
  [ -z "$dup" ] || echo "artifact-names: the pipelines share the artifact name $dup"

  # Pinned actions, no token kept in a checkout, in both files.
  for f in "$e" "$b"; do
    grep -E '^ +(- )?uses: ' "$f" | grep -vE '@[0-9a-f]{40}( |$)' | sed "s|^|pin: $f: not pinned by full SHA: |" || true
    n=$(grep -c 'uses: actions/checkout@' "$f" || true)
    m=$(grep -A2 'uses: actions/checkout@' "$f" | grep -cE '^ +persist-credentials: false( |$)' || true)
    [ "$n" = "$m" ] || echo "checkout: $f: $((n - m)) of $n checkouts keep their token"
  done

  # A failed build is not silent: report-failure runs on it.
  body=$(job "$e" report-failure)
  cond=$(field 4 if <<<"$body")
  if [ -z "$cond" ]; then echo "report: $e: report-failure has no if:"; else
    grep -qE '^    needs: \[build, api-lifecycle-pr, registry-pr\]$' <<<"$body" || echo "report: $e: report-failure does not need build and both PR jobs"
    # build ok, PR jobs ok: quiet. Anything else, and not cancelled: reports.
    expr_bad "$cond" false '{"needs":{"build":{"result":"success","outputs":{"api-lifecycle-result":"success","registry-result":"success"}},"api-lifecycle-pr":{"result":"success"},"registry-pr":{"result":"success"}}}' | sed "s|^|report: $e: |"
    expr_bad "$cond" true '{"needs":{"build":{"result":"failure","outputs":{}},"api-lifecycle-pr":{"result":"skipped"},"registry-pr":{"result":"skipped"}}}' | sed "s|^|report: $e: |"
    expr_bad "$cond" true '{"needs":{"build":{"result":"success","outputs":{"api-lifecycle-result":"failure","registry-result":"success"}},"api-lifecycle-pr":{"result":"skipped"},"registry-pr":{"result":"success"}}}' | sed "s|^|report: $e: |"
    expr_bad "$cond" true '{"needs":{"build":{"result":"success","outputs":{"api-lifecycle-result":"success","registry-result":"missing"}},"api-lifecycle-pr":{"result":"success"},"registry-pr":{"result":"skipped"}}}' | sed "s|^|report: $e: |"
    expr_bad "$cond" true '{"needs":{"build":{"result":"success","outputs":{"api-lifecycle-result":"success","registry-result":"success"}},"api-lifecycle-pr":{"result":"success"},"registry-pr":{"result":"failure"}}}' | sed "s|^|report: $e: |"
  fi
}

real=$(audit "$entry" "$build") || true
if [ -z "$real" ]; then
  ok "$entry and $build: unreviewed jobs only in a run on $branch, no cache, artifacts by run id under distinct names"
else
  fail "the structure rules fail on the real workflows:"
  sed 's/^/     /' <<<"$real" >&2
fi

# --- mutants: each must fail the rule it breaks ------------------------------

# mutant <name> <tag> <entry-perl|-> <build-perl|->: copies of the two files
# with the perl substitutions applied; the audit must report <tag>.
mutant() {
  local name=$1 tag=$2 ep=$3 bp=$4 d got
  d=$work/mutant-$(tr -c 'a-z0-9\n' - <<<"$name")
  mkdir -p "$d"
  cp "$entry" "$d/kb-refresh.yml" && cp "$build" "$d/kb-refresh-build.yml"
  [ "$ep" = - ] || perl -0pi -e "$ep" "$d/kb-refresh.yml"
  [ "$bp" = - ] || perl -0pi -e "$bp" "$d/kb-refresh-build.yml"
  if cmp -s "$entry" "$d/kb-refresh.yml" && cmp -s "$build" "$d/kb-refresh-build.yml"; then
    fail "mutant '$name' changed nothing; fix the test"
    return
  fi
  got=$(audit "$d/kb-refresh.yml" "$d/kb-refresh-build.yml") || true
  if grep -q "^$tag:" <<<"$got"; then ok "mutant fails $tag: $name"; else
    fail "mutant '$name' is not caught by '$tag'; the audit says: ${got:-nothing}"; fi
}

# The old structure: the unreviewed jobs in the file the schedule runs.
old_entry='
  my $b = do { local $/; open(my $f, "<", "'"$build"'") or die; <$f> };
  $b =~ s/\A.*?^jobs:\n//ms;
  s/^(jobs:\n)/$1$b\n/m;
'
mutant "the unreviewed jobs back in the file scheduled from main" entry-runs-code "$old_entry" -
mutant "the build workflow also runs on a schedule" trigger - 's/^on:\n/on:\n  schedule:\n    - cron: "0 1 * * 1"\n/m'
mutant "the build workflow also runs on push" trigger - 's/^on:\n/on:\n  push:\n    branches: [main]\n/m'
mutant "the build workflow also runs on pull_request" trigger - 's/^on:\n/on:\n  pull_request:\n/m'
mutant "a build job without the ref guard" ref-guard - 's/^    if: github.ref == .refs\/heads\/bot\/kb-refresh-build.\n//m'
mutant "a build job guarded to main" ref-guard - 's/refs\/heads\/bot\/kb-refresh-build\x27$/refs\/heads\/main\x27/m'
mutant "a build job guarded on a prefix match" ref-guard - 's/github.ref == \x27refs\/heads\/bot\/kb-refresh-build\x27/startsWith(github.ref, \x27refs\/heads\/\x27)/m'
mutant "setup-go with a cache in an unreviewed job" cache - 's/cache: false\n/cache: true\n/'
mutant "an actions/cache step in the build workflow" cache - 's/(      - name: Validate the regenerated KB\n)/      - uses: actions\/cache\@0000000000000000000000000000000000000000 # v4\n        with:\n          path: ~\/go\/pkg\/mod\n          key: k\n$1/'
mutant "an unreviewed job with write permissions" perms - 's/^(  registry:\n(?:.*\n)*?    permissions:\n      contents: )read/${1}write/m'
mutant "an unreviewed job with an extra permission" perms - 's/^(  api-lifecycle:\n(?:.*\n)*?      contents: read\n)/$1      actions: write\n/m'
mutant "the registry pipeline uploads under the api pipeline's name" artifact-names - 's/name: kb-refresh-registry/name: kb-refresh-api-lifecycle/'
mutant "a shared artifact name in both pipelines and both downloads" artifact-names \
  's/kb-refresh-registry/kb-refresh-api-lifecycle/g' 's/kb-refresh-registry/kb-refresh-api-lifecycle/g'
mutant "a PR job that downloads by name from its own run" download 's/^          run-id: .*\n//m' -
mutant "a PR job downloading the other pipeline's artifact" download 's/(  registry-pr:\n(?:.*\n)*?          name: )kb-refresh-registry/${1}kb-refresh-api-lifecycle/' -
mutant "a PR job that does not wait for the build result" pr-job 's/^    if: needs.build.result == .success. && needs.build.outputs.registry-result == .success.\n//m' -
mutant "a PR job that runs whatever the other pipeline did" pr-job 's/needs.build.outputs.api-lifecycle-result == .success./needs.build.outputs.registry-result == \x27success\x27/' -
mutant "the build job dispatches on main" build-job 's/--ref "\$BUILD_BRANCH"/--ref main/' -
mutant "the build job checks out the repository" build-job 's/(    steps:\n)(      - name: Run the unreviewed build)/$1      - uses: actions\/checkout\@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1\n        with:\n          persist-credentials: false\n$2/' -
mutant "the build job holds more than it needs" build-job 's/(      actions: write # dispatch[^\n]*\n)/$1      pull-requests: write\n/' -
mutant "an action not pinned by SHA" pin 's/actions\/download-artifact\@[0-9a-f]{40}/actions\/download-artifact\@v8/' -
mutant "a checkout that keeps its token" checkout - 's/(uses: actions\/checkout\@[0-9a-f]{40} # v7.0.1\n        with:\n          persist-credentials: )false/${1}true/'
mutant "a failed build that report-failure ignores" report 's/^    if: >-\n.*?\n    runs-on/    if: failure()\n    runs-on/ms' -

# --- the build job's script, against a stub gh -------------------------------

body=$(job "$entry" build)
step 'Run the unreviewed build on the staging branch' <<<"$body" | run_block >"$work/build.sh"
step 'Remove the staging branch' <<<"$body" | run_block >"$work/cleanup.sh"
[ -s "$work/build.sh" ] && [ -s "$work/cleanup.sh" ] || fail "the build job has no 'Run the unreviewed build' or 'Remove the staging branch' run block"

mkdir -p "$work/bin"
# gh: logs each call. Answers from files in $STUB: ref-exists (present: the
# staging branch exists), list.json (gh run list's JSON), status.N (the n-th
# run status reading, the last once they run out), jobs.json (the run's
# jobs). `gh api` / `gh run list` apply --jq with jq, as gh does.
cat >"$work/bin/gh" <<'STUB'
#!/usr/bin/env bash
echo "gh $*" >>"$GH_LOG"
jqf() { local f=; while [ $# -gt 0 ]; do [ "$1" = --jq ] && f=$2; shift; done; jq -r "${f:-.}"; }
case "$1 $2" in
  "api repos/o/r/git/ref/heads/bot/kb-refresh-build")
    [ -e "$STUB/ref-exists" ] || { echo 'gh: Not Found (HTTP 404)' >&2; exit 1; }
    echo '{}' ;;
  "api -X")
    case "$3 $4" in
      "PATCH repos/o/r/git/refs/heads/bot/kb-refresh-build" | "POST repos/o/r/git/refs" | "DELETE repos/o/r/git/refs/heads/bot/kb-refresh-build") echo '{}' ;;
      *) echo "stub gh: unexpected call: $*" >&2; exit 2 ;;
    esac ;;
  "workflow run") : ;;
  "run list") jqf "$@" <"$STUB/list.json" ;;
  "run cancel") : ;;
  "api repos/o/r/actions/runs/777/jobs?per_page=100") jqf "$@" <"$STUB/jobs.json" ;;
  "api repos/o/r/actions/runs/777")
    n=$(($(cat "$STUB/status.n" 2>/dev/null || echo 0) + 1)); echo "$n" >"$STUB/status.n"
    last=$(ls "$STUB" | grep -c '^status\.[0-9]' || true)
    echo "{\"status\":\"$(cat "$STUB/status.$((n < last ? n : last))")\"}" | jqf "$@" ;;
  *) echo "stub gh: unexpected call: $*" >&2; exit 2 ;;
esac
STUB
cat >"$work/bin/sleep" <<'STUB'
#!/usr/bin/env bash
echo "$*" >>"$SLEEP_LOG"
STUB
chmod +x "$work/bin/gh" "$work/bin/sleep"

sha=1111111111111111111111111111111111111111
# build_case <label> <exists|absent> <list.json> <jobs.json> <status>...: runs
# the script; leaves rc, output, outputs and the gh log in $work/bc.*.
build_case() {
  local exists=$1 list=$2 jobs=$3
  shift 3
  export STUB=$work/stub
  rm -rf "$STUB" "$work"/bc.*
  mkdir -p "$STUB"
  [ "$exists" = exists ] && touch "$STUB/ref-exists"
  echo "$list" >"$STUB/list.json"; echo "$jobs" >"$STUB/jobs.json"
  local i=0 s
  for s in "$@"; do i=$((i + 1)); echo "$s" >"$STUB/status.$i"; done
  : >"$work/bc.gh"; : >"$work/bc.sleep"; : >"$work/bc.out"
  BC_RC=0
  PATH="$work/bin:$PATH" GH_LOG="$work/bc.gh" SLEEP_LOG="$work/bc.sleep" GITHUB_OUTPUT="$work/bc.out" \
    GITHUB_SERVER_URL=https://github.com GH_REPO=o/r SHA=$sha REQUEST=55-1 BUILD_BRANCH=$branch GH_TOKEN=stub \
    bash --noprofile --norc -e "$work/build.sh" >"$work/bc.log" 2>&1 || BC_RC=$?
}
title='kb-refresh-build 55-1'
list_ok="[{\"databaseId\":776,\"displayTitle\":\"$title\",\"headSha\":\"2222222222222222222222222222222222222222\"},{\"databaseId\":775,\"displayTitle\":\"kb-refresh-build 54-1\",\"headSha\":\"$sha\"},{\"databaseId\":777,\"displayTitle\":\"$title\",\"headSha\":\"$sha\"}]"
jobs_ok='{"jobs":[{"name":"api-lifecycle","conclusion":"success"},{"name":"registry","conclusion":"success"}]}'
jobs_mixed='{"jobs":[{"name":"api-lifecycle","conclusion":"failure"},{"name":"registry","conclusion":"success"}]}'
jobs_skipped='{"jobs":[{"name":"api-lifecycle","conclusion":"skipped"}]}'
out() { sort "$work/bc.out" | tr '\n' ' ' | sed 's/ $//'; }

build_case absent "$list_ok" "$jobs_ok" queued in_progress completed
if [ "$BC_RC" = 0 ] && grep -q "api -X POST repos/o/r/git/refs -f ref=refs/heads/$branch -f sha=$sha" "$work/bc.gh" &&
  ! grep -q -- '-X PATCH' "$work/bc.gh" &&
  grep -qxF "gh workflow run kb-refresh-build.yml --ref $branch -f request=55-1" "$work/bc.gh" &&
  [ "$(out)" = "api-lifecycle-result=success registry-result=success run-id=777" ]; then
  ok "build: creates the staging branch at the commit, dispatches on it, finds its run by name and commit (not the lookalikes), outputs success"
else
  fail "build: new branch, both succeed: rc $BC_RC, outputs '$(out)': $(cat "$work/bc.gh") $(tail -5 "$work/bc.log")"
fi

build_case exists "$list_ok" "$jobs_mixed" completed
if [ "$BC_RC" = 0 ] && grep -q "api -X PATCH repos/o/r/git/refs/heads/$branch -f sha=$sha -F force=true" "$work/bc.gh" &&
  ! grep -q -- '-X POST' "$work/bc.gh" &&
  [ "$(out)" = "api-lifecycle-result=failure registry-result=success run-id=777" ]; then
  ok "build: moves an existing staging branch; one pipeline failing is an output, not a failed job"
else
  fail "build: existing branch, api failed: rc $BC_RC, outputs '$(out)': $(cat "$work/bc.gh") $(tail -5 "$work/bc.log")"
fi

build_case exists "$list_ok" "$jobs_skipped" completed
if [ "$BC_RC" = 0 ] && [ "$(out)" = "api-lifecycle-result=skipped registry-result=missing run-id=777" ]; then
  ok "build: a pipeline whose job never ran is 'missing', never 'success'"
else
  fail "build: skipped jobs: rc $BC_RC, outputs '$(out)'"
fi

build_case exists '[]' "$jobs_ok" completed
if [ "$BC_RC" != 0 ] && ! grep -q '^run-id=' "$work/bc.out" && grep -q '::error::' "$work/bc.log"; then
  ok "build: fails, with no run id, when the dispatched run never appears"
else
  fail "build: no run: rc $BC_RC, outputs '$(out)': $(tail -3 "$work/bc.log")"
fi

# The run is still going after the wait: the job fails (the cleanup step
# then cancels it).
build_case exists "$list_ok" "$jobs_ok" in_progress
if [ "$BC_RC" != 0 ] && grep -q '^run-id=777' "$work/bc.out" && ! grep -q 'result=' "$work/bc.out" && grep -q '::error::' "$work/bc.log"; then
  ok "build: fails when the run never completes, after a bounded wait"
else
  fail "build: run never completes: rc $BC_RC, outputs '$(out)': $(tail -3 "$work/bc.log")"
fi

# cleanup_case <status> <ID>: the cleanup step; cancels an unfinished run,
# always deletes the branch, fails nothing.
cleanup_case() {
  export STUB=$work/stub
  rm -rf "$STUB" "$work"/cc.*; mkdir -p "$STUB"; echo "$1" >"$STUB/status.1"
  : >"$work/cc.gh"
  CC_RC=0
  PATH="$work/bin:$PATH" GH_LOG="$work/cc.gh" SLEEP_LOG=/dev/null GH_REPO=o/r BUILD_BRANCH=$branch ID=$2 GH_TOKEN=stub \
    bash --noprofile --norc -e "$work/cleanup.sh" >"$work/cc.log" 2>&1 || CC_RC=$?
}
cleanup_case in_progress 777
if [ "$CC_RC" = 0 ] && grep -q '^gh run cancel 777' "$work/cc.gh" && grep -q "api -X DELETE repos/o/r/git/refs/heads/$branch" "$work/cc.gh"; then
  ok "cleanup: cancels a run still going and deletes the staging branch"
else fail "cleanup: unfinished run: rc $CC_RC: $(cat "$work/cc.gh")"; fi
cleanup_case completed 777
if [ "$CC_RC" = 0 ] && ! grep -q 'run cancel' "$work/cc.gh" && grep -q "api -X DELETE" "$work/cc.gh"; then
  ok "cleanup: leaves a finished run alone and deletes the staging branch"
else fail "cleanup: finished run: rc $CC_RC: $(cat "$work/cc.gh")"; fi
cleanup_case completed ''
if [ "$CC_RC" = 0 ] && ! grep -q 'run cancel' "$work/cc.gh" && grep -q "api -X DELETE" "$work/cc.gh"; then
  ok "cleanup: with no run id (the dispatch failed) it still deletes the staging branch"
else fail "cleanup: no run id: rc $CC_RC: $(cat "$work/cc.gh")"; fi

pass=$(grep -c '^ok' "$work/results" || true)
failed=$(grep -c '^FAIL' "$work/results" || true)
echo "kb-refresh-scope_test: $pass passed, $failed failed"
[ "$failed" -eq 0 ] && [ "$pass" -gt 0 ]
