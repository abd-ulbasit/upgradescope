package engine

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

var update = flag.Bool("update", false, "rewrite golden expected.json files")

// target and now per case; fixed so goldens are stable forever.
var goldenParams = map[string]struct{ target, now string }{
	"clean-cluster": {"1.34", "2026-06-10T00:00:00Z"},
	// A target more than three minors ahead (1.29 to 1.36): the upgrade-path
	// title shows the ends of the path, not every step (#191).
	"upgrade-path-long":     {"1.36", "2026-06-10T00:00:00Z"},
	"removed-api-at-target": {"1.22", "2026-06-10T00:00:00Z"},
	"eol-ingress-nginx":     {"1.30", "2026-06-10T00:00:00Z"},
	"mixed-everything":      {"1.38", "2026-06-10T00:00:00Z"},
	// #148: a kube-proxy 4 minors from the kubelet on its node is a warning
	// naming the node; one with no node, or on a node not listed, is not
	// paired.
	"kube-proxy-node-skew": {"1.35", "2026-06-10T00:00:00Z"},
	// Optional capabilities degraded, and no crds capability at all (a
	// collector that predates it): gaps, none required.
	"degraded-capabilities": {"1.34", "2026-06-10T00:00:00Z"},
	// verdict unknown: a required capability (api-usage) was not assessed.
	"required-capability-missing": {"1.34", "2026-06-10T00:00:00Z"},
	// verdict unknown: target beyond testdata/kb.json MaxKnownK8s (1.36).
	"target-above-horizon": {"1.37", "2026-06-10T00:00:00Z"},
	// GKE GitVersions: the 1.30 node pool is within skew today (3 behind
	// 1.33) but not after the control plane reaches 1.34 → blocker.
	"gke-vendor-versions": {"1.34", "2026-06-10T00:00:00Z"},
	"files-mode":          {"1.36", "2026-06-10T00:00:00Z"},
	// #124: a resource.k8s.io/v1alpha3 DeviceClass, deleted upstream in
	// 1.34 (a KB tombstone), at target 1.34 — the removal release — blocks.
	"removed-alpha-tombstone": {"1.34", "2026-06-10T00:00:00Z"},
	// A built-in group at a version the KB does not know (batch/v2alpha1)
	// is an unscored unknown-api info finding; a CRD group stays silent.
	"unknown-builtin-api": {"1.34", "2026-06-10T00:00:00Z"},
	// CronJobs written via batch/v1beta1 plus caller rows for the same API:
	// one blocker carrying both; the PDB caller row has no objects and
	// stays a standalone deprecated-api-in-use blocker.
	"removed-api-with-callers": {"1.25", "2026-06-10T00:00:00Z"},
	// The warning and info tiers: CronJobs written via batch/v1beta1
	// (removed 1.25, one release after the target) are a removed-api
	// warning that carries the matching caller row; PSP callers (removed
	// 1.25) are a deprecated-api-in-use warning; FlowSchema callers
	// (removed 1.29) and a row without a removal release are info; an
	// unparseable kubelet version is a version-skew info.
	"api-warning-tiers": {"1.24", "2026-06-10T00:00:00Z"},
	// Release-line lifecycle: Istio 1.27 (ended) blocks; the containerd 1.7
	// node is EOL but only warns (the node image carries it, and the
	// kubelet keeps 1.x support through 1.37); a containerd 2.0 node is
	// fine; ExternalDNS from Helm is judged by appVersion 0.14.2 (compat
	// row, no lifecycle data → info).
	"addon-lifecycle": {"1.36", "2026-10-02T00:00:00Z"},
	// One Istio per namespace, as the collector reports them: a Helm
	// release (1.31.1) and sidecars (1.31.0) on the 1.31 line, whose end is
	// within 90 days (one warning naming both namespaces and versions), and
	// an older image-only install (1.27.3) the release must not mask: its
	// EOL and compat blockers name only istio-legacy and team legacy.
	"addon-mixed-versions": {"1.34", "2026-12-15T00:00:00Z"},
	// #165: versions older than the oldest tracked line, which has ended,
	// are past end of life: Istio 1.5.2 (oldest line 1.27) blocks, a
	// containerd 1.6 node (oldest line 1.7) warns. Istio between tracked
	// lines (1.29) or newer than all of them (1.33), and ExternalDNS (no
	// lines), stay addon-no-data.
	"addon-predates-lines": {"1.34", "2026-10-02T00:00:00Z"},
	// Helm releases, no registry data needed: shop/web's chart kubeVersion
	// excludes 1.25 (blocker); shop/legacy-web's does not parse (info);
	// batch/jobs renders two batch/v1beta1 CronJobs, one of which the live
	// scan flags, so the release blocker names only the other; ops/
	// status-page's manifest uses a deprecated API (warning); legacy/gone
	// was uninstalled with --keep-history and is ignored.
	"helm-releases": {"1.25", "2026-06-10T00:00:00Z"},
	// #48 CRD versions, independent of the target: Certificates written
	// through deprecated v1alpha2 (warning, quoting the CRD's warning, and
	// still stored there) and through v1alpha3, served: false (blocker);
	// v1alpha3 still in status.storedVersions (warning); an unused
	// deprecated Issuer version and an HTTPRoute version whose objects the
	// partial crds capability did not check (info); a clean Widget CRD
	// (nothing). The v1alpha2 caller row stays its own info finding.
	"crd-versions": {"1.35", "2026-06-10T00:00:00Z"},
	// #70: ingress-nginx deployed by an Argo CD Application or a Flux
	// HelmRelease leaves no Helm release, only a chart reference with no app
	// version. The retired product is still a blocker, and the helm gap
	// (partial, naming the tool) is shown, not required: the verdict is
	// blocked by the finding, not unknown.
	"gitops-argocd": {"1.30", "2026-06-10T00:00:00Z"},
	"gitops-flux":   {"1.30", "2026-06-10T00:00:00Z"},
	// #77 support lifecycle, one EKS 1.34 cluster (standard support ends
	// 2026-12-02, extended 2027-12-02) at five instants: 175 days before
	// the end of standard support (no finding, but the status carries the
	// date and the figure), 48 days before (warning), the first instant of
	// extended support (blocker, costed), after extended support ended
	// (blocker, no cost), and a cluster whose provider is other (nothing).
	"support-before-window":    {"1.35", "2026-06-10T00:00:00Z"},
	"support-warning-window":   {"1.35", "2026-10-15T00:00:00Z"},
	"support-extended":         {"1.35", "2026-12-02T00:00:00Z"},
	"support-ended":            {"1.35", "2028-01-10T00:00:00Z"},
	"support-unknown-provider": {"1.35", "2026-12-02T00:00:00Z"},
	// AKS publishes no price the dataset can cite: the dates and no cost line.
	"support-no-price": {"1.35", "2026-10-15T00:00:00Z"},
}

