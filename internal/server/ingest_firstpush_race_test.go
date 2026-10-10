package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// firstPushRace is a brand-new cluster's first push that loses a race to
// another writer registering the same name: ahead of the first commit the
// fake registers the cluster out of band (what a racing replica's commit
// does), and then either refuses the commit (store.ErrConflict) or, with
// snapshot, stores the snapshot being pushed as that writer's (the same
// hash), so the commit answers a duplicate. commits records the cluster id each
// commit of the push was made with.
type firstPushRace struct {
	st       *fakeStore
	srv      *Server
	rec      *recordingNotifier
	ts       *httptest.Server
	commits  []int64 // batch.Cluster.ID of every CommitEvaluations of the push
	rivalID  int64   // the id the racing writer registered
	snapshot bool
}

func newFirstPushRace(t *testing.T, snapshot bool) *firstPushRace {
	t.Helper()
	r := &firstPushRace{st: newFakeStore(), rec: &recordingNotifier{}, snapshot: snapshot}
	r.st.beforeCommit = func(b store.EvaluationBatch) error {
		if b.Cluster == nil { // a re-evaluation's commit, not the push's
			return nil
		}
		r.commits = append(r.commits, b.Cluster.ID)
		if len(r.commits) > 1 {
			return nil
		}
		// The first commit: the rival got there first. The hook runs
		// under the fake's lock, so the registration and this commit's
		// refusal are one step: no scheduling can come between them.
		id, err := r.st.upsertClusterLocked(store.Cluster{Name: b.Cluster.Name, ClusterUID: b.Cluster.ClusterUID, LastSeen: b.Cluster.LastSeen})
		if err != nil {
			return err
		}
		r.rivalID = id
		if r.snapshot {
			sn := *b.Snapshot
			sn.ID, sn.ClusterID = r.st.id(), id
			r.st.snapshots = append(r.st.snapshots, sn)
			return nil // the commit meets the rival's snapshot: a duplicate
		}
		return store.ErrConflict
	}
	r.srv = newTestServer(t, r.st, func(c *Config) { c.Notifier = r.rec })
	r.ts = httptest.NewServer(r.srv.Handler())
	t.Cleanup(r.ts.Close)
	return r
}

// push posts the cluster's first snapshot and returns the answer.
func (r *firstPushRace) push(t *testing.T) (*http.Response, map[string]any) {
	t.Helper()
	return postSnapshot(t, r.ts, "ingest-tok", pushReqBody(t, testInventoryWithPSP()), false)
}

// counts reads the rows the fake holds for the cluster.
func (r *firstPushRace) counts() (clusters, snapshots, evals int) {
	r.st.mu.Lock()
	defer r.st.mu.Unlock()
	return len(r.st.clusters), len(r.st.snapshots), len(r.st.evals)
}

// A brand-new cluster's first push whose commit is refused because a
// racing writer registered the name (store.ErrConflict) is evaluated
// again against the cluster that writer registered: the retry commits with
// its id, not 0, and the cluster ends with one row and one snapshot,
// and announces nothing (a first evaluation is silent). Without ingestSnapshot's re-lookup the retry would run as
// a new name again, read no latest snapshot, and meet the same refusal until its
// attempts ran out. #358.
func TestFirstPushRacingRegistrationRetriesWithTheRegisteredID(t *testing.T) {
	r := newFirstPushRace(t, false)
	resp, out := r.push(t)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("push = %d %v, want 202: the retry ingests it", resp.StatusCode, out)
	}
	if r.rivalID == 0 {
		t.Fatal("the racing registration never ran")
	}
	if want := []int64{0, r.rivalID}; !slices.Equal(r.commits, want) {
		t.Errorf("commits were made with cluster ids %v, want %v: the retry must carry the id the racing writer registered", r.commits, want)
	}
	clusters, snapshots, evals := r.counts()
	if clusters != 1 || snapshots != 1 {
		t.Errorf("rows = %d clusters, %d snapshots, want 1 and 1", clusters, snapshots)
	}
	if evals == 0 {
		t.Error("no evaluation was stored with the snapshot")
	}
	// A cluster's first evaluation is the silent baseline, so the raced
	// push announces nothing, once or twice.
	r.srv.deliverOutbox(context.Background())
	if got := kinds(r.rec); len(got) != 0 {
		t.Errorf("delivered %v, want no notification: a first evaluation is silent", got)
	}
	// The one snapshot is the push's, under the registered cluster.
	r.st.mu.Lock()
	defer r.st.mu.Unlock()
	if r.st.snapshots[0].ClusterID != r.rivalID {
		t.Errorf("snapshot belongs to cluster %d, want the registered %d", r.st.snapshots[0].ClusterID, r.rivalID)
	}
}

