package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestStaleClusters: a cluster whose agent stopped pushing kept showing its
// last score with nothing to say the data was months old. The cluster
// list, cluster detail, fleet matrix and /metrics now carry lastSeen and
// stale (no push, duplicates included, within Config.StaleAfter).
func TestStaleClusters(t *testing.T) {
	st := newFakeStore()
	s := newTestServer(t, st, func(c *Config) { c.StaleAfter = 2 * time.Hour })
	clock := &fakeClock{t: s.now()}
	s.now = clock.now
	pushedAt := clock.now()
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	seedViaPush(t, ts)

	type views struct {
		list   []struct{ Stale *bool }
		detail struct {
			Stale    *bool
			LastSeen time.Time
		}
		fleet struct {
			Clusters []struct {
				Stale    *bool
				LastSeen time.Time
			}
		}
		metrics string
	}
	read := func() views {
		t.Helper()
		var v views
		getJSON(t, ts, "/api/v1/clusters", "", &v.list)
		getJSON(t, ts, "/api/v1/clusters/1", "", &v.detail)
		getJSON(t, ts, "/api/v1/fleet", "", &v.fleet)
		_, v.metrics = getRaw(t, ts, "/metrics", "")
		return v
	}
	check := func(v views, want bool) {
		t.Helper()
		if len(v.list) != 1 || v.list[0].Stale == nil || *v.list[0].Stale != want {
			t.Errorf("list stale = %+v, want %v", v.list, want)
		}
		if v.detail.Stale == nil || *v.detail.Stale != want || !v.detail.LastSeen.Equal(pushedAt) {
			t.Errorf("detail stale %v lastSeen %v, want %v / %v", v.detail.Stale, v.detail.LastSeen, want, pushedAt)
		}
		if len(v.fleet.Clusters) != 1 || v.fleet.Clusters[0].Stale == nil || *v.fleet.Clusters[0].Stale != want ||
			!v.fleet.Clusters[0].LastSeen.Equal(pushedAt) {
			t.Errorf("fleet row = %+v, want stale %v lastSeen %v", v.fleet.Clusters, want, pushedAt)
		}
		gauge := `upgradescope_cluster_stale{cluster="prod-eu-1"} 0`
		if want {
			gauge = `upgradescope_cluster_stale{cluster="prod-eu-1"} 1`
		}
		if !strings.Contains(v.metrics, gauge) {
			t.Errorf("/metrics has no %s", gauge)
		}
	}

	clock.set(pushedAt.Add(2 * time.Hour)) // exactly the window: still fresh
	check(read(), false)
	clock.set(pushedAt.Add(2*time.Hour + time.Second))
	check(read(), true)
}

// TestStaleAfterDefault: a zero Config.StaleAfter means DefaultStaleAfter,
// which leaves room for an unchanged cluster's hourly force-sync that
// lands on the agent's next tick (about 70 minutes apart by default).
func TestStaleAfterDefault(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	if s.staleAfter() != DefaultStaleAfter || DefaultStaleAfter < 90*time.Minute {
		t.Errorf("staleAfter = %v, DefaultStaleAfter = %v; want the default, above the agent's ~70m push spacing", s.staleAfter(), DefaultStaleAfter)
	}
}
