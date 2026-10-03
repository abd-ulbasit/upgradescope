#!/usr/bin/env bash
# The upgradescope GitHub Action's logic. action.yml (repository root) and
# action/action.yml are thin wrappers that run it twice:
#   run.sh install   validate the inputs, put a checksum-verified
#                    upgradescope on PATH (via GITHUB_PATH)
#   run.sh scan      run the gate, set the outputs, annotate, and write the
#                    step summary
#
# Inputs arrive as environment variables (INPUT_PATH, INPUT_TARGET,
# INPUT_FAIL_ON, INPUT_ALLOW_INCOMPLETE, INPUT_VERSION, INPUT_CONFIG,
# INPUT_BASELINE, INPUT_WRITE_BASELINE), never as ${{ }} expressions in a script:
# the runner pastes an expression's value into the script text, so a value
# holding `"; cmd` would run cmd (GitHub's script-injection guidance).
# ACTION_REF is the ref the action was used at (github.action_ref). Text that
# reaches the log from an input, a file name or the binary cannot start a
# workflow command: a value in a message is escaped (esc), and the gate's
# stderr and the Markdown report are printed by logged, which defangs any
# line that would be one, including a ##[ anywhere in it.
# hack/action_test.sh (make action-test) covers every path here offline.
set -euo pipefail

releases=https://github.com/abd-ulbasit/upgradescope/releases
module=github.com/abd-ulbasit/upgradescope/cmd/upgradescope

