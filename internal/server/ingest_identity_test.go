package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// pushAs pushes inv as clusterName with token and returns the status and
// the decoded reply.
func pushAs(t *testing.T, ts *httptest.Server, token, clusterName string, inv inventory.Inventory) (int, map[string]any) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"schemaVersion": 1, "clusterName": clusterName, "inventory": inv})
	if err != nil {
		t.Fatal(err)
	}
	resp, out := postSnapshot(t, ts, token, body, false)
	return resp.StatusCode, out
}

// TestIngestUIDPushAdoptsNameRegisteredWithoutUID pins the documented
// adoption order (#368): a name first registered by a push with no
// clusterId is bound by the first clusterId pushed afterwards, from
// whichever cluster sends it, and that push continues the name's history.
// From then on another clusterId, or none, is a 409. The store side runs
// on Postgres too (storetest CommitAdoptsUIDOfNameRegisteredWithoutUID).
func TestIngestUIDPushAdoptsNameRegisteredWithoutUID(t *testing.T) {
	st := openSQLite(t)
	s := newTestServer(t, nil, func(c *Config) { c.Store = st })
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	anon := testInventoryWithPSP() // 1 blocker at the default target 1.35: score 75
	anon.ClusterID = ""
	if code, out := pushAs(t, ts, "ingest-tok", "shared-name", anon); code != http.StatusAccepted {
		t.Fatalf("UID-less first push = %d %v, want 202", code, out)
	}
	bound := testInventory() // clean: 100
	bound.ClusterID = "abababab-abab-abab-abab-abababababab"
	if code, out := pushAs(t, ts, "ingest-tok", "shared-name", bound); code != http.StatusAccepted {
		t.Fatalf("first push with a clusterId = %d %v, want 202", code, out)
	}
	c, err := st.ClusterByName(context.Background(), "shared-name")
	if err != nil || c.ClusterUID != bound.ClusterID {
		t.Fatalf("cluster = (%+v, %v), want the name bound to %s", c, err, bound.ClusterID)
	}
	points, err := st.ScoreHistory(context.Background(), c.ID, "1.35", 10)
	if err != nil || len(points) != 2 || points[0].Score != 75 || points[1].Score != 100 {
		t.Errorf("history = (%+v, %v), want [75 100]: the adopting push continues the name's history", points, err)
	}
	other := testInventoryWithPSP()
	other.ClusterID = "cdcdcdcd-cdcd-cdcd-cdcd-cdcdcdcdcdcd"
	again := testInventoryWithPSP()
	again.ClusterID = ""
	for _, inv := range []inventory.Inventory{other, again} {
		code, out := pushAs(t, ts, "ingest-tok", "shared-name", inv)
		if code != http.StatusConflict {
			t.Errorf("push with clusterId %q after the name is bound = %d %v, want 409", inv.ClusterID, code, out)
		}
	}
	if list, err := st.ListClusters(context.Background()); err != nil || len(list) != 1 {
		t.Errorf("clusters = (%+v, %v), want one", list, err)
	}
}

// TestIngestLogsUIDAdoption: the push that binds a name registered without
// a clusterId logs one line naming the cluster, the adopted clusterId and
// the snapshot, and never the token (#368). An ordinary push, the
// UID-less first one and the bound name's next one, logs none.
func TestIngestLogsUIDAdoption(t *testing.T) {
	logged := captureLog(t)
	st := openSQLite(t)
	s := newTestServer(t, nil, func(c *Config) { c.Store = st })
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	const uid = "abababab-abab-abab-abab-abababababab"
	adoptions := func() []string {
		var lines []string
		for _, l := range strings.Split(logged.String(), "\n") {
			if strings.Contains(l, "adopted") {
				lines = append(lines, l)
			}
		}
		return lines
	}

	anon := testInventoryWithPSP()
	anon.ClusterID = ""
	if code, out := pushAs(t, ts, "ingest-tok", "shared-name", anon); code != http.StatusAccepted {
		t.Fatalf("UID-less first push = %d %v", code, out)
	}
	if got := adoptions(); len(got) != 0 {
		t.Errorf("the UID-less first push logged an adoption: %q", got)
	}
	bound := testInventory()
	bound.ClusterID = uid
	code, out := pushAs(t, ts, "ingest-tok", "shared-name", bound)
	if code != http.StatusAccepted {
		t.Fatalf("adopting push = %d %v", code, out)
	}
	got := adoptions()
	if len(got) != 1 {
		t.Fatalf("adoption lines = %q, want one", got)
	}
	want := fmt.Sprintf(`cluster="shared-name" clusterId=%q snapshotId=%v`, uid, out["snapshotId"])
	if !strings.Contains(got[0], want) {
		t.Errorf("adoption line %q does not carry %s", got[0], want)
	}
	if strings.Contains(logged.String(), "ingest-tok") {
		t.Errorf("the log carries the ingest token:\n%s", logged.String())
	}
	next := testInventoryWithPSP()
	next.ClusterID = uid
	if code, out := pushAs(t, ts, "ingest-tok", "shared-name", next); code != http.StatusAccepted {
		t.Fatalf("next push of the bound name = %d %v", code, out)
	}
	if got := adoptions(); len(got) != 1 {
		t.Errorf("an ordinary push of the bound name logged an adoption: %q", got)
	}
}

