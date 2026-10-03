# Deprecated-API detection

Finding removed and deprecated API usage in a live cluster is harder than it
looks, and getting it wrong in either direction is costly: a false blocker
stops an upgrade for nothing, a missed one breaks a deployment pipeline the
day after. upgradescope uses two signals, and is explicit about what each
can and cannot see.

## Objects: who writes them, not what is served

The apiserver stores an object once and serves it at **every** version its
resource supports, converting on read. Listing a deprecated endpoint
(`GET /apis/extensions/v1beta1/ingresses` on a cluster old enough to serve
it) returns every Ingress, including ones created through `networking.k8s.io/v1`
and ones the control plane maintains itself. So "the cluster serves this
deprecated version and has objects" says nothing about whether anyone
*uses* that version, and treating it as usage produces false blockers on
every cluster. It would also make the scanner a deprecated-API caller.

upgradescope therefore asks **who writes** each object through the flagged
version:

1. For each resource the cluster still serves at a version the knowledge
   base flags, it lists the objects once, metadata only, at a version that
   is not flagged when the cluster serves one (or at the replacement's group
   when that group serves the same objects). When every version the
   cluster serves is flagged, listing is itself a deprecated request, so
   the resource is listed only when the knowledge base schedules a removal
   for it (`policy/v1beta1` PodSecurityPolicy on 1.24). A deprecated kind
   that is never removed (core `v1` Endpoints and ComponentStatus on 1.33
   and later) is not listed: it could only ever give an info finding.
2. For a kind that **continues** under another version, an object counts
   only when a writer still writes it through the flagged version, judged
   from `metadata.managedFields`:
    - a field manager's server-side **Apply** entry for the flagged version
      counts (each apply replaces it, so it is that manager's current
      configuration);
    - an **Update** entry for the flagged version counts only when it is
      newer than every entry the same manager has for another version
      (managers that moved to the new version keep a stale old entry, and
      it is ignored);
    - with no managedFields entry to judge by, the apiVersion in the
      `kubectl.kubernetes.io/last-applied-configuration` annotation
      decides.

    The finding names the managers (`Written by: helm, kubectl-client-side-apply`).
3. For a kind that **goes away** (no surviving version in the knowledge
   base, and none served), every stored object counts, whoever wrote it:
   it will not exist after the upgrade (PodSecurityPolicy). The exception
   is an object whose every managedFields entry names a control-plane
   manager: the control plane replaces its own objects across an upgrade.
   This matters for kinds upstream has shipped only as alpha or beta
   (LeaseCandidate), where "no surviving version" may just mean the
   knowledge base does not know the GA version yet.

Entries for the `status` subresource are ignored, and so are three
control-plane managers whose entries only record what was current when that
release wrote the object (`kube-apiserver`, `kube-controller-manager`,
`api-priority-and-fairness-config-producer-v1`). API Priority and Fairness
objects the apiserver maintains (`apf.kubernetes.io/autoupdate-spec: "true"`)
are skipped.

A freshly created cluster, scanned against its next minor, has no
removed-API blocker; the kind end-to-end tests check that on every pull
request ([claims ledger](../claims.md), API-01b).

## Requests: the apiserver's own record

The second signal is the apiserver metric `apiserver_requested_deprecated_apis`,
read from `/metrics`. It covers **callers that store nothing**: a CronJob
that lists `batch/v1beta1` jobs, a dashboard that polls a beta API. A
requested API with no object finding becomes a `deprecated-api-in-use`
finding; one that matches an object finding is added to it as evidence.

What this metric is, and is not:

- It says **that** some client requested a deprecated API, not **which**
  client: it has no user or user-agent label. The apiserver's audit log
  records the `k8s.io/deprecated` annotation per request, with the user;
  upgradescope does not read audit logs.
- It counts since **that apiserver** started, and resets on restart.
- One `/metrics` request reaches **one** apiserver replica. On an HA control
  plane, requests served by the other replicas are not seen.
