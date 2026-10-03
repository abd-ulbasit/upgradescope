package collect

import (
	"context"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// Issue #199 (API-01): an object of a flagged kind that nothing can be
// attributed to (no managedFields entry, no last-applied annotation: a
// DeviceClass created through resource.k8s.io/v1beta1 with spec {} has
// neither, the apiserver prunes the empty field set) cannot be told apart
// from one created through v1. It is not a blocker; it is reported as an
// info finding that its authorship is unknown, beside the attributed ones.
func TestCollectAPIUsageFieldlessObjectsAreAuthorshipUnknown(t *testing.T) {
	const beta, ga = "resource.k8s.io/v1beta1", "resource.k8s.io/v1"
	deviceClasses := metav1.APIResource{Name: "deviceclasses", Kind: "DeviceClass", Verbs: metav1.Verbs{"list"}}
	disc := fakeDiscovery(resources(ga, deviceClasses), resources(beta, deviceClasses))
	k := loadKB(t)

	collect := func(objs ...obj) inventory.Inventory {
		var inv inventory.Inventory
		if _, err := collectAPIUsage(context.Background(), disc, metaClient(servedAt("DeviceClass", []string{ga, beta}, objs...)), k.APILifecycle, &inv); err != nil {
			t.Fatal(err)
		}
		return inv
	}
	names := func(us []inventory.APIUsage) (out []string) {
		for _, u := range us {
			for _, o := range u.Objects {
				out = append(out, u.Version+"/"+o.Name+"@"+o.Manager)
			}
		}
		return out
	}
	empty := obj{name: "b-empty"}
	ssa := obj{name: "c-ssa", managed: []metav1.ManagedFieldsEntry{appliedAt("kubectl", beta, 1)}}
	create := obj{name: "e-real", managed: []metav1.ManagedFieldsEntry{wrote("kubectl-create", beta)}}

	inv := collect(empty, ssa, create)
	if want := []string{"v1beta1/c-ssa@kubectl", "v1beta1/e-real@kubectl-create"}; !reflect.DeepEqual(names(inv.APIUsage), want) {
		t.Errorf("attributed = %v, want %v (the field-less object is not authored by anyone we know)", names(inv.APIUsage), want)
	}
	if want := []string{"v1beta1/b-empty@"}; !reflect.DeepEqual(names(inv.APIAuthorshipUnknown), want) {
		t.Errorf("authorship unknown = %v, want %v", names(inv.APIAuthorshipUnknown), want)
	}
	if u := inv.APIAuthorshipUnknown; len(u) == 1 && (u[0].Count != 1 || u[0].Namespaces[""] != 1 || u[0].Kind != "DeviceClass") {
		t.Errorf("authorship unknown entry = %+v, want one DeviceClass counted once, cluster-scoped", u[0])
	}

	// Anything that attributes the object, or that is not the user's, is
	// not authorship unknown.
	for name, o := range map[string]obj{
		"last-applied at another version":   {name: "x", annotations: lastApplied(ga, "DeviceClass")},
		"last-applied at the flagged one":   {name: "x", annotations: lastApplied(beta, "DeviceClass")},
		"written through the replacement":   {name: "x", managed: []metav1.ManagedFieldsEntry{wrote("kubectl", ga)}},
		"written by the control plane only": {name: "x", managed: []metav1.ManagedFieldsEntry{wrote("kube-apiserver", beta)}},
	} {
		if got := collect(o).APIAuthorshipUnknown; len(got) != 0 {
			t.Errorf("%s: authorship unknown = %+v, want none", name, got)
		}
	}

	t.Run("only a field-less object never blocks and never lowers the score", func(t *testing.T) {
		with, without := collect(empty), collect()
		if len(with.APIUsage) != 0 || len(with.APIAuthorshipUnknown) != 1 {
			t.Fatalf("usage %+v, authorship unknown %+v; want only the unknown entry", with.APIUsage, with.APIAuthorshipUnknown)
		}
		for _, target := range []inventory.Version{{Major: 1, Minor: 37}, {Major: 1, Minor: 38}} {
			a, b := evaluateAt(with, k, "v1.34.0", target), evaluateAt(without, k, "v1.34.0", target)
			if a.Verdict != b.Verdict || a.Score != b.Score {
				t.Errorf("target %s: verdict/score %s/%d with the field-less object, %s/%d without; want the same", target, a.Verdict, a.Score, b.Verdict, b.Score)
			}
			var unknown []engine.Finding
			for _, f := range a.Findings {
				if strings.Contains(f.Title, "authorship unknown") {
					unknown = append(unknown, f)
				}
			}
			if len(unknown) != 1 || unknown[0].Severity != engine.SevInfo || len(unknown[0].Objects) != 1 || unknown[0].Objects[0].Name != "b-empty" {
				t.Errorf("target %s: authorship-unknown findings = %+v, want one info finding naming b-empty", target, unknown)
			}
		}
	})
}