// TestAdoptingPushSendsNoBecameReady: the push that binds a name
// registered without a clusterId continues a history that may be another
// cluster's, so a blocked-to-ready step across it is no news of this
// cluster and sends no became-ready (#368). The same step between two
// pushes of the bound cluster still does.
func TestAdoptingPushSendsNoBecameReady(t *testing.T) {
	st := openSQLite(t)
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	rec := &recordingNotifier{}
	s, err := New(Config{Store: st, KB: k, Notifier: rec, IngestToken: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	base := inventory.Inventory{
		SchemaVersion:   1,
		CollectorSchema: inventory.CurrentCollectorSchema,
		ServerVersion:   "v1.35.0",
		Capabilities:    collectedCaps(),
	}
	blocked := base
	blocked.APIUsage = []inventory.APIUsage{{Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy", Count: 1}}
	becameReady := func() int {
		s.deliverOutbox(context.Background())
		n := 0
		for _, ev := range rec.all() {
			if ev.Kind == notify.KindBecameReady {
				n++
			}
		}
		return n
	}

	pushInventory(t, ts.URL, "tok", blocked) // no clusterId: registers prod-test unbound
	clean := base
	clean.ClusterID = "uid-1"
	pushInventory(t, ts.URL, "tok", clean) // adopts uid-1: blocked → ready
	if n := becameReady(); n != 0 {
		t.Errorf("the adopting push sent %d became-ready, want none: %+v", n, rec.all())
	}
	// Control: the bound cluster's own blocked → ready step notifies.
	blocked.ClusterID = "uid-1"
	pushInventory(t, ts.URL, "tok", blocked)
	pushInventory(t, ts.URL, "tok", clean)
	if n := becameReady(); n != 1 {
		t.Errorf("the bound cluster's own blocked → ready step sent %d became-ready, want 1: %+v", n, rec.all())
	}
}

// beforeLookupStore runs hook once, just before the next ClusterByName:
// an admin operation landing after a push was authenticated, while its
// body is still read or it waits for the ingest slot, before the handler
// looks its cluster up.
type beforeLookupStore struct {
	store.Store
	mu   sync.Mutex
	hook func()
}

func (s *beforeLookupStore) arm(hook func()) {
	s.mu.Lock()
	s.hook = hook
	s.mu.Unlock()
}

func (s *beforeLookupStore) ClusterByName(ctx context.Context, name string) (store.Cluster, error) {
	s.mu.Lock()
	hook := s.hook
	s.hook = nil
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	return s.Store.ClusterByName(ctx, name)
}

// raceBeforeLookup registers prod-test with a per-cluster token, then
// pushes a changed inventory with token while op lands before the
// handler's lookup. It returns the push's status and reply, the
// cluster's first registration, and the store.
func raceBeforeLookup(t *testing.T, token string, op func(ctx context.Context, st store.Store) error) (int, map[string]any, store.Cluster, store.Store) {
	t.Helper()
	const perCluster = "prod-test-token-0123456789abcdef01234"
	ctx := context.Background()
	st := &beforeLookupStore{Store: openSQLite(t)}
	if _, err := st.CreateToken(ctx, "prod-test", perCluster); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, nil, func(c *Config) { c.Store = st })
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	first := testInventory()
	first.ClusterID = "uid-1"
	if code, out := pushAs(t, ts, perCluster, "prod-test", first); code != http.StatusAccepted {
		t.Fatalf("first push = %d %v", code, out)
	}
	registered, err := st.ClusterByName(ctx, "prod-test")
	if err != nil {
		t.Fatal(err)
	}
	st.arm(func() {
		if err := op(ctx, st.Store); err != nil {
			t.Errorf("admin operation: %v", err)
		}
	})
	if token == "" {
		token = perCluster
	}
	second := testInventoryWithPSP()
	second.ClusterID = "uid-1"
	code, out := pushAs(t, ts, token, "prod-test", second)
	return code, out, registered, st.Store
}

// clusterNames lists the registered clusters' names, failing the test on
// a cluster other than first: a push that registered a second one.
func clusterNames(t *testing.T, st store.Store, first store.Cluster) []string {
	t.Helper()
	list, err := st.ListClusters(context.Background())
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
	return names
}

// TestPerClusterPushRacingRenameBeforeLookupAnswers409: a per-cluster
// token's cluster is resolved at auth, so a rename or a delete that lands
// while the push is still read or queued, before the handler looks the
// cluster up, is a 409 "renamed or deleted" that stores nothing, not a
// 401 "revoked" (the token moved with the cluster, or went with it)
// (#369).
func TestPerClusterPushRacingRenameBeforeLookupAnswers409(t *testing.T) {
	for _, tc := range []struct {
		name string
		op   func(ctx context.Context, st store.Store) error
		left []string
	}{
		{"rename", func(ctx context.Context, st store.Store) error { return st.RenameCluster(ctx, "prod-test", "prod-eu") }, []string{"prod-eu"}},
		{"delete", func(ctx context.Context, st store.Store) error { return st.DeleteCluster(ctx, "prod-test") }, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, first, st := raceBeforeLookup(t, "", tc.op)
			if code != http.StatusConflict {
				t.Errorf("push racing the %s = %d %v, want 409", tc.name, code, out)
			}
			if msg, _ := out["error"].(string); !strings.Contains(msg, "renamed or deleted") {
				t.Errorf("409 message %q does not say the cluster was renamed or deleted", msg)
			}
			if names := clusterNames(t, st, first); fmt.Sprint(names) != fmt.Sprint(tc.left) {
				t.Errorf("clusters after the push = %v, want %v", names, tc.left)
			}
			if tc.left != nil {
				if n, err := st.ScoreHistory(context.Background(), first.ID, "1.35", 10); err != nil || len(n) != 1 {
					t.Errorf("history = (%+v, %v), want the first push's only: nothing stored", n, err)
				}
			}
		})
	}
}

// TestPerClusterPushRevokedBeforeLookupAnswers401: a revoke that lands
// before the handler's lookup is still a 401 that stores nothing: the
// cluster did not change, its token did (#369).
func TestPerClusterPushRevokedBeforeLookupAnswers401(t *testing.T) {
	code, out, first, st := raceBeforeLookup(t, "", func(ctx context.Context, st store.Store) error {
		return st.RevokeToken(ctx, "prod-test")
	})
	if code != http.StatusUnauthorized {
		t.Errorf("push racing the revoke = %d %v, want 401", code, out)
	}
	if names := clusterNames(t, st, first); fmt.Sprint(names) != "[prod-test]" {
		t.Errorf("clusters after the push = %v, want [prod-test]", names)
	}
	if n, err := st.ScoreHistory(context.Background(), first.ID, "1.35", 10); err != nil || len(n) != 1 {
		t.Errorf("history = (%+v, %v), want the first push's only: nothing stored", n, err)
	}
}

// TestSharedTokenPushRacingDeleteBeforeLookupRegistersTheName pins the
// narrowed claim (#369): the shared token names no cluster, so a push
// with it whose cluster is deleted before the handler's lookup registers
// the name afresh, as a push arriving after the delete would. The
// guarantee for it starts at the lookup
// (TestPushRacingAnAdminOperationWritesNothing).
func TestSharedTokenPushRacingDeleteBeforeLookupRegistersTheName(t *testing.T) {
	code, out, first, st := raceBeforeLookup(t, "ingest-tok", func(ctx context.Context, st store.Store) error {
		return st.DeleteCluster(ctx, "prod-test")
	})
	if code != http.StatusAccepted {
		t.Errorf("shared-token push racing the delete = %d %v, want 202 (documented)", code, out)
	}
	c, err := st.ClusterByName(context.Background(), "prod-test")
	if err != nil || c.ID == first.ID {
		t.Errorf("prod-test after the push = (%+v, %v), want it registered again as a new cluster", c, err)
	}
}
