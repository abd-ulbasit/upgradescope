#!/usr/bin/env bash
# Tests for hack/chart-release-annotations.sh (make hack-test). Each case
# stamps a copy of deploy/chart with a fixture CHANGELOG and asserts the
# result, with helm when it is installed (the packaged chart must still
# lint and render the pinned image). Offline.
set -euo pipefail
cd "$(dirname "$0")/.."

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
DIGEST="sha256:$(printf '%064d' 42)"
IMAGE=ghcr.io/abd-ulbasit/upgradescope

cat >"$work/CHANGELOG.md" <<'EOF'
# Changelog

## [Unreleased]

## [0.2.0] - 2026-10-20

### Added

- `upgradescope version` prints the commit, build date and KB
  horizon.
- deb, rpm and apk packages with "quotes" and a backslash \ in text.

### Fixed

- The chart pulls a published image.

## [0.1.1] - 2026-07-27

### Fixed

- Older entry that must not appear.
EOF

: >"$work/results"
ok() { echo "ok   $1" | tee -a "$work/results"; }
bad() { echo "FAIL $1" >&2; echo "FAIL $1" >>"$work/results"; }
check() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }
stamp() { # stamp <case> <tag> [digest]: copy the chart, run the script
  local d="$work/$1"
  cp -R deploy/chart "$d"
  hack/chart-release-annotations.sh "$d" "$2" "${3:-$DIGEST}" "$work/CHANGELOG.md" >"$work/$1.out" 2>&1
}

check "in-repo artifacthub.io/images names the appVersion image" \
  "grep -qxF \"      image: $IMAGE:\$(sed -n 's/^appVersion: *//p' deploy/chart/Chart.yaml | tr -d '\"')\" deploy/chart/Chart.yaml"

stamp stable v0.2.0
c="$work/stable"
check "stable: values pin the digest" "grep -qxF '  digest: \"$DIGEST\"' '$c/values.yaml'"
check "stable: Artifact Hub image is tag@digest" "grep -qxF '      image: $IMAGE:v0.2.0@$DIGEST' '$c/Chart.yaml'"
check "stable: not a prerelease" "grep -qxF '  artifacthub.io/prerelease: \"false\"' '$c/Chart.yaml'"
check "stable: three changes" "[ \"\$(grep -c '^    - kind: ' '$c/Chart.yaml')\" = 4 ]" # + the CRD entry
check "stable: continuation lines joined" "grep -qF 'description: \"\`upgradescope version\` prints the commit, build date and KB horizon.\"' '$c/Chart.yaml'"
check "stable: kinds from the headings" "grep -A1 -F -- '- kind: fixed' '$c/Chart.yaml' | grep -qF 'The chart pulls a published image.'"
check "stable: quotes and backslashes escaped" "grep -qF '\\\"quotes\\\" and a backslash \\\\ in text' '$c/Chart.yaml'"
check "stable: older versions left out" "! grep -qF 'Older entry' '$c/Chart.yaml'"
check "stable: source chart untouched" "grep -qxF '  digest: \"\"' deploy/chart/values.yaml"
if command -v helm >/dev/null; then
  check "stable: stamped chart lints" "helm lint --strict '$c' >/dev/null 2>&1"
  check "stable: stamped chart renders the pinned image on both pods" \
    "[ \"\$(helm template x '$c' --set server.enabled=true --set server.ingestToken=t | grep -cF \"image: \\\"$IMAGE:\$(sed -n 's/^appVersion: *//p' '$c/Chart.yaml' | tr -d '\"')@$DIGEST\\\"\")\" = 2 ]"
  check "stable: Chart.yaml annotations still parse" \
    "helm show chart '$c' | grep -qF 'artifacthub.io/changes'"
fi

stamp pre v0.2.0-rc.1
check "prerelease tag sets artifacthub.io/prerelease true" "grep -qxF '  artifacthub.io/prerelease: \"true\"' '$work/pre/Chart.yaml'"
check "prerelease takes its version's changes" "grep -qF 'The chart pulls a published image.' '$work/pre/Chart.yaml'"
stamp unreleased v0.3.0 || true
check "a tag with no CHANGELOG entries fails" "grep -qF 'no CHANGELOG entries for v0.3.0' '$work/unreleased.out'"

stamp baddigest v0.2.0 sha256:abc || true
check "a malformed digest fails" "grep -qF 'is not a sha256:<64 hex> digest' '$work/baddigest.out'"
stamp badtag 0.2.0 || true
check "a tag without v fails" "grep -qF \"'0.2.0' is not a vX.Y.Z[-pre] tag\" '$work/badtag.out'"

cp -R deploy/chart "$work/pinned"
sed -i.bak 's/^  digest: ""$/  digest: "sha256:'"$(printf '%064d' 1)"'"/' "$work/pinned/values.yaml" && rm -f "$work/pinned/values.yaml.bak"
hack/chart-release-annotations.sh "$work/pinned" v0.2.0-rc.1 "$DIGEST" "$work/CHANGELOG.md" >"$work/pinned.out" 2>&1 || true
check "a chart that already pins a digest fails (stamp only a fresh copy)" "grep -qF 'image.digest: expected to change exactly one line' '$work/pinned.out'"

pass=$(grep -c '^ok' "$work/results" || true)
fail=$(grep -c '^FAIL' "$work/results" || true)
echo "chart-release-annotations_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ] && [ "$pass" -gt 0 ]
