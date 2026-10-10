package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/abd-ulbasit/upgradescope/internal/collect"
	"github.com/abd-ulbasit/upgradescope/internal/crd"
)

// #228: the pods outside kube-system are listed every --pod-pass-every
// ticks, and the age of what a tick reuses is on the status, the report and
// the metrics.

func TestConfigPodPassDefaultsAndLimits(t *testing.T) {
	var c Config
	if err := c.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	if c.PodPassEvery != collect.DefaultPodPassEvery || c.PodPassMaxAge != collect.DefaultPodPassMaxAge {
		t.Errorf("defaults = %d, %s; want %d, %s", c.PodPassEvery, c.PodPassMaxAge, collect.DefaultPodPassEvery, collect.DefaultPodPassMaxAge)
	}
	for _, cfg := range []Config{{PodPassEvery: -1}, {PodPassMaxAge: -time.Second}} {
		if err := cfg.applyDefaults(); err == nil {
			t.Errorf("applyDefaults(%+v) = nil, want an error", cfg)
		}
	}
	for _, n := range []int{0, -1} {
		if err := ValidatePodPassEvery(n); err == nil {
			t.Errorf("ValidatePodPassEvery(%d) = nil, want an error", n)
		}
	}
	if err := ValidatePodPassEvery(1); err != nil {
		t.Errorf("ValidatePodPassEvery(1) = %v", err)
	}
	for _, d := range []time.Duration{0, -time.Minute} {
		if err := ValidatePodPassMaxAge(d); err == nil {
			t.Errorf("ValidatePodPassMaxAge(%v) = nil, want an error", d)
		}
	}
	if err := ValidatePodPassMaxAge(time.Minute); err != nil {
		t.Errorf("ValidatePodPassMaxAge(1m) = %v", err)
	}
}

// The age of the reused evidence grows with every tick that reuses it, so
// it is not part of what is pushed as changed: a tick that reuses the pass
// pushes nothing new, as one that read every pod would not.
func TestSnapshotHashIgnoresAddOnEvidenceAge(t *testing.T) {
	inv := fakeClientsInventory(t)
	h1, _, err := snapshotHash(inv)
	if err != nil {
		t.Fatal(err)
	}
	inv.AddOnEvidenceAgeSeconds = 1200
	h2, raw, err := snapshotHash(inv)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Error("hash changed when only AddOnEvidenceAgeSeconds changed: every reusing tick would push")
	}
	if !strings.Contains(string(raw), `"addOnEvidenceAgeSeconds":1200`) {
		t.Errorf("wire JSON lacks the age: %s", raw)
	}
}

// Tick by tick: the first lists the pods, the next two reuse that pass and
// say how old it is on the status (valid against the CRD's schema), which
// the fourth, a full pass again, clears; and the unchanged reuse pushes
// nothing.
func TestTickReusesThePodPassAndRecordsItsAge(t *testing.T) {
	ctx := context.Background()
	dyn := fakeDyn()
	srv := newSnapServer(t)
	cfg := Config{ServerURL: srv.srv.URL, ServerToken: "tok", PodPassEvery: 3, PodPassMaxAge: time.Hour}
	if err := cfg.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	clients := fakeClients(t, "v1.35.2")
	podLists := 0
	clients.Kube.(*kubefake.Clientset).PrependReactor("list", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetNamespace() == "" {
			podLists++
		}
		return false, nil, nil
	})
	r := newRunner(clients, dyn, mustKB(t), cfg)
	r.pusher.wait = func(context.Context, time.Duration) error { return nil }

	for tick, want := range []struct {
		lists   int
		reused  bool
		pushes  int
		metrics string
	}{{1, false, 1, "0"}, {1, true, 1, ""}, {1, true, 1, ""}, {2, false, 1, "0"}} {
		if err := r.tick(ctx); err != nil {
			t.Fatalf("tick %d: %v", tick+1, err)
		}
		st := readCRStatus(t, dyn, crd.DefaultName)
		if podLists != want.lists {
			t.Errorf("tick %d: %d cluster-wide pod lists so far, want %d", tick+1, podLists, want.lists)
		}
		if got := st.AddOnEvidenceAgeSeconds > 0; got != want.reused {
			t.Errorf("tick %d: status.addOnEvidenceAgeSeconds = %d, want set = %v", tick+1, st.AddOnEvidenceAgeSeconds, want.reused)
		}
		if got := r.last.reports[0].AddOnEvidenceAgeSeconds; got != st.AddOnEvidenceAgeSeconds {
			t.Errorf("tick %d: report age %d, status age %d: want the same", tick+1, got, st.AddOnEvidenceAgeSeconds)
		}
		if got := srv.count(); got != want.pushes {
			t.Errorf("tick %d: %d pushes so far, want %d: the age is not content", tick+1, got, want.pushes)
		}
		requireCRValidAgainstCRD(t, dyn, crd.DefaultName)
	}
}
