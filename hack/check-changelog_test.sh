#!/usr/bin/env bash
# Tests for hack/check-changelog.sh (make hack-test): which CHANGELOG.md
# section describes a tag, and that an empty one refuses the release.
# Offline.
set -euo pipefail
cd "$(dirname "$0")/.."

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
: >"$work/results"

# changelog <file> <unreleased-entries> <0.2.0-section?>
changelog() {
  {
    printf '# Changelog\n\n## [Unreleased]\n\n'
    [ -n "$2" ] && printf '### Fixed\n\n- %s\n\n' "$2"
    [ "$3" = yes ] && printf '## [0.2.0] - 2026-10-20\n\n### Added\n\n- Packages.\n\n'
    [ "$3" = empty ] && printf '## [0.2.0] - 2026-10-20\n\n'
    printf '## [0.1.1] - 2026-07-27\n\n### Fixed\n\n- Old.\n'
  } >"$1"
}
expect() { # expect <name> <want-exit> <substring> <args...>
  local name=$1 want=$2 needle=$3 got=0
  shift 3
  hack/check-changelog.sh "$@" >"$work/out" 2>&1 || got=$?
  if [ "$got" = "$want" ] && grep -qF -- "$needle" "$work/out"; then
    echo "ok   $name" | tee -a "$work/results"
  else
    echo "FAIL $name: exit $got (want $want), output:" >&2
    sed 's/^/     /' "$work/out" >&2
    echo "FAIL $name" >>"$work/results"
  fi
}

c="$work/released.md"
changelog "$c" "" yes
expect "stable tag with its own section passes" 0 "described by ## [0.2.0]" v0.2.0 "$c"
expect "pre-release falls back to its version's section" 0 "described by ## [0.2.0]" v0.2.0-rc.1 "$c"
expect "--print writes the section body" 0 "- Packages." --print v0.2.0 "$c"
if hack/check-changelog.sh --print v0.2.0 "$c" | grep -qF -- '- Old.'; then
  echo "FAIL --print stops at the next section" >&2; echo "FAIL --print stops at the next section" >>"$work/results"
else
  echo "ok   --print stops at the next section" | tee -a "$work/results"
fi

c="$work/unreleased.md"
changelog "$c" "A fix." no
expect "no version section: a non-empty [Unreleased] describes the tag" 0 "described by ## [Unreleased]" v0.2.0 "$c"

c="$work/empty.md"
changelog "$c" "" no
expect "empty [Unreleased] and no version section refuses the tag" 1 "the ## [Unreleased] section of $c has no entries" v0.2.0 "$c"

c="$work/empty-version.md"
changelog "$c" "A fix." empty
expect "an empty version section refuses the tag (no fallback past it)" 1 "the ## [0.2.0] section of $c has no entries" v0.2.0 "$c"

expect "a malformed tag fails" 1 "'0.2.0' is not a vX.Y.Z[-pre] tag" 0.2.0 "$c"

pass=$(grep -c '^ok' "$work/results" || true)
fail=$(grep -c '^FAIL' "$work/results" || true)
echo "check-changelog_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ] && [ "$pass" -gt 0 ]
