#!/usr/bin/env bash
# Tests that only the highest stable release moves GitHub's Latest, the
# image's :latest and the floating major tag (make hack-test, #244): a
# re-run of an older release, or the slower of two racing ones, used to move
# v0 (and, on a re-dispatch, :latest) back to an older engine.
#   - hack/release-newest.sh's decisions, on fixture tag lists;
#   - release.yml's steps that call it, run the way Actions runs a run:
#     step without shell: (`bash -e {0}`: errexit, no pipefail) against a
#     stub gh, docker and oras: preflight's newest outputs, the goreleaser
#     job's re-check and its closing step that points Latest and :latest at
#     the highest release, and major-tag; each with a failing gh too, which
#     must fail the step and decide nothing (a piped `gh release list`
#     failing under bash -e read as an empty list: newest=true);
#   - the wiring: major-tag runs only on preflight's major-newest, under one
#     repository-wide concurrency group; GoReleaser gets the re-check as
#     UPGRADESCOPE_NEWEST;
#   - .goreleaser.yml's make_latest and :latest templates, rendered with Go's
#     text/template and an envOrDefault like GoReleaser's.
# Offline; needs jq, node (for the if: expressions) and Go.
set -euo pipefail
cd "$(dirname "$0")/.."

wf=.github/workflows/release.yml
gr=.goreleaser.yml
newest=$PWD/hack/release-newest.sh
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
: >"$work/results"
ok() { echo "ok   $1" | tee -a "$work/results"; }
fail() {
  echo "FAIL $1" >&2
  [ -z "${2:-}" ] || sed 's/^/     /' "$2" >&2
  echo "FAIL $1" >>"$work/results"
}

# --- hack/release-newest.sh ---------------------------------------------------

# decide <name> <want> <stdin tags (space-separated)> <args...>
decide() {
  local name=$1 want=$2 tags=$3 got rc=0
  shift 3
  got=$(tr ' ' '\n' <<<"$tags" | "$newest" "$@" 2>"$work/err") || rc=$?
  [ "$rc" = 0 ] || got="exit $rc"
  if [ "$got" = "$want" ]; then ok "$name"; else
    echo "got '$got', want '$want'" >>"$work/err"
    fail "$name" "$work/err"
  fi
}
decide "the only release is the newest" true "" is-newest v0.2.0
decide "a re-run of the newest release is still the newest" true "v0.2.0 v0.2.1" is-newest v0.2.1
decide "v0.2.0 with v0.2.1 published is not the newest" false "v0.2.0 v0.2.1" is-newest v0.2.0
decide "v0.2.0 is not the newest while a newer stable release exists, even unpublished here" false "v0.2.1" is-newest v0.2.0
decide "a pre-release is never the newest" false "v0.1.1" is-newest v0.2.0-rc.2
decide "a newer pre-release does not outrank a stable release" true "v0.2.0 v0.3.0-rc.1" is-newest v0.2.0
decide "versions compare numerically (v0.10.0 > v0.9.9)" true "v0.9.9 v0.2.0" is-newest v0.10.0
decide "versions compare numerically (v0.9.9 < v0.10.0)" false "v0.10.0" is-newest v0.9.9
decide "patch numbers compare numerically" false "v1.0.10" is-newest v1.0.9
decide "a non-release tag on stdin is ignored" true "v0 nightly v99 latest v0.1.1" is-newest v0.2.0
decide "v1 outranks every v0 release" false "v1.0.0" is-newest v0.9.1
decide "--major: v0.9.1 is v0's newest even once v1.0.0 exists" true "v1.0.0 v0.9.0" is-newest --major v0.9.1
decide "--major: a higher v0 release still outranks" false "v1.0.0 v0.9.2" is-newest --major v0.9.1
decide "--major: v1 releases do not count for v0, nor v10 for v1" true "v10.0.0 v0.9.9 v1.0.0" is-newest --major v1.2.0
decide "highest is the highest stable tag" v0.10.0 "v0.2.0 v0.10.0 v0.9.0 v0.11.0-rc.1 v0" highest
decide "highest --major v0" v0.9.1 "v0.9.1 v1.0.0 v0.2.0" highest --major v0
decide "highest of nothing fails" "exit 1" "v0 v0.2.0-rc.1" highest
decide "an unknown command fails" "exit 2" "" newest v0.2.0
decide "is-newest needs one tag" "exit 2" "" is-newest

