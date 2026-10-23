#!/usr/bin/env bash
# Checks that the `release` environment, which release.yml's goreleaser job
# waits on before anything is published, holds every release for a reviewer
# (make release-environment-check; CI's repo-checks job on pull requests and
# pushes, not on a release). Its protection rules are repository settings
# that no file here sets, and GitHub creates an environment a job names, with
# no protection at all, the first time a job uses it. Without this check, a
# missing or reviewer-less `release` environment would let the next tag
# publish with no approval while release.yml, the CHANGELOG and the security
# model page say a release waits for one.
#
# Read-only: it only GETs `repos/$REPO/environments/release` and its
# `deployment-branch-policies` (the job's github.token needs `actions: read`
# for them). It never changes the environment. It fails when:
#   - there is no environment named `release`;
#   - it has no `required_reviewers` rule naming at least one reviewer;
#   - its deployments are not limited to selected refs, or those refs are
#     not exactly the tag pattern `v*.*.*` (a branch could then ask for the
#     approval a release tag gets).
# A GET that fails, other than a 404 for the environment, is retried twice;
# if the API still cannot be read it fails closed (it never skips).
#
# What it does not check: who the reviewers are, "Prevent self-review" (keep
# it off while one maintainer both tags and approves, or no release can be
# approved), and whether admins may bypass the rules. Setting the environment
# up: packaging/README.md.
#
# Knobs: RELEASE_ENV_REPO (default $GITHUB_REPOSITORY, else
# abd-ulbasit/upgradescope), RELEASE_ENV_GH (the gh command),
# RELEASE_ENV_RETRY_SLEEP (seconds between attempts, default 2).
set -euo pipefail

repo=${RELEASE_ENV_REPO:-${GITHUB_REPOSITORY:-abd-ulbasit/upgradescope}}
gh_cmd=${RELEASE_ENV_GH:-gh}
sleep_s=${RELEASE_ENV_RETRY_SLEEP:-2}
env_name=release
tag_pattern='v*.*.*'
me=release-environment-live

command -v jq >/dev/null 2>&1 || { echo "$me: jq is not installed" >&2; exit 1; }
command -v "$gh_cmd" >/dev/null 2>&1 || { echo "$me: gh is not installed, so the environment cannot be read" >&2; exit 1; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# api <path> <outfile>: GET, retried twice; returns 2 on a 404 (not retried)
# and 1 when the API cannot be read.
api() {
  local path=$1 out=$2 attempt
  for attempt in 1 2 3; do
    if "$gh_cmd" api "$path" >"$out" 2>"$work/err"; then return 0; fi
    if grep -q 'HTTP 404' "$work/err"; then return 2; fi
    [ "$attempt" = 3 ] || sleep "$sleep_s"
  done
  echo "$me: could not read $path ($repo): $(tr '\n' ' ' <"$work/err")" >&2
  return 1
}

rc=0
api "repos/$repo/environments/$env_name" "$work/env.json" || rc=$?
if [ "$rc" = 2 ]; then
  echo "$me: $repo has no environment named \"$env_name\". release.yml's goreleaser job names it, and GitHub creates it with no protection on first use, so the next tag would publish without approval. Create it with a required reviewer and the deployment tag pattern $tag_pattern (packaging/README.md)." >&2
  exit 1
fi
[ "$rc" = 0 ] || exit 1
jq -e 'type == "object" and has("protection_rules")' "$work/env.json" >/dev/null 2>&1 ||
  { echo "$me: the environment \"$env_name\" of $repo is not a JSON object with protection_rules: $(head -c 300 "$work/env.json")" >&2; exit 1; }

bad=0
reviewers=$(jq '[.protection_rules[]? | select(.type == "required_reviewers") | .reviewers[]?] | length' "$work/env.json")
if [ "$reviewers" -lt 1 ]; then
  echo "$me: the \"$env_name\" environment of $repo has no required reviewer, so a release publishes without approval (Settings -> Environments -> $env_name -> Required reviewers)" >&2
  bad=$((bad + 1))
fi

if ! jq -e '.deployment_branch_policy.custom_branch_policies == true' "$work/env.json" >/dev/null; then
  echo "$me: the \"$env_name\" environment of $repo does not limit deployments to selected tags (deployment_branch_policy: $(jq -c '.deployment_branch_policy' "$work/env.json")); select the tag pattern $tag_pattern only" >&2
  bad=$((bad + 1))
else
  rc=0
  api "repos/$repo/environments/$env_name/deployment-branch-policies?per_page=100" "$work/policies.json" || rc=$?
  if [ "$rc" != 0 ]; then
    [ "$rc" != 2 ] || echo "$me: could not read the deployment policies of \"$env_name\" ($repo): 404" >&2
    exit 1
  fi
  jq -e '.branch_policies | type == "array"' "$work/policies.json" >/dev/null 2>&1 ||
    { echo "$me: the deployment policies of \"$env_name\" ($repo) are not a list: $(head -c 300 "$work/policies.json")" >&2; exit 1; }
  got=$(jq -c '[.branch_policies[] | {name, type: (.type // "branch")}] | sort' "$work/policies.json")
  want=$(jq -nc --arg p "$tag_pattern" '[{name: $p, type: "tag"}]')
  if [ "$got" != "$want" ]; then
    echo "$me: the \"$env_name\" environment of $repo deploys from $got, want only the tag pattern $want" >&2
    bad=$((bad + 1))
  fi
fi

if [ "$bad" -gt 0 ]; then
  echo "$me: $bad problem(s) with the \"$env_name\" environment of $repo; a release would not wait for a reviewer as release.yml says (packaging/README.md)" >&2
  exit 1
fi
echo "$me: $repo: the \"$env_name\" environment requires a reviewer ($reviewers named) and deploys only tags $tag_pattern"
