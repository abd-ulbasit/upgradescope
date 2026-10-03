package store

import (
	"context"
	"slices"
	"testing"
)

// A row written by a binary that predates 0008 has a NULL teams column:
// it reads TeamsUnknown, in the summary too, no scoped token reads its
// cluster, and the refresh the server's next pass makes writes them.
func TestEvaluationWrittenWithoutTeamsIsUnknown(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	cid := mustCluster(t, s, "prod")
	sid := mustSnapshot(t, s, cid, "aaa", tBase)
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO evaluations (cluster_id, snapshot_id, target, kb_version, score, ready, blockers, warnings, report, created_at, evaluated_at, team_map_hash, not_assessed)
		VALUES (?, ?, '1.36', 'kb', 100, 1, 0, 0, ?, ?, ?, '', '')`, cid, sid, []byte(`{}`), formatTime(tBase), formatTime(tBase))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	for name, read := range map[string]func() (Evaluation, error){
		"CurrentEvaluation":        func() (Evaluation, error) { return s.CurrentEvaluation(ctx, cid, "1.36") },
		"CurrentEvaluationSummary": func() (Evaluation, error) { return s.CurrentEvaluationSummary(ctx, cid, "1.36") },
	} {
		if got, err := read(); err != nil || !got.TeamsUnknown {
			t.Errorf("%s of a pre-0008 row = (TeamsUnknown %v, %v), want true", name, got.TeamsUnknown, err)
		}
	}
	if got, err := s.ClustersOfTeams(ctx, []string{"payments"}); err != nil || len(got) != 0 {
		t.Fatalf("ClustersOfTeams before the refresh = (%v, %v), want none", got, err)
	}
	if _, _, err := s.CommitEvaluations(ctx, EvaluationBatch{
		ClusterID: cid, SnapshotID: sid, Current: map[string]int64{"1.36": id},
		Refresh: []Evaluation{{ID: id, Report: []byte(`{}`), Teams: []string{"payments"}, EvaluatedAt: tPlus(30)}},
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.CurrentEvaluationSummary(ctx, cid, "1.36"); err != nil || got.TeamsUnknown {
		t.Errorf("after the refresh = (TeamsUnknown %v, %v), want known", got.TeamsUnknown, err)
	}
	if got, err := s.ClustersOfTeams(ctx, []string{"payments"}); err != nil || !slices.Equal(got, []int64{cid}) {
		t.Errorf("ClustersOfTeams after the refresh = (%v, %v), want [%d]", got, err, cid)
	}
}

// Read tokens are stored as ingest tokens are: the sha256 and the prefix,
// never the plaintext.
func TestReadTokenStoresOnlyHashAndPrefix(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	token := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	if _, err := s.CreateReadToken(ctx, []string{"payments"}, token); err != nil {
		t.Fatal(err)
	}
	var teams, hash, prefix string
	if err := s.db.QueryRowContext(ctx, `SELECT teams, token_hash, token_prefix FROM read_tokens`).Scan(&teams, &hash, &prefix); err != nil {
		t.Fatal(err)
	}
	if hash != HashToken(token) || prefix != "abcdef01" || teams != `["payments"]` {
		t.Errorf("stored (teams %s, hash %s, prefix %s), want the sha256 and the first 8 characters", teams, hash, prefix)
	}
}