- Managed control planes often deny `/metrics` regardless of RBAC. The
  capability is then reported as not assessed, with the reason; it is not
  required for the verdict ([Managed clusters](../guides/managed-clusters.md)).
- A `/metrics` that never answers is given up after `--request-timeout`
  (default 30s), or at the latest when its step's share of the scan's time
  runs out. Only `deprecated-calls` is then not assessed; every other check
  has already run. Because `deprecated-calls` is not required, such a scan
  can still be `ready`: the verdict then rests on stored objects alone, and
  the report's `NOT ASSESSED` section says the request signal is missing.

## Manifests

In files mode (`scan --files`, the CI gate) every object in the manifests
uses the API version it is written in, so every flagged object counts and is
reported with its file and line. An object of a built-in API group at a
version or kind the knowledge base does not know (a typo such as
`apps/v1beta9`) is an `unknown-api` info finding, so it is not passed in
silence; groups that belong to CRDs produce nothing. Their versions are
judged against their CRDs instead (next section).

## CRD versions

Custom resources have no entry in the knowledge base: their versions are
whatever the add-on that ships the CRD declares. A Kubernetes upgrade
usually needs add-on upgrades, and those break on CRD versions, so the
`crds` capability reads each CustomResourceDefinition's `spec.versions`
(`served`, `storage`, `deprecated`, `deprecationWarning`) and
`status.storedVersions`, and reports `crd-version` findings:

| Key | Severity | When |
|---|---|---|
| `crd-version/unserved/<group>/<version>/<kind>` | blocker | Custom resources use a version the CRD does not serve (`served: false`, or, in files mode, not listed at all). The apiserver rejects them. |
| `crd-version/deprecated/<group>/<version>/<kind>` | warning | Custom resources are written through a version the CRD marks `deprecated: true`. The finding quotes the CRD's `deprecationWarning`. Info when nothing was found using it. |
| `crd-version/stored-unserved/<group>/<version>/<kind>` | warning | `status.storedVersions` lists a version the CRD no longer serves (live only). Objects may still be stored there, and the CRD update that drops the version is rejected until it is gone from `status.storedVersions`. |

These findings are about the add-on's CRD, not the Kubernetes target, and
say so; their severity does not depend on the target. A blocker is still a
blocker, so it makes the verdict `blocked`.

