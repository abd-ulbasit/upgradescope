#!/usr/bin/env bash
# Tests that kb-refresh's unreviewed build runs outside main's cache scope
# and outside the other pipeline's artifact namespace, and that the result
# is verified before it is used (make hack-test, #273, IR-22, IR-23).
#
# kb-refresh used to build freshly bumped, unreviewed modules (go get of the
# newest k8s.io modules, make gen-kb, go test, make eol-sync) in a job of a
# workflow scheduled from main. Any job can write the GitHub Actions cache
# with the runner's token, whatever its permissions: block, and writes go to
# the scope of the run's ref: main's CI and a tag's release run restore what
# main's scope holds. The same token reaches the artifact store of the job's
# own run, so two pipelines in one run could upload, delete or replace each
# other's artifact. So the structure is now:
#  - kb-refresh.yml (main): `stage` points bot/kb-refresh-build at main's
#    head; per pipeline, `<p>-run` dispatches kb-refresh-build.yml on it FOR
#    THAT PIPELINE (a run of its own) and waits, `<p>-verify` checks that run
#    through the API before downloading from it (workflow file, branch,
#    commit, event, status and conclusion, repository and head repository,
#    run name, jobs, artifacts), reads the patch's sha256 from the run's
#    `seal` job log, downloads the artifact by run id with a read-only token,
#    compares the sha256 and uploads the checked patch again in its own run,
#    and `<p>-pr` (which holds the writes) takes that one. Nothing in the
#    file runs go, make or repository code;
#  - kb-refresh-build.yml: triggered by workflow_dispatch only, every build
#    job guarded to run only on refs/heads/bot/kb-refresh-build (never main)
#    and only for its own `pipeline` input, contents: read only, no cache
#    restored or saved, one artifact per run; the `seal` job (no repository
#    code) prints the artifact's digest in a log the build job cannot write.
# The structure rules run against the real workflows and against mutants that
# must each fail the rule they break (the old structure, in which the
# unreviewed jobs sit in the file scheduled from main, is one of them); the
# scripts in the workflows run against stub gh and stub artifacts, with the
# refusals pinned by mutating the stubbed run field by field. Offline. Needs
# node and jq.
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
# strip_comments <text>: the YAML lines without full-line comments.
strip_comments() { grep -vE '^ *#' <<<"$1" || true; }
# line_of <text> <fixed string>: the first line number holding it, or 0.
line_of() { { grep -nF -- "$2" <<<"$1" || true; } | head -1 | cut -d: -f1 | grep . || echo 0; }

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
    printf '%s' "$got" >"$work/memo.$key.$$" && mv "$work/memo.$key.$$" "$work/memo.$key"
  fi
  [ "$got" = "$2" ] || echo "'$1' is '$got' (want '$2') for $3"
}
# tag <prefix>: prefixes every line on stdin.
tag() { sed "s|^|$1|"; }

other_of() { if [ "$1" = api-lifecycle ]; then echo registry; else echo api-lifecycle; fi; }

# --- the structure rules ------------------------------------------------------

