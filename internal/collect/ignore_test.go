package collect

import (
	"context"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// The upgradescope.dev/ignore annotations travel with the object ref, so
// suppression can accept a finding for that one object.
func TestCollectFilesIgnoreAnnotations(t *testing.T) {
	dir := writeTree(t, map[string]string{"all.yaml": `apiVersion: networking.k8s.io/v1beta1
kind: Ingress
metadata:
  name: legacy
  annotations:
    upgradescope.dev/ignore: removed-api, deprecated-api
    upgradescope.dev/ignore-reason: decommissioned with the old cluster
---
apiVersion: networking.k8s.io/v1beta1
kind: Ingress
metadata:
  name: web
  annotations:
    other: x
    upgradescope.dev/ignore: [not, a, string]
`})
	inv, _, err := CollectFiles(dir, kb.KB{})
	if err != nil {
		t.Fatal(err)
	}
	want := []inventory.ObjectRef{
		{Name: "legacy", File: "all.yaml", Line: 1, Ignore: "removed-api, deprecated-api", IgnoreReason: "decommissioned with the old cluster"},
		{Name: "web", File: "all.yaml", Line: 9},
	}
	if len(inv.APIUsage) != 1 || !reflect.DeepEqual(inv.APIUsage[0].Objects, want) {
		t.Errorf("api usage = %+v\nwant objects %+v", inv.APIUsage, want)
	}
}

// Live objects carry their upgradescope.dev/ignore annotations too.
func TestCollectAPIUsageRecordsIgnoreAnnotations(t *testing.T) {
	const beta3 = "flowcontrol.apiserver.k8s.io/v1beta3"
	served := []string{"flowcontrol.apiserver.k8s.io/v1", beta3}
	meta := metaClient(servedAt("FlowSchema", served,
		obj{name: "accepted",
			annotations: map[string]string{IgnoreAnnotation: "removed-api", IgnoreReasonAnnotation: "owned by vendor"},
			managed:     []metav1.ManagedFieldsEntry{wrote("kubectl-client-side-apply", beta3)}},
	))
	flowschemas := metav1.APIResource{Name: "flowschemas", Kind: "FlowSchema", Verbs: metav1.Verbs{"list"}}
	disc := fakeDiscovery(resources("flowcontrol.apiserver.k8s.io/v1", flowschemas), resources(beta3, flowschemas))
	lifecycle := []kb.APILifecycleEntry{
		{Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta3", Kind: "FlowSchema", Introduced: inventory.Version{Major: 1, Minor: 26}, Deprecated: ver(1, 29), Removed: ver(1, 32),
			Replacement: &kb.GVK{Group: "flowcontrol.apiserver.k8s.io", Version: "v1", Kind: "FlowSchema"}},
	}

	var inv inventory.Inventory
	if _, err := collectAPIUsage(context.Background(), disc, meta, lifecycle, &inv); err != nil {
		t.Fatal(err)
	}
	want := []inventory.ObjectRef{{Name: "accepted", Manager: "kubectl-client-side-apply", Ignore: "removed-api", IgnoreReason: "owned by vendor"}}
	if len(inv.APIUsage) != 1 || !reflect.DeepEqual(inv.APIUsage[0].Objects, want) {
		t.Errorf("api usage = %#v\nwant objects %#v", inv.APIUsage, want)
	}
}
