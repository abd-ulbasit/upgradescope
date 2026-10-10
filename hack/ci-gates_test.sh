#!/usr/bin/env bash
# Tests for the path gates of ci.yml's pull-request jobs (make hack-test),
# offline. A pull request runs a Go-heavy job only for a change it can see,
# and ci-ok must never pass with a job skipped that the change needed. So:
#
#   1. the go and cross path filters are held to what is in the tree: every
#      tracked file is matched by go, or is on the list below of files no
#      gated job reads; and every hack/ script a gated job reaches (through
#      the Makefile and the scripts' own references) is matched by go (or
#      cross, for the cross-build job). The unit tests are not gated at all:
#      a pull request that changes no Go code still runs the packages in
#      hack/test-readers.txt, because some read every file of the checkout;
#   2. hack/ci-changes.sh turns the filters into the outputs correctly;
#   3. every gated job's `if`, evaluated for each kind of run and each
#      combination of filter results, runs exactly the jobs the scenario
#      needs, and hack/ci-ok.sh then passes what ci.yml would produce;
#   4. the cross-build matrix adds up to every platform hack/cross-build.sh
#      compiles.
#
# CI_GATES_VERBOSE=1 lists the hack/ files the gated jobs reach.
#
# Needs node (the job conditions use only ==, !=, &&, || and !, which
# JavaScript evaluates the same way) and jq.
set -euo pipefail
cd "$(dirname "$0")/.."
export LC_ALL=C