# audit <entry> <build>: one line per violated rule, "<tag>: what"; nothing
# when the structure holds.
audit() {
  local e=$1 b=$2 j p body ids n m cond guard s dl up names dup co vn bn
  local ln_check ln_dl ln_cmp ln_up chk need other
  [ -f "$e" ] && [ -f "$b" ] || { echo "files: $e or $b is missing"; return; }

  # ---- the build workflow: only dispatched, only on the staging branch,
  # one pipeline per run.
  [ "$(triggers "$b")" = workflow_dispatch ] ||
    echo "trigger: $b is triggered by '$(triggers "$b")', not workflow_dispatch alone"
  [ "$(triggers "$e")" = "schedule workflow_dispatch" ] ||
    echo "entry-trigger: $e is triggered by '$(triggers "$e")', not schedule and workflow_dispatch"
  ids=$(job_ids "$b")
  [ "$ids" = "$(printf 'api-lifecycle\nregistry\nseal')" ] || echo "jobs: $b has jobs '$(tr '\n' ' ' <<<"$ids")', want api-lifecycle, registry and seal"
  grep -qxF 'run-name: kb-refresh-build ${{ inputs.pipeline }} ${{ inputs.request }}' "$b" ||
    echo "run-name: $b's run-name does not carry the pipeline and the request (the dispatcher finds its run by it)"
  [ "$(awk '/^      pipeline:/{p=1;next} p&&/^      [a-z]/{exit} p&&/^          - /{sub(/^ *- /,"");printf "%s ",$0}' "$b")" = 'api-lifecycle registry ' ] ||
    echo "input: $b's pipeline input does not offer exactly api-lifecycle and registry"
  names=''
  for p in api-lifecycle registry; do
    body=$(job "$b" "$p")
    other=$(other_of "$p")
    guard=$(field 4 if <<<"$body")
    if [ -z "$guard" ]; then
      echo "ref-guard: $b: job $p has no if: restricting it to $branch and its pipeline"
    else
      for s in "refs/heads/main" "refs/heads/feature" "refs/tags/v0.2.0" "refs/pull/1/merge" ""; do
        expr_bad "$guard" false "{\"github\":{\"ref\":\"$s\"},\"inputs\":{\"pipeline\":\"$p\"}}" | tag "ref-guard: $b: job $p, ref '$s': "
      done
      expr_bad "$guard" true "{\"github\":{\"ref\":\"refs/heads/$branch\"},\"inputs\":{\"pipeline\":\"$p\"}}" | tag "ref-guard: $b: job $p: "
      # One pipeline per run: the artifact store is the point.
      expr_bad "$guard" false "{\"github\":{\"ref\":\"refs/heads/$branch\"},\"inputs\":{\"pipeline\":\"$other\"}}" | tag "pipeline-guard: $b: job $p runs in the $other run: "
      expr_bad "$guard" false "{\"github\":{\"ref\":\"refs/heads/$branch\"},\"inputs\":{\"pipeline\":\"\"}}" | tag "pipeline-guard: $b: job $p runs with no pipeline: "
    fi
    [ "$(perms_of "$body")" = "contents: read" ] || echo "perms: $b: job $p has permissions '$(perms_of "$body")', want contents: read"
    runs_code "$body" || echo "build-runs-code: $b: job $p runs no go or make (is the unreviewed build still here?)"
    grep -qE '^          cache: false( |$)' <<<"$body" || echo "cache: $b: job $p runs unreviewed code without setup-go cache: false"
    n=$(grep -c 'uses: actions/upload-artifact@' <<<"$body" || true)
    up=$(grep -A3 'uses: actions/upload-artifact@' <<<"$body" | sed -n 's/^ *name: //p')
    if [ "$n" = 1 ] && [ "$up" = "kb-refresh-$p" ]; then names="$names $up"; else
      echo "artifact-names: $b: job $p uploads $n artifact(s) named '$up', want exactly one named kb-refresh-$p"; fi
  done
  dup=$(tr ' ' '\n' <<<"$names" | grep -v '^$' | sort | uniq -d || true)
  [ -z "$dup" ] || echo "artifact-names: the pipelines share the artifact name $dup"
  awk '/^permissions:/{p=1;next} p&&/^[^ ]/{exit} p' "$b" | sed 's/ *#.*//; s/^ *//' | grep -v '^$' | tr '\n' ' ' | grep -qx 'contents: read ' ||
    echo "perms: $b: the workflow-level permissions are not contents: read"

  # seal: after the build job, no repository code, prints the digest.
  body=$(job "$b" seal)
  if [ -z "$body" ]; then echo "seal: $b has no seal job"; else
    [ "$(perms_of "$body")" = "contents: read" ] || echo "seal: $b: seal has permissions '$(perms_of "$body")', want contents: read"
    runs_code "$body" && echo "seal: $b: seal runs go, make or setup-go"
    grep -qE 'uses: actions/(checkout|upload-artifact)@' <<<"$body" && echo "seal: $b: seal checks out code or uploads an artifact"
    grep -qE '^    needs: \[api-lifecycle, registry\]$' <<<"$body" || echo "seal: $b: seal does not need both build jobs"
    cond=$(field 4 if <<<"$body")
    if [ -z "$cond" ]; then echo "seal: $b: seal has no if:"; else
      for p in api-lifecycle registry; do
        other=$(other_of "$p")
        expr_bad "$cond" true "{\"github\":{\"ref\":\"refs/heads/$branch\"},\"needs\":{\"$p\":{\"result\":\"success\"},\"$other\":{\"result\":\"skipped\"}}}" | tag "seal: $b: "
        expr_bad "$cond" false "{\"github\":{\"ref\":\"refs/heads/$branch\"},\"needs\":{\"$p\":{\"result\":\"failure\"},\"$other\":{\"result\":\"skipped\"}}}" | tag "seal: $b: "
      done
      expr_bad "$cond" false "{\"github\":{\"ref\":\"refs/heads/main\"},\"needs\":{\"api-lifecycle\":{\"result\":\"success\"},\"registry\":{\"result\":\"skipped\"}}}" | tag "seal: $b: "
    fi
    dl=$(grep -A6 'uses: actions/download-artifact@' <<<"$body" | awk 'NR > 1 && /^      - /{exit} {print}')
    grep -qF 'name: kb-refresh-${{ inputs.pipeline }}' <<<"$dl" || echo "seal: $b: seal does not download kb-refresh-\${{ inputs.pipeline }}"
    grep -qE '^          (run-id|github-token):' <<<"$dl" && echo "seal: $b: seal downloads from another run"
    grep -qF 'echo "kb-refresh-patch-sha256 $PIPELINE $hash"' <<<"$body" || echo "seal: $b: seal does not print the kb-refresh-patch-sha256 line"
  fi

  # ---- no cache restored or saved, in either file.
  for f in "$e" "$b"; do
    grep -nE 'uses: actions/cache(/restore|/save)?@|type=gha|cache-from|cache-to|uses: actions/setup-node@' "$f" |
      sed "s|^|cache: $f: |" || true
    grep -nE '^ +cache(-dependency-path)?:' "$f" | grep -vE 'cache: false( |$)' | sed "s|^|cache: $f: a cache setting that is not false: |" || true
  done

  # ---- the entry workflow (main) builds nothing.
  for j in $(job_ids "$e"); do
    body=$(job "$e" "$j")
    case $j in *-run) ;; *) strip_comments "$body" | grep -q 'github.run_attempt' && echo "rerun: $e: job $j uses github.run_attempt (a re-run of this job alone would see a later attempt than the job that dispatched)" ;; esac
    if runs_code "$body"; then echo "entry-runs-code: $e: job $j runs go, make or setup-go on the ref the schedule runs on"; fi
    if grep -q 'uses: actions/upload-artifact@' <<<"$body"; then
      case $j in *-verify) ;; *) echo "entry-artifact: $e: job $j uploads an artifact" ;; esac
    fi
    # A cross-run download presents a token that must not carry a write.
    if grep -qE '^          (run-id|github-token):' <<<"$(strip_comments "$body")" && grep -qE ': write' <<<"$(perms_of "$body")"; then
      echo "download-token: $e: job $j downloads from another run, or names a github-token, while holding '$(perms_of "$body")'"
    fi
  done
  [ "$(job_ids "$e" | tr '\n' ' ')" = 'stage api-lifecycle-run api-lifecycle-verify api-lifecycle-pr registry-run registry-verify registry-pr cleanup report-failure ' ] ||
    echo "jobs: $e has jobs '$(job_ids "$e" | tr '\n' ' ')'"

  body=$(job "$e" stage)
  if [ -z "$body" ]; then echo "stage: $e has no stage job"; else
    [ "$(perms_of "$body")" = "contents: write" ] || echo "stage: $e: stage has permissions '$(perms_of "$body")', want contents: write only"
    grep -qE 'uses: actions/(checkout|setup-[a-z]+)@' <<<"$body" && echo "stage: $e: stage checks out or sets up code; it must run gh only"
    grep -qF -- '-X POST "repos/$GH_REPO/git/refs"' <<<"$body" || echo "stage: $e: stage does not create the branch"
    grep -qF -- '-X DELETE "repos/$GH_REPO/git/refs/$ref"' <<<"$body" || echo "stage: $e: stage does not delete a leftover branch before creating it"
    grep -qF -- '-X PATCH' <<<"$body" && echo "stage: $e: stage moves a leftover branch (a ref update that moves workflow files can be refused)"
    [ "$(field 6 BUILD_BRANCH <<<"$body")" = "$branch" ] || echo "stage: $e: BUILD_BRANCH is not $branch (the build workflow's guard)"
    grep -qF "grep -q 'HTTP 404'" <<<"$body" || echo "stage: $e: stage does not tell a missing branch (404) from other lookup errors"
  fi
  body=$(job "$e" cleanup)
  [ "$(perms_of "$body")" = "contents: write" ] || echo "cleanup: $e: cleanup has permissions '$(perms_of "$body")', want contents: write only"
  runs_code "$body" && echo "cleanup: $e: cleanup runs code"

  vn=''
  for p in api-lifecycle registry; do
    # -- the run job: dispatch for this pipeline, wait, hold actions: write
    # and nothing that reads the artifact.
    body=$(job "$e" "$p-run")
    if [ -z "$body" ]; then echo "run-job: $e has no $p-run job"; else
      [ "$(perms_of "$body")" = "actions: write contents: read" ] ||
        echo "run-job: $e: $p-run has permissions '$(perms_of "$body")', want actions: write and contents: read only"
      grep -qE '^    needs: stage$' <<<"$body" || echo "run-job: $e: $p-run does not need stage alone"
      grep -qE 'uses: actions/(checkout|setup-[a-z]+|download-artifact|upload-artifact)@' <<<"$body" && echo "run-job: $e: $p-run uses an action; it must run gh only"
      [ "$(field 6 PIPELINE <<<"$body")" = "$p" ] || echo "run-job: $e: $p-run's PIPELINE is not $p"
      [ "$(field 6 BUILD_BRANCH <<<"$body")" = "$branch" ] || echo "run-job: $e: $p-run's BUILD_BRANCH is not $branch"
      grep -qF "gh workflow run $(basename "$b") --ref \"\$BUILD_BRANCH\" -f pipeline=\"\$PIPELINE\" -f request=\"\$REQUEST\"" <<<"$body" ||
        echo "run-job: $e: $p-run does not dispatch $(basename "$b") on \$BUILD_BRANCH for \$PIPELINE"
      grep -qF 'select(.displayTitle == "kb-refresh-build " + env.PIPELINE + " " + env.REQUEST and .headSha == env.SHA)' <<<"$body" ||
        echo "run-job: $e: $p-run does not find its run by pipeline, request and commit"
      grep -qF '[ "$conclusion" = success ]' <<<"$body" || echo "run-job: $e: $p-run does not fail on a run that did not succeed"
      # The name the verify job looks for is fixed here, once.
      grep -qF 'request: ${{ steps.run.outputs.request }}' <<<"$body" || echo "rerun: $e: $p-run does not hand on the request name as a job output"
      grep -qF 'echo "request=$REQUEST" >>"$GITHUB_OUTPUT"' <<<"$body" || echo "rerun: $e: $p-run does not write the request name to its step output"
    fi

    # -- the verify job: a read-only token, the run checked BEFORE the
    # download, the digest compared AFTER it, the checked patch re-uploaded.
    body=$(job "$e" "$p-verify")
    if [ -z "$body" ]; then echo "verify: $e has no $p-verify job"; else
      [ "$(perms_of "$body")" = "actions: read contents: read" ] ||
        echo "verify: $e: $p-verify has permissions '$(perms_of "$body")', want actions: read and contents: read only (the cross-run download token)"
      grep -qE "^    needs: $p-run\$" <<<"$body" || echo "verify: $e: $p-verify does not need $p-run alone"
      runs_code "$body" && echo "verify: $e: $p-verify runs go, make or setup-go"
      grep -qE 'uses: actions/checkout@' <<<"$body" && echo "verify: $e: $p-verify checks out the repository"
      [ "$(field 6 PIPELINE <<<"$body")" = "$p" ] || echo "verify: $e: $p-verify's PIPELINE is not $p"
      grep -qF 'RUN_ID: ${{ needs.'"$p"'-run.outputs.run-id }}' <<<"$body" || echo "verify: $e: $p-verify does not check the run $p-run dispatched"
      # "Re-run failed jobs" on this job alone runs it at attempt 2 while the
      # build run is still named for the dispatch: the name must come from
      # the run job's outputs, never from this job's own run_attempt.
      rq=$(field 6 REQUEST <<<"$body")
      [ "$rq" = '${{ needs.'"$p"'-run.outputs.request }}' ] || echo "rerun: $e: $p-verify's REQUEST is '$rq', want the $p-run job's request output"
      expr_bad "$rq" 55-1 "{\"github\":{\"run_id\":55,\"run_attempt\":2},\"needs\":{\"$p-run\":{\"outputs\":{\"request\":\"55-1\"}}}}" | tag "rerun: $e: $p-verify re-run at attempt 2: "
      chk=$(step "Check the run and read the patch's digest" <<<"$body")
      for need in \
        '.id | tostring) != $id' \
        'sub("@.*$"; "")) != ".github/workflows/kb-refresh-build.yml"' \
        '.head_branch != $branch' '.head_sha != $sha' '.event != "workflow_dispatch"' \
        '.status != "completed"' '.conclusion != "success"' \
        '.repository.full_name != $repo' '.head_repository.full_name != $repo' \
        '.display_title != $title' \
        'select(.name == $p or .name == "seal")' 'select(.conclusion == "success")' \
        '.name != $p and .name != "seal" and .conclusion != "skipped"' \
        '= "kb-refresh-$PIPELINE" ]' '.expired' \
        'kb-refresh-patch-sha256 $PIPELINE [0-9a-f]{64}$' \
        "could not fetch the seal job's log" 'for attempt in 1 2 3'; do
        grep -qF -- "$need" <<<"$chk" || echo "verify-run: $e: $p-verify does not check '$need' before it downloads"
      done
      grep -qF 'echo "sha256=${lines##* }" >>"$GITHUB_OUTPUT"' <<<"$chk" || echo "verify-run: $e: $p-verify does not hand on the sealed digest"
      [ "$(grep -c 'uses: actions/download-artifact@' <<<"$body" || true)" = 1 ] || echo "download: $e: $p-verify does not download exactly one artifact"
      dl=$(grep -A8 'uses: actions/download-artifact@' <<<"$body" | awk 'NR > 1 && /^      - /{exit} {print}')
      grep -qE "^          name: kb-refresh-$p\$" <<<"$dl" || echo "download: $e: $p-verify does not download kb-refresh-$p"
      grep -qF 'run-id: ${{ needs.'"$p"'-run.outputs.run-id }}' <<<"$dl" || echo "download: $e: $p-verify downloads without run-id of its own build run"
      grep -qF 'github-token: ${{ github.token }}' <<<"$dl" || echo "download: $e: $p-verify's download names no github-token (a run-id download needs one)"
      ln_check=$(line_of "$body" "- name: Check the run and read the patch's digest")
      ln_dl=$(line_of "$body" 'uses: actions/download-artifact@')
      ln_cmp=$(line_of "$body" '[ "$got" = "$DIGEST" ]')
      ln_up=$(line_of "$body" 'uses: actions/upload-artifact@')
      if ! { [ "$ln_check" -gt 0 ] && [ "$ln_check" -lt "$ln_dl" ] && [ "$ln_dl" -lt "$ln_cmp" ] && [ "$ln_cmp" -lt "$ln_up" ]; }; then
        echo "verify-order: $e: $p-verify must check the run, then download, then compare the digest, then upload (lines $ln_check, $ln_dl, $ln_cmp, $ln_up)"
      fi
      step 'Check the patch against the digest' <<<"$body" | grep -qF '[ "$got" = "$DIGEST" ] || {' ||
        echo "digest: $e: $p-verify does not refuse a patch whose sha256 differs from the sealed digest"
      step 'Check the patch against the digest' <<<"$body" | grep -qF 'got=$(sha256sum "$patch" | cut -d'"' '"' -f1)' ||
        echo "digest: $e: $p-verify does not recompute the patch's sha256"
      [ "$(grep -c 'uses: actions/upload-artifact@' <<<"$body" || true)" = 1 ] || echo "verify: $e: $p-verify does not upload exactly one artifact"
      up=$(grep -A3 'uses: actions/upload-artifact@' <<<"$body" | sed -n 's/^ *name: //p')
      [ "$up" = "kb-refresh-$p-verified" ] || echo "artifact-names: $e: $p-verify uploads '$up', want kb-refresh-$p-verified"
      vn="$vn $up"
    fi

    # -- the PR job: the verified patch from its own run, no run id, no
    # token; only after its verify job succeeded.
    body=$(job "$e" "$p-pr")
    if [ -z "$body" ]; then echo "pr-job: $e has no $p-pr job"; continue; fi
    grep -qE "^    needs: $p-verify\$" <<<"$body" || echo "pr-job: $e: $p-pr does not need $p-verify alone"
    cond=$(field 4 if <<<"$body")
    if [ -z "$cond" ]; then echo "pr-job: $e: $p-pr runs even when its verify job did not succeed"; else
      for s in success failure skipped cancelled; do
        want=false; [ "$s" != success ] || want=true
        expr_bad "$cond" "$want" "{\"needs\":{\"$p-verify\":{\"result\":\"$s\"}}}" | tag "pr-job: $e: $p-pr: "
      done
    fi
    [ "$(grep -c 'uses: actions/download-artifact@' <<<"$body" || true)" = 1 ] || echo "pr-download: $e: $p-pr does not download exactly one artifact"
    dl=$(grep -A8 'uses: actions/download-artifact@' <<<"$body" | awk 'NR > 1 && /^      - /{exit} {print}')
    grep -qE "^          name: kb-refresh-$p-verified\$" <<<"$dl" || echo "pr-download: $e: $p-pr does not download kb-refresh-$p-verified"
    grep -qE '^          (run-id|github-token|repository):' <<<"$(strip_comments "$body")" &&
      echo "pr-download: $e: $p-pr names a run id, a github-token or a repository; the job that holds the writes must present no token to the artifact API"
    grep -q 'uses: actions/upload-artifact@' <<<"$body" && echo "pr-download: $e: $p-pr uploads an artifact"
    grep -qF "kb-refresh-$p-verified/kb-refresh-$p.patch" <<<"$body" || echo "pr-download: $e: $p-pr applies a patch from somewhere else than the verified artifact"
  done
  dup=$(tr ' ' '\n' <<<"$vn" | grep -v '^$' | sort | uniq -d || true)
  [ -z "$dup" ] || echo "artifact-names: the pipelines share the verified artifact name $dup"

  # ---- pinned actions, no token kept in a checkout, in both files.
  for f in "$e" "$b"; do
    grep -E '^ +(- )?uses: ' "$f" | grep -vE '@[0-9a-f]{40}( |$)' | sed "s|^|pin: $f: not pinned by full SHA: |" || true
    n=$(grep -c 'uses: actions/checkout@' "$f" || true)
    m=$(grep -A2 'uses: actions/checkout@' "$f" | grep -cE '^ +persist-credentials: false( |$)' || true)
    [ "$n" = "$m" ] || echo "checkout: $f: $((n - m)) of $n checkouts keep their token"
  done

  # ---- a failed build is not silent: report-failure runs on it.
  body=$(job "$e" report-failure)
  cond=$(field 4 if <<<"$body")
  if [ -z "$cond" ]; then echo "report: $e: report-failure has no if:"; else
    grep -qE '^    needs: \[stage, api-lifecycle-run, registry-run, api-lifecycle-verify, registry-verify, api-lifecycle-pr, registry-pr, cleanup\]$' <<<"$body" ||
      echo "report: $e: report-failure does not need every job before it"
    local all='stage api-lifecycle-run registry-run api-lifecycle-verify registry-verify api-lifecycle-pr registry-pr cleanup' ctx k
    ctx=$(for k in $all; do printf '"%s":{"result":"success","outputs":{}},' "$k"; done)
    ctx="{\"needs\":{${ctx%,}}}"
    expr_bad "$cond" false "$ctx" | tag "report: $e: "
    for k in stage api-lifecycle-run registry-run api-lifecycle-verify registry-verify api-lifecycle-pr registry-pr; do
      expr_bad "$cond" true "$(jq -c --arg k "$k" '.needs[$k].result = "failure"' <<<"$ctx")" | tag "report: $e: $k failing: "
    done
    # A pipeline whose run failed has its verify and PR jobs skipped: the
    # run job's failure reports it.
    expr_bad "$cond" true "$(jq -c '.needs["api-lifecycle-run"].result = "failure" | .needs["api-lifecycle-verify"].result = "skipped" | .needs["api-lifecycle-pr"].result = "skipped"' <<<"$ctx")" | tag "report: $e: "
  fi
}

