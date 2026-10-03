package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/registry"
)

// An EKS cluster's pushed provider reaches its stored evaluation: the
// report the read API serves carries the support calendar and the
// support-lifecycle finding, judged with the server's clock (pinned to
// 2026-06-10, 175 days before EKS 1.34 leaves standard support: a status,
// no finding yet). A provider this build does not know (a newer agent's) is accepted and
// has no calendar; one that is not a bounded name is refused.
func TestSupportLifecycleThroughTheServer(t *testing.T) {
	k := testKB()
	k.Providers = []registry.ProviderSupport{{
		SchemaVersion: 1, ID: "eks", DisplayName: "Amazon EKS",
		Citations: []string{"https://docs.aws.amazon.com/eks/latest/userguide/kubernetes-versions.html"},
		Versions:  []registry.SupportWindow{{Minor: "1.34", StandardEnd: "2026-12-02", ExtendedEnd: "2027-12-02"}},
		Pricing: &registry.Pricing{
			Currency: "USD", StandardPerClusterHour: 0.10, ExtendedPerClusterHour: 0.60, AsOf: "2026-10-03",
			Citations: []string{"https://aws.amazon.com/eks/pricing/"},
		},
	}}
	ts := httptest.NewServer(newTestServer(t, newFakeStore(), func(c *Config) { c.KB = k }).Handler())
	defer ts.Close()

	inv := testInventory()
	inv.ServerVersion, inv.Provider = "v1.34.2-eks-3abc123", inventory.ProviderEKS
	if resp, out := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, inv), true); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("push status = %d (body %v), want 202", resp.StatusCode, out)
	}
	var rep engine.Report
	if resp := getJSON(t, ts, "/api/v1/clusters/1/report?target=1.35", "", &rep); resp.StatusCode != http.StatusOK {
		t.Fatalf("report status = %d", resp.StatusCode)
	}
	s := rep.Support
	if s == nil || s.Provider != "eks" || s.Phase != engine.SupportStandard || s.ExtendedSupportFrom != "2026-12-02" || s.AnnualCostDelta != "4380.00" || s.PriceAsOf != "2026-10-03" {
		t.Fatalf("report support = %+v, want the EKS 1.34 calendar with the list-price delta", s)
	}

	// A newer agent that learned a fourth provider is not refused: the
	// snapshot is judged, with no support calendar for the provider.
	future := testInventory()
	future.Provider = "oke"
	if resp, out := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, future), true); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("future provider: status = %d (body %v), want 202", resp.StatusCode, out)
	}

	bad := testInventory()
	bad.Provider = "<script>"
	if resp, out := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, bad), true); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("unbounded provider: status = %d (body %v), want 422", resp.StatusCode, out)
	}
}