command -v node >/dev/null || { echo "ci-gates_test: node is required" >&2; exit 1; }
command -v jq >/dev/null || { echo "ci-gates_test: jq is required" >&2; exit 1; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
pass=0 fail=0
ok() { echo "ok   $1"; pass=$((pass + 1)); }
bad() { echo "FAIL $1" >&2; fail=$((fail + 1)); }

ci=.github/workflows/ci.yml

# ---- the filters ----------------------------------------------------------
# filter_patterns <name>: the globs of a filter of the `paths` step.
filter_patterns() {
  awk -v want="$1" '
    /^      - id: paths$/ { inpaths = 1; next }
    inpaths && /^      - / { inpaths = 0 }
    inpaths && /^            [a-z]+:[ ]*(#.*)?$/ { name = $1; sub(/:$/, "", name); next }
    inpaths && name == want && /^              - / {
      l = $0; sub(/^              - /, "", l); sub(/^'"'"'/, "", l); sub(/'"'"'.*$/, "", l); print l
    }' "$ci"
}
# glob2re <glob>: an extended regular expression for a paths-filter glob
# (** crosses directories, * does not; a dot is a dot).
glob2re() {
  printf '%s' "$1" | sed -e 's/[.+^$(){}|]/\\&/g' -e 's#\*\*/#@@D@@#g' -e 's#\*\*#@@S@@#g' -e 's#\*#[^/]*#g' -e 's#@@D@@#(.*/)?#g' -e 's#@@S@@#.*#g'
}
# alt_re <glob>...: ^(a|b|...)$
alt_re() {
  local re="" p
  for p in "$@"; do
    [ -z "$re" ] && re=$(glob2re "$p") || re="$re|$(glob2re "$p")"
  done
  printf '^(%s)$' "$re"
}
# filter_re <name>: the filter as one regular expression. (The globs are read
# line by line, never word-split: * would expand against the directory.)
filter_re() {
  local pats=() p
  while read -r p; do
    [ -z "$p" ] || pats+=("$p")
  done < <(filter_patterns "$1")
  [ "${#pats[@]}" -gt 3 ] || { echo "FAIL the $1 filter is missing from the paths step of $ci" >&2; exit 1; }
  alt_re "${pats[@]}"
}
go_re=$(filter_re go)
cross_re=$(filter_re cross)
in_go() { grep -qE "$go_re" <<<"$1"; }
in_cross() { grep -qE "$cross_re" <<<"$1"; }

# Examples of each, so the glob translation above is itself held to account.
for f in internal/cli/scan.go go.mod go.sum tools/gen-kb/go.sum tools/promrule-test/go.mod internal/kb/data/lifecycle.yaml \
  internal/server/webdist/index.html web/src/App.tsx deploy/chart/values.yaml deploy/chart/templates/x.yaml Dockerfile Dockerfile.release \
  Makefile hack/test-heap.sh hack/test.sh hack/shard.sh hack/test-heap-durations.txt hack/e2e/kind-config.yaml hack/renovate/package-lock.json \
  .github/workflows/ci.yml .github/actions/dockerhub-mirror/action.yml registry/data/ingress-nginx.yaml api/openapi.yaml \
  examples/policies/kyverno/x.yaml internal/server/testdata/a.json ci/gitlab/upgradescope.gitlab-ci.yml .goreleaser.yml; do
  in_go "$f" && ok "go matches $f" || bad "go does not match $f"
done
for f in docs/getting-started/ci-gate.md README.md CHANGELOG.md .github/workflows/release.yml .github/workflows/kb-refresh.yml \
  hack/check-changelog.sh hack/release-check.sh hack/ci-ok_test.sh hack/docs-live-check.sh registry/CONTRIBUTING.md .github/CODEOWNERS; do
  if in_go "$f"; then bad "go matches $f, which no gated job reads"; else ok "go does not match $f"; fi
done
# There is no docs filter: a change to no Go code runs the readers' tests, not
# nothing, so which files are documentation decides nothing (and a docs filter
# was where a file read by a test that walks the tree could fall between).
if grep -qE '^            docs:' "$ci"; then bad "$ci has a docs filter again: nothing may decide that a pull request runs no unit test"; else ok "there is no docs path filter that could skip the unit tests"; fi
for f in cmd/upgradescope/main.go internal/cli/scan.go deploy/chart/rbac_test.go tools/gen-kb/main.go go.mod go.sum tools/gen-kb/go.mod .goreleaser.yml Makefile hack/cross-build.sh .github/workflows/ci.yml; do
  in_cross "$f" && ok "cross matches $f" || bad "cross does not match $f"
done
for f in docs/x.md README.md web/src/App.tsx deploy/chart/values.yaml hack/test.sh Dockerfile registry/data/x.yaml; do
  if in_cross "$f"; then bad "cross matches $f, which cannot break a compile"; else ok "cross does not match $f"; fi
done
# Everything that can break a compile also runs the Go jobs.
tracked=$(git ls-files --cached --others --exclude-standard)
only_cross=$(grep -E "$cross_re" <<<"$tracked" | grep -vE "$go_re" || true)
[ -z "$only_cross" ] && ok "every file the cross filter matches is also matched by go" || bad "the cross filter matches files go does not: $(echo $only_cross | cut -c1-200)"

# ---- 1. every tracked file is judged --------------------------------------
# Files no Go-heavy job (test-heap, build, cross-build, examples, images, vuln,
# pg-conformance, the kind e2e, envtest) reads, each with why. A file that is
# in the go filter or not here fails the test: a new directory or workflow must
# be decided on, by someone reading what the gated jobs read, not left to fall
# through.
#
# What this list does NOT say is that no Go test reads these files. The unit
# tests (the test job) are not gated: a change to any of them runs at least
# hack/test-readers.txt, which holds every package that reads files outside Go
# code, the two that walk the whole checkout included (internal/server decodes
# every YAML file under the root; internal/crd/apigroup reads every tracked
# file). hack/test_test.sh holds that list to the tree with a detector for
# tests that walk it. (The earlier evidence for this list, the Go tests run on
# a tree with the files deleted, proved nothing for those two, which skip a
# file that is missing; the rule is the list, not the deletion.)
inert=(
  # documentation and the site: no gated job reads a page (the e2e's one page
  # has its own filter, e2e_doc, and release-check its own, release); the
  # readers' tests do, and run on every pull request
  'docs/**'
  '**/*.md'
  'hack/docs/**'
  'mkdocs.yml'
  # the repository's own metadata and the workflows the gated jobs do not run
  '.github/CODEOWNERS'
  '.github/ISSUE_TEMPLATE/*'
  '.github/dependabot.yml'
  '.github/rulesets/*'
  '.github/workflows/docs.yml'
  '.github/workflows/kb-refresh-build.yml'
  '.github/workflows/kb-refresh.yml'
  '.github/workflows/pr-lint.yml'
  '.github/workflows/release.yml'
  '.github/workflows/scorecard.yml'
  '.github/workflows/vuln-latest-release.yml'
  '.gitignore'
  # packaging that is not built, tested or imaged by the gated jobs: the
  # Homebrew tap and the Artifact Hub listing (their scripts are hack-test)
  'packaging/.gitignore'
  'packaging/artifacthub/*'
  'packaging/homebrew-tap/**'
  # hack/ scripts and their tests that no gated job reaches (hack-checks runs
  # every *_test.sh on every pull request): release, docs, KB-refresh,
  # changelog, notices, chart and bench tooling, and the CI scripts' own tests
  'hack/*_test.sh'
  'hack/action_test.sh'
  'hack/bench/*.sh'
  'hack/chart-release-annotations.sh'
  'hack/check-*.sh'
  'hack/claims-check.sh'
  'hack/docs-live-check.sh'
  'hack/flags-diff.sh'
  'hack/helm-docs.sh'
  'hack/helm-test.sh'
  'hack/kb-derived.sh'
  'hack/notices.sh'
  'hack/release-*.sh'
  'hack/test-chart.sh'
  'hack/test-release-repro.sh'
  'hack/vuln-latest-release.sh'
  'hack/web-test.sh'
  'hack/ci-ok.sh'
  'hack/ci-changes.sh'
)
inert_re=$(alt_re "${inert[@]}")
# * in an inert pattern crosses nothing it should not: it is one path segment,
# the way the filters read it, except that these patterns are written as globs
# of one directory.
unjudged=$(grep -vE "$go_re" <<<"$tracked" | grep -vE "$inert_re" || true)
if [ -n "$unjudged" ]; then
  bad "files in neither the go filter nor the list of files no gated job reads (decide: add to a filter in $ci, or to this test's list with the reason): $(echo $unjudged | cut -c1-400)"
else
  ok "every tracked file is matched by go, or is a file no gated job reads"
fi
# The list is true: every pattern on it names something.
for pat in "${inert[@]}"; do
  if grep -qE "$(alt_re "$pat")" <<<"$tracked"; then :; else bad "the inert pattern '$pat' matches no tracked file (stale)"; fi
done
ok "every pattern on the inert list names tracked files"

# ---- 1b. the hack/ scripts the gated jobs reach are in the filter ----------
# job_block <job>: the job's lines in ci.yml.
job_block() { awk -v j="$1" '$0 == "  " j ":" { c = 1; next } c && /^  [a-z0-9_-]+:[ ]*$/ { c = 0 } c' "$ci"; }
# make_target_files <target>: every hack/ path in the recipe of a Makefile
# target and of its prerequisites.
make_target_files() {
  awk -v want="$1" '
    function visit(t,   i, n, deps, d) {
      if (t in seen) return
      seen[t] = 1
      print recipe[t]
      n = split(prereq[t], deps, " ")
      for (i = 1; i <= n; i++) visit(deps[i])
    }
    /^[A-Za-z0-9_.-]+:/ && !/^\.PHONY/ && !/:=/ && !/\?=/ {
      cur = $1; sub(/:.*/, "", cur)
      p = $0; sub(/^[^:]*:[ ]*/, "", p); prereq[cur] = p
      next
    }
    /^\t/ && cur != "" { recipe[cur] = recipe[cur] "\n" $0; next }
    /^[^\t#]/ { }
    END { visit(want) }' Makefile | grep -oE 'hack/[A-Za-z0-9_./-]+' | sed 's#/$##' | sort -u
}
all_hack=$(grep '^hack/' <<<"$tracked")
# reach <job>...: the tracked hack/ files the jobs reach: those their run
# lines and the Makefile recipes they call name, and those the reached scripts
# name in turn (a file by its name, a directory as hack/<dir> or, for the
# ones scripts keep beside themselves, <dir>/). An over-approximation: a name
# in a comment counts, which only widens what the go filter must match.
hack_top=$(grep -E '^hack/[^/]+$' <<<"$all_hack" | sed 's#^hack/##')
hack_dirs=$(grep -E '^hack/[^/]+/' <<<"$all_hack" | cut -d/ -f2 | sort -u)
reach() {
  local job text targets t f todo seen toks b d code
  local files=""
  targets=""
  for job in "$@"; do
    text=$(job_block "$job" | grep -vE '^[[:space:]]*#')
    targets="$targets $(grep -oE 'make [a-z0-9-]+( [a-z0-9-]+)*' <<<"$text" | sed 's/^make //' | tr ' ' '\n' | grep -vE '^[A-Z]' || true)"
    files="$files $(grep -oE 'hack/[A-Za-z0-9_./-]+' <<<"$text" || true)"
  done
  for t in $(tr ' ' '\n' <<<"$targets" | sort -u); do
    [ -n "$t" ] || continue
    files="$files $(make_target_files "$t")"
  done
  todo=$(tr ' ' '\n' <<<"$files" | sed '/^$/d; s#/$##' | sort -u)
  seen=""
  while [ -n "$todo" ]; do
    f=$(head -1 <<<"$todo")
    todo=$(tail -n +2 <<<"$todo")
    grep -qxF "$f" <<<"$seen" && continue
    seen="$seen"$'\n'"$f"
    if [ -d "$f" ]; then
      # a directory: every file in it is reached, and so is what its scripts name
      todo="$todo"$'\n'"$(grep -E "^$f/" <<<"$all_hack")"
      continue
    fi
    [ -f "$f" ] || continue
    # the scripts a *_test.sh runs are tested by it, not read by the job
    case "$f" in *_test.sh) continue ;; *.sh) ;; *) continue ;; esac
    code=$(grep -vE '^[[:space:]]*#' "$f" || true)
    toks=$(grep -oE '[A-Za-z0-9_][A-Za-z0-9_.-]*' <<<"$code" | sed 's/[.-]*$//' | sort -u)
    for b in $(grep -Fxf <(echo "$hack_top") <<<"$toks" | grep -v '_test\.sh$' || true); do
      [ "hack/$b" = "$f" ] || todo="$todo"$'\n'"hack/$b"
    done
    for d in $(grep -oE 'hack/[A-Za-z0-9_-]+/?' <<<"$code" | sed 's#^hack/##; s#/$##' | sort -u | grep -Fxf <(echo "$hack_dirs") || true); do
      todo="$todo"$'\n'"hack/$d"
    done
    for d in e2e renovate demo; do
      grep -qE "(^|[^A-Za-z0-9_.-])$d/" <<<"$code" && todo="$todo"$'\n'"hack/$d"
    done
    todo=$(sed '/^$/d' <<<"$todo" | sort -u)
  done
  sed '/^$/d' <<<"$seen" | while read -r f; do
    if [ -d "$f" ]; then grep -E "^$f/" <<<"$all_hack"; elif [ -f "$f" ]; then echo "$f"; fi
  done | sort -u
}
gated_go="test test-heap build examples images vuln pg-conformance kube envtest kube-matrix pg-matrix"
reached=$(reach $gated_go)
[ "$(wc -l <<<"$reached" | tr -d ' ')" -gt 10 ] || { bad "the reachability walk found only $(wc -l <<<"$reached" | tr -d ' ') hack/ files; it is broken"; }
[ -z "${CI_GATES_VERBOSE:-}" ] || { echo "hack/ files the gated jobs reach:"; sed 's/^/  /' <<<"$reached"; }
missing=$(while read -r f; do in_go "$f" || echo "$f"; done <<<"$reached")
if [ -n "$missing" ]; then
  bad "hack/ files the gated jobs reach but the go filter does not match (a change to one would skip the job that reads it): $(echo $missing)"
