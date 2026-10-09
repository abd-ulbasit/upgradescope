package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// beforeCommitStore runs hook once, just before the next CommitEvaluations
// that registers a snapshot: an admin operation landing while a push that
// was already authenticated and evaluated is about to commit.
type beforeCommitStore struct {
	store.Store
	mu   sync.Mutex
	hook func()
}

func (s *beforeCommitStore) arm(hook func()) {
	s.mu.Lock()
	s.hook = hook
	s.mu.Unlock()
}

func (s *beforeCommitStore) CommitEvaluations(ctx context.Context, b store.EvaluationBatch) (int64, bool, error) {
	s.mu.Lock()
	hook := s.hook
	if b.Snapshot != nil {
		s.hook = nil
	}
	s.mu.Unlock()
	if hook != nil && b.Snapshot != nil {
		hook()
	}
	return s.Store.CommitEvaluations(ctx, b)
}

// A push authenticated before a token revoke, a cluster delete or a
// rename used to commit anyway: the revoked token wrote, the deleted
// cluster came back without its tokens, and the rename left a second
// cluster with the same UID (#240). Each interleaving, on the duplicate
// fast path and with a changed inventory, now writes nothing: the revoke
// answers 401, the rename and the delete 409.
func TestPushRacingAnAdminOperationWritesNothing(t *testing.T) {
	const tok = "prod-test-token-0123456789abcdef01234"
	base := testInventory()
	base.ClusterID = "uid-1"
	changed := testInventoryWithPSP()
	changed.ClusterID = "uid-1"
	for _, tc := range []struct {
		name   string
		op     func(ctx context.Context, st store.Store) error
		status int
		left   []string // cluster names after the push
	}{
		{"revoke", func(ctx context.Context, st store.Store) error { return st.RevokeToken(ctx, "prod-test") }, http.StatusUnauthorized, []string{"prod-test"}},
		{"rename", func(ctx context.Context, st store.Store) error { return st.RenameCluster(ctx, "prod-test", "prod-eu") }, http.StatusConflict, []string{"prod-eu"}},
		{"delete", func(ctx context.Context, st store.Store) error { return st.DeleteCluster(ctx, "prod-test") }, http.StatusConflict, nil},
	} {
		for _, second := range []struct {
			name string
			inv  inventory.Inventory
		}{{"changed inventory", changed}, {"duplicate", base}} {
			t.Run(tc.name+", "+second.name, func(t *testing.T) {
				ctx := context.Background()
				st := &beforeCommitStore{Store: openSQLite(t)}
				if _, err := st.CreateToken(ctx, "prod-test", tok); err != nil {
					t.Fatal(err)
				}
				s := newTestServer(t, nil, func(c *Config) { c.Store = st; c.IngestToken = "" })
				ts := httptest.NewServer(s.Handler())
				defer ts.Close()
				push := func(inv inventory.Inventory) (*http.Response, map[string]any) {
					body, err := json.Marshal(map[string]any{"schemaVersion": 1, "clusterName": "prod-test", "inventory": inv})
					if err != nil {
						t.Fatal(err)
					}
					return postSnapshot(t, ts, tok, body, false)
				}
				if resp, out := push(base); resp.StatusCode != http.StatusAccepted {
					t.Fatalf("first push = %d %v", resp.StatusCode, out)
				}
				first, err := st.ClusterByName(ctx, "prod-test")
				if err != nil {
					t.Fatal(err)
				}
				before, err := st.LatestSnapshot(ctx, first.ID)
				if err != nil {
					t.Fatal(err)
				}
				st.arm(func() {
					if err := tc.op(ctx, st.Store); err != nil {
						t.Errorf("%s: %v", tc.name, err)
					}
				})
				if resp, out := push(second.inv); resp.StatusCode != tc.status {
					t.Errorf("push racing the %s = %d %v, want %d", tc.name, resp.StatusCode, out, tc.status)
				}
				list, err := st.ListClusters(ctx)
				if err != nil {
					t.Fatal(err)
				}
				var names []string
				for _, c := range list {
					names = append(names, c.Name)
					if c.ID != first.ID {
						t.Errorf("the push registered cluster %q (id %d), a second one", c.Name, c.ID)
					}
				}
				if len(names) != len(tc.left) || (len(names) == 1 && names[0] != tc.left[0]) {
					t.Errorf("clusters after the push = %v, want %v", names, tc.left)
				}
				if tc.left != nil {
					after, err := st.LatestSnapshot(ctx, first.ID)
					if err != nil || after.ID != before.ID {
						t.Errorf("latest snapshot = %d (%v), want the first push's %d: nothing stored", after.ID, err, before.ID)
					}
					if c, _ := st.GetCluster(ctx, first.ID); !c.LastSeen.Equal(first.LastSeen) {
						t.Errorf("last seen moved from %v to %v", first.LastSeen, c.LastSeen)
					}
				}
			})
		}
	}
}
