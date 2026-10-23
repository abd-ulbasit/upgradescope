#!/usr/bin/env bash
# Tests for hack/image-scan.sh (make hack-test), offline: gh, cosign and
# grype are stubs. It checks that the scan
#   - verifies checksums.txt's signature against the release workflow at
#     the tag, and every SBOM against checksums.txt, before grype runs;
#   - runs grype on both platform images (by digest when given one) and on
#     every SBOM, with hack/grype.yaml and --only-fixed;
#   - fails (1) on a high or critical match, passes medium ones, and names
#     each finding; fails closed (2) when grype, the download, the
#     signature or an SBOM's checksum fails.
# Also pins that release.yml's verify job and vuln-latest-release.yml run it.
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

mkdir -p "$work/bin" "$work/rel"
cat >"$work/bin/gh" <<'EOF'
#!/usr/bin/env bash
echo "gh $*" >>"$CALLS"
case "$1 $2" in
  "api repos/o/r/releases/latest") echo v9.9.9 ;;
  "release download")
    [ "${STUB_GH_FAIL:-}" != 1 ] || { echo 'gh: HTTP 502' >&2; exit 1; }
    dir=
    while [ $# -gt 0 ]; do [ "$1" = -D ] && dir=$2; shift; done
    cp "$STUB_REL"/* "$dir/" ;;
  *) echo "stub gh: unexpected $*" >&2; exit 2 ;;
esac
EOF
cat >"$work/bin/cosign" <<'EOF'
#!/usr/bin/env bash
echo "cosign $*" >>"$CALLS"
exit "${STUB_COSIGN_RC:-0}"
EOF
# grype <source> ... --file <out>: writes $STUB_MATCHES (a JSON array of
# matches) as the report, or fails with $STUB_GRYPE_RC.
cat >"$work/bin/grype" <<'EOF'
#!/usr/bin/env bash
echo "grype $*" >>"$CALLS"
[ "${STUB_GRYPE_RC:-0}" = 0 ] || { echo "grype: failed" >&2; exit "$STUB_GRYPE_RC"; }
out=
while [ $# -gt 0 ]; do [ "$1" = --file ] && out=$2; shift; done
jq -n --argjson m "${STUB_MATCHES:-[]}" '{matches: $m}' >"$out"
EOF
chmod +x "$work/bin/"*

sha() { if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | awk '{print $1}'; }
# release: a release directory with two SBOMs listed in checksums.txt.
release() {
  rm -f "$work/rel"/*
  for p in linux_amd64 darwin_arm64; do
    echo "{\"sbom\": \"$p\"}" >"$work/rel/upgradescope_$p.tar.gz.sbom.json"
  done
  echo '{}' >"$work/rel/checksums.txt.sigstore.json"
  for f in "$work"/rel/*.sbom.json; do printf '%s  %s\n' "$(sha "$f")" "${f##*/}"; done >"$work/rel/checksums.txt"
}
match() { # match <severity> <id>: one grype match
  jq -nc --arg s "$1" --arg id "$2" '{vulnerability: {id: $id, severity: $s, fix: {versions: ["1.2.3"]}}, artifact: {name: "example.com/m", version: "1.2.2"}}'
}
run() {
  : >"$work/calls"
  code=0
  env PATH="$work/bin:$PATH" CALLS="$work/calls" STUB_REL="$work/rel" UPGRADESCOPE_REPO=o/r \
    IMAGE_SCAN_GRYPE="$work/bin/grype" GH_TOKEN=stub "$@" hack/image-scan.sh v9.9.9 >"$work/out" 2>&1 || code=$?
}

release
run
if [ "$code" = 0 ] && grep -q 'no fixable high or critical finding' "$work/out"; then ok "a clean release passes"; else fail "a clean release did not pass (exit $code)" "$work/out"; fi
grep -qF -- "--certificate-identity https://github.com/o/r/.github/workflows/release.yml@refs/tags/v9.9.9" "$work/calls" &&
  ok "checksums.txt is verified against the release workflow at the tag" || fail "no exact-identity cosign verify-blob" "$work/calls"
for p in linux/amd64 linux/arm64; do
  grep -qF "grype registry:ghcr.io/o/r:9.9.9 --platform $p -c $PWD/hack/grype.yaml --only-fixed" "$work/calls" &&
    ok "grype scans the $p image with hack/grype.yaml and --only-fixed" || fail "no grype scan of the $p image" "$work/calls"
