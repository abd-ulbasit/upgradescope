#!/usr/bin/env bash
# Tests for script/update.sh, offline: gh, curl and cosign are stubs, the
# checksums come from script/testdata, and the formula is rendered for real
# in a scratch copy of the tap. It checks that update.sh
#   - renders a newer release, and verifies it with the exact tag identity
#     and the commit the tag names (--certificate-github-workflow-sha);
#   - refuses a release older than the formula's version (GitHub's Latest
#     moved back, or an older tag dispatched), unless
#     UPGRADESCOPE_ALLOW_DOWNGRADE=1, and re-renders the same version;
#   - refuses a tag whose commit is not on main, before downloading or
#     verifying anything, and a tag it cannot resolve;
#   - leaves the formula unchanged whenever it refuses.
# In the upgradescope repository, `make hack-test` runs it.
set -euo pipefail
cd "$(dirname "$0")/.."

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
: >"$work/results"
ok() { echo "ok   $1" | tee -a "$work/results"; }
fail() {
  echo "FAIL $1" >&2
  [ -z "${2:-}" ] || sed 's/^/     /' "$2" >&2
  echo "FAIL $1" >>"$work/results"
}

mkdir -p "$work/bin"
# gh api: releases/latest -> $STUB_LATEST; commits/<tag> -> $STUB_COMMIT;
# compare/<a>...main -> $STUB_COMPARE. Every call is logged.
cat >"$work/bin/gh" <<'EOF'
#!/usr/bin/env bash
echo "gh $*" >>"$CALLS"
[ "$1" = api ] || exit 2
case "$2" in
  repos/o/upgradescope/releases/latest) echo "$STUB_LATEST" ;;
  repos/o/upgradescope/commits/*) [ -n "${STUB_COMMIT:-}" ] || { echo 'gh: Not Found (HTTP 404)' >&2; exit 1; }; echo "$STUB_COMMIT" ;;
  repos/o/upgradescope/compare/*...main) echo "$STUB_COMPARE" ;;
  *) echo "stub gh: unexpected $*" >&2; exit 2 ;;
esac
EOF
# curl -o <file> <url>: checksums.txt from the fixture, any bundle as {}.
cat >"$work/bin/curl" <<'EOF'
#!/usr/bin/env bash
echo "curl $*" >>"$CALLS"
out= url=
while [ $# -gt 0 ]; do
  case $1 in -o) out=$2; shift ;; -*) ;; *) url=$1 ;; esac
  shift
done
case $url in
  */checksums.txt) cp "$FIXTURE" "$out" ;;
  */checksums.txt.sigstore.json) echo '{}' >"$out" ;;
  *) exit 22 ;;
esac
EOF
cat >"$work/bin/cosign" <<'EOF'
#!/usr/bin/env bash
echo "cosign $*" >>"$CALLS"
exit "${STUB_COSIGN_RC:-0}"
EOF
chmod +x "$work/bin/"*

sha=0123456789abcdef0123456789abcdef01234567
fixture="$PWD/script/testdata/checksums-v0.1.1.txt"
# tap [version]: a scratch tap, with a formula rendered for <version>.
tap() {
  rm -rf "$work/tap"
  mkdir -p "$work/tap"
  cp -R formula.rb.tmpl script "$work/tap/"
  if [ -n "${1:-}" ]; then
    (cd "$work/tap" && script/render-formula.sh "$1" "$fixture" 2>/dev/null)
  fi
}
formula_version() { sed -n 's#.*/releases/download/v\([^/]*\)/.*#\1#p' "$work/tap/Formula/upgradescope.rb" | head -1; }
# run [env...]: update.sh in the scratch tap.
run() {
  : >"$work/calls"
  code=0
  (cd "$work/tap" && env PATH="$work/bin:$PATH" CALLS="$work/calls" FIXTURE="$fixture" \
    UPGRADESCOPE_REPO=o/upgradescope STUB_LATEST=v0.3.1 STUB_COMMIT=$sha STUB_COMPARE=ahead "$@" \
    bash script/update.sh) >"$work/out" 2>&1 || code=$?
}

tap 0.3.0
run
if [ "$code" = 0 ] && [ "$(formula_version)" = 0.3.1 ]; then ok "a newer latest release is rendered"; else fail "v0.3.1 over v0.3.0 was not rendered (exit $code)" "$work/out"; fi
grep -qF -- "--certificate-identity https://github.com/o/upgradescope/.github/workflows/release.yml@refs/tags/v0.3.1" "$work/calls" &&
  ok "the bundle is verified against the exact tag's identity" || fail "cosign was not given the tag's identity" "$work/calls"
