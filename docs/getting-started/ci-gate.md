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
verified against the release's `checksums.txt` and, for releases from
v0.2.0-rc.2 on, its build provenance (the release workflow at that tag built
it: `gh attestation verify`, or `cosign verify-blob` without gh 2.49+; the
[Action's README](https://github.com/abd-ulbasit/upgradescope/blob/main/action/README.md#install-and-integrity)
says exactly what is checked), scans rendered manifests,
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
At a full 40-character commit SHA, the action looks the commit up with
`git ls-remote --tags https://github.com/abd-ulbasit/upgradescope` (an
annotated tag counts at the commit it points at) and runs the release tag,
`vX.Y.Z` or `vX.Y.Z-rc.N`, that points at it, the newest if several do. If
no release tag points at the commit, or the lookup fails, it runs `latest`
and logs one `::warning` that says which. At any other ref (a branch, `v0`)
it is `latest`. The ref counts only when `github.action_repository` is
`abd-ulbasit/upgradescope` (in any letter case): inside a composite action
that wraps this one, GitHub reports the wrapper's repository and ref
([actions/runner#2473](https://github.com/actions/runner/issues/2473)), so
there the default is `latest`, and the wrapper should set `version`.

The action sets `sarif-file` only when the gate exits 0 or 2, which leaves a
complete SARIF, a failed gate included. After a scan error (exit 1) or any
other exit, such as an out-of-memory kill, it sets no `sarif-file`, so the
`sarif-file != ''` guard above skips the upload instead of failing on an
empty file.

**Targets past the horizon.** A `target` newer than the knowledge base's
horizon makes the verdict `unknown`, which fails the gate. Target a minor the
pinned release knows (`upgradescope version` prints the horizon), or set
`allow-incomplete: true` (the action's input for `scan --allow-incomplete`)
to gate on findings alone: blockers still fail the step, and the `verdict`
output still says `unknown`.

Inputs, outputs, annotations, use without code scanning, and ignore rules
and baselines in the action:
[action/README.md](https://github.com/abd-ulbasit/upgradescope/blob/main/action/README.md).

## Who can turn the gate off

On a `pull_request` the gate judges the pull request's own files, and
everything that can suppress a finding is among them. **A pull request can
suppress its own findings, and the gate then passes.** This is how any
linter with an in-repository config or inline disable comments behaves, and
it is not hidden: a suppressed finding is listed with its reason in the
`SUPPRESSED` section and the `N suppressed` line of the table, in the step
summary, and as a SARIF suppression, and the file that did it is in the
diff. But the gate is a control against a careless change, not against an
author who wants to get past it, until you pin the inputs below.

| What the gate trusts | Where `scan` reads it | A pull request can change it |
|---|---|---|
| Ignore rules | the file `--config` (the Action's `config`) names; with none named, `.upgradescope.yaml` in the scan root, then at the repository root | by adding or editing that file. One `ignore: [{category: removed-api, reason: x}]` next to a manifest with a removed API turns `READY no` and exit 2 into `READY yes` and exit 0 |
| Object annotations | `upgradescope.dev/ignore` with `ignore-reason`, on the objects being scanned; no flag turns them off | by adding the pair to the offending object (in a chart template, a rendered file or a patch) |
| The baseline | the file `--baseline` (the Action's `baseline`) names | by committing a baseline that holds its own findings |
| The workflow and the Action's inputs | `.github/workflows/` in the pull request's merge commit | by editing the file: `fail-on: never`, `allow-incomplete`, another `version`, or removing the step |

To hold the gate against a pull request's author:

1. **Take the config and the baseline from the base commit.** The action's
   `config` and `baseline` are paths in the workspace, and the action does
   not care which commit wrote them, so check the base commit out beside the
   pull request and copy its two files over the pull request's. Naming the
   config also stops `scan` looking for any other: a
   `.upgradescope.yaml` the pull request adds in the scan root is not read.
   The copy fails the step when the base has no such file, so commit a
   config (`ignore: []` is a valid empty one) and a baseline first, or drop
   the lines you don't use. A pull request that changes either is then
   judged by the old rules, so accepting a finding takes a pull request of
   its own that changes only the config or the baseline, reviewed by the
   people who own them; the new rules apply after it merges.

   Two details of the snippet matter. The copy is the **last step before the
   gate**, and it deletes the destination first (`rm -f --`): a pull request
   can commit `.upgradescope.yaml` as a symlink to a file that an earlier
   step writes (the output of `helm template`, say), and a plain `cp` writes
   through the symlink, after which that step overwrites the trusted content
   with the pull request's own. Removing the link and copying afterwards
   leaves a regular file that nothing else touches. And the Action's
   `config` points at the copy in the workspace, not at
   `trusted/.upgradescope.yaml`, because the config's file globs resolve
   relative to the config's directory.

   The snippet also assumes that no step the pull request controls can write
   into `trusted/`. The one that runs the pull request's code, `helm
   template`, writes only below its `--output-dir` (`rendered/<chart>/...`);
   if you add a step that runs a script or a build from the pull request,
   have it write elsewhere and keep it before the copy.

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

2. **Review the files that decide.** Put `.upgradescope.yaml`, the baseline
   and `.github/workflows/` in `CODEOWNERS`, turn on "Require review from
   Code Owners" in branch protection or a ruleset, and make the gate job a
   required status check. For the workflow itself, which a pull request can
   edit, a required workflow in an organization ruleset can run a workflow
   file from another repository, which the pull request cannot change.
3. **Watch the annotations.** They live in the manifests, so a `CODEOWNERS`
   entry cannot single them out, and no setting turns them off. To fail
   the job when any finding was suppressed by one, add this after the gate
   step (the `jq` of a GitHub-hosted runner reads the report; keep the
   workflow under review, as in step 2):

```yaml
      - if: ${{ !cancelled() && steps.gate.outputs.report-json != '' }}
        run: jq -e '[.suppressed[]? | select(.source == "annotation")] | length == 0' "$REPORT"
        env:
          REPORT: ${{ steps.gate.outputs.report-json }}
```

The server's gate endpoint has the same boundary: the `config` it is sent
and the manifests' annotations come from the request, so the pipeline that
posts them decides what is suppressed.
[Suppressions and baselines](../guides/suppressions-and-baselines.md#who-can-turn-the-gate-off)
describes each input.

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

A `target` below 1.16, the oldest minor the knowledge base covers, is a 400
that says to quote the version, not a verdict: YAML makes an unquoted
`target: 1.30` the number 1.3, which would read ready.

A `target` newer than the server's knowledge base (a cluster already on its
newest minor, upgrading to the next) makes the verdict `unknown`, and an
`unknown` verdict fails the gate (422). Pass a `target` the release knows,
or add `&allow-incomplete=true`, the server's counterpart to
`allow-incomplete`: the gate then decides on findings alone. A blocker the
manifests introduce still answers 422, an `unknown` verdict without one
answers 200, and the `X-Upgradescope-Verdict` header, the body's `verdict`
and its required `notAssessed` gaps still say `unknown`. A `target` that is
not an upgrade of the cluster still fails. The parameter takes `true` or
`false` (anything else is a 422), and `format=junit` follows it too.

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
  one. A server with no read credential and an open read API answers a
  placeholder bearer with `401`: leave `READ_TOKEN` empty there, which
  sends `Bearer ` and counts as no bearer
  ([an unknown bearer is never the open read](../operations/auth.md)). A team-scoped token judges the PR against that team's share of the
  cluster only: its score and cluster verdict are the share's, and a PR
  that breaks only another team's workloads passes. Gate a repository
  that can break shared or other teams' resources with a fleet-wide
  token ([the gate with a team-scoped token](../operations/auth.md#the-gate-with-a-team-scoped-token)).

This example is run as written by `TestGateDocsExample`, against a server
holding the cluster it names, and must fail on a removed API. All
parameters and limits: [`POST /api/v1/gate`](../reference/api.md#gate).
