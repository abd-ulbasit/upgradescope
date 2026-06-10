package server

import (
	"context"
	"sort"
	"sync"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// fakeStore is a hand-written in-memory store.Store so handler tests never
// touch sqlite. IDs are assigned from one shared sequence (cluster 1,
// snapshot 2, eval 3, ... in typical single-cluster tests). It mirrors the
// contract's semantics: ErrNotFound sentinels, duplicate iff same
// cluster+hash as the latest snapshot, ScoreHistory oldest-first.
type fakeStore struct {
	mu     sync.Mutex
	nextID int64

	clusters  map[int64]store.Cluster
	snapshots []store.Snapshot
	evals     []store.Evaluation

	// errs injects failures by method name, e.g. errs["InsertSnapshot"].
	errs map[string]error
}

var _ store.Store = (*fakeStore)(nil)

func newFakeStore() *fakeStore {
	return &fakeStore{clusters: map[int64]store.Cluster{}, errs: map[string]error{}}
}

func (f *fakeStore) id() int64 { f.nextID++; return f.nextID }

func (f *fakeStore) UpsertCluster(_ context.Context, c store.Cluster) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["UpsertCluster"]; err != nil {
		return 0, err
	}
	for id, existing := range f.clusters {
		if existing.Name == c.Name {
			existing.ClusterUID = c.ClusterUID
			existing.LastSeen = c.LastSeen
			f.clusters[id] = existing
			return id, nil
		}
	}
	id := f.id()
	c.ID = id
	c.FirstSeen = c.LastSeen
	f.clusters[id] = c
	return id, nil
}

func (f *fakeStore) latestSnapshotLocked(clusterID int64) (store.Snapshot, bool) {
	for i := len(f.snapshots) - 1; i >= 0; i-- {
		if f.snapshots[i].ClusterID == clusterID {
			return f.snapshots[i], true
		}
	}
	return store.Snapshot{}, false
}

func (f *fakeStore) InsertSnapshot(_ context.Context, sn store.Snapshot) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["InsertSnapshot"]; err != nil {
		return 0, false, err
	}
	if latest, ok := f.latestSnapshotLocked(sn.ClusterID); ok && latest.Hash == sn.Hash {
		return latest.ID, true, nil
	}
	sn.ID = f.id()
	f.snapshots = append(f.snapshots, sn)
	return sn.ID, false, nil
}

func (f *fakeStore) LatestSnapshot(_ context.Context, clusterID int64) (store.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["LatestSnapshot"]; err != nil {
		return store.Snapshot{}, err
	}
	if sn, ok := f.latestSnapshotLocked(clusterID); ok {
		return sn, nil
	}
	return store.Snapshot{}, store.ErrNotFound
}

func (f *fakeStore) ListClusters(_ context.Context) ([]store.Cluster, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["ListClusters"]; err != nil {
		return nil, err
	}
	out := make([]store.Cluster, 0, len(f.clusters))
	for _, c := range f.clusters {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *fakeStore) GetCluster(_ context.Context, id int64) (store.Cluster, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["GetCluster"]; err != nil {
		return store.Cluster{}, err
	}
	c, ok := f.clusters[id]
	if !ok {
		return store.Cluster{}, store.ErrNotFound
	}
	return c, nil
}

func (f *fakeStore) InsertEvaluation(_ context.Context, e store.Evaluation) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["InsertEvaluation"]; err != nil {
		return 0, err
	}
	e.ID = f.id()
	f.evals = append(f.evals, e)
	return e.ID, nil
}

func (f *fakeStore) LatestEvaluation(_ context.Context, clusterID int64, target string) (store.Evaluation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["LatestEvaluation"]; err != nil {
		return store.Evaluation{}, err
	}
	for i := len(f.evals) - 1; i >= 0; i-- {
		if f.evals[i].ClusterID == clusterID && f.evals[i].Target == target {
			return f.evals[i], nil
		}
	}
	return store.Evaluation{}, store.ErrNotFound
}

func (f *fakeStore) ScoreHistory(_ context.Context, clusterID int64, target string, limit int) ([]store.ScorePoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["ScoreHistory"]; err != nil {
		return nil, err
	}
	var all []store.ScorePoint
	for _, e := range f.evals {
		if e.ClusterID == clusterID && e.Target == target {
			all = append(all, store.ScorePoint{At: e.CreatedAt, Score: e.Score, Ready: e.Ready})
		}
	}
	if limit > 0 && len(all) > limit {
		all = all[len(all)-limit:] // most recent N, still oldest-first
	}
	return all, nil // oldest first — matches the store contract
}

func (f *fakeStore) Close() error { return nil }
