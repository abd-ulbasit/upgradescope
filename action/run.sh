#!/usr/bin/env bash
# The upgradescope GitHub Action's logic. action.yml (repository root) and
# action/action.yml are thin wrappers that run it twice:
#   run.sh install   validate the inputs, put a checksum-verified
#                    upgradescope on PATH (via GITHUB_PATH)
#   run.sh scan      run the gate, set the outputs, annotate, and write the
#                    step summary
#
# Inputs arrive as environment variables (INPUT_PATH, INPUT_TARGET,
# INPUT_FAIL_ON, INPUT_VERSION), never as ${{ }} expressions in a script:
# the runner pastes an expression's value into the script text, so a value
# holding `"; cmd` would run cmd (GitHub's script-injection guidance).
# hack/action_test.sh (make action-test) covers every path here offline.
set -euo pipefail

releases=https://github.com/abd-ulbasit/upgradescope/releases
module=github.com/abd-ulbasit/upgradescope/cmd/upgradescope

die() {
  echo "::error::upgradescope action: $*"
  exit 1
}

validate() {
  local v=${INPUT_VERSION-} t=${INPUT_TARGET-} f=${INPUT_FAIL_ON-} p=${INPUT_PATH-}
  [[ $v =~ ^(latest|preinstalled|v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?)$ ]] ||
    die "invalid version '$v' (want latest, preinstalled or a release tag like v0.2.0)"
  [[ $t =~ ^v?1\.[0-9]+(\.[0-9]+)?$ ]] ||
    die "invalid target '$t' (want a Kubernetes minor version like 1.36)"
  case $f in
    blocker | warning | never) ;;
    *) die "invalid fail-on '$f' (want blocker, warning or never)" ;;
  esac
  [ -n "$p" ] || die "path is required"
  [ -e "$p" ] || die "path '$p' does not exist (render the manifests before this step)"
  : "${RUNNER_TEMP:?run.sh runs in a GitHub Actions step}" "${GITHUB_OUTPUT:?}" "${GITHUB_PATH:?}"
}

sha256() {
  if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | awk '{print $1}'
}

# go_install <version>: the fallback when no release archive downloads.
# Builds from source, so only with a Go toolchain on the runner.
go_install() {
  command -v go >/dev/null ||
    die "no release archive for $os/$arch at $1 and no Go toolchain to build it from source; use a published release (https://github.com/abd-ulbasit/upgradescope/releases) or add actions/setup-go before this action"
  echo "falling back to go install $module@$1"
  go install "$module@$1"
  local gobin
  gobin=$(go env GOBIN)
  [ -n "$gobin" ] || gobin="$(go env GOPATH)/bin"
  echo "$gobin" >>"$GITHUB_PATH"
}

