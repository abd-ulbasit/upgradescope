package server

import (
	"context"
	"encoding/csv"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
	"github.com/abd-ulbasit/upgradescope/internal/suppress"
)

const pspAcceptReason = "replaced by Pod Security Admission before the 1.35 upgrade"

// pspInventory is testInventory with one PodSecurityPolicy object, legacy,
// carrying the object ignore annotation for the PSP removal (with its
// reason) when annotated: with testKB, a blocker for 1.35 unless accepted.
func pspInventory(annotated bool) inventory.Inventory {
	inv := testInventory()
	obj := inventory.ObjectRef{Name: "legacy"}
	if annotated {
		obj.Ignore, obj.IgnoreReason = pspKey, pspAcceptReason
	}
	inv.APIUsage = []inventory.APIUsage{{
		Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy", Count: 1,
		Namespaces: map[string]int{"": 1}, Objects: []inventory.ObjectRef{obj},
	}}
	return inv
}

func push(t *testing.T, ts *httptest.Server, inv inventory.Inventory) {
	t.Helper()
	if resp, out := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, inv), false); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("push = %d %v", resp.StatusCode, out)
	}
}

// storedReport is what GET /report says of the PSP finding.
type storedReport struct {
	Verdict    string                 `json:"verdict"`
	Ready      bool                   `json:"ready"`
	Score      int                    `json:"score"`
	Source     string                 `json:"source"`
	Findings   []struct{ Key string } `json:"findings"`
	Suppressed []struct {
		Key    string `json:"key"`
		Reason string `json:"reason"`
		Source string `json:"source"`
	} `json:"suppressed"`
}

func getReport(t *testing.T, ts *httptest.Server, target string) storedReport {
	t.Helper()
	var got storedReport
	if resp := getJSON(t, ts, "/api/v1/clusters/1/report?target="+target, "", &got); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET report?target=%s = %d", target, resp.StatusCode)
	}
	return got
}

// acceptedPSP reports whether rep is ready with the PSP finding in
// suppressed only, accepted by its annotation and with its reason.
func acceptedPSP(rep storedReport) bool {
	return rep.Verdict == "ready" && rep.Ready && rep.Score == 100 && len(rep.Findings) == 0 && len(rep.Suppressed) == 1 &&
		rep.Suppressed[0].Key == pspKey && rep.Suppressed[0].Source == suppress.AnnotationSource && rep.Suppressed[0].Reason == pspAcceptReason
}

// #381: the server's stored evaluations apply the objects' ignore
// annotations, as scan, the agent and /gate?cluster= do. The stored
// verdict, the fleet cell and a read-time what-if agree with them, and
// the accepted finding is listed in suppressed, from the annotation.
func TestStoredEvaluationAppliesObjectAnnotations(t *testing.T) {
	rec := &recordingNotifier{}
	s := newTestServer(t, newFakeStore(), func(c *Config) { c.Notifier = rec })
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	push(t, ts, pspInventory(true))

	if got := getReport(t, ts, "1.35"); got.Source != sourceStored || !acceptedPSP(got) {
		t.Errorf("stored 1.35 report = %+v, want stored, ready, score 100 and the PSP suppressed by its annotation", got)
	}
	// 1.37 is not a stored target: computed at read time, as ingest would
	// (blocked by the skipped minors, which no annotation names).
	got := getReport(t, ts, "1.37")
	whatIfAccepted := got.Source == sourceWhatIf && len(got.Suppressed) == 1 && got.Suppressed[0].Key == pspKey &&
		got.Suppressed[0].Source == suppress.AnnotationSource
	for _, f := range got.Findings {
		whatIfAccepted = whatIfAccepted && f.Key != pspKey
	}
	if !whatIfAccepted {
		t.Errorf("what-if 1.37 report = %+v, want what-if and the PSP suppressed by its annotation only", got)
	}
	var fleet fleetResponse
	if resp := getJSON(t, ts, "/api/v1/fleet", "", &fleet); resp.StatusCode != http.StatusOK || len(fleet.Clusters) != 1 {
		t.Fatalf("GET fleet = %d %+v", resp.StatusCode, fleet)
	}
	if c := fleet.Clusters[0].Cells["1.35"]; c == nil || !c.Ready || c.Verdict != "ready" || c.Blockers != 0 || c.Score != 100 {
		t.Errorf("fleet cell 1.35 = %+v, want ready, score 100, no blockers", c)
	}

	// The annotation removed: the PSP is a blocker again, and news.
	push(t, ts, pspInventory(false))
	if got := getReport(t, ts, "1.35"); got.Verdict != "blocked" || len(got.Suppressed) != 0 || len(got.Findings) != 1 || got.Findings[0].Key != pspKey {
		t.Errorf("1.35 report without the annotation = %+v, want blocked by the PSP, nothing suppressed", got)
	}
	s.deliverOutbox(context.Background())
	if evs := rec.all(); len(evs) != 1 || evs[0].Kind != notify.KindNewBlocker || evs[0].Target != "1.35" {
		t.Errorf("events = %+v, want one new-blocker for 1.35", evs)
	}
}

