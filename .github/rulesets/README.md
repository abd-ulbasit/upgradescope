# Repository rulesets

GitHub does not read these files. They record this repository's rulesets,
so that the rulesets are reviewed like code and can be re-applied, and
`hack/rulesets-live.sh` checks that the rulesets applied on GitHub equal them.

| File | State | Ruleset id |
|---|---|---|
| `release-tags.json` | applied (2026-10-11) | 24349764 |
| `main.json` | applied (2026-10-11); the `ci-ok` pin (`integration_id` 15368) awaits a re-apply with the PUT below | 24343445 |
| `release-tag-creation.json` | not yet applied: create it with the POST below | none yet |
| `major-tag.json` | not applied (see below) | none |

Updating or checking one needs the GitHub API. Writing needs admin on the
repository; reading does not.

```sh
# update an applied ruleset from its file (PUT replaces it as a whole)
gh api -X PUT repos/abd-ulbasit/upgradescope/rulesets/24349764 --input .github/rulesets/release-tags.json
gh api -X PUT repos/abd-ulbasit/upgradescope/rulesets/24343445 --input .github/rulesets/main.json
# create a ruleset that does not exist yet (then note its id in the table)
gh api -X POST repos/abd-ulbasit/upgradescope/rulesets --input .github/rulesets/release-tag-creation.json
# check that the tag rules are active
gh api 'repos/abd-ulbasit/upgradescope/rulesets?targets=tag'
# check what a branch is bound by (the required checks of main)
gh api repos/abd-ulbasit/upgradescope/rules/branches/main
# check that every applied ruleset equals its file (read-only)
make rulesets-check
```

A POST (`gh api -X POST repos/abd-ulbasit/upgradescope/rulesets --input
<file>`) only creates a ruleset, on a repository that has none with that
name. For the two applied files it fails with 422 "Name must be unique",
because they exist: that error means use PUT with the id above. (The ids are
also in `gh api repos/abd-ulbasit/upgradescope/rulesets`.)

## Checking the live rulesets

(How this could become a plan-and-apply workflow instead:
`docs/design/repository-settings-as-code.md`.)

`hack/rulesets-live.sh` (`make rulesets-check`) reads the live rulesets with
`gh api` (GET only; it never changes one) and compares each applied file
(`main.json`, `release-tags.json` and `release-tag-creation.json`, never
`major-tag.json`) with the live ruleset of the same name: `target`, `enforcement`, `conditions` and `rules`.
The rules are compared as a set with their parameters in full, so the order
the API returns them in does not matter, and a required check that lost its
`integration_id` does. A missing ruleset or any difference fails, with a diff
naming the file and the field. CI's `repo-checks` job runs it on every pull
request and push, and not on a release, which depends on the tagged code and
never on live repository state. If the API cannot be read after two retries,
it fails rather than skips.

`bypass_actors` is compared only when the live answer includes it, and
GitHub shows it to repository admins only: the CI token does not get it, so
CI does not compare it. Run `make rulesets-check` locally, logged in as the
owner, to compare it too (it was on 2026-10-11).

After an intended change in the UI, copy the new state into the file in the
same pull request: `gh api repos/abd-ulbasit/upgradescope/rulesets/<id>`,
without the `id`, `node_id`, `source*`, `created_at`, `updated_at`,
`current_user_can_bypass` and `_links` fields, formatted like the files here.
After a change to a file, apply it with the PUT above. Until then the
check fails on that pull request and on every one after it, which is the
point; so apply a file change right after it merges, or just before, with
the pull request's check re-run.

## `release-tags.json`: release tags are immutable

Every release tag, `vX.Y.Z` or `vX.Y.Z-pre` (`refs/tags/v*.*.*`), cannot
be deleted, moved (`update`) or force-moved, by anyone. There are no bypass
actors, admins included.

The include pattern needs two dots, so it never matches a floating major
tag (`v0`, `v1`, ... `v12`): those have none, and the release workflow's
`major-tag` job must be able to move them. `hack/ruleset-tags_test.sh`
(`make hack-test`) checks that every tag `release.yml` releases is covered,
and that the major tag `major-tag` derives from each (`${tag%%.*}`) is not.
Before #244 the ruleset covered `refs/tags/v*` and excluded only `v0`, so
the first v1.x release would have created `v1` and the next one could not
move it.

