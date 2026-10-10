#!/usr/bin/env bash
# The decision step of ci.yml's changes job: turns what the path filters say
# about a pull request into the outputs the gated jobs read and ci-ok checks
# (hack/ci-ok.sh). Prints name=value lines for $GITHUB_OUTPUT.
#
# Inputs, from the environment, each "true" or "false" (anything else, or
# nothing, fails the step, and a failed changes job fails ci-ok):
#   EVENT     github.event_name
#   GO        the `go` filter: Go code, or anything a Go job reads
#   DOCS      the `docs` filter: documentation some Go test reads
#   CROSS     the `cross` filter: what can break a platform's compile
#   E2E_CODE  the `e2e` filter: anything but docs (push to main only)
#   E2E_DOC   the one docs page the e2e runs as written
#
# Outputs:
#   go          the Go-heavy jobs (test-heap, build, images, examples, vuln,
#               pg-conformance) run
#   cross       the cross-platform compile runs
#   test-scope  all: the whole unit suite; docs: only the packages that read
#               documentation (hack/test-docs-readers.txt); none: neither
#   e2e         the kind e2e and envtest jobs run
#
# The gates apply to pull requests only. A push to main keeps what it ran
# before this step existed: everything, but the kind e2e and envtest, which
# keep their old docs-only skip (the `e2e` filter). Every other event does not
# reach this job (it is skipped on a schedule, a dispatch and a release, whose
# jobs then run everything), and one that did would run everything too.
set -euo pipefail

for v in EVENT GO DOCS CROSS E2E_CODE E2E_DOC; do
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
    # A change that can break a platform's compile is Go code, so it is also
    # a change the Go jobs run for, whatever the filters say.
    go=$(either "$GO" "$CROSS")
    cross=$CROSS
    if [ "$go" = true ]; then
      scope=all
    elif [ "$DOCS" = true ]; then
      scope=docs
    else
      scope=none
    fi
    e2e=$(either "$go" "$E2E_DOC")
    ;;
  push)
    go=true cross=true scope=all
    e2e=$(either "$E2E_CODE" "$E2E_DOC")
    ;;
  *)
    go=true cross=true scope=all e2e=true
    ;;
esac

echo "go=$go"
echo "cross=$cross"
echo "test-scope=$scope"
echo "e2e=$e2e"
