package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// arrivalNotifier records, in order, the delivery ids it received and
// when.
type arrivalNotifier struct {
	mu  sync.Mutex
	ids []string
	at  []time.Time
}

func (a *arrivalNotifier) Notify(_ context.Context, n notify.Notification) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ids, a.at = append(a.ids, n.DeliveryID), append(a.at, time.Now())
	return nil
}

func (a *arrivalNotifier) arrivals() ([]string, []time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.ids...), append([]time.Time(nil), a.at...)
}

// hangingNotifier never answers: each delivery lasts until its context
// (notifyTimeout) ends. It records when each one started and ended.
type hangingNotifier struct {
	mu         sync.Mutex
	start, end map[string]time.Time
}

func (h *hangingNotifier) Notify(ctx context.Context, n notify.Notification) error {
	h.mu.Lock()
	if h.start == nil {
		h.start, h.end = map[string]time.Time{}, map[string]time.Time{}
	}
	h.start[n.DeliveryID] = time.Now()
	h.mu.Unlock()
	<-ctx.Done()
	h.mu.Lock()
	h.end[n.DeliveryID] = time.Now()
	h.mu.Unlock()
	return ctx.Err()
}

// queueFor queues n messages for sink, delivery ids "<sink>-<i>".
func queueFor(t *testing.T, st *fakeStore, sink string, n int, at time.Time) {
	t.Helper()
	st.mu.Lock()
	defer st.mu.Unlock()
	for i := range n {
		payload, err := json.Marshal(notify.Notification{SchemaVersion: notify.SchemaVersion, DeliveryID: fmt.Sprintf("%s-%02d", sink, i),
			Type: notify.TypeReadinessChanged, Timestamp: at, Cluster: notify.Cluster{ID: 1, Name: "prod"}})
		if err != nil {
			t.Fatal(err)
		}
		st.outbox = append(st.outbox, store.OutboxMessage{ID: st.id(), ClusterID: 1, Sink: sink, Payload: payload, CreatedAt: at, NextAttemptAt: at})
	}
}

// TestHungSinkDoesNotDelayOtherSinks (#241): messages for every sink were
// delivered one after another, so a sink that never answered held each
// healthy sink's message behind its own for a whole timeout: the healthy
// sink's 20th notification arrived after 20 timeouts. Sinks are delivered
// concurrently now, each in its queue order.
func TestHungSinkDoesNotDelayOtherSinks(t *testing.T) {
	st := newFakeStore()
	hung, healthy := &hangingNotifier{}, &arrivalNotifier{}
	s := newTestServer(t, st, func(c *Config) { c.Notifier = notify.Multi(hung, healthy) })
	s.now = time.Now
	s.notifyTimeout = 100 * time.Millisecond
	if len(s.sinks) != 2 {
		t.Fatalf("sinks = %+v, want two", s.sinks)
	}
	// Interleaved, oldest first, as two notifications a pass queue them.
	now := time.Now()
	for i := range 20 {
		for _, sk := range s.sinks {
			payload, _ := json.Marshal(notify.Notification{SchemaVersion: notify.SchemaVersion, DeliveryID: fmt.Sprintf("%02d", i),
				Type: notify.TypeReadinessChanged, Timestamp: now, Cluster: notify.Cluster{ID: 1, Name: "prod"}})
			st.mu.Lock()
			st.outbox = append(st.outbox, store.OutboxMessage{ID: st.id(), ClusterID: 1, Sink: sk.name, Payload: payload, CreatedAt: now, NextAttemptAt: now})
			st.mu.Unlock()
		}
	}
	start := time.Now()
	s.deliverOutbox(context.Background())
	ids, at := healthy.arrivals()
	if len(ids) != 20 {
		t.Fatalf("healthy sink got %d notifications, want 20", len(ids))
	}
	for i, id := range ids {
		if want := fmt.Sprintf("%02d", i); id != want {
			t.Fatalf("healthy sink's order = %v, want queue order", ids)
		}
	}
	if took := at[19].Sub(start); took > 5*s.notifyTimeout {
		t.Errorf("the healthy sink's 20th notification arrived after %v, want within about one timeout (%v), not 20", took, s.notifyTimeout)
	}
}

// leaseStore records the lease each claimed message was given.
type leaseStore struct {
	*fakeStore
	mu     sync.Mutex
	leases map[int64][]time.Time
}

func (l *leaseStore) ClaimOutbox(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]store.OutboxMessage, error) {
	msgs, err := l.fakeStore.ClaimOutbox(ctx, now, lease, limit)
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, m := range msgs {
		l.leases[m.ID] = append(l.leases[m.ID], m.NextAttemptAt)
	}
	return msgs, err
}

// TestDeliveryNeverOutlastsItsLease: a claimed message is re-claimable
// once its lease ends (by another replica, or this one's next pass), so a
// delivery that ran past it could be made twice. A delivery starts only
// when its timeout ends within the lease; the rest of a hung sink's batch
// is put back unattempted, and every message is still tried.
func TestDeliveryNeverOutlastsItsLease(t *testing.T) {
	fake := newFakeStore()
	st := &leaseStore{fakeStore: fake, leases: map[int64][]time.Time{}}
	hung := &hangingNotifier{}
	s, err := New(Config{Store: st, KB: testKB(), IngestToken: "tok", Notifier: hung})
	if err != nil {
		t.Fatal(err)
	}
	s.now = time.Now
	s.notifyTimeout = 50 * time.Millisecond
	s.outboxLease = 175 * time.Millisecond // about three deliveries
	queueFor(t, fake, s.sinks[0].name, 10, time.Now())
	ids := map[string]int64{}
	fake.mu.Lock()
	for _, m := range fake.outbox {
		n, err := notificationOf(m)
		if err != nil {
			t.Fatal(err)
		}
		ids[n.DeliveryID] = m.ID
	}
	fake.mu.Unlock()

	s.deliverOutbox(context.Background())

	hung.mu.Lock()
	defer hung.mu.Unlock()
	if len(hung.start) != 10 {
		t.Errorf("%d of 10 messages were tried, want every one", len(hung.start))
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	for id, end := range hung.end {
		leases := st.leases[ids[id]]
		lease := leases[len(leases)-1] // the claim that delivered it
		if end.After(lease) {
			t.Errorf("message %s was still being delivered %v after its lease ended", id, end.Sub(lease))
		}
	}
}

// TestOutboxLeaseCoversABatch pins the invariant behind the lease: a
// whole batch delivered at the shipped sinks' per-attempt bound fits in
// one lease, so even a batch for one hung sink never has a message put
// back for want of lease.
func TestOutboxLeaseCoversABatch(t *testing.T) {
	const shippedAttempt = 2 * time.Second // notify's webhookTimeout (NT-02)
	if outboxBatch*shippedAttempt >= outboxLease {
		t.Errorf("outboxBatch x %v = %v, want under outboxLease %v", shippedAttempt, outboxBatch*shippedAttempt, outboxLease)
	}
	if notifyTimeout >= outboxLease {
		t.Errorf("notifyTimeout %v must be under outboxLease %v, or no delivery could start", notifyTimeout, outboxLease)
	}
}
