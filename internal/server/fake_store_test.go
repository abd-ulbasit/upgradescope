package server

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"sync"
	"time"

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
	outbox    []store.OutboxMessage
	tokens    map[string]*fakeToken // keyed by plaintext token

	// errs injects failures by method name, e.g. errs["InsertSnapshot"].
	errs map[string]error

	pruneCalls []time.Time // cutoffs Prune was called with
}

var _ store.Store = (*fakeStore)(nil)

type fakeToken struct {
	id      int64
	cluster string
	revoked bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		clusters: map[int64]store.Cluster{},
		tokens:   map[string]*fakeToken{},
		errs:     map[string]error{},
	}
}

func (f *fakeStore) id() int64 { f.nextID++; return f.nextID }

func (f *fakeStore) UpsertCluster(_ context.Context, c store.Cluster) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["UpsertCluster"]; err != nil {
		return 0, err
	}
	return f.upsertClusterLocked(c)
}

func (f *fakeStore) upsertClusterLocked(c store.Cluster) (int64, error) {
	for id, existing := range f.clusters {
		if existing.Name == c.Name {
			if existing.ClusterUID != "" && existing.ClusterUID != c.ClusterUID {
				return 0, &store.ClusterUIDConflictError{Name: c.Name, StoredUID: existing.ClusterUID, PushedUID: c.ClusterUID}
			}
			if c.ClusterUID != "" {
				existing.ClusterUID = c.ClusterUID
			}
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

func (f *fakeStore) DeleteCluster(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["DeleteCluster"]; err != nil {
		return err
	}
	for id, c := range f.clusters {
		if c.Name != name {
			continue
		}
		delete(f.clusters, id)
		f.snapshots = slices.DeleteFunc(f.snapshots, func(s store.Snapshot) bool { return s.ClusterID == id })
		f.evals = slices.DeleteFunc(f.evals, func(e store.Evaluation) bool { return e.ClusterID == id })
		f.outbox = slices.DeleteFunc(f.outbox, func(m store.OutboxMessage) bool { return m.ClusterID == id })
		for tok, tk := range f.tokens {
			if tk.cluster == name {
				delete(f.tokens, tok)
			}
		}
		return nil
	}
	return store.ErrNotFound
}

func (f *fakeStore) ClusterByName(_ context.Context, name string) (store.Cluster, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["ClusterByName"]; err != nil {
		return store.Cluster{}, err
	}
	for _, c := range f.clusters {
		if c.Name == name {
			return c, nil
		}
	}
	return store.Cluster{}, store.ErrNotFound
}

func (f *fakeStore) RenameCluster(_ context.Context, name, newName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["RenameCluster"]; err != nil {
		return err
	}
	found := int64(0)
	for id, c := range f.clusters {
		if c.Name == name {
			found = id
		}
	}
	for id, c := range f.clusters {
		if c.Name == newName && id != found && found != 0 {
			return store.ErrClusterNameTaken
		}
	}
	if found == 0 {
		return store.ErrNotFound
	}
	c := f.clusters[found]
	c.Name = newName
	f.clusters[found] = c
	for _, tk := range f.tokens {
		if tk.cluster == name {
			tk.cluster = newName
		}
	}
	return nil
}

// Prune mirrors the real stores: evaluations before cutoff, then
// unreferenced snapshots before it, sparing each cluster's latest.
func (f *fakeStore) Prune(_ context.Context, cutoff time.Time) (store.PruneResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pruneCalls = append(f.pruneCalls, cutoff)
	if err := f.errs["Prune"]; err != nil {
		return store.PruneResult{}, err
	}
	latest := map[int64]bool{}
	for id := range f.clusters {
		if sn, ok := f.latestSnapshotLocked(id); ok {
			latest[sn.ID] = true
		}
	}
	var res store.PruneResult
	f.evals = slices.DeleteFunc(f.evals, func(e store.Evaluation) bool {
		gone := e.CreatedAt.Before(cutoff) && !latest[e.SnapshotID]
		if gone {
			res.Evaluations++
		}
		return gone
	})
	f.snapshots = slices.DeleteFunc(f.snapshots, func(sn store.Snapshot) bool {
		referenced := slices.ContainsFunc(f.evals, func(e store.Evaluation) bool { return e.SnapshotID == sn.ID })
		gone := sn.ReceivedAt.Before(cutoff) && !latest[sn.ID] && !referenced
		if gone {
			res.Snapshots++
		}
		return gone
	})
	return res, nil
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

func (f *fakeStore) LatestSnapshotHead(_ context.Context, clusterID int64) (store.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["LatestSnapshotHead"]; err != nil {
		return store.Snapshot{}, err
	}
	if sn, ok := f.latestSnapshotLocked(clusterID); ok {
		sn.Inventory = nil
		return sn, nil
	}
	return store.Snapshot{}, store.ErrNotFound
}

func (f *fakeStore) LatestSnapshotHeads(_ context.Context) (map[int64]store.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["LatestSnapshotHeads"]; err != nil {
		return nil, err
	}
	out := map[int64]store.Snapshot{}
	for _, sn := range f.snapshots { // ascending id: the last one wins
		sn.Inventory = nil
		out[sn.ClusterID] = sn
	}
	return out, nil
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

// InsertEvaluation honors ctx cancellation (like a real DB driver) so tests
// can prove the evaluation fan-out runs detached from the request context.
func (f *fakeStore) InsertEvaluation(ctx context.Context, e store.Evaluation) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := f.errs["InsertEvaluation"]; err != nil {
		return 0, err
	}
	return f.insertEvalLocked(e), nil
}

func (f *fakeStore) insertEvalLocked(e store.Evaluation) int64 {
	if e.EvaluatedAt.IsZero() {
		e.EvaluatedAt = e.CreatedAt
	}
	e.ID = f.id()
	f.evals = append(f.evals, e)
	return e.ID
}

// currentEvalLocked is the newest evaluation of snapshotID for target
// (evals are appended in insert order, which is also created order here).
func (f *fakeStore) currentEvalLocked(snapshotID int64, target string) (store.Evaluation, bool) {
	for i := len(f.evals) - 1; i >= 0; i-- {
		if f.evals[i].SnapshotID == snapshotID && f.evals[i].Target == target {
			return f.evals[i], true
		}
	}
	return store.Evaluation{}, false
}

func (f *fakeStore) CurrentEvaluation(ctx context.Context, clusterID int64, target string) (store.Evaluation, error) {
	return f.current(ctx, "CurrentEvaluation", clusterID, target)
}

// CurrentEvaluationSummary is CurrentEvaluation without the report; its
// injected error is errs["CurrentEvaluationSummary"].
func (f *fakeStore) CurrentEvaluationSummary(ctx context.Context, clusterID int64, target string) (store.Evaluation, error) {
	e, err := f.current(ctx, "CurrentEvaluationSummary", clusterID, target)
	e.Report = nil
	return e, err
}

func (f *fakeStore) current(ctx context.Context, method string, clusterID int64, target string) (store.Evaluation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return store.Evaluation{}, err
	}
	if err := f.errs[method]; err != nil {
		return store.Evaluation{}, err
	}
	snap, ok := f.latestSnapshotLocked(clusterID)
	if !ok {
		return store.Evaluation{}, store.ErrNotFound
	}
	if e, ok := f.currentEvalLocked(snap.ID, target); ok {
		e.NotAssessed = fakeNotAssessed(e.Report)
		return e, nil
	}
	return store.Evaluation{}, store.ErrNotFound
}

// fakeNotAssessed derives NotAssessed from a report as the real stores do
// on write: the notAssessed array, nil when absent or empty.
func fakeNotAssessed(report []byte) []byte {
	var r struct {
		NotAssessed []json.RawMessage `json:"notAssessed"`
	}
	if json.Unmarshal(report, &r) != nil || len(r.NotAssessed) == 0 {
		return nil
	}
	out, _ := json.Marshal(r.NotAssessed)
	return out
}

func (f *fakeStore) LatestKnownEvaluation(_ context.Context, clusterID int64, target string) (store.Evaluation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["LatestKnownEvaluation"]; err != nil {
		return store.Evaluation{}, err
	}
	for i := len(f.evals) - 1; i >= 0; i-- {
		e := f.evals[i]
		if e.ClusterID == clusterID && e.Target == target && (e.Ready || e.Blockers > 0) {
			return e, nil
		}
	}
	return store.Evaluation{}, store.ErrNotFound
}

// CommitEvaluations mirrors the real stores: all-or-nothing, duplicate
// snapshots write nothing, stale SnapshotID/Current is ErrConflict.
func (f *fakeStore) CommitEvaluations(ctx context.Context, b store.EvaluationBatch) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	if err := f.errs["CommitEvaluations"]; err != nil {
		return 0, false, err
	}
	if b.Cluster != nil {
		// Check the UID rule before any write; the upsert itself happens
		// with the other writes, so a failed commit registers nothing.
		b.ClusterID = 0
		for id, c := range f.clusters {
			if c.Name != b.Cluster.Name {
				continue
			}
			if c.ClusterUID != "" && c.ClusterUID != b.Cluster.ClusterUID {
				return 0, false, &store.ClusterUIDConflictError{Name: c.Name, StoredUID: c.ClusterUID, PushedUID: b.Cluster.ClusterUID}
			}
			b.ClusterID = id
		}
	}
	upsert := func() {
		if b.Cluster != nil {
			b.ClusterID, _ = f.upsertClusterLocked(*b.Cluster) // conflict ruled out above
		}
	}
	latest, hasLatest := f.latestSnapshotLocked(b.ClusterID)
	snapID := b.SnapshotID
	if b.Snapshot != nil {
		if hasLatest && latest.Hash == b.Snapshot.Hash {
			upsert()
			for i := range f.snapshots {
				if f.snapshots[i].ID == latest.ID {
					f.snapshots[i].KBVersion, f.snapshots[i].AgentVersion = b.Snapshot.KBVersion, b.Snapshot.AgentVersion
				}
			}
			return latest.ID, true, nil
		}
	} else {
		if !hasLatest || latest.ID != snapID {
			return 0, false, store.ErrConflict
		}
		for target, want := range b.Current {
			var got int64
			if e, ok := f.currentEvalLocked(snapID, target); ok {
				got = e.ID
			}
			if got != want {
				return 0, false, store.ErrConflict
			}
		}
	}
	refreshAt := make([]int, len(b.Refresh))
	for i, r := range b.Refresh {
		refreshAt[i] = slices.IndexFunc(f.evals, func(e store.Evaluation) bool { return e.ID == r.ID })
		if refreshAt[i] < 0 {
			return 0, false, store.ErrNotFound
		}
	}
	upsert()
	if b.Snapshot != nil {
		sn := *b.Snapshot
		sn.ID, sn.ClusterID = f.id(), b.ClusterID
		snapID = sn.ID
		f.snapshots = append(f.snapshots, sn)
	}
	for _, e := range b.Insert {
		e.ClusterID, e.SnapshotID = b.ClusterID, snapID
		f.insertEvalLocked(e)
	}
	for i, r := range b.Refresh {
		e := &f.evals[refreshAt[i]]
		e.Report, e.KBVersion, e.TeamMapHash = r.Report, r.KBVersion, r.TeamMapHash
		e.Blockers, e.Warnings, e.EvaluatedAt = r.Blockers, r.Warnings, r.EvaluatedAt
	}
	for _, m := range b.Outbox {
		m.ID, m.ClusterID = f.id(), b.ClusterID
		if m.NextAttemptAt.IsZero() {
			m.NextAttemptAt = m.CreatedAt
		}
		f.outbox = append(f.outbox, m)
	}
	return snapID, false, nil
}

func (f *fakeStore) ClaimOutbox(_ context.Context, now time.Time, lease time.Duration, limit int) ([]store.OutboxMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["ClaimOutbox"]; err != nil {
		return nil, err
	}
	var out []store.OutboxMessage
	for i := range f.outbox {
		if len(out) == limit {
			break
		}
		m := &f.outbox[i]
		if m.NextAttemptAt.After(now) {
			continue
		}
		m.Attempts++
		m.NextAttemptAt = now.Add(lease)
		out = append(out, *m)
	}
	return out, nil
}

func (f *fakeStore) DeleteOutbox(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := slices.IndexFunc(f.outbox, func(m store.OutboxMessage) bool { return m.ID == id })
	if i < 0 {
		return store.ErrNotFound
	}
	f.outbox = slices.Delete(f.outbox, i, i+1)
	return nil
}

func (f *fakeStore) RescheduleOutbox(_ context.Context, id int64, next time.Time, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := slices.IndexFunc(f.outbox, func(m store.OutboxMessage) bool { return m.ID == id })
	if i < 0 {
		return store.ErrNotFound
	}
	f.outbox[i].NextAttemptAt = next
	return nil
}

func (f *fakeStore) DeferOutbox(_ context.Context, id int64, next time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := slices.IndexFunc(f.outbox, func(m store.OutboxMessage) bool { return m.ID == id })
	if i < 0 {
		return store.ErrNotFound
	}
	f.outbox[i].NextAttemptAt = next
	f.outbox[i].Attempts = max(f.outbox[i].Attempts-1, 0)
	return nil
}

func (f *fakeStore) LatestEvaluation(ctx context.Context, clusterID int64, target string) (store.Evaluation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return store.Evaluation{}, err
	}
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

func (f *fakeStore) CreateToken(_ context.Context, clusterName, token string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["CreateToken"]; err != nil {
		return 0, err
	}
	if _, exists := f.tokens[token]; exists {
		return 0, store.ErrNotFound // any error works; real stores fail on UNIQUE
	}
	id := f.id()
	f.tokens[token] = &fakeToken{id: id, cluster: clusterName}
	return id, nil
}

func (f *fakeStore) ListTokens(_ context.Context, clusterName string) ([]store.Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["ListTokens"]; err != nil {
		return nil, err
	}
	var out []store.Token
	for _, tk := range f.tokens {
		if clusterName != "" && tk.cluster != clusterName {
			continue
		}
		row := store.Token{ID: tk.id, ClusterName: tk.cluster}
		if tk.revoked {
			row.RevokedAt = &time.Time{}
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *fakeStore) RevokeTokenID(_ context.Context, clusterName string, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["RevokeTokenID"]; err != nil {
		return err
	}
	for _, tk := range f.tokens {
		if tk.id == id && tk.cluster == clusterName && !tk.revoked {
			tk.revoked = true
			return nil
		}
	}
	return store.ErrNotFound
}

func (f *fakeStore) ValidToken(_ context.Context, token string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["ValidToken"]; err != nil {
		return "", false, err
	}
	tk, ok := f.tokens[token]
	if !ok || tk.revoked {
		return "", false, nil
	}
	return tk.cluster, true, nil
}

func (f *fakeStore) RevokeToken(_ context.Context, clusterName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["RevokeToken"]; err != nil {
		return err
	}
	revoked := false
	for _, tk := range f.tokens {
		if tk.cluster == clusterName && !tk.revoked {
			tk.revoked = true
			revoked = true
		}
	}
	if !revoked {
		return store.ErrNotFound
	}
	return nil
}

func (f *fakeStore) Close() error { return nil }
