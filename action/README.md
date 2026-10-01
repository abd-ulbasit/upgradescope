# upgradescope GitHub Action

Gates a pull request on Kubernetes upgrade readiness. The action scans
rendered manifests (`helm template`, `kustomize build`, plain YAML) for APIs
that are removed or deprecated at a target Kubernetes version. It fails the
step when findings reach `fail-on`, and it reports in three places:

- **Step summary.** The job's summary page gets a table with one row per
  finding: severity, title, the objects as `file:line`, and the fix. This
  happens whether the gate passes or fails.
- **Job log and annotations.** The log gets the same table. Each finding
  that fails the gate at your `fail-on` becomes an `::error` annotation, and
  every other blocker or warning a `::warning`. Annotations sit at the
  finding's first file and line. With `fail-on: never`, nothing fails the
  gate, so every annotation is a warning.
- **SARIF and outputs.** The SARIF file is complete even when the gate fails,
  so you can upload it to code scanning. The action also sets the verdict,
  score and counts as outputs for later steps.

## Usage

Pin the action and the binary. Use a release tag or, stricter, the tag's
full commit SHA. Set `version` too: if you pin only the action ref, the
binary still floats to the latest release, and with it the knowledge base
and the gate's verdicts. The examples below use v0.2.0, the first release
that ships this action (root `action.yml`, outputs, step summary). Put the
release you pin in its place. There is no moving `v0` tag.

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
| `path` | yes | | File or directory of rendered manifests (`*.yaml`, `*.yml`, `*.json`). It must exist. Relative paths resolve from the workspace. |
| `target` | yes | | Target Kubernetes minor version, such as `1.36`. |
| `fail-on` | no | `blocker` | `blocker`, `warning` or `never`. The step fails when findings reach this severity, or when the verdict is `unknown`. `never` never fails. |
| `version` | no | `latest` | A release tag such as `v0.2.0`, `latest`, or `preinstalled`. `preinstalled` installs nothing and uses the `upgradescope` already on `PATH`. |

The action checks every input before it downloads anything. A bad
`version`, `target` or `fail-on` value, or a `path` that does not exist,
fails the step with an error that names the input. Earlier versions of
the action passed any other `version` to `go install`, so a branch name or
commit worked there; now it must be a release tag, `latest` or
`preinstalled`.

## Outputs

| Output | |
|---|---|
| `sarif-file` | Path to the SARIF report. It is complete when the gate fails, so upload it with `if: ${{ !cancelled() }}`. |
| `report-json` | Path to the JSON report, the same as `upgradescope scan --output json`. |
| `verdict` | `ready`, `blocked` or `unknown`. `unknown` means no blocker was found but a required check could not run. |
| `ready` | `true` when the verdict is `ready`, otherwise `false`. |
| `score` | Readiness score, 0-100. |
| `blockers` | Number of blocker findings. |
| `warnings` | Number of warning findings. |

Each use of the action writes its reports to a new directory under
`RUNNER_TEMP`. You can run it several times in one job, for example once
per chart or per target. A later run does not overwrite the files that an
earlier run's `sarif-file` and `report-json` point to.

The action takes `verdict`, `ready`, `score`, `blockers`, `warnings` and
`report-json` from the JSON report with `jq`. GitHub-hosted runners have
`jq`. On a self-hosted runner without it, the gate and `sarif-file` still
work, and a warning says that the other outputs are not set.

With `version` set to a v0.1.x release, whose JSON report has no verdict,
the action derives `verdict` as later releases compute it: `blocked` if there is a blocker,
otherwise `ready` or `unknown` from the report's `ready`. Those releases
cannot write the step summary, so a warning replaces it.

## Exit status

| Exit | Meaning |
|---|---|
| 0 | The gate passed. |
| 2 | The scan worked and the gate failed. The SARIF, outputs and summary are all written. |
| 1 | The scan itself failed, for example because no manifests were found under `path`. The summary says so and the log has the error. |

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
