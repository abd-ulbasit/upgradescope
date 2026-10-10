# upgradescope GitHub Action

Gates a pull request on Kubernetes upgrade readiness. The action scans
rendered manifests (`helm template`, `kustomize build`, plain YAML) for APIs
that are removed or deprecated at a target Kubernetes version. It fails the
step when findings reach `fail-on`, and it reports in three places:

- **Step summary.** The job's summary page gets a table with one row per
  finding: severity, title, the objects as `file:line`, and the fix. This
  happens whether the gate passes or fails. Findings an ignore rule
  suppressed get a table of their own, with the reason, and against a
  baseline each finding is marked new or unchanged, so the summary says
  why a gate passed.
- **Job log and annotations.** The log gets the same tables. Each finding
  that fails the gate at your `fail-on` becomes an `::error` annotation, and
  every other blocker or warning a `::warning`. Annotations sit at the
  finding's first file and line. With `fail-on: never`, nothing fails the
  gate, so every annotation is a warning. A finding the baseline already
  had never fails the gate: its `::warning` title says "in baseline".
  Suppressed findings are not annotated.
- **SARIF and outputs.** The SARIF file is complete even when the gate fails,
  so you can upload it to code scanning. When the scan itself fails (exit
  1) or is killed (any exit but 0 or 2), there is no complete SARIF and
  `sarif-file` is not set. The action also sets the verdict, score and
  counts as outputs for later steps.

## Usage

Pick how the gate moves:

- **`@v0` follows the newest v0.x release.** The release workflow moves the
  `v0` tag to a stable release once it is published and verified (from
  v0.2.0 on), and only when no higher v0.x release is published: a re-run
  of an older release, or the slower of two releases cut close together,
  leaves `v0`, GitHub's latest release and the image's `:latest` on the
  higher one. GitHub's latest moves to a release only after that
  release's provenance attestation exists (the release workflow, not
  GoReleaser, sets it), so a `latest` install never meets a release that
  cannot yet be verified; if the attestation step fails, latest stays on the
  previous release. Since `v0` is not a release tag, the default `version`
  is `latest`, and the binary and its knowledge base move with it. This is
  the convenient choice, but a release can change the verdict on an
  unchanged pull request.
- **`@vX.Y.Z` or a commit SHA, plus `version`, for a reproducible gate.**
  Pin the action to a release tag or, stricter, the tag's full commit SHA,
  and set `version` to the same release. With `version` unset, an action at
  a release tag (`@vX.Y.Z` or `@vX.Y.Z-rc.N`) runs that same release, so
  `@v0.2.0-rc.2` runs v0.2.0-rc.2. At a full 40-character commit SHA, the
  action looks the commit up with
  `git ls-remote --tags https://github.com/abd-ulbasit/upgradescope`
  (an annotated tag counts at the commit it points at) and runs the release
  tag, `vX.Y.Z` or `vX.Y.Z-rc.N`, that points at it. If several do, it
  tries them newest first and runs the first whose release is published
  (a tag exists before its release does, and an older release candidate at
  the same commit may be published). If the commit has release tags but
  none is published yet, the step fails and names them: a SHA pin asks for
  that commit's version, so it does not fall back to `latest`, and
  `verify-provenance: false` would not help, since there is nothing to
  download. Pin a published release (`version: vX.Y.Z`) or wait for the
  release workflow to publish it. If the release lookup itself fails (GitHub's
  API answers with an error), the step fails too, since it cannot tell. If
  no release tag points at the commit, or the tag lookup fails (no `git` on
  the runner, no network), it runs `latest` and logs one `::warning` that
  says which. At any other ref (a branch, `v0`) an unset `version`
  means `latest`, which floats the binary to the newest stable release, and
  with it the knowledge base and the verdicts. With both pinned, the gate
  changes only when you bump them.