// #381: notifications follow the annotations. Annotating a blocked
// cluster's last blocker sends became-ready; a new object that arrives
// already annotated sends nothing; removing the annotation is a
// new-blocker. On SQLite and, with UPGRADESCOPE_PG_TEST_DSN, Postgres
// (forEachStore), so the stores' baselines are the ones judged.
func TestStoredAnnotationLifecycleNotifications(t *testing.T) {
	setup := func(t *testing.T, st store.Store) (*Server, *httptest.Server, *recordingNotifier) {
		rec := &recordingNotifier{}
		s := newTestServer(t, newFakeStore(), func(c *Config) { c.Store, c.Notifier = st, rec })
		ts := httptest.NewServer(s.Handler())
		t.Cleanup(ts.Close)
		return s, ts, rec
	}
	t.Run("annotating the last blocker sends became-ready", func(t *testing.T) {
		forEachStore(t, func(t *testing.T, st store.Store) {
			s, ts, rec := setup(t, st)
			push(t, ts, pspInventory(false))
			s.deliverOutbox(context.Background())
			push(t, ts, pspInventory(true))
			s.deliverOutbox(context.Background())
			if evs := rec.all(); len(evs) != 1 || evs[0].Kind != notify.KindBecameReady || evs[0].Target != "1.35" {
				t.Errorf("events = %+v, want one became-ready for 1.35", evs)
			}
		})
	})
	t.Run("a new already-annotated object sends nothing", func(t *testing.T) {
		forEachStore(t, func(t *testing.T, st store.Store) {
			s, ts, rec := setup(t, st)
			push(t, ts, testInventory())
			push(t, ts, pspInventory(true))
			s.deliverOutbox(context.Background())
			if evs := rec.all(); len(evs) != 0 {
				t.Errorf("events = %+v, want none: the new object is accepted", evs)
			}
		})
	})
	t.Run("removing the annotation is a new-blocker", func(t *testing.T) {
		forEachStore(t, func(t *testing.T, st store.Store) {
			s, ts, rec := setup(t, st)
			push(t, ts, pspInventory(true))
			push(t, ts, pspInventory(false))
			s.deliverOutbox(context.Background())
			if evs := rec.all(); len(evs) != 1 || evs[0].Kind != notify.KindNewBlocker || evs[0].Target != "1.35" {
				t.Errorf("events = %+v, want one new-blocker for 1.35", evs)
			}
		})
	})
}