else
  ok "every hack/ file the gated jobs reach is matched by the go filter ($(wc -l <<<"$reached" | tr -d ' ') files)"
fi
reached_cross=$(reach cross-build)
missing=$(while read -r f; do in_cross "$f" || echo "$f"; done <<<"$reached_cross")
[ -z "$missing" ] && ok "every hack/ file the cross-build job reaches is matched by the cross filter" || bad "cross-build reaches hack/ files the cross filter does not match: $(echo $missing)"
# A gated script that names a docs page reads it, and a docs page is not in
# go: only the one page the e2e runs (the e2e_doc filter) may be named.
docs_named_by() { # <script>: the docs/ paths it names
  grep -vE '^[[:space:]]*#' "$1" | grep -oE '(^|[^A-Za-z0-9_/.-])docs/[A-Za-z0-9_./-]+' | sed 's/^[^d]*//' || true
}
named_docs=$(for f in $reached; do
  if [[ "$f" == *.sh && "$f" != *_test.sh ]]; then docs_named_by "$f"; fi
done | sort -u | grep -v '^docs/getting-started/ci-gate.md$' || true)
[ -z "$named_docs" ] && ok "no gated script names a docs page but the one the e2e_doc filter covers" || bad "gated scripts name docs pages ($(echo $named_docs)): a docs-only change would skip the job that reads them"

