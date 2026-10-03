# Repository rulesets

GitHub does not read these files. They record this repository's rulesets,
so that the rulesets are reviewed like code and can be re-applied.
`release-tags.json` is applied; `major-tag.json` is not (see below). Apply
or update one with the GitHub API. You need admin on the repository.

```sh
# create
gh api -X POST repos/abd-ulbasit/upgradescope/rulesets --input .github/rulesets/release-tags.json
# update (the id is in: gh api repos/abd-ulbasit/upgradescope/rulesets)
gh api -X PUT repos/abd-ulbasit/upgradescope/rulesets/<id> --input .github/rulesets/release-tags.json
# check that the tag rules are active
gh api 'repos/abd-ulbasit/upgradescope/rulesets?targets=tag'
```

## `release-tags.json`: release tags are immutable

`refs/tags/v*` except `v0` cannot be deleted, moved (`update`) or
force-moved, by anyone. There are no bypass actors, admins included.

A release tag names the commit that the published archives, packages,
image, chart, checksums, signatures and provenance were built from.
`values.yaml` and the release notes tell users that a tag always means the
same bytes. The chart pins its image by digest as well. Before this
ruleset, a tag could be deleted and re-pushed at another commit, and a
re-run of the release would then publish different artifacts under the
same version (#127).

What this means for maintainers:

- A bad release is fixed forward with a new patch version. It is never
  re-tagged.
- Tag creation is not restricted. A mistyped tag that has not been released
  can only be removed by disabling the ruleset first, and that is visible in
  the audit log.
- The release workflow can still re-run on the same tag
  (`replace_existing_artifacts` in `.goreleaser.yml`). Turning on GitHub's
  *immutable releases* (Settings → General → Releases) also locks the
  assets of a published release, but it stops a half-failed release from
  being re-run in place. Make that trade-off before enabling it.

## The `main` ruleset: required checks

No JSON is kept for the `main` ruleset. It requires `ci-ok`, the one check
that aggregates the `ci.yml` jobs. `pr-lint.yml` is a separate workflow and
is not part of `ci-ok`, so a failing `pr-lint / breaking-change` check does
not block a merge on its own. To make the BREAKING CHANGE rule binding, add
`pr-lint / breaking-change` to the ruleset's required status checks
(Settings → Rules → the `main` ruleset → Require status checks to pass).
That is a setting for the repository admin; no file here applies it.

## `major-tag.json`: not applied

`v0` is the floating major tag that `uses: abd-ulbasit/upgradescope@v0`
resolves to. The `major-tag` job of `.github/workflows/release.yml` moves it
with the workflow's `GITHUB_TOKEN` once a stable release is verified.

`major-tag.json` names the GitHub Actions app (integration id 15368) as the
only actor that may move or delete `v0`. GitHub refused it on this
personal-account repository with 422: "Actor GitHub Actions integration
must be part of the ruleset source or owner organization". A ruleset on
`v0` with no bypass would also stop the release workflow, so none is
applied.

What this means today: no ruleset covers `v0`. Anyone with write access can
move or delete it, and so can any workflow here that runs with
`contents: write`. If that matters to you, pin the Action to a commit SHA,
or to a release tag, and set its `version` input to the same release (see
[Usage](../../action/README.md#usage)). Release tags stay put while
`release-tags.json` is active. Only a commit SHA does not depend on a
ruleset.

`major-tag.json` is kept for a repository owner that GitHub accepts it on,
such as an organization. Even there, it would block a direct tag push and
make every move of `v0` appear as a workflow run, but it would not narrow
who can move `v0`. The bypass belongs to the Actions app, not to one
workflow, and the `main` ruleset protects only the default branch. Anyone
with write access could push a branch with a workflow that has
`contents: write` and moves `v0`. To apply it there, run the create command
above with `--input .github/rulesets/major-tag.json` and the new owner in
the path.

A narrower option, not tried: give a deploy key or a dedicated GitHub App
installed on the repository the bypass, and have the `major-tag` job push
`v0` with that credential.

## When v1 ships

`v*` in `release-tags.json` also matches a future floating `v1`, which
would then be locked like a release tag and the release workflow could not
move it. Before the first v1 release:

1. add `refs/tags/v1` to the `exclude` list of `release-tags.json` and
   re-apply it;
2. if `major-tag.json` is applied by then, add `refs/tags/v1` to its
   `include` list and re-apply it too.
