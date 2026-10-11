package engine

import (
	"slices"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/registry"
)

// The compatibility rows of the cluster-operations and storage add-ons (AWS
// EFS CSI driver, Longhorn), and the entries that publish none (kube-state-metrics,
// Cluster Autoscaler, AWS EBS CSI driver), judged through evalAddOns with the
// real registry on 2026-10-12. Both new ranges are minimums read from the
// project's own pages, which a forward-upgrade scan cannot trip, so each
// affected case targets a Kubernetes below the minimum; the clean cases pin
// the first target that is fine and a target far above, where no maximum may
// fire. The titles are what the CLI prints.
func TestEmbeddedCompatRangesStorageAndOperations(t *testing.T) {
	addons, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	k := kb.KB{AddOns: addons}
	now := day("2026-10-12")
	at := func(minor int) inventory.Version { return inventory.Version{Major: 1, Minor: minor} }

	const (
		efs      = "https://github.com/kubernetes-sigs/aws-efs-csi-driver/blob/f84ed49bf55e3b11f135fb5f78a136bd207cce10/docs/compatibility.md"
		lhNotes  = "https://longhorn.io/docs/1.13.0/important-notes/"
		lhNotes8 = "https://longhorn.io/docs/1.11.0/important-notes/"
		lhBest7  = "https://longhorn.io/docs/1.7.0/best-practices/"
	)
	cases := []struct {
		id, version string
		target      inventory.Version
		want        string // chart-incompat title, "" for none
		cite        string // a citation the finding must carry
	}{
		// AWS EFS CSI driver: the "Kubernetes Version Compability Matrix".
		{"aws-efs-csi-driver", "2.1.0", at(16), "AWS EFS CSI Driver 2.1.0 requires Kubernetes 1.17 or newer (target 1.16)", efs},
		{"aws-efs-csi-driver", "2.1.0", at(17), "", ""},
		{"aws-efs-csi-driver", "1.7.0", at(16), "AWS EFS CSI Driver 1.7.0 requires Kubernetes 1.17 or newer (target 1.16)", efs},
		{"aws-efs-csi-driver", "1.7.0", at(37), "", ""},
		{"aws-efs-csi-driver", "1.1.0", at(13), "AWS EFS CSI Driver 1.1.0 requires Kubernetes 1.14 or newer (target 1.13)", efs},
		{"aws-efs-csi-driver", "1.1.0", at(14), "", ""},
		{"aws-efs-csi-driver", "0.1.0", at(14), "AWS EFS CSI Driver 0.1.0 supports Kubernetes up to 1.13 (target 1.14)", efs},
		{"aws-efs-csi-driver", "0.1.0", at(13), "", ""},
		{"aws-efs-csi-driver", "0.1.0", at(10), "AWS EFS CSI Driver 0.1.0 requires Kubernetes 1.11 or newer (target 1.10)", efs},
		// The matrix has no 3.x row: nothing is said about it.
		{"aws-efs-csi-driver", "3.5.1", at(10), "", ""},

		// Longhorn: minimums from the Important Notes and Best Practices.
		{"longhorn", "1.13.0", at(33), "Longhorn 1.13.0 requires Kubernetes 1.34 or newer (target 1.33)", lhNotes},
		{"longhorn", "1.13.0", at(34), "", ""},
		{"longhorn", "1.13.0", at(37), "", ""},
		{"longhorn", "1.12.1", at(24), "Longhorn 1.12.1 requires Kubernetes 1.25 or newer (target 1.24)", lhNotes8},
		{"longhorn", "1.12.1", at(25), "", ""},
		{"longhorn", "1.8.2", at(24), "Longhorn 1.8.2 requires Kubernetes 1.25 or newer (target 1.24)", lhNotes8},
		{"longhorn", "1.7.2", at(20), "Longhorn 1.7.2 requires Kubernetes 1.21 or newer (target 1.20)", lhBest7},
		{"longhorn", "1.7.2", at(21), "", ""},
		// Before 1.5 no minimum was read.
		{"longhorn", "1.4.0", at(10), "", ""},

		// Entries whose upstream publishes no bound: a tested-with or relative
		// policy, never a blocker (see the comments of their registry files).
		{"cluster-autoscaler", "1.30.0", at(37), "", ""},
		{"aws-ebs-csi-driver", "1.30.0", at(37), "", ""},
		{"kube-state-metrics", "2.16.0", at(37), "", ""},
	}
	for _, tc := range cases {
		var got []Finding
		for _, f := range evalAddOns(addOnAt(tc.id, tc.version), k, tc.target, now) {
			if f.Category == CatChartIncompat {
				got = append(got, f)
			}
		}
		if tc.want == "" {
			if len(got) != 0 {
				t.Errorf("%s %s at %s: unexpected compatibility finding %q", tc.id, tc.version, tc.target, got[0].Title)
			}
			continue
		}
		if len(got) != 1 {
			t.Errorf("%s %s at %s: %d compatibility findings, want 1", tc.id, tc.version, tc.target, len(got))
			continue
		}
		f := got[0]
		if f.Severity != SevBlocker || f.Title != tc.want {
			t.Errorf("%s %s at %s: %s %q, want blocker %q", tc.id, tc.version, tc.target, f.Severity, f.Title, tc.want)
		}
		if !slices.Contains(f.Citations, tc.cite) {
			t.Errorf("%s %s at %s: citations %q lack %s", tc.id, tc.version, tc.target, f.Citations, tc.cite)
		}
		if !strings.Contains(f.Detail, "compatibility range") {
			t.Errorf("%s %s at %s: detail %q does not name the compatibility range", tc.id, tc.version, tc.target, f.Detail)
		}
	}
}

// Longhorn's release lines are judged from the synced cycles: 1.8 ended
// 2026-07-22, 1.9 ends 2026-11-27 (within 90 days of 2026-10-12), 1.13 has
// years left.
func TestLonghornCyclesJudgeInstalledLine(t *testing.T) {
	addons, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	k := kb.KB{AddOns: addons}
	now := day("2026-10-12")
	target := inventory.Version{Major: 1, Minor: 37}
	sev := func(version string) []Severity {
		var out []Severity
		for _, f := range evalAddOns(addOnAt("longhorn", version), k, target, now) {
			if f.Category == CatEOLAddon || f.Category == CatEOLApproaching {
				out = append(out, f.Severity)
			}
		}
		return out
	}
	if got := sev("1.8.2"); !slices.Equal(got, []Severity{SevBlocker}) {
		t.Errorf("longhorn 1.8.2: end-of-life findings %v, want one blocker", got)
	}
	if got := sev("1.9.2"); !slices.Equal(got, []Severity{SevWarning}) {
		t.Errorf("longhorn 1.9.2: end-of-life findings %v, want one warning", got)
	}
	if got := sev("1.13.0"); len(got) != 0 {
		t.Errorf("longhorn 1.13.0: end-of-life findings %v, want none", got)
	}
}
