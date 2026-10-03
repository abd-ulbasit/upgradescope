#!/usr/bin/env bash
# Tests for the release notes footer in .goreleaser.yml (make hack-test): it
# renders for a stable tag and for a pre-release, and the Homebrew line is
# only on the stable one. The tap (abd-ulbasit/homebrew-tap) pulls stable
# releases only, so a pre-release's notes cannot offer `brew install` (#198).
# The footer is a Go text/template over {{ .Version }} and {{ .Prerelease }}
# (GoReleaser fills in the rest at publish), so it is rendered here with
# text/template and those two fields. Needs Go. Offline.
set -euo pipefail
cd "$(dirname "$0")/.."

shopt -s nocasematch # [[ == ]] below is the case-insensitive substring test
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
: >"$work/results"

# The footer is the last key of the file: from "  footer: |" on, 4 spaces in.
awk '/^  footer: \|/ { on = 1; next } on { sub(/^    /, ""); print }' .goreleaser.yml >"$work/footer.tmpl"
[ -s "$work/footer.tmpl" ] || { echo "FAIL no release footer in .goreleaser.yml" >&2; exit 1; }

cat >"$work/render.go" <<'EOF'
package main

import (
	"os"
	"text/template"
)

func main() {
	t := template.Must(template.ParseFiles(os.Args[1]))
	if err := t.Execute(os.Stdout, map[string]string{"Version": os.Args[2], "Prerelease": os.Args[3]}); err != nil {
		panic(err)
	}
}
EOF
go build -o "$work/render" "$work/render.go"

check() { # check <name> <version> <prerelease> <want: has|lacks> <substring>
  local name=$1 version=$2 pre=$3 want=$4 needle=$5 got=lacks
  "$work/render" "$work/footer.tmpl" "$version" "$pre" >"$work/out"
  [[ "$(<"$work/out")" == *"$needle"* ]] && got=has
  if [ "$got" = "$want" ]; then
    echo "ok   $name" | tee -a "$work/results"
  else
    echo "FAIL $name: footer $got '$needle', want $want; footer:" >&2
    sed 's/^/     /' "$work/out" >&2
    echo "FAIL $name" >>"$work/results"
  fi
}

check "a stable release offers Homebrew" 0.2.0 "" has 'brew install abd-ulbasit/tap/upgradescope'
check "a pre-release does not offer Homebrew" 0.2.0-rc.2 rc.2 lacks 'brew install'
check "a pre-release does not mention the tap at all" 0.2.0-rc.2 rc.2 lacks 'homebrew'
check "a stable release's list is intact (krew follows Homebrew)" 0.2.0 "" has $'hours)\n- krew:'
check "a pre-release's list starts at krew" 0.2.0-rc.2 rc.2 has $'## Install\n\n- krew:'
check "the image line carries the version (pre-release)" 0.2.0-rc.2 rc.2 has 'ghcr.io/abd-ulbasit/upgradescope:0.2.0-rc.2'
check "no template action is left unrendered" 0.2.0 "" lacks '{{'

pass=$(grep -c '^ok' "$work/results" || true)
fail=$(grep -c '^FAIL' "$work/results" || true)
echo "release-footer_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ] && [ "$pass" -gt 0 ]
