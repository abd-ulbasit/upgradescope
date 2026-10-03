# CI gate

Three ways to fail a pull request that would break the upgrade: the GitHub
Action, the CLI in any CI system, and the server's gate endpoint, which
judges manifests inside a known cluster's context.

A manifest gate sees what the render contains: removed and deprecated APIs,
and add-ons named by the images and labels of workload pod templates, so a
pull request that adds or keeps an end-of-life add-on (an Ingress NGINX
controller, say) fails. It cannot see version skew, Helm releases, or
images injected at admission time (a mesh sidecar); those need a
cluster.

## GitHub Action

The repository root is a composite action. It installs a release binary
verified against the release's `checksums.txt`, scans rendered manifests,
writes SARIF and a job summary, annotates the findings, and fails the step
when the gate fails.

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
      - uses: abd-ulbasit/upgradescope@v0.2.0   # or the tag's full commit SHA
        id: gate
        with:
          path: rendered
          target: "1.37"
          fail-on: blocker
          version: v0.2.0                        # pin the binary, and with it the knowledge base
      - uses: github/codeql-action/upload-sarif@v4
        if: ${{ !cancelled() && steps.gate.outputs.sarif-file != '' }}  # also when the gate failed
        with:
          sarif_file: ${{ steps.gate.outputs.sarif-file }}
          category: upgradescope
```

**Where the alerts land.** Each SARIF result sits on the file and line the
scan read: the object's `apiVersion` line, in a path relative to the
checkout. Code scanning shows an alert on a pull request's diff only when
that file is committed to the repository. The example above renders the
chart into `rendered/`, which is not committed, so its alerts are filed
under `rendered/<chart>/templates/x.yaml`, a path the repository does not
have: they are in the repository's Security tab and the gate still fails,
but they never appear on the diff, and the message's "rendered from
`<chart>/templates/x.yaml`" names the chart's directory, not its path in the
repository. For alerts on the diff, scan manifests that are committed:
plain YAML or kustomize overlays in the repository, or a rendered directory
you commit and keep current. Manifests outside the checkout get absolute
`file://` locations, which code scanning cannot place anywhere (the scan
prints a note). The job summary lists every finding and the step
annotates each blocker and warning, committed or not. A finding with no
file (anything found in a live cluster) is not a SARIF result.

**Pin it.** v0.2.0 is the first release that ships the action at the
repository root. Release tags `vX.Y.Z` are immutable (a tag ruleset protects
them); the floating `v0` tag, which the release workflow moves to every
stable release, is not, and with `version: latest` the binary and its
knowledge base move too, so a release can change the verdict on an
unchanged pull request. For a gate that changes only when you change it, pin
the action to `@vX.Y.Z` or its commit SHA, and `version` to the same
release. Left unset, `version` follows the ref when it is a release tag
(`@vX.Y.Z` or `@vX.Y.Z-rc.N`), so `@v0.2.0-rc.2` runs v0.2.0-rc.2 and not
the older "latest" release, which GitHub never sets to a release candidate.
At any other ref (a branch, a commit SHA, `v0`) it is `latest`, so give a
SHA its release in `version`.

If the scan itself fails (exit 1), the action sets no `sarif-file`, so the
`sarif-file != ''` guard above skips the upload instead of failing on an
empty file. Any other exit, a failed gate included, leaves a complete SARIF.

**Targets past the horizon.** A `target` newer than the knowledge base's
horizon makes the verdict `unknown`, which fails the gate. Target a minor the
pinned release knows (`upgradescope version` prints the horizon), or set
`allow-incomplete: true` (the action's input for `scan --allow-incomplete`)
to gate on findings alone: blockers still fail the step, and the `verdict`
output still says `unknown`.

Inputs, outputs, annotations, use without code scanning, and ignore rules
and baselines in the action:
[action/README.md](https://github.com/abd-ulbasit/upgradescope/blob/main/action/README.md).

## The CLI in any CI

```sh
upgradescope scan --files rendered --target 1.37 --output sarif > upgradescope.sarif
```

The exit code is the gate: 0 passed, 2 failed, 1 an error. The SARIF (or
JSON) report is complete either way, so keep it as an artifact.
[Other CI systems](../guides/other-ci.md) has GitLab CI and Jenkins
examples.

To adopt the gate on manifests that already have findings, record a
baseline once and fail only on what a change adds, or accept specific
findings with a reason and an expiry date:
[Suppressions and baselines](../guides/suppressions-and-baselines.md).

## The server's gate endpoint

With an `upgradescope serve` that agents push to, CI can ask a narrower
question: *would these manifests block this cluster's upgrade?* The
manifests are judged inside the cluster's latest stored inventory: its
version, nodes, add-ons, CRDs and team labels. Only what the manifests introduce
counts toward the verdict, so a cluster's existing EOL add-on does not fail
every pull request. The manifests' API usage, add-ons and CRDs are merged
into the cluster's: an EOL add-on the manifests deploy, or a custom
resource at a version the cluster's CRD (or a CRD in the manifests) does not
serve, is the manifests' finding, also when the cluster already has the
same. One case is not seen: the cluster lists its custom resources only at
versions its own CRDs deprecate or do not serve, so live custom resources
at a version that a posted CRD newly deprecates or stops serving are not
judged.

The server gate has no counterpart to `allow-incomplete`. A cluster
already on the knowledge base's newest minor has a default target past the
horizon, so every request answers `unknown` (422) until a release with a
newer knowledge base is deployed; pass an explicit `target` the release
knows, or `fail-on=never` and read the verdict header yourself, until then.

```sh
curl -sS --fail-with-body --retry 5 -X POST \
  "$SERVER/api/v1/gate?target=1.37&cluster=prod-eu-1&format=sarif&path=rendered.yaml" \
  -H "Authorization: Bearer $READ_TOKEN" \
  -H "Content-Type: application/x-yaml" \
  --data-binary @rendered.yaml > results.sarif
```

- The gate fails like `scan --fail-on` (default `blocker`, `&fail-on=`
  to change it) with status 422 and the full report in the body, so
  `--fail-with-body` fails the step and still writes the SARIF file.
- `--retry 5` retries a `503`, after the `Retry-After` it carries: the
  server answers `503` when other gate requests hold its turn or its
  buffers, or when answers still waiting for slow clients leave no room
  for this one. A `422` is not retried.
- `path` names the file the stream was rendered to, so code scanning places
  the findings on it.
- Without `cluster`, the manifests are judged on their own, like
  `scan --files`: API usage, add-ons, and custom resources against the
  CRDs in the stream. `format=json` (the default) returns
  the full report.
- `upgradescope.dev/ignore` annotations are applied, and so are the ignore
  rules of a `.upgradescope.yaml` sent in `config`
  ([The server gate](../guides/suppressions-and-baselines.md#the-server-gate)).
- The gate stores nothing. It needs the read token, when the server has
  one.

This example is run as written by `TestGateDocsExample`, against a server
holding the cluster it names, and must fail on a removed API. All
parameters and limits: [`POST /api/v1/gate`](../reference/api.md#gate).
