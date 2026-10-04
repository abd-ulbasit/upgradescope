package chart

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The agent's default CPU limit is a cgroup quota, and a tick is CPU-bound:
// at 200m the first tick on a cluster of 1,000 Helm releases stopped at its
// Helm-step deadline with 132 releases unread, at 500m it took 48 s of a
// 59 s deadline, and at 1 CPU 35 s (#229, docs/operations/scale.md). So the
// default is 1 CPU, with the request left at 50m: the limit reserves
// nothing. The numbers are pinned here so that lowering the limit again is a
// decision with the measurements in front of it, not an edit.
func TestAgentDefaultResources(t *testing.T) {
	c := container(t, render(t), "upgradescope-agent")
	for _, tc := range []struct {
		path []string
		want string
	}{
		{[]string{"resources", "limits", "cpu"}, "1"}, {[]string{"resources", "limits", "memory"}, "256Mi"},
		{[]string{"resources", "requests", "cpu"}, "50m"}, {[]string{"resources", "requests", "memory"}, "64Mi"},
	} {
		got, _, _ := unstructured.NestedString(c, tc.path...)
		if got != tc.want {
			t.Errorf("agent %v = %q, want %q", tc.path, got, tc.want)
		}
	}
}