// canonical re-marshals JSON with sorted keys + fixed indent so byte
// comparison ignores fixture formatting but catches any content drift.
func canonical(t *testing.T, raw []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(out) + "\n"
}

func TestEvaluateGolden(t *testing.T) {
	kbRaw, err := os.ReadFile("testdata/kb.json")
	if err != nil {
		t.Fatal(err)
	}
	var k kb.KB
	if err := json.Unmarshal(kbRaw, &k); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	visited := make(map[string]bool, len(goldenParams))
	for _, e := range entries {
		if !e.IsDir() {
			continue // kb.json
		}
		name := e.Name()
		params, ok := goldenParams[name]
		if !ok {
			t.Fatalf("no goldenParams entry for testdata/%s", name)
		}
		visited[name] = true
		t.Run(name, func(t *testing.T) {
			invRaw, err := os.ReadFile(filepath.Join("testdata", name, "inventory.json"))
			if err != nil {
				t.Fatal(err)
			}
			var inv inventory.Inventory
			if err := json.Unmarshal(invRaw, &inv); err != nil {
				t.Fatal(err)
			}
			target, err := inventory.ParseVersion(params.target)
			if err != nil {
				t.Fatal(err)
			}
			now, err := time.Parse(time.RFC3339, params.now)
			if err != nil {
				t.Fatal(err)
			}

			got := Evaluate(inv, k, target, now)
			// A key identifies one finding: baselines, ignore rules,
			// notification deltas and SARIF rule ids all match on it.
			seen := map[string]bool{}
			for _, f := range got.Findings {
				if f.Key == "" || seen[f.Key] {
					t.Errorf("finding key %q is empty or not unique (%s)", f.Key, f.Title)
				}
				seen[f.Key] = true
			}
			gotRaw, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			gotJSON := canonical(t, gotRaw)

			goldenPath := filepath.Join("testdata", name, "expected.json")
			if *update {
				if err := os.WriteFile(goldenPath, []byte(gotJSON), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			wantRaw, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatal(err)
			}
			if want := canonical(t, wantRaw); gotJSON != want {
				t.Errorf("report mismatch for %s\n got:\n%s\nwant:\n%s", name, gotJSON, want)
			}
		})
	}
	// Reverse check: every declared golden case must have a testdata
	// directory — a deleted/renamed dir must not silently skip its case.
	for name := range goldenParams {
		if !visited[name] {
			t.Errorf("goldenParams entry %q has no testdata/%s directory", name, name)
		}
	}
}