real=$(audit "$entry" "$build") || true
if [ -z "$real" ]; then
  ok "$entry and $build: unreviewed jobs only in a run per pipeline on $branch, no cache, the run verified before download, the digest compared, no token from a write job"
else
  fail "the structure rules fail on the real workflows:"
  sed 's/^/     /' <<<"$real" >&2
fi

# --- mutants: each must fail the rule it breaks ------------------------------

# mutant <name> <tag> <entry-perl|-> <build-perl|->: copies of the two files
# with the perl substitutions applied; the audit must report <tag>. Run at
# most KB_JOBS (default 4) at a time: each audit is a few hundred greps.
mutant() {
  while [ "$(jobs -rp | wc -l | tr -d ' ')" -ge "${KB_JOBS:-4}" ]; do sleep 0.2; done
  mutant_run "$@" &
}
mutant_run() {
  local name=$1 tg=$2 ep=$3 bp=$4 d got
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
  if grep -q "^$tg:" <<<"$got"; then ok "mutant fails $tg: $name"; else
    fail "mutant '$name' is not caught by '$tg'; the audit says: ${got:-nothing}"; fi
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
mutant "a build job without the ref guard" ref-guard - 's/ref == .refs\/heads\/bot\/kb-refresh-build. && inputs.pipeline == .registry./inputs.pipeline == \x27registry\x27/m'
mutant "a build job guarded to main" ref-guard - 's/refs\/heads\/bot\/kb-refresh-build\x27 && inputs.pipeline == \x27registry/refs\/heads\/main\x27 \&\& inputs.pipeline == \x27registry/m'
mutant "a build job guarded on a prefix match" ref-guard - 's/github.ref == \x27refs\/heads\/bot\/kb-refresh-build\x27 && inputs.pipeline == \x27api-lifecycle\x27/startsWith(github.ref, \x27refs\/heads\/\x27) \&\& inputs.pipeline == \x27api-lifecycle\x27/m'
# Both pipelines' jobs in one run: each could upload the other's name first.
mutant "the registry job also runs in the api-lifecycle run" pipeline-guard - 's/ && inputs.pipeline == .registry.\n/\n/m'
mutant "both build jobs run in either run (one shared artifact store)" pipeline-guard - 's/ && inputs.pipeline == .[a-z-]+.\n/\n/g'
mutant "the registry job runs in every run whose pipeline is set" pipeline-guard - 's/inputs.pipeline == .registry./inputs.pipeline != \x27\x27/m'
mutant "the run name drops the pipeline" run-name - 's/^run-name: kb-refresh-build \$\{\{ inputs.pipeline \}\} /run-name: kb-refresh-build /m'
mutant "setup-go with a cache in an unreviewed job" cache - 's/cache: false\n/cache: true\n/'
mutant "an actions/cache step in the build workflow" cache - 's/(      - name: Validate the regenerated KB\n)/      - uses: actions\/cache\@0000000000000000000000000000000000000000 # v4\n        with:\n          path: ~\/go\/pkg\/mod\n          key: k\n$1/'
mutant "an unreviewed job with write permissions" perms - 's/^(  registry:\n(?:.*\n)*?    permissions:\n      contents: )read/${1}write/m'
mutant "an unreviewed job with an extra permission" perms - 's/^(  api-lifecycle:\n(?:.*\n)*?      contents: read\n)/$1      actions: write\n/m'
mutant "the registry pipeline uploads under the api pipeline's name" artifact-names - 's/name: kb-refresh-registry/name: kb-refresh-api-lifecycle/'
mutant "the verified artifacts share one name" artifact-names 's/kb-refresh-registry-verified/kb-refresh-api-lifecycle-verified/g' -
mutant "the seal job checks out the repository" seal - 's/(  seal:\n(?:.*\n)*?    steps:\n)/$1      - uses: actions\/checkout\@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1\n        with:\n          persist-credentials: false\n/m'
mutant "the seal job prints no digest" seal - 's/echo "kb-refresh-patch-sha256 \$PIPELINE \$hash"/echo sealed/'
mutant "the seal job runs when the build job failed" seal - 's/needs.api-lifecycle.result == .success. \|\| needs.registry.result == .success./needs.api-lifecycle.result != \x27x\x27/'
mutant "the seal job downloads from another run" seal - 's/(      - uses: actions\/download-artifact\@[0-9a-f]{40} # v8.0.2\n        with:\n)(          name: kb-refresh-\$\{\{ inputs.pipeline \}\})/$1          run-id: 1\n$2/'

# The consumer's verification.
mutant "a PR job that downloads by run id from the build run" pr-download 's/(  registry-pr:\n(?:.*\n)*?      - uses: actions\/download-artifact\@[0-9a-f]{40} # v8.0.2\n        with:\n)/$1          run-id: \${{ needs.registry-run.outputs.run-id }}\n/' -
mutant "a PR job whose download names a token" pr-download 's/(  api-lifecycle-pr:\n(?:.*\n)*?      - uses: actions\/download-artifact\@[0-9a-f]{40} # v8.0.2\n        with:\n)/$1          github-token: \${{ github.token }}\n/' -
mutant "a PR job downloading the other pipeline's artifact" pr-download 's/(  registry-pr:\n(?:.*\n)*?          name: )kb-refresh-registry-verified/${1}kb-refresh-api-lifecycle-verified/' -
mutant "a PR job downloading the build run's artifact, not the verified one" pr-download 's/(  registry-pr:\n(?:.*\n)*?          name: kb-refresh-registry)-verified/$1/' -
mutant "the verify job holds a write permission (its download token)" verify 's/(  registry-verify:\n(?:.*\n)*?      actions: )read/${1}write/' -
mutant "the verify job also holds contents: write" verify 's/(  api-lifecycle-verify:\n(?:.*\n)*?      contents: )read/${1}write/' -
mutant "the verify job checks out the repository" verify 's/(  registry-verify:\n(?:.*\n)*?    steps:\n)/$1      - uses: actions\/checkout\@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1\n        with:\n          persist-credentials: false\n/' -
mutant "a download from another run in a job that holds writes" download-token 's/(  registry-pr:\n(?:.*\n)*?      - uses: actions\/download-artifact\@[0-9a-f]{40} # v8.0.2\n        with:\n)/$1          github-token: \${{ github.token }}\n/' -
mutant "the verify job downloads without a run id" download 's/(  api-lifecycle-verify:\n(?:.*\n)*?)          run-id: \$\{\{ needs.api-lifecycle-run.outputs.run-id \}\}\n/$1/' -
mutant "the verify job downloads the other pipeline's artifact" download 's/(  registry-verify:\n(?:.*\n)*?          name: )kb-refresh-registry\n/${1}kb-refresh-api-lifecycle\n/' -
mutant "the verify job downloads with no token" download 's/(  registry-verify:\n(?:.*\n)*?)          github-token: \$\{\{ github.token \}\}\n/$1/' -
# Each field of the run the verify job must check: dropping its line fails
# the structure rule.
for pair in \
  'id|\(if \(\(\.id \| tostring\) != \$id\)' \
  'workflow file|\(if \(\(\.path \/\/ ""\)' \
  'branch|\(if \.head_branch != \$branch' \
  'commit|\(if \.head_sha != \$sha' \
  'event|\(if \.event != "workflow_dispatch"' \
  'status|\(if \.status != "completed"' \
  'conclusion|\(if \.conclusion != "success"' \
  'repository|\(if \.repository\.full_name != \$repo' \
  'head repository (fork)|\(if \.head_repository\.full_name != \$repo' \
  'name|\(if \.display_title != \$title'; do
  mutant "the verify job does not check the run's ${pair%%|*}" verify-run "s/^ +(?:\\[ )?${pair#*|}[^\\n]*\\n//m" -
done
mutant "the verify job does not check the pipeline's jobs succeeded" verify-run 's/\| select\(\.conclusion == "success"\) //' -
mutant "the verify job does not check for jobs of the other pipeline" verify-run 's/\.name != \$p and \.name != "seal" and \.conclusion != "skipped"/.name == "none"/' -
mutant "the verify job does not check the run's artifacts" verify-run 's/\[ "\$found" = "kb-refresh-\$PIPELINE" \]/true/' -
mutant "the verify job does not read the sealed digest from one line" verify-run 's/kb-refresh-patch-sha256 \$PIPELINE \[0-9a-f\]\{64\}\$/kb-refresh-patch-sha256 [a-z-]+ [0-9a-f]{64}/' -
mutant "the verify job compares no digest" digest 's/\[ "\$got" = "\$DIGEST" \] \|\| \{/[ -n "\$got" ] || {/' -
mutant "the verify job does not recompute the digest" digest 's/got=\$\(sha256sum "\$patch" \| cut -d. . -f1\)/got=\$DIGEST/' -
mutant "the verify job downloads before checking the run" verify-order \
  's/(      # Refuse any run that is not the one this workflow dispatched.*?\n)(      - uses: actions\/download-artifact\@[0-9a-f]{40} # v8.0.2\n        with:\n          # The checked run.*?\n          path: [^\n]*\n\n)/$2$1/s' -
mutant "the verify job uploads before comparing the digest" verify-order \
  's/(      # The downloaded patch is the one the seal job hashed\.\n.*?\n\n)(      - uses: actions\/upload-artifact\@[0-9a-f]{40} # v7.0.2\n.*?retention-days: 1\n)/$2\n$1/s' -

mutant "a run job holding more than it needs" run-job 's/(  registry-run:\n(?:.*\n)*?      contents: )read/${1}write/' -
mutant "a run job dispatching without a pipeline" run-job 's/ -f pipeline="\$PIPELINE"//' -
mutant "the registry run job dispatching the api-lifecycle pipeline" run-job 's/(  registry-run:\n(?:.*\n)*?      PIPELINE: )registry/${1}api-lifecycle/' -
mutant "a run job that does not fail on a failed run" run-job 's/\[ "\$conclusion" = success \]/true/' -
mutant "a run job that finds its run by request only" run-job 's/ and \.headSha == env\.SHA//' -
mutant "the stage job moves a leftover branch" stage 's/gh api -X DELETE "repos\/\$GH_REPO\/git\/refs\/\$ref" >\/dev\/null/gh api -X PATCH "repos\/\$GH_REPO\/git\/refs\/\$ref" -f sha="\$SHA" -F force=true >\/dev\/null/' -
mutant "the stage job checks out the repository" stage 's/(  stage:\n(?:.*\n)*?    steps:\n)/$1      - uses: actions\/checkout\@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1\n        with:\n          persist-credentials: false\n/' -
mutant "the stage job holds more than it needs" stage 's/(  stage:\n(?:.*\n)*?      contents: write[^\n]*\n)/$1      actions: write\n/' -
mutant "the stage job stages main on another branch" stage 's/(  stage:\n(?:.*\n)*?      BUILD_BRANCH: )bot\/kb-refresh-build/${1}bot\/other/' -
mutant "the cleanup job holds more than it needs" cleanup 's/(  cleanup:\n(?:.*\n)*?      contents: write[^\n]*\n)/$1      actions: write\n/' -
# Re-running.
mutant "the verify job derives the request name from its own run attempt" rerun 's/(  registry-verify:\n(?:.*\n)*?      REQUEST: )\$\{\{ needs\.registry-run\.outputs\.request \}\}/$1\${{ github.run_id }}-\${{ github.run_attempt }}/' -
mutant "the api-lifecycle verify job derives the request name from its own run attempt" rerun 's/(  api-lifecycle-verify:\n(?:.*\n)*?      REQUEST: )\$\{\{ needs\.api-lifecycle-run\.outputs\.request \}\}/$1\${{ github.run_id }}-\${{ github.run_attempt }}/' -
mutant "a run job that hands on no request name" rerun 's/(  registry-run:\n(?:.*\n)*?    outputs:\n)      request: [^\n]*\n/$1/' -
mutant "a PR job that derives a name from the run attempt" rerun 's/(  registry-pr:\n(?:.*\n)*?    runs-on: ubuntu-latest\n)/$1    env:\n      N: \${{ github.run_attempt }}\n/' -
mutant "the verify job reports a failed log fetch as a missing digest" verify-run 's/could not fetch the seal job.s log/the seal job log is odd/g' -
mutant "the verify job does not fetch the log again" verify-run 's/for attempt in 1 2 3/for attempt in 1/g' -
mutant "the stage job reads every lookup error as an absent branch" stage 's/elif grep -q .HTTP 404. <<<"\$err"; then/elif true; then/' -
mutant "a PR job that runs whatever its verify did" pr-job 's/^    if: needs\.registry-verify\.result == .success.\n//m' -
mutant "a PR job that runs whatever the other pipeline did" pr-job 's/needs\.api-lifecycle-verify\.result == .success./needs.registry-verify.result == \x27success\x27/' -
mutant "an action not pinned by SHA" pin 's/actions\/download-artifact\@[0-9a-f]{40}/actions\/download-artifact\@v8/' -
mutant "a checkout that keeps its token" checkout - 's/(uses: actions\/checkout\@[0-9a-f]{40} # v7.0.1\n        with:\n          persist-credentials: )false/${1}true/'
mutant "a failed build run that report-failure ignores" report 's/ needs\.api-lifecycle-run\.result == .failure. \|\|//' -
mutant "a failed stage that report-failure ignores" report 's/needs\.stage\.result == .failure. \|\| *//' -
mutant "a failed verify that report-failure ignores" report 's/ \|\|\n      needs\.api-lifecycle-verify\.result == .failure. \|\| needs\.registry-verify\.result == .failure.//' -

wait

# --- the entry workflow's scripts, against a stub gh --------------------------

mkdir -p "$work/bin"
# gh: logs each call. Answers from files in $STUB: ref-exists (present: the
# staging branch exists; ref-error: looking it up fails with a 500), list.json (gh run list's JSON), status.N (the n-th
# run reading of the build run, "<status> <conclusion>", the last once they
# run out), run.json / jobs.json / artifacts.json / logs.txt (the build run as
# the verify job reads it). `gh api` / `gh run list` apply --jq with jq, as gh
# does. The job's log is the one thing gh never serves here (it refuses a
# response with escape sequences, as the runner's does); the stub curl serves it.
cat >"$work/bin/gh" <<'STUB'
#!/usr/bin/env bash
echo "gh $*" >>"$GH_LOG"
jqf() { local f=; while [ $# -gt 0 ]; do [ "$1" = --jq ] && f=$2; shift; done; jq -r "${f:-.}"; }
case "$1 $2" in
  "api repos/o/r/git/ref/heads/bot/kb-refresh-build")
    [ ! -e "$STUB/ref-error" ] || { echo 'gh: Internal Server Error (HTTP 500)' >&2; exit 1; }
    [ -e "$STUB/ref-exists" ] || { echo 'gh: Not Found (HTTP 404)' >&2; exit 1; }
    echo '{}' ;;
  "api -X")
    case "$3 $4" in
      "POST repos/o/r/git/refs" | "DELETE repos/o/r/git/refs/heads/bot/kb-refresh-build") echo '{}' ;;
      *) echo "stub gh: unexpected call: $*" >&2; exit 2 ;;
    esac ;;
  "workflow run") : ;;
  "run list") jqf "$@" <"$STUB/list.json" ;;
  "run cancel") : ;;
  "api repos/o/r/actions/runs/777")
    if [ -e "$STUB/run.json" ]; then jqf "$@" <"$STUB/run.json"; else
      n=$(($(cat "$STUB/status.n" 2>/dev/null || echo 0) + 1)); echo "$n" >"$STUB/status.n"
      last=$(ls "$STUB" | grep -c '^status\.[0-9]' || true)
      read -r st co <"$STUB/status.$((n < last ? n : last))"
      jq -n --arg s "$st" --arg c "${co:-none}" '{status: $s, conclusion: (if $c == "none" then null else $c end)}' | jqf "$@"
    fi ;;
  "api repos/o/r/actions/runs/777/jobs?per_page=100") jqf "$@" <"$STUB/jobs.json" ;;
  "api repos/o/r/actions/runs/777/artifacts?per_page=100") jqf "$@" <"$STUB/artifacts.json" ;;
  "api repos/o/r/actions/jobs/13/logs")
    # What the runner's gh does with a job log: it holds ANSI escapes, so gh
    # refuses to print it. The verify job must not depend on gh for the log.
    echo 'the response contains terminal escape sequences; pass --allow-escape-sequences to output it anyway' >&2
    exit 1 ;;
  *) echo "stub gh: unexpected call: $*" >&2; exit 2 ;;
