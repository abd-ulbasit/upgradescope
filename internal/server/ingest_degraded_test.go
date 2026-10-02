package server

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// degraded is inv as an agent whose versions collector failed pushes it:
// no serverVersion, and the versions capability unavailable.
func degraded(inv inventory.Inventory) inventory.Inventory {
	inv.ServerVersion = ""
	inv.Capabilities = map[inventory.Capability]inventory.CapabilityStatus{
		inventory.CapVersions: {Available: false, Reason: "get /version: forbidden"},
	}
	return inv
}

type clusterReportView struct {
	Target        string `json:"target"`
	Verdict       string `json:"verdict"`
	Score         int    `json:"score"`
	SnapshotID    int64  `json:"snapshotId"`
	ServerVersion string `json:"serverVersion"`
	NotAssessed   []struct {
		Capability string `json:"capability"`
		Required   bool   `json:"required"`
	} `json:"notAssessed"`
}

// TestIngestDegradedPushKeepsFleetCell: a push without a server version
// (the versions collector failed) is judged at the version the cluster
// last reported. It used to become a latest snapshot no target could be
// derived from: the blocked cluster's fleet cell went blank, its column
// left the default fleet and /report answered 422.
func TestIngestDegradedPushKeepsFleetCell(t *testing.T) {
	h := newHarness(t, Config{KB: testKB()}, aug1)
	if code, out := h.push("prod", testInventoryWithPSP()); code != http.StatusAccepted {
		t.Fatalf("seed push = %d %v", code, out)
	}
	if c := h.fleetCell("prod", "1.35"); c == nil || c.Verdict != "blocked" || c.Score != 75 {
		t.Fatalf("seed cell = %+v, want blocked/75", c)
	}

	h.clock.set(aug1.Add(time.Hour))
	code, out := h.push("prod", degraded(testInventoryWithPSP()))
	if code != http.StatusAccepted {
		t.Fatalf("degraded push = %d %v, want 202", code, out)
	}
	snapID := int64(out["snapshotId"].(float64))

	var f fleetView
	if code := h.get("/api/v1/fleet", &f); code != http.StatusOK {
		t.Fatalf("GET /fleet = %d", code)
	}
	if !slices.Contains(f.Targets, "1.35") || len(f.Clusters) != 1 {
		t.Fatalf("fleet = %+v, want the 1.35 column kept", f)
	}
	row := f.Clusters[0]
	cell := row.Cells["1.35"]
	if cell == nil || cell.SnapshotID != snapID || cell.Verdict != "blocked" || cell.Ready {
		t.Errorf("degraded cell = %+v, want blocked (the PSP is still there) for snapshot %d", cell, snapID)
	}
	if row.ServerVersion != "v1.34.2" {
		t.Errorf("row serverVersion = %q, want the last known v1.34.2", row.ServerVersion)
	}

	id := h.clusterID("prod")
	var rep clusterReportView
	if code := h.get("/api/v1/clusters/"+itoa(id)+"/report", &rep); code != http.StatusOK {
		t.Fatalf("GET /report = %d, want 200", code)
	}
	if rep.Target != "1.35" || rep.SnapshotID != snapID || !hasRequiredGap(rep, "versions") {
		t.Errorf("report = %+v, want target 1.35 of snapshot %d with a required versions gap", rep, snapID)
	}
	var list []struct {
		Latest *struct {
			Target     string `json:"target"`
			SnapshotID int64  `json:"snapshotId"`
		} `json:"latest"`
	}
	if code := h.get("/api/v1/clusters", &list); code != http.StatusOK || len(list) != 1 || list[0].Latest == nil || list[0].Latest.SnapshotID != snapID {
		t.Errorf("GET /clusters = %d %+v, want the degraded snapshot's 1.35 summary", code, list)
	}

	// A clean cluster's degraded push is unknown — never ready — and a
	// second degraded push in a row keeps the inherited version.
	if code, _ := h.push("clean", testInventory()); code != http.StatusAccepted {
		t.Fatal("clean seed push failed")
	}
	if c := h.fleetCell("clean", "1.35"); c == nil || c.Verdict != "ready" {
		t.Fatalf("clean seed cell = %+v, want ready", c)
	}
	for i, ns := range []string{"a", "b"} {
		inv := degraded(testInventory())
		inv.Namespaces = []inventory.NamespaceInfo{{Name: ns}}
		if code, out := h.push("clean", inv); code != http.StatusAccepted {
			t.Fatalf("degraded push %d = %d %v", i, code, out)
		}
		if c := h.fleetCell("clean", "1.35"); c == nil || c.Verdict != "unknown" || c.Ready {
			t.Errorf("after degraded push %d: clean cell = %+v, want unknown, not ready", i, c)
		}
	}
}

func hasRequiredGap(rep clusterReportView, capability string) bool {
	for _, g := range rep.NotAssessed {
		if g.Capability == capability && g.Required {
			return true
		}
	}
	return false
}
