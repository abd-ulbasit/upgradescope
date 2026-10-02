package cli

import (
	"encoding/json"
	"reflect"
	"testing"
)

// #124 KB-01, end to end with the embedded KB: a DRA v1alpha3 DeviceClass,
// deleted upstream in 1.34, read as ready/100 at --target 1.34 because the
// KB did not know the type. It now blocks at the removal release and
// warns one release before; a built-in API the KB does not know at all
// (batch/v2alpha1, deleted in 1.21, is known now, so an invented version)
// is an info finding, and a CRD stays silent.
func TestScanFilesDeletedAlphaAPI(t *testing.T) {
	dir := writeFiles(t, map[string]string{"dra.yaml": `apiVersion: resource.k8s.io/v1alpha3
kind: DeviceClass
metadata:
  name: gpu.example.com
---
apiVersion: batch/v9alpha1
kind: CronJob
metadata:
  name: nightly
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: web
`})
	type finding struct{ Severity, Key string }
	cases := []struct {
		target, verdict string
		exit            int
		want            []finding
	}{
		{"1.34", "blocked", 2, []finding{
			{"blocker", "removed-api/resource.k8s.io/v1alpha3/DeviceClass"},
			{"info", "unknown-api/batch/v9alpha1/CronJob"},
		}},
		{"1.33", "ready", 0, []finding{
			{"warning", "removed-api/resource.k8s.io/v1alpha3/DeviceClass"},
			{"info", "unknown-api/batch/v9alpha1/CronJob"},
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