install() {
  if [ "$INPUT_VERSION" = preinstalled ]; then
    command -v upgradescope >/dev/null || die "version is preinstalled, but there is no upgradescope on PATH"
    echo "using $(command -v upgradescope): $(upgradescope --version)"
    return
  fi
  case "$(uname -s)" in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    *) die "unsupported runner OS $(uname -s): the action runs on Linux and macOS runners" ;;
  esac
  case "$(uname -m)" in
    x86_64 | amd64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) die "unsupported runner architecture $(uname -m) (want amd64 or arm64)" ;;
  esac
  # Asset name must match .goreleaser.yml archives.name_template, which
  # deliberately omits the version (hack/release-check.sh checks this line).
  asset="upgradescope_${os}_${arch}.tar.gz"

  local tag=$INPUT_VERSION
  if [ "$tag" = latest ]; then
    # Pin "latest" to one tag first, so the archive and checksums.txt come
    # from the same release even if one is published in between.
    local url
    url=$(curl -fsSL --retry 3 -o /dev/null -w '%{url_effective}' "$releases/latest") || url=
    tag=${url##*/}
    if [[ ! $tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
      echo "cannot resolve the latest release (got '$url')"
      go_install latest
      return
    fi
    echo "latest release is $tag"
  fi

  local dl
  dl=$(mktemp -d "$RUNNER_TEMP/upgradescope-dl.XXXXXX")
  if ! curl -fsSL --retry 3 -o "$dl/$asset" "$releases/download/$tag/$asset"; then
    echo "no release archive at $releases/download/$tag/$asset"
    rm -rf "$dl"
    go_install "$tag"
    return
  fi
  # From here on, fail closed: an archive that cannot be verified is never
  # installed, and never swapped for a source build.
  curl -fsSL --retry 3 -o "$dl/checksums.txt" "$releases/download/$tag/checksums.txt" ||
    die "cannot download checksums.txt for $tag, so $asset cannot be verified"
  local want got
  want=$(awk -v f="$asset" '$2 == f || $2 == "*" f { print $1; exit }' "$dl/checksums.txt")
  [ -n "$want" ] || die "checksums.txt for $tag has no entry for $asset"
  got=$(sha256 "$dl/$asset")
  [ "$got" = "$want" ] || die "sha256 mismatch for $asset ($tag): got $got, checksums.txt says $want"
  echo "sha256 OK: $asset ($tag)"

  local bin_dir="$RUNNER_TEMP/upgradescope-bin"
  mkdir -p "$bin_dir"
  tar -xzf "$dl/$asset" -C "$bin_dir" upgradescope
  rm -rf "$dl"
  chmod +x "$bin_dir/upgradescope"
  echo "$bin_dir" >>"$GITHUB_PATH"
  echo "installed $("$bin_dir/upgradescope" --version) from $releases/download/$tag/$asset"
}

# annotations: one ::error (blocker) or ::warning per finding, at its first
# object's file and line when it has one. Values are escaped per the
# workflow-command format.
annotations() {
  jq -r '
    def data: gsub("%"; "%25") | gsub("\r"; "%0D") | gsub("\n"; "%0A");
    def prop: data | gsub(":"; "%3A") | gsub(","; "%2C");
    (.filesBase // "") as $base
    | (.findings // [])[]
    | select(.severity == "blocker" or .severity == "warning")
    | ([(.objects // [])[] | select(.file)] | first) as $o
    | "::" + (if .severity == "blocker" then "error" else "warning" end) + " "
      + (if $o then "file=" + ((if $base == "" then $o.file else $base + "/" + $o.file end) | prop)
           + ",line=" + ($o.line | tostring) + "," else "" end)
      + "title=" + ("upgradescope \(.severity) (\(.category))" | prop)
      + "::" + ((.title + (if .remediation then ". Fix: " + .remediation else "" end)) | data)
  ' "$1"
}

scan() {
  local sarif="$RUNNER_TEMP/upgradescope-results.sarif"
  local json="$RUNNER_TEMP/upgradescope-report.json"
  local md="$RUNNER_TEMP/upgradescope-summary.md"
  local args=(--files="$INPUT_PATH" --target="$INPUT_TARGET")
  echo "sarif-file=$sarif" >>"$GITHUB_OUTPUT"

  # The gate. exit 0: passed; 2: scan worked, gate failed (the SARIF is
  # still complete: upload it with `if: always()`); 1: the scan broke.
  local status=0
  upgradescope scan "${args[@]}" --output sarif --fail-on="$INPUT_FAIL_ON" >"$sarif" || status=$?
  if [ "$status" != 0 ] && [ "$status" != 2 ]; then
    printf '### upgradescope: scan failed (exit %s)\n\nThe job log has the error.\n' "$status" >>"${GITHUB_STEP_SUMMARY:-/dev/null}"
    echo "::error::upgradescope scan failed (exit $status)"
    exit "$status"
  fi

  # The same scan as JSON (outputs, annotations) and Markdown (summary,
  # log). --fail-on never: the gate above already decided the exit code.
  if ! command -v jq >/dev/null; then
    echo "::warning::jq is not installed, so the verdict, score, blockers and warnings outputs and the annotations are not set"
  elif upgradescope scan "${args[@]}" --output json --fail-on never >"$json" 2>"$RUNNER_TEMP/upgradescope-json.err"; then
    echo "report-json=$json" >>"$GITHUB_OUTPUT"
    # v0.1.x reports have no verdict: derive it as the engine does (a
    # blocker is blocked; else ready, or unknown when not ready).
    jq -r '
      (.verdict // (if any((.findings // [])[]; .severity == "blocker") then "blocked"
                    elif .ready then "ready" else "unknown" end)) as $verdict
      | "verdict=\($verdict)", "score=\(.score)", "ready=\(.ready)",
      "blockers=\([(.findings // [])[] | select(.severity == "blocker")] | length)",
      "warnings=\([(.findings // [])[] | select(.severity == "warning")] | length)"
    ' "$json" >>"$GITHUB_OUTPUT"
    annotations "$json"
  else
    echo "::warning::upgradescope --output json failed, so the outputs and annotations are not set: $(cat "$RUNNER_TEMP/upgradescope-json.err")"
  fi
  if upgradescope scan "${args[@]}" --output markdown --fail-on never >"$md" 2>"$RUNNER_TEMP/upgradescope-md.err"; then
    cat "$md"
    cat "$md" >>"${GITHUB_STEP_SUMMARY:-/dev/null}"
  else
    echo "::warning::this upgradescope cannot write the step summary (it predates --output markdown, added after v0.1.1): $(cat "$RUNNER_TEMP/upgradescope-md.err")"
  fi
  exit "$status"
}

case "${1:-}" in
  install) validate && install ;;
  scan) validate && scan ;;
  *) die "usage: run.sh install|scan" ;;
esac