- **Only this repository's ref counts.** The action reads its ref as a
  release only when `github.action_repository` is
  `abd-ulbasit/upgradescope` (in any letter case). Inside a composite action
  that wraps this one, GitHub gives the wrapper's repository and ref
  ([actions/runner#2473](https://github.com/actions/runner/issues/2473)),
  so a wrapper pinned at its own `v1.0.0` does not pick upgradescope
  v1.0.0: with `version` unset it runs `latest`. Set `version` in the
  wrapper.
- **`latest` skips prereleases.** GitHub's latest release is never a release
  candidate. If you set `version: latest` at a release tag that is newer
  than the latest release (a release candidate, say), the step logs a
  `::warning` that the binary is older than the action, since an older
  engine can pass what the newer one blocks. If that latest is v0.1.0 or
  v0.1.1, which predate provenance, and the action's release is v0.2.0-rc.2
  or later, the step fails instead ([Latest moved
  back](#install-and-integrity)). With `version: latest` the action does
  not look up a commit SHA's release, so at a SHA or a branch it gives no
  such warning and no such failure.

The examples below pin v0.2.0, the first release that ships this action
(root `action.yml`, outputs, step summary). Put the release you pin in its
place, or use `@v0` and leave out `version`.

```yaml
jobs:
  upgrade-gate:
    runs-on: ubuntu-latest
    permissions:
      contents: read          # actions/checkout
      security-events: write  # upload-sarif
      actions: read           # upload-sarif, private repositories only
    steps:
      - uses: actions/checkout@v7
        with:
          persist-credentials: false
      - run: helm template my-release ./chart --output-dir rendered
      - uses: abd-ulbasit/upgradescope@v0.2.0   # or @<full commit SHA>
        id: gate
        with:
          path: rendered
          target: "1.36"
          fail-on: blocker
          version: v0.2.0
      - uses: github/codeql-action/upload-sarif@v4
        if: ${{ !cancelled() && steps.gate.outputs.sarif-file != '' }}  # also when the gate failed
        with:
          sarif_file: ${{ steps.gate.outputs.sarif-file }}
          category: upgradescope
```

Code scanning shows an alert on a pull request's diff only when the file it
sits on is committed to the repository. This example renders into
`rendered/`, which is not committed, so its alerts are filed under
`rendered/<chart>/templates/x.yaml`: they are in the Security tab and the
gate still fails, but they never appear on the diff. To get alerts on the
diff, scan manifests that are committed (plain YAML, a kustomize overlay, or
a rendered directory you commit and keep current). The job summary lists
every finding and the step annotates each blocker and warning either way. The
[CI gate page](https://github.com/abd-ulbasit/upgradescope/blob/main/docs/getting-started/ci-gate.md)
has the details.

`uses: abd-ulbasit/upgradescope/action@<ref>` is the same action at its
original path, and it keeps working. The two `action.yml` files run the
same `action/run.sh`, and CI fails if they drift apart.

### kustomize

```yaml
      - run: |
          mkdir -p rendered
          kustomize build overlays/prod > rendered/prod.yaml
      - uses: abd-ulbasit/upgradescope@v0.2.0
        with:
          path: rendered
          target: "1.36"
          version: v0.2.0
```

### Without code scanning

Private repositories need GitHub Code Security to upload SARIF. You don't
need it to gate. Drop the upload step and `security-events: write`, keep
`contents: read`, and read the step summary, the annotations or the
outputs:

```yaml
      - uses: abd-ulbasit/upgradescope@v0.2.0
        id: gate
        with: {path: rendered, target: "1.36", version: v0.2.0}
      - if: ${{ !cancelled() }}   # also when the gate failed
        run: echo "verdict $VERDICT, score $SCORE, $BLOCKERS blockers"
        env:
          VERDICT: ${{ steps.gate.outputs.verdict }}
          SCORE: ${{ steps.gate.outputs.score }}
          BLOCKERS: ${{ steps.gate.outputs.blockers }}
```

Pass values like these through `env:`, as shown. Never put `${{ }}` inside
a `run:` script.

### Ignore rules and baselines

Two ways to adopt the gate on manifests that already have findings:

- **Ignore rules** accept specific findings, with a reason and an optional
  expiry date, in an `.upgradescope.yaml`. The scan finds that file in
  `path` or at the repository root without any input. Set `config` to use
  a file elsewhere. Suppressed findings are not scored and do not fail the
  gate. The step summary lists each one with its reason and the file that
  suppressed it.
- **A baseline** is the JSON report of an earlier scan. With `baseline`
  set, the gate fails only on findings that are new since. The findings
  the baseline already had still count toward the score and the verdict,
  so a passing gate can say `blocked`. The summary marks each finding new
  or unchanged, and `new-blockers` counts what the gate counted.

The rule format, how findings match a baseline, and finding keys are in
[Suppressions and baselines](https://abd-ulbasit.github.io/upgradescope/guides/suppressions-and-baselines/).

Write the baseline once, from manifests rendered to the same `path` that
CI scans (object paths in the report are relative to it), and commit it:

```sh
upgradescope scan --files rendered --target 1.36 --fail-on never \
  --write-baseline upgradescope-baseline.json
```

```yaml
      - uses: abd-ulbasit/upgradescope@v0.2.0
        with:
          path: rendered
          target: "1.36"
          version: v0.2.0
          config: ci/upgradescope.yaml          # only when not .upgradescope.yaml at the root
          baseline: upgradescope-baseline.json
```

To refresh it from CI, set `write-baseline` to a path, then commit the file
or save it as an artifact. The `report-json` output also works as a
baseline. If `baseline` and `write-baseline` name the same file, the gate
uses the old baseline and the file is then replaced by this scan's report.

These inputs need upgradescope v0.2.0 or later. With an older `version`,
the scan does not know the flags and the step fails with exit 1.

### Who can turn the gate off

On a `pull_request`, `config`, `baseline`, the object annotations and the
workflow itself come from the pull request's tree, so **a pull request can
suppress its own findings and the gate then passes**. The suppressions are
visible (the step summary's suppressed table names each reason and the file
that suppressed it), but they are not blocked. `config` and `baseline` are
plain workspace paths, and the action does not care which commit wrote
them, so point them at files copied from the base commit. Naming `config`
also stops `scan` looking for another `.upgradescope.yaml`:

```yaml
on: pull_request
jobs:
  upgrade-gate:
    runs-on: ubuntu-latest
    permissions:
      contents: read
    steps:
      - uses: actions/checkout@v7            # the pull request (its merge commit)
        with:
          persist-credentials: false
      - name: Clear the path the base commit goes to
        run: rm -rf -- trusted               # a pull request may have committed one
      - uses: actions/checkout@v7            # the base commit, only the two files
        with:
          ref: ${{ github.event.pull_request.base.sha }}
          path: trusted
          sparse-checkout: |
            /.upgradescope.yaml
            /upgradescope-baseline.json
          sparse-checkout-cone-mode: false
          persist-credentials: false
      - run: helm template my-release ./chart --output-dir rendered
      - name: Take the config and baseline from the base commit, last
        run: |
          rm -f -- .upgradescope.yaml upgradescope-baseline.json
          cp trusted/.upgradescope.yaml trusted/upgradescope-baseline.json .
      - uses: abd-ulbasit/upgradescope@v0.2.0
        id: gate
        with:
          path: rendered
          target: "1.37"
          version: v0.2.0
          config: .upgradescope.yaml           # named, so no other config is looked for
          baseline: upgradescope-baseline.json
```

The copy fails the step when the base commit has no such file (`ignore: []`
is a valid empty config), and a pull request that changes either file is
judged by the old rules, so accepting a finding takes a pull request of its
own that changes only those files. The copy comes last, right before the
gate, and removes the destination first (`rm -f --`): a pull request can
commit `.upgradescope.yaml` as a symlink to a file an earlier step writes, a
plain `cp` would write through it, and that step would then overwrite the
trusted content. `config` names the copy, not `trusted/...`, because the
config's file globs resolve relative to the config's directory. Annotations
stay honoured and have no input to turn them off: put `CODEOWNERS` with
required review on the config, the baseline and `.github/workflows/`, and to
fail the job when an annotation suppressed a finding, add after the gate
step:

```yaml
      - if: ${{ !cancelled() && steps.gate.outputs.report-json != '' }}
        run: jq -e '[.suppressed[]? | select(.source == "annotation")] | length == 0' "$REPORT"
        env:
          REPORT: ${{ steps.gate.outputs.report-json }}
```

The trust table, with the reasoning, is on the
[CI gate page](https://github.com/abd-ulbasit/upgradescope/blob/main/docs/getting-started/ci-gate.md#who-can-turn-the-gate-off).

### Targets past the horizon

The embedded knowledge base knows Kubernetes up to one minor, its horizon
(`upgradescope version` prints it). A `target` newer than that cannot be
fully judged: an API removed in a release the knowledge base does not know
would be missed. The verdict is then `unknown` (a required `kb-coverage`
check was not assessed), and `unknown` fails the gate like a blocker.

Either target the horizon or lower, upgrade to a release with a newer
knowledge base, or accept the gap knowingly and gate on findings alone:

```yaml
      - uses: abd-ulbasit/upgradescope@v0.2.0
        with:
          path: rendered
          target: "1.38"           # newer than the pinned release's horizon
          version: v0.2.0
          allow-incomplete: true   # an unknown verdict no longer fails the step
```

With `allow-incomplete: true`, blockers (and warnings, with
`fail-on: warning`) still fail the step, the `verdict` output and the step
summary still say `unknown`, and the summary's not-assessed section names
the gap. `allow-incomplete` needs upgradescope v0.2.0 or later, like the
inputs above.

## Permissions

| Permission | Why |
|---|---|
| `contents: read` | `actions/checkout`. Any `permissions:` block sets everything it leaves out to `none`, so list this one. |
| `security-events: write` | Only for `github/codeql-action/upload-sarif`. |
| `actions: read` | Only for `upload-sarif` in private repositories. |

The action downloads release assets anonymously. Its one GitHub API call is
`gh attestation verify`, which reads this repository's public attestations
with `github.token`; the install step passes that token to gh as
`GH_TOKEN`, and nothing else uses it. That needs no `attestations:`
permission in your workflow: the permission scopes the token on your own
repository, and this repository's attestations are public (on 2026-10-09
the attestations endpoint answered an unauthenticated request for
v0.2.0-rc.2's linux/amd64 archive with its attestation).

## Inputs

| Input | Required | Default | |
|---|---|---|---|
| `path` | yes | | File or directory of rendered manifests (`*.yaml`, `*.yml`, `*.json`). It must exist. |
| `target` | yes | | Target Kubernetes minor version, `MAJOR.MINOR` such as `1.36`, **quoted** (`target: "1.30"`): YAML reads an unquoted `1.30` as the number 1.3, which would judge nothing and read ready, so the action refuses any target below 1.16, the oldest minor the knowledge base covers, and says to quote it. |
| `fail-on` | no | `blocker` | `blocker`, `warning` or `never`. The step fails when findings reach this severity, or when the verdict is `unknown` (unless `allow-incomplete`). `never` never fails. |
| `allow-incomplete` | no | `false` | `true` or `false`. `true` passes `scan --allow-incomplete`: the gate fails on findings alone, not on an `unknown` verdict. The `verdict` output still says `unknown`. See [Targets past the horizon](#targets-past-the-horizon). |
| `version` | no | the action ref's release, else `latest` | A release tag such as `v0.2.0`, `latest` (the newest stable release), or `preinstalled`. `preinstalled` installs nothing and uses the `upgradescope` already on `PATH`. Unset, the action at a release tag ref (`@vX.Y.Z` or `@vX.Y.Z-rc.N`) runs that tag; at a full commit SHA it runs the release tag that points at that commit, or `latest` with a `::warning` when none does or the lookup fails; at any other ref, or inside another action, it runs `latest`. See [Usage](#usage). |
| `verify-provenance` | no | `true` | `true` or `false`. `true` verifies, for every release but v0.1.0 and v0.1.1 (which predate provenance), that this repository's release workflow built the archive at that release's tag, with `gh attestation verify` or, without gh 2.68+, `cosign verify-blob`, and fails the step before installing when it does not verify, neither tool is on `PATH`, or no archive downloads (there is no source-build fallback). `false` checks the archive against `checksums.txt` only and warns, and falls back to `go install` when no archive downloads. See [Install and integrity](#install-and-integrity). |
| `config` | no | | Path to an `.upgradescope.yaml` with ignore rules (`scan --config`). Unset, the scan looks for `.upgradescope.yaml` in `path`, then at the repository root. |
| `baseline` | no | | Path to the JSON report of an earlier scan: the `report-json` output, or a `write-baseline` file (`scan --baseline`). The gate then fails only on findings that are new since. |
| `write-baseline` | no | | Also write this scan's JSON report, after suppression, to this path, for a later `baseline` (`scan --write-baseline`). |

Relative paths resolve from the workspace. The action checks every input
before it downloads anything. A bad `version`, `target` (not `MAJOR.MINOR`, or below 1.16), `fail-on`,
`allow-incomplete` or `verify-provenance` value, a `path` that does not exist, a `config` or `baseline` that is not
a file, or a `write-baseline` whose directory does not exist fails the
step with an error that names the input. The error shows the value with
`%`, carriage returns and line breaks escaped (as `%25`, `%0D` and `%0A`),
so a value holding a line break and `::warning::` cannot forge an
annotation or a log mask. The scan's own messages repeat the `path` input
and the names of files under it, which a pull request from a fork chooses.
The action prints them, and the Markdown report (which names the files
with findings), to the log with a `| ` in front of every line, so no line
can start a workflow command: the runner reads `::` after stripping any
leading Unicode whitespace, form feeds and no-break spaces included, so no
narrower rule is safe. A carriage return is written `%0D`, and every `##[`
(the runner's older command form, which it reads anywhere in a line) is
written `# #[`; the rest is unchanged. The step summary and the `summary-file` output keep the
report as it is. Earlier versions of
the action passed any other `version` to `go install`, so a branch name or
commit worked there; now it must be a release tag, `latest` or
`preinstalled`.

## Outputs

| Output | |
|---|---|
| `sarif-file` | Path to the SARIF report. It is complete when the gate fails, so upload it with `if: ${{ !cancelled() && steps.gate.outputs.sarif-file != '' }}`. It is set only when the gate exits 0 or 2, and not when the scan itself failed (exit 1) or was killed, so that guard skips the upload instead of failing on an empty file. |
| `report-json` | Path to the JSON report, the same as `upgradescope scan --output json`. |
| `verdict` | `ready`, `blocked` or `unknown`. `unknown` means no blocker was found but a required check could not run. |
| `ready` | `true` when the verdict is `ready`, otherwise `false`. |
| `score` | Readiness score, 0-100. |
| `blockers` | Number of blocker findings. Suppressed findings are not counted. |
| `warnings` | Number of warning findings. Suppressed findings are not counted. |
| `new-blockers` | Blocker findings that are not in the baseline, the ones the gate counts. Without a baseline, the same as `blockers`. |
| `new-warnings` | Warning findings that are not in the baseline. Without a baseline, the same as `warnings`. |
| `suppressed` | Number of findings that ignore rules or object annotations suppressed. |
| `summary-file` | Path to the Markdown summary, the same text as the step summary, for example to post as a pull request comment. Not set when the release cannot write it (v0.1.x). |

Each use of the action writes its reports and summary to a new directory
under `RUNNER_TEMP`. You can run it several times in one job, for example
once per chart or per target. A later run does not overwrite the files
that an earlier run's `sarif-file`, `report-json` and `summary-file` point
to.

The action takes `report-json`, `verdict`, `ready`, `score` and the counts
from the JSON report with `jq`. GitHub-hosted runners have `jq`. On a
self-hosted runner without it, the gate, `sarif-file` and the step summary
still work, and a warning says that the other outputs are not set.

With `version` set to a v0.1.x release, whose JSON report has no verdict,
the action derives `verdict` as later releases compute it: `blocked` if there is a blocker,
otherwise `ready` or `unknown` from the report's `ready`. Those releases
cannot write the step summary, so a warning replaces it.

## Exit status

| Exit | Meaning |
|---|---|
| 0 | The gate passed. |
| 2 | The scan worked and the gate failed: findings at or above `fail-on`, or an `unknown` verdict without `allow-incomplete`. The SARIF, outputs and summary are all written. |
| 1 | The scan itself failed, for example because no manifests were found under `path`, or the config file or baseline is invalid. The summary says so and the log has the error. No outputs are set, `sarif-file` included. |

## Install and integrity

- **Release archive.** The action downloads `upgradescope_<os>_<arch>.tar.gz`
  for the runner. `latest` is first resolved to one tag. The archive's sha256
  must match the entry for it in that release's `checksums.txt`. A mismatch,
  a missing entry or a missing `checksums.txt` fails the step. Nothing is
  installed in that case, and the action never falls back to a source build.
  The checksums come from the same release, so on its own this check
  catches corruption and truncation but not a tampered release: anything
  that can replace a release's assets can replace the archive and
  `checksums.txt` together.
- **Provenance** (`verify-provenance: true`, the default). For every release
  but v0.1.0 and v0.1.1 (see below), the action then checks that this
  repository's release workflow built the archive, before it installs
  anything:
  - with `gh` on `PATH` (GitHub-hosted runners have it), it runs
    `gh attestation verify <archive> --repo abd-ulbasit/upgradescope
    --signer-workflow abd-ulbasit/upgradescope/.github/workflows/release.yml
    --source-ref refs/tags/<tag> --deny-self-hosted-runners`: the archive's
    build-provenance attestation must be signed by `release.yml`, run for
    that release's tag, on a GitHub-hosted runner. Any workflow of another
    tag or branch, or a self-hosted runner, does not count;
  - without `gh`, or with a `gh` older than 2.68 (some self-hosted runners;
    the log says so), with `cosign` on `PATH`, it downloads
    `checksums.txt.sigstore.json` from the same release and runs
    `cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json
    --certificate-identity
    https://github.com/abd-ulbasit/upgradescope/.github/workflows/release.yml@refs/tags/<tag>
    --certificate-oidc-issuer https://token.actions.githubusercontent.com`:
    `checksums.txt` must be signed by that workflow at that tag, and the
    archive is held to it by its sha256;
  - with neither, the step fails and says so: install `gh` (2.68 or later)
    or `cosign` (`sigstore/cosign-installer`) before the action, or set
    `verify-provenance: false`.

  The action finds out whether the `gh` can verify by asking it once,
  `gh attestation verify --help`, and requiring every flag the call passes
  to be listed. 2.68 is the first `gh` with `--source-ref`
  ([cli/cli#10308](https://github.com/cli/cli/pull/10308)); `gh` 2.49 to
  2.67 has `attestation verify` but not that flag, and is treated like a
  `gh` without `attestation`, not as a failed verification. Once the `gh`
  can verify, any `gh` failure fails the step; `cosign` is not tried after
  it.
  On GitHub Enterprise Server or GHE.com runners, `github.token` belongs to
  that host, not github.com, so `gh attestation verify` against this
  repository fails: put `cosign` on `PATH` and no `gh`, or set
  `verify-provenance: false`.

  A check that does not pass fails the step, and nothing is put on `PATH`.
  The verifier's output is in the log, each line behind `| `. What is
  verified is the build: which workflow, tag and kind of runner produced
  the archive. Not that the release's code is free of bugs.
- **Without provenance.** Two releases predate provenance, v0.1.0 and v0.1.1
  (GitHub's latest until v0.2.0 ships): `gh attestation verify` finds no
  attestation for their archives. They are named, not found by a version
  comparison, so every other tag is verified, a prerelease suffix such as
  `-beta` included, and one without provenance fails the step. v0.2.0-rc.2
  is the first release that publishes it (`gh attestation verify` of its
  linux/amd64 archive passes at `refs/tags/v0.2.0-rc.2`; v0.2.0-rc.1 is a
  tag that was never published as a release). v0.1.0 and v0.1.1 are
  checked against `checksums.txt` only, with a `::warning` that says so.
  `verify-provenance: false` does the same for any release, also with a
  `::warning`. Neither detects a tampered release.
- **Latest moved back.** With `version: latest` and the action at a
  release tag from v0.2.0-rc.2 on (`@vX.Y.Z` or `@vX.Y.Z-rc.N`), a latest
  that resolves to v0.1.0 or v0.1.1 fails the step before anything is
  downloaded. Such a latest means someone moved GitHub's latest back to a
  release nothing can verify (what anything that can edit the releases
  would do to install a binary of its choosing), or that the action is at
  a release candidate, which GitHub's latest skips, before a stable
  release from v0.2.0 on is latest.
  Set `version` to the action's release, or `verify-provenance: false` to
  install it on the checksum alone. At a commit SHA, a branch or `@v0`,
  `latest` is not compared with the action's release.
- **No archive, no install.** When the archive cannot be downloaded (the
  release has none for the runner, the asset is gone, the download fails)
  or `latest` does not resolve to a tag, the step fails with curl's final
  error and installs nothing. It never falls back to a source build while
  `verify-provenance` is `true`: a `go install` build cannot be checked
  against the release's provenance, so a fallback would let anything that
  can make the download fail install an unverified binary.
- **Source build, opt-in.** Only with `verify-provenance: false` does the
  action then run `go install` for that version (or `@latest`), with a
  `::warning` that it is unverified, and only when a Go toolchain is set up
  (`actions/setup-go`); without Go, the step fails and says why. A source
  build is checked by Go's module checksum database only.
- **Runners.** Linux and macOS, amd64 and arm64. Windows runners are not
  supported.

## GitHub Marketplace

`action.yml` at the repository root, with `branding`, makes the repository
eligible for a Marketplace listing. The listing itself is a manual step: tick
"Publish this Action to the GitHub Marketplace" when you draft a release in
the GitHub UI. Until the action has been published that way, use it by
`owner/repo@ref` as shown above, which works without a listing.
