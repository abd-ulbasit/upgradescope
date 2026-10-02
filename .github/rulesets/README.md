# Repository rulesets

GitHub does not read these files. They record the rulesets applied to this
repository, so that the rulesets are reviewed like code and can be
re-applied. Apply or update one with the GitHub API. You need admin on the
repository.

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

## `major-tag.json`: `v0` moves only through the release workflow

`v0` is the floating major tag that `uses: abd-ulbasit/upgradescope@v0`
resolves to. The `major-tag` job of `.github/workflows/release.yml` moves it
with the workflow's `GITHUB_TOKEN` once a stable release is verified. The
only bypass actor is the GitHub Actions app (integration id 15368), so a
person cannot move or delete `v0` by hand.