esac
STUB
# curl: the seal job's log, as the blob store serves it after the redirect.
# Logs each call's arguments. Answers the one URL the verify job may fetch,
# with the token it holds; logs-fail holds the number of calls that fail
# first (curl -f: a failed HTTP status exits 22, nothing on stdout).
cat >"$work/bin/curl" <<'STUB'
#!/usr/bin/env bash
echo "curl $*" >>"$CURL_LOG"
url=${!#}
[ "$url" = "https://api.github.com/repos/o/r/actions/jobs/13/logs" ] || { echo "stub curl: unexpected url: $url" >&2; exit 2; }
case " $* " in *" -H Authorization: Bearer stub "*) ;; *) echo "stub curl: no Authorization header" >&2; exit 22 ;; esac
if [ -e "$STUB/logs-fail" ]; then
  n=$(($(cat "$STUB/logs.n" 2>/dev/null || echo 0) + 1)); echo "$n" >"$STUB/logs.n"
  if [ "$n" -le "$(cat "$STUB/logs-fail")" ]; then echo 'curl: (22) The requested URL returned error: 502' >&2; exit 22; fi
fi
cat "$STUB/logs.txt"
STUB
cat >"$work/bin/sleep" <<'STUB'
#!/usr/bin/env bash
echo "$*" >>"$SLEEP_LOG"
STUB
chmod +x "$work/bin/gh" "$work/bin/curl" "$work/bin/sleep"
sha=1111111111111111111111111111111111111111
out() { sort "$work/bc.out" | tr '\n' ' ' | sed 's/ $//'; }

