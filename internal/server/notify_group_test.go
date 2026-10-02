package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// TestOneNotificationPerPassAcrossTargets: with --targets, a blocker that
// does not depend on the target (here PodSecurityPolicy, removed in 1.35,
// so a blocker for 1.35, 1.36 and 1.37) used to be sent once per target.
// A pass now sends one notification per cluster, and the change lists
// every target it applies to.
func TestOneNotificationPerPassAcrossTargets(t *testing.T) {
	st := newFakeStore()
	rec := &recordingNotifier{}
	s := newTestServer(t, st, func(c *Config) {
		c.ExtraTargets = []string{"1.36", "1.37"}
		c.Notifier = rec
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	for _, body := range [][]byte{pushReqBody(t, testInventory()), pushReqBody(t, testInventoryWithPSP())} {
		if resp, out := postSnapshot(t, ts, "ingest-tok", body, false); resp.StatusCode != http.StatusAccepted {
			t.Fatalf("push = %d %v", resp.StatusCode, out)
		}
	}
	s.deliverOutbox(context.Background())

	got := rec.notifications()
	if len(got) != 1 {
		t.Fatalf("notifications = %d, want 1 for the pass: %+v", len(got), got)
	}
	n := got[0]
	if n.SchemaVersion != notify.SchemaVersion || n.Type != notify.TypeReadinessChanged || len(n.DeliveryID) != 32 ||
		!n.Timestamp.Equal(s.now()) || n.Cluster != (notify.Cluster{ID: 1, Name: "prod-eu-1"}) {
		t.Errorf("envelope = %+v", n)
	}
	if len(n.Changes) != 1 {
		t.Fatalf("changes = %+v, want one new blocker", n.Changes)
	}
	c := n.Changes[0]
	if c.Kind != notify.KindNewBlocker || c.Severity != "blocker" || c.Key == "" || !slices.Equal(c.Targets, []string{"1.35", "1.36", "1.37"}) {
		t.Errorf("change = %+v, want one new-blocker for 1.35, 1.36 and 1.37", c)
	}
	if len(n.Targets) != 3 || n.Targets[0] != (notify.Target{Target: "1.35", Verdict: "blocked", Score: 75, Blockers: 1}) {
		t.Errorf("targets = %+v, want the three blocked verdicts", n.Targets)
	}
}

// TestWebhookDeliveryIsSigned drives a pass end to end to a generic
// webhook with a secret: the receiver gets the versioned JSON and a
// signature it can verify over the raw body.
func TestWebhookDeliveryIsSigned(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	var sigs []string
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies, sigs = append(bodies, b), append(sigs, r.Header.Get(notify.SignatureHeader))
		mu.Unlock()
	}))
	defer recv.Close()
	hook := notify.NewGenericWebhook(recv.URL)
	hook.Secret = "hook-secret"
	s := newTestServer(t, newFakeStore(), func(c *Config) { c.Notifier = hook })
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	blockedThenClean(t, ts)
	s.deliverOutbox(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("deliveries = %d, want 1", len(bodies))
	}
	if sigs[0] != notify.Sign("hook-secret", bodies[0]) {
		t.Errorf("signature %q does not verify", sigs[0])
	}
	var raw map[string]any
	if err := json.Unmarshal(bodies[0], &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"schemaVersion", "deliveryId", "type", "timestamp", "cluster", "targets", "changes"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("payload lacks %q: %s", key, bodies[0])
		}
	}
}

// newLegacyMessage is an outbox message as a pre-v1 server queued it.
func newLegacyMessage(sink string, at time.Time) store.OutboxMessage {
	return store.OutboxMessage{ID: 999, Sink: sink, CreatedAt: at, NextAttemptAt: at,
		Payload: []byte(`{"Cluster":"prod","Target":"1.36","Kind":"new-blocker","Title":"x removed","Detail":"d"}`)}
}

// TestLegacyOutboxPayloadStillDelivers: a message queued by a server that
// predates the versioned payload (one PascalCase event) is delivered as a
// one-change notification after an upgrade, not dropped as corrupt.
func TestLegacyOutboxPayloadStillDelivers(t *testing.T) {
	st := newFakeStore()
	rec := &recordingNotifier{}
	s := newTestServer(t, st, func(c *Config) { c.Notifier = rec })
	st.outbox = append(st.outbox, newLegacyMessage(s.sinks[0].name, s.now()))
	s.deliverOutbox(context.Background())
	got := rec.notifications()
	if len(got) != 1 || got[0].Cluster.Name != "prod" || got[0].DeliveryID == "" || len(got[0].Changes) != 1 ||
		got[0].Changes[0].Kind != notify.KindNewBlocker || !slices.Equal(got[0].Changes[0].Targets, []string{"1.36"}) {
		t.Errorf("legacy message delivered as %+v", got)
	}
}
