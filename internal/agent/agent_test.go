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

func TestResolveTargetsUnparseableServerVersion(t *testing.T) {
	_, _, err := resolveTargets(crd.Spec{}, inventory.Inventory{ServerVersion: "v1.34.2-gke.100"})
	if err == nil || !strings.Contains(err.Error(), "gke") {
		t.Fatalf("err = %v, want unparseable-version error naming the version", err)
	}
}
