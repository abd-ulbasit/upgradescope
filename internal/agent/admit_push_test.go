package agent

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// admittingServer answers like the real ingest: 202 for an inventory
// inventory.Admit accepts, 422 for one it refuses.
type admittingServer struct {
	mu       sync.Mutex
	accepted []inventory.Inventory
	refused  []string
	srv      *httptest.Server
}

func newAdmittingServer(t *testing.T) *admittingServer {
	s := &admittingServer{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		var pl struct {
			Inventory inventory.Inventory `json:"inventory"`
		}
		if err := json.NewDecoder(zr).Decode(&pl); err != nil {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := pl.Inventory.Admit(); err != nil {
			s.refused = append(s.refused, err.Error())
			w.WriteHeader(http.StatusUnprocessableEntity)
			io.WriteString(w, err.Error())
			return
		}
		s.accepted = append(s.accepted, pl.Inventory)
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, `{"snapshotId": 1}`)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// hostileInventory is what a collector that copied what anyone with a
// Secret, a ConfigMap or a Pod in one namespace can write would hand the
// agent: a release named by a free label value, a manifest object named by
// text in a payload, a 17 KiB image repository and chart metadata.
func hostileInventory() inventory.Inventory {
	return inventory.Inventory{
		SchemaVersion: 1, CollectorSchema: inventory.CurrentCollectorSchema, ClusterID: "uid-123",
		ServerVersion: "v1.35.2", CollectedAt: time.Now().UTC(),
		Capabilities: map[inventory.Capability]inventory.CapabilityStatus{
			inventory.CapHelm: {Available: true}, inventory.CapAddOns: {Available: true}, inventory.CapAPIUsage: {Available: true},
		},
		HelmReleases: []inventory.HelmRelease{
			{Name: "Bad_Name", Namespace: "tenant", ChartName: "app", ChartVersion: "1.0.0", Status: "deployed"},
			{Name: "long", Namespace: "tenant", ChartName: "app", ChartVersion: "1.0.0", KubeVersion: strings.Repeat("x", 19<<10), Status: "deployed"},
			{Name: "cron", Namespace: "tenant", ChartName: "app", ChartVersion: "1.0.0", Status: "deployed", ManifestAPIs: []inventory.APIUsage{{
				Group: "batch", Version: "v1beta1", Kind: "CronJob", Count: 2,
				Objects: []inventory.ObjectRef{{Namespace: "tenant", Name: "Bad/Name", Line: 3}, {Namespace: "tenant", Name: "fine", Line: 9}},
			}}},
			{Name: "fine", Namespace: "tenant", ChartName: "fine", ChartVersion: "1.0.0", Status: "deployed"},
		},
		UnrecognizedImages: []string{"registry.example.com/" + strings.Repeat("a", 17<<10), "registry.example.com/team/app"},
	}
}

// #268: an agent whose collector hands it such an inventory pushes one the
// server accepts (202), where the server refused the same one (422) on
// every tick for ever. What was left out is named in the capabilities.
func TestTickPushesAnInventoryTheServerAccepts(t *testing.T) {
	srv := newAdmittingServer(t)
	r := testRunner(t, fakeDyn(), srv.srv.URL)
	r.collectFn = func(context.Context) inventory.Inventory { return hostileInventory() }
	if err := r.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.refused) != 0 || len(srv.accepted) != 1 {
		t.Fatalf("server accepted %d, refused %q; want one accepted and none refused", len(srv.accepted), srv.refused)
	}
	got := srv.accepted[0]
	if len(got.HelmReleases) != 2 || got.HelmReleases[0].Name != "cron" || got.HelmReleases[1].Name != "fine" {
		t.Errorf("releases = %+v, want the two the server can take", got.HelmReleases)
	}
	if st := got.Capabilities[inventory.CapHelm]; !st.Partial || len(st.Skipped) < 3 {
		t.Errorf("helm = %+v, want partial naming the releases left out", st)
	}
	if got.UnrecognizedImagesOmitted != 1 || len(got.UnrecognizedImages) != 1 {
		t.Errorf("images = %q omitted %d, want the long one counted, not listed", got.UnrecognizedImages, got.UnrecognizedImagesOmitted)
	}
	if st := got.Capabilities[inventory.CapAddOns]; !st.Partial {
		t.Errorf("addons = %+v, want partial", st)
	}
	// Unchanged content is not sent again: the push succeeded.
	if err := r.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(srv.accepted) != 1 {
		t.Errorf("pushes = %d, want the second tick deduplicated", len(srv.accepted))
	}
}

// What no repair mends (here a server version that is not a Kubernetes
// version) is not sent at all: the agent says so, rather than offering the
// server a push it refuses for good every tick.
func TestTickDoesNotPushAnInventoryNoRepairMends(t *testing.T) {
	srv := newAdmittingServer(t)
	r := testRunner(t, fakeDyn(), srv.srv.URL)
	r.collectFn = func(context.Context) inventory.Inventory {
		inv := hostileInventory()
		inv.ServerVersion = "not-a-version"
		return inv
	}
	err := r.tick(context.Background())
	if err == nil || !strings.Contains(err.Error(), "the server would refuse this inventory") {
		t.Fatalf("tick error = %v, want it to say the push was skipped because the server would refuse it", err)
	}
	if n := len(srv.accepted) + len(srv.refused); n != 0 {
		t.Errorf("the server was sent %d push(es), want none", n)
	}
	if r.last.push != pushFailed {
		t.Errorf("push outcome = %v, want failed", r.last.push)
	}
}
