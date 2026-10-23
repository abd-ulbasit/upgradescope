#!/usr/bin/env bash
# Tests for the release notes footer in .goreleaser.yml (make hack-test): it
# renders for a stable tag and for a pre-release, and the Homebrew line is
# only on the stable one. The tap (abd-ulbasit/homebrew-tap) pulls stable
# releases only, so a pre-release's notes cannot offer `brew install` (#198).
# The header (#388) links the tag's CHANGELOG.md. Header and footer are Go
# text/templates over {{ .Tag }}, {{ .Version }} and {{ .Prerelease }}
# (GoReleaser fills in the rest at publish), so they are rendered here with
# text/template and those three fields. Needs Go. Offline.
set -euo pipefail
cd "$(dirname "$0")/.."

shopt -s nocasematch # [[ == ]] below is the case-insensitive substring test
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
: >"$work/results"

# The footer is the last key of the file: from "  footer: |" on, 4 spaces in.
awk '/^  footer: \|/ { on = 1; next } on { sub(/^    /, ""); print }' .goreleaser.yml >"$work/footer.tmpl"
[ -s "$work/footer.tmpl" ] || { echo "FAIL no release footer in .goreleaser.yml" >&2; exit 1; }
# The header is the block under "  header: |" in the release: section, up to
# the next key (a line indented 2 spaces that is not a continuation).
awk '/^  header: \|/ { on = 1; next } on && /^  [^ ]/ { exit } on { sub(/^    /, ""); print }' .goreleaser.yml >"$work/header.tmpl"
[ -s "$work/header.tmpl" ] || { echo "FAIL no release header in .goreleaser.yml" >&2; exit 1; }

cat >"$work/render.go" <<'EOF'
package main

import (
	"os"
	"text/template"
)

func main() {
	t := template.Must(template.ParseFiles(os.Args[1]))
	if err := t.Execute(os.Stdout, map[string]string{"Tag": "v" + os.Args[2], "Version": os.Args[2], "Prerelease": os.Args[3]}); err != nil {
		panic(err)
	}
}
EOF
go build -o "$work/render" "$work/render.go"

check() { # check <name> <version> <prerelease> <want: has|lacks> <substring> [header]
  local name=$1 version=$2 pre=$3 want=$4 needle=$5 part=${6:-footer} got=lacks
  "$work/render" "$work/$part.tmpl" "$version" "$pre" >"$work/out"
  [[ "$(<"$work/out")" == *"$needle"* ]] && got=has
  if [ "$got" = "$want" ]; then
    echo "ok   $name" | tee -a "$work/results"
  else
    echo "FAIL $name: $part $got '$needle', want $want; $part:" >&2
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
# The Verify block (#377): the signatures are cosign 3 bundles, so the image
# and chart commands fail with "no signatures found" on cosign 2.6.2 and older.
for v in "0.2.0 ''" "0.2.0-rc.2 rc.2"; do
  set -- $v
  pre=${2:-}; pre=${pre//\'/}
  check "the footer ($1) names the cosign minimum" "$1" "$pre" has 'cosign 3.x, or 2.6.3 or later'
  check "the footer ($1) says older cosign finds no signatures" "$1" "$pre" has 'no signatures found'
  check "the footer ($1) keeps the image cosign verify command" "$1" "$pre" has "cosign verify ghcr.io/abd-ulbasit/upgradescope:$1 \\"
  check "the footer ($1) shows the chart cosign verify command" "$1" "$pre" has "cosign verify ghcr.io/abd-ulbasit/charts/upgradescope:$1"
  check "the footer ($1) keeps verify-blob for checksums.txt" "$1" "$pre" has 'cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json'
  check "the footer ($1) says verify-blob works on older cosign" "$1" "$pre" has 'verify-blob` of `checksums.txt` works from cosign 2.4.3'
  # The exact identity of this tag, never a pattern over every tag: one would
  # accept another release's signed checksums.txt or image (a rollback or a
  # swap between releases).
  check "the footer ($1) verifies against this tag's exact identity" "$1" "$pre" has \
    "--certificate-identity 'https://github.com/abd-ulbasit/upgradescope/.github/workflows/release.yml@refs/tags/v$1'"
  check "the footer ($1) offers no identity regexp" "$1" "$pre" lacks 'identity-regexp'
  # Build provenance: an attestation from any workflow of the repository
  # passes without --signer-workflow and --source-ref.
  check "the footer ($1) pins gh attestation verify to release.yml" "$1" "$pre" has \
    '--signer-workflow abd-ulbasit/upgradescope/.github/workflows/release.yml'
  check "the footer ($1) pins gh attestation verify to this tag" "$1" "$pre" has "--source-ref refs/tags/v$1 --deny-self-hosted-runners"
  check "the footer ($1) verifies the image's provenance from the registry" "$1" "$pre" has \
    "gh attestation verify oci://ghcr.io/abd-ulbasit/upgradescope:$1 --bundle-from-oci"
done
check "no template action is left unrendered" 0.2.0 "" lacks '{{'

# The header (#388): the notes list only the type!: commits under Breaking
# changes, so they link the CHANGELOG at the tag, the reviewed record.
for v in "0.2.1 ''" "0.3.0-rc.1 rc.1"; do
  set -- $v
  pre=${2:-}; pre=${pre//\'/}
  check "the header ($1) links CHANGELOG.md at the tag" "$1" "$pre" has "(https://github.com/abd-ulbasit/upgradescope/blob/v$1/CHANGELOG.md)" header
  check "the header ($1) says the CHANGELOG lists every breaking change" "$1" "$pre" has 'reviewed record and lists every breaking change' header
  check "the header ($1) leaves no template action unrendered" "$1" "$pre" lacks '{{' header
done

pass=$(grep -c '^ok' "$work/results" || true)
fail=$(grep -c '^FAIL' "$work/results" || true)
echo "release-footer_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ] && [ "$pass" -gt 0 ]
