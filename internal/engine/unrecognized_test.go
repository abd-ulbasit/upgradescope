package engine

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// The image repositories no registry matcher claims reach the report
// (#18), so a detection gap is visible instead of silent: sorted,
// deduplicated, capped, with the dropped ones counted. They are never
// findings: they change neither the score nor the verdict.
func TestEvaluateReportsUnrecognizedImages(t *testing.T) {
	inv := clusterInv()
	inv.UnrecognizedImages = []string{"docker.io/library/redis", "corp.example/edge/nginx-controller", "docker.io/library/redis"}
	inv.UnrecognizedImagesOmitted = 3
	rep := Evaluate(inv, testRegistryKB(), inventory.Version{Major: 1, Minor: 35}, testNow)
	if want := []string{"corp.example/edge/nginx-controller", "docker.io/library/redis"}; !reflect.DeepEqual(rep.UnrecognizedImages, want) {
		t.Errorf("unrecognized = %v, want %v", rep.UnrecognizedImages, want)
	}
	if rep.UnrecognizedImagesOmitted != 3 {
		t.Errorf("omitted = %d, want 3", rep.UnrecognizedImagesOmitted)
	}
	if inv.UnrecognizedImages[0] != "docker.io/library/redis" {
		t.Error("Evaluate modified the inventory's slice")
	}
	clean := Evaluate(clusterInv(), testRegistryKB(), inventory.Version{Major: 1, Minor: 35}, testNow)
	if rep.Score != clean.Score || rep.Verdict != clean.Verdict || len(rep.Findings) != len(clean.Findings) {
		t.Errorf("unrecognized images changed the result: %+v vs %+v", rep, clean)
	}

	// An inventory from another collector may hold more than the cap.
	inv.UnrecognizedImages = nil
	for i := range inventory.MaxUnrecognizedImages + 5 {
		inv.UnrecognizedImages = append(inv.UnrecognizedImages, fmt.Sprintf("example.com/app-%03d", i))
	}
	rep = Evaluate(inv, testRegistryKB(), inventory.Version{Major: 1, Minor: 35}, testNow)
	if len(rep.UnrecognizedImages) != inventory.MaxUnrecognizedImages || rep.UnrecognizedImagesOmitted != 3+5 {
		t.Errorf("capped to %d, omitted %d; want %d and 8", len(rep.UnrecognizedImages), rep.UnrecognizedImagesOmitted, inventory.MaxUnrecognizedImages)
	}
}