# --- release.yml: stubs and step extraction ----------------------------------

# job <name>: the lines of the top-level job <name> in release.yml.
job() { awk -v want="  $1:" '$0 == want { on = 1; next } on && /^  [a-z0-9_-]+:/ { exit } on { print }' "$wf"; }
# step_run <job> <name>: the `run: |` block of that job's step, dedented.
step_run() {
  job "$1" | awk -v want="- name: $2" '
    { line = $0; sub(/^ +/, "", line) }
    line ~ /^- / { inside = (line == want); inrun = 0; next }
    inside && line == "run: |" { match($0, /^ */); ind = RLENGTH; inrun = 1; next }
    inrun {
      match($0, /^ */)
      if ($0 !~ /^ *$/ && RLENGTH <= ind) { inrun = 0; inside = 0; next }
      print substr($0, ind + 3)
    }
  '
}
# step <job> <name>: the whole step (for its env and id).
step() {
  job "$1" | awk -v want="- name: $2" '
    { line = $0; sub(/^ +/, "", line) }
    line ~ /^- / { inside = (line == want) }
    inside { print }
  '
}

# Every step that calls release-newest.sh runs under Actions' default shell,
# as run_block runs it: a shell: there would make this test's runs differ
# from the workflow's. None pipes `release-newest.sh published` into
# anything: without pipefail, a failed gh would go unnoticed.
calls_newest=$(awk '/^      - name: / { name = $0; sub(/^      - name: /, "", name) } !/^ *#/ && /hack\/release-newest\.sh/ && name != "" { print name }' "$wf" | sort -u)
[ "$(wc -l <<<"$calls_newest")" -ge 4 ] && ok "release.yml's steps that call release-newest.sh are found ($(wc -l <<<"$calls_newest" | tr -d ' '))" ||
  fail "fewer than 4 release.yml steps call hack/release-newest.sh: $calls_newest"
while IFS= read -r name; do
  if awk -v want="      - name: $name" '$0 == want { on = 1; next } on && /^      - / { on = 0 } on && /^  [a-z]/ { on = 0 } on && /^        shell:/ { f = 1 } END { exit f }' "$wf"; then
    ok "'$name' runs under Actions' default shell (bash -e), as this test runs it"
  else
    fail "'$name' sets shell:; run_block runs it under bash -e, so update the test with it"
  fi
done <<<"$calls_newest"
if grep -nE 'release-newest\.sh[^|#]*published[^|#]*\|' "$wf" >"$work/piped"; then
  fail "release.yml pipes release-newest.sh published into a command (no pipefail: a failed gh reads as no releases)" "$work/piped"
else
  ok "release.yml never pipes release-newest.sh published (it writes the list to a file first)"
fi

# A scratch checkout: the real hack/release-newest.sh, a stub install-tool.sh.
mkdir -p "$work/repo/hack" "$work/bin"
cp "$newest" "$work/repo/hack/release-newest.sh"
cat >"$work/repo/hack/install-tool.sh" <<EOF
#!/usr/bin/env bash
[ "\$1" = oras ] || exit 2
echo "$work/bin/oras"
EOF
# gh: release list answers from \$STUB_RELEASES (JSON: tagName, isPrerelease,
# isDraft), dropping drafts only with --exclude-drafts; releases/latest is
# \$STUB_LATEST (404 when empty); every write is logged and succeeds.
cat >"$work/bin/gh" <<'EOF'
#!/usr/bin/env bash
echo "gh $* (token:${GH_TOKEN:+set})" >>"$CALLS"
filter=. drafts=keep args=()
while [ $# -gt 0 ]; do
  case $1 in
    --jq) filter=$2; shift ;;
    --exclude-drafts) drafts=drop ;;
    *) args+=("$1") ;;
  esac
  shift
