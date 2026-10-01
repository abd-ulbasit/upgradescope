package agent

import (
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
	if len(notes) != 1 || !strings.Contains(notes[0], "latest") {
		t.Errorf("notes = %v, want one mentioning %q", notes, "latest")
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
