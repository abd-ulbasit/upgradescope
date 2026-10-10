package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
)

// #351, the issue's repro: a Deployment with gitRepo, awsElasticBlockStore
// and glusterfs volumes scanned READY 100/100 with no findings. Now each
// plugin is one cited finding, classified as upstream says.
const volumeRepro = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  template:
    spec:
      containers: [{name: c, image: busybox}]
      volumes:
      - {name: g, gitRepo: {repository: "https://x"}}
      - {name: e, awsElasticBlockStore: {volumeID: vol-1}}
      - {name: r, glusterfs: {endpoints: x, path: p}}
`

func TestScanFilesVolumePluginRepro(t *testing.T) {
	dir := writeFiles(t, map[string]string{"f.yaml": volumeRepro})
	for _, tc := range []struct {
		target string
		want   map[string]engine.Severity
		ready  bool
	}{
		// The issue's target: glusterfs (1.26) and gitRepo (disabled 1.33)
		// block, awsElasticBlockStore needs ebs.csi.aws.com.
		{"1.36", map[string]engine.Severity{
			"volume-plugin/glusterfs": engine.SevBlocker, "volume-plugin/gitRepo": engine.SevBlocker,
			"volume-plugin/awsElasticBlockStore": engine.SevWarning}, false},
		// One minor before glusterfs's removal: a warning, and the others info.
		{"1.25", map[string]engine.Severity{
			"volume-plugin/glusterfs": engine.SevWarning, "volume-plugin/gitRepo": engine.SevInfo,
			"volume-plugin/awsElasticBlockStore": engine.SevInfo}, true},
	} {
		out, _, err := execScanFiles(t, "--files", dir, "--target", tc.target, "--output", "json", "--fail-on", "never")
		if err != nil {
			t.Fatalf("%s: %v\n%s", tc.target, err, out)
		}
		var r engine.Report
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatalf("%s: %v\n%s", tc.target, err, out)
		}
		got := map[string]engine.Severity{}
		for _, f := range r.Findings {
			if f.Category != engine.CatVolumePlugin {
				continue
			}
			got[f.Key] = f.Severity
			if len(f.Citations) == 0 || len(f.Objects) != 1 || f.Objects[0].File != "f.yaml" || f.Objects[0].Line != 1 {
				t.Errorf("%s %s: citations %v, objects %+v; want cited and located at f.yaml:1", tc.target, f.Key, f.Citations, f.Objects)
			}
		}
		if len(got) != len(tc.want) {
			t.Errorf("%s: volume findings %v, want %v", tc.target, got, tc.want)
		}
		for k, sev := range tc.want {
			if got[k] != sev {
				t.Errorf("%s: %s = %q, want %q", tc.target, k, got[k], sev)
			}
		}
		if r.Ready != tc.ready {
			t.Errorf("%s: ready = %v, want %v", tc.target, r.Ready, tc.ready)
		}
	}

	// The same Deployment without those volumes is unchanged: 100.
	clean := strings.Split(volumeRepro, "      volumes:\n")[0]
	out, _, err := execScanFiles(t, "--files", writeFiles(t, map[string]string{"f.yaml": clean}), "--output", "json")
	if err != nil {
		t.Fatalf("clean: %v\n%s", err, out)
	}
	var r engine.Report
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatal(err)
	}
	if r.Score != 100 || !r.Ready || len(r.Findings) != 0 {
		t.Errorf("clean manifest: score %d ready %v findings %+v, want 100, ready, none", r.Score, r.Ready, r.Findings)
	}
}

// The ignore annotation takes a volume-plugin finding as any other: by
// key, on the workload that names the plugin.
func TestScanFilesVolumePluginIgnoreAnnotation(t *testing.T) {
	annotated := strings.Replace(volumeRepro, "  name: web\n",
		"  name: web\n  annotations:\n    upgradescope.basit.engineer/ignore: volume-plugin/glusterfs\n    upgradescope.basit.engineer/ignore-reason: migrating to ceph-csi\n", 1)
	out, _, err := execScanFiles(t, "--files", writeFiles(t, map[string]string{"f.yaml": annotated}), "--output", "json", "--fail-on", "never")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var r engine.Report
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatal(err)
	}
	for _, f := range r.Findings {
		if f.Key == "volume-plugin/glusterfs" {
			t.Errorf("glusterfs finding not suppressed: %+v", f)
		}
	}
	if len(r.Suppressed) != 1 || r.Suppressed[0].Key != "volume-plugin/glusterfs" || r.Suppressed[0].Reason != "migrating to ceph-csi" {
		t.Errorf("suppressed = %+v, want the glusterfs finding with its reason", r.Suppressed)
	}
}