# -- stage
stage_body=$(job "$entry" stage)
step "Point the staging branch at main's head" <<<"$stage_body" | run_block >"$work/stage.sh"
[ -s "$work/stage.sh" ] || fail "the stage job has no 'Point the staging branch' run block"
stage_case() { # exists|absent
  export STUB=$work/stub
  rm -rf "$STUB"; mkdir -p "$STUB"
  [ "$1" = exists ] && touch "$STUB/ref-exists"
  [ "$1" = error ] && touch "$STUB/ref-error"
  : >"$work/st.gh"; SC_RC=0
  PATH="$work/bin:$PATH" GH_LOG="$work/st.gh" GH_REPO=o/r SHA=$sha BUILD_BRANCH=$branch GH_TOKEN=stub \
    bash --noprofile --norc -e "$work/stage.sh" >"$work/st.log" 2>&1 || SC_RC=$?
}
stage_case absent
if [ "$SC_RC" = 0 ] && grep -q "api -X POST repos/o/r/git/refs -f ref=refs/heads/$branch -f sha=$sha" "$work/st.gh" && ! grep -qE -- '-X (PATCH|DELETE)' "$work/st.gh"; then
  ok "stage: creates the staging branch at the commit"
else fail "stage: new branch: rc $SC_RC: $(cat "$work/st.gh")"; fi
stage_case error
if [ "$SC_RC" != 0 ] && ! grep -qE -- '-X (POST|PATCH|DELETE)' "$work/st.gh" && grep -q '::error::could not look up' "$work/st.log"; then
  ok "stage: a lookup error that is not a 404 stops, naming the error, instead of reading as an absent branch"
