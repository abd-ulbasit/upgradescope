package collect

import (
	"context"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// coordination.k8s.io/v1alpha1 LeaseCandidate is a KB tombstone (deleted
// upstream, not served from 1.32; #124 KB-01), so a 1.31 cluster serving
// it is now listed. The #108 exclusion still holds for it: the candidates
// kube-controller-manager and kube-scheduler write for coordinated leader
// election are rewritten by the control plane after the upgrade, and only
// one someone else wrote is a removed-api blocker.
func TestCollectAPIUsageTombstoneSkipsControlPlaneObjects(t *testing.T) {
	const alpha = "coordination.k8s.io/v1alpha1"
	meta := metaClient(servedAt("LeaseCandidate", []string{alpha},
		obj{namespace: "kube-system", name: "kube-controller-manager-cp1", managed: []metav1.ManagedFieldsEntry{wrote("kube-controller-manager", alpha)}},
		obj{namespace: "kube-system", name: "kube-scheduler-cp1", managed: []metav1.ManagedFieldsEntry{wrote("kube-scheduler", alpha)}},
		obj{namespace: "apps", name: "my-operator", managed: []metav1.ManagedFieldsEntry{wrote("my-operator", alpha)}},
	))
	disc := fakeDiscovery(
		resources("coordination.k8s.io/v1", metav1.APIResource{Name: "leases", Kind: "Lease", Namespaced: true, Verbs: metav1.Verbs{"list"}}),
		resources(alpha, metav1.APIResource{Name: "leasecandidates", Kind: "LeaseCandidate", Namespaced: true, Verbs: metav1.Verbs{"list"}}),
	)
	k := loadKB(t)

	var inv inventory.Inventory
	if _, err := collectAPIUsage(context.Background(), disc, meta, k.APILifecycle, &inv); err != nil {
		t.Fatal(err)
	}
	want := []inventory.APIUsage{{
		Group: "coordination.k8s.io", Version: "v1alpha1", Kind: "LeaseCandidate", Count: 1,
		Namespaces: map[string]int{"apps": 1},
		Objects:    []inventory.ObjectRef{{Namespace: "apps", Name: "my-operator"}},
	}}
	if !reflect.DeepEqual(inv.APIUsage, want) {
		t.Errorf("api usage = %#v\nwant       %#v", inv.APIUsage, want)
	}
	rep := evaluateAt(inv, k, "v1.31.4", inventory.Version{Major: 1, Minor: 32})
	if fs := removedAPIFindings(rep); len(fs) != 1 || fs[0].Severity != engine.SevBlocker || !reflect.DeepEqual(fs[0].Namespaces, []string{"apps"}) {
		t.Errorf("1.31 → 1.32: removed-api findings %+v, want one blocker for the apps candidate", fs)
	}
}
