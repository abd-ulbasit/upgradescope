#!/usr/bin/env bash
# Tests .github/workflows/vuln-latest-release.yml's two run steps (make
# hack-test): the scan step must record hack/vuln-latest-release.sh's own
# exit status (0 clean, 1 findings, 2 broken), and the issue step must word
# the tracking issue by it. Run through make, the status was make's: a
# failing recipe always makes make exit 2, so reachable advisories were
# reported as "could not run". This extracts each step's `run: |` block from
# the workflow and runs it the way Actions does (bash -eo pipefail), with a
# stub scan script and a stub gh. Offline.
set -euo pipefail
cd "$(dirname "$0")/.."

wf=.github/workflows/vuln-latest-release.yml
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
: >"$work/results"
ok() { echo "ok   $1" | tee -a "$work/results"; }
fail() { echo "FAIL $1" >&2; echo "FAIL $1" >>"$work/results"; }

# step_run <name>: the `run: |` block of the step named <name>, dedented.
step_run() {
  awk -v want="- name: $1" '
    { line = $0; sub(/^ +/, "", line) }
    line ~ /^- / { inside = (line == want); inrun = 0; next }
    inside && line == "run: |" { match($0, /^ */); ind = RLENGTH; inrun = 1; next }
    inrun {
      match($0, /^ */)
      if ($0 !~ /^ *$/ && RLENGTH <= ind) { inrun = 0; inside = 0; next }
      print substr($0, ind + 3)
    }
  ' "$wf"
}
step_run 'govulncheck (binary mode) on the latest release' >"$work/scan.sh"
step_run 'Open, update or close the tracking issue' >"$work/issue.sh"
[ -s "$work/scan.sh" ] && [ -s "$work/issue.sh" ] || { echo "FAIL cannot find the scan and issue steps' run blocks in $wf" >&2; exit 1; }

# A checkout whose scan script exits $STUB_CODE. It carries the real
# Makefile, so a step that goes back to `make vuln-latest` is caught.
mkdir -p "$work/repo/hack" "$work/bin"
cp Makefile "$work/repo/"
cat >"$work/repo/hack/vuln-latest-release.sh" <<'EOF'
#!/usr/bin/env bash
printf 'v9.9.9\n' >"$UPGRADESCOPE_RELEASE_TAG_FILE"
echo "vuln-latest-release: stub (exit $STUB_CODE)"
[ "$STUB_CODE" = 0 ] || echo "::error::vulncheck: ${STUB_FINDING:-GO-2026-0001} is reachable"
exit "$STUB_CODE"
EOF
chmod +x "$work/repo/hack/vuln-latest-release.sh"

# gh: `issue list` prints $STUB_OPEN (the open tracking issue, if any),
# `issue view` $STUB_SEEN (what that issue already says); every call is
# logged, and a --body-file's content with it.
cat >"$work/bin/gh" <<'EOF'
#!/usr/bin/env bash
echo "gh $*" >>"$GH_LOG"
case "$1 $2" in
  "issue list") [ -z "${STUB_OPEN:-}" ] || echo "$STUB_OPEN" ;;
  "issue view") printf '%s\n' "${STUB_SEEN:-}" ;;
esac
while [ $# -gt 0 ]; do
  [ "$1" = --body-file ] && { cat "$2" >>"$GH_LOG"; shift; }
  shift
done
EOF
chmod +x "$work/bin/gh"

# run_case <name> <scan-exit> <open-issue> <want-code> <want-issue-exit> <want-in-gh-log>...
run_case() {
  local name=$1 code=$2 open=$3 want_code=$4 want_exit=$5 got=0
  shift 5
  local rt="$work/rt-$code-${open:-none}"
  mkdir -p "$rt"
  : >"$rt/out" >"$rt/summary" >"$rt/gh.log"
  (cd "$work/repo" && RUNNER_TEMP="$rt" GITHUB_OUTPUT="$rt/out" STUB_CODE="$code" \
    bash --noprofile --norc -eo pipefail "$work/scan.sh" >/dev/null 2>&1) || { fail "$name: scan step failed"; return; }
  grep -qx "code=$want_code" "$rt/out" || { fail "$name: scan step recorded $(grep '^code=' "$rt/out") (want code=$want_code)"; return; }
  grep -qx "tag=v9.9.9" "$rt/out" || { fail "$name: scan step did not record the tag"; return; }
  (cd "$work/repo" && PATH="$work/bin:$PATH" RUNNER_TEMP="$rt" GITHUB_STEP_SUMMARY="$rt/summary" GH_LOG="$rt/gh.log" \
    CODE="$want_code" TAG="${TAG:-v9.9.9}" STUB_OPEN="$open" STUB_SEEN="${SEEN:-}" RUN_URL=https://example.invalid/run/1 \
    bash --noprofile --norc -eo pipefail "$work/issue.sh" >/dev/null 2>&1) || got=$?
  [ "$got" = "$want_exit" ] || { fail "$name: issue step exited $got (want $want_exit)"; return; }
  local needle
  for needle in "$@"; do
    case "$needle" in
      '!'*) ! grep -qF -- "${needle#!}" "$rt/gh.log" || { fail "$name: gh log has '${needle#!}'"; sed 's/^/     /' "$rt/gh.log" >&2; return; } ;;
      *) grep -qF -- "$needle" "$rt/gh.log" || { fail "$name: gh log lacks '$needle'"; sed 's/^/     /' "$rt/gh.log" >&2; return; } ;;
    esac
  done
  ok "$name"
}

run_case "findings (exit 1) open an issue worded as findings" 1 "" 1 1 \
  "gh issue create --title Latest release has reachable vulnerabilities" \
  "govulncheck finds reachable advisories in the published binary of v9.9.9" \
  "GO-2026-0001 is reachable" "!could not run"
run_case "findings with an open issue comment on it" 1 42 1 1 \
  "gh issue comment 42 --body-file" "govulncheck finds reachable advisories" "!gh issue create"
# The issue already carries that run's report: the same findings on the
# same tag add no comment (the job still fails); new findings or a new tag
# do.
SEEN=$(cat "$work/rt-1-42/gh.log")
run_case "unchanged findings on the same tag do not comment again" 1 42 1 1 \
  "!gh issue comment" "!gh issue create"
STUB_FINDING=GO-2026-0002 run_case "a changed finding set comments again" 1 42 1 1 \
  "gh issue comment 42 --body-file" "GO-2026-0002 is reachable"
TAG=v9.9.10 run_case "the same findings on a new tag comment again" 1 42 1 1 \
  "gh issue comment 42 --body-file" "published binary of v9.9.10"
SEEN=
run_case "a broken scan (exit 2) is worded as could not run" 2 "" 2 1 \
  "the scan of the latest release (v9.9.9) could not run" "!finds reachable advisories"
run_case "a clean scan closes the open issue" 0 42 0 0 \
  "gh issue close 42" "!gh issue create"
run_case "a clean scan with no open issue files nothing" 0 "" 0 0 \
  "!gh issue create" "!gh issue comment"

pass=$(grep -c '^ok' "$work/results" || true)
failed=$(grep -c '^FAIL' "$work/results" || true)
echo "vuln-latest-release-workflow_test: $pass passed, $failed failed"
[ "$failed" -eq 0 ] && [ "$pass" -gt 0 ]
