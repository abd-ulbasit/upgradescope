# Suppressions and baselines

This page covers what a team can configure about which findings count:
ignore rules in `.upgradescope.yaml`, the `upgradescope.dev/ignore`
object annotations, baselines for CI, the server's gate, and the agent's
`ClusterReadiness` `spec.ignore`. The file format, field by field, is in the
[configuration file reference](../reference/config.md); the flags in
[`upgradescope scan`](../reference/cli/upgradescope_scan.md). The GitHub
Action passes them as its `config`, `baseline` and `write-baseline` inputs
([action/README.md](https://github.com/abd-ulbasit/upgradescope/blob/main/action/README.md#ignore-rules-and-baselines)).

Contents:

1. [Ignore rules (`.upgradescope.yaml`)](#ignore-rules-upgradescopeyaml)
2. [Object annotations](#object-annotations)
3. [Baselines](#baselines)
4. [How the gate decides](#how-the-gate-decides)
5. [The server gate](#the-server-gate)
6. [The agent: `spec.ignore`](#the-agent-specignore)
7. [Finding keys](#finding-keys)
8. [Who can turn the gate off](#who-can-turn-the-gate-off)

## Ignore rules (`.upgradescope.yaml`)

An ignore rule accepts a finding that the team knows about and has decided
to live with for now, such as an EOL ingress-nginx that is being migrated,
or a removed API in manifests that are never deployed. A suppressed finding:

- does not count toward the score, the verdict or the `--fail-on` gate;
- is still reported, with its reason, in every output format: a `SUPPRESSED`
  section and an `N suppressed` line in the table, a `suppressed` array in
  JSON, and SARIF results that carry a `suppressions` entry (`kind:
  external`, `justification` = the reason), so GitHub code scanning shows
  them as dismissed instead of dropping them.

```yaml
# .upgradescope.yaml
ignore:
  # A whole finding, by key, until a date.
  - key: eol-addon/ingress-nginx
    reason: migrating to Gateway API, tracked in PLAT-123
    expires: 2026-12-31

  # Every removed-API finding, but only for objects under legacy/.
  - category: removed-api
    file: legacy/**
    reason: legacy/ is kept for reference and never deployed

  # One object.
  - key: removed-api/networking.k8s.io/v1beta1/Ingress
    namespace: shop
    name: admin
    reason: admin ingress is deleted in the next release
```

Every field, the categories a rule can name, and what makes a file
invalid are in the [configuration file reference](../reference/config.md).

**Which findings a rule takes.** A rule without `namespace`, `name` or
`file` takes the whole finding, including objects beyond the 100 that a
finding lists. With selectors it takes only the listed objects that match
all of them. If every object is taken, and none were left unlisted, the
finding leaves the scored list. Otherwise it stays, with the remaining
objects and a note saying how many were suppressed. A finding that lists no
objects (add-ons, version skew) matches a `namespace` selector only when
every namespace it names matches, and never matches `name` or `file`.
Such a finding names at most 100 namespaces and counts the rest, which
cannot be shown to match, so past 100 a `namespace` rule no longer takes
it, even with a glob such as `*` that would match them all; use a rule
without selectors (by `key`) for it.
Rules apply in order, and the first one that matches an object takes it.

**Expiry.** On the day after `expires`, the rule stops applying. The finding
counts again, and the scan prints a warning naming the rule, so an
acceptance cannot quietly outlive its reason.

**Errors and discovery.** An invalid file makes the scan exit 1 before it
scans anything, so a typo fails loudly instead of suppressing nothing.
`--config <path>` names the file; otherwise `.upgradescope.yaml` is looked
up in the scan root, then at the repository root
([details](../reference/config.md#where-the-file-is-found)).

## Object annotations

An object can opt out of findings itself:

```yaml
metadata:
  annotations:
    upgradescope.dev/ignore: removed-api, deprecated-api   # categories or keys, comma-separated
    upgradescope.dev/ignore-reason: replaced by the HTTPRoute in routes.yaml
```

The annotation works in `--files` mode (rendered manifests) and on live
objects, which the collector reads from object metadata. It applies to
findings that list objects, which today are the API-usage findings. The
reason is required: an `ignore` annotation without `ignore-reason` is not
applied, and the scan prints a warning. Annotations have no expiry.
Annotations only work on the objects that a finding lists, at most 100 per
API. On a live cluster the collector keeps at most 16 KiB of each
annotation (of `ignore`, the categories and keys that fit whole), as the
server takes it.

## Baselines

A baseline lets CI adopt the gate on an existing estate: the gate fails
only on problems that are new since the baseline, and everything already
known stays visible.

```bash
# Once, on the main branch: record what exists today.
upgradescope scan --files rendered/ --target 1.36 --fail-on never --write-baseline upgradescope-baseline.json

# On every PR: fail only on what the change adds.
upgradescope scan --files rendered/ --target 1.36 --baseline upgradescope-baseline.json
```

`--write-baseline <path>` writes the scan's JSON report, the same document
that `--output json` produces, after suppression. `--baseline <path>` reads
such a report. Any `--output json` report also works as a baseline. Each
finding is then marked:

- `unchanged` if the baseline has a finding with the same key, every object
  the current finding lists appears in it (matched by namespace, name and
  file, not line, so edits around an object do not matter), the finding
  has no more objects in total than the baseline had, and its severity has
  not risen since the baseline;
- `new` otherwise.

The mark is `baselineState` in JSON and SARIF, `(in baseline)` after the
title in the table, and a `BASELINE  N unchanged, M new` line under the
score. Only the baseline's `findings` count. Its `suppressed` entries do
not, so when a suppression expires, the finding is new again.

Baseline matching relies on finding keys staying stable across runs and
versions (see [Finding keys](#finding-keys)).

## How the gate decides

`--fail-on blocker|warning` exits 2 when, after suppression, a finding at or
above the threshold remains that is not `unchanged` against the baseline.
If none does, it still exits 2 when a required check was not assessed,
unless `--allow-incomplete` is given: a new blocker could be hiding behind
the gap, even when every known blocker is in the baseline. A `--target`
that is not an upgrade of the cluster (a downgrade, the same minor, or a
typo) exits 2 even with `--allow-incomplete`. `--fail-on never` always
exits 0. Operational errors, including an invalid config file
or baseline, exit 1.

Score and verdict exclude suppressed findings, but they include baseline
findings. A baseline only changes what fails the gate. It does not change
how ready the cluster is.

## The server gate

`POST /api/v1/gate` on `upgradescope serve` suppresses as `scan` does,
with the same code. It applies the `upgradescope.dev/ignore` annotations of
the posted objects (and, with `?cluster=`, of the cluster's stored
objects), and the ignore rules of a `.upgradescope.yaml` sent, URL-encoded,
in the `config` query parameter:

```sh
curl -sS --fail-with-body -X POST \
  "$SERVER/api/v1/gate?target=1.37&cluster=prod-eu-1&path=rendered.yaml" \
  --url-query "config@.upgradescope.yaml" \
  -H "Authorization: Bearer $READ_TOKEN" \
  -H "Content-Type: application/x-yaml" \
  --data-binary @rendered.yaml
```

`--url-query` needs curl 7.87 or later; with an older curl, append
`&config=` and the file's URL-encoded text to the URL yourself. Rule `file`
globs match the `path` parameter. The config is parsed and validated by
the same code as `scan --config`: an invalid config (an unknown field, a
rule without a `reason`, an `expires` that is not a date), one over 32 KiB,
or `config` given twice is refused with 422 before anything is judged.
32 KiB is a few hundred rules.

The parameter travels in the URL, so two limits come first for a large
config. URL-encoding expands YAML (a colon or a line break becomes three
bytes, and so may each space of indentation), so a config under 32 KiB can exceed the server's
64 KiB limit on the request line and headers once encoded; Go's HTTP
server then answers 431 with a plain-text body, not the JSON 422. A
reverse proxy in front of `serve` usually allows far less: ingress-nginx's
default `large-client-header-buffers` (8 KiB) answers 414 for a request
line longer than that. Keep the config small, or raise the proxy's limit.

Suppressed findings count toward neither the verdict nor `fail-on`. With
`?cluster=`, what the manifests introduce is decided after suppression: a
finding whose posted objects are all suppressed is not the pull request's,
and objects the cluster already has at that key stay the cluster's. The
JSON answer lists them in `suppressed` with a `suppressedCount`, and names
expired rules and annotations without a reason in `warnings`. SARIF carries
the manifests' suppressed findings as results with an external suppression,
JUnit as skipped test cases, and GitLab Code Quality leaves them out, as
`scan` does. With `?cluster=`, a suppressed finding that is the pull
request's only because the gate fails closed (for example a posted CRD
that drops a stored version) is in the JSON `suppressed` list but not in
the SARIF or JUnit answer.

The server gate has no baseline input. With `?cluster=`, the cluster's
stored state is its baseline: findings the cluster already has are tagged
`source: cluster` and do not fail the gate, and only what the manifests
introduce counts. Debt in the manifests themselves (a removed API that every
render still contains) is accepted with `scan --files --baseline` (the
GitHub Action's `baseline` input) or with ignore rules.

## The agent: `spec.ignore`

The in-cluster agent reads the same rules from its `ClusterReadiness`
object and applies them, along with the object annotations, on every tick:

```yaml
apiVersion: upgradescope.dev/v1alpha1
kind: ClusterReadiness
metadata:
  name: cluster
spec:
  ignore:
    - key: eol-addon/ingress-nginx
      reason: migrating to Gateway API, tracked in PLAT-123
      expires: "2026-12-31"
```

Each `status.targets[]` entry counts suppressed findings in `suppressed`.
They are not included in `blockers`, `warnings` and `infos`, in the score or
in the verdict. Expired or invalid rules, and annotations without a reason,
are listed in `status.notAssessed` and are not applied. The CRD schema
already rejects a rule without a reason or with a malformed `expires`. The
agent catches the rest (for example, both `key` and `category`). `file`
never matches live objects.

The agent still pushes the raw inventory to the server. The server's
dashboard does not apply `spec.ignore`, and neither does its gate, which
applies the rules sent with each request instead
([The server gate](#the-server-gate)).

## Finding keys

Every finding has a `key`: its category, a slash, and a stable
discriminator. The key never includes counts, so the same problem keeps the
same key from run to run while the title changes (for example, "3 objects"
becomes "2 objects"). Examples:

| Finding | Key |
|---|---|
| Removed or deprecated API | `removed-api/networking.k8s.io/v1beta1/Ingress`, `deprecated-api/core/v1/ComponentStatus` (the core group is `core`) |
| Objects of a flagged API nothing can be attributed to | `deprecated-api/<group>/<version>/<kind>/authorship-unknown` (info) |
| Deprecated API still requested | `deprecated-api-in-use/<group>/<version>/<resource>` |
| EOL add-on | `eol-addon/ingress-nginx`, or `eol-addon/<id>/<cycle>` for a release line |
| Version skew | `version-skew/kubelet-post-upgrade`, `version-skew/<component>-newer` or `-behind`, `version-skew/upgrade-path` |
| Unknown built-in API | `unknown-api/<group>/<version>/<kind>` |
| CRD version | `crd-version/unserved/<group>/<version>/<kind>`, `crd-version/deprecated/…`, `crd-version/stored-unserved/…` |
| Managed-provider support | `support-lifecycle/<provider>/<minor>`, e.g. `support-lifecycle/eks/1.34` |
| Knowledge base behind the target | `kb-stale` |

`--output json` shows the key of every finding, and the SARIF output uses
it as the rule id.

## Who can turn the gate off

Everything on this page that makes a finding stop counting is a file in the
repository being judged: the ignore rules, the annotations in the manifests
and the baseline. In CI on a `pull_request` those files come from the pull
request's own tree, so **a pull request can suppress its own findings and
the gate then passes.** Nothing is hidden (the suppressed findings, their
reasons and the rule's file are in the table, the step summary, the JSON and
the SARIF, and the change is in the diff), but the gate does not stop an
author who wants past it unless you pin its inputs:

- **Ignore rules.** `.upgradescope.yaml` is found in the scan root, then at
  the repository root, so a pull request that adds one next to a manifest
  with a removed API turns exit 2 into exit 0. `--config <path>` (the
  Action's `config`) names the one file to read and stops that search.
- **Annotations.** `upgradescope.dev/ignore` with `ignore-reason` on an
  object always applies; no flag turns it off. Review the suppressed table,
  or fail the job when a suppression came from an annotation
  (`jq -e '[.suppressed[]? | select(.source == "annotation")] | length == 0'`
  on the JSON report).
- **The baseline.** `--baseline <path>` (the Action's `baseline`) reads
  whatever file it is given, so a pull request can commit a baseline that
  holds its own findings.

To pin the config and the baseline, take both from the base commit: check it
out into another directory, copy the two files over the pull request's, and
name them in the Action. The files must exist at the base (`ignore: []` is a
valid empty config), and a pull request that changes them is judged by the
old rules, so accepting a finding takes a pull request of its own that
changes only those files. `CODEOWNERS` with required code-owner review on the
two files and on `.github/workflows/` covers what a checkout cannot, and
the workflow can be edited by a pull request too:

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
      - uses: actions/checkout@v7            # the base commit, only the two files
        with:
          ref: ${{ github.event.pull_request.base.sha }}
          path: trusted
          sparse-checkout: |
            /.upgradescope.yaml
            /upgradescope-baseline.json
          sparse-checkout-cone-mode: false
          persist-credentials: false
      - name: Take the config and baseline from the base commit
        run: cp trusted/.upgradescope.yaml trusted/upgradescope-baseline.json .
      - run: helm template my-release ./chart --output-dir rendered
      - uses: abd-ulbasit/upgradescope@v0.2.0
        id: gate
        with:
          path: rendered
          target: "1.37"
          version: v0.2.0
          config: .upgradescope.yaml           # named, so no other config is looked for
          baseline: upgradescope-baseline.json
```

The [CI gate page](../getting-started/ci-gate.md#who-can-turn-the-gate-off)
has the full trust table and the annotation guard.
