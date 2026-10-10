package agent

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/crd"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

func TestConfigApplyDefaults(t *testing.T) {
	cfg := Config{}
	if err := cfg.applyDefaults(); err != nil {
		t.Fatalf("applyDefaults on zero config: %v", err)
	}
	if cfg.Interval != 10*time.Minute {
		t.Errorf("Interval = %v, want 10m", cfg.Interval)
	}
	if cfg.CRName != crd.DefaultName {
		t.Errorf("CRName = %q, want %q", cfg.CRName, crd.DefaultName)
	}
	if cfg.TeamLabel != "team" {
		t.Errorf("TeamLabel = %q, want team", cfg.TeamLabel)
	}
	if cfg.ForceSyncEvery != time.Hour {
		t.Errorf("ForceSyncEvery = %v, want 1h", cfg.ForceSyncEvery)
	}
}

func TestConfigIntervalMinimum(t *testing.T) {
	cfg := Config{Interval: 30 * time.Second}
	err := cfg.applyDefaults()
	if err == nil || !strings.Contains(err.Error(), "1m") {
		t.Fatalf("err = %v, want minimum-interval error", err)
	}
}

func TestConfigServerURLRequiresToken(t *testing.T) {
	cfg := Config{ServerURL: "http://server:8080"}
	if err := cfg.applyDefaults(); err == nil {
		t.Fatal("server-url without server-token must error")
	}
	cfg = Config{ServerURL: "http://server:8080", ServerToken: "t"}
	if err := cfg.applyDefaults(); err != nil {
		t.Fatalf("valid server config rejected: %v", err)
	}
}

// A bad --targets value fails at startup, not as a per-tick note: the
// agent would otherwise write it into spec.targets on every tick.
func TestConfigRejectsInvalidTargets(t *testing.T) {
	cfg := Config{Targets: []string{"1.37", "latest"}}
	err := cfg.applyDefaults()
	if err == nil || !strings.Contains(err.Error(), "latest") {
		t.Fatalf("err = %v, want invalid-target error naming the value", err)
	}
}

// A truncated --targets entry (what YAML makes of 1.30 in a chart's
// agent.targets) fails at startup, naming the pitfall, instead of being
// written to spec.targets and judged against a Kubernetes older than the
// knowledge base covers (#237).
func TestConfigRejectsTargetsBelowTheKnowledgeBase(t *testing.T) {
	for _, bad := range []string{"1.3", "1.4", "1.15"} {
		cfg := Config{Targets: []string{"1.37", bad}}
		err := cfg.applyDefaults()
		if err == nil || !strings.Contains(err.Error(), "targets:") || !strings.Contains(err.Error(), "oldest minor the knowledge base covers is 1.16") {
			t.Errorf("targets [1.37 %s]: err = %v, want a targets error naming the knowledge base floor", bad, err)
		}
	}
	cfg := Config{Targets: []string{"1.3"}}
	if err := cfg.applyDefaults(); err == nil || !strings.Contains(err.Error(), `is this 1.30 written as a YAML number? quote it`) {
		t.Errorf("err = %v, want the YAML pitfall named", err)
	}
	cfg = Config{Targets: []string{"1.16", "1.30"}}
	if err := cfg.applyDefaults(); err != nil {
		t.Errorf("targets [1.16 1.30]: %v, want them accepted", err)
	}
}

// Targets are normalized to the MAJOR.MINOR form the CRD's spec.targets
// pattern requires (and that resolveTargets compares against), dropping
// duplicates the normalization creates.
func TestConfigNormalizesTargets(t *testing.T) {
	cfg := Config{Targets: []string{"v1.38", "1.37.2", "1.37", "1.36.0-rc.1"}}
	if err := cfg.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"1.38", "1.37", "1.36"}; !slices.Equal(cfg.Targets, want) {
		t.Errorf("Targets = %v, want %v", cfg.Targets, want)
	}
}

func TestResolveTargetsFromSpec(t *testing.T) {
	targets, notes, err := resolveTargets(
		crd.Spec{Targets: []string{"1.36", "1.37"}},
		inventory.Inventory{ServerVersion: "v1.35.2"},
	)
	if err != nil || len(notes) != 0 {
		t.Fatalf("err=%v notes=%v", err, notes)
	}
	want := []inventory.Version{{Major: 1, Minor: 36}, {Major: 1, Minor: 37}}
	if len(targets) != 2 || targets[0] != want[0] || targets[1] != want[1] {
		t.Errorf("targets = %v, want %v", targets, want)
	}
}

