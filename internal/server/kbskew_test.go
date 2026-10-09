package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
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

// pushKBStatus is pushKB for a push whose status the test checks itself.
func (h *harness) pushKBStatus(cluster, agentVersion, kbVersion string, inv inventory.Inventory) (int, map[string]any) {
	h.t.Helper()
	body, err := json.Marshal(map[string]any{
		"schemaVersion": 1, "clusterName": cluster, "agentVersion": agentVersion, "kbVersion": kbVersion, "inventory": inv,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	resp, out := postSnapshot(h.t, h.ts, "ingest-tok", body, false)
	return resp.StatusCode, out
}

// hostile is a 64 KiB label of control characters and a letter: what a
// compromised agent could send as its kbVersion or agentVersion.
func hostile() string { return strings.Repeat("\x01\x7fA", 64<<10/3) }

// checkReasonsBounded fails when a gap's reason in rep is over the bound
// the reasons the server builds keep to, or carries any of the label.
func checkReasonsBounded(t *testing.T, where string, rep legacyReportView, label string) {
	t.Helper()
	if len(rep.NotAssessed) == 0 {
		t.Errorf("%s: no gaps, want the skew named", where)
	}
	for _, g := range rep.NotAssessed {
		if len(g.Reason) > 4<<10 {
			t.Errorf("%s: %s reason is %d bytes, want it bounded (a label is never echoed)", where, g.Capability, len(g.Reason))
		}
		for _, bad := range []string{"\x01", "\x7f", `\x01`, `\u0001`, `\x7f`, "AAAA", label[:64]} {
			if strings.Contains(g.Reason, bad) {
				t.Errorf("%s: %s reason carries %q of the label: %.200q", where, g.Capability, bad, g.Reason)
			}
		}
	}
}

// K1: the push envelope's kbVersion and agentVersion are agent-controlled
// text. Ingest refuses an oversized or non-printable one (422), so a
// compromised agent cannot store it; a snapshot stored before that check
// carries whatever it sent, and every read path judges it, so the reasons
// built from it never quote it and stay within the bound admission applies.
func TestKBSkewNeverEchoesTheAgentsLabels(t *testing.T) {
	k := embeddedKB(t)
	bad := hostile()
	h := newHarness(t, Config{KB: k}, aug1)

	for _, tc := range []struct{ name, agent, kb string }{
		{"kbVersion 64 KiB of control characters", "0.2.0", bad},
		{"agentVersion 64 KiB of control characters", bad, k.Version},
		{"kbVersion with a newline", "0.2.0", "k8s.io/api v0.36.1\n; lifecycle 00000000; registry 00000000"},
		{"kbVersion with a DEL", "0.2.0", "x\x7f"},
		{"agentVersion with a tab", "0.2\t0", k.Version},
		{"kbVersion over the bound", "0.2.0", strings.Repeat("a", 1<<10)},
		{"agentVersion over the bound", strings.Repeat("1", 257), k.Version},
		{"kbVersion not ASCII", "0.2.0", "k8s.io/api v0.36.1 \u202e; lifecycle 00000000; registry 00000000"},
	} {
		code, out := h.pushKBStatus("hostile-"+strings.ReplaceAll(strings.Fields(tc.name)[0], "V", "v"), tc.agent, tc.kb, testInventory())
		if code != http.StatusUnprocessableEntity {
			t.Errorf("%s: push = %d %v, want 422", tc.name, code, out)
		}
		if msg, _ := out["error"].(string); len(msg) > 512 {
			t.Errorf("%s: the error is %d bytes, want it not to echo the label", tc.name, len(msg))
		}
	}
	// A plain label of any shape is accepted: it names no digests.
	for _, label := range []string{"", "rc2-kb", "k8s.io/api v0.36.1; lifecycle 1a2b3c4d; registry 5e6f7a8b", "(devel)"} {
		if code, out := h.pushKBStatus("plain", "dev", label, testInventory()); code != http.StatusAccepted && code != http.StatusOK {
			t.Errorf("kbVersion %q: push = %d %v, want it accepted", label, code, out)
		}
	}

	// A snapshot stored with the hostile envelope (by a server before the
	// check): judged on every path without echoing it.
	h.pushKB("stored", "0.2.0", k.Version, unmarked(testInventory())) // unmarked: a hostile agentVersion makes it legacy
	ctx := context.Background()
	c, err := h.st.ClusterByName(ctx, "stored")
	if err != nil {
		t.Fatal(err)
	}
	latest, err := h.st.LatestSnapshot(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, agent, kb string }{
		{"kbVersion", "0.2.0", bad},
		{"agentVersion", bad, k.Version},
		{"both", bad, bad},
	} {
		snap := latest
		snap.ID, snap.Hash, snap.KBVersion, snap.AgentVersion = 0, "hostile-"+tc.name, tc.kb, tc.agent
		snap.ReceivedAt = snap.ReceivedAt.Add(time.Second)
		if _, dup, err := h.st.CommitEvaluations(ctx, store.EvaluationBatch{Cluster: &c, Snapshot: &snap}); err != nil || dup {
			t.Fatalf("storing the hostile snapshot: dup %v, %v", dup, err)
		}
		// No evaluation holds the new snapshot, so each target is judged
		// afresh from the stored bytes and envelope: decodeInventory.
		for _, target := range []string{"1.35", "1.36"} {
			checkReasonsBounded(t, tc.name+" what-if "+target, h.report("stored", target), bad)
		}
		var detail struct {
			Capabilities map[string]inventory.CapabilityStatus `json:"capabilities"`
		}
		h.get("/api/v1/clusters/"+itoa(c.ID), &detail)
		for name, st := range detail.Capabilities {
			if len(st.Reason) > 4<<10 || strings.ContainsAny(st.Reason, "\x01\x7f") || strings.Contains(st.Reason, "AAAA") {
				t.Errorf("%s: cluster detail %s reason carries the label: %.200q", tc.name, name, st.Reason)
			}
		}
	}
	// And the view itself, whatever the store holds.
	for _, label := range []string{bad, "rc2-kb\n", strings.Repeat("é", 1<<15)} {
		got := judgedView(skewInventory(), label, label, k)
		for c, st := range got.Capabilities {
			if len(st.Reason) > 4<<10 || strings.ContainsAny(st.Reason, "\x01\x7f\n") || strings.Contains(st.Reason, "é") || strings.Contains(st.Reason, "AAAA") {
				t.Errorf("judgedView(%.20q): %s reason carries the label: %.200q", label, c, st.Reason)
			}
		}
	}
}

// Upgrading an agent to the server's data clears the gap at once, even when
// the cluster has not changed: the push is a duplicate (the hash covers the
// inventory only), and a duplicate whose envelope differs re-judges every
// target instead of waiting for the next UTC day.
func TestKBSkewClearsWhenTheAgentIsUpgraded(t *testing.T) {
	k := embeddedKB(t)
	h := newHarness(t, Config{KB: k}, aug1)
	inv := func() inventory.Inventory {
		i := testInventory()
		i.UnrecognizedImages = []string{"docker.io/weaveworks/weave-kube"}
		return i
	}
	h.pushKB("upgraded", "0.2.0", "rc2-kb", inv())
	for _, target := range []string{"1.35", "1.36"} {
		if rep := h.report("upgraded", target); rep.Verdict != "unknown" {
			t.Fatalf("target %s before the upgrade: verdict = %s, want unknown", target, rep.Verdict)
		}
	}
	code, out := h.pushKBStatus("upgraded", "0.2.0", k.Version, inv())
	if code != http.StatusOK || out["duplicate"] != true {
		t.Fatalf("the same inventory from the upgraded agent = %d %v, want 200 duplicate", code, out)
	}
	for _, target := range []string{"1.35", "1.36"} {
		rep := h.report("upgraded", target)
		if rep.Verdict != "ready" || len(rep.NotAssessed) != 0 {
			t.Errorf("target %s after the upgrade: verdict = %s, gaps %+v, want ready with none", target, rep.Verdict, rep.NotAssessed)
		}
	}
	// And back: the server judges what the envelope says now.
	if code, out := h.pushKBStatus("upgraded", "0.2.0", "rc2-kb", inv()); code != http.StatusOK {
		t.Fatalf("downgrade push = %d %v", code, out)
	}
	if rep := h.report("upgraded", "1.35"); rep.Verdict != "unknown" {
		t.Errorf("after the agent reports older data again: verdict = %s, want unknown", rep.Verdict)
	}

	// An agent version that changes how the inventory is judged: an
	// unmarked inventory is legacy until the agent is 0.2.0 or later.
	lh := newHarness(t, Config{KB: testKB()}, aug1)
	lh.pushAs("legacy", "dev", unmarked(testInventory()))
	if _, required := lh.report("legacy", "1.35").gap("api-usage"); !required {
		t.Fatal("a v0.1.x agent's api-usage is not a required gap")
	}
	if code, out := lh.pushAs("legacy", "0.2.0", unmarked(testInventory())); code != http.StatusOK || out["duplicate"] != true {
		t.Fatalf("upgraded agent, same inventory = %d %v, want 200 duplicate", code, out)
	}
	if rep := lh.report("legacy", "1.35"); len(rep.NotAssessed) != 0 || rep.Verdict != "ready" {
		t.Errorf("an agent upgraded to 0.2.0 is still judged as legacy: verdict %s, gaps %+v", rep.Verdict, rep.NotAssessed)
	}
}

// An agent run with --registry-dir has a different registry digest than a
// server without the same entries (KB-15: the entries change the
// knowledge-base version), so every verdict is unknown until both have them.
// The reason says that, not only "upgrade the agent", which cannot fix it.
func TestKBSkewRegistryDirMismatchIsNamed(t *testing.T) {
	server := embeddedKB(t)
	dir := t.TempDir()
	entry := "schema_version: 2\nid: acme-proxy\ndisplay_name: Acme Proxy\nmatchers:\n  images:\n    - acme/acme-proxy\nsupport:\n  status: eol\n  citations:\n    - https://acme.dev/lifecycle\n"
	if err := os.WriteFile(filepath.Join(dir, "acme-proxy.yaml"), []byte(entry), 0o644); err != nil {
		t.Fatal(err)
	}
	agent, err := kb.LoadWithRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	if agent.Version == server.Version {
		t.Fatal("the extra registry did not change the knowledge-base version")
	}
	h := newHarness(t, Config{KB: server}, aug1)
	h.pushKB("extra", "0.2.0", agent.Version, testInventory())
	rep := h.report("extra", "1.35")
	if rep.Verdict != "unknown" {
		t.Fatalf("verdict = %s, want unknown", rep.Verdict)
	}
	reason, required := rep.gap("addons")
	if !required || !strings.Contains(reason, "--registry-dir") || !strings.Contains(reason, "different") {
		t.Errorf("addons gap = %q (required %v), want it to name --registry-dir entries as a cause", reason, required)
	}
	// The server given the same entries judges the same push as complete.
	h2 := newHarness(t, Config{KB: agent}, aug1)
	h2.pushKB("extra", "0.2.0", agent.Version, testInventory())
	if rep := h2.report("extra", "1.35"); rep.Verdict != "ready" {
		t.Errorf("server with the same --registry-dir: verdict = %s, gaps %+v, want ready", rep.Verdict, rep.NotAssessed)
	}
}

// A server upgrade turns every cluster whose agent runs older data from
// ready to unknown, and that is not news: an unknown pass sends nothing and
// is not a baseline (operations: notifications), so the upgrade does not
// notify every such cluster at once, and neither does the agent catching up.
func TestKBSkewMovesNoNotifications(t *testing.T) {
	k := embeddedKB(t)
	_, registry, _ := kbDigests(k.Version)
	h := newHarness(t, Config{KB: k}, aug1)
	h.pushKB("fleet-a", "0.2.0", k.Version, testInventory())
	h.pushKB("fleet-b", "0.2.0", k.Version, testInventory())
	h.drain()
	// The server is upgraded to newer data: the agents' snapshots were
	// collected with the data it replaced.
	newer := k
	newer.Version = withDigests(t, k.Version, "deadbeef", registry)
	h.restart(Config{KB: newer})
	h.tick()
	if events := h.drain(); len(events) != 0 {
		t.Errorf("the server upgrade sent %d notification(s), want none: %+v", len(events), events)
	}
	if rep := h.report("fleet-a", "1.35"); rep.Verdict != "unknown" {
		t.Fatalf("after the server upgrade: verdict = %s, want unknown (the test would prove nothing)", rep.Verdict)
	}
	// The agent catches up: a duplicate push, re-judged, is ready again.
	h.pushKBStatus("fleet-a", "0.2.0", newer.Version, testInventory())
	if rep := h.report("fleet-a", "1.35"); rep.Verdict != "ready" {
		t.Fatalf("after the agent catches up: verdict = %s, want ready", rep.Verdict)
	}
	if events := h.drain(); len(events) != 0 {
		t.Errorf("the agent catching up sent %d notification(s), want none: %+v", len(events), events)
	}
}
