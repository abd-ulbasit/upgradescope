#!/usr/bin/env bash
# Tests for hack/docs-cosign-minimum.sh (make hack-test), and the release
# workflow's cosign legs (#377). Offline; needs bash and awk.
#   - the script passes on the repository's docs, and fails on a fixture whose
#     section shows `cosign verify` without the minimum, whether the minimum
#     sits in another section or is missing;
#   - release.yml's verify job installs cosign v2.6.3 (pinned by SHA) after
#     the default one, runs the documented flags (the same regexp as
#     install.md) for the index, each platform digest and the chart, and
#     verifies the chart with the default cosign too; the signing steps use
#     no cosign-release pin.
set -euo pipefail
cd "$(dirname "$0")/.."

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
: >"$work/results"
ok() { echo "ok   $1" | tee -a "$work/results"; }
fail() { echo "FAIL $1" | tee -a "$work/results" >&2; }
script=hack/docs-cosign-minimum.sh
min='cosign 3.x, or 2.6.3 or later'

if "$script" >"$work/out" 2>&1; then ok "the repository's docs state the minimum"; else fail "the repository's docs: $(cat "$work/out")"; fi

printf '## Image\n\nUse %s:\n\n```sh\ncosign verify ghcr.io/x/y:1\n```\n' "$min" >"$work/good.md"
if "$script" "$work/good.md" >/dev/null 2>&1; then ok "a section with the minimum passes"; else fail "a section with the minimum fails"; fi

printf '## Image\n\nUse cosign 3.x, or 2.6.3\nor later (wrapped).\n\n    cosign verify ghcr.io/x/y:1\n' >"$work/wrap.md"
if "$script" "$work/wrap.md" >/dev/null 2>&1; then ok "a minimum wrapped over two lines counts"; else fail "a wrapped minimum fails"; fi

printf '## Image\n\n```sh\ncosign verify ghcr.io/x/y:1\n```\n' >"$work/bad.md"
if "$script" "$work/bad.md" >"$work/out" 2>&1; then fail "a section without the minimum passes"; else
  grep -q "bad.md:1: the section shows 'cosign verify' without" "$work/out" && ok "a section without the minimum fails, naming the heading line" || fail "no heading line in: $(cat "$work/out")"
fi

printf '## A\n\n%s\n\n## B\n\n```sh\ncosign verify ghcr.io/x/y:1\n```\n' "$min" >"$work/other.md"
if "$script" "$work/other.md" >/dev/null 2>&1; then fail "the minimum in another section passes"; else ok "the minimum in another section does not count"; fi

printf '## Blob\n\n```sh\ncosign verify-blob checksums.txt --bundle b\n```\n' >"$work/blob.md"
if "$script" "$work/blob.md" >/dev/null 2>&1; then fail "a file with only verify-blob passes (nothing to check)"; else ok "a file with no 'cosign verify' fails loudly rather than passing empty"; fi

printf '## Mixed\n\n```sh\ncosign verify-blob checksums.txt\n```\n\n## Image\n\n%s\n\ncosign verify x\n' "$min" >"$work/mixed.md"
if "$script" "$work/mixed.md" >/dev/null 2>&1; then ok "verify-blob needs no minimum"; else fail "verify-blob section failed"; fi

# --- release.yml's verify job ---
wf=.github/workflows/release.yml
vj="$work/verify.yml"
awk '/^  verify:/ { on = 1; print; next } on && /^  [a-z]/ { exit } on { print }' "$wf" >"$vj"
has() { grep -qE -- "$2" "$vj" && ok "$1" || fail "$1 (no match for: $2)"; }
installer='sigstore/cosign-installer@6f9f17788090df1f26f669e9d70d6ae9567deba6 # v4.1.2'
n=$(grep -cF "uses: $installer" "$vj" || true)
[ "$n" = 2 ] && ok "the verify job installs cosign twice (default, then pinned), both by SHA" || fail "verify job has $n cosign-installer steps, want 2"
has "the second install pins cosign v2.6.3" '^          cosign-release: v2\.6\.3$'
[ "$(grep -c 'cosign-release' "$vj")" = 1 ] && ok "only one cosign-release pin in the verify job" || fail "cosign-release count in verify job"
! grep -q 'cosign-release' <(awk '/^  verify:/ { exit } { print }' "$wf") && ok "no cosign-release in the signing jobs" || fail "a signing job pins cosign-release"
has "the leg asserts the version it runs" "\[ \"\\\$got\" = v2\.6\.3 \]"
# The leg's flags are install.md's, character for character.
regexp="$(grep -o "'\^https://github.com/abd-ulbasit/upgradescope/[^']*'" docs/operations/install.md | sort -u)"
[ "$(grep -c . <<<"$regexp")" = 1 ] && ok "install.md has one identity regexp" || fail "install.md identity regexps: $regexp"
grep -qF -- "--certificate-identity-regexp $regexp" "$vj" && ok "the leg uses install.md's identity regexp" || fail "the leg's regexp differs from install.md's"
has "the leg verifies the image index" 'cosign verify "\$IMAGE:\$VERSION" "\$\{flags\[@\]\}"'
has "the leg verifies each platform digest" 'cosign verify "\$IMAGE@\$d" "\$\{flags\[@\]\}"'
has "the leg verifies the chart" 'cosign verify "\$\{CHART_REPO#oci://\}/upgradescope:\$VERSION" "\$\{flags\[@\]\}"'
has "the chart is also verified with the default cosign" 'cosign verify "\$\{CHART_REPO#oci://\}/upgradescope:\$VERSION" --certificate-identity "\$identity"'
# Order: the default chart verify precedes the pinned install.
dl=$(grep -n 'default cosign' "$vj" | head -1 | cut -d: -f1)
pl=$(grep -n 'cosign-release: v2.6.3' "$vj" | cut -d: -f1)
[ -n "$dl" ] && [ -n "$pl" ] && [ "$dl" -lt "$pl" ] && ok "the default-cosign checks run before the pin" || fail "order of default and pinned legs"
! grep -qE 'continue-on-error|\|\| true' <(sed -n "${pl},\$p" "$vj") && ok "the pinned leg cannot be ignored (no continue-on-error, no || true)" || fail "the pinned leg is ignorable"

pass=$(grep -c '^ok' "$work/results" || true)
failed=$(grep -c '^FAIL' "$work/results" || true)
echo "docs-cosign-minimum_test: $pass passed, $failed failed"
[ "$failed" -eq 0 ] && [ "$pass" -gt 0 ]
