#!/usr/bin/env bash
# Tests for script/render-formula.sh against the real checksums.txt of
# upgradescope v0.1.1 (script/testdata, as published on the release).
# Offline; Ruby checks the rendered formula's syntax when it is installed.
# In the upgradescope repository, `make hack-test` runs it.
set -euo pipefail
cd "$(dirname "$0")/.."

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
: >"$work/results"
ok() { echo "ok   $1" | tee -a "$work/results"; }
check() { if eval "$2"; then ok "$1"; else echo "FAIL $1" >&2; echo "FAIL $1" >>"$work/results"; fi; }

sums=script/testdata/checksums-v0.1.1.txt
script/render-formula.sh 0.1.1 "$sums" "$work/upgradescope.rb" 2>/dev/null
f="$work/upgradescope.rb"
for p in darwin_arm64 darwin_amd64 linux_arm64 linux_amd64; do
  sum="$(awk -v n="upgradescope_$p.tar.gz" '$2 == n { print $1 }' "$sums")"
  check "$p: url and its sha256 from checksums.txt" \
    "grep -A1 -F 'releases/download/v0.1.1/upgradescope_$p.tar.gz\"' '$f' | grep -qxF '      sha256 \"$sum\"'"
done
check "no placeholder left" "! grep -q '@[A-Z0-9_]*@' '$f'"
check "the test runs upgradescope version" "grep -qF 'shell_output(\"#{bin}/upgradescope version\")' '$f'"
check "installs completions and man pages" "grep -qF 'generate_completions_from_executable' '$f' && grep -qF 'man1.install' '$f'"
# Until the update workflow moves it to a newer release, the committed
# formula must be exactly what the template renders (no hand edits).
check "a committed v0.1.1 Formula/upgradescope.rb is this render" \
  "! grep -qF 'download/v0.1.1/' Formula/upgradescope.rb || cmp -s '$f' Formula/upgradescope.rb"
if command -v ruby >/dev/null; then
  check "rendered formula is valid Ruby" "ruby -c '$f' >/dev/null"
fi

grep -v linux_arm64 "$sums" >"$work/missing.txt"
check "a checksums.txt without an archive fails" \
  "! script/render-formula.sh 0.1.1 '$work/missing.txt' - >'$work/out' 2>&1 && grep -qF 'no single sha256 for upgradescope_linux_arm64.tar.gz' '$work/out'"
check "a pre-release version is refused" \
  "! script/render-formula.sh 0.2.0-rc.1 '$sums' - >'$work/out' 2>&1 && grep -qF 'stable releases only' '$work/out'"

pass=$(grep -c '^ok' "$work/results" || true)
fail=$(grep -c '^FAIL' "$work/results" || true)
echo "render-formula_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ] && [ "$pass" -gt 0 ]
