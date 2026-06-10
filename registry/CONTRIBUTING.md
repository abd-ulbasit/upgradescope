# Contributing to the add-on registry

The registry (`registry/data/*.yaml`) is upgradescope's public, citation-backed
dataset of Kubernetes add-on EOL status and compatibility. Every claim must be
traceable to an upstream or public source — that is the whole point.

## How an entry is used

- `internal/collect` matches running pod images and Helm releases against
  `matchers`, producing detected add-on instances.
- `internal/engine` turns `support.status: eol` into a **blocker**, an
  `eol_date` within 90 days into a **warning**, and a target Kubernetes
  version above a `compat` row's `k8s_max` into a compat finding.

## Schema (`schema_version: 1`)

```yaml
schema_version: 1                  # required, must be 1
id: my-addon                       # required, kebab-case, unique, = file name
display_name: My Add-on            # required
endoflife_product: my-addon        # optional: endoflife.date slug (see below)
matchers:                          # at least one image OR chart required
  images:
    - registry.example.com/org/app # repo prefix match; tag becomes the version
  charts:
    - my-addon                     # exact Helm chart name
support:
  status: supported                # supported | eol | unknown
  eol_date: "2027-01-31"           # optional, YYYY-MM-DD
  citations:                       # ≥1 http(s) URL unless status is unknown
    - https://example.com/lifecycle
compat:                            # optional rows; each needs ≥1 citation
  - range: ">=2.0.0 <3.0.0"        # semver constraint on the add-on version
    k8s_min: "1.25"                # MAJOR.MINOR, inclusive
    k8s_max: "1.32"                # MAJOR.MINOR, inclusive
    citations:
      - https://example.com/compat-matrix
recommendation: Optional one-line remediation hint shown with findings.
```

### How matchers work

- **images** match by *repository prefix*: `docker.io/istio` matches
  `docker.io/istio/pilot` and `docker.io/istio/proxyv2`; the image tag
  (leading `v` stripped) is recorded as the detected version. Remember that
  pod specs contain the literal string users wrote — Docker Hub images often
  appear *without* the `docker.io/` prefix, so list both forms
  (`velero/velero` **and** `docker.io/velero/velero`).
- **charts** match the Helm chart name exactly; the chart version (not the
  app version) is the detected version, and chart evidence wins over image
  evidence when both match.

### `endoflife_product` — API-synced vs hand-curated entries

If the add-on is tracked by [endoflife.date](https://endoflife.date), set
`endoflife_product` to its slug (the path segment in
`https://endoflife.date/<slug>`). `tools/eol-sync` (run weekly by the
`kb-refresh` workflow, or via `make eol-sync`) then owns `support.status`
and `support.eol_date`:

- the add-on is `eol` only when its **newest release cycle** is EOL;
- a dated newest cycle records the date and flips status once it passes.

Do not hand-edit those two fields on synced entries — CI runs
`eol-sync -check` and fails on drift. Everything else (matchers, citations,
compat rows) stays hand-maintained.

If endoflife.date does not track the product, leave `endoflife_product`
out and maintain `support` by hand, citing the upstream lifecycle or
compatibility page.

### Citation rules

- Every `support` (unless `status: unknown`) and every `compat` row needs at
  least one resolving `http(s)` URL.
- Prefer primary sources: upstream release/support-policy docs, compatibility
  matrices, official blog announcements. endoflife.date product pages are
  fine *in addition* for synced entries.
- Only record **bounded** compat rows. If upstream says "1.18 to latest",
  there is no honest `k8s_max` — skip the row and put the matrix URL in
  `support.citations` instead (see `velero.yaml`).

## Adding an add-on, step by step

1. Create `registry/data/<id>.yaml` (file name = `id`, `.yaml` extension —
   `.yml` is rejected).
2. Fill in the template above; check whether endoflife.date tracks it.
3. If synced: run `make eol-sync` to let the tool write `status`/`eol_date`.
4. Validate: `go test ./registry/` (schema, citations, semver ranges,
   duplicate IDs) and add the entry to the curated-entries table in
   `registry/load_test.go`.
5. Run `make eol-check` — must report `in sync` / drift 0.

## PR checklist

- [ ] `id` is kebab-case and matches the file name
- [ ] at least one image or chart matcher, with Docker Hub short forms listed
- [ ] every citation URL opens in a browser (CI does not fetch them; you do)
- [ ] compat rows only where upstream publishes bounded ranges
- [ ] `endoflife_product` set when endoflife.date tracks the product, and
      `make eol-check` passes
- [ ] `go test ./registry/` passes, including your `load_test.go` row
