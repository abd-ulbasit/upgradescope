# Configuration

This page covers what a team can configure about which findings count:
ignore rules in `.upgradescope.yaml`, the `upgradescope.dev/ignore`
object annotations, baselines for CI, and the agent's
`ClusterReadiness` `spec.ignore`. Flags are listed in
`upgradescope scan --help`. The GitHub Action passes them as its
`config`, `baseline` and `write-baseline` inputs
([action/README.md](../action/README.md#ignore-rules-and-baselines)).

Contents:

1. [Ignore rules (`.upgradescope.yaml`)](#ignore-rules-upgradescopeyaml)
2. [Object annotations](#object-annotations)
3. [Baselines](#baselines)
4. [How the gate decides](#how-the-gate-decides)
5. [The agent: `spec.ignore`](#the-agent-specignore)
6. [Finding keys](#finding-keys)

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

| Field | Required | Meaning |
|---|---|---|
| `key` | one of `key`, `category` | Exact [finding key](#finding-keys). |
| `category` | one of `key`, `category` | Every finding of this category: `removed-api`, `deprecated-api`, `deprecated-api-in-use`, `eol-addon`, `eol-approaching`, `version-skew`, `chart-incompat`, `kb-stale`, `addon-no-data`. |
| `namespace` | no | Glob (`*`, `?`, `[...]`) on the object's namespace. |
| `name` | no | Glob on the object's name. |
| `file` | no | Glob on the manifest path, relative to the directory that holds the config file; `**` spans directories. Never matches live objects. |
| `reason` | **yes** | Why the finding is accepted. Shown wherever the finding is. |
| `expires` | no | `YYYY-MM-DD`, the last day (UTC) the rule applies. |

**Which findings a rule takes.** A rule without `namespace`, `name` or
`file` takes the whole finding, including objects beyond the 100 that a
finding lists. With selectors it takes only the listed objects that match
all of them. If every object is taken, and none were left unlisted, the
finding leaves the scored list. Otherwise it stays, with the remaining
objects and a note saying how many were suppressed. A finding that lists no
objects (add-ons, version skew) matches a `namespace` selector only when
every namespace it names matches, and never matches `name` or `file`.
Rules apply in order, and the first one that matches an object takes it.

**Expiry.** On the day after `expires`, the rule stops applying. The finding
counts again, and the scan prints a warning naming the rule, so an
acceptance cannot quietly outlive its reason.

**Errors.** A missing reason, both or neither of `key` and `category`, an
unknown category, a key that does not start with a category, a malformed
date or glob, and any unknown field all make the scan exit 1 before it
scans anything. A typo fails loudly instead of suppressing nothing.

**Where the file is found.** `upgradescope scan` uses `--config <path>` when
it is given. Otherwise it looks for `.upgradescope.yaml` in the scan root
(the `--files` directory, or the working directory for a live scan), then at
the root of the git repository that contains it. The first file found is
used, and files are never merged.

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
API.

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
  file, not line, so edits around an object do not matter), and the finding
  has no more objects in total than the baseline had;
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
the gap, even when every known blocker is in the baseline. `--fail-on
never` always exits 0. Operational errors, including an invalid config file
or baseline, exit 1.

Score and verdict exclude suppressed findings, but they include baseline
findings. A baseline only changes what fails the gate. It does not change
how ready the cluster is.

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
dashboard and gate do not apply `spec.ignore`.

## Finding keys

Every finding has a `key`: its category, a slash, and a stable
discriminator. The key never includes counts, so the same problem keeps the
same key from run to run while the title changes (for example, "3 objects"
becomes "2 objects"). Examples:

| Finding | Key |
|---|---|
| Removed or deprecated API | `removed-api/networking.k8s.io/v1beta1/Ingress`, `deprecated-api/core/v1/ComponentStatus` (the core group is `core`) |
| Deprecated API still requested | `deprecated-api-in-use/<group>/<version>/<resource>` |
| EOL add-on | `eol-addon/ingress-nginx`, or `eol-addon/<id>/<cycle>` for a release line |
| Version skew | `version-skew/kubelet-post-upgrade` |
| Knowledge base behind the target | `kb-stale` |

`--output json` shows the key of every finding, and the SARIF output uses
it as the rule id.
