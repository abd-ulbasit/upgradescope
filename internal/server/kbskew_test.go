package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

func embeddedKB(t *testing.T) kb.KB {
	t.Helper()
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// withDigests replaces the digests of a kb.Version label.
func withDigests(t *testing.T, version, lifecycle, registry string) string {
	t.Helper()
	i := strings.Index(version, "; lifecycle ")
	if i < 0 {
		t.Fatalf("version %q is not in the dataset form", version)
	}
	return version[:i] + "; lifecycle " + lifecycle + "; registry " + registry
}

func TestKBDigests(t *testing.T) {
	k := embeddedKB(t)
	lifecycle, registry, ok := kbDigests(k.Version)
	if !ok || len(lifecycle) != 8 || len(registry) != 8 {
		t.Fatalf("kbDigests(%q) = %q, %q, %v", k.Version, lifecycle, registry, ok)
	}
	for _, v := range []string{"", "agent-kb", "rc2-kb", "k8s.io/api v0.37.1; lifecycle 1a2b3c4d", "k8s.io/api v0.37.1; lifecycle XYZ; registry 12345678"} {
		if _, _, ok := kbDigests(v); ok {
			t.Errorf("kbDigests(%q) ok, want no digests", v)
		}
	}
}

func skewInventory() inventory.Inventory {
	inv := testInventory()
	inv.Capabilities[inventory.CapAPIUsage] = inventory.CapabilityStatus{Available: true, Partial: true, Reason: "r", Skipped: []string{"policy/v1beta1 PodSecurityPolicy"}}
	return inv
}

// A snapshot collected with the server's own data is judged exactly as it
// is; one collected with other data is partial over what the server's data
// would have collected, naming the digests and inventory.SkippedNewerKB.
func TestKBSkewView(t *testing.T) {
	k := embeddedKB(t)
	lifecycle, registry, _ := kbDigests(k.Version)
	other := "00000000"
	for _, tc := range []struct {
		name         string
		agentKB      string
		server       kb.KB
		api, helm    bool // marked
		addons       bool
		wantReasonIn string
	}{
		{"same data", k.Version, k, false, false, false, ""},
		{"same digests, another generatedFrom", "k8s.io/api v0.0.1; lifecycle " + lifecycle + "; registry " + registry, k, false, false, false, ""},
		{"lifecycle differs", withDigests(t, k.Version, other, registry), k, true, true, false, "the agent collected with " + other + ", this server judges with " + lifecycle},
		{"registry differs", withDigests(t, k.Version, lifecycle, other), k, false, false, true, "the agent collected with " + other + ", this server judges with " + registry},
		{"both differ", withDigests(t, k.Version, other, other), k, true, true, true, ""},
		{"agent names no digests", "rc2-kb", k, true, true, true, "names no dataset digests"},
		{"agent sent nothing", "", k, true, true, true, "names no dataset digests"},
		{"server cannot compare", "rc2-kb", testKB(), false, false, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := skewInventory()
			before := reflect.ValueOf(inv.Capabilities).Pointer()
			got := kbSkewView(inv, tc.agentKB, tc.server)
			if reflect.ValueOf(inv.Capabilities).Pointer() != before || inv.Capabilities[inventory.CapAPIUsage].Reason != "r" || len(inv.Capabilities[inventory.CapAddOns].Skipped) != 0 {
				t.Fatal("kbSkewView changed the inventory it was given")
			}
			for c, marked := range map[inventory.Capability]bool{inventory.CapAPIUsage: tc.api, inventory.CapHelm: tc.helm, inventory.CapAddOns: tc.addons} {
				st := got.Capabilities[c]
				if has := slices.Contains(st.Skipped, inventory.SkippedNewerKB); has != marked || (marked && (!st.Available || !st.Partial)) {
					t.Errorf("%s = %+v, marked = %v, want marked = %v", c, st, has, marked)
				}
				if marked && tc.wantReasonIn != "" && (c == inventory.CapAddOns) == (tc.addons && !tc.api) && !strings.Contains(st.Reason, tc.wantReasonIn) {
					t.Errorf("%s reason %q lacks %q", c, st.Reason, tc.wantReasonIn)
				}
			}
			if !tc.api {
				if want := skewInventory().Capabilities[inventory.CapAPIUsage]; !reflect.DeepEqual(got.Capabilities[inventory.CapAPIUsage], want) {
					t.Errorf("api-usage = %+v, want it untouched", got.Capabilities[inventory.CapAPIUsage])
				}
			} else if st := got.Capabilities[inventory.CapAPIUsage]; !slices.Contains(st.Skipped, "policy/v1beta1 PodSecurityPolicy") || !strings.HasPrefix(st.Reason, "r; ") {
				t.Errorf("api-usage = %+v, want what it skipped before kept", st)
			}
		})
	}
	// A capability that was not read at all is already a gap of its own.
	inv := testInventory()
	inv.Capabilities[inventory.CapAddOns] = inventory.CapabilityStatus{Reason: "forbidden"}
	if got := kbSkewView(inv, "rc2-kb", k).Capabilities[inventory.CapAddOns]; got.Available || got.Partial || got.Reason != "forbidden" {
		t.Errorf("unavailable addons = %+v, want untouched", got)
	}
}

