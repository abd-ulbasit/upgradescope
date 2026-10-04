# Contributing to the add-on registry

The registry (`registry/data/*.yaml`) is upgradescope's public, citation-backed
dataset of Kubernetes add-on EOL status and compatibility. Every claim must be
traceable to an upstream or public source — that is the whole point.

## How an entry is used

- `internal/collect` matches running pod images and Helm releases against
  `matchers`, producing detected add-on instances with an **app version**.
  Node container runtimes (`containerd://1.7.27`) are matched by
  `matchers.runtimes`.
- `internal/engine` judges each detected add-on at its installed version:
  - `support.status: eol` (or a past `support.eol_date`) → **blocker**, a
    `support.eol_date` within 90 days → **warning**. These product-level
    fields are for products retired as a whole (ingress-nginx).
  - otherwise the version is mapped to its release line in `cycles`: a line
    that has ended → **blocker**, one ending within 90 days → **warning**.
    A node runtime's ended line is only a **warning** naming the nodes: the
    runtime comes with the node image, not the Kubernetes version. Put the
    Kubernetes release that drops it in a `compat` row (see `containerd.yaml`);
  - a target Kubernetes version outside the line's `k8s_min`/`k8s_max`, or
    outside the bounds of the first `compat` row whose range matches the
    version → **blocker** (`chart-incompat`);
  - no product date and no cycle for the version, or no version at all →
    an **info** finding ("no lifecycle data for …"), never a blocker.

## Schema (`schema_version: 2`)

```yaml
schema_version: 2                  # required, must be 2
id: my-addon                       # required, kebab-case, unique, = file name
display_name: My Add-on            # required
endoflife_product: my-addon        # optional: endoflife.date slug (see below)
matchers:                          # at least one image, chart or runtime
  images:
    - org/app                      # repository path, no host/tag (see below)
  charts:
    - my-addon                     # exact Helm chart name
  runtimes:
    - containerd                   # node container runtime name
support:
  status: supported                # supported | eol | unknown
  eol_date: "2027-01-31"           # optional, YYYY-MM-DD: whole-product EOL only; not with unknown
  citations:                       # ≥1 http(s) URL unless status is unknown; replace the example.com placeholders (rejected)
    - https://example.com/lifecycle
cycles:                            # release lines of the APP version
  - {cycle: "2.1", eol: "2027-06-30", k8s_min: "1.30", k8s_max: "1.34", citations: ["https://example.com/lifecycle"]}
  - {cycle: "2.0", eol: true, citations: ["https://example.com/lifecycle"]}
compat:                            # optional rows; each needs ≥1 citation
  - range: ">=2.0.0 <3.0.0"        # semver constraint on the app version
    k8s_min: "1.25"                # MAJOR.MINOR, inclusive; optional
    k8s_max: "1.32"                # MAJOR.MINOR, inclusive; optional
    citations:                     # (at least one of k8s_min/k8s_max)
      - https://example.com/compat-matrix
recommendation: Optional one-line remediation hint shown with findings.
```

### How matchers work

