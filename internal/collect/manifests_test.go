package collect

import (
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

func TestCollectManifests(t *testing.T) {
	stream := `apiVersion: policy/v1beta1
kind: PodSecurityPolicy
metadata:
  name: psp-a
--- # separator with comment
apiVersion: policy/v1beta1
kind: PodSecurityPolicy
metadata:
  name: psp-b
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: shop
---
# comment-only document
`
	inv, err := CollectManifests(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("CollectManifests: %v", err)
	}
	if inv.ClusterID != "manifests" {
		t.Errorf("clusterID = %q, want manifests", inv.ClusterID)
	}
	if !inv.Capabilities[inventory.CapAPIUsage].Available {
		t.Error("api-usage capability must be available")
	}
	if st := inv.Capabilities[inventory.CapVersions]; st.Available || st.Reason == "" {
		t.Errorf("versions capability = %+v, want degraded with reason", st)
	}
	if len(inv.APIUsage) != 2 {
		t.Fatalf("apiUsage = %+v, want 2 GVKs", inv.APIUsage)
	}
	// Sorted: apps before policy.
	dep := inv.APIUsage[0]
	if dep.Group != "apps" || dep.Kind != "Deployment" || dep.Count != 1 || dep.Namespaces["shop"] != 1 {
		t.Errorf("deployment usage = %+v", dep)
	}
	psp := inv.APIUsage[1]
	if psp.Group != "policy" || psp.Version != "v1beta1" || psp.Kind != "PodSecurityPolicy" || psp.Count != 2 || psp.Namespaces[""] != 2 {
		t.Errorf("psp usage = %+v", psp)
	}
}

func TestCollectManifestsMalformed(t *testing.T) {
	if _, err := CollectManifests(strings.NewReader("apiVersion: v1\nkind: [broken\n")); err == nil {
		t.Fatal("malformed YAML must be a hard error (silent skip = false pass in CI)")
	}
}

func TestCollectManifestsEmpty(t *testing.T) {
	inv, err := CollectManifests(strings.NewReader(""))
	if err != nil {
		t.Fatalf("CollectManifests(empty): %v", err)
	}
	if len(inv.APIUsage) != 0 {
		t.Fatalf("apiUsage = %+v, want empty", inv.APIUsage)
	}
}