**Live.** Custom resources are judged like built-in objects: a CRD with a
deprecated or unserved version has its custom resources listed once,
metadata only, at a served version that is not deprecated (listing the
deprecated one would make the scanner a caller), and an object counts when
a field manager still writes it through that version, by the same
managedFields rules as above. The finding names the managers. A manager
that has stopped writing (or was removed) leaves its entry behind, so its
objects stay listed until another manager takes over its fields or the
entry is removed; the remediation says so. The remedy
for a stale stored version is a storage version migration: rewrite every
object at the storage version (the
[kube-storage-version-migrator](https://github.com/kubernetes-sigs/kube-storage-version-migrator),
the add-on's own tool such as cert-manager's `cmctl upgrade
migrate-api-version`, or a no-op update of each object), then remove the
version from `status.storedVersions` (`kubectl patch crd <name>
--subresource=status`); see the
[Kubernetes docs](https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definition-versioning/#upgrade-existing-objects-to-a-new-stored-version).

**Files mode.** `apiextensions.k8s.io/v1` CustomResourceDefinition
manifests in the scanned files give the versions, and every custom resource
in the files at a deprecated, unserved or unlisted version counts, with its
file and line. A cert-manager 1.6 CRD (v1alpha2 no longer served) next to a
`cert-manager.io/v1alpha2` Certificate is a blocker. A manifest's `status`
is ignored. A custom resource whose CRD is not in the files cannot be
judged: `crds` is then partial, naming it, so it reads as not assessed, not
as fine. Render charts with `helm template --include-crds` to include
their CRDs. Tool configuration that is never applied to a cluster needs
no CRD: a `Kustomization`, a `Kptfile`, and any group without a dot (such
as `skaffold/v4beta6`), which no CRD can define.

`crds` is never required: CRD versions do not decide whether Kubernetes can
be upgraded. An inventory from an agent that predates the capability (or
a files inventory an older CLI saved) does not report it, and the report
lists `crds` as not assessed.

## Known limits

- **No managedFields to judge by, no usable annotation, no attribution.**
  An object written through a deprecated version is not counted as use of
  it when no managedFields entry is left to judge by (none outside the
  status subresource and the control plane's managers) and the
  last-applied annotation is absent, unparseable, or names no apiVersion.
  That covers objects created before field tracking existed, objects whose
  managedFields a client cleared, and objects created with no fields at all
  (the apiserver drops the entry of a manager that owns no fields, so a
  `DeviceClass` created through `resource.k8s.io/v1beta1` with `spec: {}`
  has none), including such an object whose status a driver or controller
  wrote afterwards: a status entry says nothing about who created it. Such
  an object is stored the same however it was created, so it cannot be a
  blocker. It is reported as an info finding, `deprecated-api` titled
  "authorship unknown" with the key suffix `/authorship-unknown`, naming the
  objects, so a client that authors through the deprecated version is still
  visible. When a kind has several flagged versions served at once, it is
  one finding for the kind (under one of the versions), not one per
  version. It changes neither the verdict nor the score.
- **Control-plane-only objects are not "unknown".** An object whose every
  entry is the control plane's (including its status) is the control
  plane's own, which it replaces across an upgrade. It is neither counted
  as use nor reported as authorship unknown.
- **The control plane's managers are trusted.** A write through a
  deprecated version by `kube-apiserver`, `kube-controller-manager` or the
  APF producer is not reported, and neither is an object of a kind that
  goes away when only control-plane managers ever wrote it.
- **Old entries can keep a finding open.** When an object moves from one
  tool to another (`kubectl apply` to Helm), the old tool's entry keeps the
  finding until it is gone. A Helm upgrade that changes nothing but the
  apiVersion can leave Helm's old entry in place: Helm computes a patch,
  finds no change and skips the write. The finding then stays until the
  object is next changed through the new version.
- **Kinds the knowledge base knows only as alpha or beta.** For a kind with
  no GA version in the knowledge base yet (LeaseCandidate, PodGroup,
  Workload, EvictionRequest), "no surviving version" can mean "the GA
  version is not released yet", so objects a user (not the control plane)
  wrote through it count as if the kind went away.
- **The scanner's own deprecated LISTs hide other callers.** When step 1
  has to list at a deprecated endpoint (a kind being removed that the
  cluster serves only at flagged versions), the metric counts the
  scanner's own request. The engine ignores the metric's rows for those
  resources and reports `deprecated-calls` as partial, naming them, the
  same way on every scan, so the scanner never reports itself as a caller.
  The cost: other clients of those resources are not seen through the
  metric. The apiserver audit log (annotation `k8s.io/deprecated`) shows
  them.
- **Object references are capped** at 100 per API; the finding says how
  many more there are, and the managers it names come from the listed ones.
- **The agent cannot read custom resources.** The chart grants reading
  CRDs but no custom resource (it grants no wildcards), so for a CRD with a
  deprecated or unserved version the agent reports `crds` as partial,
  naming the versions it could not check; a deprecated version is then
  info, not a warning. `scan` with credentials that can list them checks
  them.
- **CRD versions dropped from `spec.versions`** are not looked for in a
  live cluster: nothing in the CRD names them. Files mode reports custom
  resources at a version the CRD does not list.
- **A CRD whose every served version is deprecated** is not listed (that
  would be a deprecated request), and `crds` is partial, naming it.
- **Partial access.** A resource the credentials cannot list makes the
  capability partial; the verdict becomes `unknown` only if the skipped
  resource has an API removed at or before the target.

The detailed rules, and the tests that pin each one, are in the
[architecture guide](../architecture.md#api-usage-authorship-not-residency)
and the [claims ledger](../claims.md#api-usage-removed-and-deprecated-apis).