# esc <text>: the text as one workflow-command value (% and line breaks
# escaped), so no line of it can start a command of its own. An input
# echoed to the log goes through it: a value holding a newline and
# "::warning::" would otherwise forge an annotation, or "::add-mask::" a
# log mask. A ##[ is written "# #[": the runner reads that legacy command
# anywhere in a line that is not a :: command, and esc is also used in plain
# echoes.
esc() {
  local s=$1
  s=${s//\%/%25}
  s=${s//$'\r'/%0D}
  s=${s//$'\n'/%0A}
  s=${s//'##['/'# #['}
  printf '%s' "$s"
}

die() {
  echo "::error::$(esc "upgradescope action: $*")"
  exit 1
}

# An action ref that is a release tag: vX.Y.Z or vX.Y.Z-rc.N.
release_tag='^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$'

# release_older <a> <b>: whether release tag a is older than b. Semver
# order: the core numerically, then a candidate (-rc.N) before its release.
release_older() {
  local a=${1#v} b=${2#v} ac bc ap bp i
  ac=${a%%-*} bc=${b%%-*}
  ap=${a#"$ac"} bp=${b#"$bc"}
  if [ "$ac" != "$bc" ]; then
    local IFS=.
    local x=($ac) y=($bc)
    for i in 0 1 2; do
      ((10#${x[i]} == 10#${y[i]})) || return $((10#${x[i]} < 10#${y[i]} ? 0 : 1))
    done
  fi
  [ "$ap" != "$bp" ] || return 1
  [ -n "$ap" ] || return 1
  [ -n "$bp" ] || return 0
  ap=${ap##*.} bp=${bp##*.}
  [[ $ap =~ ^[0-9]+$ ]] || ap=0
  [[ $bp =~ ^[0-9]+$ ]] || bp=0
  ((10#$ap < 10#$bp))
}

validate() {
  local v=${INPUT_VERSION-} t=${INPUT_TARGET-} f=${INPUT_FAIL_ON-} p=${INPUT_PATH-}
  # Empty is the default: the action ref's release, else latest.
  [ -z "$v" ] || [[ $v =~ ^(latest|preinstalled|v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?)$ ]] ||
    die "invalid version '$v' (want latest, preinstalled or a release tag like v0.2.0)"
  [[ $t =~ ^v?1\.[0-9]+(\.[0-9]+)?$ ]] ||
    die "invalid target '$t' (want a Kubernetes minor version like 1.36)"
  case $f in
    blocker | warning | never) ;;
    *) die "invalid fail-on '$f' (want blocker, warning or never)" ;;
  esac
  # Unset (an older action.yml, a direct run) is false.
  case ${INPUT_ALLOW_INCOMPLETE:-false} in
    true | false) ;;
    *) die "invalid allow-incomplete '$INPUT_ALLOW_INCOMPLETE' (want true or false)" ;;
  esac
  [ -n "$p" ] || die "path is required"
  [ -e "$p" ] || die "path '$p' does not exist (render the manifests before this step)"
  local c=${INPUT_CONFIG-} b=${INPUT_BASELINE-} w=${INPUT_WRITE_BASELINE-}
  [ -z "$c" ] || [ -f "$c" ] || die "config '$c' is not a file (want the path to an .upgradescope.yaml)"
  [ -z "$b" ] || [ -f "$b" ] || die "baseline '$b' is not a file (want the JSON report of an earlier scan)"
  if [ -n "$w" ]; then
    [ ! -d "$w" ] || die "write-baseline '$w' is a directory (want a file path)"
    local d=.
    case $w in */*) d=${w%/*} && d=${d:-/} ;; esac
    [ -d "$d" ] || die "write-baseline '$w': directory '$d' does not exist"
  fi
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
    echo "using $(command -v upgradescope): $(upgradescope --version | sed -n 1p)"
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

  # No version: the release the action itself is pinned at, when its ref is
  # one, so @v0.2.0-rc.2 runs v0.2.0-rc.2. GitHub's latest skips
  # prereleases, and would run an older release's engine (it missed
  # removals added since) and pass what this one blocks. Any other ref (a
  # branch, a SHA, v0) has no release of its own: latest.
  local tag=$INPUT_VERSION ref=${ACTION_REF-}
  if [ -z "$tag" ]; then
    if [[ $ref =~ $release_tag ]]; then
      tag=$ref
      echo "version defaults to the action ref $tag"
    else
      tag=latest
    fi
  fi
  if [ "$tag" = latest ]; then
    # Pin "latest" to one tag first, so the archive and checksums.txt come
    # from the same release even if one is published in between.
    local url
    url=$(curl -fsSL --retry 3 -o /dev/null -w '%{url_effective}' "$releases/latest") || url=
    tag=${url##*/}
    if [[ ! $tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
      echo "cannot resolve the latest release (got '$(esc "$url")')"
      go_install latest
      return
    fi
    echo "latest release is $tag"
    if [[ $ref =~ $release_tag ]] && release_older "$tag" "$ref"; then
      echo "::warning::version latest is $tag, older than this action's own release $ref: GitHub's latest skips prereleases, and an older engine can pass what $ref blocks. Set version: $ref"
    fi
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
  echo "installed $("$bin_dir/upgradescope" --version | sed -n 1p) from $releases/download/$tag/$asset"
}

# annotations: one annotation per blocker or warning finding, at its first
# object's file and line when it has one: ::error when the finding fails
# the gate at INPUT_FAIL_ON, else ::warning (so with fail-on never, a
# passing step shows no errors). A finding the baseline already had never
# fails the gate, so it is a ::warning titled "in baseline"; suppressed
# findings are not in .findings and get none. Values are escaped per the
# workflow-command format.
annotations() {
  jq -r --arg failon "$INPUT_FAIL_ON" '
    def data: gsub("%"; "%25") | gsub("\r"; "%0D") | gsub("\n"; "%0A");
    def prop: data | gsub(":"; "%3A") | gsub(","; "%2C");
    (.filesBase // "") as $base
    | (.findings // [])[]
    | select(.severity == "blocker" or .severity == "warning")
    | ([(.objects // [])[] | select(.file)] | first) as $o
    | (.baselineState == "unchanged") as $known
    | (if ($known | not) and ($failon == "warning" or (.severity == "blocker" and $failon == "blocker"))
       then "error" else "warning" end) as $level
    | "::" + $level + " "
      + (if $o then "file=" + ((if $base == "" then $o.file else $base + "/" + $o.file end) | prop)
           + ",line=" + ($o.line | tostring) + "," else "" end)
      + "title=" + ("upgradescope \(.severity) (\(.category)\(if $known then ", in baseline" else "" end))" | prop)
      + "::" + ((.title + (if .remediation then ". Fix: " + .remediation else "" end)) | data)
  ' "$1"
}

# escaped <file>: the file as one workflow-command value (% and line breaks
# escaped), so none of its lines can start a command of its own.
escaped() {
  awk '{ gsub(/%/, "%25"); gsub(/\r/, "%0D"); printf "%s%s", (NR > 1 ? "%0A" : ""), $0 }' "$1"
}

# logged <file>: the file to the log, unchanged except that no line can be a
# workflow command. The runner reads a command after trimming leading
# whitespace and treats a carriage return as a line break, so a line that
# then starts with :: gets a "| " in front and a CR is written %0D. The
# runner also reads the legacy form ##[name] anywhere in a line that is not
# a :: command (and runs ##[warning], ##[error] and ##[add-mask]), so each
# ##[ is written "# #[". The binary's messages repeat the path input and
# file names from the scanned tree, which a fork PR chooses, and so does
# the Markdown report (the renderer turns a line break in a path into a
# space, but leaves the rest).
logged() {
  awk '{ gsub(/\r/, "%0D"); gsub(/##\[/, "# #["); if ($0 ~ /^[ \t]*::/) $0 = "| " $0; print }' "$1"
}

scan() {
  # Its own directory per scan: RUNNER_TEMP is shared by every step of the
  # job, and a later use of the action must not overwrite the reports an
  # earlier one's sarif-file and report-json outputs point at.
  local out
  out=$(mktemp -d "$RUNNER_TEMP/upgradescope.XXXXXX")
  local sarif="$out/results.sarif" json="$out/report.json" md="$out/summary.md"
  local args=(--files="$INPUT_PATH" --target="$INPUT_TARGET")
  [ -z "${INPUT_CONFIG-}" ] || args+=(--config="$INPUT_CONFIG")
  if [ -n "${INPUT_BASELINE-}" ]; then
    # All three passes compare with a copy: with write-baseline naming the
    # same file, the gate pass replaces it before the others read it.
    cp -- "$INPUT_BASELINE" "$out/baseline.json"
    args+=(--baseline="$out/baseline.json")
  fi
  local gate=("${args[@]}")
  [ -z "${INPUT_WRITE_BASELINE-}" ] || gate+=(--write-baseline="$INPUT_WRITE_BASELINE")
  # Only the gate pass: the JSON and Markdown passes run with --fail-on
  # never, where an unknown verdict already passes.
  local allow=()
  [ "${INPUT_ALLOW_INCOMPLETE:-false}" != true ] || allow=(--allow-incomplete)

  # The gate. exit 0: passed; 2: scan worked, gate failed (the SARIF is
  # still complete: upload it with `if: ${{ !cancelled() && ... }}`);
  # anything else (1: the scan broke; 137: killed): the SARIF is empty or cut short.
  local status=0
  upgradescope scan "${gate[@]}" --output sarif --fail-on="$INPUT_FAIL_ON" ${allow[@]+"${allow[@]}"} >"$sarif" 2>"$out/gate.err" || status=$?
  # Its messages (the scan error, a skipped file) are the log's account of
  # why the gate went as it did, whatever the exit status.
  logged "$out/gate.err"
  if [ "$status" != 0 ] && [ "$status" != 2 ]; then
    printf '### upgradescope: scan failed (exit %s)\n\nThe job log has the error.\n' "$status" >>"${GITHUB_STEP_SUMMARY:-/dev/null}"
    echo "::error::upgradescope scan failed (exit $status)"
    exit "$status"
  fi
  # Only a complete SARIF is an output: after a scan error, an upload step
  # guarded on sarif-file != '' skips, and does not fail on an empty file.
  echo "sarif-file=$sarif" >>"$GITHUB_OUTPUT"

  # The same scan as JSON (outputs, annotations) and Markdown (summary,
  # log). --fail-on never: the gate above already decided the exit code.
  if ! command -v jq >/dev/null; then
    echo "::warning::jq is not installed, so report-json, the verdict, score and count outputs and the annotations are not set"
  elif upgradescope scan "${args[@]}" --output json --fail-on never >"$json" 2>"$out/json.err"; then
    echo "report-json=$json" >>"$GITHUB_OUTPUT"
    # v0.1.x reports have no verdict: derive it as the engine does (a
    # blocker is blocked; else ready, or unknown when not ready). new-*
    # leave out what the baseline already had: the findings the gate
    # counts.
    jq -r '
      def count($sev): [(.findings // [])[] | select(.severity == $sev)] | length;
      def new($sev): [(.findings // [])[] | select(.severity == $sev and .baselineState != "unchanged")] | length;
      (.verdict // (if any((.findings // [])[]; .severity == "blocker") then "blocked"
                    elif .ready then "ready" else "unknown" end)) as $verdict
      | "verdict=\($verdict)", "score=\(.score)", "ready=\(.ready)",
      "blockers=\(count("blocker"))", "warnings=\(count("warning"))",
      "new-blockers=\(new("blocker"))", "new-warnings=\(new("warning"))",
      "suppressed=\((.suppressed // []) | length)"
    ' "$json" >>"$GITHUB_OUTPUT"
    annotations "$json"
  else
    echo "::warning::upgradescope --output json failed, so the outputs and annotations are not set: $(escaped "$out/json.err")"
  fi
  if upgradescope scan "${args[@]}" --output markdown --fail-on never >"$md" 2>"$out/md.err"; then
    echo "summary-file=$md" >>"$GITHUB_OUTPUT"
    logged "$md"
    cat "$md" >>"${GITHUB_STEP_SUMMARY:-/dev/null}"
  else
    echo "::warning::this upgradescope cannot write the step summary (it predates --output markdown, added after v0.1.1): $(escaped "$out/md.err")"
  fi
  exit "$status"
}

case "${1:-}" in
  install) validate && install ;;
  scan) validate && scan ;;
  *) die "usage: run.sh install|scan" ;;
esac
