#!/usr/bin/env bash
# Tests for script/render-formula.sh against the real checksums.txt of
# upgradescope v0.1.1 (script/testdata, as published on the release), and
# for update.yml's commit step. v0.1.1 is a fixture only: it is unsigned, so
# update.sh refuses it and the tap never ships it.
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
if command -v ruby >/dev/null; then
  check "rendered formula is valid Ruby" "ruby -c '$f' >/dev/null"
fi

grep -v linux_arm64 "$sums" >"$work/missing.txt"
check "a checksums.txt without an archive fails" \
  "! script/render-formula.sh 0.1.1 '$work/missing.txt' - >'$work/out' 2>&1 && grep -qF 'no single sha256 for upgradescope_linux_arm64.tar.gz' '$work/out'"
check "a pre-release version is refused" \
  "! script/render-formula.sh 0.2.0-rc.1 '$sums' - >'$work/out' 2>&1 && grep -qF 'stable releases only' '$work/out'"

# update.yml's commit step, run as Actions runs it, in a scratch tap clone
# with `git push` stubbed. The tap starts with no formula (the first signed
# release creates it), so the first render is a new, untracked file: the
# step must commit it, then find a second identical render current.
commit_step="$work/commit-step.sh"
awk '
  { line = $0; sub(/^ +/, "", line) }
  line ~ /^- / { inside = (line == "- name: commit when the formula changed"); inrun = 0; next }
  inside && line == "run: |" { match($0, /^ */); ind = RLENGTH; inrun = 1; next }
  inrun { match($0, /^ */); if ($0 !~ /^ *$/ && RLENGTH <= ind) { inrun = 0; inside = 0; next }; print substr($0, ind + 3) }
' .github/workflows/update.yml >"$commit_step"
if [ -s "$commit_step" ] && command -v git >/dev/null; then
  realgit="$(command -v git)"
  mkdir -p "$work/bin" "$work/tap"
  printf '#!/bin/sh\nif [ "$1" = push ]; then echo "push $*" >>"%s/pushes"; exit 0; fi\nexec "%s" "$@"\n' "$work" "$realgit" >"$work/bin/git"
  chmod +x "$work/bin/git"
  : >"$work/pushes"
  (
    cd "$work/tap"
    git init -q -b main . && git -c user.name=t -c user.email=t@t commit -q --allow-empty -m init
  )
  run_commit_step() {
    (cd "$work/tap" && PATH="$work/bin:$PATH" GH_TOKEN=x GITHUB_REPOSITORY=o/homebrew-tap GITHUB_REF_NAME=main \
      GITHUB_STEP_SUMMARY="$work/summary" bash --noprofile --norc -eo pipefail "$commit_step" >/dev/null 2>&1)
  }
  cp -R formula.rb.tmpl script "$work/tap/"
  check "the first render creates Formula/upgradescope.rb" \
    "'$work/tap/script/render-formula.sh' 0.1.1 '$work/tap/$sums' 2>/dev/null && cmp -s '$f' '$work/tap/Formula/upgradescope.rb'"
  check "the first formula (a new file) is committed and pushed" \
    "run_commit_step && [ \"\$(git -C '$work/tap' log -1 --format=%s)\" = 'upgradescope 0.1.1' ] && [ \$(wc -l <'$work/pushes') -eq 1 ]"
  check "an unchanged formula is not committed again" \
    "run_commit_step && [ \$(git -C '$work/tap' rev-list --count HEAD) -eq 2 ] && [ \$(wc -l <'$work/pushes') -eq 1 ] && grep -qF 'is current' '$work/summary'"
else
  check "update.yml has a 'commit when the formula changed' step" "false"
fi

pass=$(grep -c '^ok' "$work/results" || true)
fail=$(grep -c '^FAIL' "$work/results" || true)
echo "render-formula_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ] && [ "$pass" -gt 0 ]
