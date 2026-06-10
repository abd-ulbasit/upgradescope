package collect

import (
	"reflect"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

func TestCollectFiles(t *testing.T) {
	inv, err := CollectFiles("testdata/files")
	if err != nil {
		t.Fatal(err)
	}

	if inv.SchemaVersion != 1 {
		t.Errorf("SchemaVersion = %d, want 1", inv.SchemaVersion)
	}
	if inv.ClusterID != "files" {
		t.Errorf("ClusterID = %q, want \"files\"", inv.ClusterID)
	}
	if inv.CollectedAt.IsZero() {
		t.Error("CollectedAt must be set")
	}

	if got := inv.Capabilities[inventory.CapAPIUsage]; !got.Available {
		t.Errorf("api-usage capability = %+v, want available", got)
	}
	for _, cap := range []inventory.Capability{
		inventory.CapDeprecatedCalls, inventory.CapHelm, inventory.CapAddOns, inventory.CapVersions,
	} {
		if got := inv.Capabilities[cap]; got.Available || got.Reason != "files mode" {
			t.Errorf("capability %s = %+v, want {false, \"files mode\"}", cap, got)
		}
	}

	want := []inventory.APIUsage{
		{Group: "", Version: "v1", Kind: "ConfigMap", Count: 1, Namespaces: map[string]int{"shop": 1}},
		{Group: "apps", Version: "v1", Kind: "Deployment", Count: 1, Namespaces: map[string]int{"shop": 1}},
		{Group: "networking.k8s.io", Version: "v1beta1", Kind: "Ingress", Count: 2, Namespaces: map[string]int{"internal": 1, "shop": 1}},
		{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole", Count: 1, Namespaces: map[string]int{"": 1}},
	}
	if !reflect.DeepEqual(inv.APIUsage, want) {
		t.Errorf("api usage = %#v\nwant     %#v", inv.APIUsage, want)
	}
}

func TestCollectFilesMissingDir(t *testing.T) {
	if _, err := CollectFiles("testdata/does-not-exist"); err == nil {
		t.Fatal("want error for missing directory")
	}
}
