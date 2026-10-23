# abd-ulbasit/homebrew-tap

Homebrew formulae for [upgradescope](https://github.com/abd-ulbasit/upgradescope),
the continuous Kubernetes upgrade-readiness scanner.

```sh
brew install abd-ulbasit/tap/upgradescope
upgradescope version
```

The formula installs the `upgradescope` binary for macOS and Linux (amd64 and
arm64), its bash, zsh and fish completions, and its man pages
(`man upgradescope`, `man upgradescope-scan`, ...).

## How the formula stays current

No upgradescope workflow can write to this repository, and this repository
holds no secret from upgradescope. Updates are pulled, not pushed.
[`.github/workflows/update.yml`](.github/workflows/update.yml) runs every 6
hours and on demand (Actions → update formula → Run workflow, optionally
with a tag). Each run:

1. reads the latest stable upgradescope release. Pre-releases are skipped.
   A release older than the version the formula already ships is refused,
   so GitHub's Latest moved back (by hand, or by a mistake) never
   downgrades `brew upgrade`. To roll back on purpose, run the workflow by
   hand with the tag and `allow-downgrade` checked.
2. resolves the tag to its commit and refuses it unless that commit is on
   upgradescope's `main` (GitHub's compare API says `main` is ahead of it
   or identical). A release tag can be pushed on any commit, and the
   release workflow's own "the tagged commit is on main" check runs from
   that commit, so the tap checks again from outside.
3. downloads that release's `checksums.txt` and `checksums.txt.sigstore.json`.
4. verifies the bundle with cosign, keylessly. The certificate must have
   been issued to
   `https://github.com/abd-ulbasit/upgradescope/.github/workflows/release.yml@refs/tags/<tag>`
   by `https://token.actions.githubusercontent.com`, for a run at the
   commit from step 2 (`--certificate-github-workflow-sha`). If anything
   does not verify, the run fails and the formula is left unchanged.
5. re-renders `Formula/upgradescope.rb` from
   [`formula.rb.tmpl`](formula.rb.tmpl) with
   [`script/render-formula.sh`](script/render-formula.sh). The run commits
   only when the formula changed, with this repository's own `GITHUB_TOKEN`.
6. installs and tests the formula on macOS and Linux with `brew install` and
   `brew test`. `brew test` runs `upgradescope version`.

So a new upgradescope release reaches `brew upgrade` within about 6 hours.

To render by hand, for example to test a template change:

```sh
script/render-formula_test.sh                       # offline, v0.1.1 fixture
script/update_test.sh                               # offline, stub gh, curl and cosign
UPGRADESCOPE_TAG=v0.2.0 GH_TOKEN=... script/update.sh   # needs cosign
brew style Formula/upgradescope.rb
```

Edit `formula.rb.tmpl`, never `Formula/upgradescope.rb`. The next run
overwrites the formula.

### Before the first signed release

The tap starts with no formula: `Formula/upgradescope.rb` does not exist
until the first run that verifies a release creates it. Releases up to
v0.1.1 were published before upgradescope's release workflow signed
anything, so `update.sh` cannot verify them and refuses them. Until the
first signed release (v0.2.0) is out, scheduled runs fail with "has no
checksums.txt.sigstore.json" and `brew install abd-ulbasit/tap/upgradescope`
finds no formula. Push this repository only after v0.2.0 is published, so
its first run creates the formula. The v0.1.1 `checksums.txt` in
`script/testdata/` is only a fixture for `script/render-formula_test.sh`.