func (h *harness) pushKB(cluster, agentVersion, kbVersion string, inv inventory.Inventory) {
	h.t.Helper()
	body, err := json.Marshal(map[string]any{
		"schemaVersion": 1, "clusterName": cluster, "agentVersion": agentVersion, "kbVersion": kbVersion, "inventory": inv,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	if resp, out := postSnapshot(h.t, h.ts, "ingest-tok", body, false); resp.StatusCode != http.StatusAccepted {
		h.t.Fatalf("push %s = %d %v", cluster, resp.StatusCode, out)
	}
}

// #268: weave-net (retired: a blocker at any version) joined the registry
// after v0.2.0-rc.2. An rc.2 agent's registry does not know it, so it sent
// the image as unrecognized, and a newer server read the cluster ready. The
// verdict is now unknown, with a required addons gap saying why, until the
// agent runs the server's data.
func TestKBSkewOlderAgentIsNeverReady(t *testing.T) {
	k := embeddedKB(t)
	lifecycle, registry, _ := kbDigests(k.Version)
	rc2 := func() inventory.Inventory {
		inv := testInventory()
		inv.CollectorSchema = 0 // v0.2.0-rc.2 predates the stamp
		inv.UnrecognizedImages = []string{"docker.io/weaveworks/weave-kube"}
		return inv
	}
	h := newHarness(t, Config{KB: k}, aug1)
	for _, tc := range []struct {
		cluster, kbVersion string
		wantVerdict        string
		gaps               map[string]string // capability → text its required gap's reason has
	}{
		{"same-data", k.Version, "ready", nil},
		{"rc2-kb-label", "rc2-kb", "unknown", map[string]string{"addons": "names no dataset digests", "api-usage": "names no dataset digests"}},
		{"older-registry", withDigests(t, k.Version, lifecycle, "deadbeef"), "unknown", map[string]string{"addons": "the agent collected with deadbeef, this server judges with " + registry}},
		{"older-lifecycle", withDigests(t, k.Version, "deadbeef", registry), "unknown", map[string]string{"api-usage": "the agent collected with deadbeef, this server judges with " + lifecycle}},
	} {
		h.pushKB(tc.cluster, "0.2.0-rc.2", tc.kbVersion, rc2())
		// The stored evaluation, and the same snapshot judged afresh for a
		// target no evaluation holds (a what-if).
		for _, target := range []string{"1.35", "1.36"} {
			rep := h.report(tc.cluster, target)
			if rep.Verdict != tc.wantVerdict {
				t.Errorf("%s, target %s: verdict = %s, want %s: %+v", tc.cluster, target, rep.Verdict, tc.wantVerdict, rep.NotAssessed)
			}
			for c, text := range tc.gaps {
				reason, required := rep.gap(c)
				if !required || !strings.Contains(reason, text) || !strings.Contains(reason, "upgrade the agent") {
					t.Errorf("%s, target %s: %s gap = %q (required %v), want a required gap with %q telling to upgrade the agent", tc.cluster, target, c, reason, required, text)
				}
			}
			if len(tc.gaps) == 0 && len(rep.NotAssessed) != 0 {
				t.Errorf("%s, target %s: gaps %+v, want none: the data is the same", tc.cluster, target, rep.NotAssessed)
			}
		}
	}
	// The registry-only skew leaves api-usage alone.
	rep := h.report("older-registry", "1.35")
	if _, required := rep.gap("api-usage"); required {
		t.Error("registry skew made api-usage a required gap")
	}
	// The cluster detail shows the same capabilities as the evaluation.
	var detail struct {
		Capabilities map[string]inventory.CapabilityStatus `json:"capabilities"`
	}
	h.get("/api/v1/clusters/"+itoa(h.clusterID("older-registry")), &detail)
	if st := detail.Capabilities["addons"]; !st.Partial || !slices.Contains(st.Skipped, inventory.SkippedNewerKB) {
		t.Errorf("cluster detail addons = %+v, want partial naming the skew", st)
	}
}
