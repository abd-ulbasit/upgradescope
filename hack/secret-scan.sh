#!/usr/bin/env bash
# Fails when the repository holds something that looks like a credential
# (make secret-scan; CI's repo-checks job runs it on every pull request and
# push). gitleaks, pinned and sha256-verified by hack/install-tool.sh, with
# its default rules and the allowlist of test fixtures in .gitleaks.toml.
# Findings are printed redacted.
#
#   hack/secret-scan.sh            the files git would commit: tracked and
#                                  untracked ones, not ignored ones (bin/,
#                                  dist/, node_modules/), as they are on disk
#   hack/secret-scan.sh --history  every commit reachable from any ref, which
#                                  needs a full clone (no --depth, no
#                                  --filter): after a scare, before making a
#                                  fork public, or when rotating credentials
#
# Neither replaces GitHub's push protection, which stops a known token type
# before it reaches the repository; this also catches generic keys and runs
# on every pull request whatever the repository settings are.
#
# Knobs: SECRET_SCAN_GITLEAKS (a gitleaks binary; default
# hack/install-tool.sh gitleaks).
# Exit: 0 nothing found, 1 findings, 2 could not scan.
set -euo pipefail
cd "$(dirname "$0")/.."

die() { echo "::error::secret-scan: $*" >&2; exit 2; }
mode=tree
case "${1:-}" in
  "") ;;
  --history) mode=history ;;
  *) echo "usage: $0 [--history]" >&2; exit 2 ;;
esac
command -v jq >/dev/null || die "jq is not installed"
gitleaks=${SECRET_SCAN_GITLEAKS:-$(hack/install-tool.sh gitleaks)} || die "cannot install gitleaks"
config=$PWD/.gitleaks.toml
[ -f "$config" ] || die "no $config"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
report="$work/report.json"

if [ "$mode" = history ]; then
  [ "$(git rev-parse --is-shallow-repository)" = false ] || die "a shallow clone has no history to scan: fetch it all first (git fetch --unshallow)"
  set +e
  "$gitleaks" git . --config "$config" --redact --no-banner --log-opts=--all \
    --report-format json --report-path "$report" >"$work/log" 2>&1
  rc=$?
  set -e
  what="every commit of every ref"
else
  # A copy of exactly the files git would commit, so build output and
  # dependencies that .gitignore keeps out are not scanned.
  mkdir "$work/tree"
  n=0
  while IFS= read -r -d '' f; do
    [ -f "$f" ] && [ ! -L "$f" ] || continue
    mkdir -p "$work/tree/$(dirname "$f")"
    cp "$f" "$work/tree/$f"
    n=$((n + 1))
  done < <(git ls-files -z --cached --others --exclude-standard --deduplicate)
  [ "$n" -gt 0 ] || die "git lists no files to scan"
  set +e
  (cd "$work/tree" && "$gitleaks" dir . --config "$config" --redact --no-banner \
    --report-format json --report-path "$report") >"$work/log" 2>&1
  rc=$?
  set -e
  what="$n files"
fi

# gitleaks exits 1 both on findings and on errors: the report decides.
if ! jq -e 'type == "array"' "$report" >/dev/null 2>&1; then
  sed 's/^/  /' "$work/log" >&2
  die "gitleaks did not produce a report (exit $rc)"
fi
count=$(jq length "$report")
if [ "$count" -gt 0 ]; then
  jq -r '.[] | "::error file=\(.File),line=\(.StartLine)::secret-scan: \(.RuleID): \(.Description) (\(.Match))" +
    (if .Commit != "" then " in commit \(.Commit[0:12])" else "" end)' "$report"
  echo "secret-scan: $count finding(s) in $what. Remove the secret and rotate it at its source (a value that reached a commit, a pushed branch or a CI log is exposed); allowlist a fake fixture in .gitleaks.toml by its value" >&2
  exit 1
fi
[ "$rc" = 0 ] || { sed 's/^/  /' "$work/log" >&2; die "gitleaks failed (exit $rc) with an empty report"; }
echo "secret-scan: no secrets in $what"
