package server

import (
	"context"
	"testing"
	"time"
)

// pruneCutoffs returns the cutoffs Prune has been called with so far.
func (f *fakeStore) pruneCutoffs() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.pruneCalls...)
}

// TestRetentionPrunesAtStartupAndPeriodically: with Config.Retention set,
// the server prunes once at startup (a long-stopped server catches up at
// once) and then every retentionInterval, each time with the cutoff
// now - Retention.
func TestRetentionPrunesAtStartupAndPeriodically(t *testing.T) {
	st := newFakeStore()
	s := newTestServer(t, st, func(c *Config) {
		c.Listen = "127.0.0.1:0"
		c.Retention = 90 * 24 * time.Hour
	})
	s.retentionInterval = 20 * time.Millisecond
	startServer(t, s)
	want := s.now().Add(-90 * 24 * time.Hour)
	deadline := time.Now().Add(5 * time.Second)
	for len(st.pruneCutoffs()) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	calls := st.pruneCutoffs()
	if len(calls) < 2 {
		t.Fatalf("Prune called %d times, want at startup and again after the interval", len(calls))
	}
	for i, c := range calls {
		if !c.Equal(want) {
			t.Errorf("Prune call %d cutoff = %v, want now - 90d = %v", i, c, want)
		}
	}
}

// TestRetentionZeroKeepsEverything: Retention 0 never prunes.
func TestRetentionZeroKeepsEverything(t *testing.T) {
	st := newFakeStore()
	s := newTestServer(t, st, func(c *Config) { c.Listen = "127.0.0.1:0" })
	s.retentionInterval = time.Millisecond
	stop := startServer(t, s)
	time.Sleep(30 * time.Millisecond)
	if err := stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls := st.pruneCutoffs(); len(calls) != 0 {
		t.Errorf("Prune called %d times with retention off, want 0", len(calls))
	}
}
