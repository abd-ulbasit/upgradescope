#!/usr/bin/env bash
# Tests ci.yml's workflow-level concurrency (run by `make hack-test`).
# GitHub applies a concurrency group across the whole repository, and a
# PENDING run is cancelled whenever another run joins its group, even with
# cancel-in-progress: false. When release.yml calls ci.yml, `github.*` is the
# tag push's context (event push, the tagged sha), so a group keyed only by
# event and sha puts the release's CI in main's group for the same commit:
# re-running main's kind e2e, or tagging rc.1 and final on one commit,
# cancels it and nothing is published. This evaluates the group and
# cancel-in-progress expressions read from ci.yml for each kind of run and
# asserts which runs may share a group. Offline; needs node (the expressions
# use only ==, && and ||, which JavaScript evaluates the same way).
set -euo pipefail
cd "$(dirname "$0")/.."

command -v node >/dev/null || { echo "ci-concurrency_test: node is required" >&2; exit 1; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# The top-level block only: `concurrency:` at column 0, its keys indented two.
ci=.github/workflows/ci.yml
group=$(awk '/^concurrency:/{c=1;next} c&&/^[^ ]/{c=0} c&&/^  group:/{sub(/^  group: */,"");print}' "$ci")
cancel=$(awk '/^concurrency:/{c=1;next} c&&/^[^ ]/{c=0} c&&/^  cancel-in-progress:/{sub(/^  cancel-in-progress: */,"");sub(/ *#.*/,"");print}' "$ci")
[ -n "$group" ] && [ -n "$cancel" ] || { echo "FAIL no top-level concurrency group/cancel-in-progress in $ci" >&2; exit 1; }

# render <template> <event> <ref> <sha> [pr-number]: the template with every
# ${{ expr }} evaluated against that github context.
render() {
  node -e '
    const [tmpl, event_name, ref, sha, pr] = process.argv.slice(1);
    const github = { event_name, ref, sha,
      event: { pull_request: { number: pr ? Number(pr) : null } } };
    process.stdout.write(tmpl.replace(/\$\{\{(.*?)\}\}/g, (_, e) => {
      const v = new Function("github", "return (" + e + ");")(github);
      return v === null || v === undefined ? "" : String(v);
    }));
  ' "$1" "$2" "$3" "$4" "${5:-}"
}

# The runs that matter. sha A is both main's head and the tagged commit (the
# usual merge-then-tag flow); release.yml's workflow_call runs carry the
# caller's event, so a tag run is event push on refs/tags/*. botapi and
# botreg are ci.yml dispatched by hand on kb-refresh's bot branches.
A=1111111111111111111111111111111111111111
B=2222222222222222222222222222222222222222
runs="pr7a     pull_request      refs/pull/7/merge   $A 7
pr7b     pull_request      refs/pull/7/merge   $B 7
pr8      pull_request      refs/pull/8/merge   $A 8
mainA    push              refs/heads/main     $A
mainB    push              refs/heads/main     $B
sched    schedule          refs/heads/main     $A
dispatch workflow_dispatch refs/heads/main     $A
botapi   workflow_dispatch refs/heads/bot/kb-refresh-api $A
botreg   workflow_dispatch refs/heads/bot/kb-refresh-registry $B
rc1      push              refs/tags/v0.2.0-rc.1 $A
final    push              refs/tags/v0.2.0    $A
redo     workflow_dispatch refs/tags/v0.2.0    $A"

# check <group-template> <cancel-template>: prints one line per failed
# assertion; empty output means the pair is safe. Rendered values go to
# files, not an associative array: macOS still ships bash 3.2.
check() {
  local g=$1 c=$2 name ev ref sha pr
  rm -rf "$work/r" && mkdir "$work/r"
  while read -r name ev ref sha pr; do
    render "$g" "$ev" "$ref" "$sha" "$pr" >"$work/r/$name.group"
    render "$c" "$ev" "$ref" "$sha" "$pr" >"$work/r/$name.cancel"
  done <<<"$runs"

  # A PR's superseded run is cancelled; nothing else ever is.
  [ "$(G pr7a)" = "$(G pr7b)" ] || echo "a new push to PR 7 does not share its group ($(G pr7a) vs $(G pr7b))"
  [ "$(C pr7a)" = true ] || echo "a PR run is not cancel-in-progress ($(C pr7a))"
  for n in mainA sched dispatch botapi botreg rc1 final redo; do
    [ "$(C "$n")" = false ] || echo "$n is cancel-in-progress ($(C "$n"))"
  done
  # No two of these may share a group: each would cancel the other while it
  # is pending. Above all, no tag run may share one with main or another tag.
  local names=(pr7a pr8 mainA mainB sched dispatch botapi botreg rc1 final redo) i j
  for ((i = 0; i < ${#names[@]}; i++)); do
    for ((j = i + 1; j < ${#names[@]}; j++)); do
      [ "$(G "${names[i]}")" != "$(G "${names[j]}")" ] ||
        echo "${names[i]} and ${names[j]} share group $(G "${names[i]}")"
    done
  done
}
G() { cat "$work/r/$1.group"; }
C() { cat "$work/r/$1.cancel"; }

fails=0
check "$group" "$cancel" >"$work/out"
if [ -s "$work/out" ]; then
  echo "FAIL $ci concurrency (group: $group):" >&2
  sed 's/^/     /' "$work/out" >&2
  fails=1
else
  echo "ok   $ci concurrency: PR runs supersede; main, schedule, dispatch (bot branches too) and tag runs never share a group"
fi

# The check itself must catch the regression it exists for: the event+sha
# group put a tag's release CI in main's group for the same commit.
check 'ci-${{ github.event_name }}-${{ github.event.pull_request.number || github.sha }}' "$cancel" >"$work/out"
if grep -q '^mainA and rc1 share group' "$work/out" && grep -q '^rc1 and final share group' "$work/out"; then
  echo "ok   the event+sha group is rejected (tag run shares main's group)"
else
  echo "FAIL the event+sha group was not rejected:" >&2
  sed 's/^/     /' "$work/out" >&2
  fails=1
fi

exit "$fails"
