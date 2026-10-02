#!/usr/bin/env bash
# Tests release.yml's "Push the Artifact Hub repository metadata" step (make
# hack-test). No CI run reaches it before a tag is pushed, and it runs after
# the chart is already pushed and signed: a broken step fails the chart job
# and skips verify, major-tag and krew. It once called oras by the relative
# path hack/install-tool.sh printed after a `cd`, which failed on every
# release. This extracts the step's `run: |` block and runs it the way
# Actions does (bash -eo pipefail), in a copy of the checkout with the real
# hack/install-tool.sh and the real metadata file, against a stub oras that
# is already installed under the pinned version. Offline.
set -euo pipefail
cd "$(dirname "$0")/.."

wf=.github/workflows/release.yml
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
step_run 'Push the Artifact Hub repository metadata' >"$work/step.sh"
[ -s "$work/step.sh" ] || { echo "FAIL cannot find the Artifact Hub step's run block in $wf" >&2; exit 1; }
chart_repo=$(sed -n 's/^  CHART_REPO: *//p' "$wf")
[ -n "$chart_repo" ] || { echo "FAIL cannot find the workflow-level CHART_REPO in $wf" >&2; exit 1; }

# The checkout: the real installer and metadata, and a stub oras already
# installed where the installer puts it by default (bin/tools), with the pin
# it records, so it is reused instead of downloaded.
repo="$work/repo"
mkdir -p "$repo/hack" "$repo/packaging" "$repo/bin/tools"
cp hack/install-tool.sh "$repo/hack/"
cp -R packaging/artifacthub "$repo/packaging/"
platform=linux/amd64
version=$(sed -n 's/^ORAS_VERSION=\([^ ]*\).*/\1/p' hack/install-tool.sh)
sha=$(sed -n "s|^ *\"oras $platform\") echo \([0-9a-f]*\) ;;|\1|p" hack/install-tool.sh)
[ -n "$version" ] && [ -n "$sha" ] || { echo "FAIL cannot read the oras pin for $platform from hack/install-tool.sh" >&2; exit 1; }
echo "$version $sha" >"$repo/bin/tools/.oras.pin"
# oras: logs its working directory, arguments and stdin; for push, every
# file:mediatype argument must name a file that exists from there.
cat >"$repo/bin/tools/oras" <<'EOF'
#!/usr/bin/env bash
{ echo "cwd $PWD"; echo "oras $*"; } >>"$ORAS_LOG"
case "$1" in
  login) echo "stdin $(cat)" >>"$ORAS_LOG" ;;
  push)
    shift 2
    for arg in "$@"; do
      case "$arg" in
        --*) ;;
        *:application/*)
          f=${arg%%:application/*}
          [ -e "$f" ] || { echo "oras: $f: no such file" >&2; exit 1; }
          [ "$f" = /dev/null ] || cp "$f" "$ORAS_LOG.layer"
          ;;
      esac
    done
    ;;
esac
EOF
chmod +x "$repo/bin/tools/oras"

: >"$work/oras.log"
got=0
(cd "$repo" && CHART_REPO="$chart_repo" GH_TOKEN=s3cr3t GITHUB_ACTOR=release-bot \
  UPGRADESCOPE_TOOL_PLATFORM="$platform" ORAS_LOG="$work/oras.log" \
  bash --noprofile --norc -eo pipefail "$work/step.sh" >"$work/out" 2>&1) || got=$?
if [ "$got" = 0 ]; then
  ok "the step runs from a fresh checkout"
else
  fail "the step exited $got:"
  sed 's/^/     /' "$work/out" >&2
fi

# expect_log <name> <line>: the oras log has <line>.
expect_log() {
  if grep -qxF -- "$2" "$work/oras.log"; then ok "$1"; else
    fail "$1: oras log lacks '$2'"
    sed 's/^/     /' "$work/oras.log" >&2
  fi
}
expect_log "logs in to ghcr.io as the actor, token on stdin" \
  "oras login ghcr.io -u release-bot --password-stdin"
expect_log "the token reaches oras on stdin, not argv" "stdin s3cr3t"
expect_log "pushes to the chart repository's artifacthub.io tag" \
  "oras push ${chart_repo#oci://}/upgradescope:artifacthub.io --config /dev/null:application/vnd.cncf.artifacthub.config.v1+yaml artifacthub-repo.yml:application/vnd.cncf.artifacthub.repository-metadata.layer.v1.yaml"
if cmp -s "$work/oras.log.layer" packaging/artifacthub/artifacthub-repo.yml; then
  ok "the pushed layer is packaging/artifacthub/artifacthub-repo.yml"
else
  fail "the pushed layer is not packaging/artifacthub/artifacthub-repo.yml"
fi
if grep -q '^oras .*s3cr3t' "$work/oras.log"; then fail "the token appears in oras's arguments"; else
  ok "the token is never an argument"
fi

pass=$(grep -c '^ok' "$work/results" || true)
failed=$(grep -c '^FAIL' "$work/results" || true)
echo "release-artifacthub-workflow_test: $pass passed, $failed failed"
[ "$failed" -eq 0 ] && [ "$pass" -gt 0 ]