done
set -- "${args[@]}"
case "$1 $2" in
  "release list")
    [ "${STUB_GH_FAIL:-}" != 1 ] || { echo 'gh: HTTP 502' >&2; exit 1; }
    if [ "$drafts" = drop ]; then jq -c '[.[] | select(.isDraft | not)]' "$STUB_RELEASES"; else cat "$STUB_RELEASES"; fi | jq -r "$filter" ;;
  "api repos/o/r/releases/latest")
    [ -n "${STUB_LATEST:-}" ] || { echo 'gh: Not Found (HTTP 404)' >&2; exit 1; }
    jq -nr --arg t "$STUB_LATEST" '{tag_name: $t}' | jq -r "$filter" ;;
  "api repos/o/r/releases/tags/"*)
    jq -nr --arg t "${2##*/}" '{id: ("id-" + $t)}' | jq -r "$filter" ;;
  "api repos/o/r/git/ref/tags/"*)
    [ -n "${STUB_MAJOR_EXISTS:-}" ] || exit 1 ;;
  "api -X") echo '{}' ;;
  *) echo "stub gh: unexpected: $*" >&2; exit 2 ;;
esac
EOF
# docker buildx imagetools inspect <ref> --format ...: {"digest": ...} from
# \$STUB_DIGESTS ("<ref> <digest>" lines); an unknown ref fails.
cat >"$work/bin/docker" <<'EOF'
#!/usr/bin/env bash
echo "docker $*" >>"$CALLS"
[ "$1 $2 $3" = "buildx imagetools inspect" ] || exit 2
d=$(awk -v r="$4" '$1 == r { print $2 }' "$STUB_DIGESTS")
[ -n "$d" ] || { echo "ERROR: $4: not found" >&2; exit 1; }
jq -nc --arg d "$d" '{digest: $d}'
EOF
cat >"$work/bin/oras" <<'EOF'
#!/usr/bin/env bash
echo "oras $*" >>"$CALLS"
EOF
chmod +x "$work/repo/hack/install-tool.sh" "$work/bin/"*

# releases <tag[:pre|:draft]>...: the stub's published releases.
releases() {
  local t pre draft
  for t in "$@"; do
    pre=false draft=false
    case $t in *:pre) pre=true ;; *:draft) draft=true ;; esac
    jq -nc --arg t "${t%%:*}" --argjson p "$pre" --argjson d "$draft" '{tagName: $t, isPrerelease: $p, isDraft: $d}'
  done | jq -cs . >"$work/releases.json"
}
# run_block <script> <tag> [env...]: the block as a step of a run on <tag>,
# under `bash -e` as Actions runs a run: step that sets no shell: errexit
# but no pipefail, so a failure inside a pipe is not the step's failure.
run_block() {
  local script=$1 tag=$2
  shift 2
  : >"$work/calls" && : >"$work/output" && : >"$work/summary"
  code=0
  (cd "$work/repo" && env PATH="$work/bin:$PATH" CALLS="$work/calls" STUB_RELEASES="$work/releases.json" \
    STUB_DIGESTS="$work/digests" GITHUB_REPOSITORY=o/r GITHUB_REF_NAME="$tag" GITHUB_SHA=c0ffee \
    GITHUB_OUTPUT="$work/output" GITHUB_STEP_SUMMARY="$work/summary" RUNNER_TEMP="$work" GH_TOKEN=stub \
    IMAGE=ghcr.io/o/r "$@" bash -e "$script") >"$work/out" 2>&1 || code=$?
}
outputs() { tr '\n' ' ' <"$work/output" | sed 's/ $//'; }
writes() { grep -E '^(gh api -X|oras )' "$work/calls" | sed 's/ (token:set)$//' || true; }

# --- preflight ---------------------------------------------------------------

step_run preflight 'is this the highest stable release?' >"$work/preflight.sh"
[ -s "$work/preflight.sh" ] || { echo "FAIL no 'is this the highest stable release?' step in preflight" >&2; exit 1; }
# want_pre <name> <tag> <want outputs> <releases...>
want_pre() {
  local name=$1 tag=$2 want=$3
  shift 3
  releases "$@"
  run_block "$work/preflight.sh" "$tag"
  if [ "$code" = 0 ] && [ "$(outputs)" = "$want" ]; then ok "preflight: $name"; else
    echo "exit $code, outputs '$(outputs)', want '$want'" >>"$work/out"
    fail "preflight: $name" "$work/out"
  fi
}
want_pre "v0.2.0 with v0.2.1 published (the #244 re-run): newest=false" v0.2.0 \
  "newest=false major-newest=false" v0.1.1 v0.2.0 v0.2.1