- **images** are repository paths *without* the registry host, tag or
  digest. A matcher of two or more segments is matched as a suffix on whole
  path segments; a one-segment matcher (`etcd`) is the repository exactly
  and never a suffix, so list a product's other paths in full
  (`bitnami/etcd`) and a bare `controller` or `operator` claims nothing but
  a repository of that name. `ingress-nginx/controller`
  matches `registry.k8s.io/ingress-nginx/controller`, the legacy
  `k8s.gcr.io/ingress-nginx/controller`, a mirror such as
  `harbor.example/k8s/ingress-nginx/controller` and an ECR pull-through cache
  path. References are normalised first: `traefik:v3.1` is
  `docker.io/library/traefik`, so the matcher is `library/traefik`. List
  every image whose tag carries the add-on's own version (`istio/proxyv2`,
  `istio/pilot`, …), and leave out images that version separately
  (`tigera/operator`, Flux's controllers). Vendor forks with their own
  support (AKS application routing, RKE2) get their own entries.
- **Any-prefix opt-in (`"*/name"`)** — a one-segment matcher is exact, so a
  product that is pulled from whatever mirror the operator chose (etcd,
  behind a kubeadm `imageRepository` or a Harbor proxy cache) cannot list
  every path. Writing the matcher as `"*/etcd"` (quote it: a YAML plain
  scalar cannot start with `*`) declares that the final segment `etcd`
  matches under any registry host or prefix, bare or not:
  `registry.k8s.io/etcd`, `bitnamilegacy/etcd`, `harbor.corp/k8s/etcd`. It
  takes exactly one segment (a longer path already matches under any
  prefix) and only a distinctive name: the validator rejects generic ones
  (`controller`, `operator`, `server`, `agent`, `proxy`, `manager`,
  `webhook` and similar), which other products publish too, and the
  embedded registry uses it for etcd alone (`TestAnyPrefixMatchersAreEtcdOnly`).
  A provider build is still claimed only by a matcher naming the provider.
- **Provider builds** — images under `gke.gcr.io/`, `gcr.io/gke-release/`
  or `mcr.microsoft.com/` (`ProviderBuildPrefixes` in `providers.go`), such
  as GKE's Calico and Dataplane V2 Cilium or AKS's Calico, Cilium, Istio and
  KEDA — follow the provider's support policy, so host-less matchers never
  match them: an upstream line's EOL is not theirs. An entry for a provider
  build is the one place a matcher names its host
  (`mcr.microsoft.com/oss/kubernetes/ingress/nginx-ingress-controller`, see
  `aks-app-routing-nginx.yaml`); it still matches through a mirror.
- The version is read from anywhere in the tag: `v1.9.4`,
  `nginx-1.9.4-hardened1` and `1.9.4-debian-12-r0` all mean 1.9.4.
- **charts** match the Helm chart name exactly. The release's `appVersion`
  is the detected version (falling back to the image tag when a chart has
  none); the chart version is shown as evidence only. Write every range and
  cycle in **app** versions.
- **runtimes** match the scheme of a node's `containerRuntimeVersion`.

### `cycles` and `endoflife_product` — API-synced vs hand-curated entries

`cycles` are the add-on's release lines, keyed on the app version: `cycle`
holds the leading components ("1.31" covers 1.31.x), `eol` is a date, `true`
(ended, no date published) or `false` (no end announced), and the optional
`k8s_min`/`k8s_max` give the Kubernetes versions that line supports. Quote
every version (`cycle: "1.10"`): unquoted, YAML reads 1.10 as the number
1.1, so the loader rejects it.

If the add-on is tracked by [endoflife.date](https://endoflife.date), set
`endoflife_product` to its slug (the path segment in
`https://endoflife.date/<slug>`) and cite that page in `support.citations`.
`tools/eol-sync` (run weekly by the `kb-refresh` workflow, or via
`make eol-sync`) then owns the `cycles:` block: it writes every cycle the API
publishes, with the Kubernetes range where the product publishes one. Do not
hand-edit synced cycles — `make eol-check` reports drift. eol-sync never
touches `support`: setting `status: eol` for a retired product stays a
human decision (eol-sync prints a note when every cycle has ended).
Everything else (matchers, citations, compat rows) stays hand-maintained.

If endoflife.date does not track the product, leave `endoflife_product`
out, maintain `support` by hand, and add `cycles` only when you can cite a
per-version lifecycle source.

### Citation rules

- Every `support` (unless `status: unknown`), every cycle and every `compat`
  row needs at least one resolving `http(s)` URL.
- `registry.Validate` rejects a citation on a placeholder or local host
  (`example.com`, `example.org`, `example.net`, `*.test`, `*.invalid`,
  `localhost`, a single-label host, any IP address). It cannot tell a wrong
  real URL from a right one: CI does not fetch citations, so the checklist
  below is yours.
- `eol_date` of a product retired as a whole is a date a vendor page states
  (Promtail, Grafana Agent) or, for a project with no end-of-support notice,
  the day its repository was archived as the repository page shows it
  (Kubernetes Dashboard, Weave Net). Where neither is available, leave the
  date out rather than infer one: `status: eol` alone is a blocker.
- A date is a claim, so `eol_date` needs `status: supported` or `eol`;
  `status: unknown` (which needs no citation) with a date is rejected, since
  it would print an uncited end-of-life blocker.
- Prefer primary sources: upstream release/support-policy docs, compatibility
  matrices, official blog announcements. endoflife.date product pages are
  fine *in addition* for synced entries.
- A compat bound needs a source that states it. If upstream says "1.18 to
  latest", there is no honest `k8s_max`; record only the bound upstream
  publishes, or skip the row and put the matrix URL in `support.citations`
  (see `velero.yaml`).
- When a source gives a month but findings print a day, say in a YAML
  comment where the day comes from (see `ingress-nginx.yaml`,
  `aks-app-routing-nginx.yaml`).

## Managed-provider support calendars

`registry/data/providers/{eks,gke,aks}.yaml` are a dataset of their own
(`schema_version: 1`, `registry.ProviderSupport`): for each Kubernetes
minor of a managed service, when standard support ends and when extended
support ends. `internal/engine` uses them for the `support-lifecycle`
finding (see [managed-provider
support](https://abd-ulbasit.github.io/upgradescope/concepts/support-lifecycle/)).

```yaml
schema_version: 1
id: eks                          # eks | gke | aks, = file name
display_name: Amazon EKS
endoflife_product: amazon-eks    # optional: tools/eol-sync owns `versions`
extended_support_note: One sentence on what extended support means here.
extended_support_condition: the cluster is on ...   # only where extended support is opt-in (GKE, AKS)
citations: [https://docs.aws.amazon.com/eks/latest/userguide/kubernetes-versions.html]
pricing:                         # optional, hand-entered, USD per cluster-hour
  currency: USD
  standard_per_cluster_hour: 0.10
  extended_per_cluster_hour: 0.60   # the whole price in extended support
  as_of: "2026-10-03"               # the day you read the pricing page
  note: charged only for ...        # optional: whom the price applies to
  citations: [https://aws.amazon.com/eks/pricing/]
versions:                        # newest first
  - {minor: "1.34", standard_end: "2026-12-02", extended_end: "2027-12-02"}
  - {minor: "1.20", standard_end: "2022-11-01"}   # no extended support offered
```

- `standard_end` is the day extended support begins (EKS bills from the
  start of that day, UTC); `extended_end` is the day the provider stops
  supporting the minor, and must be later. Quote every minor.
- `endoflife_product` is set for EKS and AKS: `tools/eol-sync` (`make
  eol-sync`) rewrites `versions` from the API, mapping `eol` to
  `standard_end` and `extendedSupport` to `extended_end`, and `make
  eol-check` reports drift. GKE has no slug (endoflife.date has no extended
  end for it and its standard dates differ from Google's) and is read by
  hand from Google's release schedule; leave out minors whose dates are
  still a month or quarter estimate.
- **A price is entered only when the provider's own pricing page states it
  unambiguously**, with that page as the citation and the day you read it
  as `as_of`. Leave `pricing` out otherwise (AKS): the finding then has
  dates and no cost line. Never derive a number. Prices are list prices;
  the finding labels them so.
- `extended_support_condition` is set for a provider whose extended support
  is not automatic (GKE: "the cluster is on the Extended release channel";
  AKS: "Long Term Support is enabled"): a bare clause that completes "only
  if ...", with no leading "if" and no trailing period. The collector cannot
  see a cluster's channel or tier, so the finding then says the provider's
  extended window applies only under that condition instead of asserting the
  cluster is in it. Leave it out where extended support is the default
  (EKS).
- A minor missing from `versions` gets no finding. Files in
  `registry/data/providers` must be `<id>.yaml`.

## Adding an add-on, step by step

1. Create `registry/data/<id>.yaml` (file name = `id`, `.yaml` extension;
   `go test ./registry/...` fails on a `.yml` or any other file in
   `registry/data`, which the embed would skip).
2. Fill in the template above; check whether endoflife.date tracks it.
3. If synced: run `make eol-sync` to let the tool write `cycles`.
4. Validate: `go test ./registry/...`. The tests check every data file
   (schema, matchers, citations, semver ranges, id = file name, cycles on
   synced entries); no Go change is needed for a new entry.
5. Run `make eol-check` — must report `in sync` / drift 0.

## Covering add-ons only you run: `--registry-dir`

An add-on the registry lacks (an in-house controller, a product too niche
to ship) does not need a pull request to be judged in your fleet.
`upgradescope scan`, `agent` and `serve` take `--registry-dir <path>`, a
single `<id>.yaml` file or a directory of them, in the schema above.

- Extra entries pass the same validator as the embedded ones (schema,
  citations, id = file name): an invalid file stops the command at start
  with an error naming the file, and a path with no `*.yaml` entry is an
  error too, so an unmounted ConfigMap cannot silently leave add-ons unjudged.
- An extra entry whose `id` is an embedded entry's **replaces** it entirely,
  nothing is merged field by field: copy the embedded file and edit it to
  correct a date for your fleet or add a mirror's image path. Any other id
  adds an add-on.
- No image or chart may be claimed by two entries. An extra entry whose
  matcher claims an image or chart that an embedded entry of another id
  already claims (the same repository, or a longer mirror path of it) stops
  the command at start, naming both entries: replace the embedded entry
  instead, by using its id. An `id` that is another entry's chart name (or
  a chart that is another entry's id) is refused the same way, because a
  pod's `app.kubernetes.io/name` label names an add-on by either.
- The entries are part of the knowledge base version a report carries.
- `serve` judges what agents push and what `/gate` is posted against its
  own registry: give it the same `--registry-dir` as the agents. In the
  Helm chart, `agent.extraRegistry` renders the ConfigMap and the flag.

## PR checklist

- [ ] `id` is kebab-case and matches the file name
- [ ] image matchers are host-less repository paths (a provider-build
      entry: host-qualified) covering every image
      that carries the add-on's version; no images that version separately
- [ ] versions, ranges and cycles are app versions, not chart versions
- [ ] every citation URL opens in a browser (CI does not fetch them; you do)
- [ ] compat bounds only where upstream publishes them
- [ ] `endoflife_product` set when endoflife.date tracks the product, and
      `make eol-check` passes
- [ ] `go test ./registry/...` passes
