# Configuration file

`.upgradescope.yaml` holds ignore rules for `upgradescope scan`: findings a
team has accepted, each with a reason and, optionally, an expiry date. It is
the only file `scan` reads besides the manifests and a `--baseline`. How
suppression interacts with the score, the verdict, baselines and SARIF is in
[Suppressions and baselines](../guides/suppressions-and-baselines.md).

A test (`TestDocsConfigReference`) loads the example below with the real
loader and checks that this page lists every field and category the loader
accepts.

```yaml
# .upgradescope.yaml
ignore:
  - key: eol-addon/ingress-nginx
    reason: migrating to Gateway API, tracked in PLAT-123
    expires: 2026-12-31
  - category: removed-api
    file: legacy/**
    reason: legacy/ is kept for reference and never deployed
  - key: removed-api/networking.k8s.io/v1beta1/Ingress
    namespace: shop
    name: admin
    reason: admin ingress is deleted in the next release
```

## Top level

| Key | Type | Meaning |
|---|---|---|
| `ignore` | list of rules | The ignore rules, applied in order; the first rule that matches an object takes it. |

Any other key is an error.

## Rule fields

| Field | Required | Meaning |
|---|---|---|
| `key` | one of `key`, `category` | Exact finding key, such as `eol-addon/ingress-nginx`. Every finding prints its key in `--output json`, and SARIF uses it as the rule id. |
| `category` | one of `key`, `category` | Every finding of this category (list below). |
| `namespace` | no | Glob (`*`, `?`, `[...]`) on the object's namespace. A finding without objects (add-ons, version skew) matches only when every namespace it names matches. |
| `name` | no | Glob on the object's name. Never matches a finding without objects. |
| `file` | no | Glob on the manifest path, relative to the directory that holds the config file; `**` spans directories. Never matches live objects. |
| `reason` | **yes** | Why the finding is accepted. Shown wherever the finding is. |
| `expires` | no | `YYYY-MM-DD`, the last day (UTC) the rule applies. From the next day the rule stops applying and the scan prints a warning naming it. |

A rule without `namespace`, `name` or `file` takes the whole finding. With
selectors it takes only the objects that match all of them.

## Categories

`removed-api`, `deprecated-api`, `deprecated-api-in-use`, `eol-addon`,
`eol-approaching`, `version-skew`, `chart-incompat`, `kb-stale`,
`addon-no-data`. What each means and its severity:
[Verdict and score](../concepts/verdict-and-score.md#severity-by-category).

## Where the file is found

1. `--config <path>`, when given.
2. Otherwise `.upgradescope.yaml` in the scan root: the `--files` directory,
   or the working directory for a live scan.
3. Otherwise `.upgradescope.yaml` at the root of the repository that
   contains the scan root.

The first file found is used; files are never merged. The GitHub Action's
`config` input is `--config`.

## Errors

Each of these makes `scan` exit 1 before it scans anything:

- a rule without `reason`;
- both `key` and `category`, or neither;
- an unknown category, or a `key` that does not start with a known category;
- an `expires` that is not `YYYY-MM-DD`, or a malformed glob;
- any field or top-level key not listed on this page.

## Object annotations

Objects can carry their own suppression; it applies to findings that list
objects (the API-usage findings), in manifests and on live objects, and has
no expiry:

| Annotation | Meaning |
|---|---|
| `upgradescope.dev/ignore` | Comma-separated categories or finding keys this object opts out of. |
| `upgradescope.dev/ignore-reason` | Required. Without it the `ignore` annotation is not applied, and the scan warns. |

## In the cluster

The agent takes the same rules from its `ClusterReadiness` object's
`spec.ignore` ([CRD reference](crd.md)); the CRD schema requires `reason` and
validates `expires`.