want_pre "v0.2.1, the highest: newest=true" v0.2.1 "newest=true major-newest=true" v0.1.1 v0.2.0
want_pre "the first stable release: newest=true" v0.2.0 "newest=true major-newest=true" v0.2.0-rc.2:pre
want_pre "a newer pre-release does not stop a stable one" v0.2.0 "newest=true major-newest=true" v0.3.0-rc.1:pre
want_pre "a newer draft is not published, so it does not count" v0.2.0 "newest=true major-newest=true" v0.2.1:draft
want_pre "a pre-release tag: newest=false" v0.3.0-rc.1 "newest=false major-newest=false" v0.2.0
want_pre "a v0 patch after v1.0.0: not GitHub's Latest, still v0's newest" v0.9.1 \
  "newest=false major-newest=true" v0.9.0 v1.0.0
grep -q -- '--exclude-drafts' "$work/calls" && ok "preflight lists releases without drafts" ||
  fail "preflight's release list does not exclude drafts" "$work/calls"
grep -q '(token:set)' "$work/calls" && ok "preflight lists releases with the job's token" ||
  fail "preflight's gh has no GH_TOKEN" "$work/calls"
releases v0.2.0
run_block "$work/preflight.sh" v0.2.0 STUB_GH_FAIL=1
[ "$code" != 0 ] && ! grep -q '^newest=' "$work/output" && ok "preflight: a failed release listing fails the job, not newest=true" ||
  fail "preflight: a failed release listing did not fail the step" "$work/out"
step preflight 'is this the highest stable release?' | grep -qxF '        id: newest' &&
  job preflight | grep -qxF '      newest: ${{ steps.newest.outputs.newest }} # the highest stable release' &&
  job preflight | grep -qF '      major-newest: ${{ steps.newest.outputs.major-newest }}' &&
  ok "preflight outputs newest and major-newest from the step" ||
  fail "preflight does not output newest and major-newest from steps.newest"

# --- goreleaser: re-check, GoReleaser's env, the closing reconcile -----------

step_run goreleaser 'is this still the highest stable release?' >"$work/recheck.sh"
[ -s "$work/recheck.sh" ] || { echo "FAIL no re-check step in the goreleaser job" >&2; exit 1; }
releases v0.2.0 v0.2.1
run_block "$work/recheck.sh" v0.2.0
[ "$code" = 0 ] && [ "$(outputs)" = newest=false ] && ok "goreleaser: v0.2.0 after v0.2.1 published: UPGRADESCOPE_NEWEST=false" ||
  fail "goreleaser: v0.2.0's re-check is not newest=false" "$work/out"
run_block "$work/recheck.sh" v0.2.1
[ "$code" = 0 ] && [ "$(outputs)" = newest=true ] && ok "goreleaser: the highest release: UPGRADESCOPE_NEWEST=true" ||
  fail "goreleaser: v0.2.1's re-check is not newest=true" "$work/out"
# The #244 false-green on a transient API error: a re-run of v0.2.0 with
# v0.2.1 published, and gh release list failing (HTTP 502, rate limit).
run_block "$work/recheck.sh" v0.2.0 STUB_GH_FAIL=1
[ "$code" != 0 ] && [ ! -s "$work/output" ] &&
  ok "goreleaser: a failed release listing fails the re-check, with no newest output (under bash -e)" ||
  fail "goreleaser: a failed release listing did not fail the re-check (exit $code, outputs '$(outputs)')" "$work/out"
step goreleaser 'is this still the highest stable release?' | grep -qxF '        id: newest' &&
  job goreleaser | grep -qxF '          UPGRADESCOPE_NEWEST: ${{ steps.newest.outputs.newest }}' &&
  [ "$(job goreleaser | grep -n 'id: newest' | cut -d: -f1)" -lt "$(job goreleaser | grep -n 'uses: goreleaser/goreleaser-action@' | cut -d: -f1)" ] &&
  ok "GoReleaser runs after the re-check, with UPGRADESCOPE_NEWEST from it" ||
  fail "GoReleaser does not get UPGRADESCOPE_NEWEST from the re-check step before it"

