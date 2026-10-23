# Runbook: pulling back a bad release

For maintainers. A published release is either **broken** (a regression,
a wrong knowledge-base entry: safe to run, wrong to recommend) or
**compromised** (its artifacts are not what the reviewed commit builds, or
contain something malicious: nobody should run them). The policy for both
is to fix forward with a new patch version, never to re-tag; this page is
what to do between finding out and that patch.

Why it takes several steps: release tags cannot be moved or deleted
(`.github/rulesets/release-tags.json`), Sigstore signatures and attestations
cannot be revoked (the Rekor log is append-only), GitHub's immutable
releases setting is off (so assets can be deleted, which a yank needs), and
four things follow GitHub's Latest release or the newest tag on their own:
the Action's `version: latest`, the image's `:latest`, the floating `v0`
tag, and the Homebrew tap.

`$BAD` is the bad tag (`v0.3.1`), `$GOOD` the release users should be on
(`v0.3.0`). All commands need the owner's `gh` login.

## 1. Stop new installs (both cases)

1. **GitHub's Latest** back to `$GOOD`, which is what `version: latest` and
   `releases/latest/download/...` resolve to:

        id=$(gh api repos/abd-ulbasit/upgradescope/releases/tags/$GOOD --jq .id)
        gh api -X PATCH repos/abd-ulbasit/upgradescope/releases/$id -f make_latest=true

2. **Mark `$BAD` a pre-release** with a warning at the top of its notes, so
   no tool picks it as the newest stable release again (the tap and
   `hack/release-newest.sh` skip pre-releases):

        gh release edit $BAD -R abd-ulbasit/upgradescope --prerelease \
          --notes-file <(printf '**Do not use: see <advisory link>.**\n\n'; gh release view $BAD -R abd-ulbasit/upgradescope --json body --jq .body)

3. **The image's `:latest`** back to `$GOOD`'s index (oras, pinned:
   `hack/install-tool.sh oras`, logged in to ghcr.io):

        digest=$(docker buildx imagetools inspect ghcr.io/abd-ulbasit/upgradescope:${GOOD#v} --format '{{json .Manifest}}' | jq -r .digest)
        oras tag ghcr.io/abd-ulbasit/upgradescope@$digest latest

4. **The floating `v0` tag** back to `$GOOD`'s commit (no ruleset covers
   `v0`; see `.github/rulesets/README.md`):

        sha=$(gh api repos/abd-ulbasit/upgradescope/commits/$GOOD --jq .sha)
        gh api -X PATCH repos/abd-ulbasit/upgradescope/git/refs/tags/v0 -f sha=$sha -F force=true

5. **Homebrew.** With Latest moved back, the tap's scheduled run refuses
   the downgrade and fails, on purpose. Roll it back deliberately: in
   abd-ulbasit/homebrew-tap, Actions → update formula → Run workflow, with
   tag `$GOOD` and `allow-downgrade` checked. Users who already upgraded get
   `$GOOD` on their next `brew upgrade`.

## 2. If it is compromised: remove the artifacts

A broken release keeps its assets (pinned users and reproducibility need
them). A compromised one must not be downloadable:

- delete every asset of `$BAD` (archives, packages, SBOMs, `checksums.txt`,
  its bundle) with `gh release delete-asset`, keeping the release page and
  its warning: the tag cannot be deleted, and the page is where users land;
- delete the image versions `${BAD#v}` and `$BAD` and the chart version
  `${BAD#v}` in the GHCR package settings (Packages → upgradescope and
  charts/upgradescope → versions), so pulls by tag and by digest fail;
  Artifact Hub drops the chart version on its next scan.

The signatures and attestations stay valid forever for what they signed,
and a verifier pinned to `release.yml@refs/tags/$BAD` still accepts them.
So the advisory must name the version and the sha256 of each bad artifact
(from that release's `checksums.txt`, saved before deleting it) and the
image digest, so users can check what they have.

## 3. Tell users

- A GitHub security advisory (Security → Advisories) for a compromise, or a
  pinned issue for a broken release, naming `$BAD`, what is affected, the
  digests, and the fix version once it exists.
- A CHANGELOG.md entry in the fix release.

## 4. Find out how it happened (compromise)

- Who created the tag, and from which commit: the release run's preflight
  log, `gh api repos/abd-ulbasit/upgradescope/commits/$BAD`, and whether it
  is on `main` (`gh api repos/abd-ulbasit/upgradescope/compare/<sha>...main`).
- Who approved the `release` environment deployment (Settings →
  Environments → release, and the run's deployment history).
- The organization or account audit log for ruleset, environment and
  token changes; revoke any token that could have pushed the tag or run
  the workflow.
- Every Sigstore certificate issued to the release identity: search the
  Rekor log for `https://github.com/abd-ulbasit/upgradescope/.github/workflows/release.yml@refs/tags/`
  (search.sigstore.dev), and `gh attestation` for attestations of this
  repository, to find anything else that was signed.

## 5. Fix forward

Release `$BAD`'s successor (`v0.3.2`) from a reviewed commit on `main` as
usual. It becomes Latest, `:latest` and `v0` on its own, and the tap moves
to it without `allow-downgrade`. Leave `$BAD` marked as a pre-release.
