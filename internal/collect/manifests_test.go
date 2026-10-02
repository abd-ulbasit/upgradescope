package collect

import (
	"reflect"
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
	// A posted stream has no file name; lines are stream lines.
	wantRefs := []inventory.ObjectRef{{Name: "psp-a", Line: 1}, {Name: "psp-b", Line: 6}}
	if !reflect.DeepEqual(psp.Objects, wantRefs) {
		t.Errorf("psp objects = %+v, want %+v", psp.Objects, wantRefs)
	}
}

// The gate skips non-manifest documents the same way --files does.
func TestCollectManifestsSkipsNonManifestDocs(t *testing.T) {
	stream := "- a\n- b\n---\njust a scalar\n---\nreplicaCount: 1\n---\napiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n"
	inv, err := CollectManifests(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("CollectManifests: %v", err)
	}
	if len(inv.APIUsage) != 1 || inv.APIUsage[0].Kind != "Deployment" || inv.APIUsage[0].Objects[0].Line != 8 {
		t.Fatalf("apiUsage = %+v, want only the Deployment at line 8", inv.APIUsage)
	}
}

func TestCollectManifestsMalformed(t *testing.T) {
	if _, err := CollectManifests(strings.NewReader("apiVersion: v1\nkind: [broken\n")); err == nil {
		t.Fatal("malformed YAML must be a hard error (silent skip = false pass in CI)")
	}
}

// List items that are YAML aliases are not expanded (an alias bomb would
// describe billions of objects), so the stream cannot be assessed: an
// error, never a shorter list. A duplicate key is not an error: kubectl
// decodes it last-wins, and so does the gate.
func TestCollectManifestsAliasedItemsAndDuplicates(t *testing.T) {
	aliased := "apiVersion: v1\nkind: List\nitems:\n- &cm {apiVersion: v1, kind: ConfigMap, metadata: {name: a}}\n- *cm\n"
	if _, err := CollectManifests(strings.NewReader(aliased)); err == nil || !strings.Contains(err.Error(), "alias") {
		t.Errorf("aliased items: err = %v, want an error naming the alias", err)
	}
	inv, err := CollectManifests(strings.NewReader("apiVersion: networking.k8s.io/v1\nkind: Ingress\napiVersion: extensions/v1beta1\n"))
	if err != nil || len(inv.APIUsage) != 1 || inv.APIUsage[0].Group != "extensions" {
		t.Errorf("duplicate apiVersion: usage %+v, err %v; want the last value, extensions/v1beta1", inv.APIUsage, err)
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
