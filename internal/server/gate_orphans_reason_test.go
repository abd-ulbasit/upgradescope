package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// orphanNames are n custom resource APIs in the sorted "group/version
// Kind" form mergeManifests hands orphansReason: A1, A2, ... .
func orphanNames(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("a.example.com/v1 A%d", i+1)
	}
	return out
}

// orphansReason is the text of the gate's partial crds gap for custom
// resources with no CRD in the cluster or the manifests, and it is
// pinned to the word: the singular form for one API, then the count, the
// first five names and, past five, how many more were left out. A wrong
// number here would put a wrong count of unassessed APIs into a CI gate's
// answer. #359.
func TestOrphansReason(t *testing.T) {
	const tail = ", so whether their versions are served was not assessed"
	cases := []struct {
		n    int
		want string
	}{
		{1, "no CRD in the cluster or the posted manifests for a.example.com/v1 A1, so whether its version is served was not assessed"},
		{2, "no CRD in the cluster or the posted manifests for 2 custom resource APIs (a.example.com/v1 A1, a.example.com/v1 A2)" + tail},
		{5, "no CRD in the cluster or the posted manifests for 5 custom resource APIs (a.example.com/v1 A1, a.example.com/v1 A2, a.example.com/v1 A3, a.example.com/v1 A4, a.example.com/v1 A5)" + tail},
		{6, "no CRD in the cluster or the posted manifests for 6 custom resource APIs (a.example.com/v1 A1, a.example.com/v1 A2, a.example.com/v1 A3, a.example.com/v1 A4, a.example.com/v1 A5 and 1 more)" + tail},
		{12, "no CRD in the cluster or the posted manifests for 12 custom resource APIs (a.example.com/v1 A1, a.example.com/v1 A2, a.example.com/v1 A3, a.example.com/v1 A4, a.example.com/v1 A5 and 7 more)" + tail},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%d", c.n), func(t *testing.T) {
			orphans := orphanNames(c.n)
			before := slices.Clone(orphans)
			if got := orphansReason(orphans); got != c.want {
				t.Errorf("orphansReason(%d APIs) =\n%q\nwant\n%q", c.n, got, c.want)
			}
			if !slices.Equal(orphans, before) {
				t.Errorf("orphansReason modified its argument: %v", orphans)
			}
		})
	}
}

// Through the gate: seven posted custom resources whose CRDs neither the
// cluster nor the manifests have make the crds capability partial, with
// all seven in skipped, and the reason is orphansReason's: seven APIs, the
// first five by name, "and 2 more". The verdict is not blocked by them.
// #359.
func TestGateOrphanCustomResourcesReasonCountsAndCaps(t *testing.T) {
	ts := widgetCluster(t, nil)
	var manifests strings.Builder
	for i := 1; i <= 7; i++ {
		fmt.Fprintf(&manifests, "---\napiVersion: orphans.example.com/v1\nkind: Orphan%d\nmetadata: {name: o%d, namespace: shop}\n", i, i)
	}
	resp, raw := postGate(t, ts, "?target=1.35&cluster=prod-eu-1&fail-on=never", "", manifests.String(), "application/x-yaml")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.StatusCode, raw)
	}
	var body struct {
		NotAssessed []engine.CapabilityGap `json:"notAssessed"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("gate body is not JSON: %v\n%s", err, raw)
	}
	var skipped []string
	for i := 1; i <= 7; i++ {
		skipped = append(skipped, fmt.Sprintf("orphans.example.com/v1 Orphan%d", i))
	}
	const wantText = "no CRD in the cluster or the posted manifests for 7 custom resource APIs " +
		"(orphans.example.com/v1 Orphan1, orphans.example.com/v1 Orphan2, orphans.example.com/v1 Orphan3, orphans.example.com/v1 Orphan4, orphans.example.com/v1 Orphan5 and 2 more)" +
		", so whether their versions are served was not assessed"

	var crds []engine.CapabilityGap
	for _, g := range body.NotAssessed {
		if g.Capability == inventory.CapCRDs {
			crds = append(crds, g)
		}
	}
	if len(crds) != 1 {
		t.Fatalf("crds gaps = %+v, want exactly one\n%s", crds, raw)
	}
	g := crds[0]
	if !g.Partial || g.Reason != wantText || !slices.Equal(g.Skipped, skipped) {
		t.Errorf("crds gap = %+v\nwant partial, reason %q, skipped %v", g, wantText, skipped)
	}
}