// #380 with #381: on a cluster whose default target is past the knowledge
// base's horizon (unknown when clean, a notification baseline), the stored
// verdict is judged after the annotations. An object that arrives already
// annotated leaves the pass a clean unknown baseline and sends nothing;
// the annotation removed is the first blocker after unknown passes,
// announced; annotated again, the unknown pass saw the blocker resolved
// (no became-ready: the target is not decided), so its return is a
// new-blocker again.
func TestAnnotatedBlockerPastTheHorizonNotifies(t *testing.T) {
	forEachStore(t, func(t *testing.T, st store.Store) {
		rec := &recordingNotifier{}
		s := newTestServer(t, newFakeStore(), func(c *Config) {
			c.Store, c.Notifier = st, rec
			c.KB.MaxKnownK8s = inventory.Version{Major: 1, Minor: 34} // the 1.34 cluster's default target 1.35 is past it
		})
		ts := httptest.NewServer(s.Handler())
		defer ts.Close()
		ctx := context.Background()
		step := func(name string, inv inventory.Inventory, want ...string) {
			t.Helper()
			before := len(rec.all())
			push(t, ts, inv)
			s.deliverOutbox(ctx)
			evs := rec.all()[before:]
			var got []string
			for _, e := range evs {
				if e.Target != "1.35" {
					t.Errorf("%s: event for target %s, want 1.35 only: %+v", name, e.Target, e)
				}
				got = append(got, e.Kind)
			}
			if !slices.Equal(got, want) {
				t.Errorf("%s: events = %+v, want %v", name, evs, want)
			}
		}
		step("clean, past the horizon", testInventory())
		cur, err := st.CurrentEvaluation(ctx, 1, "1.35")
		if err != nil || cur.Ready || cur.Blockers != 0 || !cur.Baseline {
			t.Fatalf("clean 1.35 = (ready %v, blockers %d, baseline %v, %v); want unknown, and a baseline", cur.Ready, cur.Blockers, cur.Baseline, err)
		}
		step("a new already-annotated PSP", pspInventory(true))
		if cur, err := st.CurrentEvaluation(ctx, 1, "1.35"); err != nil || cur.Blockers != 0 || !cur.Baseline {
			t.Fatalf("annotated 1.35 = (blockers %d, baseline %v, %v); want no blocker, a baseline", cur.Blockers, cur.Baseline, err)
		}
		step("the annotation removed", pspInventory(false), notify.KindNewBlocker)
		step("annotated again", pspInventory(true))
		step("the annotation removed again", pspInventory(false), notify.KindNewBlocker)
	})
}

// #381: an auditor export never silently loses an accepted finding. The
// CSV lists it as a suppressed row with its severity, source and reason;
// the HTML report in a suppressed section.
func TestExportListsSuppressedFindings(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t, newFakeStore()).Handler())
	defer ts.Close()
	push(t, ts, pspInventory(true))

	resp, raw := getExport(t, ts, "?target=1.35&format=csv")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("csv export = %d %s", resp.StatusCode, raw)
	}
	rows, err := csv.NewReader(strings.NewReader(string(raw))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range rows[1:] {
		if r[6] == csvSuppressed {
			found = r[8] == pspKey && strings.Contains(r[10], "blocker") && strings.Contains(r[10], suppress.AnnotationSource) &&
				strings.Contains(r[10], pspAcceptReason)
		}
		if r[6] == "blocker" {
			t.Errorf("csv lists the accepted PSP as a blocker: %v", r)
		}
	}
	if !found {
		t.Errorf("csv export has no suppressed row for %s with its severity, source and reason:\n%s", pspKey, raw)
	}
	if summary := rows[1]; summary[4] != "ready" || !strings.Contains(summary[10], "1 suppressed") {
		t.Errorf("csv summary = %v, want ready and the suppressed count", summary)
	}

	resp, raw = getExport(t, ts, "?target=1.35&format=html")
	html := string(raw)
	if resp.StatusCode != http.StatusOK || !strings.Contains(html, "<h2>suppressed (1)</h2>") ||
		!strings.Contains(html, pspAcceptReason) || !strings.Contains(html, suppress.AnnotationSource) {
		t.Errorf("html export = %d, want a suppressed section with the PSP's source and reason:\n%s", resp.StatusCode, html)
	}
}

// A stored evaluation's teams include those of its suppressed findings, so
// a team-scoped token still reads a cluster whose only findings of the
// team's are accepted.
func TestStoredTeamsIncludeSuppressed(t *testing.T) {
	inv := testInventory()
	inv.Namespaces = []inventory.NamespaceInfo{{Name: "pay-prod", Team: "payments"}}
	rep := engine.Report{
		Findings:   []engine.Finding{{Key: "a", Teams: []string{"payments"}}},
		Suppressed: []engine.SuppressedFinding{{Finding: engine.Finding{Key: "b", Teams: []string{"web", "payments"}}}},
	}
	if got := storedTeams(inv, rep); !slices.Equal(got, []string{"payments", "web"}) {
		t.Errorf("storedTeams = %v, want [payments web]", got)
	}
}