name="GitHub's Latest and the image's :latest name the highest stable release"
step_run goreleaser "$name" >"$work/reconcile.sh"
[ -s "$work/reconcile.sh" ] || { echo "FAIL no '$name' step in the goreleaser job" >&2; exit 1; }
step goreleaser "$name" | grep -qxF "        if: \${{ !contains(github.ref_name, '-') }}" &&
  [ "$(job goreleaser | grep -n 'uses: goreleaser/goreleaser-action@' | cut -d: -f1)" -lt "$(job goreleaser | grep -nF -- "- name: $name" | cut -d: -f1)" ] &&
  ok "goreleaser: the reconcile step runs after publishing, for stable tags only" ||
  fail "goreleaser: the reconcile step is not after GoReleaser with if: !contains(github.ref_name, '-')"
cat >"$work/digests" <<'EOF'
ghcr.io/o/r:v0.2.0 sha256:aaa
ghcr.io/o/r:v0.2.1 sha256:bbb
EOF
# want_rec <name> <tag> <latest> <:latest digest|-> <want writes> <releases...>
want_rec() {
  local name=$1 tag=$2 latest=$3 cur=$4 want=$5
  shift 5
  releases "$@"
  grep -v ':latest ' "$work/digests" >"$work/d" || true
  [ "$cur" = - ] || echo "ghcr.io/o/r:latest $cur" >>"$work/d"
  mv "$work/d" "$work/digests"
  run_block "$work/reconcile.sh" "$tag" STUB_LATEST="$latest"
  if [ "$code" = 0 ] && [ "$(writes | tr '\n' ';')" = "$want" ]; then ok "reconcile: $name"; else
    { echo "exit $code, writes:"; writes; echo "want: $want"; } >>"$work/out"
    fail "reconcile: $name" "$work/out"
  fi
}
want_rec "v0.2.1 published last and already Latest: nothing to move" v0.2.1 v0.2.1 sha256:bbb "" v0.2.0 v0.2.1
want_rec "a racing v0.2.0 published last and took both: both move back to v0.2.1" v0.2.0 v0.2.0 sha256:aaa \
  "gh api -X PATCH repos/o/r/releases/id-v0.2.1 -f make_latest=true;oras tag ghcr.io/o/r@sha256:bbb latest;" v0.2.0 v0.2.1
want_rec "a re-run of v0.2.0 with v0.2.1 in place: nothing moves" v0.2.0 v0.2.1 sha256:bbb "" v0.2.0 v0.2.1
want_rec "the first release with no :latest yet: :latest is created" v0.2.0 v0.2.0 - \
  "oras tag ghcr.io/o/r@sha256:aaa latest;" v0.2.0
want_rec "no Latest release yet: v0.2.0 is made Latest" v0.2.0 "" sha256:aaa \
  "gh api -X PATCH repos/o/r/releases/id-v0.2.0 -f make_latest=true;" v0.2.0 v0.2.0-rc.2:pre
# A higher release racing this one: published, its image not pushed yet.
# Its own run points :latest at it when it ends; this one leaves :latest.
releases v0.2.0 v0.3.0
run_block "$work/reconcile.sh" v0.2.0 STUB_LATEST=v0.2.0
[ "$code" = 0 ] && [ "$(writes | tr '\n' ';')" = "gh api -X PATCH repos/o/r/releases/id-v0.3.0 -f make_latest=true;" ] &&
  grep -q '^::notice::ghcr.io/o/r:v0.3.0 is not pushed yet' "$work/out" &&
  ok "reconcile: a higher release still publishing its image: Latest moves to it, :latest is left to its run" ||
  fail "reconcile: a higher release without its image yet did not leave :latest to its run" "$work/out"
# This run's own image missing: a broken publish, not a race.
releases v0.2.0 v0.3.0
run_block "$work/reconcile.sh" v0.3.0 STUB_LATEST=v0.3.0
[ "$code" != 0 ] && [ -z "$(writes | grep oras || true)" ] && grep -q '^::error::no digest for ghcr.io/o/r:v0.3.0' "$work/out" &&
  ok "reconcile: this release's own image not found fails the job, moving no :latest" ||
  fail "reconcile: a missing image for this run's own release did not fail the step" "$work/out"