// The CRD schema does not make spec.targets a set, so a hand-edited CR can
// list a minor twice. It is evaluated once: a second report for the same
// target would duplicate every per-target metric series and fail /metrics.
func TestResolveTargetsDropsDuplicateSpecTargets(t *testing.T) {
	targets, notes, err := resolveTargets(
		crd.Spec{Targets: []string{"1.36", "1.37", "1.36"}},
		inventory.Inventory{ServerVersion: "v1.35.2"},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []inventory.Version{{Major: 1, Minor: 36}, {Major: 1, Minor: 37}}
	if !slices.Equal(targets, want) {
		t.Errorf("targets = %v, want %v", targets, want)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "duplicate") || !strings.Contains(notes[0], "1.36") {
		t.Errorf("notes = %v, want one naming the duplicate 1.36", notes)
	}
}

func TestResolveTargetsSkipsInvalidWithNote(t *testing.T) {
	targets, notes, err := resolveTargets(
		crd.Spec{Targets: []string{"latest", "1.37"}},
		inventory.Inventory{ServerVersion: "v1.35.2"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0] != (inventory.Version{Major: 1, Minor: 37}) {
		t.Errorf("targets = %v, want [1.37]", targets)
	}
	// The note says why, in ParseTarget's own words.
	_, perr := inventory.ParseTarget("latest")
	if perr == nil {
		t.Fatal(`ParseTarget accepted "latest"`)
	}
	if want := fmt.Sprintf("targets: skipped invalid spec target %q: %v", "latest", perr); len(notes) != 1 || notes[0] != want {
		t.Errorf("notes = %q, want [%q]", notes, want)
	}
}

// A spec.targets entry below the knowledge base ("1.3", written by hand or
// by a tool that read the YAML number) is skipped with a note, never
// evaluated: it would read ready.
func TestResolveTargetsSkipsTargetsBelowTheKnowledgeBase(t *testing.T) {
	targets, notes, err := resolveTargets(
		crd.Spec{Targets: []string{"1.3", "1.37"}},
		inventory.Inventory{ServerVersion: "v1.35.2"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0] != (inventory.Version{Major: 1, Minor: 37}) {
		t.Errorf("targets = %v, want [1.37]", targets)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], `"1.3"`) {
		t.Errorf("notes = %v, want one naming %q", notes, "1.3")
	}
	// The note carries ParseTarget's error, so the YAML-number hint (1.3 is
	// what YAML makes of 1.30) and the knowledge base's floor reach the
	// operator in status.notAssessed.
	_, perr := inventory.ParseTarget("1.3")
	if perr == nil || len(notes) != 1 || !strings.HasSuffix(notes[0], ": "+perr.Error()) ||
		!strings.Contains(notes[0], `quote it ("1.30")`) || !strings.Contains(notes[0], inventory.OldestCovered().String()) {
		t.Errorf("notes = %q, want the note to end with ParseTarget's error %v (the YAML hint and the floor)", notes, perr)
	}
}

func TestResolveTargetsDefaultNextMinor(t *testing.T) {
	targets, _, err := resolveTargets(crd.Spec{}, inventory.Inventory{ServerVersion: "v1.35.2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0] != (inventory.Version{Major: 1, Minor: 36}) {
		t.Errorf("targets = %v, want [1.36] (next minor above observed)", targets)
	}
}

func TestResolveTargetsNoServerVersion(t *testing.T) {
	if _, _, err := resolveTargets(crd.Spec{}, inventory.Inventory{}); err == nil {
		t.Fatal("no targets and no server version: want error")
	}
}

// Chart default is targets: [] — the default target must derive from the
// vendor-suffixed GitVersion managed clusters report.
func TestResolveTargetsDefaultFromVendorServerVersion(t *testing.T) {
	for _, sv := range []string{
		"v1.34.2-gke.100", "v1.34.2-eks-aeac579", "v1.34.2+k3s1", "v1.34.2+rke2r1", "v1.34.2+29a0aa9",
	} {
		targets, notes, err := resolveTargets(crd.Spec{}, inventory.Inventory{ServerVersion: sv})
		if err != nil || len(notes) != 0 {
			t.Fatalf("%s: err=%v notes=%v", sv, err, notes)
		}
		if len(targets) != 1 || targets[0] != (inventory.Version{Major: 1, Minor: 35}) {
			t.Errorf("%s: targets = %v, want [1.35]", sv, targets)
		}
	}
}

func TestResolveTargetsUnparseableServerVersion(t *testing.T) {
	_, _, err := resolveTargets(crd.Spec{}, inventory.Inventory{ServerVersion: "garbage"})
	if err == nil || !strings.Contains(err.Error(), "garbage") {
		t.Fatalf("err = %v, want unparseable-version error naming the version", err)
	}
}

// Kubernetes has only ever shipped major 1: a spec target like "2.0" is a
// typo, not a version to evaluate against.
func TestResolveTargetsSkipsNonMajorOneSpecTarget(t *testing.T) {
	targets, notes, err := resolveTargets(
		crd.Spec{Targets: []string{"2.0", "1.37"}},
		inventory.Inventory{ServerVersion: "v1.35.2"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0] != (inventory.Version{Major: 1, Minor: 37}) {
		t.Errorf("targets = %v, want [1.37]", targets)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], `"2.0"`) {
		t.Errorf("notes = %v, want one naming \"2.0\"", notes)
	}
}
