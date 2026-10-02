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
sha256sum --ignore-missing -c checksums.txt
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
  image: alpine/helm:3   # any image with helm; add curl and tar if it lacks them
  variables:
    UPGRADESCOPE_VERSION: v0.2.0
  script:
    - helm template my-release ./chart --output-dir rendered
    - base=https://github.com/abd-ulbasit/upgradescope/releases/download/$UPGRADESCOPE_VERSION
    - curl -fsSLO "$base/upgradescope_linux_amd64.tar.gz" && curl -fsSLO "$base/checksums.txt"
    - sha256sum -c checksums.txt 2>/dev/null | grep -q 'upgradescope_linux_amd64.tar.gz: OK'
    - tar -xzf upgradescope_linux_amd64.tar.gz upgradescope
    - ./upgradescope scan --files rendered --target 1.37 --output markdown | tee upgradescope.md
    - ./upgradescope scan --files rendered --target 1.37 --output json > upgradescope.json
  artifacts:
    when: always
    paths: [upgradescope.md, upgradescope.json]
```

`tee` hides the exit code of the first scan (the pipeline's status is
`tee`'s), so the second, unpiped scan is the gate. `upgradescope.md` is the
same Markdown table the GitHub Action posts, ready to paste into a merge
request comment.

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