else fail "stage: lookup error: rc $SC_RC: $(cat "$work/st.gh") $(cat "$work/st.log")"; fi
stage_case exists
if [ "$SC_RC" = 0 ] && ! grep -q -- '-X PATCH' "$work/st.gh" &&
  [ "$(grep -n -- '-X DELETE' "$work/st.gh" | head -1 | cut -d: -f1)" -lt "$(grep -n -- '-X POST' "$work/st.gh" | head -1 | cut -d: -f1)" ]; then
  ok "stage: deletes a leftover staging branch and creates it again, never moves it"
else fail "stage: leftover branch: rc $SC_RC: $(cat "$work/st.gh")"; fi

# -- <p>-run
title_of() { echo "kb-refresh-build $1 55-1"; }
# run_case <pipeline> <list.json> <status "<status> <conclusion>">...: runs
# the pipeline's run script; leaves rc, outputs and the gh log in $work/bc.*.
run_case() {
  local p=$1 list=$2 i=0 s
  shift 2
  export STUB=$work/stub
  rm -rf "$STUB" "$work"/bc.*
  mkdir -p "$STUB"
  echo "$list" >"$STUB/list.json"
  for s in "$@"; do i=$((i + 1)); echo "$s" >"$STUB/status.$i"; done
  : >"$work/bc.gh"; : >"$work/bc.sleep"; : >"$work/bc.out"
  step "Run the unreviewed $p build on the staging branch" < <(job "$entry" "$p-run") | run_block >"$work/run-$p.sh"
  [ -s "$work/run-$p.sh" ] || { fail "$p-run has no run block"; BC_RC=99; return; }
  BC_RC=0
  PATH="$work/bin:$PATH" GH_LOG="$work/bc.gh" SLEEP_LOG="$work/bc.sleep" GITHUB_OUTPUT="$work/bc.out" \
    GITHUB_SERVER_URL=https://github.com GH_REPO=o/r SHA=$sha REQUEST=55-1 BUILD_BRANCH=$branch PIPELINE=$p GH_TOKEN=stub \
    bash --noprofile --norc -e "$work/run-$p.sh" >"$work/bc.log" 2>&1 || BC_RC=$?
}
for p in api-lifecycle registry; do
  o=$(other_of "$p")
  list_ok="[{\"databaseId\":776,\"displayTitle\":\"$(title_of "$p")\",\"headSha\":\"2222222222222222222222222222222222222222\"},{\"databaseId\":775,\"displayTitle\":\"kb-refresh-build $p 54-1\",\"headSha\":\"$sha\"},{\"databaseId\":778,\"displayTitle\":\"$(title_of "$o")\",\"headSha\":\"$sha\"},{\"databaseId\":777,\"displayTitle\":\"$(title_of "$p")\",\"headSha\":\"$sha\"}]"
  run_case "$p" "$list_ok" "queued none" "in_progress none" "completed success"
  if [ "$BC_RC" = 0 ] && grep -qxF "gh workflow run kb-refresh-build.yml --ref $branch -f pipeline=$p -f request=55-1" "$work/bc.gh" &&
    [ "$(out)" = "request=55-1 run-id=777" ]; then
    ok "$p-run: dispatches for its own pipeline, finds its run by pipeline, request and commit (not the lookalikes or the other pipeline's), waits for success"
  else fail "$p-run: success: rc $BC_RC, outputs '$(out)': $(cat "$work/bc.gh") $(tail -5 "$work/bc.log")"; fi

  for c in failure cancelled timed_out skipped; do
    run_case "$p" "$list_ok" "completed $c"
    if [ "$BC_RC" != 0 ] && grep -q '^run-id=777' "$work/bc.out" && grep -q "::error::.*concluded $c" "$work/bc.log"; then
      ok "$p-run: fails, naming the run, when the run concluded $c"
    else fail "$p-run: conclusion $c: rc $BC_RC, outputs '$(out)': $(tail -3 "$work/bc.log")"; fi
  done

  run_case "$p" '[]' "completed success"
  if [ "$BC_RC" != 0 ] && ! grep -q '^run-id=' "$work/bc.out" && grep -q '::error::' "$work/bc.log"; then
    ok "$p-run: fails, with no run id, when the dispatched run never appears"
  else fail "$p-run: no run: rc $BC_RC, outputs '$(out)': $(tail -3 "$work/bc.log")"; fi

  # Only the other pipeline's run exists for this request: not ours.
  run_case "$p" "[{\"databaseId\":778,\"displayTitle\":\"$(title_of "$o")\",\"headSha\":\"$sha\"}]" "completed success"
  if [ "$BC_RC" != 0 ] && ! grep -q '^run-id=' "$work/bc.out"; then
    ok "$p-run: never adopts the other pipeline's run"
  else fail "$p-run: the other pipeline's run was adopted: rc $BC_RC, outputs '$(out)'"; fi

  # Still going after the wait: the job fails (the cancel step then runs).
  run_case "$p" "$list_ok" "in_progress none"
  if [ "$BC_RC" != 0 ] && grep -q '^run-id=777' "$work/bc.out" && grep -q '::error::' "$work/bc.log"; then
    ok "$p-run: fails when the run never completes, after a bounded wait"
  else fail "$p-run: run never completes: rc $BC_RC, outputs '$(out)': $(tail -3 "$work/bc.log")"; fi
done

# -- cancel an unfinished run, and cleanup
cancel_case() { # status ID
  export STUB=$work/stub
  rm -rf "$STUB" "$work"/cc.*; mkdir -p "$STUB"; echo "$1" >"$STUB/status.1"
  : >"$work/cc.gh"; CC_RC=0
  step 'Cancel an unfinished run' < <(job "$entry" api-lifecycle-run) | run_block >"$work/cancel.sh"
  PATH="$work/bin:$PATH" GH_LOG="$work/cc.gh" SLEEP_LOG=/dev/null GH_REPO=o/r ID=$2 GH_TOKEN=stub \
    bash --noprofile --norc -e "$work/cancel.sh" >"$work/cc.log" 2>&1 || CC_RC=$?
}
cancel_case in_progress 777
if [ "$CC_RC" = 0 ] && grep -q '^gh run cancel 777' "$work/cc.gh"; then ok "cancel: cancels a run still going"; else fail "cancel: unfinished run: rc $CC_RC: $(cat "$work/cc.gh")"; fi
cancel_case completed 777
if [ "$CC_RC" = 0 ] && ! grep -q 'run cancel' "$work/cc.gh"; then ok "cancel: leaves a finished run alone"; else fail "cancel: finished run: rc $CC_RC: $(cat "$work/cc.gh")"; fi
cancel_case completed ''
if [ "$CC_RC" = 0 ] && ! grep -q 'run cancel' "$work/cc.gh"; then ok "cancel: with no run id (the dispatch failed) it does nothing"; else fail "cancel: no run id: rc $CC_RC: $(cat "$work/cc.gh")"; fi

job "$entry" cleanup | grep -E '^        run: ' | sed 's/^        run: //' >"$work/cleanup.sh"
export STUB=$work/stub; rm -rf "$STUB"; mkdir -p "$STUB"; : >"$work/cl.gh"; CL_RC=0
PATH="$work/bin:$PATH" GH_LOG="$work/cl.gh" GH_REPO=o/r BUILD_BRANCH=$branch GH_TOKEN=stub bash --noprofile --norc -e "$work/cleanup.sh" >/dev/null 2>&1 || CL_RC=$?
if [ "$CL_RC" = 0 ] && grep -q "api -X DELETE repos/o/r/git/refs/heads/$branch" "$work/cl.gh"; then ok "cleanup: deletes the staging branch"; else fail "cleanup: rc $CL_RC: $(cat "$work/cl.gh")"; fi

# --- the build workflow's seal job, and the verify job against its output -----

