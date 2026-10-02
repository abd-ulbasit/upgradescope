package cli

import (
	"encoding/json"
	"reflect"
	"testing"
)

// #166 KB-01, end to end with the embedded KB. A kind no cluster serves
// (PodStatusResult, a kubelet wrapper; policy/v1beta1 Eviction, a
// subresource body; apidiscovery v2beta1 APIGroupDiscovery, a discovery
// payload; batch/v2alpha1 JobTemplate, which was never stored) was a
// removed-api blocker at its removal (PodStatusResult 1.37,
// APIGroupDiscovery v2beta1 1.35, Eviction v1beta1 1.25, JobTemplate
// v2alpha1 1.21); it is an unknown-api info now. rbac.authorization.k8s.io/v1alpha1, gone in
// 1.23, scanned as ready at 1.23 with an unknown-api info; it blocks now and
// warns one release before.
func TestScanFilesNonPersistedAndUntaggedAPIs(t *testing.T) {
	dir := writeFiles(t, map[string]string{"m.yaml": `apiVersion: v1
kind: PodStatusResult
metadata:
  name: x
---
apiVersion: batch/v2alpha1
kind: JobTemplate
metadata:
  name: t
---
apiVersion: policy/v1beta1
kind: Eviction
metadata:
  name: e
---
apiVersion: apidiscovery.k8s.io/v2beta1
kind: APIGroupDiscovery
metadata:
  name: d
---
apiVersion: rbac.authorization.k8s.io/v1alpha1
kind: ClusterRole
metadata:
  name: reader
`})
	type finding struct{ Severity, Key string }
	cases := []struct {
		target, verdict string
		exit            int
		want            []finding
	}{
		{"1.23", "blocked", 2, []finding{
			{"blocker", "removed-api/rbac.authorization.k8s.io/v1alpha1/ClusterRole"},
			{"info", "unknown-api/apidiscovery.k8s.io/v2beta1/APIGroupDiscovery"},
			{"info", "unknown-api/batch/v2alpha1/JobTemplate"},
			{"info", "unknown-api/policy/v1beta1/Eviction"},
			{"info", "unknown-api/core/v1/PodStatusResult"},
		}},
		{"1.22", "ready", 0, []finding{
			{"warning", "removed-api/rbac.authorization.k8s.io/v1alpha1/ClusterRole"},
			{"info", "unknown-api/apidiscovery.k8s.io/v2beta1/APIGroupDiscovery"},
			{"info", "unknown-api/batch/v2alpha1/JobTemplate"},
			{"info", "unknown-api/policy/v1beta1/Eviction"},
			{"info", "unknown-api/core/v1/PodStatusResult"},
		}},
		{"1.37", "blocked", 2, []finding{
			{"blocker", "removed-api/rbac.authorization.k8s.io/v1alpha1/ClusterRole"},
			{"info", "unknown-api/apidiscovery.k8s.io/v2beta1/APIGroupDiscovery"},
			{"info", "unknown-api/batch/v2alpha1/JobTemplate"},
			{"info", "unknown-api/policy/v1beta1/Eviction"},
			{"info", "unknown-api/core/v1/PodStatusResult"},
		}},
	}
	for _, c := range cases {
		out, _, err := execScanFiles(t, "--files", dir, "--target", c.target, "--output", "json")
		if ExitCode(err) != c.exit {
			t.Fatalf("--target %s: ExitCode = %d (err %v), want %d", c.target, ExitCode(err), err, c.exit)
		}
		var rep struct {
			Verdict  string
			Findings []finding
		}
		if err := json.Unmarshal([]byte(out), &rep); err != nil {
			t.Fatal(err)
		}
		if rep.Verdict != c.verdict || !reflect.DeepEqual(rep.Findings, c.want) {
			t.Errorf("--target %s: verdict %s, findings %+v; want %s, %+v", c.target, rep.Verdict, rep.Findings, c.verdict, c.want)
		}
	}
}