// The same race where the writer also stored this very snapshot: the
// commit answers a duplicate for a push that began as a new name, and the
// stored snapshot is re-judged under the registered cluster's id
// (ingestOnce), so the cluster has evaluations rather than a snapshot
// nobody judged. Judged under id 0, the re-evaluation finds no snapshot to
// commit against and is dropped. #358.
func TestFirstPushRacingTheSameSnapshotIsJudgedForTheRegisteredCluster(t *testing.T) {
	r := newFirstPushRace(t, true)
	resp, out := r.push(t)
	if resp.StatusCode != http.StatusOK || out["duplicate"] != true {
		t.Fatalf("push = %d %v, want 200 duplicate", resp.StatusCode, out)
	}
	clusters, snapshots, evals := r.counts()
	if clusters != 1 || snapshots != 1 {
		t.Errorf("rows = %d clusters, %d snapshots, want 1 and 1", clusters, snapshots)
	}
	if evals == 0 {
		t.Fatal("the duplicate's snapshot was never evaluated: the re-evaluation ran without the registered cluster's id")
	}
	r.st.mu.Lock()
	for _, e := range r.st.evals {
		if e.ClusterID != r.rivalID {
			t.Errorf("evaluation stored for cluster %d, want the registered %d", e.ClusterID, r.rivalID)
		}
	}
	r.st.mu.Unlock()
	r.srv.deliverOutbox(context.Background())
	if got := kinds(r.rec); len(got) != 0 {
		t.Errorf("delivered %v, want no notification: a first evaluation is silent", got)
	}
}

// clusterIDStore records the cluster id of every commit of a pushed
// snapshot.
type clusterIDStore struct {
	store.Store
	mu  sync.Mutex
	ids []int64
}

func (c *clusterIDStore) CommitEvaluations(ctx context.Context, b store.EvaluationBatch) (int64, bool, error) {
	if b.Snapshot != nil && b.Cluster != nil {
		c.mu.Lock()
		c.ids = append(c.ids, b.Cluster.ID)
		c.mu.Unlock()
	}
	return c.Store.CommitEvaluations(ctx, b)
}

// The race as two replicas produce it, on the stores that check the
// baseline: a blocked first push (A, which has a notification sink, so its
// commit expects the cluster to have no snapshot) meets a ready push for
// the same new name that B commits first. A's commit is refused for real,
// and its retry, carrying the id B registered, takes B's evaluation as its
// baseline: the cluster ends with one row, B's snapshot and A's, and one
// new-blocker notification, which is not repeated by the retry. Without the
// re-lookup the retry is a new name again, expects no snapshot again and is
// refused until its attempts run out: the push answers 503. #358.
func TestFirstPushLosingTheRegistrationRaceNotifiesOnce(t *testing.T) {
	for name, open := range raceStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			st := open(t)
			a, b, hook, rec := racingServers(t, st, testKB())
			commits := &clusterIDStore{Store: hook.Store}
			hook.Store = commits // A's commits are recorded under the hook
			tsA, tsB := httptest.NewServer(a.Handler()), httptest.NewServer(b.Handler())
			defer tsA.Close()
			defer tsB.Close()

			hook.setBefore(func() { pushInventory(t, tsB.URL, "tok", testInventory()) })
			pushInventory(t, tsA.URL, "tok", testInventoryWithPSP()) // requires 202
			a.deliverOutbox(ctx)

			clusters, err := st.ListClusters(ctx)
			if err != nil || len(clusters) != 1 {
				t.Fatalf("clusters = %v, %v, want exactly one", clusters, err)
			}
			id := clusters[0].ID
			if want := []int64{0, id}; !slices.Equal(commits.ids, want) {
				t.Errorf("A committed with cluster ids %v, want %v: the retry must carry the registered id", commits.ids, want)
			}
			if got := kinds(rec); len(got) != 1 || got[0] != notify.KindNewBlocker {
				t.Errorf("delivered %v, want exactly one %s", got, notify.KindNewBlocker)
			}
			if e, err := st.CurrentEvaluation(ctx, id, "1.35"); err != nil || e.Ready {
				t.Errorf("current 1.35 = (ready %v, %v), want A's blocked evaluation", e.Ready, err)
			}
		})
	}
}
