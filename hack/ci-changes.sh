#!/usr/bin/env bash
# The decision step of ci.yml's changes job: turns what the path filters say
# about a pull request into the outputs the gated jobs read and ci-ok checks
# (hack/ci-ok.sh). Prints name=value lines for $GITHUB_OUTPUT.
#
# Inputs, from the environment. Each is "true" or "false" (anything else, or
# nothing, fails the step, and a failed changes job fails ci-ok), except
# EVENT and CHANGED_FILES:
#   EVENT          github.event_name
#   CHANGED_FILES  the pull request's changed-file count (a pull request
#                  only: the other events do not read it). The path filters
#                  list files through the API, which stops at 3000, so a list
#                  that long may be cut short and its "false" cannot be trusted
#   GO             the `go` filter: Go code, or anything a Go-heavy job reads
#   CROSS          the `cross` filter: what can break a platform's compile
#   E2E_CODE       the `e2e` filter: anything but docs (push to main only)
#   E2E_DOC        the one docs page the e2e runs as written
#
# Outputs:
#   go          the Go-heavy jobs (test-heap, build, images, examples, vuln,
#               pg-conformance) run
#   cross       the cross-platform compile runs
#   test-scope  all: the whole unit suite; readers: only the packages whose
#               tests read the repository outside Go code
#               (hack/test-readers.txt). The unit tests are never skipped: a
#               test that walks the checkout (internal/server decodes every
#               YAML file in it; internal/crd/apigroup reads every tracked
#               file) can fail on a change to any file, one no filter lists
#   e2e         the kind e2e and envtest jobs run
#   release     the release-check job runs
#   action      the action job runs
#
# The gates apply to pull requests only. A push to main keeps what it ran
# before this step existed: everything, but the kind e2e and envtest, which
# keep their old docs-only skip (the `e2e` filter). Every other event does not
# reach this job (it is skipped on a schedule, a dispatch and a release, whose
# jobs then run everything), and one that did would run everything too.
set -euo pipefail

# The most files the paths-filter action can be shown for a pull request.
# GitHub's API lists at most 3000; a count at or above it means the list the
# filters matched against may be missing files.
FILE_LIST_CAP=3000

for v in EVENT GO CROSS E2E_CODE E2E_DOC RELEASE ACTION; do
  [ -n "${!v:-}" ] || { echo "ci-changes: $v is not set" >&2; exit 1; }
  case $v in
    EVENT) ;;
    *) case ${!v} in true | false) ;; *) echo "ci-changes: $v is '${!v}', want true or false" >&2; exit 1 ;; esac ;;
  esac
done

# a || b over "true"/"false".
either() { if [ "$1" = true ] || [ "$2" = true ]; then echo true; else echo false; fi; }

case $EVENT in
  pull_request)
    case ${CHANGED_FILES:-} in
      '' | *[!0-9]*) echo "ci-changes: CHANGED_FILES is '${CHANGED_FILES:-}', want the pull request's file count" >&2; exit 1 ;;
    esac
    if [ "$CHANGED_FILES" -ge "$FILE_LIST_CAP" ]; then
      # A list cut short could hide the one file a job reads: run everything.
      go=true cross=true scope=all e2e=true release=true action=true
    else
      # A change that can break a platform's compile is Go code, so it is also
      # a change the Go jobs run for, whatever the filters say.
      go=$(either "$GO" "$CROSS")
      cross=$CROSS
      if [ "$go" = true ]; then scope=all; else scope=readers; fi
      e2e=$(either "$go" "$E2E_DOC")
      release=$RELEASE action=$ACTION
    fi
    ;;
  push)
    go=true cross=true scope=all
    e2e=$(either "$E2E_CODE" "$E2E_DOC")
    release=$RELEASE action=$ACTION
    ;;
  *)
    go=true cross=true scope=all e2e=true release=true action=true
    ;;
esac

echo "go=$go"
echo "cross=$cross"
echo "test-scope=$scope"
echo "e2e=$e2e"
echo "release=$release"
echo "action=$action"
