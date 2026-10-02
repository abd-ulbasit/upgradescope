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
2. downloads that release's `checksums.txt` and `checksums.txt.sigstore.json`.
3. verifies the bundle with cosign, keylessly. The certificate must have
   been issued to
   `https://github.com/abd-ulbasit/upgradescope/.github/workflows/release.yml@refs/tags/<tag>`
   by `https://token.actions.githubusercontent.com`. If anything does not
   verify, the run fails and the formula is left unchanged.
4. re-renders `Formula/upgradescope.rb` from
   [`formula.rb.tmpl`](formula.rb.tmpl) with
   [`script/render-formula.sh`](script/render-formula.sh). The run commits
   only when the formula changed, with this repository's own `GITHUB_TOKEN`.
5. installs and tests the formula on macOS and Linux with `brew install` and
   `brew test`. `brew test` runs `upgradescope version`.

So a new upgradescope release reaches `brew upgrade` within about 6 hours.

To render by hand, for example to test a template change:

```sh
script/render-formula_test.sh                       # offline, v0.1.1 fixture
UPGRADESCOPE_TAG=v0.2.0 GH_TOKEN=... script/update.sh   # needs cosign
brew style Formula/upgradescope.rb
```

Edit `formula.rb.tmpl`, never `Formula/upgradescope.rb`. The next run
overwrites the formula.

### The first formula

The initial `Formula/upgradescope.rb` was rendered from the `checksums.txt`
of v0.1.1. That release was published before upgradescope's release workflow
signed anything, so `update.sh` cannot verify it and refuses it. The first
signed release (v0.2.0) replaces that formula on the next scheduled run.
Until then, scheduled runs fail with "has no checksums.txt.sigstore.json".