releases v0.2.0 v0.2.1
run_block "$work/reconcile.sh" v0.2.0 STUB_LATEST=v0.2.0 STUB_GH_FAIL=1
[ "$code" != 0 ] && [ -z "$(writes)" ] &&
  ok "reconcile: a failed release listing fails the step and moves nothing (under bash -e)" ||
  fail "reconcile: a failed release listing did not fail the step, or moved something" "$work/out"

# --- major-tag ---------------------------------------------------------------

mt=$(job major-tag)
want_if() { # want_if <label> <expression> <want> <context-json>
  local got
  got=$(node -e '
    const [src, ctxJSON] = process.argv.slice(1);
    const ctx = JSON.parse(ctxJSON);
    const e = src.trim().replace(/^\$\{\{([\s\S]*)\}\}$/, "$1").replace(/\.([A-Za-z_]\w*(?:-[\w]+)+)/g, (_, k) => "[" + JSON.stringify(k) + "]");
    const contains = (a, b) => String(a).toLowerCase().includes(String(b).toLowerCase());
    process.stdout.write(String(new Function("github", "needs", "contains", "return (" + e + ");")(ctx.github, ctx.needs, contains)));
  ' "$2" "$4") || { fail "$1: cannot evaluate '$2'"; return; }
  if [ "$got" = "$3" ]; then ok "$1"; else fail "$1: '$2' is '$got', want '$3' for $4"; fi
}
cond=$(sed -n 's/^    if: //p' <<<"$mt")
ctx() { echo "{\"github\":{\"ref_name\":\"$1\"},\"needs\":{\"preflight\":{\"outputs\":{\"major-newest\":\"$2\"}}}}"; }
want_if "major-tag runs for the highest stable release of its major" "$cond" true "$(ctx v0.2.1 true)"
want_if "major-tag skips a release that is not its major's highest" "$cond" false "$(ctx v0.2.0 false)"
want_if "major-tag skips a pre-release" "$cond" false "$(ctx v0.3.0-rc.1 false)"
grep -qE '^    needs: \[preflight, verify\]$' <<<"$mt" && ok "major-tag needs preflight (for major-newest) and verify" ||
  fail "major-tag does not need [preflight, verify]"
if awk '/^    concurrency:/ { c = 1; next } c && /^      group: release-major-tag$/ { g = 1 } c && /^      cancel-in-progress: false$/ { k = 1 } c && /^    [^ ]/ { c = 0 } END { exit !(g && k) }' <<<"$mt"; then
  ok "major-tag runs one at a time across the repository (group release-major-tag, never cancelled in progress)"
else
  fail "major-tag has no repository-wide concurrency group release-major-tag with cancel-in-progress: false"
fi
grep -A2 'uses: actions/checkout@' <<<"$mt" | grep -qF 'persist-credentials: false' &&
  ok "major-tag's checkout keeps no token" || fail "major-tag's checkout persists its credentials"

step_run major-tag 'point the major tag at this release' >"$work/major.sh"
[ -s "$work/major.sh" ] || { echo "FAIL no 'point the major tag at this release' step" >&2; exit 1; }
# want_major <name> <tag> <want writes> <releases...>; v0 exists.
want_major() {
  local name=$1 tag=$2 want=$3
  shift 3
  releases "$@"
  run_block "$work/major.sh" "$tag" STUB_MAJOR_EXISTS=1
  if [ "$code" = 0 ] && [ "$(writes | tr '\n' ';')" = "$want" ]; then ok "major-tag: $name"; else
    { echo "exit $code, writes:"; writes; echo "want: $want"; } >>"$work/out"
    fail "major-tag: $name" "$work/out"
  fi
}
want_major "the highest release moves v0" v0.2.1 \
  "gh api -X PATCH repos/o/r/git/refs/tags/v0 -f sha=c0ffee -F force=true;" v0.2.0 v0.2.1
want_major "v0.2.0 finding v0.2.1 published leaves v0 (two releases racing)" v0.2.0 "" v0.2.0 v0.2.1
has_notice=$(grep -c '^::notice::v0 stays' "$work/out" || true)
[ "$has_notice" = 1 ] && ok "major-tag: leaving v0 says why" || fail "major-tag: no notice when v0 stays" "$work/out"
want_major "a v0 patch after v1.0.0 still moves v0" v0.9.1 \
  "gh api -X PATCH repos/o/r/git/refs/tags/v0 -f sha=c0ffee -F force=true;" v0.9.0 v0.9.1 v1.0.0
releases v1.0.0
run_block "$work/major.sh" v1.0.0
[ "$code" = 0 ] && [ "$(writes)" = "gh api -X POST repos/o/r/git/refs -f ref=refs/tags/v1 -f sha=c0ffee" ] &&
  ok "major-tag: the first v1 release creates v1" || fail "major-tag: v1.0.0 does not create v1" "$work/out"
# A re-run of v0.2.0 with v0.2.1 published and gh release list failing must
# not move v0 back (the #244 false-green on a transient API error).
releases v0.2.0 v0.2.1
run_block "$work/major.sh" v0.2.0 STUB_MAJOR_EXISTS=1 STUB_GH_FAIL=1
[ "$code" != 0 ] && [ -z "$(writes)" ] && ! grep -q 'PATCH' "$work/calls" &&
  ok "major-tag: a failed release listing fails the job and moves no tag (under bash -e)" ||
  fail "major-tag: a failed release listing did not fail the job, or moved the tag" "$work/out"

# --- .goreleaser.yml templates -------------------------------------------------

tpl_latest=$(sed -n "s/^      - '\(.*latest{{ end }}\)'$/\1/p" "$gr")
tpl_make=$(sed -n "s/^  make_latest: '\(.*\)'$/\1/p" "$gr")
if [ -z "$tpl_latest" ] || [ -z "$tpl_make" ]; then
  fail "cannot find the :latest image tag or release.make_latest template in $gr"
else
  mkdir -p "$work/tpl"
  cat >"$work/tpl/main.go" <<'EOF'
// Renders a GoReleaser template with .Prerelease and envOrDefault (an env
// var, or the default when unset, as GoReleaser's own).
package main

import (
	"fmt"
	"os"
	"text/template"
)

func main() {
	t := template.Must(template.New("t").Funcs(template.FuncMap{
		"envOrDefault": func(name, def string) string {
			if v, ok := os.LookupEnv(name); ok {
				return v
			}
			return def
		},
	}).Option("missingkey=error").Parse(os.Args[1]))
	if err := t.Execute(os.Stdout, map[string]any{"Prerelease": os.Args[2] == "true"}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
EOF
  printf 'module example.com/tpl\n\ngo 1.22\n' >"$work/tpl/go.mod"
  (cd "$work/tpl" && GOPROXY=off GOWORK=off go build -o "$work/render" .)
  # render <name> <template> <prerelease> <UPGRADESCOPE_NEWEST|unset> <want>
  render() {
    local got
    if [ "$4" = unset ]; then got=$(env -u UPGRADESCOPE_NEWEST "$work/render" "$2" "$3"); else got=$(UPGRADESCOPE_NEWEST=$4 "$work/render" "$2" "$3"); fi
    if [ "$got" = "$5" ]; then ok "$1"; else fail "$1: rendered '$got', want '$5'"; fi
  }
  render ":latest image tag for the highest stable release" "$tpl_latest" false true latest
  render "no :latest image tag for an older release's run" "$tpl_latest" false false ""
  render "no :latest image tag when UPGRADESCOPE_NEWEST is unset (a snapshot)" "$tpl_latest" false unset ""
  render "no :latest image tag for a pre-release" "$tpl_latest" true true ""
  render "make_latest is true for the highest stable release" "$tpl_make" false true true
  render "make_latest is an explicit false for an older release" "$tpl_make" false false false
  render "make_latest is an explicit false when UPGRADESCOPE_NEWEST is unset" "$tpl_make" false unset false
  render "make_latest is false for anything but exactly true" "$tpl_make" false yes false
fi

pass=$(grep -c '^ok' "$work/results" || true)
nfail=$(grep -c '^FAIL' "$work/results" || true)
echo "release-newest_test: $pass passed, $nfail failed"
[ "$nfail" -eq 0 ] && [ "$pass" -gt 0 ]
