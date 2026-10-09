package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// metricValue reads one series of the server's registry: the family name
// and the label values in order. ok is false when the series is absent.
func metricValue(t *testing.T, s *Server, family string, labels ...string) (v float64, ok bool) {
	t.Helper()
	families, err := s.metrics.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range families {
		if mf.GetName() != family {
			continue
		}
	metric:
		for _, m := range mf.GetMetric() {
			if len(m.GetLabel()) != len(labels) {
				continue
			}
			for i, l := range m.GetLabel() {
				if l.GetValue() != labels[i] {
					continue metric
				}
			}
			if m.GetCounter() != nil {
				return m.GetCounter().GetValue(), true
			}
			return m.GetGauge().GetValue(), true
		}
	}
	return 0, false
}

const (
	mFailures = "upgradescope_retention_prune_failures_total"
	mLast     = "upgradescope_retention_last_success_timestamp_seconds"
	mDeleted  = "upgradescope_retention_rows_deleted_total"
)

func retentionServer(t *testing.T, st *fakeStore) *Server {
	t.Helper()
	return newTestServer(t, st, func(c *Config) { c.Retention = 90 * 24 * time.Hour })
}

// TestRetentionPruneFailureIsCountedAndLeavesTheGauge: a failed prune adds
// one to the failure counter for the store and leaves the last-success
// gauge as it was (absent before any success); the next prune that
// completes sets it, and does not reset the counter.
func TestRetentionPruneFailureIsCountedAndLeavesTheGauge(t *testing.T) {
	st := newFakeStore()
	s := retentionServer(t, st)
	ctx := context.Background()

	if v, ok := metricValue(t, s, mFailures, "sqlite"); !ok || v != 0 {
		t.Fatalf("failures before any prune = (%v, %v), want the series at 0", v, ok)
	}
	if _, ok := metricValue(t, s, mLast); ok {
		t.Fatal("last-success gauge present before any prune completed, want it absent")
	}

	st.errs["Prune"] = errors.New("disk I/O error (6410)")
	s.pruneOnce(ctx)
	if v, _ := metricValue(t, s, mFailures, "sqlite"); v != 1 {
		t.Errorf("failures after a failed prune = %v, want 1", v)
	}
	if _, ok := metricValue(t, s, mLast); ok {
		t.Error("a failed prune set the last-success gauge")
	}

	delete(st.errs, "Prune")
	first := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	s.pruneOnce(ctx)
	if v, ok := metricValue(t, s, mLast); !ok || v != float64(first.Unix()) {
		t.Errorf("last-success gauge after a complete prune = (%v, %v), want %d", v, ok, first.Unix())
	}
	if v, _ := metricValue(t, s, mFailures, "sqlite"); v != 1 {
		t.Errorf("failures after a later success = %v, want it kept at 1", v)
	}

	// A new failure leaves the gauge at the last success.
	later := first.Add(24 * time.Hour)
	s.now = func() time.Time { return later }
	st.errs["Prune"] = errors.New("database is locked")
	s.pruneOnce(ctx)
	if v, _ := metricValue(t, s, mLast); v != float64(first.Unix()) {
		t.Errorf("last-success gauge after a failure = %v, want the earlier success %d", v, first.Unix())
	}
	if v, _ := metricValue(t, s, mFailures, "sqlite"); v != 2 {
		t.Errorf("failures after two failed prunes = %v, want 2", v)
	}
}

// TestRetentionCountsRowsDeletedEvenWhenThePruneFails: the batches a
// failing prune committed are rows gone, and counted.
func TestRetentionCountsRowsDeletedEvenWhenThePruneFails(t *testing.T) {
	st := newFakeStore()
	s := retentionServer(t, st)
	st.errs["Prune"] = errors.New("boom")
	st.prunePart = store.PruneResult{Snapshots: 3, Evaluations: 10}
	s.pruneOnce(context.Background())
	delete(st.errs, "Prune")
	st.prunePart = store.PruneResult{}
	s.pruneOnce(context.Background())
	if v, _ := metricValue(t, s, mDeleted, "snapshots"); v != 3 {
		t.Errorf("snapshots deleted = %v, want 3", v)
	}
	if v, _ := metricValue(t, s, mDeleted, "evaluations"); v != 10 {
		t.Errorf("evaluations deleted = %v, want 10", v)
	}
}

// TestRetentionShutdownIsNotAFailure: a prune cut short because the server
// is stopping counts neither as a failure nor as a success.
func TestRetentionShutdownIsNotAFailure(t *testing.T) {
	st := newFakeStore()
	s := retentionServer(t, st)
	st.errs["Prune"] = context.Canceled
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.pruneOnce(ctx)
	if v, _ := metricValue(t, s, mFailures, "sqlite"); v != 0 {
		t.Errorf("failures after a prune stopped by shutdown = %v, want 0", v)
	}
	if _, ok := metricValue(t, s, mLast); ok {
		t.Error("a prune stopped by shutdown set the last-success gauge")
	}
}

// TestRetentionOffExportsNoRetentionSeries: with --retention 0 there is no
// prune, so no series: the chart omits the alert, and an absent gauge there
// does not mean a stuck prune.
func TestRetentionOffExportsNoRetentionSeries(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	for _, f := range []string{mFailures, mLast, mDeleted} {
		if _, ok := metricValue(t, s, f, "sqlite"); ok {
			t.Errorf("%s exported with retention off", f)
		}
	}
	families, err := s.metrics.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range families {
		switch mf.GetName() {
		case mFailures, mLast, mDeleted:
			t.Errorf("family %s exported with retention off", mf.GetName())
		}
	}
}

// TestReadyzIgnoresPruneFailures: retention failing is a metric and a log
// line, never a readiness failure: the server still ingests and serves.
func TestReadyzIgnoresPruneFailures(t *testing.T) {
	st := newFakeStore()
	s := retentionServer(t, st)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	st.errs["Prune"] = errors.New("disk I/O error (6410)")
	s.pruneOnce(context.Background())
	if resp, body := getRaw(t, ts, "/readyz", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("/readyz after a failed prune = %d %s, want 200", resp.StatusCode, body)
	}
}
