package storetest

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// Conformance for read scopes: team-scoped read tokens, and the teams each
// evaluation stores, which decide the clusters a scoped token reads.

// testReadTokens pins the read-token lifecycle: minted with a team scope,
// validated to that scope, listed with metadata only, revoked by id.
func testReadTokens(t *testing.T, s store.Store) {
	ctx := context.Background()
	if toks, err := s.ListReadTokens(ctx); err != nil || len(toks) != 0 {
		t.Fatalf("ListReadTokens on empty store = (%v, %v), want none", toks, err)
	}
	long := "0123abcd" + strings.Repeat("e", 56)
	teamsID, err := s.CreateReadToken(ctx, []string{"payments", "checkout"}, long)
	if err != nil {
		t.Fatalf("CreateReadToken(payments, checkout): %v", err)
	}
	fleetID, err := s.CreateReadToken(ctx, []string{"*"}, "fleet-read")
	if err != nil {
		t.Fatalf("CreateReadToken(*): %v", err)
	}
	if teamsID <= 0 || fleetID <= 0 || teamsID == fleetID {
		t.Fatalf("CreateReadToken ids = %d, %d; want distinct positive ids", teamsID, fleetID)
	}

	teams, ok, err := s.ValidReadToken(ctx, long)
	if err != nil || !ok || !slices.Equal(teams, []string{"checkout", "payments"}) {
		t.Errorf("ValidReadToken(team token) = (%v, %v, %v), want ([checkout payments], true, nil): sorted", teams, ok, err)
	}
	teams, ok, err = s.ValidReadToken(ctx, "fleet-read")
	if err != nil || !ok || !slices.Equal(teams, []string{"*"}) {
		t.Errorf("ValidReadToken(fleet token) = (%v, %v, %v), want ([*], true, nil)", teams, ok, err)
	}
	if teams, ok, err := s.ValidReadToken(ctx, "no-such-token"); err != nil || ok || teams != nil {
		t.Errorf("ValidReadToken(unknown) = (%v, %v, %v), want (nil, false, nil)", teams, ok, err)
	}
	// An ingest token is not a read token, nor the other way round.
	if _, err := s.CreateToken(ctx, "prod", "ingest-only"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.ValidReadToken(ctx, "ingest-only"); err != nil || ok {
		t.Errorf("ValidReadToken(an ingest token) = (ok %v, err %v), want (false, nil)", ok, err)
	}
	if _, ok, err := s.ValidToken(ctx, "fleet-read"); err != nil || ok {
		t.Errorf("ValidToken(a read token) = (ok %v, err %v), want (false, nil)", ok, err)
	}

	if _, err := s.CreateReadToken(ctx, []string{"other"}, "fleet-read"); err == nil {
		t.Error("CreateReadToken with an already-issued token succeeded, want error")
	}
	if _, err := s.CreateReadToken(ctx, nil, "no-teams"); err == nil {
		t.Error("CreateReadToken with no teams succeeded, want error: a token must have a scope")
	}
	if _, err := s.CreateReadToken(ctx, []string{"a"}, ""); err == nil {
		t.Error("CreateReadToken with an empty token succeeded, want error")
	}

	if err := s.RevokeReadToken(ctx, teamsID); err != nil {
		t.Fatalf("RevokeReadToken(%d): %v", teamsID, err)
	}
	if _, ok, err := s.ValidReadToken(ctx, long); err != nil || ok {
		t.Errorf("revoked read token: ValidReadToken = (ok %v, err %v), want (false, nil)", ok, err)
	}
	if _, ok, _ := s.ValidReadToken(ctx, "fleet-read"); !ok {
		t.Error("revoking one read token must leave the others valid")
	}
	for _, id := range []int64{teamsID, fleetID + 1000} {
		if err := s.RevokeReadToken(ctx, id); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("RevokeReadToken(%d) err = %v, want ErrNotFound (already revoked or unknown)", id, err)
		}
	}

	all, err := s.ListReadTokens(ctx)
	if err != nil {
		t.Fatalf("ListReadTokens: %v", err)
	}
	if len(all) != 2 || all[0].ID != teamsID || all[1].ID != fleetID {
		t.Fatalf("ListReadTokens = %+v, want ids %d, %d in creation order", all, teamsID, fleetID)
	}
	if a := all[0]; !slices.Equal(a.Teams, []string{"checkout", "payments"}) || a.Prefix != "0123abcd" || a.CreatedAt.IsZero() || a.RevokedAt == nil {
		t.Errorf("revoked read token row = %+v, want teams [checkout payments], prefix 0123abcd, created, revoked", a)
	}
	if b := all[1]; !slices.Equal(b.Teams, []string{"*"}) || b.Prefix != "" || b.RevokedAt != nil {
		t.Errorf("fleet read token row = %+v, want teams [*], no prefix (short token), active", b)
	}
	// Ingest tokens are listed apart.
	if toks, err := s.ListTokens(ctx, ""); err != nil || len(toks) != 1 {
		t.Errorf("ListTokens = (%v, %v), want only the ingest token", toks, err)
	}
}