# ---- 1c. the decide step cannot hide a failure -----------------------------
# `ci-changes.sh | tee` under the default `bash -e` has no pipefail: tee
# succeeds when the script fails, and the changes job would report success
# with empty outputs. The step must set shell: bash (which adds pipefail).
decide=$(job_block changes | awk '/^      - id: decide/{d=1;next} d&&/^      - /{d=0} d')
grep -qE '^        shell: bash$' <<<"$decide" && grep -qE '^        run: hack/ci-changes\.sh \| tee -a "\$GITHUB_OUTPUT"$' <<<"$decide" &&
  ok "the decide step runs ci-changes.sh under shell: bash (pipefail), so a failure is not hidden by tee" ||
  bad "the changes job's decide step does not run ci-changes.sh with shell: bash"
# ...and the same shell really fails on the script's failure:
if (set -e; bash -e -o pipefail -c 'false | tee /dev/null') 2>/dev/null; then bad "bash -o pipefail did not fail a failing pipeline"; else ok "a failing script piped to tee fails under pipefail"; fi

# ---- 2. hack/ci-changes.sh -------------------------------------------------
changes() { # EVENT GO CROSS E2E_CODE E2E_DOC [CHANGED_FILES [RELEASE [ACTION]]] -> "go cross scope e2e release action" or "fail"
  local out
  out=$(EVENT=$1 GO=$2 CROSS=$3 E2E_CODE=$4 E2E_DOC=$5 CHANGED_FILES=${6-10} RELEASE=${7-false} ACTION=${8-false} hack/ci-changes.sh 2>/dev/null) || { echo fail; return; }
  echo "$(sed -n 's/^go=//p' <<<"$out") $(sed -n 's/^cross=//p' <<<"$out") $(sed -n 's/^test-scope=//p' <<<"$out") $(sed -n 's/^e2e=//p' <<<"$out") $(sed -n 's/^release=//p' <<<"$out") $(sed -n 's/^action=//p' <<<"$out")"
}
expect_changes() { # name want args...
  local name=$1 want=$2
  shift 2
  local got
  got=$(changes "$@")
  if [ "$got" = "$want" ]; then ok "ci-changes: $name"; else bad "ci-changes: $name: want [$want] got [$got]"; fi
}
#                   event  GO    CROSS E2Ec  E2Ed    files release action
expect_changes "PR touching nothing gated: no Go job, the readers' tests still run" "false false readers false false false" pull_request false false false false
expect_changes "PR touching code: the Go jobs and the e2e run, cross does not" "true false all true false false" pull_request true false true false
expect_changes "PR touching a compile input: cross runs too" "true true all true false false" pull_request true true true false
expect_changes "PR whose cross filter matched but go did not: go is forced on" "true true all true false false" pull_request false true false false
expect_changes "PR touching only the e2e's docs page: the e2e runs, no Go job, the readers' tests" "false false readers true false false" pull_request false false false true
expect_changes "PR for release-check and the action: their outputs are passed on" "false false readers false true true" pull_request false false false false 10 true true
expect_changes "push to main, a code change: everything" "true true all true false false" push true true true false
expect_changes "push to main, a docs-only change: all but the e2e" "true true all false false false" push false false false false
expect_changes "push to main, only the e2e page: everything" "true true all true false false" push false false false true
expect_changes "push to main passes release and action on" "true true all false true true" push false false false false 10 true true
expect_changes "any other event: everything" "true true all true true true" workflow_dispatch false false false false
# The paths-filter action lists at most 3000 files: a list that long may have
# lost the file a job reads, so a pull request that big runs everything.
expect_changes "PR of 2999 files believes the filters" "false false readers false false false" pull_request false false false false 2999
expect_changes "PR of 3000 files runs everything" "true true all true true true" pull_request false false false false 3000
expect_changes "PR of 12000 files runs everything" "true true all true true true" pull_request false false false false 12000
expect_changes "a push does not read the file count" "true true all false false false" push false false false false ""
for v in GO CROSS E2E_CODE E2E_DOC RELEASE ACTION; do
  args=(pull_request false false false false 10 false false)
  case $v in GO) args[1]=maybe ;; CROSS) args[2]=1 ;; E2E_CODE) args[3]=TRUE ;; E2E_DOC) args[4]= ;; RELEASE) args[6]=yes ;; ACTION) args[7]= ;; esac
  expect_changes "an unusable $v fails the step" fail "${args[@]}"
