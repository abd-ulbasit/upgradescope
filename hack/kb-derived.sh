#!/usr/bin/env bash
# Regenerates the committed files derived from the embedded knowledge base
# (make kb-derived), so a refresh of the data and what it feeds land in one
# commit. kb-refresh.yml's registry job runs this after `make eol-sync`, and
# its api-lifecycle job after `make gen-kb`, and each commits what it
# rewrote with its data: a weekly sync used to open a
# PR that failed its own CI on a doc example the new data had made stale
# (#252: EKS 1.37's dates changed "the newest known is 1.36" in
# docs/concepts/support-lifecycle.md).
#
# Each file is rewritten by the test that pins it, run with -update, so the
# generator and the check cannot disagree:
#   - docs/concepts/support-lifecycle.md: the scan examples
#     (TestSupportLifecycleDocExamplesMatchScanOutput; registry data), only
#     the examples that are stale;
#   - deploy/chart/files/kb-rbac-rules.yaml: the agent's list rules for
#     the KB's kinds (TestKBRBACRulesInSync; API lifecycle data).
# Neither reads the network or runs anything but this module's tests.
#
# --list prints the files it may rewrite, one a line, and runs nothing.
set -euo pipefail
cd "$(dirname "$0")/.."

# package <TAB> test <TAB> file it rewrites
derived='./internal/cli	TestSupportLifecycleDocExamplesMatchScanOutput	docs/concepts/support-lifecycle.md
./deploy/chart	TestKBRBACRulesInSync	deploy/chart/files/kb-rbac-rules.yaml'

if [ "${1:-}" = --list ]; then
  cut -f3 <<<"$derived"
  exit 0
fi
[ $# = 0 ] || { echo "usage: $0 [--list]" >&2; exit 2; }

while IFS=$'\t' read -r pkg test file; do
  echo "== $file ($pkg $test -update)"
  go test "$pkg" -count=1 -run "^$test\$" -update
done <<<"$derived"
echo "kb-derived: regenerated; git status shows what changed"
