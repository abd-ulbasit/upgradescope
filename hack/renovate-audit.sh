#!/usr/bin/env bash
# Advisory gate for hack/renovate/package-lock.json (make renovate-audit,
# CI's vuln job; #244). That lockfile pins the Renovate release CI runs to
# validate examples/renovate; it is bumped by hand, and the vuln and web
# gates cover only the Go binary and web/. This fails closed:
#   - on any advisory `npm audit` reports in the lockfile whose GHSA ID is
#     not an [[IgnoredVulns]] entry of hack/renovate/osv-scanner.toml (the
#     same file OSV-Scanner, and so Scorecard, reads);
#   - on an entry past its ignoreUntil date;
#   - on an entry whose package has a fix the lockfile can take without a
#     new Renovate major (npm's fixAvailable: true, or a non-major
#     Renovate bump): then bump, do not ignore;
#   - on an osv-scanner.toml entry without an id, an ignoreUntil date or a
#     reason, and on npm audit failing or printing anything but its JSON
#     report.
# It warns on an entry no advisory matches any more.
#
# Exit status: 0 pass, 1 an advisory fails the gate, 2 the audit or the
# ignore list is broken.
#
# Knobs, mostly for hack/renovate-audit_test.sh:
#   RENOVATE_AUDIT_JSON    read this npm audit --json report instead of running npm
#   RENOVATE_AUDIT_IGNORE  the ignore list (default hack/renovate/osv-scanner.toml)
#   RENOVATE_AUDIT_TODAY   YYYY-MM-DD to evaluate ignoreUntil against (default: today, UTC)
set -euo pipefail
cd "$(dirname "$0")/.."

IGNORE=${RENOVATE_AUDIT_IGNORE:-hack/renovate/osv-scanner.toml}
TODAY=${RENOVATE_AUDIT_TODAY:-$(date -u +%Y-%m-%d)}

die() { echo "::error::renovate-audit: $*" >&2; exit 2; }
command -v jq >/dev/null || die "jq is required"
[ -r "$IGNORE" ] || die "$IGNORE is missing"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# The ignore list, one "<id> <ignoreUntil> <reason>" line per entry. Only the
# subset of TOML this file uses: [[IgnoredVulns]] tables of id, ignoreUntil
# (a bare date) and reason (a one-line basic string).
awk -v f="$IGNORE" '
  function flush() {
    if (!inside) return
    if (id == "" || until == "" || reason == "") { printf "entry %d: needs id, ignoreUntil and reason\n", n > "/dev/stderr"; bad = 1 }
    else print id, until, reason
  }
  /^[ \t]*(#|$)/ { next }
  /^\[\[IgnoredVulns\]\][ \t]*$/ { flush(); inside = 1; n++; id = until = reason = ""; next }
  /^\[/ { printf "unexpected table %s\n", $0 > "/dev/stderr"; bad = 1; next }
  inside && /^id = "GHSA-[0-9a-z]{4}-[0-9a-z]{4}-[0-9a-z]{4}"$/ { id = $3; gsub(/"/, "", id); next }
  inside && /^ignoreUntil = [0-9]{4}-[0-9]{2}-[0-9]{2}$/ { until = $3; next }
  inside && /^reason = ".+"$/ { reason = substr($0, 11, length($0) - 11); next }
  { printf "cannot read line %d: %s\n", NR, $0 > "/dev/stderr"; bad = 1 }
  END { flush(); exit bad }
' "$IGNORE" >"$work/ignore" 2>"$work/ignore.err" || die "$IGNORE: $(tr '\n' ';' <"$work/ignore.err")"

if [ -n "${RENOVATE_AUDIT_JSON:-}" ]; then
  cp "$RENOVATE_AUDIT_JSON" "$work/audit.json"
else
  command -v npm >/dev/null || die "npm is required"
  # npm audit exits 1 when it finds anything; the report decides.
  (cd hack/renovate && npm audit --package-lock-only --json) >"$work/audit.json" 2>"$work/audit.err" || true
fi
jq -e 'type == "object" and (.vulnerabilities | type == "object") and (.metadata.vulnerabilities | type == "object")' \
  "$work/audit.json" >/dev/null 2>&1 ||
  die "npm audit did not produce a report: $(head -c 400 "$work/audit.err" 2>/dev/null | tr '\n' ' ')$(head -c 400 "$work/audit.json" | tr '\n' ' ')"

# One "<GHSA id> <package> <fix>" line per advisory and the package it is
# reported on directly; fix is "fix" when npm can fix it without a Renovate
# major, else "none".
jq -r '
  .vulnerabilities | to_entries[] | .key as $pkg | .value as $v
  | ($v.fixAvailable | if . == true then "fix" elif type == "object" and (.isSemVerMajor | not) then "fix" else "none" end) as $fix
  | $v.via[] | objects | (.url // "" | capture("(?<id>GHSA-[0-9a-z-]+)$").id // "") as $id
  | "\(if $id == "" then .source else $id end) \($pkg) \($fix)"
' "$work/audit.json" | sort -u >"$work/found"

rc=0
while read -r id pkg fix; do
  entry=$(awk -v id="$id" '$1 == id' "$work/ignore")
  if [ -z "$entry" ]; then
    echo "::error::renovate-audit: $id in $pkg (hack/renovate/package-lock.json) is not accepted in $IGNORE: bump hack/renovate (see package.json), or add an entry with a reason and an ignoreUntil date"
    rc=1
    continue
  fi
  read -r _ until _ <<<"$entry"
  if [[ "$until" < "$TODAY" ]]; then
    echo "::error::renovate-audit: the $IGNORE entry for $id ($pkg) expired on $until: check its reason again and bump ignoreUntil, or bump hack/renovate"
    rc=1
  elif [ "$fix" = fix ]; then
    echo "::error::renovate-audit: $id ($pkg) now has a fix the lockfile can take: run 'cd hack/renovate && npm update --package-lock-only --ignore-scripts' (or bump Renovate) and drop the entry"
    rc=1
  else
    echo "renovate-audit: $id ($pkg) accepted until $until"
  fi
done <"$work/found"
while read -r id until _; do
  grep -q "^$id " "$work/found" || echo "::warning::renovate-audit: the $IGNORE entry for $id matches no advisory any more; remove it"
done <"$work/ignore"
total=$(jq -r '.metadata.vulnerabilities.total // 0' "$work/audit.json")
echo "renovate-audit: $total vulnerable packages reported, $(cut -d' ' -f1 "$work/found" | sort -u | grep -c . || true) advisories, $(wc -l <"$work/ignore" | tr -d ' ') accepted"
exit "$rc"