seal_body=$(job "$build" seal)
step "Print the patch's digest" <<<"$seal_body" | run_block >"$work/seal.sh"
[ -s "$work/seal.sh" ] || fail "the seal job has no 'Print the patch's digest' run block"
# seal_case <pipeline> <dir>: the digest line the seal job prints.
seal_case() {
  SEAL_RC=0
  SEALED=$2 PIPELINE=$1 bash --noprofile --norc -e "$work/seal.sh" >"$work/seal.out" 2>"$work/seal.err" || SEAL_RC=$?
}
patchdir=$work/patch; mkdir -p "$patchdir"
printf 'diff --git a/x b/x\n+one\n' >"$patchdir/kb-refresh-api-lifecycle.patch"
want_hash=$(shasum -a 256 "$patchdir/kb-refresh-api-lifecycle.patch" | cut -d' ' -f1)
seal_case api-lifecycle "$patchdir"
if [ "$SEAL_RC" = 0 ] && [ "$(cat "$work/seal.out")" = "kb-refresh-patch-sha256 api-lifecycle $want_hash" ]; then
  ok "seal: prints one 'kb-refresh-patch-sha256 <pipeline> <sha256>' line for the artifact's patch"
else fail "seal: rc $SEAL_RC: $(cat "$work/seal.out" "$work/seal.err")"; fi
touch "$patchdir/extra"
seal_case api-lifecycle "$patchdir"
if [ "$SEAL_RC" != 0 ] && ! grep -q kb-refresh-patch-sha256 "$work/seal.out"; then ok "seal: refuses an artifact of more than one file"; else fail "seal: extra file: rc $SEAL_RC"; fi
rm "$patchdir/extra"
seal_case registry "$patchdir"
if [ "$SEAL_RC" != 0 ] && ! grep -q kb-refresh-patch-sha256 "$work/seal.out"; then ok "seal: refuses an artifact that is not this pipeline's patch"; else fail "seal: wrong pipeline: rc $SEAL_RC"; fi

# -- verify: the run check
vstep() { step "Check the run and read the patch's digest" < <(job "$entry" "$1-verify") | run_block >"$work/verify-$1.sh"; [ -s "$work/verify-$1.sh" ] || fail "$1-verify has no check run block"; }
# v_reset <pipeline>: a good run, as the API returns it.
v_reset() {
  local p=$1
  export STUB=$work/stub
  rm -rf "$STUB"; mkdir -p "$STUB"
  jq -n --arg sha "$sha" --arg p "$p" '{id: 777, path: ".github/workflows/kb-refresh-build.yml", head_branch: "bot/kb-refresh-build", head_sha: $sha,
    event: "workflow_dispatch", status: "completed", conclusion: "success", repository: {full_name: "o/r"}, head_repository: {full_name: "o/r"},
    display_title: ("kb-refresh-build " + $p + " 55-1")}' >"$STUB/run.json"
  jq -n --arg p "$p" --arg o "$(other_of "$p")" '{jobs: [{id: 11, name: $p, conclusion: "success"}, {id: 12, name: $o, conclusion: "skipped"}, {id: 13, name: "seal", conclusion: "success"}]}' >"$STUB/jobs.json"
  jq -n --arg p "$p" '{artifacts: [{name: ("kb-refresh-" + $p), expired: false}]}' >"$STUB/artifacts.json"
  {
    echo "2026-10-12T06:20:00.1000000Z ##[group]Run set -euo pipefail"
    printf '2026-10-12T06:20:00.1000002Z \033[36;1mhash=$(sha256sum patch)\033[0m\n'
    echo "2026-10-12T06:20:00.1000001Z   echo \"kb-refresh-patch-sha256 \$PIPELINE \$hash\""
    echo "2026-10-12T06:20:00.2000000Z kb-refresh-patch-sha256 $p $good_digest"
  } >"$STUB/logs.txt"
}
good_digest=$(printf 'c%.0s' $(seq 64))
# v_case <label> <pipeline> <want: pass|refuse> [<file> <jq filter>]...
v_case() {
  local label=$1 p=$2 want=$3 file filter
  shift 3
  v_reset "$p"
  [ -z "${V_LOGS_FAIL:-}" ] || echo "$V_LOGS_FAIL" >"$STUB/logs-fail"
  while [ $# -ge 2 ]; do
    file=$1 filter=$2; shift 2
    jq -c "$filter" "$STUB/$file" >"$STUB/tmp" && mv "$STUB/tmp" "$STUB/$file"
  done
  vstep "$p"
  : >"$work/v.gh"; : >"$work/v.curl"; : >"$work/v.out"; V_RC=0
  PATH="$work/bin:$PATH" GH_LOG="$work/v.gh" CURL_LOG="$work/v.curl" SLEEP_LOG=/dev/null GITHUB_OUTPUT="$work/v.out" GH_REPO=o/r SHA=$sha PIPELINE=$p REQUEST=${V_REQUEST:-55-1} \
    BUILD_BRANCH=$branch RUN_ID=777 GH_TOKEN=stub bash --noprofile --norc -e "$work/verify-$p.sh" >"$work/v.log" 2>&1 || V_RC=$?
  case $want in
    pass) [ "$V_RC" = 0 ] && [ "$(cat "$work/v.out")" = "sha256=$good_digest" ] ;;
    refuse) [ "$V_RC" != 0 ] && [ ! -s "$work/v.out" ] && grep -q '::error::refusing run 777' "$work/v.log" ;;
  esac && ok "verify($p): $label" || fail "verify($p): $label: rc $V_RC, output '$(cat "$work/v.out")': $(tail -4 "$work/v.log")"
}
for p in api-lifecycle registry; do
  o=$(other_of "$p")
  v_case "the run it dispatched is accepted, and the digest is read from the seal job's log (decoy lines ignored)" "$p" pass
  v_case "the workflow file with an @ref suffix, as the API shows it for some events, is accepted" "$p" pass run.json '.path += "@refs/heads/bot/kb-refresh-build"'
  v_case "a run of another workflow file is refused" "$p" refuse run.json '.path = ".github/workflows/ci.yml"'
  v_case "a run of a workflow file with the same name elsewhere is refused" "$p" refuse run.json '.path = ".github/workflows/sub/kb-refresh-build.yml"'
  v_case "a run on main is refused" "$p" refuse run.json '.head_branch = "main"'
  v_case "a run on another branch is refused" "$p" refuse run.json '.head_branch = "bot/other"'
  v_case "a run at another commit is refused" "$p" refuse run.json '.head_sha = "2222222222222222222222222222222222222222"'
  v_case "a run of another event (push) is refused" "$p" refuse run.json '.event = "push"'
  v_case "a pull_request run is refused" "$p" refuse run.json '.event = "pull_request"'
  v_case "a run still in progress is refused" "$p" refuse run.json '.status = "in_progress" | .conclusion = null'
  v_case "a failed run is refused" "$p" refuse run.json '.conclusion = "failure"'
  v_case "a cancelled run is refused" "$p" refuse run.json '.conclusion = "cancelled"'
  v_case "a run of another repository is refused" "$p" refuse run.json '.repository.full_name = "evil/r"'
  v_case "a run whose head is a fork is refused" "$p" refuse run.json '.head_repository.full_name = "fork/r"'
  v_case "a run with another id is refused" "$p" refuse run.json '.id = 778'
  v_case "the other pipeline's run is refused" "$p" refuse run.json ".display_title = \"kb-refresh-build $o 55-1\""
  v_case "another request's run is refused" "$p" refuse run.json '.display_title = "kb-refresh-build '"$p"' 54-1"'
  v_case "a run whose pipeline job failed is refused" "$p" refuse jobs.json "(.jobs[] | select(.name == \"$p\") | .conclusion) = \"failure\""
  v_case "a run with no sealed job is refused" "$p" refuse jobs.json '.jobs |= map(select(.name != "seal"))'
  v_case "a run whose seal job failed is refused" "$p" refuse jobs.json '(.jobs[] | select(.name == "seal") | .conclusion) = "failure"'
  v_case "a run in which the other pipeline's job also ran is refused" "$p" refuse jobs.json "(.jobs[] | select(.name == \"$o\") | .conclusion) = \"success\""
  v_case "a run in which a job of another name ran is refused" "$p" refuse jobs.json '.jobs += [{id: 14, name: "extra", conclusion: "success"}]'
  v_case "a run with the other pipeline's artifact is refused" "$p" refuse artifacts.json ".artifacts[0].name = \"kb-refresh-$o\""
  v_case "a run with a second artifact is refused" "$p" refuse artifacts.json '.artifacts += [{name: "kb-refresh-extra", expired: false}]'
  v_case "a run with no artifact is refused" "$p" refuse artifacts.json '.artifacts = []'
  v_case "a run whose artifact expired is refused, telling the maintainer to re-run the workflow" "$p" refuse artifacts.json '.artifacts[0].expired = true'
