package sarif

import (
	"fmt"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// The cap is the omitted objects' reason only when the finding reached it
// (#362): below inventory.MaxObjectRefs listed, the omitted objects are
// ones the finding never names, such as a cluster's pods beside its
// PersistentVolumes, and no reason is given.
func TestUnlistedMessageCapReasonOnlyAtTheCap(t *testing.T) {
	const reason = "(at most 100 objects are recorded per finding)"
	for _, tc := range []struct {
		listed int
		want   bool
	}{{1, false}, {inventory.MaxObjectRefs - 1, false}, {inventory.MaxObjectRefs, true}} {
		objs := make([]inventory.ObjectRef, tc.listed)
		for i := range objs {
			objs[i] = inventory.ObjectRef{Name: fmt.Sprintf("o%d", i), File: "all.yaml", Line: i + 1}
		}
		f := engine.Finding{Key: "volume-plugin/rbd", Title: "In-tree volume plugin rbd removed in 1.31", Objects: objs, ObjectsOmitted: 2}
		msg := unlistedMessage(f, 2)
		if got := strings.Contains(msg, reason); got != tc.want {
			t.Errorf("%d listed: %q: the cap's reason given = %v, want %v", tc.listed, msg, got, tc.want)
		}
	}
}
