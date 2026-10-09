package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// slowStore makes every inventory load take delay, as a large inventory
// does, and counts them.
type slowStore struct {
	*countingStore
	delay time.Duration
}

func (s *slowStore) LatestSnapshot(ctx context.Context, id int64) (store.Snapshot, error) {
	time.Sleep(s.delay)
	return s.countingStore.LatestSnapshot(ctx, id)
}

type fleetTeamsView struct {
	Teams map[string]struct {
		WorstScore int      `json:"worstScore"`
		Blockers   int      `json:"blockers"`
		Clusters   []string `json:"clusters"`
	} `json:"teams"`
	Evaluated []struct {
		Name        string    `json:"name"`
		Source      string    `json:"source"`
		SnapshotID  int64     `json:"snapshotId"`
		EvaluatedAt time.Time `json:"evaluatedAt"`
	} `json:"evaluated"`
	Missing []string `json:"missing"`
}

// teamsFleet pushes n clusters, each with a PSP object in team-ns (owned
// by payments), on a server whose inventory loads take delay.
func teamsFleet(t *testing.T, n int, delay time.Duration) (*Server, *slowStore, *httptest.Server) {
	t.Helper()
	st := &slowStore{countingStore: &countingStore{fakeStore: newFakeStore()}, delay: delay}
	s, err := New(Config{Store: st, KB: testKB(), IngestToken: "ingest-tok"})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC) }
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	for i := range n {
		inv := testInventory()
		inv.ClusterID = fmt.Sprintf("uid-%d", i)
		inv.APIUsage = []inventory.APIUsage{{Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy", Count: 1, Namespaces: map[string]int{"team-ns": 1}}}
		inv.Namespaces = []inventory.NamespaceInfo{{Name: "team-ns", Team: "payments"}}
		pushCluster(t, ts, fmt.Sprintf("c%d", i), inv)
	}
	st.inventories.Store(0)
	return s, st, ts
}

// TestFleetTeamsWhatIfIsCached (#241): a rollup at a target nothing stores
// computed a what-if for every cluster on every request. The second
// request for the same target loads no inventory and answers the same;
// a cluster that pushes a new snapshot is computed again.
func TestFleetTeamsWhatIfIsCached(t *testing.T) {
	_, st, ts := teamsFleet(t, 5, 0)
	var first, second fleetTeamsView
	if resp := getJSON(t, ts, "/api/v1/fleet/teams?target=1.40", "", &first); resp.StatusCode != http.StatusOK {
		t.Fatalf("first GET = %d", resp.StatusCode)
	}
	if n := st.inventories.Swap(0); n != 5 {
		t.Fatalf("first rollup loaded %d inventories, want 5", n)
	}
	start := time.Now()
	if resp := getJSON(t, ts, "/api/v1/fleet/teams?target=1.40", "", &second); resp.StatusCode != http.StatusOK {
		t.Fatalf("second GET = %d", resp.StatusCode)
	}
	t.Logf("second rollup: %v", time.Since(start))
	if n := st.inventories.Swap(0); n != 0 {
		t.Errorf("second rollup loaded %d inventories, want 0", n)
	}
	if fmt.Sprint(first) != fmt.Sprint(second) {
		t.Errorf("cached rollup differs:\n first %+v\nsecond %+v", first, second)
	}
	pay := second.Teams["payments"]
	if pay.Blockers != 5 || len(pay.Clusters) != 5 || len(second.Evaluated) != 5 || second.Evaluated[0].Source != sourceWhatIf {
		t.Errorf("rollup = %+v, want payments blocked in all 5 clusters by what-ifs", second)
	}

	// c0 fixes its PSP object: only it is computed again, and it is clean.
	clean := testInventory()
	clean.ClusterID = "uid-0"
	clean.Namespaces = []inventory.NamespaceInfo{{Name: "team-ns", Team: "payments"}}
	pushCluster(t, ts, "c0", clean)
	st.inventories.Store(0)
	var third fleetTeamsView
	if resp := getJSON(t, ts, "/api/v1/fleet/teams?target=1.40", "", &third); resp.StatusCode != http.StatusOK {
		t.Fatalf("third GET = %d", resp.StatusCode)
	}
	if n := st.inventories.Load(); n != 1 {
		t.Errorf("rollup after one cluster's push loaded %d inventories, want 1", n)
	}
	if pay := third.Teams["payments"]; pay.Blockers != 4 || len(pay.Clusters) != 4 {
		t.Errorf("payments after c0's fix = %+v, want 4 blocked clusters", pay)
	}
}

// TestFleetTeamsDoesNotHoldTheReadSlot: the rollup takes the read slot per
// cluster, so a per-cluster read made during it waits for one cluster's
// compute, not the whole rollup's.
func TestFleetTeamsDoesNotHoldTheReadSlot(t *testing.T) {
	const delay = 100 * time.Millisecond
	_, _, ts := teamsFleet(t, 8, delay) // a rollup of at least 800ms
	var wg sync.WaitGroup
	var rollup time.Duration
	wg.Add(1)
	go func() {
		defer wg.Done()
		start := time.Now()
		if resp := getJSON(t, ts, "/api/v1/fleet/teams?target=1.40", "", nil); resp.StatusCode != http.StatusOK {
			t.Errorf("GET /fleet/teams = %d", resp.StatusCode)
		}
		rollup = time.Since(start)
	}()
	time.Sleep(2 * delay)
	start := time.Now()
	if resp := getJSON(t, ts, "/api/v1/clusters/1/report?target=1.40", "", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("GET report during the rollup = %d", resp.StatusCode)
	}
	report := time.Since(start)
	wg.Wait()
	t.Logf("rollup %v; a what-if report during it %v", rollup, report)
	// The report waits for at most one cluster's compute, then makes its own.
	if report > 4*delay || report >= rollup/2 {
		t.Errorf("a per-cluster read during the rollup took %v (rollup %v), want about two loads of %v", report, rollup, delay)
	}
}

// TestFleetTeamsTimeBound: a rollup that would run past its bound answers
// 503 with Retry-After instead of holding on, and keeps what it computed,
// so retries finish it.
func TestFleetTeamsTimeBound(t *testing.T) {
	const delay = 100 * time.Millisecond
	s, st, ts := teamsFleet(t, 6, delay)
	s.fleetTeamsBudget = 250 * time.Millisecond
	tries := 0
	for {
		tries++
		var got fleetTeamsView
		resp := getJSON(t, ts, "/api/v1/fleet/teams?target=1.40", "", &got)
		if resp.StatusCode == http.StatusOK {
			if pay := got.Teams["payments"]; len(pay.Clusters) != 6 {
				t.Errorf("finished rollup = %+v, want all 6 clusters", got)
			}
			break
		}
		if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
			t.Fatalf("try %d: GET = %d (Retry-After %q), want 503 with Retry-After", tries, resp.StatusCode, resp.Header.Get("Retry-After"))
		}
		if tries == 10 {
			t.Fatal("10 tries did not finish the rollup")
		}
	}
	if tries < 2 {
		t.Errorf("the rollup finished in %d try, want the bound to cut the first one", tries)
	}
	if n := st.inventories.Load(); n != 6 {
		t.Errorf("tries loaded %d inventories in all, want each of the 6 once", n)
	}
}
