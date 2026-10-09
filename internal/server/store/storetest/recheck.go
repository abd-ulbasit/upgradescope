package storetest

import (
	"context"
	"errors"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// recheckPush is a push's commit for cluster c, authenticated with token
// ("" = the shared ingest token).
func recheckPush(c store.Cluster, token, hash string) store.EvaluationBatch {
	return store.EvaluationBatch{
		Cluster:     &c,
		IngestToken: token,
		Snapshot:    &store.Snapshot{Hash: hash, ReceivedAt: at(2), Inventory: []byte(`{}`)},
		Insert:      []store.Evaluation{{Target: "1.37", Score: 90, EvaluatedAt: at(2), Report: []byte(`{}`)}},
	}
}

// testCommitRechecksIngestToken: a push authenticated with a per-cluster
// token before that token was revoked writes nothing, a duplicate
// included: the commit requires the token still active for the cluster,
// in its own transaction (#240).
func testCommitRechecksIngestToken(t *testing.T, s store.Store) {
	ctx := context.Background()
	if _, err := s.CreateToken(ctx, "prod", "prod-token-0123456789abcdef0123456789"); err != nil {
		t.Fatal(err)
	}
	c := store.Cluster{Name: "prod", ClusterUID: "uid-prod", LastSeen: at(1)}
	if _, _, err := s.CommitEvaluations(ctx, recheckPush(c, "prod-token-0123456789abcdef0123456789", "aaa")); err != nil {
		t.Fatalf("push with an active token: %v", err)
	}
	registered, err := s.ClusterByName(ctx, "prod")
	if err != nil {
		t.Fatal(err)
	}
	c.ID = registered.ID
	if err := s.RevokeToken(ctx, "prod"); err != nil {
		t.Fatal(err)
	}
	for _, hash := range []string{"bbb", "aaa"} { // a new inventory, then a duplicate
		c.LastSeen = at(3)
		_, _, err := s.CommitEvaluations(ctx, recheckPush(c, "prod-token-0123456789abcdef0123456789", hash))
		if !errors.Is(err, store.ErrTokenRevoked) {
			t.Errorf("push %s with a revoked token: err = %v, want ErrTokenRevoked", hash, err)
		}
	}
	if latest, err := s.LatestSnapshot(ctx, c.ID); err != nil || latest.Hash != "aaa" {
		t.Errorf("latest snapshot = %q (%v), want the first push's only", latest.Hash, err)
	}
	if got, _ := s.GetCluster(ctx, c.ID); !got.LastSeen.Equal(at(1)) {
		t.Errorf("a refused push moved last seen to %v", got.LastSeen)
	}
	// A token of another cluster never commits for this one.
	if _, err := s.CreateToken(ctx, "dev", "dev-token-0123456789abcdef0123456789ab"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CommitEvaluations(ctx, recheckPush(c, "dev-token-0123456789abcdef0123456789ab", "ccc")); !errors.Is(err, store.ErrTokenRevoked) {
		t.Errorf("push for prod with dev's token: err = %v, want ErrTokenRevoked", err)
	}
	// The shared token ("") is the server's to check, not the store's.
	if _, _, err := s.CommitEvaluations(ctx, recheckPush(c, "", "ddd")); err != nil {
		t.Errorf("push with the shared token: %v", err)
	}
}

// testCommitRefusesChangedCluster: a push that looked its cluster up
// before a rename or a delete writes nothing: a commit for Cluster.ID
// that would land on another row (the name re-registered as a new
// cluster, or a duplicate of the renamed one's UID) is ErrClusterChanged
// (#240).
func testCommitRefusesChangedCluster(t *testing.T, s store.Store) {
	ctx := context.Background()
	c := store.Cluster{Name: "prod", ClusterUID: "uid-prod", LastSeen: at(1)}
	if _, _, err := s.CommitEvaluations(ctx, recheckPush(c, "", "aaa")); err != nil {
		t.Fatal(err)
	}
	registered, err := s.ClusterByName(ctx, "prod")
	if err != nil {
		t.Fatal(err)
	}
	c.ID = registered.ID

	if err := s.RenameCluster(ctx, "prod", "prod-eu"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CommitEvaluations(ctx, recheckPush(c, "", "bbb")); !errors.Is(err, store.ErrClusterChanged) {
		t.Errorf("push after a rename: err = %v, want ErrClusterChanged", err)
	}
	if _, err := s.ClusterByName(ctx, "prod"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a push after the rename re-registered prod (err %v)", err)
	}

	renamed := c
	renamed.Name = "prod-eu"
	if err := s.DeleteCluster(ctx, "prod-eu"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CommitEvaluations(ctx, recheckPush(renamed, "", "ccc")); !errors.Is(err, store.ErrClusterChanged) {
		t.Errorf("push after a delete: err = %v, want ErrClusterChanged", err)
	}
	if list, err := s.ListClusters(ctx); err != nil || len(list) != 0 {
		t.Errorf("clusters after the refused pushes = %+v (%v), want none", list, err)
	}
	// A push that found no cluster registers one, as before.
	c.ID = 0
	if _, _, err := s.CommitEvaluations(ctx, recheckPush(c, "", "ddd")); err != nil {
		t.Errorf("first push after the delete: %v", err)
	}
}
