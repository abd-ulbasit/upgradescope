# Other CI systems

Outside GitHub, run the CLI directly: install a pinned release, render the
manifests, scan, and let the exit code gate the job. There are no native
templates or CI-specific report formats yet (GitLab Code Quality, JUnit;
[#73](https://github.com/abd-ulbasit/upgradescope/issues/73)), so keep the SARIF or JSON report as an artifact and, where you want a
human-readable summary, the Markdown output.

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
./upgradescope scan --files rendered --target 1.37 --output json > upgradescope.json
```

| Exit code | Meaning |
|---|---|
| 0 | The gate passed. |
| 1 | An error (no manifests found, an invalid flag, config file or baseline). |
| 2 | The gate failed: a finding at or above `--fail-on`, or the verdict is `unknown` (pass `--allow-incomplete` to gate on findings alone). |

Most CI systems fail the job on any non-zero exit, which is what you want
for 1 and 2. To keep the report on failure, write it to a file (as above)
and mark it as an artifact that is kept on failure.

## GitLab CI

```yaml
upgrade-readiness:
  image:
    name: alpine/helm:3   # helm, curl, tar and bash; any image with those works
    entrypoint: [""]      # its entrypoint is `helm`; GitLab needs a shell
  variables:
    UPGRADESCOPE_VERSION: v0.2.0
  script:
    - helm template my-release ./chart --output-dir rendered
    - base=https://github.com/abd-ulbasit/upgradescope/releases/download/$UPGRADESCOPE_VERSION
    - curl -fsSLO "$base/upgradescope_linux_amd64.tar.gz" && curl -fsSLO "$base/checksums.txt"
    - grep ' upgradescope_linux_amd64.tar.gz$' checksums.txt > upgradescope.sha256 && sha256sum -c upgradescope.sha256
    - tar -xzf upgradescope_linux_amd64.tar.gz upgradescope
    - ./upgradescope scan --files rendered --target 1.37 --output markdown > upgradescope.md || true
    - ./upgradescope scan --files rendered --target 1.37 --output json > upgradescope.json
  artifacts:
    when: always
    paths: [upgradescope.md, upgradescope.json]
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
- **Two scans.** The first writes the Markdown summary and never fails the
  job (`|| true`); the second, unpiped, is the gate, and its exit code is
  the job's. `upgradescope.md` is the same Markdown table the GitHub Action
  posts, ready to paste into a merge request comment.

!!! note "How this was checked"
    `TestDocsGitLabJob` (in `internal/cli`) keeps the job's shape: the
    entrypoint override, no GNU-only flags, no piped gate. The script itself
    was run under `bash -eo pipefail` on 2026-10-02: in `alpine/helm:3`
    (entrypoint cleared) against the v0.1.1 release for the download,
    checksum and JSON gate, and the two scan lines again with a build of
    `main`, since `--output markdown` is new in v0.2.0. No GitLab runner is
    part of CI.

## Jenkins

```groovy
stage('Upgrade readiness') {
  steps {
    sh '''
      helm template my-release ./chart --output-dir rendered
      ./upgradescope scan --files rendered --target 1.37 --output sarif > upgradescope.sarif
    '''
  }
  post {
    always { archiveArtifacts artifacts: 'upgradescope.sarif' }
  }
}
```

(Install the binary in an earlier stage, as above.) The Warnings Next
Generation plugin can read SARIF to show findings in the build.

## Against a server

If an `upgradescope serve` holds the cluster you deploy to, the gate
endpoint judges the manifests in that cluster's context and needs no binary
in the job: [CI gate](../getting-started/ci-gate.md#the-servers-gate-endpoint).
