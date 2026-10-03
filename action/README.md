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
  `v0` tag to every stable release once it is published and verified
  (from v0.2.0 on). Since `v0` is not a release tag, the default `version`
  is `latest`, and the binary and its knowledge base move with it. This is
  the convenient choice, but a release can change the verdict on an
  unchanged pull request.
- **`@vX.Y.Z` or a commit SHA, plus `version`, for a reproducible gate.**
  Pin the action to a release tag or, stricter, the tag's full commit SHA,
  and set `version` to the same release. With `version` unset, an action at
  a release tag (`@vX.Y.Z` or `@vX.Y.Z-rc.N`) runs that same release, so
  `@v0.2.0-rc.2` runs v0.2.0-rc.2. At any other ref (a branch, a commit SHA,
  `v0`) an unset `version` means `latest`, which floats the binary to the
  newest stable release, and with it the knowledge base and the verdicts.
  With both pinned, the gate changes only when you bump them.
- **`latest` skips prereleases.** GitHub's latest release is never a release
  candidate. If you set `version: latest` at a release tag that is newer
  than the latest release (a release candidate, say), the step logs a
  `::warning` that the binary is older than the action, since an older
  engine can pass what the newer one blocks. The action cannot compare a
  commit SHA or branch with a release, so it gives no such warning there.

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

The action itself calls no GitHub API and needs no token. It downloads
release assets anonymously.

## Inputs

| Input | Required | Default | |
|---|---|---|---|
| `path` | yes | | File or directory of rendered manifests (`*.yaml`, `*.yml`, `*.json`). It must exist. |
| `target` | yes | | Target Kubernetes minor version, such as `1.36`. |
| `fail-on` | no | `blocker` | `blocker`, `warning` or `never`. The step fails when findings reach this severity, or when the verdict is `unknown` (unless `allow-incomplete`). `never` never fails. |
| `allow-incomplete` | no | `false` | `true` or `false`. `true` passes `scan --allow-incomplete`: the gate fails on findings alone, not on an `unknown` verdict. The `verdict` output still says `unknown`. See [Targets past the horizon](#targets-past-the-horizon). |
| `version` | no | the action ref's release, else `latest` | A release tag such as `v0.2.0`, `latest` (the newest stable release), or `preinstalled`. `preinstalled` installs nothing and uses the `upgradescope` already on `PATH`. Unset, the action at a release tag ref (`@vX.Y.Z` or `@vX.Y.Z-rc.N`) runs that tag, and at any other ref it runs `latest`. |
| `config` | no | | Path to an `.upgradescope.yaml` with ignore rules (`scan --config`). Unset, the scan looks for `.upgradescope.yaml` in `path`, then at the repository root. |
| `baseline` | no | | Path to the JSON report of an earlier scan: the `report-json` output, or a `write-baseline` file (`scan --baseline`). The gate then fails only on findings that are new since. |
| `write-baseline` | no | | Also write this scan's JSON report, after suppression, to this path, for a later `baseline` (`scan --write-baseline`). |

Relative paths resolve from the workspace. The action checks every input
before it downloads anything. A bad `version`, `target`, `fail-on` or
`allow-incomplete` value, a `path` that does not exist, a `config` or `baseline` that is not
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
  The checksums come from the same release, so this check catches corruption
  and truncation but not a tampered release. Releases after v0.1.1 also
  publish a cosign bundle for `checksums.txt` and a build-provenance
  attestation for every archive. To check those, run
  `gh attestation verify <archive> --repo abd-ulbasit/upgradescope`.
- **Source build.** If the release has no archive for the runner, the
  action runs `go install` for that version, but only when a Go toolchain
  is set up (`actions/setup-go`). Without Go, the step fails and says why.
- **Runners.** Linux and macOS, amd64 and arm64. Windows runners are not
  supported.

## GitHub Marketplace

`action.yml` at the repository root, with `branding`, makes the repository
eligible for a Marketplace listing. The listing itself is a manual step: tick
"Publish this Action to the GitHub Marketplace" when you draft a release in
the GitHub UI. Until the action has been published that way, use it by
`owner/repo@ref` as shown above, which works without a listing.
