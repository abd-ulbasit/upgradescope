#!/usr/bin/env bash
# Offline tests for hack/check-doc-sizes.sh (make hack-test): a fake dist/
# (artifacts.json and files of chosen sizes) and fixture pages drive it
# through its pass and fail paths, and --list proves it reads sizes from
# the real README.md and docs/operations/install.md. Needs bash and jq.
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

MiB=1048576
# file <path> <bytes>
file() { mkdir -p "$(dirname "$1")" && head -c "$2" /dev/zero >"$1"; }

# dist <dir> <linux/amd64 binary bytes> <its archive bytes>: a fake GoReleaser
# dist/ with linux/amd64 and darwin/arm64 (1 MiB binary, 0.5 MiB archive).
dist() {
  local d=$1
  file "$d/upgradescope_linux_amd64_v1/upgradescope" "$2"
  file "$d/upgradescope_linux_amd64.tar.gz" "$3"
  file "$d/upgradescope_darwin_arm64_v8.0/upgradescope" "$MiB"
  file "$d/upgradescope_darwin_arm64.tar.gz" $((MiB / 2))
  cat >"$d/artifacts.json" <<EOF
[
  {"name": "upgradescope", "path": "$d/upgradescope_linux_amd64_v1/upgradescope", "goos": "linux", "goarch": "amd64", "type": "Binary"},
  {"name": "upgradescope_linux_amd64.tar.gz", "path": "$d/upgradescope_linux_amd64.tar.gz", "goos": "linux", "goarch": "amd64", "type": "Archive"},
  {"name": "upgradescope", "path": "$d/upgradescope_darwin_arm64_v8.0/upgradescope", "goos": "darwin", "goarch": "arm64", "type": "Binary"},
  {"name": "upgradescope_darwin_arm64.tar.gz", "path": "$d/upgradescope_darwin_arm64.tar.gz", "goos": "darwin", "goarch": "arm64", "type": "Archive"}
]
EOF
}

# Fixture pages in the real pages' formats.
readme() {
  cat >"$work/README.md" <<EOF
## Measured

| What | Measured | How |
|---|---|---|
| \`scan --files\` | median 0.022 s | 30 runs |
| Binary and archive, linux/amd64 (as released) | ${1:-2.0} MiB binary, ${2:-1.0} MiB archive | GoReleaser snapshot |
EOF
}
install_md() {
  cat >"$work/install.md" <<EOF
## Sizes

| Platform | Binary | Archive |
|---|---|---|
| linux/amd64 | ${1:-2.0} MiB | ${2:-1.0} MiB |
| darwin/arm64 | 1.0 MiB | 0.5 MiB |
${3:-}
EOF
}

# run <name> <want-exit> [env...]
run() {
  local name=$1 want=$2 got=0
  shift 2
  env DIST="$work/dist" DOC_SIZES_README="$work/README.md" DOC_SIZES_INSTALL="$work/install.md" \
    "$@" hack/check-doc-sizes.sh >"$work/out" 2>&1 || got=$?
  if [ "$got" = "$want" ]; then ok "$name"; else
    echo "(exit $got, want $want)" >>"$work/out"
    fail "$name" "$work/out"
  fi
}
has() { if grep -qF -- "$2" "$work/out"; then ok "$1"; else fail "$1" "$work/out"; fi; }

dist "$work/dist" $((2 * MiB)) "$MiB"
readme
install_md
run "documented sizes equal to dist/ pass" 0
has "each size is reported" "README.md linux/amd64 binary 2.0 MiB (built 2.00 MiB"
has "the install page's archives are checked" "ok: $work/install.md darwin/arm64 archive 0.5 MiB"

# 2% of 2 MiB is 0.04 MiB.
install_md 2.03
run "a size within 2% passes (rounding)" 0
install_md 2.1
run "a binary 5% off dist/ fails" 1
has "the failure names the page, platform, kind and the built size" "$work/install.md: linux/amd64 binary: documented 2.1 MiB, built 2.00 MiB"
install_md
readme 2.0 1.1
run "an archive 10% off dist/ fails" 1
has "the README archive failure is named" "README.md: linux/amd64 archive: documented 1.1 MiB, built 1.00 MiB"
readme

install_md 2.0 1.0 "| windows/arm64 | 2.0 MiB | 1.0 MiB |"
run "a documented platform dist/ did not build fails" 1
has "the missing platform is named" "no windows/arm64 binary in"
install_md

printf '## Sizes\n\nThe binary is about 2 MB.\n' >"$work/install.md"
run "an install page that documents no size fails, not passes vacuously" 1
has "the empty page is named" "$work/install.md documents no size"
install_md
printf '# upgradescope\n' >"$work/README.md"
run "a README that documents no size fails" 1
has "the empty README is named" "README.md documents no size"
readme

run "a missing dist/ fails" 1 DIST="$work/nodist"
has "the missing artifacts.json is named" "$work/nodist/artifacts.json"

install_md 2.3
run "DOC_SIZES_TOLERANCE_PCT widens the tolerance" 0 DOC_SIZES_TOLERANCE_PCT=20
install_md

# The real pages: --list reads at least one binary and one archive size
# from each, so a reformatted table cannot turn the release check off.
if hack/check-doc-sizes.sh --list >"$work/out" 2>&1; then ok "--list reads the real pages"; else fail "--list reads the real pages" "$work/out"; fi
for page in README.md docs/operations/install.md; do
  for kind in binary archive; do
    if grep -qE "^$page [a-z]+/(amd64|arm64) $kind [0-9.]+$" "$work/out"; then
      ok "$page documents a $kind size the check reads"
    else
      fail "$page documents a $kind size the check reads" "$work/out"
    fi
  done
done

pass=$(grep -c '^ok' "$work/results" || true)
failed=$(grep -c '^FAIL' "$work/results" || true)
echo "check-doc-sizes_test: $pass passed, $failed failed"
[ "$failed" -eq 0 ] && [ "$pass" -gt 0 ]