A release tag names the commit that the published archives, packages,
image, chart, checksums, signatures and provenance were built from.
`values.yaml` and the release notes tell users that a tag always means the
same bytes. The chart pins its image by digest as well. Before this
ruleset, a tag could be deleted and re-pushed at another commit, and a
re-run of the release would then publish different artifacts under the
same version (#127).

What this means for maintainers:

- A bad release is fixed forward with a new patch version. It is never
  re-tagged. Until the fix is out, `docs/maintainers/yanking-a-release.md`
  says how to move Latest, `:latest`, `v0` and the Homebrew tap back, and,
  for a compromised release, how to remove its artifacts.
- Who may create a release tag is restricted by `release-tag-creation.json`
  (below), not by this ruleset. A mistyped tag that has not been released
  can only be removed by disabling this ruleset first, and that is visible
  in the audit log.
- The release workflow can still re-run on the same tag
  (`replace_existing_artifacts` in `.goreleaser.yml`). Turning on GitHub's
  *immutable releases* (Settings → General → Releases) also locks the
  assets of a published release, but it stops a half-failed release from
  being re-run in place. Make that trade-off before enabling it.

## `release-tag-creation.json`: only admins create release tags

A release tag is what makes a release: pushing `vX.Y.Z` runs
`release.yml` from the tagged commit, and everything that run signs carries
the identity `release.yml@refs/tags/vX.Y.Z`. That identity does not say
which branch the commit is on. The preflight's "the tagged commit is on
main" check (`hack/check-release-ancestry.sh`) runs inside the tagged
commit's own `release.yml`, so a tag on an unmerged branch commit whose
`release.yml` drops that step would still produce a validly signed release.

This ruleset closes the first half of that: on `refs/tags/v*.*.*` it has a
single `creation` rule, which only the repository's admin role
(`RepositoryRole` 5, the owner) may bypass. A collaborator with write
access, a workflow's `GITHUB_TOKEN` with `contents: write`, or a deploy key
cannot create a release tag. It is a separate ruleset because bypass is
per ruleset: an admin bypass on `release-tags.json` would also let admins
move or delete released tags.

What it does not cover:

- A token with the owner's rights (an AI agent's `gh auth`, a leaked
  personal access token) is the admin role and can still create a tag. The
  `release` environment's required reviewer (`release.yml`'s `goreleaser`
  job) is the second gate for that case, as long as the tagged
  `release.yml` keeps referencing it.
- It cannot make a tag's commit be on `main`. The Homebrew tap therefore
  checks that itself before it publishes a release: the signed workflow
  commit must be an ancestor of `main`
  (`packaging/homebrew-tap/script/update.sh`).
- Floating major tags (`v0`) are not covered (no dots); see `major-tag.json`.

## `main.json`: the `main` ruleset

It applies to the default branch (`~DEFAULT_BRANCH`) and forbids deleting
it and force-pushing it. A change goes through a pull request (squash or
merge; no approving review is required), and two status checks must pass:

- **The required checks.** `ci-ok`, the one check that aggregates the
  `ci.yml` jobs, and `breaking-change`, the check of `pr-lint.yml`. A PR with
  a `BREAKING CHANGE:` footer but no `type!:` subject, or an uppercase type,
  cannot merge (`hack/check-breaking.sh`; IR-06). `pr-lint.yml` is a separate
  workflow and not part of `ci-ok`, which is why it is required on its own.
- **The context.** It is the check-run (job) name. The pull request page
  shows `pr-lint / breaking-change` (workflow / job), but the context a
  ruleset matches is `breaking-change`, the job id in `pr-lint.yml`. Writing
  `pr-lint / breaking-change` there would never be satisfied.
- **The pin.** `breaking-change` is pinned to `integration_id` 15368, the
  GitHub Actions app, so a commit status or another app's check with that
  name does not satisfy it. `ci-ok` is pinned to the same app in the file;
  until that is applied with the PUT above, the live ruleset accepts any
  status or check named `ci-ok` (a commit status any token with
  `statuses: write` can post), and `make rulesets-check` fails.
- **Bypass.** The repository's admin role (`RepositoryRole` 5) can bypass the
  ruleset, always. That is the owner of this personal repository; nobody else
  can.

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
`contents: write` and moves `v0`. To apply it there, create it with the POST
above, with `--input .github/rulesets/major-tag.json` and the new owner in
the path (the repository has no ruleset of that name, so POST is right).

A narrower option, not tried: give a deploy key or a dedicated GitHub App
installed on the repository the bypass, and have the `major-tag` job push
`v0` with that credential.

## When v1 ships

`release-tags.json` needs no change: `refs/tags/v*.*.*` never matches `v1`.
If `major-tag.json` is applied by then, add `refs/tags/v1` to its `include`
list and re-apply it.
