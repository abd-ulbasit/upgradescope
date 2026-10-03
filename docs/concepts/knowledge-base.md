# Knowledge base

The knowledge base (KB) is what upgradescope knows about Kubernetes and the
add-ons around it. It has three parts, all **compiled into the binary**:
nothing is fetched at runtime, and a KB update reaches you only through a
new release.

| Part | Source | Maintained by |
|---|---|---|
| API lifecycle: for each group/version/kind, when it was introduced, deprecated and removed, and its replacement | generated from `k8s.io/api` source (`internal/kb/data/apilifecycle.json`), and nothing else: no hand-written overlay. The few facts the source lacks are fixups in the generator, each with a citation | `tools/gen-kb` |
| Add-on registry: end of life, release lines and Kubernetes compatibility of common add-ons | one YAML file per add-on in `registry/data/`, every claim cited | hand-curated, and synced with endoflife.date where it has the product ([Add-on registry](addon-registry.md)) |
| Version-skew policy | the upstream [version skew policy](https://kubernetes.io/releases/version-skew-policy/) | `internal/kb/skew.go` ([Version skew](version-skew.md)) |

`upgradescope version` prints what a binary carries:

```console
$ upgradescope version
...
  kb:            k8s.io/api v0.37.1; lifecycle 696a4b81; registry de96a5da
  kb horizon:    Kubernetes 1.37
  registry date: 2026-10-01
```

The KB version names the `k8s.io/api` release and a digest of each dataset,
so two binaries with the same KB version judge identically. Reports carry it
too (`kbVersion`). The registry date (the last change to the registry data)
is stamped into release builds; a `go install` build prints `unknown`.

## Generated, not copied

`tools/gen-kb` imports every group/version package of `k8s.io/api` and asks
each registered type for its generated `APILifecycleIntroduced`,
`APILifecycleDeprecated`, `APILifecycleRemoved` and
`APILifecycleReplacement` methods: the same source of truth the apiserver is
built from, never a hand-copied table. That also covers removals scheduled
further ahead than the human-written
[deprecation guide](https://kubernetes.io/docs/reference/using-api/deprecation-guide/).
CI regenerates the file on every change and fails when the committed copy
differs, and checks that the generator imports every `k8s.io/api`
group/version package.

Three things the generator adds to what the source says, each in
`tools/gen-kb` (`history.go`, `merge.go` and `fixups.go`) and tested:

- **Tombstones.** A type `k8s.io/api` deleted stays in the data, removed in
  the release that stopped serving it, so a manifest still using it blocks.
  The generator reads every `k8s.io/api` release since v0.17 for this.
- **Untagged types.** Some registered types carry no lifecycle markers, so
  the source says nothing about them. `rbac.authorization.k8s.io/v1alpha1`
  (removed in 1.23) and `node.k8s.io/v1alpha1` RuntimeClass (1.24) get their
  lifecycle from the Kubernetes release notes, cited in the generator. A
  test fails once upstream tags the type, so the entry cannot go stale
  quietly.
- **Non-resources.** An explicit list in the generator, each with its evidence,
  leaves out wrapper, subresource-body and payload types that
  kube-apiserver never stored as resources and that no manifest can create:
  `PodStatusResult`, `EphemeralContainers`, `ReplicationControllerDummy`,
  `JobTemplate` (`batch/v1beta1`, `batch/v2alpha1`), the `Scale` and
  `DeploymentRollback` bodies of the removed `apps` and `extensions`
  versions, the v1beta1 `AdmissionReview` and `ConversionReview`, the
  `policy/v1beta1` `Eviction` (the body of `pods/eviction`) and the
  `apidiscovery.k8s.io/v2beta1` `APIGroupDiscovery` (a discovery response
  format). The list is exactly these 14 kinds; any other kind
  `k8s.io/api` registers and tags is treated as a resource. A manifest of one is an
  `unknown-api` info, not a removal. The list is explicit rather than
  derived, because `k8s.io/api` registers these like any resource. A test
  checks every inferred removal left in the data against a list of types
  kube-apiserver served, so a new one cannot slip in unaudited.

Every other fact is `k8s.io/api`'s own. There is no separate hand-written
dataset; a test fails if one is added.

## The horizon

The newest minor the API dataset can describe is the newest one `k8s.io/api`
has released, `maxKnownK8s` in the dataset, shown as `kb horizon` by
`upgradescope version`. A target above it is allowed, but nothing can say
what that release removes, so the report gets a `kb-stale` warning and a
required `kb-coverage` gap, and the verdict is `unknown`
([Verdict and score](verdict-and-score.md)). This is also what happens to
the in-cluster agent's default target (the next minor) once a cluster runs
the horizon minor, until you upgrade to a release with a newer KB.

## How fresh it is

- A weekly workflow (`kb-refresh`) bumps `k8s.io/api`, regenerates the
  dataset, syncs the registry with endoflife.date and opens a pull request
  for review. Nothing changes the datasets without review, and a failing
  refresh opens an issue. It has failed for weeks at a time before ([#23](https://github.com/abd-ulbasit/upgradescope/issues/23)), so
  it is a helper, not a guarantee.
- A merged refresh changes nothing for users until it is released. There is
  no fixed release schedule yet; the [changelog](../changelog.md) lists what
  each release changed in the KB.
- So the KB in your binary is as fresh as the release you run. Upgrading the
  binary, image or chart is how you get new removals and EOL dates
  ([Upgrade](../operations/upgrade.md)).

## What it does not cover

- Registered types with no lifecycle markers and no release-note source are
  not judged, so they never block. A `scheduling.k8s.io/v1alpha3` Workload,
  PodGroup or CompositePodGroup (still served at 1.37) is an `unknown-api`
  info, and so are `imagepolicy.k8s.io/v1alpha1` ImageReview and
  `internal.apiserver.k8s.io/v1alpha1` StorageVersion: the dataset lists
  every group `k8s.io/api` registers (`builtinGroups`), whether or not it
  has entries, and a manifest of any of them the KB cannot place is an
  `unknown-api` info. Only groups outside that list (CRDs, aggregated APIs)
  produce no finding.
- CRD versions served by your own or third-party CRDs (deprecated CRD
  versions and stale `status.storedVersions`) are not in the KB: they are
  judged from the CRDs themselves, deprecated and unserved versions live or
  in `--files`, stale `status.storedVersions` live only
  ([#48](https://github.com/abd-ulbasit/upgradescope/issues/48)).
- Add-ons outside the registry are not judged: their images are listed as
  `unrecognizedImages` in the inventory and the report, and never become
  findings.
- Feature gates, flags and behaviour changes that are not API removals.