done
for n in "" many 12x -1 "1 2"; do
  expect_changes "a pull request whose file count is '$n' fails the step" fail pull_request false false false false "$n"
done

# The case a reviewer found: a pull request that changes ONE file no filter
# lists (a workflow, an issue template, Dependabot's config) must still run the
# tests that read the whole checkout. For each pair of filter answers a tracked
# file can have, the packages the resulting scope runs include them.
walkers=". internal/server
. internal/crd/apigroup"
for pair in "false false" "true false" "true true"; do
  set -- $pair
  scope=$(changes pull_request "$1" "$2" false false | cut -d' ' -f3)
  case $scope in
    all) ran=$(hack/test.sh --list) ;;
    readers) ran=$(hack/test.sh --list --readers) ;;
    *) bad "a PR with go=$1 cross=$2 gets test-scope '$scope': the unit tests would not run"; continue ;;
  esac
  lost=$(comm -23 <(sort <<<"$walkers") <(sort <<<"$ran"))
  [ -z "$lost" ] && ok "PR with go=$1 cross=$2 (scope $scope) runs the packages that read every file of the checkout" || bad "PR with go=$1 cross=$2 (scope $scope) does not run: $(echo $lost)"
done
# Every tracked file has one of those answers, so the three cover the tree; an
# inert file is the first of them.
inert_only=$(grep -vE "$go_re" <<<"$tracked" | grep -E "$inert_re" | head -1)
[ -n "$inert_only" ] && ! in_go "$inert_only" && ok "an inert file ($inert_only) is a go=false change, which runs the readers' scope" || bad "no inert file found to stand for a go=false change"

