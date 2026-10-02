# Packaging and distribution

upgradescope is published through every channel below from one place:
`.github/workflows/release.yml`, on a `vX.Y.Z` tag. This page lists what is
automated, how it is verified, and the one-time manual steps that no
workflow can do.

| Channel | Built by | Verified by |
| --- | --- | --- |
| Archives, linux/darwin/windows × amd64/arm64 (binary, completions, man pages, LICENSE, NOTICE, THIRD_PARTY_NOTICES) | GoReleaser (`.goreleaser.yml`) | `checksums.txt` + keyless cosign bundle, SBOM per archive, GitHub build provenance |
| deb / rpm / apk packages (linux amd64/arm64) | GoReleaser `nfpms` | covered by `checksums.txt` |
| Image `ghcr.io/abd-ulbasit/upgradescope` (licenses in `/licenses/`) | GoReleaser `dockers_v2` | cosign on the index and every platform image, SBOM and provenance attestations |
| Helm chart `oci://ghcr.io/abd-ulbasit/charts/upgradescope` (image pinned by digest) | release.yml `chart` job | cosign |
| Homebrew `abd-ulbasit/tap/upgradescope` | the tap's own workflow ([homebrew-tap/](homebrew-tap/)) | the tap verifies `checksums.txt` with cosign before rendering |
| krew `kubectl upgradescope` | release.yml `krew` job (krew-release-bot, from [`.krew.yaml`](../.krew.yaml)) | krew-index CI |
| Artifact Hub listing of the chart | release.yml pushes [artifacthub/artifacthub-repo.yml](artifacthub/artifacthub-repo.yml) | Artifact Hub |
| `go install …/cmd/upgradescope@vX.Y.Z` | the Go module proxy | ci.yml `build` job (`make go-install-check`) |

Local checks:

```sh
make release-check   # snapshot of every archive, package and image, contents asserted
make release-repro   # two snapshots from fresh clones must be byte-identical
make notices-check   # THIRD_PARTY_NOTICES current, every dependency license allowed
make docs            # completions and man pages into packaging/generated/
make hack-test       # offline tests of the scripts, the formula renderer included
```

## One-time manual steps

The orchestrator or the maintainer does these once. Each one needs an
account action that a workflow cannot do.

### After the first release (v0.2.0)

1. **Make the GHCR packages public.** GitHub creates `upgradescope` and
   `charts/upgradescope` as private packages. Set both to public under
   Package settings → Danger Zone → Change visibility. Then re-run the
   release's `verify` job, which pulls them anonymously.
2. **Apply the tag rulesets** in [`.github/rulesets/`](../.github/rulesets/).
   That README has the commands.

### Homebrew tap

Only after v0.2.0 is published: it is the first signed release, and the tap
ships nothing it cannot verify. The tap's files include no formula; its
first run creates `Formula/upgradescope.rb`.

1. Create the public repository `abd-ulbasit/homebrew-tap`.
2. Push the contents of [`homebrew-tap/`](homebrew-tap/) to its default
   branch.
3. Run its *update formula* workflow once (Actions → Run workflow). It
   verifies the latest signed release and commits the formula. After that
   it runs every 6 hours.

The tap holds no upgradescope secret, and upgradescope holds no tap
secret. The tap pulls and verifies.

### krew

krew-release-bot only *updates* plugins that are already in krew-index, so
the first submission is a manual pull request:

1. Render the manifest for the release. Install the bot binary (or run its
   image) and run
   `krew-release-bot template --tag vX.Y.Z --template-file .krew.yaml > upgradescope.yaml`.
   [`krew/krew_test.go`](krew/krew_test.go) shows exactly what the bot
   renders.
2. Test it: `kubectl krew install --manifest=upgradescope.yaml`, then
   `kubectl upgradescope version`.
3. Open a PR to [kubernetes-sigs/krew-index](https://github.com/kubernetes-sigs/krew-index)
   that adds `plugins/upgradescope.yaml`, following its
   [plugin naming](https://krew.sigs.k8s.io/docs/developer-guide/develop/naming-guide/)
   and [release guidelines](https://krew.sigs.k8s.io/docs/developer-guide/release/new-plugin/).
4. Once the PR merges, set the repository variable
   `KREW_PLUGIN_IN_INDEX=true` (Settings → Secrets and variables → Actions →
   Variables). From then on, every stable release opens the version-bump PR
   automatically.

### Artifact Hub

1. Sign in at [artifacthub.io](https://artifacthub.io). Under Control Panel
   → Repositories → Add, choose kind *Helm charts* and URL
   `oci://ghcr.io/abd-ulbasit/charts/upgradescope`.
2. Copy the repository ID that Artifact Hub shows into `repositoryID` in
   [`artifacthub/artifacthub-repo.yml`](artifacthub/artifacthub-repo.yml)
   and commit it. The next release pushes it to the `artifacthub.io` tag,
   and Artifact Hub then shows the repository as a Verified Publisher.

The chart's `artifacthub.io/*` annotations (`deploy/chart/Chart.yaml`) supply
the license, links, CRD, images, prerelease flag and per-release changes.
At package time, `hack/chart-release-annotations.sh` stamps the image digest
and the changes from `CHANGELOG.md`. Artifact Hub detects the cosign
signature of the OCI chart by itself. The `artifacthub.io/signKey`
annotation is not used: it describes a PGP or key-based provenance key, and
this chart is signed keylessly.

### OpenSSF Scorecard

`.github/workflows/scorecard.yml` publishes results to the OpenSSF API on
every push to `main` and weekly. After its first run, the badge
`https://api.scorecard.dev/projects/github.com/abd-ulbasit/upgradescope/badge`
links to `https://scorecard.dev/viewer/?uri=github.com/abd-ulbasit/upgradescope`.