done
# Re-running the verify job alone ("Re-run failed jobs") runs it at attempt 2;
# the build run is still named for the dispatch, and the job takes that name
# from the run job's outputs.
for p in api-lifecycle registry; do
  rq_expr=$(field 6 REQUEST <<<"$(job "$entry" "$p-verify")")
  rq=$(expr "$rq_expr" "{\"github\":{\"run_id\":55,\"run_attempt\":2},\"needs\":{\"$p-run\":{\"outputs\":{\"request\":\"55-1\"}}}}" 2>/dev/null) || rq=''
  V_REQUEST=$rq v_case "a re-run at attempt 2 checks the run that was dispatched (named 55-1) and accepts it" "$p" pass
  V_REQUEST=55-2 v_case "a name derived from the later attempt (55-2) is refused: the old derivation could never match" "$p" refuse
  # The seal log's fetch can fail now and then: retried, then reported as a fetch failure.
  V_LOGS_FAIL=2 v_case "a seal log that fails to fetch twice is fetched again and read" "$p" pass
  V_LOGS_FAIL=9 v_case "a seal log that never fetches is refused as a fetch failure" "$p" refuse
  grep -q "could not fetch the seal job's log" "$work/v.log" && ok "verify($p): a log fetch failure is reported as one, not as a missing digest" ||
    fail "verify($p): a log fetch failure is not reported as one: $(tail -3 "$work/v.log")"
done
# The log is fetched with curl, not gh (the runner's gh refuses a response
# holding terminal escape sequences, and a job log is full of them): the
# stub gh refuses the logs endpoint outright, so every passing case above
# already proves the verify job does not need gh for it. These pin the rest.
for p in api-lifecycle registry; do
  v_case "a log full of ANSI escape sequences is read, with gh refusing to print it" "$p" pass
  grep -q 'logs' "$work/v.gh" && fail "verify($p): gh was asked for the job log" || ok "verify($p): gh is never asked for the job log"
  [ "$(wc -l <"$work/v.curl" | tr -d ' ')" = 1 ] && ok "verify($p): the log is fetched once when the first fetch works" || fail "verify($p): curl calls: $(cat "$work/v.curl")"
  c=$(cat "$work/v.curl")
  case " $c " in *" -L "*|*" -fsSL "*) ok "verify($p): curl follows the redirect to the blob store" ;; *) fail "verify($p): curl does not follow redirects: $c" ;; esac
  case "$c" in *location-trusted*) fail "verify($p): curl forwards the token across hosts (--location-trusted)" ;; *) ok "verify($p): curl does not forward the token across hosts" ;; esac
  case "$c" in *"-f"*) ok "verify($p): curl fails on an HTTP error status" ;; *) fail "verify($p): curl does not fail on HTTP errors: $c" ;; esac
  V_LOGS_FAIL=3 v_case "a seal log that fails to fetch three times is refused as a fetch failure" "$p" refuse
  [ "$(wc -l <"$work/v.curl" | tr -d ' ')" = 3 ] && ok "verify($p): three log fetches are attempted, no more" || fail "verify($p): curl calls: $(cat "$work/v.curl")"
  grep -q "could not fetch the seal job's log" "$work/v.log" && ok "verify($p): three failed fetches are reported as a fetch failure" || fail "verify($p): not reported as a fetch failure: $(tail -3 "$work/v.log")"
done
# The log cases edit logs.txt (not JSON), so they are written out.
l_case() { # label pipeline want <log lines...>
  local label=$1 p=$2 want=$3
  shift 3
  v_reset "$p"
  printf '%s\n' "$@" >"$STUB/logs.txt"
  vstep "$p"
  : >"$work/v.out"; V_RC=0
  PATH="$work/bin:$PATH" GH_LOG="$work/v.gh" CURL_LOG="$work/v.curl" SLEEP_LOG=/dev/null GITHUB_OUTPUT="$work/v.out" GH_REPO=o/r SHA=$sha PIPELINE=$p REQUEST=${V_REQUEST:-55-1} \
    BUILD_BRANCH=$branch RUN_ID=777 GH_TOKEN=stub bash --noprofile --norc -e "$work/verify-$p.sh" >"$work/v.log" 2>&1 || V_RC=$?
  case $want in
    pass) [ "$V_RC" = 0 ] && [ "$(cat "$work/v.out")" = "sha256=$good_digest" ] ;;
    refuse) [ "$V_RC" != 0 ] && [ ! -s "$work/v.out" ] ;;
  esac && ok "verify($p): $label" || fail "verify($p): $label: rc $V_RC, output '$(cat "$work/v.out")': $(tail -4 "$work/v.log")"
}
ts=2026-10-12T06:20:00.2000000Z
other_digest=$(printf 'd%.0s' $(seq 64))
for p in api-lifecycle registry; do
  o=$(other_of "$p")
  l_case "a log with no digest line is refused" "$p" refuse "$ts nothing here"
  l_case "two different digest lines are refused" "$p" refuse "$ts kb-refresh-patch-sha256 $p $good_digest" "$ts kb-refresh-patch-sha256 $p $other_digest"
  l_case "a digest line for the other pipeline is refused" "$p" refuse "$ts kb-refresh-patch-sha256 $o $good_digest"
  l_case "a digest of the wrong length is refused" "$p" refuse "$ts kb-refresh-patch-sha256 $p ${good_digest:1}"
  l_case "a digest line with no timestamp (typed into a script's output by hand) is refused" "$p" refuse "kb-refresh-patch-sha256 $p $good_digest"
  l_case "a digest line with Windows line endings is read" "$p" pass "$ts kb-refresh-patch-sha256 $p $good_digest"$'\r'
done

# -- the whole path: the seal job's output, as a log line, is what the verify
# job reads, and the digest check passes for that patch and fails for another.
patch_for() { printf 'diff --git a/%s b/%s\n+one\n' "$1" "$1"; }
for p in api-lifecycle registry; do
  d=$work/e2e-$p; mkdir -p "$d/art"
  patch_for "$p" >"$d/art/kb-refresh-$p.patch"
  seal_case "$p" "$d/art"
  v_reset "$p"
  { echo "2026-10-12T06:20:00.1Z ##[group]Run echo"; echo "2026-10-12T06:20:00.2Z $(cat "$work/seal.out")"; } >"$STUB/logs.txt"
  vstep "$p"
  : >"$work/v.out"; V_RC=0
  PATH="$work/bin:$PATH" GH_LOG="$work/v.gh" CURL_LOG="$work/v.curl" SLEEP_LOG=/dev/null GITHUB_OUTPUT="$work/v.out" GH_REPO=o/r SHA=$sha PIPELINE=$p REQUEST=${V_REQUEST:-55-1} \
    BUILD_BRANCH=$branch RUN_ID=777 GH_TOKEN=stub bash --noprofile --norc -e "$work/verify-$p.sh" >"$work/v.log" 2>&1 || V_RC=$?
  digest=$(sed -n 's/^sha256=//p' "$work/v.out")
  step 'Check the patch against the digest' < <(job "$entry" "$p-verify") | run_block >"$work/cmp-$p.sh"
  [ -s "$work/cmp-$p.sh" ] || fail "$p-verify has no 'Check the patch against the digest' run block"
  cmp_case() { # label dir digest want
    local rc=0
    DIR=$2 DIGEST=$3 PIPELINE=$p bash --noprofile --norc -e "$work/cmp-$p.sh" >"$work/cmp.log" 2>&1 || rc=$?
    case $4 in pass) [ "$rc" = 0 ] ;; refuse) [ "$rc" != 0 ] && grep -q '::error::' "$work/cmp.log" ;; esac &&
      ok "digest($p): $1" || fail "digest($p): $1: rc $rc: $(cat "$work/cmp.log")"
  }
  if [ "$V_RC" = 0 ] && [ -n "$digest" ]; then
    ok "e2e($p): the verify job reads the digest the seal job printed"
    cmp_case "the patch the seal job hashed is accepted" "$d/art" "$digest" pass
    cp -R "$d/art" "$d/tampered"; echo '+evil' >>"$d/tampered/kb-refresh-$p.patch"
    cmp_case "a patch changed after it was sealed is refused" "$d/tampered" "$digest" refuse
    cp -R "$d/art" "$d/other"; patch_for other >"$d/other/kb-refresh-$p.patch"
    cmp_case "another patch is refused" "$d/other" "$digest" refuse
    cp -R "$d/art" "$d/extra"; touch "$d/extra/second"
    cmp_case "an artifact with a second file is refused" "$d/extra" "$digest" refuse
    mkdir -p "$d/empty"
    cmp_case "an artifact with no patch is refused" "$d/empty" "$digest" refuse
    cmp_case "a patch with an empty digest is refused" "$d/art" "" refuse
  else
    fail "e2e($p): the verify job did not read the seal job's output: rc $V_RC: $(cat "$work/seal.out") $(tail -3 "$work/v.log")"
  fi
done

pass=$(grep -c '^ok' "$work/results" || true)
failed=$(grep -c '^FAIL' "$work/results" || true)
echo "kb-refresh-scope_test: $pass passed, $failed failed"
[ "$failed" -eq 0 ] && [ "$pass" -gt 0 ]
