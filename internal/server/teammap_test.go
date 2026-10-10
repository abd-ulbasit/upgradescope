package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/textsafe"
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
		// The name of the findings no team owns: a team of that name would
		// be merged into the bucket on every surface.
		"reserved team": "- pattern: \"a-*\"\n  team: \"(unattributed)\"\n",
	}
	for name, in := range cases {
		if _, err := ParseTeamMap([]byte(in)); err == nil {
			t.Errorf("%s: ParseTeamMap(%q): want error, got nil", name, in)
		}
	}
}

// A team is free text, as it was before read scopes (#72): a map that
// names "Platform Team", a team with a comma or a non-ASCII one still
// loads. A team called "*", the fleet-wide read scope, is read as
// StarTeam, with a warning, and serve keeps starting.
func TestParseTeamMapKeepsFreeTextTeams(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	tm, err := ParseTeamMap([]byte("- pattern: \"plat-*\"\n  team: Platform Team\n" +
		"- pattern: \"fr-*\"\n  team: \"Équipe, Paris\"\n- pattern: \"star-*\"\n  team: \"*\"\n"))
	if err != nil {
		t.Fatalf("ParseTeamMap: %v", err)
	}
	var teams []string
	for _, r := range tm {
		teams = append(teams, r.Team)
	}
	if want := []string{"Platform Team", "Équipe, Paris", StarTeam}; !slices.Equal(teams, want) {
		t.Errorf("teams = %q, want %q", teams, want)
	}
	if !strings.Contains(logged.String(), `"(*)"`) {
		t.Errorf("no warning that team \"*\" is read as %q: %q", StarTeam, logged.String())
	}
}

// A map that names both "*" and "(*)" makes them one team, StarTeam:
// serve says so at load, and keeps starting. A map with only one of them
// gets no such warning.
func TestParseTeamMapWarnsWhenStarTeamIsTaken(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	const collision = "are now one team"
	if _, err := ParseTeamMap([]byte("- pattern: \"star-*\"\n  team: \"*\"\n- pattern: \"paren-*\"\n  team: \"(*)\"\n")); err != nil {
		t.Fatalf("ParseTeamMap: %v", err)
	}
	if !strings.Contains(logged.String(), collision) {
		t.Errorf("no warning that teams \"*\" and %q are one: %q", StarTeam, logged.String())
	}
	for _, only := range []string{"\"*\"", "\"(*)\""} {
		logged.Reset()
		if _, err := ParseTeamMap([]byte("- pattern: \"a-*\"\n  team: " + only + "\n")); err != nil {
			t.Fatalf("ParseTeamMap: %v", err)
		}
		if strings.Contains(logged.String(), collision) {
			t.Errorf("team %s alone warned of a collision: %q", only, logged.String())
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

// A --team-map is an operator's file, but its patterns and teams can come
// from a repository the operator did not write. The startup log and the
// errors quote them with %q, which escapes C0, C1, DEL, U+202E and U+2028;
// this keeps a later %s from putting them on the terminal raw.
func TestTeamMapLogsAndErrorsQuoteHostileText(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	const bad = "ev\x1b(2Jil\r::error::x\u202eabc\u009b\u2028\nnext"
	check := func(what, out string) {
		t.Helper()
		for _, r := range out {
			if r != '\n' && textsafe.Unsafe(r) {
				t.Errorf("%s holds %U raw: %q", what, r, out)
				return
			}
		}
		if strings.Contains(out, "\nnext") {
			t.Errorf("%s: a newline inside a name survived: %q", what, out)
		}
	}

	// %q writes \x1b, \u009b and \u202e, which a YAML double-quoted
	// string reads back as the characters.
	rules := func(pattern, team string) []byte {
		return []byte(fmt.Sprintf("- pattern: %q\n  team: %q\n", pattern, team))
	}
	if _, err := ParseTeamMap(rules(bad+"-*", "*")); err != nil {
		t.Fatalf("ParseTeamMap: %v", err)
	}
	if !strings.Contains(logged.String(), `\x1b(2J`) {
		t.Fatalf("the startup warning does not show the escape: %q", logged.String())
	}
	check("startup warning", logged.String())

	_, err := ParseTeamMap(rules(bad, ""))
	if err == nil {
		t.Fatal("a rule without a team was accepted")
	}
	check("missing-team error", err.Error())

	_, err = ParseTeamMap(rules(bad+"[", "t"))
	if err == nil {
		t.Fatal("an invalid glob was accepted")
	}
	check("invalid-glob error", err.Error())
}
