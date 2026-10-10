# Other CI systems

Outside GitHub, run the CLI directly: install a pinned release, render the
manifests, scan, and let the exit code gate the job. The repository has a
template for each of GitLab CI, Jenkins and Azure Pipelines under
[`ci/`](https://github.com/abd-ulbasit/upgradescope/tree/main/ci). Each one
publishes a report its CI shows natively:

| CI system | Template | Report |
|---|---|---|
| GitLab CI | `ci/gitlab/upgradescope.gitlab-ci.yml` | Code Quality (merge request widget) and JUnit (test report) |
| Jenkins | `ci/jenkins/Jenkinsfile` | JUnit, through the `junit` step |
| Azure Pipelines | `ci/azure/azure-pipelines.yml` | JUnit, through `PublishTestResults@2` |

`--output junit` and `--output gitlab-codequality` are new in v0.2.0, and
the templates pin `VERSION=v0.2.0`. Until v0.2.0 is on the
[releases page](https://github.com/abd-ulbasit/upgradescope/releases),
that download fails with a 404 and no earlier release has these formats;
in the meantime, build from `main` with
`go install github.com/abd-ulbasit/upgradescope/cmd/upgradescope@main`.

## Install a pinned release

```sh
VERSION=v0.2.0
base=https://github.com/abd-ulbasit/upgradescope/releases/download/$VERSION
curl -fsSLO "$base/upgradescope_linux_amd64.tar.gz"
curl -fsSLO "$base/checksums.txt"
sha256sum --ignore-missing -c checksums.txt   # macOS: shasum -a 256 -c --ignore-missing checksums.txt
tar -xzf upgradescope_linux_amd64.tar.gz upgradescope
./upgradescope version
```

Pinning the release pins the knowledge base too, so the gate changes only
when you bump `VERSION`. From v0.2.0, `checksums.txt` is signed; to verify
it as well, see [Install](../operations/install.md#verify-a-download).
Alternatively, run the container image `ghcr.io/abd-ulbasit/upgradescope`,
whose entrypoint is the binary.

## The scan

```sh
./upgradescope scan --files rendered --target 1.37 --output junit > upgradescope-junit.xml
```

| Exit code | Meaning |
|---|---|
| 0 | The gate passed. |
| 1 | An error (no manifests found, an invalid flag, config file or baseline). |
| 2 | The gate failed: a finding at or above `--fail-on`, or the verdict is `unknown` (pass `--allow-incomplete` to gate on findings alone). |

The exit code is the same whatever `--output` is. Most CI systems fail the
job on any non-zero exit, which is what you want for 1 and 2. To keep the
report on failure, write it to a file (as above) and publish it in a step
that runs when the job fails.

## The report formats

**JUnit** (`--output junit`): one test suite per finding category
(`removed-api`, `eol-addon`, ...) and one test case per finding, named by
its key. The outcomes follow the gate, so the test report fails exactly
when the exit code says so:

| Finding | Test case |
|---|---|
| At or above `--fail-on` | failure: the title, then the evidence, objects as `file:line`, fix and references |
| Below `--fail-on` (or `--fail-on never`) | passed, with the same text as its output |
| Unchanged since `--baseline` | skipped, "unchanged since the baseline" |
| Suppressed by an ignore rule or annotation | skipped, with the reason, expiry and source |
| A required check not assessed (verdict `unknown`) | error, in the `not-assessed` suite; skipped with `--allow-incomplete` (a `--target` that is not an upgrade stays an error) |
| An optional check that did not run (`deprecated-calls`, `helm` and `versions` in files mode) | skipped, in the `not-assessed` suite |

A clean live-cluster scan, with every check assessed, is one passing test,
`readiness/no findings`. A clean `--files` scan never is: files mode does
not run `deprecated-calls`, `helm` or `versions`, so its report is those
three skipped tests (`not-assessed/deprecated-calls`, `not-assessed/helm`,
`not-assessed/versions`) and no passed test. Do not count passed tests to
detect a clean files-mode scan; the exit code says it. Jenkins fails a build
whose reports hold no tests, and skipped tests are tests, so neither case
fails it. Times are 0, so the file is the same
on every run of the same scan. The file validates against the Jenkins JUnit
schema, the xUnit plugin's `junit-10.xsd` (vendored in
`internal/junit/junittest`), so readers that validate strictly accept it
too.

**GitLab Code Quality** (`--output gitlab-codequality`): one entry per
finding and object located in a file, on that file and line (line 1 when
only the file is known; relative to the working directory, so scan from the
repository root), with the finding key as `check_name` and severity
blocker → `critical`, warning → `minor`, info → `info`.

- The file is the one scanned. With `helm template --output-dir rendered`,
  that is the rendered file (`rendered/<chart>/templates/x.yaml`), which is
  usually not committed: the merge request widget lists the entry, but the
  diff cannot annotate it. The description names the template it was
  rendered from (`rendered from <chart>/templates/x.yaml`). Only manifests
  committed to the repository get inline annotations.
- GitLab requires a location, but live-cluster findings, add-ons, version
  skew and a stream posted to the gate without `path` have no file. Each
  such finding is one entry on the virtual path
  `upgradescope/<finding key>`, line 1: it shows in the merge request
  widget, and its file link goes nowhere. So are affected objects of a
  located finding that have no file or were not recorded.
- The fingerprint hashes the key and the object's file, namespace and
  name, never its line or the counts in its title, so GitLab's comparison
  with the target branch does not report a finding as fixed and new when
  its object moves within the file.
- A required check that was not assessed is a `critical` entry
  (`not-assessed/<capability>`): the verdict is `unknown`. A partial
  check is `info`.
- Suppressed findings are left out: GitLab has no dismissed state. Every
  other output lists them.

The report validates against the schema GitLab checks each entry with
(vendored in `internal/codequality/codequalitytest`), and GitLab stops
reading a report at the first entry that does not.

## GitLab CI

```yaml
--8<-- "ci/gitlab/upgradescope.gitlab-ci.yml"
```

Copy the job, or include the file at a release tag and override what
differs, such as `before_script` for your render step:

```yaml
include:
  - remote: https://raw.githubusercontent.com/abd-ulbasit/upgradescope/v0.2.0/ci/gitlab/upgradescope.gitlab-ci.yml

upgrade-readiness:
  variables:
    UPGRADESCOPE_TARGET: "1.38"
  before_script:
    - helm template shop ./deploy/shop --output-dir rendered
```

Why it is written this way:

- **`entrypoint: [""]`.** GitLab's Docker executor starts the job through
  the image's entrypoint, and `alpine/helm`'s is `helm`, so without the
  override the job fails before the script runs.
- **The checksum line** checks only the archive you downloaded. The
  image's `sha256sum` is busybox, which has no `--ignore-missing`, and
  GitLab runs the script under `set -eo pipefail`, so a
  `sha256sum -c checksums.txt | grep` would fail on the archives you did
  not download.
- **Three scans.** The JUnit and Markdown scans never fail the job
  (`|| true`); the last one, unpiped, is the gate, and its exit code is
  the job's. It writes the Code Quality report, which, like the JUnit
  report, is published with `artifacts:when: always`, so a failed gate
  still shows its findings. `upgradescope.md` is the same Markdown table
  the GitHub Action posts, ready to paste into a merge request comment.

!!! note "How this was checked"
    `TestDocsGitLabJob` (in `internal/cli`) keeps the template's shape:
    the entrypoint override, no GNU-only flags, a checksum check, no piped
    gate, and `artifacts:reports` naming the files the scans write. The
    download and checksum lines were run under `bash -eo pipefail` in
    `alpine/helm:3` (entrypoint cleared) against the v0.1.1 release
    before the report formats existed. On 2026-10-02 the template's three
    scan lines, and the Jenkins and Azure gate lines, were run with a
    development build on macOS (not in the image) against manifests with
    removed APIs: each wrote its reports and exited 2. No GitLab runner,
    Jenkins or Azure Pipelines agent is part of CI.

## Jenkins

```groovy
--8<-- "ci/jenkins/Jenkinsfile"
```

`sh` steps run with `-e`, so a failed download or checksum stops the stage,
and the gate's exit code fails it. The `junit` step runs under
`post { always }`, so a failed gate still publishes its tests: one per
finding, failing where the gate does.

## Azure Pipelines

```yaml
--8<-- "ci/azure/azure-pipelines.yml"
```

A multi-line `script` step runs without `-e`, so the install step sets it.
`PublishTestResults@2` runs on `succeededOrFailed()`, so a failed gate still
publishes its tests to the run's Tests tab; `failTaskOnFailedTests` is off
because the gate step has already failed the job.

`TestCITemplateJenkins` and `TestCITemplateAzure` (in `internal/cli`) keep
both templates' shape: the pinned release and its checksum check, an
unpiped gate, and a publishing step that runs when it fails and reads the
file it writes. Neither CI system is part of CI.

## Against a server

If an `upgradescope serve` holds the cluster you deploy to, the gate
endpoint judges the manifests in that cluster's context and needs no binary
in the job: [CI gate](../getting-started/ci-gate.md#the-servers-gate-endpoint).
It answers in the same formats (`format=junit` or
`format=gitlab-codequality`), holding the findings the manifests
introduce, with the same status code whatever the format:

```sh
curl -sS --fail-with-body --retry 5 -X POST \
  "$SERVER/api/v1/gate?target=1.37&cluster=prod-eu-1&format=gitlab-codequality&path=rendered.yaml" \
  -H "Authorization: Bearer $READ_TOKEN" \
  -H "Content-Type: application/x-yaml" \
  --data-binary @rendered.yaml > gl-code-quality-report.json
```
