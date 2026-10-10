#!/usr/bin/env bash
# The verdict of ci.yml's ci-ok job, the one check a branch ruleset requires
# (#58): fails unless every job it needs passed, or was skipped for a reason
# that holds. Tested offline by hack/ci-ok_test.sh.
#
# Environment: NEEDS, the needs context as JSON ({"job": {"result": ...,
# "outputs": {...}}}), and EVENT, github.event_name (a release's workflow_call
# carries its caller's event, a tag push).
#
# A job that was skipped passes in two cases only:
#   - it is not one of the gated jobs below: it carries its own `if` for the
#     event (lint on a schedule, registry off a pull request, ...), as ever;
#   - it is gated, and the changes job RAN and said it is not needed: result
#     "success" and the job's output equal to its not-needed value, below.
#     A gated job that is skipped while changes failed, was cancelled or was
#     skipped fails, and so does one skipped while changes said it is needed.
#     The one exception is the weekly schedule, which skips the jobs marked
#     "yes" on purpose (they run on pull requests, pushes and releases), and
#     on which changes does not run.
# Every other result than success or skipped fails, and the changes job must
# have succeeded on a pull request (nothing says a pull request needs nothing
# unless it did).
set -euo pipefail

: "${NEEDS:?ci-ok: NEEDS is not set}"
: "${EVENT:?ci-ok: EVENT is not set}"

# <job> <changes output> <output value that means not needed> <skipped on a
# schedule: yes|no>. hack/ci-ok_test.sh checks this table against ci.yml: a
# job whose `if` reads one of these outputs is here, and the reverse. The
# action job is not here: it is skipped on a release by design (see its `if`),
# which a table keyed on the changes job's answer cannot tell from a mistake.
GATED='
test          test-scope none  yes
test-heap     go         false yes
build         go         false yes
cross-build   cross      false yes
examples      go         false yes
images        go         false yes
vuln          go         false no
pg-conformance go        false no
kube          e2e        false no
envtest       e2e        false no
release-check release    false yes
'

# One jq call each: "<job> <result>" lines and "<output> <value>" lines of the
# changes job, looked up in bash (no process per lookup).
results=$(jq -r 'to_entries[] | "\(.key) \(.value.result)"' <<<"$NEEDS" | sort)
change_outputs=$(jq -r '(.changes.outputs // {}) | to_entries[] | "\(.key) \(.value)"' <<<"$NEEDS")
lookup() { # <key> <"key value" lines> <default>
  local k v
  while read -r k v; do
    if [ "$k" = "$1" ]; then
      echo "$v"
      return
    fi
  done <<<"$2"
  echo "$3"
}
result() { lookup "$1" "$results" missing; }
output() { lookup "$1" "$change_outputs" ""; }

bad=()
changes=$(result changes)
if [ "$EVENT" = pull_request ] && [ "$changes" != success ]; then
  bad+=("changes: $changes on a pull request (the gates cannot be trusted)")
fi

skipped_ok=0
while read -r job r; do
  case $r in
    success) ;;
    skipped) ;; # judged below
    *) bad+=("$job: $r") ;;
  esac
done <<<"$results"

while read -r job out notneeded onschedule; do
  [ -n "$job" ] || continue
  r=$(result "$job")
  case $r in
    missing) bad+=("$job: not in ci-ok's needs") ;;
    skipped)
      if [ "$changes" = success ] && [ "$(output "$out")" = "$notneeded" ]; then
        skipped_ok=$((skipped_ok + 1))
        echo "skipped $job: changes said $out=$notneeded"
      elif [ "$EVENT" = schedule ] && [ "$changes" = skipped ] && [ "$onschedule" = yes ]; then
        skipped_ok=$((skipped_ok + 1))
        echo "skipped $job: not run on a schedule"
      else
        bad+=("$job: skipped, but changes was '$changes' with $out='$(output "$out")' on a $EVENT event")
      fi
      ;;
  esac
done <<<"$GATED"

echo "$results"
if [ "${#bad[@]}" -gt 0 ]; then
  printf '::error::not every job passed or was skipped for a reason that holds:\n' >&2
  printf '::error::  %s\n' "${bad[@]}" >&2
  exit 1
fi
echo "ci-ok: every job passed or was skipped for a reason that holds ($skipped_ok gated off)"