grep -qF -- "--certificate-github-workflow-sha $sha" "$work/calls" &&
  ok "the bundle is verified against the commit the tag names" || fail "cosign was not given --certificate-github-workflow-sha" "$work/calls"

tap 0.3.0
run STUB_COMPARE=identical
[ "$code" = 0 ] && [ "$(formula_version)" = 0.3.1 ] && ok "a tag at main's head is on main" || fail "compare 'identical' was refused" "$work/out"

tap 0.3.1
run
[ "$code" = 0 ] && [ "$(formula_version)" = 0.3.1 ] && ok "the same version re-renders (no downgrade)" || fail "re-rendering v0.3.1 failed" "$work/out"

tap 0.10.0
run STUB_LATEST=v0.9.9
if [ "$code" != 0 ] && grep -qF "v0.9.9 is older than the formula's v0.10.0; refusing to downgrade" "$work/out" && [ "$(formula_version)" = 0.10.0 ]; then
  ok "an older latest release is refused, versions compared as numbers (v0.9.9 < v0.10.0)"
else
  fail "v0.9.9 over v0.10.0 was not refused" "$work/out"
fi
! grep -qE '^(curl|cosign) ' "$work/calls" && ok "a refused downgrade downloads and verifies nothing" || fail "a refused downgrade still downloaded or verified" "$work/calls"

tap 0.3.1
run UPGRADESCOPE_TAG=v0.3.0
[ "$code" != 0 ] && [ "$(formula_version)" = 0.3.1 ] && grep -q 'refusing to downgrade' "$work/out" &&
  ok "a dispatched older tag is refused too" || fail "a dispatched v0.3.0 over v0.3.1 was not refused" "$work/out"
tap 0.3.1
run UPGRADESCOPE_TAG=v0.3.0 UPGRADESCOPE_ALLOW_DOWNGRADE=1
[ "$code" = 0 ] && [ "$(formula_version)" = 0.3.0 ] && grep -q 'downgrading the formula from v0.3.1 to v0.3.0 on request' "$work/out" &&
  ok "UPGRADESCOPE_ALLOW_DOWNGRADE=1 rolls back on purpose, and says so" || fail "an allowed downgrade did not render v0.3.0" "$work/out"
tap 0.3.1
run UPGRADESCOPE_TAG=v0.3.0 UPGRADESCOPE_ALLOW_DOWNGRADE=true
[ "$code" != 0 ] && [ "$(formula_version)" = 0.3.1 ] && ok "only the value 1 allows a downgrade" || fail "UPGRADESCOPE_ALLOW_DOWNGRADE=true allowed a downgrade" "$work/out"

for st in diverged behind; do
  tap 0.3.0
  run STUB_COMPARE=$st
  if [ "$code" != 0 ] && grep -qF "v0.3.1 ($sha) is not on o/upgradescope's main (compare: $st)" "$work/out" && [ "$(formula_version)" = 0.3.0 ] &&
    ! grep -qE '^(curl|cosign) ' "$work/calls"; then
    ok "a tag whose commit is not on main ($st) is refused before anything is downloaded"
  else
    fail "a tag $st from main was not refused" "$work/out"
  fi
done
tap 0.3.0
run STUB_COMMIT=
[ "$code" != 0 ] && grep -q 'cannot resolve v0.3.1 to a commit' "$work/out" && [ "$(formula_version)" = 0.3.0 ] &&
  ok "a tag that does not resolve is refused" || fail "an unresolvable tag was not refused" "$work/out"

tap 0.3.0
run STUB_COSIGN_RC=1
[ "$code" != 0 ] && grep -q 'does not verify' "$work/out" && [ "$(formula_version)" = 0.3.0 ] &&
  ok "a bundle that does not verify leaves the formula unchanged" || fail "a failed verification changed the formula" "$work/out"

tap
run
[ "$code" = 0 ] && [ "$(formula_version)" = 0.3.1 ] && ok "the first render, with no formula yet, has nothing to downgrade from" ||
  fail "the first render failed" "$work/out"

tap 0.3.0
run STUB_LATEST=v0.3
[ "$code" != 0 ] && grep -q "is not vX.Y.Z" "$work/out" && ok "a tag that is not vX.Y.Z is refused" || fail "tag v0.3 was not refused" "$work/out"

pass=$(grep -c '^ok' "$work/results" || true)
nfail=$(grep -c '^FAIL' "$work/results" || true)
echo "update_test: $pass passed, $nfail failed"
[ "$nfail" -eq 0 ] && [ "$pass" -gt 0 ]
