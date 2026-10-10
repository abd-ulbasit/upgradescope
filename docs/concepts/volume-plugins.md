# In-tree volume plugins

An object can be at an API version the target serves and still break on
upgrade, because a field inside it stops working. The commonest case is a
pod volume of an in-tree plugin that upstream removed when CSI migration
finished: a Deployment with a `glusterfs` volume is valid `apps/v1`, and on
Kubernetes 1.26 or later its pods do not start. upgradescope reports these
as `volume-plugin` findings (#351).

## What is checked

The knowledge base carries a hand-authored dataset,
`internal/kb/data/volumeplugins.json`. Each entry names a plugin by its
field in a pod's `volumes[]` (`corev1.VolumeSource`) and a
PersistentVolume's spec (`corev1.PersistentVolumeSource`), the minor its
end came in, a classification, the replacement, and the upstream release
notes and pull requests that minor comes from. A unit test refuses an
entry without a citation, with a minor that does not parse, or whose name
is not a field of either type.

The classification follows the `k8s.io/api` field comments: "the in-tree
type is no longer supported" is **removed**, and "all operations are
redirected to the CSI driver" is **CSI migration**.

| Plugin | Class | Minor | CSI driver |
|---|---|---|---|
| `scaleIO` | removed | 1.22 | |
| `flocker`, `quobyte`, `storageos` | removed | 1.25 | |
| `glusterfs` | removed | 1.26 | |
| `cephfs`, `rbd` | removed | 1.31 | |
| `gitRepo` | removed (disabled) | 1.33 | |
| `cinder` | CSI migration | 1.26 | `cinder.csi.openstack.org` |
| `awsElasticBlockStore` | CSI migration | 1.27 | `ebs.csi.aws.com` |
| `azureDisk` | CSI migration | 1.27 | `disk.csi.azure.com` |
| `gcePersistentDisk` | CSI migration | 1.28 | `pd.csi.storage.gke.io` |
| `vsphereVolume` | CSI migration | 1.29 | `csi.vsphere.vmware.com` |
| `azureFile` | CSI migration | 1.30 | `file.csi.azure.com` |
| `portworxVolume` | CSI migration | 1.36 | `pxd.portworx.com` |
| `flexVolume` | deprecated (1.23) | | |

Notes:

- `gitRepo` has been deprecated since 1.11. Kubernetes 1.33 disabled it by
  default (the `GitRepoVolumeDriver` feature gate can turn it back on), and
  1.36 locked it off. It counts as removed from 1.33.
- 1.31 removed `rbd`'s CSI migration support with the plugin. So, like
  `cephfs`, which never had CSI migration, the field stops working.
- `vsphereVolume`'s minor is the release that removed the GA
  `CSIMigrationvSphere` feature gate (1.29), which made CSI migration
  unconditional.
- `azureFile`'s in-tree plugin was removed for 1.28 and the removal
  reverted before release (kubernetes/kubernetes#118388). It was removed
  again in 1.30.
- `photonPersistentDisk` is not in the dataset: its removal minor was not
  confirmed from release notes. A field the dataset does not list is not
  judged.

## Severity

| Class | Blocker | Warning | Info |
|---|---|---|---|
| removed | at or after the minor | the minor before it | earlier |
| CSI migration | never | at or after the minor: name the CSI driver that must be installed | earlier |
| deprecated | never | never | always |

A CSI-migrated plugin is never a blocker on its own. The API field keeps
working, and every operation goes to the named CSI driver, so a cluster
with that driver installed is fine. upgradescope does not check whether
the driver is installed. Each finding is keyed `volume-plugin/<plugin>`,
so ignore rules, `upgradescope.basit.engineer/ignore` annotations on the
workload and baselines work on it as on any other finding.

## Where the volumes are read from

- **`scan --files` and the gate.** The pod templates of Deployments,
  DaemonSets, StatefulSets, ReplicaSets, Jobs and CronJobs, a Pod's own
  volumes, and PersistentVolume manifests. Each one is located by file
  and line. A document that cannot be decoded makes the `volumes`
  capability partial. With `?cluster=`, the gate adds the posted
  manifests' plugins to the cluster's.
- **Live (`scan`, the agent).** The pods the collector lists anyway: the
  kube-system pods from the `versions` step and the rest from the
  `addons` step. The capability makes **no request of its own**
  (`TestCollectLiveVolumesNoExtraRequest` counts them). Pods are counted
  per plugin and namespace, and no pod is named. Between full pod passes
  (`--pod-pass-every`), the pods outside kube-system are reported as the
  last full pass read them, as add-ons are (`addOnEvidenceAgeSeconds`).
  If no pod can be listed, `volumes` is not assessed. If a later page
  fails, it is partial.
- **Not read live: PersistentVolumes and StorageClasses.** Reading them
  needs RBAC on `persistentvolumes` and `storageclasses`, which the agent
  does not have. A PersistentVolumeClaim bound to an in-tree
  PersistentVolume is not traced, so a pod that uses
  `awsElasticBlockStore` through a claim is not found live.

## Older agents

`volumes` is an optional capability. A collector that predates it does
not report it: a v0.1.x or v0.2.0 release-candidate agent, or a files
inventory saved by an older CLI. Its report then has a `volumes` gap,
"not reported by the collector", so the cluster reads as not assessed for
volume plugins, never as clean. The gap is optional and changes no
verdict.

A newer agent can push to an older server. The server ignores fields and
capabilities it does not know, so it accepts the push and judges what it
knows. No `collectorSchema` bump was needed, and agents and servers can
be upgraded in either order.

## Not checked

Field-level removals other than in-tree volume plugins are not checked.
These include the seccomp alpha annotations (ignored since 1.27), the
`kubernetes.io/ingress.class` annotation, `Service.spec.externalIPs` and
`beta.kubernetes.io/os`.