// testClustersOfTeams pins which clusters a team scope reads: those whose
// current evaluations (each target's newest of the latest snapshot) name a
// team of the scope, as inserted and as refreshed.
func testClustersOfTeams(t *testing.T, s store.Store) {
	ctx := context.Background()
	pay := mustCluster(t, s, "pay")
	web := mustCluster(t, s, "web")
	moved := mustCluster(t, s, "moved")
	mustCluster(t, s, "empty") // no snapshot: no team reads it

	paySnap := mustSnapshot(t, s, pay, "p1", base)
	mustEval(t, s, store.Evaluation{ClusterID: pay, SnapshotID: paySnap, Target: "1.36", Teams: []string{"payments"}})
	mustEval(t, s, store.Evaluation{ClusterID: pay, SnapshotID: paySnap, Target: "1.37", Teams: []string{"payments", "search"}})
	webSnap := mustSnapshot(t, s, web, "w1", base)
	mustEval(t, s, store.Evaluation{ClusterID: web, SnapshotID: webSnap, Target: "1.36", Teams: []string{"frontend"}})
	// Moved: its old snapshot named payments, its latest names only infra.
	oldSnap := mustSnapshot(t, s, moved, "m1", base)
	mustEval(t, s, store.Evaluation{ClusterID: moved, SnapshotID: oldSnap, Target: "1.36", Teams: []string{"payments"}})
	newSnap := mustSnapshot(t, s, moved, "m2", at(1))
	mustEval(t, s, store.Evaluation{ClusterID: moved, SnapshotID: newSnap, Target: "1.36", Teams: []string{"infra"}})

	for _, tc := range []struct {
		teams []string
		want  []int64
	}{
		{[]string{"payments"}, []int64{pay}},
		{[]string{"search"}, []int64{pay}},
		{[]string{"infra", "frontend"}, []int64{web, moved}},
		{[]string{"nobody"}, nil},
		{nil, nil},
	} {
		got, err := s.ClustersOfTeams(ctx, tc.teams)
		if err != nil {
			t.Fatalf("ClustersOfTeams(%v): %v", tc.teams, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("ClustersOfTeams(%v) = %v, want %v", tc.teams, got, tc.want)
		}
	}

	// A newer evaluation of the same target replaces the older one's teams.
	mustEval(t, s, store.Evaluation{ClusterID: web, SnapshotID: webSnap, Target: "1.36", Teams: []string{}})
	if got, _ := s.ClustersOfTeams(ctx, []string{"frontend"}); len(got) != 0 {
		t.Errorf("after a newer evaluation without frontend, ClustersOfTeams(frontend) = %v, want none", got)
	}

	// A refresh rewrites them (a --team-map change with an unchanged result).
	cur, err := s.CurrentEvaluation(ctx, pay, "1.36")
	if err != nil {
		t.Fatal(err)
	}
	if cur.TeamsUnknown {
		t.Error("an evaluation written with Teams reads TeamsUnknown")
	}
	cur37, err := s.CurrentEvaluation(ctx, pay, "1.37")
	if err != nil {
		t.Fatal(err)
	}
	cur.Teams, cur37.Teams = []string{"ledger"}, []string{"ledger"}
	if _, _, err := s.CommitEvaluations(ctx, store.EvaluationBatch{
		ClusterID: pay, SnapshotID: paySnap,
		Current: map[string]int64{"1.36": cur.ID, "1.37": cur37.ID},
		Refresh: []store.Evaluation{cur, cur37},
	}); err != nil {
		t.Fatalf("CommitEvaluations(refresh): %v", err)
	}
	if got, _ := s.ClustersOfTeams(ctx, []string{"payments"}); len(got) != 0 {
		t.Errorf("after a refresh to ledger, ClustersOfTeams(payments) = %v, want none", got)
	}
	if got, _ := s.ClustersOfTeams(ctx, []string{"ledger"}); !slices.Equal(got, []int64{pay}) {
		t.Errorf("after a refresh to ledger, ClustersOfTeams(ledger) = %v, want [%d]", got, pay)
	}

	// Insert through CommitEvaluations, with a new snapshot.
	if _, _, err := s.CommitEvaluations(ctx, store.EvaluationBatch{
		ClusterID: web,
		Snapshot:  &store.Snapshot{ClusterID: web, Hash: "w2", ReceivedAt: at(2), Inventory: []byte(`{}`)},
		Insert:    []store.Evaluation{{Target: "1.36", Teams: []string{"frontend", "growth"}}},
	}); err != nil {
		t.Fatalf("CommitEvaluations(insert): %v", err)
	}
	if got, _ := s.ClustersOfTeams(ctx, []string{"growth"}); !slices.Equal(got, []int64{web}) {
		t.Errorf("ClustersOfTeams(growth) = %v, want [%d]", got, web)
	}
	if sum, err := s.CurrentEvaluationSummary(ctx, web, "1.36"); err != nil || sum.TeamsUnknown {
		t.Errorf("CurrentEvaluationSummary = (TeamsUnknown %v, err %v), want known teams", sum.TeamsUnknown, err)
	}
}
