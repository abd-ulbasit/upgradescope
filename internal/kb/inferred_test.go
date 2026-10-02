package kb

import "testing"

// auditedInferredRemovals are the types whose removal tools/gen-kb infers
// (removedInferred, no upstream tag), each checked to be a resource
// kube-apiserver stored and served while it lasted (a storage entry under
// pkg/registry/<group>/rest in kubernetes/kubernetes): alpha resources
// upstream deleted, or whose removal a changelog states.
var auditedInferredRemovals = []string{
	"auditregistration.k8s.io/v1alpha1 AuditSink",
	"batch/v2alpha1 CronJob",
	"certificates.k8s.io/v1alpha1 PodCertificateRequest",
	"coordination.k8s.io/v1alpha1 LeaseCandidate",
	"discovery.k8s.io/v1alpha1 EndpointSlice",
	"networking.k8s.io/v1alpha1 ClusterCIDR",
	"networking.k8s.io/v1alpha1 IPAddress",
	"networking.k8s.io/v1alpha1 ServiceCIDR",
	"node.k8s.io/v1alpha1 RuntimeClass",
	"rbac.authorization.k8s.io/v1alpha1 ClusterRole",
	"rbac.authorization.k8s.io/v1alpha1 ClusterRoleBinding",
	"rbac.authorization.k8s.io/v1alpha1 Role",
	"rbac.authorization.k8s.io/v1alpha1 RoleBinding",
	"resource.k8s.io/v1alpha1 PodScheduling",
	"resource.k8s.io/v1alpha1 ResourceClaim",
	"resource.k8s.io/v1alpha1 ResourceClaimTemplate",
	"resource.k8s.io/v1alpha1 ResourceClass",
	"resource.k8s.io/v1alpha2 PodSchedulingContext",
	"resource.k8s.io/v1alpha2 ResourceClaim",
	"resource.k8s.io/v1alpha2 ResourceClaimParameters",
	"resource.k8s.io/v1alpha2 ResourceClaimTemplate",
	"resource.k8s.io/v1alpha2 ResourceClass",
	"resource.k8s.io/v1alpha2 ResourceClassParameters",
	"resource.k8s.io/v1alpha2 ResourceSlice",
	"resource.k8s.io/v1alpha3 DeviceClass",
	"resource.k8s.io/v1alpha3 PodSchedulingContext",
	"resource.k8s.io/v1alpha3 ResourceClaim",
	"resource.k8s.io/v1alpha3 ResourceClaimTemplate",
	"resource.k8s.io/v1alpha3 ResourceSlice",
	"scheduling.k8s.io/v1alpha1 PriorityClass",
	"scheduling.k8s.io/v1alpha1 Workload",
	"scheduling.k8s.io/v1alpha2 PodGroup",
	"scheduling.k8s.io/v1alpha2 Workload",
	"settings.k8s.io/v1alpha1 PodPreset",
	"storagemigration.k8s.io/v1alpha1 StorageVersionMigration",
}

// TestInferredRemovalsAreAudited: an inferred removal is only as good as
// the claim that kube-apiserver served the type as a resource.
// batch/v2alpha1 JobTemplate was inferred removed in 1.21 and was a false
// blocker, since it was never served (#166). A new inferred removal fails
// here until someone has checked the type has storage in kube-apiserver
// and adds it to auditedInferredRemovals, or adds it to tools/gen-kb's
// nonPersisted instead.
func TestInferredRemovalsAreAudited(t *testing.T) {
	audited := map[string]bool{}
	for _, s := range auditedInferredRemovals {
		audited[s] = true
	}
	f, err := parseLifecycle(apilifecycleJSON)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range f.Entries {
		if !e.RemovedInferred {
			continue
		}
		gv := e.Version
		if e.Group != "" {
			gv = e.Group + "/" + e.Version
		}
		k := gv + " " + e.Kind
		seen[k] = true
		if !audited[k] {
			t.Errorf("%s has an inferred removal but is not audited: check kube-apiserver served it as a resource, then add it to auditedInferredRemovals (or to gen-kb nonPersisted)", k)
		}
	}
	for k := range audited {
		if !seen[k] {
			t.Errorf("auditedInferredRemovals has %s, which has no inferred removal now; delete it", k)
		}
	}
}