# ---- 3. every gated job's `if`, for every kind of run ----------------------
table=$(awk '/^GATED=/{c=1;next} c&&/^'"'"'/{c=0} c&&NF' hack/ci-ok.sh)
gated_jobs=$(awk '{print $1}' <<<"$table")
# test is evaluated with them though it is not gated: it must run in every
# scenario a unit test could be needed, which is every one but a schedule's.
if grep -qx test <<<"$gated_jobs"; then bad "test is in ci-ok's GATED table: no change may skip the unit tests"; else ok "test is not among the gated jobs"; fi
eval_jobs="$gated_jobs test"
job_if() { job_block "$1" | awk '/^    if:/ { c = 1; sub(/^    if: *(>-)? */, ""); print; next } c && /^    [a-z-]+:/ { c = 0 } c'; }
ifs_json=$(for j in $eval_jobs; do printf '%s\t%s\n' "$j" "$(job_if "$j" | tr '\n' ' ')"; done | jq -Rn '[inputs | split("\t") | {key: .[0], value: .[1]}] | from_entries')
scope_expr=$(job_block test | sed -n "s/^          SCOPE: \\\${{ \\(.*\\) }}\$/\\1/p")
[ -n "$scope_expr" ] || bad "the test job has no SCOPE expression"

# scenarios: event | changes result | the four filter inputs for ci-changes.sh
# (the file count, release and action never vary here: ci-changes covers them)
scen="$work/scenarios"
: >"$scen"
for go in true false; do for cross in true false; do for e2ec in true false; do for e2ed in true false; do
  for ev in pull_request push; do
    echo "$ev success $go $cross $e2ec $e2ed" >>"$scen"
  done
done; done; done; done
for ev in schedule workflow_dispatch release; do echo "$ev skipped false false false false" >>"$scen"; done
# A changes job that failed (a paths-filter API error): its outputs are empty.
for ev in pull_request push; do echo "$ev failure false false false false" >>"$scen"; done

