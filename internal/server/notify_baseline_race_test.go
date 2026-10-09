package server

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// hookStore runs before, once, ahead of the next push's commit (a batch
// with a snapshot): between the push's reads of its notification
// baselines and its commit, where another writer can land.
type hookStore struct {
	store.Store
	mu     sync.Mutex
	before func()
}

func (h *hookStore) setBefore(f func()) { h.mu.Lock(); h.before = f; h.mu.Unlock() }

func (h *hookStore) CommitEvaluations(ctx context.Context, b store.EvaluationBatch) (int64, bool, error) {
	var f func()
	if b.Snapshot != nil {
		h.mu.Lock()
		f, h.before = h.before, nil
		h.mu.Unlock()
	}
	if f != nil {
		f()
	}
	return h.Store.CommitEvaluations(ctx, b)
}

// raceStores are the stores the baseline race is checked on: SQLite, and
// the fake the handler tests use (the Postgres store's commit check is
// the conformance suite's CommitRefusesStaleBaseline, under the DSN gate).
func raceStores(t *testing.T) map[string]func(t *testing.T) store.Store {
	return map[string]func(t *testing.T) store.Store{
		"sqlite": func(t *testing.T) store.Store {
			st, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			return st
		},
		"fake": func(*testing.T) store.Store { return newFakeStore() },
	}
}

// racingServers are two servers on one store (two replicas, or a push and
// the background pass of one), sharing one notifier; a's store runs the
// hook. b runs kbB.
func racingServers(t *testing.T, st store.Store, kbB kb.KB) (a, b *Server, hook *hookStore, rec *recordingNotifier) {
	t.Helper()
	rec = &recordingNotifier{}
	hook = &hookStore{Store: st}
	var err error
	if a, err = New(Config{Store: hook, KB: testKB(), Notifier: rec, IngestToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	if b, err = New(Config{Store: st, KB: kbB, Notifier: rec, IngestToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	return a, b, hook, rec
}

func kinds(rec *recordingNotifier) []string {
	var out []string
	for _, e := range rec.all() {
		out = append(out, e.Kind)
	}
	return out
}

// TestPushRacingAnotherPushKeepsTheBaseline (#241): A reads its baseline
// (ready), B pushes a blocked snapshot and announces the blocker, then A
// commits a ready one. A's delta against the replaced baseline would send
// nothing, leaving the new-blocker alert standing over a ready cluster;
// the commit is refused (ErrConflict) and A recomputes against B's, so
// the blocker is cleared by a became-ready.
func TestPushRacingAnotherPushKeepsTheBaseline(t *testing.T) {
	for name, open := range raceStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			st := open(t)
			a, b, hook, rec := racingServers(t, st, testKB())
			tsA, tsB := httptest.NewServer(a.Handler()), httptest.NewServer(b.Handler())
			defer tsA.Close()
			defer tsB.Close()

			base := testInventory()
			pushInventory(t, tsA.URL, "tok", base) // the first evaluation: silent
			hook.setBefore(func() { pushInventory(t, tsB.URL, "tok", testInventoryWithPSP()) })
			clean2 := base
			clean2.ServerVersion = "v1.34.3" // still ready, another snapshot
			pushInventory(t, tsA.URL, "tok", clean2)
			a.deliverOutbox(ctx)
			a.reevaluateAll(ctx)
			a.deliverOutbox(ctx)

			if got := kinds(rec); len(got) != 2 || got[0] != notify.KindNewBlocker || got[1] != notify.KindBecameReady {
				t.Errorf("delivered %v, want [new-blocker became-ready]: the blocker B announced is cleared", got)
			}
			c, err := st.ClusterByName(ctx, "prod-test")
			if err != nil {
				t.Fatal(err)
			}
			if e, err := st.CurrentEvaluation(ctx, c.ID, "1.35"); err != nil || !e.Ready {
				t.Errorf("current 1.35 = (ready %v, %v), want A's ready evaluation", e.Ready, err)
			}
		})
	}
}

// TestPushRacingTheBackgroundPassSendsOnce: the background pass of a
// server with a newer KB (B) re-judges the stored blocked snapshot ready
// and sends became-ready between A's baseline read and A's commit of a
// ready push. Against its stale baseline (blocked) A would send
// became-ready a second time; recomputed, it sends nothing.
func TestPushRacingTheBackgroundPassSendsOnce(t *testing.T) {
	removedLater := inventory.Version{Major: 1, Minor: 40}
	kbB := testKB()
	kbB.Version = "test-kb-2"
	kbB.APILifecycle[0].Removed = &removedLater
	for name, open := range raceStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			st := open(t)
			a, b, hook, rec := racingServers(t, st, kbB)
			tsA := httptest.NewServer(a.Handler())
			defer tsA.Close()

			pushInventory(t, tsA.URL, "tok", testInventoryWithPSP()) // blocked: the baseline
			hook.setBefore(func() { b.reevaluateAll(ctx) })
			pushInventory(t, tsA.URL, "tok", testInventory()) // ready
			a.deliverOutbox(ctx)

			if got := kinds(rec); len(got) != 1 || got[0] != notify.KindBecameReady {
				t.Errorf("delivered %v, want one became-ready (the background pass's)", got)
			}
		})
	}
}
