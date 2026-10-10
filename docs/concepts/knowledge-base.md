# Knowledge base

The knowledge base (KB) is what upgradescope knows about Kubernetes and the
add-ons around it. It has five parts, all **compiled into the binary**:
nothing is fetched at runtime, and a KB update reaches you only through a
new release.

| Part | Source | Maintained by |
|---|---|---|
| API lifecycle: for each group/version/kind, when it was introduced, deprecated and removed, and its replacement | generated from `k8s.io/api` source (`internal/kb/data/apilifecycle.json`), and nothing else: no hand-written overlay. The few facts the source lacks are fixups in the generator, each with a citation | `tools/gen-kb` |
| Migration notes: what a manifest needs besides a new `apiVersion`, for the kinds whose replacement is not a drop-in or that have none | hand-written in `internal/kb/data/migrations.json`, every note cited; merged onto the lifecycle data when the KB loads, and never written by `tools/gen-kb` or the weekly refresh | hand-curated ([Migration notes](#migration-notes)) |
| Add-on registry: end of life, release lines and Kubernetes compatibility of common add-ons | one YAML file per add-on in `registry/data/`, every claim cited | hand-curated, and synced with endoflife.date where it has the product ([Add-on registry](addon-registry.md)) |
| Version-skew policy | the upstream [version skew policy](https://kubernetes.io/releases/version-skew-policy/) | `internal/kb/skew.go` ([Version skew](version-skew.md)) |
| Managed-provider support calendars: for EKS, GKE and AKS, when each Kubernetes minor leaves standard support and when extended support ends, and where the provider publishes it, the extended-support list price | one YAML file per provider in `registry/data/providers/`, every date and price cited | EKS and AKS synced with endoflife.date, GKE and the prices hand-curated ([Managed-provider support](support-lifecycle.md)) |

`upgradescope version` prints what a binary carries:

```console
$ upgradescope version
...
  kb:            k8s.io/api v0.37.1; lifecycle 696a4b81; registry de96a5da
  kb horizon:    Kubernetes 1.37
  registry date: 2026-10-01
```

The KB version names the `k8s.io/api` release and a digest of each dataset (the lifecycle digest covers the migration notes and the in-tree volume plugins too, their StorageClass provisioners included, so editing a note or a plugin changes it; the registry digest covers the add-ons and the provider calendars),
so two binaries with the same KB version judge identically. The digests
shown in this documentation's examples are illustrative: a digest changes
with any edit to its dataset, so run `upgradescope version` for your
build's. Reports carry it
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
Those removals are `k8s.io/api` lifecycle defaults, not shipped releases, and
can change before the release ships: a removal after the horizon
(`maxKnownK8s`) is titled "(projected)" in the report, as a deprecation after
it is. CI regenerates the file on every change and fails when the committed copy
differs, and checks that the generator imports every `k8s.io/api`
group/version package.

Five things the generator adds to what the source says, each in
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
- **A removal dated by what kube-apiserver served.** `k8s.io/api` tags
  `storage.k8s.io/v1alpha1` VolumeAttachment for removal in 1.24, but
  kube-apiserver 1.23 has no storage for it (the registry dropped it with the
  beta APIs removed in 1.22, kubernetes/kubernetes#104248), so the
  knowledge base dates it 1.23 and marks the removal inferred. The override
  is cited in the generator and only applies while it is earlier than the
  tag.
- **A migration target for the alpha and beta types that lack one.**
  `k8s.io/api` tags a replacement on some removed and deprecated types
  only. A deprecated or removed type with none gets the newest GA version
  of its kind (`admissionregistration.k8s.io/v1beta1`
  ValidatingAdmissionPolicy to `v1`, `networking.k8s.io/v1beta1` ServiceCIDR
  to `v1`), so a removed-API blocker says what to migrate to. The
  generator logs each one with the migration guide and the changelog of the
  release that introduced the successor, a test lists all 33, and a
  successor the target does not serve yet is never recommended
  ([remediations](../concepts/verdict-and-score.md)).
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
lifecycle dataset; a test fails if one is added. (The migration notes below
are remediation text, not lifecycle facts, and change no date or version.)

## Migration notes

A generated hint says *where* to go (`migrate to networking.k8s.io/v1
Ingress`). For some kinds that is not enough: an Ingress, a
CustomResourceDefinition or a webhook configuration with only its
`apiVersion` changed is rejected, and `PodSecurityPolicy` has no successor
kind at all. `internal/kb/data/migrations.json` holds a short note for each,
`{group, version, kind, note, citations[]}`, written from the Kubernetes
[deprecation guide](https://kubernetes.io/docs/reference/using-api/deprecation-guide/)
and, for `PodSecurityPolicy`, the
[migration page](https://kubernetes.io/docs/tasks/configure-pod-container/migrate-from-psp/).

A finding's remediation is the generated hint, then `; `, then the note, or
the note alone when there is no hint. The note's citations follow the
deprecation guide in the finding's. The text appears wherever the finding
does: the table's `fix:` line, the Markdown cell, `--output json` and SARIF.
A Helm stored-manifest finding carries no note: its fix is to upgrade the
release to a chart version that renders supported APIs, not to edit the
manifest. Editing a note changes the lifecycle digest of the KB version.

The file is separate from `apilifecycle.json` so a regeneration or a weekly
refresh cannot drop a note, and the KB refuses to load when it cannot be
trusted: a note whose kind is not in the lifecycle data, two notes for one
kind, an empty note, or a note without an `https` citation. A lint in the
tests also requires a note on every kind that is removed by the KB's horizon
and has no replacement and no served successor, which is how
`PodSecurityPolicy` is caught. Adding a removal of that shape to the data
fails `go test ./internal/kb` until its note is written.

To add a note, add an entry to `migrations.json` with the exact group,
version and kind from `apilifecycle.json`, say only what the cited page
says, and cite it with an `https` URL. Run `go test ./internal/kb`.

Remediation never moves a manifest to a less mature API than the one it
uses (alpha before beta before GA, from the version name). When the GA
replacement is not served at the target yet, the finding says the version in
use is the right one for that target, and names the GA version with the
minor that first serves it.
A manifest written in a version the target does not serve yet follows the
same rule: it is offered another version only if that one is at least as
mature, and otherwise the finding says to upgrade the cluster to the minor
that first serves it.

## The horizon

The newest minor the API dataset can describe is the newest one `k8s.io/api`
has released, `maxKnownK8s` in the dataset, shown as `kb horizon` by
`upgradescope version`. A target above it is allowed, but nothing can say
what that release removes, so the report gets a `kb-stale` warning and a
required `kb-coverage` gap, and the verdict is `unknown`
([Verdict and score](verdict-and-score.md)). The gap says that API removals
after the horizon are projected from `k8s.io/api`'s lifecycle markers, not
from shipped releases. This is also what happens to
the in-cluster agent's default target (the next minor) once a cluster runs
the horizon minor, until you upgrade to a release with a newer KB.

## How fresh it is

- A weekly workflow (`kb-refresh`) bumps `k8s.io/api`, regenerates the
  dataset, syncs the registry with endoflife.date and opens a pull request
  for review. Nothing changes the datasets without review, and a failing
  refresh opens an issue. The regeneration, which runs freshly bumped, unreviewed modules, runs in a separate run per pipeline on a staging branch, not on `main`, so it cannot plant an Actions cache entry that `main` or a release run restores, and its result is checked before the job that opens the PR uses it. It has failed for weeks at a time before ([#23](https://github.com/abd-ulbasit/upgradescope/issues/23)), so
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
  every group the generator's scheme registers (`k8s.io/api` plus the
  apiextensions and apiregistration schemes, as `builtinGroups`), whether or
  not it has entries, and a manifest of any of them the KB cannot place is an
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
- Field-level removals inside an API that is still served, other than
  in-tree volume plugins ([Volume plugins](volume-plugins.md)): the seccomp
  alpha annotations, `Service.spec.externalIPs`, `beta.kubernetes.io/os`.
  Only the `apiVersion` and `kind` of an object, and the volume plugins its
  pods, PersistentVolumes and StorageClasses name, are judged. The reports say so ([Verdict and score](verdict-and-score.md#what-a-verdict-does-not-cover)).
