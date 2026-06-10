package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

func TestParseTeamMap(t *testing.T) {
	tm, err := ParseTeamMap([]byte("- pattern: \"payments-*\"\n  team: payments\n- pattern: \"*\"\n  team: platform\n"))
	if err != nil {
		t.Fatalf("ParseTeamMap: %v", err)
	}
	if len(tm) != 2 || tm[0].Pattern != "payments-*" || tm[0].Team != "payments" {
		t.Fatalf("parsed rules = %+v", tm)
	}
}

func TestParseTeamMapRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"bad yaml":      "{not yaml",
		"empty pattern": "- pattern: \"\"\n  team: x\n",
		"empty team":    "- pattern: \"a-*\"\n  team: \"\"\n",
		"bad glob":      "- pattern: \"[\"\n  team: x\n",
	}
	for name, in := range cases {
		if _, err := ParseTeamMap([]byte(in)); err == nil {
			t.Errorf("%s: ParseTeamMap(%q): want error, got nil", name, in)
		}
	}
}

func TestLoadTeamMap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "teams.yaml")
	if err := os.WriteFile(path, []byte("- pattern: \"db-*\"\n  team: data\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tm, err := LoadTeamMap(path)
	if err != nil {
		t.Fatalf("LoadTeamMap: %v", err)
	}
	if len(tm) != 1 || tm[0].Team != "data" {
		t.Fatalf("loaded rules = %+v", tm)
	}
	if _, err := LoadTeamMap(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("LoadTeamMap(missing): want error, got nil")
	}
}

func TestTeamMapApply(t *testing.T) {
	ns := []inventory.NamespaceInfo{
		{Name: "payments-prod", Team: "labelled"},
		{Name: "checkout", Team: "shop"},
		{Name: "misc"},
	}
	cases := []struct {
		name string
		tm   TeamMap
		want []inventory.NamespaceInfo
	}{
		{
			name: "label-only: nil map keeps labels",
			tm:   nil,
			want: []inventory.NamespaceInfo{
				{Name: "payments-prod", Team: "labelled"},
				{Name: "checkout", Team: "shop"},
				{Name: "misc"},
			},
		},
		{
			name: "override wins over label",
			tm:   TeamMap{{Pattern: "payments-*", Team: "payments"}},
			want: []inventory.NamespaceInfo{
				{Name: "payments-prod", Team: "payments"},
				{Name: "checkout", Team: "shop"},
				{Name: "misc"},
			},
		},
		{
			name: "first match wins",
			tm: TeamMap{
				{Pattern: "payments-*", Team: "payments"},
				{Pattern: "*", Team: "platform"},
			},
			want: []inventory.NamespaceInfo{
				{Name: "payments-prod", Team: "payments"},
				{Name: "checkout", Team: "platform"},
				{Name: "misc", Team: "platform"},
			},
		},
		{
			name: "no match keeps label",
			tm:   TeamMap{{Pattern: "db-*", Team: "data"}},
			want: []inventory.NamespaceInfo{
				{Name: "payments-prod", Team: "labelled"},
				{Name: "checkout", Team: "shop"},
				{Name: "misc"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := append([]inventory.NamespaceInfo(nil), ns...)
			got := tc.tm.Apply(in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Apply = %+v, want %+v", got, tc.want)
			}
			if !reflect.DeepEqual(in, ns) {
				t.Fatalf("Apply mutated its input: %+v", in)
			}
		})
	}
}

// TestIngestAppliesTeamMap proves the override rewrites namespace→team
// attribution before Evaluate: the stored report's finding carries the
// mapped team, not the inventory label.
func TestIngestAppliesTeamMap(t *testing.T) {
	st := newFakeStore()
	s := newTestServer(t, st, func(c *Config) {
		c.TeamMap = TeamMap{{Pattern: "payments-*", Team: "payments"}}
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	inv := testInventory()
	inv.Namespaces = []inventory.NamespaceInfo{{Name: "payments-prod", Team: "labelled"}}
	inv.APIUsage = []inventory.APIUsage{{
		Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy",
		Count: 1, Namespaces: map[string]int{"payments-prod": 1},
	}}
	resp, out := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, inv), false)
	if resp.StatusCode != 202 {
		t.Fatalf("push status = %d (body %v), want 202", resp.StatusCode, out)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.evals) == 0 {
		t.Fatal("no evaluations stored")
	}
	var rep engine.Report
	if err := json.Unmarshal(st.evals[0].Report, &rep); err != nil {
		t.Fatalf("unmarshal stored report: %v", err)
	}
	if len(rep.Findings) == 0 {
		t.Fatal("stored report has no findings")
	}
	if got := rep.Findings[0].Teams; !reflect.DeepEqual(got, []string{"payments"}) {
		t.Fatalf("finding teams = %v, want [payments] (override must win over label %q)", got, "labelled")
	}
}