done
[ "$(grep -c '^grype sbom:' "$work/calls")" = 2 ] && ok "grype scans every published linux and darwin SBOM" || fail "the SBOMs were not each scanned" "$work/calls"
[ "$(grep -n '^cosign ' "$work/calls" | head -1 | cut -d: -f1)" -lt "$(grep -n '^grype ' "$work/calls" | head -1 | cut -d: -f1)" ] &&
  ok "the signature is checked before grype runs" || fail "grype ran before the signature check" "$work/calls"

run IMAGE_SCAN_DIGEST="sha256:$(printf '%064d' 1)"
grep -qF "grype registry:ghcr.io/o/r@sha256:$(printf '%064d' 1) --platform linux/amd64" "$work/calls" &&
  ok "IMAGE_SCAN_DIGEST scans the index by digest" || fail "the digest was not used" "$work/calls"

run STUB_MATCHES="[$(match High GHSA-aaaa-bbbb-cccc)]"
if [ "$code" = 1 ] && grep -q '::error::v9.9.9: High GHSA-aaaa-bbbb-cccc in example.com/m 1.2.2 (fixed in 1.2.3)' "$work/out"; then
  ok "a fixable high finding fails (1) and is named"
else
  fail "a high finding did not fail with 1 (exit $code)" "$work/out"
fi
run STUB_MATCHES="[$(match Critical CVE-2026-0001)]"
[ "$code" = 1 ] && ok "a critical finding fails (1)" || fail "a critical finding did not fail (exit $code)" "$work/out"
run STUB_MATCHES="[$(match Medium CVE-2026-0002),$(match Low CVE-2026-0003)]"
[ "$code" = 0 ] && ok "medium and low findings pass" || fail "a medium finding failed (exit $code)" "$work/out"

run STUB_GRYPE_RC=1
[ "$code" = 2 ] && grep -q 'grype could not scan' "$work/out" && ok "a grype failure fails closed (2)" || fail "a grype failure did not exit 2 (exit $code)" "$work/out"
run STUB_COSIGN_RC=1
[ "$code" = 2 ] && ! grep -q '^grype ' "$work/calls" && ok "a checksums.txt that does not verify stops the scan (2), before grype" ||
  fail "an unverified checksums.txt did not stop the scan (exit $code)" "$work/out"
run STUB_GH_FAIL=1
[ "$code" = 2 ] && ok "a failed download fails closed (2)" || fail "a failed download did not exit 2 (exit $code)" "$work/out"
release
echo '{"tampered": true}' >"$work/rel/upgradescope_linux_amd64.tar.gz.sbom.json"
run
[ "$code" = 2 ] && grep -q 'upgradescope_linux_amd64.tar.gz.sbom.json does not match checksums.txt' "$work/out" && ! grep -q '^grype ' "$work/calls" &&
  ok "an SBOM that does not match checksums.txt stops the scan (2)" || fail "a tampered SBOM was scanned (exit $code)" "$work/out"
release
rm "$work"/rel/*.sbom.json
run
[ "$code" = 2 ] && grep -q 'publishes no linux or darwin SBOM' "$work/out" && ok "a release without SBOMs fails closed (2)" || fail "a release without SBOMs did not exit 2 (exit $code)" "$work/out"

# Wiring: the release verify job and the daily workflow run it.
grep -qF 'hack/image-scan.sh "$GITHUB_REF_NAME"' .github/workflows/release.yml &&
  awk '/^  verify:/ { v = 1; next } v && /^  [a-z]/ { v = 0 } v && /hack\/image-scan\.sh/ { f = 1 } END { exit !f }' .github/workflows/release.yml &&
  ok "release.yml's verify job scans the release it published" || fail "release.yml's verify job does not run hack/image-scan.sh"
grep -qE '^        run: hack/image-scan.sh$' .github/workflows/vuln-latest-release.yml &&
  ok "vuln-latest-release.yml scans the latest release daily" || fail "vuln-latest-release.yml does not run hack/image-scan.sh"
grep -q '^ignore:' hack/grype.yaml && ok "hack/grype.yaml exists with an ignore list" || fail "hack/grype.yaml has no ignore: list"

pass=$(grep -c '^ok' "$work/results" || true)
nfail=$(grep -c '^FAIL' "$work/results" || true)
echo "image-scan_test: $pass passed, $nfail failed"
[ "$nfail" -eq 0 ] && [ "$pass" -gt 0 ]