# Evaluate with node: job runs (true/false) for each scenario.
cat >"$work/eval.js" <<'JS'
const fs = require('fs');
const ifs = JSON.parse(process.env.IFS_JSON);
const scope = process.env.SCOPE_EXPR;
const outs = JSON.parse(fs.readFileSync(process.env.OUTS_JSON, 'utf8'));
function make(expr, ctx) {
  const js = expr
    .replace(/!cancelled\(\)/g, 'true')
    .replace(/needs\.([A-Za-z0-9_-]+)\.outputs\.([A-Za-z0-9_-]+)/g, 'O("$1","$2")')
    .replace(/needs\.([A-Za-z0-9_-]+)\.result/g, 'R("$1")')
    .replace(/github\.event_name/g, 'E');
  if (/\b(inputs|github|format|contains|fromJSON|always)\b/.test(js.replace(/'[^']*'/g, ''))) throw new Error('unsupported in ' + expr);
  return new Function('E', 'R', 'O', 'return (' + js + ');')(
    ctx.event, (j) => (j === 'changes' ? ctx.changes : 'success'), (j, o) => (j === 'changes' ? (ctx.outputs[o] ?? '') : ''));
}
const res = [];
for (const s of outs) {
  const event = s.event === 'release' ? 'push' : s.event; // a release is a tag push through workflow_call
  const ctx = { event, changes: s.changes, outputs: s.outputs };
  const runs = {};
  for (const [job, expr] of Object.entries(ifs)) runs[job] = !!make(expr, ctx);
  res.push({ ...s, runs, scope: make(scope, ctx) });
}
process.stdout.write(JSON.stringify(res));
JS
# the outputs each scenario's changes job would produce
{
  while read -r ev res go cross e2ec e2ed; do
    if [ "$res" = success ]; then
      o=$(EVENT=$ev GO=$go CROSS=$cross E2E_CODE=$e2ec E2E_DOC=$e2ed CHANGED_FILES=10 RELEASE=false ACTION=false hack/ci-changes.sh)
      jq -nc --arg ev "$ev" --arg res "$res" --arg in "$go $cross $e2ec $e2ed" --arg o "$o" \
        '{event: $ev, changes: $res, inputs: $in, outputs: ($o | split("\n") | map(select(length > 0) | split("=") | {key: .[0], value: .[1]}) | from_entries)}'
    else
      jq -nc --arg ev "$ev" --arg res "$res" '{event: $ev, changes: $res, inputs: "", outputs: {}}'
    fi
  done <"$scen"
} | jq -sc '.' >"$work/outs.json"
evaluated=$(IFS_JSON=$ifs_json SCOPE_EXPR=$scope_expr OUTS_JSON=$work/outs.json node "$work/eval.js")
echo "$evaluated" >"$work/evaluated.json"
[ "$(jq length <<<"$evaluated")" -gt 30 ] && ok "evaluated every gated job's if in $(jq length <<<"$evaluated") scenarios" || bad "the evaluation produced too few scenarios"

# What must run, for the scenarios that matter (the gated jobs only).
runs_of() { jq -r --arg ev "$1" --arg in "$2" '[.[] | select(.event == $ev and .inputs == $in)][0].runs | to_entries | map(select(.value) | .key) | sort | join(" ")' <<<"$evaluated"; }
want_runs() { # name want event inputs
  local got
  got=$(runs_of "$3" "$4")
  if [ "$got" = "$2" ]; then ok "runs: $1"; else bad "runs: $1: want [$2] got [$got]"; fi
}
every="build cross-build envtest examples images kube pg-conformance release-check test test-heap vuln"
# (release-check follows the release filter on a pull request and a push,
# whose output these scenarios do not vary; it is asserted for the other runs.)
every_nr="build cross-build envtest examples images kube pg-conformance test test-heap vuln"
# inputs are "GO CROSS E2E_CODE E2E_DOC"
want_runs "PR touching nothing a Go-heavy job reads runs only test (the readers' packages)" "test" pull_request "false false false false"
want_runs "PR touching code runs the Go jobs, the e2e and envtest, not cross-build" "build envtest examples images kube pg-conformance test test-heap vuln" pull_request "true false true false"
want_runs "PR touching a compile input also runs cross-build" "build cross-build envtest examples images kube pg-conformance test test-heap vuln" pull_request "false true false false"
want_runs "PR touching only the e2e's docs page runs the kind e2e and envtest, and test" "envtest kube test" pull_request "false false true true"
want_runs "push to main after code runs everything" "$every_nr" push "true true true false"
want_runs "push to main after docs only runs all but kube and envtest" "build cross-build examples images pg-conformance test test-heap vuln" push "false false false false"
# A failed changes job cannot hide a test: every gated job runs (a push to main
# keeps its test signal; on a pull request ci-ok fails on the changes job anyway).
want_runs "a push to main whose changes job failed still runs every gated job" "$every" push ""
want_runs "a PR whose changes job failed runs every gated job" "$every" pull_request ""
want_runs "a schedule runs vuln, pg-conformance, kube and envtest only" "envtest kube pg-conformance vuln" schedule ""
want_runs "a dispatch runs every gated job" "$every" workflow_dispatch ""
want_runs "a release runs every gated job" "$every" release ""
# The unit tests, on their own: every kind of run but a schedule, whatever the
# filters say (a test that walks the checkout can fail on any file).
t=$(jq -r '[.[] | select(.event != "schedule" and .runs.test == false)] | length' <<<"$evaluated")
[ "$t" = 0 ] && ok "the unit tests run in every scenario but a schedule's" || bad "the unit tests are skipped in $t scenarios that are not a schedule"
t=$(jq -r '[.[] | select(.event == "schedule" and .runs.test)] | length' <<<"$evaluated")
[ "$t" = 0 ] && ok "the unit tests do not run on a schedule" || bad "the unit tests run on a schedule"
# ...with the readers' scope when no Go-heavy job needs the full set.
scope_of() { jq -r --arg ev "$1" --arg in "$2" '[.[] | select(.event == $ev and .inputs == $in)][0].scope' <<<"$evaluated"; }
sc=$(scope_of pull_request "false false false false")
[ "$sc" = readers ] && ok "a PR that changes no Go code gets SCOPE=readers" || bad "a PR that changes no Go code gets SCOPE='$sc', want readers"
sc=$(scope_of pull_request "true false false false")
[ "$sc" = all ] && ok "a code PR's test shards get SCOPE=all" || bad "a code PR's SCOPE is '$sc', want all"
sc=$(scope_of pull_request "false true false false")
[ "$sc" = all ] && ok "a compile-input PR's test shards get SCOPE=all" || bad "a compile-input PR's SCOPE is '$sc', want all"
sc=$(jq -r '[.[] | select(.event == "workflow_dispatch")][0].scope' <<<"$evaluated")
[ "$sc" = all ] && ok "a dispatch's test shards get SCOPE=all" || bad "a dispatch's SCOPE is '$sc', want all"
sc=$(jq -r '[.[] | select(.event == "pull_request" and .changes == "failure")][0].scope' <<<"$evaluated")
[ "$sc" = all ] && ok "a PR whose changes job failed runs the whole unit suite" || bad "a PR whose changes job failed gets SCOPE='$sc', want all"
# On a pull request or a push, release-check follows the release filter, whose
# output these scenarios do not vary (they run with release=false): it is
# asserted above for the other kinds of run, and below with the verdict.

# Nothing a PR's changes need is ever skipped: for every distinct scenario,
# jobs that did not run are skipped, the rest succeed, and ci-ok must accept
# it. (The release and action outputs are what their filters said: false.)
n=0
while read -r line; do
  ev=$(jq -r .event <<<"$line")
  chg=$(jq -r .changes <<<"$line")
  # a failed changes job fails ci-ok by design (checked in ci-ok_test.sh)
  [ "$chg" = failure ] && continue
  outs=$(jq -c '.outputs + {release: "false", action: "false"}' <<<"$line")
  needs=$(jq -c --argjson outs "$outs" --arg chg "$chg" '
    (.runs | to_entries | map({key, value: {result: (if .value then "success" else "skipped" end), outputs: {}}}) | from_entries)
    + {changes: {result: $chg, outputs: $outs}, lint: {result: "success"}, registry: {result: "success"}, action: {result: "skipped"},
       "kube-matrix": {result: "success"}, "pg-matrix": {result: "success"}, "repo-checks": {result: "success"},
       web: {result: "success"}, helm: {result: "success"}, notices: {result: "success"}, "kb-freshness": {result: "success"}}' <<<"$line")
  # release-check's own filter said false on pull requests and pushes
  if [ "$ev" = pull_request ] || [ "$ev" = push ]; then needs=$(jq -c '."release-check".result = "skipped"' <<<"$needs"); fi
  ciev=$ev
  [ "$ev" = release ] && ciev=push
  # a pull request's or push's release-check: not needed per the (false) release output
  if NEEDS=$needs EVENT=$ciev hack/ci-ok.sh >"$work/ciok.out" 2>&1; then
    n=$((n + 1))
  else
    bad "ci-ok rejects what ci.yml produces for $(jq -c '{event, inputs}' <<<"$line"): $(tail -3 "$work/ciok.out" | tr '\n' ' ')"
  fi
done < <(jq -c '.[]' <<<"$evaluated")
ok "ci-ok accepts what ci.yml produces in each of the $n scenarios (none leaves a needed job skipped)"

# ---- 4. the cross-build matrix covers every platform -----------------------
block=$(job_block cross-build)
legs=$(sed -n 's/^ *platforms: *\(.*\)$/\1/p' <<<"$block" | tr ' ' '\n' | sed '/^$/d' | sort)
default=$(sed -n 's/^platforms=\${CROSS_BUILD_PLATFORMS:-\(.*\)}$/\1/p' hack/cross-build.sh | tr ' ' '\n' | sed '/^$/d' | sort)
goreleaser=$(awk '/^ *goos:/{o=1;a=0;next} /^ *goarch:/{a=1;o=0;next} o&&/^ *- /{os[++no]=$2;next} a&&/^ *- /{ar[++na]=$2;next} o||a{o=0;a=0} END{for(i=1;i<=no;i++)for(j=1;j<=na;j++)print os[i] "/" ar[j]}' .goreleaser.yml | sort)
if [ -n "$default" ] && [ "$legs" = "$default" ]; then
  ok "the cross-build matrix legs add up to hack/cross-build.sh's platforms ($(wc -l <<<"$legs" | tr -d ' '))"
else
  bad "the cross-build legs ($(echo $legs)) are not hack/cross-build.sh's default platforms ($(echo $default))"
fi
if [ -n "$goreleaser" ] && [ "$legs" = "$goreleaser" ]; then
  ok "and .goreleaser.yml's goos x goarch"
else
  bad "the cross-build legs are not .goreleaser.yml's goos x goarch ($(echo $goreleaser))"
fi
dup=$(uniq -d <<<"$legs")
[ -z "$dup" ] && ok "no platform is compiled by two legs" || bad "platforms in two legs: $(echo $dup)"
grep -qF 'CROSS_BUILD_PLATFORMS: ${{ matrix.platforms }}' <<<"$block" && grep -qE '^        run: make cross-build$' <<<"$block" &&
  ok "each leg compiles its matrix.platforms" || bad "the cross-build step does not pass matrix.platforms as CROSS_BUILD_PLATFORMS"

# ---- every job is still in ci-ok's needs; the new ones exist ----------------
grep -qE '^  repo-checks:$' "$ci" && ok "the repo-checks job (check-toolchain, hack-test, claims-check) is not gated" || bad "no repo-checks job"
block=$(job_block repo-checks)
if grep -qF 'needs.changes' <<<"$block"; then bad "repo-checks reads the changes job: it must run on every pull request"; else ok "repo-checks does not depend on the changes job"; fi
for step in 'make check-toolchain' 'make hack-test' 'make claims-check'; do
  grep -qF "run: $step" <<<"$block" && ok "repo-checks runs $step" || bad "repo-checks does not run $step"
done

echo "ci-gates_test: $pass passed, $fail failed"
[ "$fail" = 0 ]
